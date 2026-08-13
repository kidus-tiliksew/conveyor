// Package worktreemaint reconciles operator-owned linked worktrees after
// terminal task transitions. Failures never roll back terminal state and are
// retried on the next reconciliation pass (design-git-delivery).
package worktreemaint

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const CleanupCompletedEvent = "worktree.cleanup_completed"

type Result struct {
	Cleaned int
	Pruned  int
	Failed  int
}

type Maintainer struct {
	Store          store.Store
	ConfigProvider func(context.Context) (*config.Config, error)
	StartDir       string
	Logf           func(string, ...any)
}

// Reconcile prunes both cache and primary-checkout registrations, then cleans
// terminal task worktrees. Repository-local failures are logged and isolated
// so one unavailable or dirty checkout cannot block the rest of the pass.
func (m *Maintainer) Reconcile(ctx context.Context) (Result, error) {
	var result Result
	cfg, err := m.ConfigProvider(ctx)
	if err != nil {
		return result, fmt.Errorf("load workspace configuration: %w", err)
	}
	tasks, err := m.Store.ListTasks(ctx)
	if err != nil {
		return result, fmt.Errorf("list tasks for worktree maintenance: %w", err)
	}
	if err := gitx.NewManager(cfg.CacheDir, "").Prune(ctx); err != nil {
		result.Failed++
		m.logf("worktree maintenance workspace %s cache prune: %v", cfg.Workspace, err)
	}

	type resolvedRepository struct {
		root string
		err  error
	}
	roots := make(map[string]resolvedRepository, len(cfg.Repos))
	resolve := func(repo config.Repo) (string, error) {
		if cached, ok := roots[repo.Name]; ok {
			return cached.root, cached.err
		}
		startDir := m.StartDir
		if repo.Checkout != "" {
			startDir = repo.Checkout
		}
		root, resolveErr := gitx.ResolvePrimaryCheckout(ctx, startDir, repo.Name, repo.URL)
		roots[repo.Name] = resolvedRepository{root: root, err: resolveErr}
		return root, resolveErr
	}

	for _, repo := range cfg.Repos {
		root, resolveErr := resolve(repo)
		if resolveErr != nil {
			result.Failed++
			m.logf("worktree maintenance workspace %s repository %s unavailable: %v", cfg.Workspace, repo.Name, resolveErr)
			continue
		}
		if pruneErr := gitx.PruneRepository(ctx, root); pruneErr != nil {
			result.Failed++
			m.logf("worktree maintenance workspace %s repository %s primary prune: %v", cfg.Workspace, repo.Name, pruneErr)
			continue
		}
		result.Pruned++
	}

	for _, task := range tasks {
		if !core.TaskTerminal(task.State) {
			continue
		}
		completed, countErr := m.Store.CountEvents(ctx, task.ID, CleanupCompletedEvent)
		if countErr != nil {
			result.Failed++
			m.logf("worktree cleanup workspace %s task %s repository %s completion lookup: %v", cfg.Workspace, task.ID, task.Repo, countErr)
			continue
		}
		if completed != 0 {
			continue
		}
		repo, ok := cfg.Repo(task.Repo)
		if !ok {
			result.Failed++
			m.logf("worktree cleanup workspace %s task %s repository %s is not configured", cfg.Workspace, task.ID, task.Repo)
			continue
		}
		root, resolveErr := resolve(repo)
		if resolveErr != nil {
			result.Failed++
			m.logf("worktree cleanup workspace %s task %s repository %s unavailable: %v", cfg.Workspace, task.ID, task.Repo, resolveErr)
			continue
		}
		cleanup, cleanupErr := gitx.CleanupTaskWorktree(ctx, root, task.Branch)
		if cleanupErr != nil {
			result.Failed++
			m.logf("worktree cleanup workspace %s task %s repository %s branch %s: %v", cfg.Workspace, task.ID, task.Repo, task.Branch, cleanupErr)
			continue
		}
		for _, warning := range cleanup.ProcessWarnings {
			m.logf("worktree cleanup workspace %s task %s repository %s: warning: %s", cfg.Workspace, task.ID, task.Repo, warning)
		}
		if appendErr := m.Store.AppendEvent(ctx, core.Event{
			TaskID: task.ID,
			Kind:   CleanupCompletedEvent,
			Payload: core.JSONPayload(map[string]any{
				"workspace": cfg.Workspace, "repository": task.Repo, "branch": task.Branch,
				"worktree": cleanup.Worktree, "branch_result": cleanup.Branch, "path": cleanup.Path,
				"process_warnings": cleanup.ProcessWarnings,
			}),
		}); appendErr != nil {
			result.Failed++
			m.logf("worktree cleanup workspace %s task %s repository %s completion record: %v", cfg.Workspace, task.ID, task.Repo, appendErr)
			continue
		}
		result.Cleaned++
	}
	return result, nil
}

func (m *Maintainer) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
