package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// WorkspaceQueueRegistrar converges the River queue set known to one daemon
// instance with the workspaces persisted in PostgreSQL.
type WorkspaceQueueRegistrar struct {
	mu    sync.Mutex
	known map[string]struct{}
	add   func(string) error
	logf  func(string, ...any)
}

func NewWorkspaceQueueRegistrar(known []string, add func(string) error, logf func(string, ...any)) *WorkspaceQueueRegistrar {
	registered := make(map[string]struct{}, len(known))
	for _, workspace := range known {
		registered[workspace] = struct{}{}
	}
	return &WorkspaceQueueRegistrar{known: registered, add: add, logf: logf}
}

func (r *WorkspaceQueueRegistrar) Ensure(workspace string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.known[workspace]; ok {
		return false, nil
	}
	if err := r.add(workspace); err != nil {
		return false, err
	}
	r.known[workspace] = struct{}{}
	r.logf("registered River scheduling for workspace %s: queues=%s,%s,%s periodic_job=%s",
		workspace,
		queueargs.DispatchQueue(workspace),
		queueargs.ReviewPublicationQueue(workspace),
		queueargs.GitHubIssuePublicationQueue(workspace),
		queueargs.OrderClockPeriodicID(workspace),
	)
	return true, nil
}

func (r *WorkspaceQueueRegistrar) Converge(workspaces []core.Workspace) error {
	for _, workspace := range workspaces {
		if _, err := r.Ensure(workspace.ID); err != nil {
			return fmt.Errorf("register River scheduling for workspace %s: %w", workspace.ID, err)
		}
	}
	return nil
}

type dispatchTaskWorker struct {
	dispatcher *Dispatcher
	shutdown   *ShutdownMarker
}

const (
	RiverRescueSafetyMargin = 5 * time.Minute
	shutdownRetryDelay      = time.Second
)

// ShutdownMarker distinguishes daemon interruption from a stage-owned
// deadline. River supplies cancellation for both cases, so the worker also
// requires this process-scoped marker before it preserves the attempt.
type ShutdownMarker struct{ stopping atomic.Bool }

func (m *ShutdownMarker) Mark() {
	if m != nil {
		m.stopping.Store(true)
	}
}

func (m *ShutdownMarker) Stopping() bool { return m != nil && m.stopping.Load() }

// MarkedRuntime marks hard shutdown before the queue cancels active work.
type MarkedRuntime struct {
	runtime  queueargs.Runtime
	shutdown *ShutdownMarker
}

func NewMarkedRuntime(runtime queueargs.Runtime, shutdown *ShutdownMarker) *MarkedRuntime {
	return &MarkedRuntime{runtime: runtime, shutdown: shutdown}
}

func (c *MarkedRuntime) Stop(ctx context.Context) error { return c.runtime.Stop(ctx) }

func (c *MarkedRuntime) StopAndCancel(ctx context.Context) error {
	c.shutdown.Mark()
	return c.runtime.StopAndCancel(ctx)
}

type orderClockWorker struct {
	dispatcher *Dispatcher
}

func (w *orderClockWorker) Work(ctx context.Context, job queueargs.Job) error {
	args, err := queueargs.DecodeArgs[queueargs.OrderClockArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%s", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	_, err = taskops.New(w.dispatcher.Store).TickOrderClock(ctx, time.Now().UTC())
	return err
}

type reviewPublicationWorker struct {
	dispatcher *Dispatcher
}

type githubIssuePublicationWorker struct {
	dispatcher *Dispatcher
}

// Registrations binds every job kind to its handler and retry policy. The
// daemon hands them to whichever queue.Runtime is in use.
func (d *Dispatcher) Registrations(shutdown *ShutdownMarker) []queueargs.Registration {
	return []queueargs.Registration{
		{
			Kind:   queueargs.DispatchTaskArgs{}.Kind(),
			Handle: (&dispatchTaskWorker{dispatcher: d, shutdown: shutdown}).Work,
			// Bounded T12/T13 backoff between failed dispatch attempts
			// (design-task-lifecycle).
			RetryDelay: queueargs.DispatchTaskRetryDelay,
		},
		{Kind: queueargs.ReviewPublicationArgs{}.Kind(), Handle: (&reviewPublicationWorker{dispatcher: d}).Work},
		{Kind: queueargs.GitHubIssuePublicationArgs{}.Kind(), Handle: (&githubIssuePublicationWorker{dispatcher: d}).Work},
		{Kind: queueargs.OrderClockArgs{}.Kind(), Handle: (&orderClockWorker{dispatcher: d}).Work},
	}
}

// A create command failure is ambiguous: GitHub may have accepted the issue
// before the acknowledgement was lost. Require two durable, exhaustive
// no-marker passes before authorizing exactly one new create attempt.
const githubIssueReconciliationMissesBeforeCreate = 2

func (w *githubIssuePublicationWorker) Work(ctx context.Context, job queueargs.Job) error {
	args, err := queueargs.DecodeArgs[queueargs.GitHubIssuePublicationArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:github-issue-publication:%s", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	lifecycle, ok, err := w.dispatcher.Store.GetGitHubLifecycle(ctx, args.TaskID)
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
		lifecycle.ForgeAuthorClass = core.ForgeAuthorWorkspace
		lifecycle.ForgeAuthorUserID = ""
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
	lifecycle.ForgeAuthorClass = core.ForgeAuthorWorkspace
	lifecycle.ForgeAuthorUserID = ""
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

func (w *reviewPublicationWorker) Work(ctx context.Context, job queueargs.Job) error {
	args, err := queueargs.DecodeArgs[queueargs.ReviewPublicationArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:review-publication:%s", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	publication, err := w.dispatcher.Store.GetReviewPublication(ctx, args.ReviewWorkOrderID)
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
		publication.ForgeAuthorClass = core.ForgeAuthorWorkspace
		publication.ForgeAuthorUserID = ""
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
	publication.ForgeAuthorClass = core.ForgeAuthorWorkspace
	publication.ForgeAuthorUserID = ""
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

func (w *dispatchTaskWorker) Work(ctx context.Context, job queueargs.Job) error {
	args, err := queueargs.DecodeArgs[queueargs.DispatchTaskArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%s", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	err = w.dispatcher.runTask(ctx, args.TaskID)
	if err == nil {
		task, getErr := w.dispatcher.Store.GetTask(ctx, args.TaskID)
		if getErr != nil {
			return getErr
		}
		if task.State == core.TaskQueued {
			if isBlueprintAnchor(task) {
				// Blueprint parents are passive batch anchors. Their children
				// own implementation delivery, so this River row is complete
				// rather than a staged pipeline row to snooze.
				return nil
			}
			// The currently running River row owns this task's pipeline. A
			// queued stage transition cannot insert a duplicate while this row
			// is active, so snooze the same row into the next stage.
			return queueargs.Snooze(time.Second)
		}
		return nil
	}
	if w.shutdown.Stopping() && errors.Is(err, context.Canceled) {
		// Shutdown interruption is not a dispatch failure. Snoozing restores
		// River's incremented attempt and keeps the same durable row available
		// for another instance (REQ-6/AC-6.2; design-task-lifecycle).
		log.Printf("[task %s] queue job %s interrupted by daemon shutdown; preserving attempt %d", args.TaskID, job.ID, job.Attempt)
		return queueargs.Snooze(shutdownRetryDelay)
	}
	return w.handleFailure(ctx, job, err)
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job queueargs.Job, err error) error {
	args, decodeErr := queueargs.DecodeArgs[queueargs.DispatchTaskArgs](job)
	if decodeErr != nil {
		return fmt.Errorf("dispatch failed: %v; decode job: %w", err, decodeErr)
	}
	// A duplicate jobs key can be a lost acknowledgement from a dispatch that
	// already materialized the §21.30 conflict-fix order. Treat that durable
	// active order as success before emitting failure or requeue activity.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if _, active, lookupErr := w.dispatcher.activeImplementationWorkOrder(ctx, args.TaskID, "merge-conflict"); lookupErr != nil {
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
		TaskID: args.TaskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record River failure: %v", args.TaskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, args.TaskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, args.TaskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if task.State == core.TaskRunning {
			// Return the failed stage to queued before applying T13. This preserves
			// the canonical queued -> parked edge and records dispatch.fail_final
			// for operator visibility (design-task-lifecycle).
			if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: core.TaskStageBounce, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
				return fmt.Errorf("dispatch failed: %v; requeue before final River failure: %w", err, stateErr)
			}
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: core.TaskDispatchFailFinal, RecoveryStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final River attempt: %w", err, stateErr)
		}
	} else {
		command := core.TaskDispatchFailRetry
		if task.State == core.TaskRunning {
			// There is no running-state dispatch-failure retry edge. Preserve the
			// existing requeue behavior as an explicit table-gap workaround until
			// the lifecycle table is amended (design-task-lifecycle).
			command = core.TaskStageBounce
		}
		if _, stateErr := taskops.New(w.dispatcher.Store).Perform(ctx, args.TaskID, taskops.Command{Kind: command, NextStage: recoveryStage, ProjectStages: true}); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
		}
	}
	return err
}

// RiverRescueStuckJobsAfter bounds crash recovery above every stage that may
// run inside conveyord. The default timeout remains part of the calculation
// when a route is absent or has not yet been normalized.
func RiverRescueStuckJobsAfter(workspaceConfigs map[string]*config.Config) (time.Duration, error) {
	workspaces := make([]string, 0, len(workspaceConfigs))
	for workspace := range workspaceConfigs {
		workspaces = append(workspaces, workspace)
	}
	sort.Strings(workspaces)
	maxTimeout := time.Duration(0)
	routes := make([]riverRouteTimeout, 0, len(workspaces)*2)
	for _, workspace := range workspaces {
		cfg := workspaceConfigs[workspace]
		if cfg == nil {
			return 0, fmt.Errorf("River rescue threshold: workspace %s has no effective configuration", workspace)
		}
		for _, route := range riverInProcessRouteTimeouts(workspace, cfg) {
			routes = append(routes, route)
			if route.timeout > maxTimeout {
				maxTimeout = route.timeout
			}
		}
	}
	if maxTimeout == 0 {
		maxTimeout = config.DefaultStageTimeout
	}
	if maxTimeout > time.Duration(1<<63-1)-RiverRescueSafetyMargin {
		return 0, fmt.Errorf("River rescue threshold overflows maximum duration for route timeout %s", maxTimeout)
	}
	threshold := maxTimeout + RiverRescueSafetyMargin
	for _, route := range routes {
		if route.timeout <= 0 || route.timeout >= threshold {
			return 0, fmt.Errorf("River route %s timeout %s must be strictly below rescue threshold %s", route.name, route.timeout, threshold)
		}
	}
	return threshold, nil
}

type riverRouteTimeout struct {
	name    string
	timeout time.Duration
}

func riverInProcessRouteTimeouts(workspace string, cfg *config.Config) []riverRouteTimeout {
	routes := make([]riverRouteTimeout, 0, 2)
	for _, stage := range []string{"triage", "spec"} {
		timeout := config.DefaultStageTimeout
		if route, ok := cfg.Routing.Stages[stage]; ok && route.Timeout > 0 {
			timeout = route.Timeout
		}
		routes = append(routes, riverRouteTimeout{name: workspace + "/" + stage, timeout: timeout})
	}
	return routes
}

// ValidateRiverRescueStuckJobsAfter prevents a queue added after startup from
// introducing an in-process route that can be rescued while still live.
func ValidateRiverRescueStuckJobsAfter(workspace string, cfg *config.Config, threshold time.Duration) error {
	if cfg == nil {
		return fmt.Errorf("River rescue threshold: workspace %s has no effective configuration", workspace)
	}
	for _, route := range riverInProcessRouteTimeouts(workspace, cfg) {
		if route.timeout <= 0 || route.timeout >= threshold {
			return fmt.Errorf("River route %s timeout %s must be strictly below rescue threshold %s", route.name, route.timeout, threshold)
		}
	}
	return nil
}
