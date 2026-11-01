package dispatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
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
// labels, finalize intake, and enqueue. A failure after the task exists
// returns the error; the source check makes a retry file no second task.
func (d *Dispatcher) intakeReadyIssue(ctx, forgeCtx context.Context, workspace string, repo config.Repo, issue github.Issue) error {
	intakeLock.Lock()
	defer intakeLock.Unlock()
	source := fmt.Sprintf("github:%s#%d", repo.GitHub, issue.Number)
	if d.sourceTaskExists(ctx, source) {
		return nil
	}
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Source: source, Title: issue.Title, Body: issue.Body, Level: core.L2, Repo: repo.Name, BaseBranch: repo.Base, Branch: gitx.BranchName(id), State: core.TaskClaiming, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	if err := d.Store.CreateTask(ctx, task); err != nil {
		return err
	}
	if err := github.MarkIssueDispatched(forgeCtx, repo.GitHub, issue.Number, id); err != nil {
		return err
	}
	_, _ = taskops.New(d.Store).Perform(ctx, id, taskops.Command{Kind: core.TaskIntakeFinalize})
	d.Enqueue(ctx, id)
	return nil
}
