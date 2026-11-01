package dispatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Ready-labeled issue intake shared by the -poll-github issue poll and the
// issues webhook delivery (component-runtime, Timed issue intake;
// req-intake-and-triage AC-4.1; req-quiet-idle-runtime AC-3.1, AC-3.7).

// intakeLock serializes intake on one daemon, so a delivery and a poll of
// the same issue file one task.
var intakeLock sync.Mutex

// IntakeReadyIssue files one task for a conveyor:ready issue of a configured
// repository, as the issue poll does for each listed issue. An issue already
// taken in, or a repository the workspace does not configure, is skipped.
func (d *Dispatcher) IntakeReadyIssue(ctx context.Context, githubRepo string, issue github.Issue) error {
	repo, ok, err := d.webhookRepository(ctx, githubRepo)
	if err != nil || !ok {
		return err
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	forgeCtx, err := d.workspaceForgeContext(ctx)
	if err != nil {
		return err
	}
	return d.intakeReadyIssue(ctx, forgeCtx, cfg.Workspace, repo, issue)
}

// intakeReadyIssue is the intake body: create the task, move the issue's
// labels, finalize intake, and enqueue. Each failure is returned. A retry,
// or the next issue poll, resumes a task an earlier failed attempt left
// claiming, and a task past intake is not taken in again (component-runtime,
// GitHub webhook applier).
func (d *Dispatcher) intakeReadyIssue(ctx, forgeCtx context.Context, workspace string, repo config.Repo, issue github.Issue) error {
	intakeLock.Lock()
	defer intakeLock.Unlock()
	source := fmt.Sprintf("github:%s#%d", repo.GitHub, issue.Number)
	existing, found, err := d.sourceTask(ctx, source)
	if err != nil {
		return err
	}
	id := existing.ID
	switch {
	case found && existing.State != core.TaskClaiming:
		return nil
	case !found:
		id = core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Source: source, Title: issue.Title, Body: issue.Body, Level: core.L2, Repo: repo.Name, BaseBranch: repo.Base, Branch: gitx.BranchName(id), State: core.TaskClaiming, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
		if err := d.Store.CreateTask(ctx, task); err != nil {
			return err
		}
	}
	if err := github.MarkIssueDispatched(forgeCtx, repo.GitHub, issue.Number, id); err != nil {
		return fmt.Errorf("move labels of %s: %w", source, err)
	}
	if _, err := taskops.New(d.Store).Perform(ctx, id, taskops.Command{Kind: core.TaskIntakeFinalize}); err != nil {
		return fmt.Errorf("finalize intake of %s: %w", source, err)
	}
	d.Enqueue(ctx, id)
	return nil
}

// sourceTask returns the workspace task taken in from source, if any.
func (d *Dispatcher) sourceTask(ctx context.Context, source string) (core.Task, bool, error) {
	tasks, err := d.Store.ListTasksFiltered(ctx, store.TaskFilter{Query: source})
	if err != nil {
		return core.Task{}, false, err
	}
	for _, task := range tasks {
		if task.Source == source {
			return task, true, nil
		}
	}
	return core.Task{}, false, nil
}
