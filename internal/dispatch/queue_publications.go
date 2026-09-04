package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The review and GitHub issue publication handlers.

// A create command failure is ambiguous: GitHub may have accepted the issue
// before the acknowledgement was lost. Require two durable, exhaustive
// no-marker passes before authorizing exactly one new create attempt.
const githubIssueReconciliationMissesBeforeCreate = 2

func (w *githubIssuePublicationWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.GitHubIssuePublicationArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("queue:github-issue-publication:%s", job.ID), Role: core.ActorSystem})
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

func (w *reviewPublicationWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.ReviewPublicationArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("queue:review-publication:%s", job.ID), Role: core.ActorSystem})
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
