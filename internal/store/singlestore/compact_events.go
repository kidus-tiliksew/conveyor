package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// compactEventPayloadColumn removes the governance and served-requirement
// snapshot bodies from work_order.* object payloads in the database, so they
// never cross the connection. Other kinds, non-object payloads, and nested
// keys are returned as stored (component-persistence, Compact task event
// reads).
const compactEventPayloadColumn = `CASE WHEN LEFT(kind,11)='work_order.' AND JSON_GET_TYPE(payload_json)='object' THEN JSON_DELETE_KEY(JSON_DELETE_KEY(payload_json,'governance_snapshot'),'served_requirement_snapshot') ELSE payload_json END`

// ListCompactEvents returns the ListEvents rows and order with the compact
// payload projection; it uses the task timeline predicate of
// documentTaskEvents.
func (s *Store) ListCompactEvents(ctx context.Context, id string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListCompactEvents")
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,`+compactEventPayloadColumn+`,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key ORDER BY at,id`, documentWorkspace(ctx), id)
	if err != nil {
		return nil, err
	}
	return scanDocumentEvents(rows)
}

// ListTaskContextEvents reads only the kinds the task context fold uses, so a
// single-task context read never transfers the task's whole timeline
// (component-persistence, Compact task event reads).
func (s *Store) ListTaskContextEvents(ctx context.Context, id string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskContextEvents")
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,`+compactEventPayloadColumn+`,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND kind IN (?,?,?,?) ORDER BY at,id`, documentWorkspace(ctx), id, store.TaskContextRequirementAdded, store.TaskContextRequirementRemoved, store.TaskContextDesignAdded, store.TaskContextDesignRemoved)
	if err != nil {
		return nil, err
	}
	return scanDocumentEvents(rows)
}
