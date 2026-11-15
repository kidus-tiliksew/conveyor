package singlestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// riskAcceptanceHoldTx answers store.RiskAcceptanceHold for an approved review
// round inside the acceptance transaction. The task's claim events and
// implementation orders, the stored trust list, and each author's role binding
// are read through tx, never through another connection, so a read error rolls
// the whole acceptance back and leaves no verdict, settlement, or transition
// (component-risk-review invariants 16 and 19; component-review invariant 21).
func riskAcceptanceHoldTx(ctx context.Context, tx *sql.Tx, task core.Task, ratings []core.RiskRating) (bool, error) {
	ws := documentWorkspace(ctx)
	_, hold, err := store.RiskAcceptanceHold(task, ratings, store.RiskAcceptanceReads{
		Claims: func() ([]core.Event, []core.WorkOrder, error) {
			return riskAcceptanceClaimsTx(ctx, tx, ws, task.ID)
		},
		TrustedAuthors: func() ([]string, error) {
			var data string
			err := backendRow(ctx, tx, `SELECT config_yaml FROM workspaces WHERE id=?`, ws).Scan(&data)
			if errors.Is(err, sql.ErrNoRows) {
				err = store.ErrNotFound
			}
			if err != nil {
				return nil, translateBackendConflict(err)
			}
			return store.StoredTrustedAuthors([]byte(data), task.Repo)
		},
		OperateGates: func(userID string) (bool, error) {
			var role core.WorkspaceRole
			err := backendRow(ctx, tx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=? AND user_id=?`, ws, userID).Scan(&role)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, translateBackendConflict(err)
			}
			return core.RoleAllows(role, core.CapabilityOperateGates), nil
		},
	})
	return hold, err
}

// riskAcceptanceClaimsTx reads the inputs of store.TaskAuthors through tx:
// every recorded work_order.claimed event of the task and the claimant of
// each current implementation order.
func riskAcceptanceClaimsTx(ctx context.Context, tx *sql.Tx, ws, taskID string) ([]core.Event, []core.WorkOrder, error) {
	rows, err := backendRows(ctx, tx, `SELECT payload_json FROM events_columnstore WHERE workspace_id=? AND event_shard_key=? AND task_id=event_shard_key AND kind='work_order.claimed' ORDER BY at,id`, ws, taskID)
	if err != nil {
		return nil, nil, err
	}
	var events []core.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, nil, err
		}
		events = append(events, core.Event{TaskID: taskID, Kind: "work_order.claimed", Payload: payload})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = backendRows(ctx, tx, `SELECT id,claimant_id,worker_id FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='implement' ORDER BY created_at,id`, ws, taskID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var orders []core.WorkOrder
	for rows.Next() {
		order := core.WorkOrder{TaskID: taskID, Stage: core.StageImplement}
		if err := rows.Scan(&order.ID, &order.ClaimantID, &order.WorkerID); err != nil {
			return nil, nil, err
		}
		orders = append(orders, order)
	}
	return events, orders, rows.Err()
}
