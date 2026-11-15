package postgres

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Requirement-detail batch reads (component-persistence;
// component-document-corpus): one workspace-scoped set query per selection,
// whatever the number of requested tasks. Neither read transfers a task body
// or plan content.

// ListRequirementDetailTasks reads the requested tasks' ID, title, state,
// and child presence in one query.
func (s *Store) ListRequirementDetailTasks(ctx context.Context, taskIDs []string) (map[string]store.RequirementDetailTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListRequirementDetailTasks")
	out := map[string]store.RequirementDetailTask{}
	ids := store.UniqueTaskIDs(taskIDs)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT t.id, t.title, t.state,
	EXISTS (SELECT 1 FROM tasks child WHERE child.workspace_id = t.workspace_id AND child.parent_task_id = t.id)
FROM tasks t
WHERE t.workspace_id = $1 AND t.id = ANY($2::text[])`, workspace(ctx), ids)
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
	rows, err := s.boundary.Query(ctx, `SELECT s.task_id, s.version, s.approved
FROM (
	SELECT ls.task_id, max(ls.version) AS version
	FROM task_specs ls
	JOIN tasks t ON t.id = ls.task_id
	WHERE t.workspace_id = $1 AND ls.task_id = ANY($2::text[])
	GROUP BY ls.task_id
) latest
JOIN task_specs s ON s.task_id = latest.task_id AND s.version = latest.version`, workspace(ctx), ids)
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
