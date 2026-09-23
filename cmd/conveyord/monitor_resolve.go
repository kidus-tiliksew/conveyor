package main

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// assignmentResolveTask loads one poll's repository candidates before any
// matching (req-task-branch-assignment AC-3.4; component-monitor-drift).
func assignmentResolveTask(st store.Store, repositoryName, githubSlug string) func(context.Context) (monitor.TaskResolver, error) {
	return func(ctx context.Context) (monitor.TaskResolver, error) {
		tasks, err := st.ListTasksFiltered(ctx, store.TaskFilter{Repositories: []string{repositoryName}})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(tasks))
		for _, task := range tasks {
			ids = append(ids, task.ID)
		}
		eventsByID, err := st.ListMonitorPullRequestEventsForTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
		return func(_ context.Context, headRef string, number int) (string, bool, error) {
			id, ok := monitor.MatchObservedTask(tasks, eventsByID, repositoryName, githubSlug, headRef, number)
			return id, ok, nil
		}, nil
	}
}
