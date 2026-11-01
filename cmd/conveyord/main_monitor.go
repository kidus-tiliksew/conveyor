package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// monitorPoller runs repository-monitor polls when they are triggered: once
// at daemon start, for a push delivery to a monitored base branch, and from
// the timed GitHub fallback of a workspace without webhook delivery. No
// fixed loop schedules them (component-runtime, Start-up checks and
// background loops; component-monitor-drift, Forge observation;
// req-quiet-idle-runtime AC-3.1, AC-3.5).
type monitorPoller struct {
	Store      store.Backend
	Dispatcher *dispatch.Dispatcher
	Service    *monitor.Service
	// Config reads the workspace bound to the context.
	Config func(context.Context) (*config.Config, error)
	// Runner resolves the workspace App's REST runner for a repository;
	// nil uses the workspace App installation identity.
	Runner func(ctx context.Context, workspace, slug string) (func(context.Context, ...string) ([]byte, error), error)
	// Sleep waits between poll attempts; nil waits on a timer.
	Sleep func(context.Context, time.Duration) error
}

// Settings reports whether the workspace's monitor is enabled and its poll
// interval.
func (p *monitorPoller) Settings(ctx context.Context, workspace string) (bool, time.Duration, error) {
	current, err := p.Config(store.WithWorkspace(ctx, workspace))
	if err != nil {
		return false, 0, err
	}
	return current.Monitor.Enabled, current.Monitor.PollInterval, nil
}

// PollWorkspace polls every monitored repository of an enabled monitor once.
func (p *monitorPoller) PollWorkspace(ctx context.Context, workspace string) error {
	workspaceCtx := store.WithWorkspace(ctx, workspace)
	current, err := p.Config(workspaceCtx)
	if err != nil {
		return err
	}
	if !current.Monitor.Enabled {
		return nil
	}
	var failures []string
	for _, repositoryName := range current.Monitor.Repositories {
		if pollErr := p.pollRepository(workspaceCtx, workspace, current, repositoryName); pollErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", repositoryName, pollErr))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("monitor poll: %s", strings.Join(failures, "; "))
	}
	return nil
}

// PushedRef applies a push delivery: a push to refs/heads/<base> of a
// monitored repository polls that repository once; any other push does
// nothing (component-monitor-drift, Forge observation).
func (p *monitorPoller) PushedRef(ctx context.Context, githubRepo, ref string) error {
	workspace, _ := store.WorkspaceFromContext(ctx)
	current, err := p.Config(ctx)
	if err != nil {
		return err
	}
	if !current.Monitor.Enabled {
		return nil
	}
	for _, repositoryName := range current.Monitor.Repositories {
		repository, ok := current.Repo(repositoryName)
		if !ok || repository.GitHub == "" || !strings.EqualFold(repository.GitHub, githubRepo) {
			continue
		}
		base := repository.Base
		if base == "" {
			base = "main"
		}
		if ref != "refs/heads/"+base {
			return nil
		}
		return p.pollRepository(ctx, workspace, current, repositoryName)
	}
	return nil
}

// pollRepository is one poll of one monitored repository, as the removed
// five-second loop ran it.
func (p *monitorPoller) pollRepository(workspaceCtx context.Context, workspaceID string, current *config.Config, repositoryName string) error {
	st, d := p.Store, p.Dispatcher
	repository, ok := current.Repo(repositoryName)
	if !ok || repository.GitHub == "" {
		_ = p.Service.Store.RecordMonitorFailure(workspaceCtx, "forge_response",
			"monitored repository requires a GitHub slug", time.Now().Add(current.Monitor.PollInterval))
		return nil
	}
	resolve := p.Runner
	if resolve == nil {
		resolve = func(ctx context.Context, workspace, slug string) (func(context.Context, ...string) ([]byte, error), error) {
			return workspaceMonitorGitHubRunner(ctx, st, workspace, slug, githubtrigger.DefaultAppClient)
		}
	}
	runGitHub, tokenErr := resolve(workspaceCtx, workspaceID, repository.GitHub)
	if tokenErr != nil {
		_ = p.Service.Store.RecordMonitorFailure(workspaceCtx, string(githubtrigger.ErrorCategory(tokenErr)), tokenErr.Error(), time.Now().Add(current.Monitor.PollInterval))
		return nil
	}
	source := monitor.GitHubSource{
		WorkspaceID: workspaceID, Repository: repositoryName, GitHubSlug: repository.GitHub, Run: runGitHub,
		NewTaskResolver: assignmentResolveTask(st, repositoryName, repository.GitHub),
		ReconcileMerged: func(callCtx context.Context, taskID string, pr githubtrigger.PullRequest) (bool, error) {
			return d.ReconcileObservedPullRequest(callCtx, repositoryName, repository.GitHub, taskID, pr)
		},
	}
	bound := current.Monitor.MergeDeferralBound
	monitorStore := p.Service.Store
	source.MergeCommitState = func(ctx context.Context, repository, commitSHA string, committedAt time.Time) (monitor.MergeState, string, error) {
		return monitorStore.MonitorMergeCommitState(ctx, repository, commitSHA, committedAt, time.Now().UTC(), bound)
	}
	source.OnSuppressed = func(ctx context.Context, payload map[string]any) error {
		return p.Service.Store.AuditMonitor(ctx, "monitor.suppressed", payload)
	}
	source.LoadHints = func(ctx context.Context, revision string) (*monitor.HintContext, error) {
		return monitor.FetchGitHubHints(ctx, repository.GitHub, revision, runGitHub)
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	poller := monitor.Poller{
		Service: p.Service, Source: source, StartupWindow: current.Monitor.StartupWindow,
		RetryInitial: time.Second, RetryMaximum: 30 * time.Second, Sleep: sleep,
	}
	return poller.Poll(workspaceCtx)
}
