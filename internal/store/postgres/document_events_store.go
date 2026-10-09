package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ListRequirementEvents(ctx context.Context, requirementID string) ([]core.Event, error) {
	rows, err := s.queries.ListRequirementEvents(ctx, db.ListRequirementEventsParams{
		WorkspaceID: workspace(ctx), RequirementID: requirementID,
	})
	if err != nil {
		return nil, err
	}
	result := make([]core.Event, len(rows))
	for i := range rows {
		result[i] = eventFromDB(rows[i])
	}
	return result, nil
}

func (s *Store) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	rows, err := s.boundary.Query(ctx, `SELECT e.id,e.task_id,e.job_id,e.kind,e.actor_id,e.actor_role,e.payload_json,e.at,e.workspace_id,
		e.payload_json->>'requirement_id'
		FROM events e
		WHERE e.workspace_id=$1 AND e.task_id IS NULL AND e.payload_json->>'requirement_id' IS NOT NULL
		ORDER BY e.payload_json->>'requirement_id',e.at,e.id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.Event{}
	for rows.Next() {
		var event db.Event
		var requirementID string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID,
			&event.ActorRole, &event.PayloadJson, &event.At, &event.WorkspaceID, &requirementID); err != nil {
			return nil, err
		}
		out[requirementID] = append(out[requirementID], eventFromDB(event))
	}
	return out, rows.Err()
}
