package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

var _ store.ReconcileReadStore = (*Store)(nil)

// ListAutomaticReviewEvents reads only the AR-5 fold's event kinds for the
// requested tasks in one query, in the (at, id) order ListEvents returns.
func (s *Store) ListAutomaticReviewEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	out := map[string][]core.Event{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]any{ws}, taskMapArgs(taskIDs)...)
	args = append(args, taskMapArgs(store.AutomaticReviewEventKinds)...)
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at FROM events
WHERE workspace_id=? AND task_id IN (`+taskMapMarks(len(taskIDs))+`) AND kind IN (`+taskMapMarks(len(store.AutomaticReviewEventKinds))+`)
ORDER BY task_id,at,id`, args...)
	if err != nil {
		return nil, err
	}
	events, err := scanDocumentEvents(rows)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		out[event.TaskID] = append(out[event.TaskID], event)
	}
	return out, nil
}

// ListGitHubLifecycleReconcileTaskIDs evaluates the lifecycle reconcile's
// candidate predicate in one query without correlated subqueries. The latest
// plan version is the one GetLatestSpecVersion returns, and the order is
// ListTasks's.
func (s *Store) ListGitHubLifecycleReconcileTaskIDs(ctx context.Context) ([]string, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT t.id FROM tasks t
LEFT JOIN github_lifecycles g ON g.workspace_id=t.workspace_id AND g.task_id=t.id
LEFT JOIN (SELECT task_id, MAX(version) AS version FROM task_specs WHERE workspace_id=? GROUP BY task_id) latest ON latest.task_id=t.id
LEFT JOIN task_specs spec ON spec.workspace_id=t.workspace_id AND spec.task_id=t.id AND spec.version=latest.version
WHERE t.workspace_id=? AND (COALESCE(t.superseded_by,'')<>'' OR (g.task_id IS NULL AND COALESCE(spec.approved,false)))
ORDER BY t.created_at DESC,t.id ASC`, ws, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
