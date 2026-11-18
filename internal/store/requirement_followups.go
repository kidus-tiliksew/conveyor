package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RequirementFollowUpReader selects only summaries for exact intake keys.
type RequirementFollowUpReader interface {
	ListRequirementFollowUps(context.Context, []string) (map[string]RequirementDetailTask, error)
}

// ListRequirementFollowUps reads one summary per exact intake key in the
// request's workspace, the scoped batch the SQL readers select with
// workspace_id and intake_key (component-persistence). An intake key of
// another workspace never selects a row.
func (m *memory) ListRequirementFollowUps(ctx context.Context, keys []string) (map[string]RequirementDetailTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	wanted := make(map[string]bool, len(keys))
	for _, key := range UniqueTaskIDs(keys) {
		if key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return map[string]RequirementDetailTask{}, nil
	}
	matches := make(map[string]core.Task, len(wanted))
	for _, task := range m.tasks {
		if !wanted[task.IntakeKey] || scoped && task.Workspace != workspace {
			continue
		}
		if match, ok := matches[task.IntakeKey]; !ok || task.ID < match.ID {
			matches[task.IntakeKey] = task
		}
	}
	out := make(map[string]RequirementDetailTask, len(matches))
	for key, task := range matches {
		out[key] = RequirementDetailTask{ID: task.ID, Title: task.Title, State: task.State}
	}
	return out, nil
}
