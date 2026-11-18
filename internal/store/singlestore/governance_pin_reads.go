package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// GetWorkOrderGovernancePin resolves the order's stored digest within the
// selected workspace and returns only the fields core.GovernanceReferencePin
// keeps. The order row contributes its digest alone, and each design and
// decision row is projected to its reference fields in the database, so
// neither snapshot column, no design body, and no decision rationale crosses
// the connection (component-work-orders invariants 23 and 24). An order
// without a pin, or whose digest names no stored snapshot, answers nil, as
// the full read decodes no snapshot for it. Element order is not part of the
// contract: core.ProjectGovernanceReferences orders the delivered list.
func (s *Store) GetWorkOrderGovernancePin(ctx context.Context, id string) (*core.GovernanceSnapshot, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetWorkOrderGovernancePin")
	var digest string
	err := documentRow(ctx, s.db, `SELECT COALESCE(gs.digest,'') FROM work_orders wo
		LEFT JOIN governance_snapshots gs ON gs.workspace_id=wo.workspace_id AND gs.digest=wo.governance_snapshot_digest
		WHERE wo.workspace_id=? AND wo.id=?`, documentWorkspace(ctx), id).Scan(&digest)
	if err != nil {
		return nil, notFound(err, "work order %s", id)
	}
	if digest == "" {
		return nil, nil
	}
	pin := core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
	rows, err := documentRows(ctx, s.db, `SELECT COALESCE(JSON_EXTRACT_STRING(d.table_col,'id'),''),COALESCE(JSON_EXTRACT_STRING(d.table_col,'title'),''),
		COALESCE(JSON_EXTRACT_BIGINT(d.table_col,'version'),0),JSON_EXTRACT_JSON(d.table_col,'pinned_at_attachment'),JSON_EXTRACT_JSON(d.table_col,'archived'),JSON_EXTRACT_JSON(d.table_col,'superseded_by')
		FROM governance_snapshots gs JOIN TABLE(JSON_TO_ARRAY(`+governancePinArraySQL("designs")+`)) d
		WHERE gs.workspace_id=? AND gs.digest=?`, documentWorkspace(ctx), digest)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var design core.GovernanceDesignContext
		var version int64
		var pinned, archived, superseded []byte
		if err = rows.Scan(&design.ID, &design.Title, &version, &pinned, &archived, &superseded); err != nil {
			_ = rows.Close()
			return nil, err
		}
		design.Version = int(version)
		if err = decodeOptionalJSON(pinned, &design.PinnedAtAttachment); err == nil {
			err = decodeOptionalJSON(archived, &design.Archived)
		}
		if err == nil {
			err = decodeOptionalJSON(superseded, &design.SupersededBy)
		}
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		pin.Designs = append(pin.Designs, design)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = documentRows(ctx, s.db, `SELECT COALESCE(JSON_EXTRACT_STRING(c.table_col,'id'),''),COALESCE(JSON_EXTRACT_STRING(c.table_col,'status'),''),
		COALESCE(JSON_EXTRACT_STRING(c.table_col,'statement'),''),COALESCE(JSON_EXTRACT_STRING(c.table_col,'superseded_by'),'')
		FROM governance_snapshots gs JOIN TABLE(JSON_TO_ARRAY(`+governancePinArraySQL("decisions")+`)) c
		WHERE gs.workspace_id=? AND gs.digest=?`, documentWorkspace(ctx), digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var decision core.Decision
		var status string
		if err = rows.Scan(&decision.ID, &status, &decision.Statement, &decision.SupersededBy); err != nil {
			return nil, err
		}
		decision.Status = core.DecisionStatus(status)
		pin.Decisions = append(pin.Decisions, decision)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return &pin, nil
}

// governancePinArraySQL is the stored pin's collection key as a JSON array.
// JSON_TO_ARRAY refuses any other input, and a stored pin can hold the
// collection as JSON null, because a claim pins an empty collection as a nil
// slice, or can lack the key. Both read as an empty array, as decoding the
// whole snapshot reads them as no documents.
func governancePinArraySQL(key string) string {
	extract := `JSON_EXTRACT_JSON(gs.content,'` + key + `')`
	return `CASE WHEN JSON_GET_TYPE(` + extract + `)='array' THEN ` + extract + ` ELSE '[]' END`
}

// decodeOptionalJSON decodes one extracted JSON value; an absent or null
// value leaves target unchanged, as decoding the whole snapshot does.
func decodeOptionalJSON(raw []byte, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// GetTaskRepository reads the task's repository column alone.
func (s *Store) GetTaskRepository(ctx context.Context, id string) (string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetTaskRepository")
	var repository sql.NullString
	if err := documentRow(ctx, s.db, `SELECT repo_name FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id).Scan(&repository); err != nil {
		return "", notFound(err, "task %s", id)
	}
	return repository.String, nil
}

// ListTaskDesignContextEvents reads only the design attachment kinds, so a
// claim or renewal response never transfers the task's whole timeline.
func (s *Store) ListTaskDesignContextEvents(ctx context.Context, id string) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskDesignContextEvents")
	rows, err := documentRows(ctx, s.db, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,`+compactEventPayloadColumn+`,at FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND kind IN (?,?) ORDER BY at,id`, documentWorkspace(ctx), id, store.TaskContextDesignAdded, store.TaskContextDesignRemoved)
	if err != nil {
		return nil, err
	}
	return scanDocumentEvents(rows)
}
