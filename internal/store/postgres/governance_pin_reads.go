package postgres

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// governancePinProjectionSQL projects a governance_snapshots row aliased gs to
// the fields core.GovernanceReferencePin keeps, in array order. Absent keys
// are stripped, so the decoded pin equals the Go projection of the full
// snapshot. No design body, governed scope, or retired evidence leaves the
// database (component-work-orders invariants 23 and 24).
const governancePinProjectionSQL = `jsonb_build_object(
	'designs', COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
		'id', d.value->'id', 'title', d.value->'title', 'version', d.value->'version',
		'pinned_at_attachment', d.value->'pinned_at_attachment', 'archived', d.value->'archived',
		'superseded_by', d.value->'superseded_by')) ORDER BY d.ordinality)
		FROM jsonb_array_elements(CASE WHEN jsonb_typeof(gs.content->'designs') = 'array' THEN gs.content->'designs' ELSE '[]'::jsonb END) WITH ORDINALITY AS d(value, ordinality)), '[]'::jsonb),
	'decisions', COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
		'id', c.value->'id', 'status', c.value->'status', 'statement', c.value->'statement',
		'superseded_by', c.value->'superseded_by')) ORDER BY c.ordinality)
		FROM jsonb_array_elements(CASE WHEN jsonb_typeof(gs.content->'decisions') = 'array' THEN gs.content->'decisions' ELSE '[]'::jsonb END) WITH ORDINALITY AS c(value, ordinality)), '[]'::jsonb))`

// GetWorkOrderGovernancePin resolves the order's stored digest within the
// selected workspace and returns only the reference projection of its pin.
// The order row contributes its digest alone, so neither snapshot column is
// selected; an order without a pin answers nil.
func (s *Store) GetWorkOrderGovernancePin(ctx context.Context, id string) (*core.GovernanceSnapshot, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetWorkOrderGovernancePin")
	var projected []byte
	err := s.boundary.QueryRow(ctx, `SELECT CASE WHEN gs.digest IS NULL THEN NULL ELSE `+governancePinProjectionSQL+` END
		FROM work_orders wo
		LEFT JOIN governance_snapshots gs ON gs.workspace_id = wo.workspace_id AND gs.digest = wo.governance_snapshot_digest
		WHERE wo.workspace_id = $1 AND wo.id = $2`, workspace(ctx), id).Scan(&projected)
	if err != nil {
		return nil, notFound(err, "work order %s", id)
	}
	if projected == nil {
		return nil, nil
	}
	var pin core.GovernanceSnapshot
	if err = json.Unmarshal(projected, &pin); err != nil {
		return nil, err
	}
	return &pin, nil
}

// GetTaskRepository reads the task's repository column alone.
func (s *Store) GetTaskRepository(ctx context.Context, id string) (string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetTaskRepository")
	var repository string
	if err := s.boundary.QueryRow(ctx, `SELECT repo_name FROM tasks WHERE workspace_id = $1 AND id = $2`, workspace(ctx), id).Scan(&repository); err != nil {
		return "", notFound(err, "task %s", id)
	}
	return repository, nil
}

// ListTaskDesignContextEvents reads only the design attachment kinds, so a
// claim or renewal response never transfers the task's whole timeline.
func (s *Store) ListTaskDesignContextEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskDesignContextEvents")
	rows, err := s.boundary.Query(ctx, `SELECT e.id,COALESCE(e.task_id,''),COALESCE(e.job_id,''),e.kind,COALESCE(e.actor_id,''),COALESCE(e.actor_role,''),`+compactEventPayloadColumn+` AS payload_json,e.at
		FROM events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.task_id = $1 AND t.workspace_id = $2 AND e.kind = ANY($3::text[])
		ORDER BY e.at, e.id`, taskID, workspace(ctx), store.TaskDesignContextEventKinds)
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
