package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type ReviewPolicyReconciliationReader interface {
	ListReviewPolicyReconciliationTasks(context.Context) ([]core.Task, error)
}

func (m *memory) ListReviewPolicyReconciliationTasks(ctx context.Context) ([]core.Task, error) {
	tasks, err := m.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	statuses, err := m.ListOpenReviewPolicyStatuses(ctx)
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for _, status := range statuses {
		open[status.TaskID] = true
	}
	out := []core.Task{}
	for _, task := range tasks {
		if !core.TaskTerminal(task.State) || open[task.ID] {
			out = append(out, core.Task{ID: task.ID, State: task.State, SetupContract: task.SetupContract})
		}
	}
	return out, nil
}
