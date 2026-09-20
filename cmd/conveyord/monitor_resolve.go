package main

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func assignmentResolveTask(st store.Store, repositoryName, githubSlug string) func(context.Context, string, int) (string, bool, error) {
	return func(ctx context.Context, headRef string, number int) (string, bool, error) {
		tasks, err := st.ListTasks(ctx)
		if err != nil {
			return "", false, err
		}
		eventsByID := make(map[string][]core.Event, len(tasks))
		for _, task := range tasks {
			events, listErr := st.ListEvents(ctx, task.ID)
			if listErr != nil {
				return "", false, listErr
			}
			eventsByID[task.ID] = events
		}
		id, ok := monitor.MatchObservedTask(tasks, eventsByID, repositoryName, githubSlug, headRef, number)
		return id, ok, nil
	}
}
