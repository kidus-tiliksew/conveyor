package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ListMonitorPullRequestEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	result := make(map[string][]core.Event, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id,e.task_id,e.job_id,e.kind,e.actor_id,e.actor_role,e.payload_json,e.at,e.workspace_id
		FROM events e
		WHERE e.workspace_id=$1 AND e.task_id=ANY($2::text[]) AND e.kind='pull_request.opened'
		ORDER BY e.task_id,e.at,e.id`, workspace(ctx), taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var event db.Event
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID,
			&event.ActorRole, &event.PayloadJson, &event.At, &event.WorkspaceID); err != nil {
			return nil, err
		}
		converted := eventFromDB(event)
		result[converted.TaskID] = append(result[converted.TaskID], converted)
	}
	return result, rows.Err()
}
