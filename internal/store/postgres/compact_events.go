package postgres

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// compactEventPayloadColumn is the payload expression the ListCompactEvents
// query selects, for reads that select events directly. An events row aliased
// e must be in scope (component-task-lifecycle, Compact event reads).
const compactEventPayloadColumn = `CASE WHEN left(e.kind, 11) = 'work_order.' AND jsonb_typeof(e.payload_json) = 'object' THEN e.payload_json - 'governance_snapshot' - 'served_requirement_snapshot' ELSE e.payload_json END::jsonb`

// ListCompactEvents returns the ListEvents rows and order. The query removes
// the governance and served-requirement snapshot bodies from work_order.*
// object payloads in the database, so they never cross the connection
// (component-task-lifecycle invariants 51 and 52).
func (s *Store) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCompactEvents")
	rows, err := s.queries.ListCompactEvents(ctx, db.ListCompactEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Event, len(rows))
	for i := range rows {
		result[i] = eventFromDB(rows[i])
	}
	return result, nil
}

// ListTaskContextEvents reads only the kinds the task context fold uses, so a
// single-task context read never transfers the task's whole timeline
// (component-task-lifecycle invariant 53).
func (s *Store) ListTaskContextEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskContextEvents")
	rows, err := s.boundary.Query(ctx, `SELECT e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),e.kind,COALESCE(e.actor_id,''),COALESCE(e.actor_role,''),`+compactEventPayloadColumn+` AS payload_json,e.at
		FROM events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.task_id = $1 AND t.workspace_id = $2 AND e.kind = ANY($3::text[])
		ORDER BY e.at, e.id`, taskID, workspace(ctx), store.TaskContextEventKinds[:])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []core.Event{}
	for rows.Next() {
		var event core.Event
		if err = rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &event.ActorRole, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}
