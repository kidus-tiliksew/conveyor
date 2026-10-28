package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.ReconcileReadStore = (*Store)(nil)

// ListAutomaticReviewEvents reads only the AR-5 fold's event kinds for the
// requested tasks in one query, in the (at, id) order ListEvents returns.
func (s *Store) ListAutomaticReviewEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id
FROM events e
JOIN tasks t ON t.id = e.task_id
WHERE t.workspace_id = $1 AND e.task_id = ANY($2::text[]) AND e.kind = ANY($3::text[])
ORDER BY e.task_id, e.at, e.id`, workspace(ctx), taskIDs, store.AutomaticReviewEventKinds)
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.Event])
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		converted := eventFromDB(event)
		out[converted.TaskID] = append(out[converted.TaskID], converted)
	}
	return out, nil
}

// ListGitHubLifecycleReconcileTaskIDs evaluates the lifecycle reconcile's
// candidate predicate in one query. The latest plan version is the one
// GetLatestSpecVersion returns, and the order is ListTasks's.
func (s *Store) ListGitHubLifecycleReconcileTaskIDs(ctx context.Context) ([]string, error) {
	rows, err := s.boundary.Query(ctx, `SELECT t.id
FROM tasks t
LEFT JOIN github_lifecycles g ON g.workspace_id = t.workspace_id AND g.task_id = t.id
LEFT JOIN (
	SELECT DISTINCT ON (s.task_id) s.task_id, s.approved
	FROM task_specs s
	JOIN tasks owner ON owner.id = s.task_id
	WHERE owner.workspace_id = $1
	ORDER BY s.task_id, s.version DESC
) latest ON latest.task_id = t.id
WHERE t.workspace_id = $1
  AND (COALESCE(t.superseded_by, '') <> '' OR (g.task_id IS NULL AND COALESCE(latest.approved, false)))
ORDER BY t.created_at DESC, t.id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
