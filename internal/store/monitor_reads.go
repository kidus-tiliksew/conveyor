package store

import (
	"context"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) ListMonitorPullRequestEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string][]core.Event, len(taskIDs))
	workspace, scoped := WorkspaceFromContext(ctx)
	for _, taskID := range taskIDs {
		task, exists := m.tasks[taskID]
		if !exists || scoped && task.Workspace != workspace {
			continue
		}
		events := make([]core.Event, 0)
		for _, event := range m.events[taskID] {
			switch event.Kind {
			case "pull_request.opened":
				events = append(events, event)
			}
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].At.Equal(events[j].At) {
				return events[i].ID < events[j].ID
			}
			return events[i].At.Before(events[j].At)
		})
		if len(events) > 0 {
			result[taskID] = events
		}
	}
	return result, nil
}
