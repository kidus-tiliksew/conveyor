package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type dispatchTaskWorker struct {
	river.WorkerDefaults[queueargs.DispatchTaskArgs]
	dispatcher *Dispatcher
}

type orderClockWorker struct {
	river.WorkerDefaults[queueargs.OrderClockArgs]
	dispatcher *Dispatcher
}

func (w *orderClockWorker) Work(ctx context.Context, job *river.Job[queueargs.OrderClockArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	_, err := taskops.New(w.dispatcher.Store).TickOrderClock(ctx, time.Now().UTC())
	return err
}

func (w *dispatchTaskWorker) NextRetry(job *river.Job[queueargs.DispatchTaskArgs]) time.Time {
	return time.Now().UTC().Add(queueargs.DispatchTaskRetryDelay(job.Attempt))
}

type reviewPublicationWorker struct {
	river.WorkerDefaults[queueargs.ReviewPublicationArgs]
	dispatcher *Dispatcher
}

type githubIssuePublicationWorker struct {
	river.WorkerDefaults[queueargs.GitHubIssuePublicationArgs]
	dispatcher *Dispatcher
}

// A create command failure is ambiguous: GitHub may have accepted the issue
// before the acknowledgement was lost. Require two durable, exhaustive
// no-marker passes before authorizing exactly one new create attempt.
const githubIssueReconciliationMissesBeforeCreate = 2

func (w *githubIssuePublicationWorker) Work(ctx context.Context, job *river.Job[queueargs.GitHubIssuePublicationArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:github-issue-publication:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	lifecycle, ok, err := w.dispatcher.Store.GetGitHubLifecycle(ctx, job.Args.TaskID)
	if err != nil || !ok || lifecycle.State == core.GitHubPublicationPublished {
		return err
	}
	lifecycle.Attempts++
	lifecycle.State = core.GitHubPublicationRetrying
	lifecycle.ForgeErrorCategory = ""
	lifecycle.LastError = ""
	if err = w.dispatcher.Store.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		return err
	}
	task, err := w.dispatcher.Store.GetTask(ctx, lifecycle.TaskID)
	if err != nil {
		return err
	}
	spec, exists, err := w.dispatcher.Store.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !exists || !spec.Approved || spec.Version != lifecycle.SpecVersion {
		if err == nil {
			err = fmt.Errorf("approved spec version %d unavailable for task %s", lifecycle.SpecVersion, task.ID)
		}
		lifecycle.LastError = err.Error()
		if job.Attempt >= job.MaxAttempts {
			lifecycle.State = core.GitHubPublicationFailed
		}
		if updateErr := w.dispatcher.Store.UpdateGitHubLifecycle(ctx, lifecycle); updateErr != nil {
			return fmt.Errorf("validate approved GitHub issue intent: %v; record retry: %w", err, updateErr)
		}
		return err
	}
	var result github.IssuePublicationResult
	cfg, configErr := w.dispatcher.currentConfig(ctx)
	publishErr := configErr
	if publishErr == nil {
		configuredRepo, repoExists := cfg.Repo(task.Repo)
		if !repoExists || configuredRepo.GitHub == "" || configuredRepo.GitHub != lifecycle.Repository {
			publishErr = fmt.Errorf("GitHub lifecycle repository %q does not match configured task repository", lifecycle.Repository)
		}
	}
	if publishErr == nil {
		allowCreate := lifecycle.CreateState == core.GitHubCreateNotStarted ||
			(lifecycle.CreateState == core.GitHubCreateReconciling &&
				lifecycle.ReconcileMisses >= githubIssueReconciliationMissesBeforeCreate)
		result, publishErr = w.dispatcher.PublishIssue(ctx, github.IssuePublication{
			Repo: lifecycle.Repository, TaskID: task.ID, Title: task.Title,
			ApprovedSpec: spec.Content, SpecVersion: spec.Version,
			SourceIssueNumber: lifecycle.SourceIssueNumber,
			AllowCreate:       allowCreate,
			BeforeCreate: func(createCtx context.Context) error {
				lifecycle.CreateState = core.GitHubCreateReconciling
				lifecycle.CreateAttempts++
				lifecycle.ReconcileMisses = 0
				return w.dispatcher.Store.UpdateGitHubLifecycle(createCtx, lifecycle)
			},
		})
	}
	if publishErr != nil {
		if errors.Is(publishErr, github.ErrIssueReconciliationPending) {
			lifecycle.ReconcileMisses++
		}
		lifecycle.ForgeErrorCategory = string(github.ErrorCategory(publishErr))
		lifecycle.LastError = publishErr.Error()
		if job.Attempt >= job.MaxAttempts {
			lifecycle.State = core.GitHubPublicationFailed
		}
		if updateErr := w.dispatcher.Store.UpdateGitHubLifecycle(ctx, lifecycle); updateErr != nil {
			return fmt.Errorf("publish GitHub issue: %v; record retry: %w", publishErr, updateErr)
		}
		return publishErr
	}
	lifecycle.State = core.GitHubPublicationPublished
	lifecycle.CreateState = core.GitHubCreateConfirmed
	lifecycle.ReconcileMisses = 0
	lifecycle.IssueNumber = result.Number
	lifecycle.IssueURL = result.URL
	lifecycle.Outcome = "created"
	if result.Reused {
		lifecycle.Outcome = "reused"
	}
	lifecycle.LastError = ""
	lifecycle.ForgeErrorCategory = ""
	if err = w.dispatcher.Store.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		return err
	}
	return nil
}

func (w *reviewPublicationWorker) Work(ctx context.Context, job *river.Job[queueargs.ReviewPublicationArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:review-publication:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	publication, err := w.dispatcher.Store.GetReviewPublication(ctx, job.Args.ReviewWorkOrderID)
	if err != nil || (publication.State == core.ReviewPublicationPublished && publication.CommentID > 0) {
		return err
	}
	publication.Attempts++
	publication.State = core.ReviewPublicationRetrying
	publication.ForgeErrorCategory = ""
	publication.LastError = ""
	if err = w.dispatcher.Store.UpdateReviewPublication(ctx, publication); err != nil {
		return err
	}
	task, err := w.dispatcher.Store.GetTask(ctx, publication.TaskID)
	if err != nil {
		return err
	}
	cfg, err := w.dispatcher.currentConfig(ctx)
	if err != nil {
		return err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || repo.GitHub == "" {
		return fmt.Errorf("GitHub repository is not configured for %s", task.Repo)
	}
	var bounceHistory []string
	events, _ := w.dispatcher.Store.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind != "pipeline.bounced" {
			continue
		}
		var item struct {
			Count      int    `json:"count"`
			ReasonCode string `json:"reason_code"`
		}
		if json.Unmarshal(event.Payload, &item) == nil {
			bounceHistory = append(bounceHistory, fmt.Sprintf("bounce %d: %s", item.Count, item.ReasonCode))
		}
	}
	taskLink := ""
	if task.GitHub != nil && task.GitHub.IssueURL != "" {
		taskLink = task.GitHub.IssueURL
	}
	if taskLink == "" && (strings.HasPrefix(task.Source, "http://") || strings.HasPrefix(task.Source, "https://")) {
		taskLink = task.Source
	} else if taskLink == "" {
		if source, ok := strings.CutPrefix(task.Source, "github:"); ok {
			if slug, number, found := strings.Cut(source, "#"); found && slug != "" && number != "" {
				taskLink = "https://github.com/" + slug + "/issues/" + number
			}
		}
	}
	history := reviewHistory(events)
	statusState := reviewRoundStatus(events, publication.ReviewRound, publication.Verdict)
	result, publishErr := w.dispatcher.PublishReview(ctx, github.ReviewPublication{
		Repo: repo.GitHub, Branch: task.Branch, TaskID: task.ID, TaskLink: taskLink,
		ReviewWorkOrderID: publication.ReviewWorkOrderID, Verdict: publication.Verdict,
		ReasonCode: publication.ReasonCode, Summary: publication.Summary, Feedback: publication.Feedback,
		ReviewedCommitSHA: publication.ReviewedCommitSHA, ReviewerModel: publication.ReviewerModel,
		ReviewerSession: publication.ReviewerSession, SameModelAsImplementer: publication.SameModelAsImplementer,
		ReviewRound: publication.ReviewRound, ReviewSeat: publication.ReviewSeat,
		RequiredModel: publication.RequiredModel, RequiredEffort: publication.RequiredEffort, ModelEnforcement: publication.ModelEnforcement,
		History:       history,
		BounceHistory: bounceHistory,
		StatusState:   statusState,
	})
	if publishErr == nil && result.CommentID <= 0 {
		publishErr = errors.New("publish review: required aggregate comment returned no comment ID")
	}
	if publishErr != nil {
		publication.ForgeErrorCategory = string(github.ErrorCategory(publishErr))
		publication.LastError = publishErr.Error()
		if job.Attempt >= job.MaxAttempts {
			publication.State = core.ReviewPublicationFailed
		}
		if updateErr := w.dispatcher.Store.UpdateReviewPublication(ctx, publication); updateErr != nil {
			return fmt.Errorf("publish review: %v; record retry: %w", publishErr, updateErr)
		}
		return publishErr
	}
	publication.State = core.ReviewPublicationPublished
	publication.CheckRunID = result.CheckRunID
	publication.CommentID = result.CommentID
	publication.ReviewedCommitSHA = result.ReviewedCommitSHA
	publication.ForgeErrorCategory = ""
	publication.LastError = ""
	return w.dispatcher.Store.UpdateReviewPublication(ctx, publication)
}

func reviewRoundStatus(events []core.Event, round int, seatVerdict string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "review.round_completed" {
			continue
		}
		var result struct {
			Round   int    `json:"review_round"`
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal(events[i].Payload, &result) != nil || result.Round != round {
			continue
		}
		if result.Verdict == "changes_requested" {
			return "failure"
		}
		return "success"
	}
	if round > 0 {
		return "pending"
	}
	if seatVerdict == "changes_requested" {
		return "failure"
	}
	return "success"
}

func reviewHistory(events []core.Event) []github.ReviewHistoryItem {
	var history []github.ReviewHistoryItem
	maxRound := -1
	roundResults := map[int]string{}
	for _, event := range events {
		if event.Kind == "review.round_completed" {
			var result struct {
				Round   int    `json:"review_round"`
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(event.Payload, &result) == nil {
				roundResults[result.Round] = result.Verdict
			}
			continue
		}
		if event.Kind != "review.completed" {
			continue
		}
		var item struct {
			WorkOrderID   string `json:"review_work_order_id"`
			Verdict       string `json:"verdict"`
			ReasonCode    string `json:"reason_code"`
			Summary       string `json:"summary"`
			Feedback      string `json:"feedback"`
			ReviewerModel string `json:"reviewer_model"`
			Round         int    `json:"review_round"`
			Seat          int    `json:"review_seat"`
		}
		if json.Unmarshal(event.Payload, &item) != nil || item.WorkOrderID == "" {
			continue
		}
		if item.Round > maxRound {
			maxRound = item.Round
		}
		history = append(history, github.ReviewHistoryItem{WorkOrderID: item.WorkOrderID, Round: item.Round, Seat: item.Seat,
			Verdict: item.Verdict, ReasonCode: item.ReasonCode, Summary: item.Summary,
			Feedback: item.Feedback, ReviewerModel: item.ReviewerModel})
	}
	for i := range history {
		history[i].ResolutionState = "accepted"
		if history[i].Verdict != "changes_requested" {
			continue
		}
		history[i].ResolutionState = "unresolved"
		if history[i].Round < maxRound || (history[i].Round == 0 && i < len(history)-1) {
			history[i].ResolutionState = "superseded"
			for round := history[i].Round + 1; round <= maxRound; round++ {
				if roundResults[round] == "approve" {
					history[i].ResolutionState = "resolved"
				}
			}
			if history[i].Round == 0 && history[len(history)-1].Verdict == "approve" {
				history[i].ResolutionState = "resolved"
			}
		}
	}
	return history
}

func (w *dispatchTaskWorker) Work(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	err := w.dispatcher.runTask(ctx, job.Args.TaskID)
	if err == nil {
		task, getErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
		if getErr != nil {
			return getErr
		}
		if task.State == core.TaskQueued {
			// The currently running River row owns this task's pipeline. A
			// queued stage transition cannot insert a duplicate while this row
			// is active, so snooze the same row into the next stage.
			return river.JobSnooze(time.Second)
		}
		return nil
	}
	return w.handleFailure(ctx, job, err)
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs], err error) error {
	// A duplicate jobs key can be a lost acknowledgement from a dispatch that
	// already materialized the §21.30 conflict-fix order. Treat that durable
	// active order as success before emitting failure or requeue activity.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if _, active, lookupErr := w.dispatcher.activeImplementationWorkOrder(ctx, job.Args.TaskID, "merge-conflict"); lookupErr != nil {
			return fmt.Errorf("dispatch duplicate recovery: %w", lookupErr)
		} else if active {
			return nil
		}
	}
	payload := core.JSONPayload(map[string]any{
		"attempt":      job.Attempt,
		"max_attempts": job.MaxAttempts,
		"error":        err.Error(),
	})
	if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
		TaskID: job.Args.TaskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record River failure: %v", job.Args.TaskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, job.Args.TaskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if task.State == core.TaskRunning {
			// Return the failed stage to queued before applying T13. This preserves
			// the canonical queued -> parked edge and records dispatch.fail_final
			// for operator visibility (spec §3.3, §21.41).
			if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, job.Args.TaskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
				return fmt.Errorf("dispatch failed: %v; requeue before final River failure: %w", err, stateErr)
			}
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, job.Args.TaskID, taskops.Command{Kind: core.TaskDispatchFailFinal, RecoveryStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final River attempt: %w", err, stateErr)
		}
	} else {
		command := core.TaskDispatchFailRetry
		if task.State == core.TaskRunning {
			// There is no running-state dispatch-failure retry edge. Preserve the
			// existing requeue behavior as an explicit table-gap workaround until
			// the lifecycle table is amended (spec §21.37).
			command = core.TaskStageBounce
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, job.Args.TaskID, taskops.Command{Kind: command, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
		}
	}
	return err
}

// NewRiverClient binds the durable queue to the dispatcher. The Store owns
// transactional insertion; this client owns only worker execution (spec
// §3.1, §17.0).
func NewRiverClient(pool *pgxpool.Pool, dispatcher *Dispatcher, workspaces []string) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &dispatchTaskWorker{dispatcher: dispatcher})
	river.AddWorker(workers, &reviewPublicationWorker{dispatcher: dispatcher})
	river.AddWorker(workers, &githubIssuePublicationWorker{dispatcher: dispatcher})
	river.AddWorker(workers, &orderClockWorker{dispatcher: dispatcher})
	queues := map[string]river.QueueConfig{queueargs.ControlQueue: {MaxWorkers: 1}}
	periodicJobs := make([]*river.PeriodicJob, 0, len(workspaces))
	for _, workspace := range workspaces {
		queues[queueargs.DispatchQueue(workspace)] = river.QueueConfig{MaxWorkers: 1}
		queues[queueargs.ReviewPublicationQueue(workspace)] = river.QueueConfig{MaxWorkers: 1}
		queues[queueargs.GitHubIssuePublicationQueue(workspace)] = river.QueueConfig{MaxWorkers: 1}
		periodicJobs = append(periodicJobs, orderClockPeriodicJob(workspace))
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		// Dispatcher stage contexts enforce the configured per-stage wall-clock
		// limits. River's one-minute default would cancel long harness runs and
		// handoff artifact collection before those limits (spec §14).
		JobTimeout:   -1,
		Queues:       queues,
		Workers:      workers,
		PeriodicJobs: periodicJobs,
	})
}

func orderClockPeriodicJob(workspace string) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Second),
		func() (river.JobArgs, *river.InsertOpts) {
			return queueargs.OrderClockArgs{WorkspaceID: workspace}, &river.InsertOpts{Queue: queueargs.ControlQueue}
		},
		&river.PeriodicJobOpts{ID: queueargs.OrderClockPeriodicID(workspace), RunOnStart: true},
	)
}

func AddWorkspaceQueues(client *river.Client[pgx.Tx], workspace string) error {
	if err := client.Queues().Add(queueargs.DispatchQueue(workspace), river.QueueConfig{MaxWorkers: 1}); err != nil {
		if !errors.Is(err, &river.QueueAlreadyAddedError{}) {
			return err
		}
	}
	err := client.Queues().Add(queueargs.ReviewPublicationQueue(workspace), river.QueueConfig{MaxWorkers: 1})
	if err != nil && !errors.Is(err, &river.QueueAlreadyAddedError{}) {
		return err
	}
	err = client.Queues().Add(queueargs.GitHubIssuePublicationQueue(workspace), river.QueueConfig{MaxWorkers: 1})
	if err != nil && !errors.Is(err, &river.QueueAlreadyAddedError{}) {
		return err
	}
	_, err = client.PeriodicJobs().AddSafely(orderClockPeriodicJob(workspace))
	return err
}
