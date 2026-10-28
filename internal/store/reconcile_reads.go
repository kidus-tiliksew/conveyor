package store

import (
	"context"
	"slices"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// AutomaticReviewEventKinds are the only event kinds DecideAutomaticReviewAct
// folds: setup-change supersession (CurrentReviewOrders), seat verdicts
// (InterruptedReviewRecoveryNeeded), round resolutions (ReviewRecoveryNeeded),
// and the automatic acts themselves (AutomaticReviewAct). A fold that starts
// reading another kind must add it here, or the AR-5 sweep stops seeing it
// (feature-bounded-automatic-recovery AR-5; component-persistence).
var AutomaticReviewEventKinds = []string{
	"task.setup.changed", "review.completed", "review.round_completed",
	"review.round_retried", "review.round_recovered",
}

// ReconcileReadStore holds the narrow reads of the reconcile tick's sweeps,
// so a sweep reads the rows its rule can act on instead of every task in the
// workspace with each task's full history (component-runtime, Reconcile tick;
// component-persistence).
type ReconcileReadStore interface {
	// ListAutomaticReviewEvents returns each requested task's events of
	// AutomaticReviewEventKinds in (at, id) order, the order ListEvents
	// returns. Tasks without one are absent.
	ListAutomaticReviewEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
	// ListGitHubLifecycleReconcileTaskIDs returns, in ListTasks order, the
	// workspace tasks the GitHub lifecycle reconcile can act on: every task
	// superseded by a start-over, and every task whose latest plan version is
	// approved and that has no GitHub lifecycle row.
	ListGitHubLifecycleReconcileTaskIDs(ctx context.Context) ([]string, error)
}

func (m *memory) ListAutomaticReviewEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string][]core.Event{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, event := range m.sortedEventsLocked(id) {
			if slices.Contains(AutomaticReviewEventKinds, event.Kind) {
				out[id] = append(out[id], event)
			}
		}
	}
	return out, nil
}

func (m *memory) ListGitHubLifecycleReconcileTaskIDs(ctx context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	candidates := make([]core.Task, 0)
	for _, task := range m.tasks {
		if scoped && workspace != "" && task.Workspace != workspace {
			continue
		}
		versions := m.specs[task.ID]
		_, hasLifecycle := m.github[task.ID]
		approved := len(versions) > 0 && versions[len(versions)-1].Approved
		if task.SupersededBy != "" || approved && !hasLifecycle {
			candidates = append(candidates, task)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	ids := make([]string, len(candidates))
	for i, task := range candidates {
		ids[i] = task.ID
	}
	return ids, nil
}
