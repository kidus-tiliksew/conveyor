package store

import (
	"context"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
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

func (m *memory) MonitorOriginsForTasks(ctx context.Context, taskIDs []string) (map[string]monitor.ObservationRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]monitor.ObservationRecord, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}
	workspace, ok := WorkspaceFromContext(ctx)
	if !ok || workspace == "" {
		return nil, ErrWorkspaceRequired
	}
	kinds := map[string]string{}
	for _, taskID := range taskIDs {
		task, exists := m.tasks[taskID]
		if !exists || task.Workspace != workspace {
			continue
		}
		if kind, monitored := strings.CutPrefix(task.Source, "monitor:"); monitored && kind != "" {
			kinds[taskID] = kind
		}
	}
	for key, record := range m.monitorObservations {
		kind, wanted := kinds[record.TaskID]
		if !wanted || !strings.HasPrefix(key, workspace+"\x00") || string(record.Kind) != kind {
			continue
		}
		current, seen := result[record.TaskID]
		if !seen || monitorOriginBefore(record, current) {
			result[record.TaskID] = record
		}
	}
	return result, nil
}

// monitorOriginBefore orders observations by created_at and then identity.
func monitorOriginBefore(a, b monitor.ObservationRecord) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.Identity() < b.Identity()
}
