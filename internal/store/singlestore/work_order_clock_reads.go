package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
)

// clockWorkOrderColumns is workOrderColumns without the served-requirement
// and governance snapshot bodies. It keeps governance_snapshot_digest, the
// small content address that lifecycle events and projections carry, so a
// lifecycle read never moves a pin body (component-work-orders invariant 23).
var clockWorkOrderColumns = strings.Replace(workOrderColumns, "served_requirement_snapshot, governance_snapshot_digest, "+governanceSnapshotContentSQL+" AS governance_snapshot", "NULL, governance_snapshot_digest, NULL", 1)

// listTaskOrdersInStatesTx reads one task's orders in the given states with
// clockWorkOrderColumns, in the order listOrders returns them. A nil states
// selects every state.
func (s *Store) listTaskOrdersInStatesTx(ctx context.Context, db s2log.Executor, taskID string, states ...core.WorkOrderState) ([]core.WorkOrder, error) {
	query := `SELECT ` + clockWorkOrderColumns + ` FROM work_orders WHERE workspace_id=? AND task_id=?`
	args := []any{documentWorkspace(ctx), taskID}
	if len(states) > 0 {
		query += ` AND state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(states)), ",") + `)`
		for _, state := range states {
			args = append(args, string(state))
		}
	}
	rows, err := documentRows(ctx, db, query+` ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.WorkOrder{}
	for rows.Next() {
		o, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// hydrateGovernanceSnapshotTx loads the pinned governance content of an order
// read with clockWorkOrderColumns. Only a transition whose event lists the
// pinned documents by reference needs it; a no-op clock never calls it.
func hydrateGovernanceSnapshotTx(ctx context.Context, tx *sql.Tx, o *core.WorkOrder) error {
	if o.GovernanceSnapshot != nil || o.GovernanceSnapshotDigest == "" {
		return nil
	}
	var content []byte
	err := documentRow(ctx, tx, `SELECT content FROM governance_snapshots WHERE workspace_id=? AND digest=?`, documentWorkspace(ctx), o.GovernanceSnapshotDigest).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot core.GovernanceSnapshot
	if err = json.Unmarshal(content, &snapshot); err != nil {
		return err
	}
	o.GovernanceSnapshot = &snapshot
	return nil
}

// orderLifecycleWrite updates every lifecycle column of an order and leaves
// served_requirement_snapshot and governance_snapshot_digest untouched, so a
// row read without its pin bodies can be written without clearing them.
// orderWrite remains the full-row writer for callers that hold both pins.
func orderLifecycleWrite(ctx context.Context, tx *sql.Tx, o core.WorkOrder) error {
	values := orderValues(o)
	for _, key := range []string{"id", "task_id", "job_id", "created_at", "served_requirement_snapshot", "governance_snapshot_digest"} {
		delete(values, key)
	}
	_, err := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.ID}})
	return err
}
