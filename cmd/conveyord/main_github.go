package main

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// GitHub observation while the daemon runs (component-runtime, Start-up
// checks and background loops; req-quiet-idle-runtime AC-3.1, AC-3.5,
// AC-3.7; DEC-99(5)): signed webhook deliveries for every workspace, the
// timed GitHub fallback for a workspace without webhook delivery, and timed
// issue intake only with -poll-github.

// gitHubWebhookDispatcher is the dispatcher surface the applier maps
// deliveries to.
type gitHubWebhookDispatcher interface {
	ApplyPullRequestDelivery(ctx context.Context, repository string, pr githubtrigger.PullRequest) error
	ApplyChecksDelivery(ctx context.Context, repository, sha string, branches []string) error
	IntakeReadyIssue(ctx context.Context, repository string, issue githubtrigger.Issue) error
}

// newGitHubWebhookApplier wires the applier the receiver hands each accepted
// delivery to (component-runtime, GitHub webhook applier).
func newGitHubWebhookApplier(webhooks store.GitHubWebhookStore, d gitHubWebhookDispatcher, pushed func(ctx context.Context, repository, ref string) error, delivered func(workspace string), logf func(string, ...any)) *githubtrigger.WebhookApplier {
	return &githubtrigger.WebhookApplier{
		Handlers: githubtrigger.WebhookHandlers{
			PullRequest: d.ApplyPullRequestDelivery,
			Checks:      d.ApplyChecksDelivery,
			Push:        pushed,
			ReadyIssue:  d.IntakeReadyIssue,
		},
		Bind: func(ctx context.Context, workspace string) context.Context {
			return store.WithWorkspace(store.WithActor(ctx, store.SystemActor(httpapi.GitHubWebhookActor)), workspace)
		},
		Forget:    webhooks.ForgetGitHubWebhookDelivery,
		Delivered: delivered,
		Logf:      logf,
	}
}

// newGitHubFallback wires the timed GitHub fallback (component-runtime,
// Timed GitHub fallback).
func newGitHubFallback(st store.Backend, d *dispatch.Dispatcher, workspaces func(context.Context) ([]string, error), monitorPolls *monitorPoller, logf func(string, ...any)) *dispatch.GitHubFallback {
	return &dispatch.GitHubFallback{
		Dispatcher: d, Hub: st.Changes(), Workspaces: workspaces,
		Webhooks:    st.GitHubWebhookStatus,
		Monitor:     monitorPolls.Settings,
		PollMonitor: monitorPolls.PollWorkspace,
		Logf:        logf,
	}
}

// startIssueIntakePoll runs the ready-labeled issue poll for every workspace
// at interval, only when the deployment passes -poll-github above zero
// (req-quiet-idle-runtime AC-3.7). It reports whether the poll started.
// ticker returns the tick channel and its stop function; nil uses a
// time.Ticker.
func startIssueIntakePoll(ctx context.Context, interval time.Duration, workspaces func(context.Context) ([]string, error), poll func(context.Context), ticker func(time.Duration) (<-chan time.Time, func()), logf func(string, ...any)) bool {
	if interval <= 0 {
		return false
	}
	if ticker == nil {
		ticker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	logf("polling GitHub for %s issues in every workspace every %s", githubtrigger.ReadyLabel, interval)
	ticks, stop := ticker(interval)
	go func() {
		defer stop()
		for {
			ids, err := workspaces(ctx)
			if err == nil {
				for _, id := range ids {
					poll(store.WithWorkspace(ctx, id))
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticks:
			}
		}
	}()
	return true
}
