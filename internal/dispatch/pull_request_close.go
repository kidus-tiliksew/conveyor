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

// queueStartedOverPR recovers the post-commit gap from durable supersession
// and its original actor event, never from a retrying caller's credential.
func (d *Dispatcher) queueStartedOverPR(ctx context.Context, retired core.Task) error {
	if retired.SupersededBy == "" {
		return nil
	}
	successor, err := d.Store.GetTask(ctx, retired.SupersededBy)
	if err != nil {
		return err
	}
	if successor.Supersedes != retired.ID {
		return fmt.Errorf("start-over successor does not match retired task")
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	repo, ok := cfg.Repo(retired.Repo)
	if !ok || repo.GitHub == "" {
		return fmt.Errorf("GitHub repository is not configured for %s", retired.Repo)
	}
	events, err := d.Store.ListEvents(ctx, retired.ID)
	if err != nil {
		return err
	}
	var number int
	var url string
	for _, event := range events {
		if event.Kind != "pull_request.opened" {
			continue
		}
		var opened struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		}
		if json.Unmarshal(event.Payload, &opened) != nil || opened.Number <= 0 || strings.TrimSpace(opened.URL) == "" {
			continue
		}
		number, url = opened.Number, opened.URL
	}
	for _, event := range events {
		if event.Kind != "task.started_over" {
			continue
		}
		var payload struct {
			SuccessorID string `json:"successor_id"`
			Reason      string `json:"reason"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.SuccessorID != successor.ID {
			continue
		}
		return d.Store.QueuePullRequestClose(ctx, core.PullRequestClose{
			WorkspaceID: retired.Workspace, TaskID: retired.ID, Repository: repo.GitHub, Branch: retired.Branch,
			SuccessorID: successor.ID, SuccessorBranch: successor.Branch, Reason: payload.Reason,
			RestartingOperatorID: strings.TrimPrefix(event.ActorID, "user:"), ForgeAuthorClass: core.ForgeAuthorWorkspace,
			Number: number, URL: url, State: "queued",
		})
	}
	return fmt.Errorf("start-over event unavailable for retired task %s", retired.ID)
}

type pullRequestCloseWorker struct{ dispatcher *Dispatcher }

func (w *pullRequestCloseWorker) observePullRequest(ctx context.Context, p core.PullRequestClose) (github.PullRequest, error) {
	if p.Number > 0 {
		if w.dispatcher.PullRequestForNumber == nil {
			return github.PullRequest{}, errors.New("number-addressed PR lookup is unavailable")
		}
		return w.dispatcher.PullRequestForNumber(ctx, p.Repository, p.Number)
	}
	return w.dispatcher.PullRequestForClose(ctx, p.Repository, p.Branch)
}

func (w *pullRequestCloseWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.PullRequestCloseArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	ctx = store.WithActor(ctx, store.Actor{ID: "queue:pull-request-close:" + job.ID, Role: core.ActorSystem})
	return w.dispatcher.Store.WithTaskSideEffectLock(ctx, args.TaskID, func(ctx context.Context) error {
		p, ok, err := w.dispatcher.Store.GetPullRequestClose(ctx, args.TaskID)
		if err != nil || !ok || p.Terminal() {
			return err
		}
		events, err := w.dispatcher.Store.ListEvents(ctx, p.TaskID)
		if err != nil {
			return err
		}
		// Capture evidence before clearing this attempt. A persisted identity or
		// attempt count alone never proves that a PATCH was sent (AC-3.5).
		closeInvoked := false
		recordedNumber := 0
		recordedURL := ""
		recorded, originSeen := false, false
		for _, event := range events {
			if !originSeen && event.Kind == "pull_request.opened" {
				var opened struct {
					Number int    `json:"number"`
					URL    string `json:"url"`
				}
				if err := json.Unmarshal(event.Payload, &opened); err != nil {
					return err
				}
				recordedNumber, recordedURL = opened.Number, opened.URL
			}
			// AC-3.5: the append-only queued intent fixes provenance. Later
			// opened events cannot promote an identity discovered by branch lookup.
			// Missing origin evidence conservatively retains branch protection.
			if !originSeen && event.Kind == "pull_request.close_queued" {
				originSeen = true
				var intent core.PullRequestClose
				if err := json.Unmarshal(event.Payload, &intent); err != nil {
					return err
				}
				recorded = intent.WorkspaceID == p.WorkspaceID && intent.TaskID == p.TaskID &&
					intent.Repository == p.Repository && intent.Branch == p.Branch &&
					intent.Number > 0 && intent.Number == recordedNumber && intent.URL == recordedURL &&
					intent.Number == p.Number && intent.URL == p.URL
			}
			if strings.HasPrefix(event.Kind, "pull_request.close_") {
				var progress core.PullRequestClose
				if json.Unmarshal(event.Payload, &progress) == nil && progress.Attempts == p.Attempts &&
					progress.Number == p.Number && progress.LastError == closeUncertainMarker && p.LastError == closeUncertainMarker {
					closeInvoked = true
				}
			}
		}
		exhausted := p.Attempts >= core.PullRequestCloseMaxAttempts
		if !exhausted {
			p.Attempts++
		}
		p.State = "retrying"
		p.ForgeErrorCategory = ""
		p.LastError = ""
		if err = w.dispatcher.Store.UpdatePullRequestClose(ctx, p); err != nil {
			return err
		}
		fail := func(cause error) error {
			p.ForgeErrorCategory = string(github.ErrorCategory(github.CategorizeError(cause)))
			p.LastError = "pull request close: " + p.ForgeErrorCategory
			if p.ForgeErrorCategory == string(github.ForgePermission) {
				p.LastError = fmt.Sprintf("workspace %s repository %s: connect or repair the GitHub App in workspace settings", p.WorkspaceID, p.Repository)
			}
			if p.Attempts >= core.PullRequestCloseMaxAttempts || job.Attempt >= job.MaxAttempts {
				p.State = "failed"
			}
			if err := w.dispatcher.Store.UpdatePullRequestClose(ctx, p); err != nil {
				return err
			}
			// Durable records and queue logs carry only the token-free category.
			return errors.New(p.LastError)
		}
		// A crash after PATCH but before this write errs toward already_closed;
		// only a persisted post-PATCH uncertainty can authorize reconciliation.
		persistUncertain := func(closeCause error) error {
			p.ForgeErrorCategory = string(github.ErrorCategory(github.CategorizeError(closeCause)))
			p.LastError = closeUncertainMarker
			if p.Attempts >= core.PullRequestCloseMaxAttempts || job.Attempt >= job.MaxAttempts {
				p.State = "failed"
			}
			if err := w.dispatcher.Store.UpdatePullRequestClose(ctx, p); err != nil {
				return err
			}
			return errors.New(p.LastError)
		}
		task, err := w.dispatcher.Store.GetTask(ctx, p.TaskID)
		if err != nil {
			return fail(err)
		}
		if task.State != core.TaskClosed || task.SupersededBy != p.SuccessorID || task.Branch != p.Branch {
			return fail(fmt.Errorf("retired task no longer matches close intent"))
		}
		cfg, err := w.dispatcher.currentConfig(ctx)
		if err != nil {
			return fail(err)
		}
		repo, ok := cfg.Repo(task.Repo)
		if !ok || repo.GitHub != p.Repository {
			return fail(fmt.Errorf("close repository no longer matches task configuration"))
		}
		// Recorded-task provenance is distinct from a number discovered by an
		// earlier branch lookup. Retries of the latter keep the branch lock.
		attempt := func(ctx context.Context) error {
			guard := func() error { return w.checkCloseOwnership(ctx, task, p, !recorded) }
			if !recorded {
				if err := guard(); err != nil {
					return fail(err)
				}
			}
			token, err := w.dispatcher.workspaceCredential(ctx, p.Repository)
			if err != nil {
				return fail(err)
			}
			forgeCtx := github.WithCredential(ctx, token, github.AppIdentity(p.WorkspaceID))
			observedByNumber := p.Number > 0
			pr, err := w.observePullRequest(forgeCtx, p)
			if errors.Is(err, github.ErrPullRequestNotFound) {
				p.State = "skipped"
				p.Outcome = "absent"
				return w.dispatcher.Store.UpdatePullRequestClose(ctx, p)
			}
			if err != nil {
				return fail(err)
			}
			if pr.State != "open" && pr.State != "closed" {
				return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("invalid PR state")})
			}
			if !observedByNumber && p.Number > 0 && (pr.Number != p.Number || pr.URL != p.URL) {
				return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("PR identity changed")})
			}
			if observedByNumber && p.URL != "" && (pr.Number != p.Number || pr.URL != p.URL) {
				return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("PR identity changed")})
			}
			p.Number, p.URL = pr.Number, pr.URL
			if pr.Merged || pr.State != "open" {
				p.State = "skipped"
				p.Outcome = "already_closed"
				if pr.Merged {
					p.Outcome = "merged"
				} else if closeInvoked {
					p.State = "closed"
					p.Outcome = "reconciled"
				}
				return w.dispatcher.Store.UpdatePullRequestClose(ctx, p)
			}
			if err = w.dispatcher.Store.UpdatePullRequestClose(ctx, p); err != nil {
				return err
			}
			if exhausted {
				return fail(errors.New("pull request close attempts exhausted"))
			}
			if err := guard(); err != nil {
				return fail(err)
			}
			closeErr := w.dispatcher.ClosePullRequest(forgeCtx, p.Repository, p.Number, p.Comment(), token)
			pr, err = w.observePullRequest(forgeCtx, p)
			if err := guard(); err != nil {
				return fail(err)
			}
			if err != nil {
				if closeErr != nil && mutationUncertain(closeErr) {
					return persistUncertain(closeErr)
				}
				if closeErr != nil {
					return fail(closeErr)
				}
				return fail(err)
			}
			if pr.Number != p.Number || pr.URL != p.URL {
				return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("PR identity changed")})
			}
			// A validated open observation overrides even an ambiguous PATCH error.
			if !pr.Merged && pr.State == "open" {
				return fail(fmt.Errorf("pull request remains open"))
			}

			if pr.Merged {
				p.State = "skipped"
				p.Outcome = "merged"
			} else if pr.State == "closed" {
				p.State = "closed"
				p.Outcome = "closed"
			} else {
				return fail(fmt.Errorf("pull request remains open"))
			}
			return w.dispatcher.Store.UpdatePullRequestClose(ctx, p)
		}
		if recorded {
			return attempt(ctx)
		}
		return w.dispatcher.Store.WithTaskSideEffectLock(ctx, store.BranchCloseLockKey(task.Repo, p.Branch), attempt)
	})
}

func mutationUncertain(err error) bool {
	return errors.Is(err, github.ErrMutationUncertain)
}

const closeUncertainMarker = "pull request close: mutation_uncertain"

// AC-3.5 (component-git-delivery): neither branch reuse nor reuse of the
// recorded PR number grants the retired task permission to close another PR.
func (w *pullRequestCloseWorker) checkCloseOwnership(ctx context.Context, retired core.Task, p core.PullRequestClose, branchDerived bool) error {
	tasks, err := w.dispatcher.Store.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.ID == retired.ID || task.Workspace != retired.Workspace || task.Repo != retired.Repo || task.State == core.TaskClosed || task.State == core.TaskMerged {
			continue
		}
		if branchDerived && task.Branch == p.Branch {
			return fmt.Errorf("assigned branch is held by another open task")
		}
		if p.Number == 0 {
			continue
		}
		events, err := w.dispatcher.Store.ListEvents(ctx, task.ID)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind != "pull_request.opened" {
				continue
			}
			var opened struct {
				Number int `json:"number"`
			}
			if err := json.Unmarshal(event.Payload, &opened); err != nil {
				return err
			}
			if opened.Number == p.Number {
				return fmt.Errorf("pull request is recorded by another open task")
			}
		}
	}
	return nil
}
