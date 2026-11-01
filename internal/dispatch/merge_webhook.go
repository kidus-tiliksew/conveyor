package dispatch

import (
	"context"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Merge readiness and external merges from GitHub webhook deliveries
// (component-submission-merge, Merge readiness from webhook deliveries;
// req-quiet-idle-runtime AC-3.1; DEC-99(5)). The merge rules are unchanged;
// only their trigger is new. Every application takes the task side-effect
// lock the timed and start-up paths take.

// webhookRepository resolves a delivery's repository against the workspace's
// configured repositories, case-insensitively.
func (d *Dispatcher) webhookRepository(ctx context.Context, githubRepo string) (config.Repo, bool, error) {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return config.Repo{}, false, err
	}
	for _, repo := range cfg.Repos {
		if repo.GitHub != "" && strings.EqualFold(repo.GitHub, githubRepo) {
			return repo, true, nil
		}
	}
	return config.Repo{}, false, nil
}

// ApplyPullRequestDelivery hands a pull_request delivery's pull request to
// ReconcileObservedPullRequest for the task holding the branch: the
// repository task whose recorded pull_request.opened names the number or
// whose current branch equals the head ref, as the monitor matches it. A
// merged pull request completes an eligible task through the external merge
// path (req-delivery-and-forge AC-3.5); an unmerged one records nothing.
func (d *Dispatcher) ApplyPullRequestDelivery(ctx context.Context, githubRepo string, pr github.PullRequest) error {
	repo, ok, err := d.webhookRepository(ctx, githubRepo)
	if err != nil || !ok || pr.HeadRef == "" {
		return err
	}
	tasks, err := d.Store.ListTasksFiltered(ctx, store.TaskFilter{Repositories: []string{repo.Name}, Query: pr.HeadRef})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	events, err := d.Store.ListMonitorPullRequestEventsForTasks(ctx, ids)
	if err != nil {
		return err
	}
	taskID, matched := monitor.MatchObservedTask(tasks, events, repo.Name, repo.GitHub, pr.HeadRef, pr.Number)
	if !matched {
		return nil
	}
	_, err = d.ReconcileObservedPullRequest(ctx, repo.Name, repo.GitHub, taskID, pr)
	return err
}

// ApplyChecksDelivery applies a completed check_suite or check_run delivery
// or a status delivery: merge readiness for each repository task in a state
// mergeReadinessTaskStates names whose approved or reviewed head is the
// commit or whose branch is one of the delivery's branches. Each task runs
// the per-task body of ReconcileMergeReadiness, so a gate-off approved task
// merges once GitHub reports it mergeable: auto-merge on green checks
// (req-review-gates-evidence AC-1.1). Per-task failures are logged as the
// sweep logs them; only a failed task read is returned.
func (d *Dispatcher) ApplyChecksDelivery(ctx context.Context, githubRepo, sha string, branches []string) error {
	repo, ok, err := d.webhookRepository(ctx, githubRepo)
	if err != nil || !ok || (sha == "" && len(branches) == 0) {
		return err
	}
	tasks, err := d.Store.ListTasksFiltered(ctx, store.TaskFilter{States: mergeReadinessTaskStates, Repositories: []string{repo.Name}})
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if !checksDeliveryMatches(task, sha, branches) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		d.reconcileMergeReadinessTask(ctx, task)
	}
	return nil
}

func checksDeliveryMatches(task core.Task, sha string, branches []string) bool {
	if sha != "" && (task.ApprovedHeadSHA == sha || task.ReviewedHeadSHA == sha) {
		return true
	}
	for _, branch := range branches {
		if branch != "" && task.Branch == branch {
			return true
		}
	}
	return false
}
