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
			RestartingOperatorID: strings.TrimPrefix(event.ActorID, "user:"), ForgeAuthorClass: core.ForgeAuthorWorkspace, State: "queued",
		})
	}
	return fmt.Errorf("start-over event unavailable for retired task %s", retired.ID)
}

type pullRequestCloseWorker struct{ dispatcher *Dispatcher }

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
		token, err := w.dispatcher.workspaceCredential(ctx, p.Repository)
		if err != nil {
			return fail(err)
		}
		forgeCtx := github.WithCredential(ctx, token, github.AppIdentity(p.WorkspaceID))
		pr, err := w.dispatcher.PullRequestForClose(forgeCtx, p.Repository, p.Branch)
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
		if p.Number > 0 && (pr.Number != p.Number || pr.URL != p.URL) {
			return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("PR identity changed")})
		}
		discoveredBefore := p.Number > 0
		p.Number, p.URL = pr.Number, pr.URL
		if pr.Merged || pr.State != "open" {
			p.State = "skipped"
			p.Outcome = "already_closed"
			if pr.Merged {
				p.Outcome = "merged"
			} else if discoveredBefore {
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
		closeErr := w.dispatcher.ClosePullRequest(forgeCtx, p.Repository, p.Number, p.Comment(), token)
		// Even the final failed acknowledgement may have completed remotely.
		// Observe the same PR before recording an exhausted publication.
		pr, err = w.dispatcher.PullRequestForClose(forgeCtx, p.Repository, p.Branch)
		if err != nil {
			if closeErr != nil {
				return fail(closeErr)
			}
			return fail(err)
		}
		if pr.Number != p.Number || pr.URL != p.URL {
			return fail(&github.Error{Category: github.ForgeResponse, Err: errors.New("PR identity changed")})
		}
		if closeErr != nil && !pr.Merged && pr.State != "closed" {
			return fail(closeErr)
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
	})
}
