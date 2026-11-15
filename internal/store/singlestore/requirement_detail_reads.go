package singlestore

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Requirement-detail batch reads (component-persistence;
// component-document-corpus): one workspace-scoped set query per selection
// with one parameterized IN list, whatever the number of requested tasks.
// Neither read transfers a task body or plan content.

// ListRequirementDetailTasks reads the requested tasks' ID, title, state,
// and child presence in one query.
func (s *Store) ListRequirementDetailTasks(ctx context.Context, taskIDs []string) (map[string]store.RequirementDetailTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementDetailTasks")
	out := map[string]store.RequirementDetailTask{}
	ids := store.UniqueTaskIDs(taskIDs)
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	// The child predicate joins the distinct parents among the requested
	// IDs instead of a correlated subquery in the select list.
	marks := taskMapMarks(len(ids))
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	args = append(args, taskMapArgs(ids)...)
	rows, err := documentRows(ctx, s.db, `SELECT t.id,t.title,t.state,parents.parent_task_id IS NOT NULL
FROM tasks t
LEFT JOIN (SELECT DISTINCT parent_task_id FROM tasks
	WHERE workspace_id=? AND parent_task_id IN (`+marks+`)) parents ON parents.parent_task_id=t.id
WHERE t.workspace_id=? AND t.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var task store.RequirementDetailTask
		var state string
		if err = rows.Scan(&task.ID, &task.Title, &state, &task.HasChildren); err != nil {
			return nil, fmt.Errorf("scan requirement detail task: %w", err)
		}
		task.State = core.TaskState(state)
		out[task.ID] = task
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLatestPlanSummaries reads the version and approval flag of each
// requested task's latest plan version in one query.
func (s *Store) ListLatestPlanSummaries(ctx context.Context, taskIDs []string) (map[string]store.PlanSummary, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListLatestPlanSummaries")
	out := map[string]store.PlanSummary{}
	ids := store.UniqueTaskIDs(taskIDs)
	if len(ids) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(ids)...)
	args = append(args, ws)
	rows, err := documentRows(ctx, s.db, `SELECT s.task_id,s.version,s.approved
FROM (SELECT task_id,MAX(version) AS version FROM task_specs
	WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(ids))+`) GROUP BY task_id) latest
JOIN task_specs s ON s.workspace_id=? AND s.task_id=latest.task_id AND s.version=latest.version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var plan store.PlanSummary
		if err = rows.Scan(&taskID, &plan.Version, &plan.Approved); err != nil {
			return nil, fmt.Errorf("scan latest plan summary: %w", err)
		}
		out[taskID] = plan
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
