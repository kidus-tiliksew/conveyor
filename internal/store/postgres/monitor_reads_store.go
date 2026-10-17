package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
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

func (s *Store) MonitorOriginsForTasks(ctx context.Context, taskIDs []string) (map[string]monitor.ObservationRecord, error) {
	result := make(map[string]monitor.ObservationRecord, len(taskIDs))
	if len(taskIDs) == 0 {
		return result, nil
	}
	ws := workspace(ctx)
	rows, err := s.boundary.Query(ctx, `SELECT `+observationColumns+` FROM monitor_observations o
		WHERE o.workspace_id=$1 AND o.task_id=ANY($2::text[])
		  AND EXISTS (SELECT 1 FROM tasks t WHERE t.workspace_id=o.workspace_id AND t.id=o.task_id AND t.source='monitor:'||o.kind)
		ORDER BY o.task_id,o.created_at,o.identity`, ws, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanObservation(rows, ws)
		if err != nil {
			return nil, err
		}
		if _, seen := result[record.TaskID]; !seen {
			result[record.TaskID] = record
		}
	}
	return result, rows.Err()
}
