package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// riskAcceptanceHoldTx answers store.RiskAcceptanceHold for an approved review
// round inside the acceptance transaction. The task's claim events and
// implementation orders, the stored trust list, and each author's role binding
// are read through tx, never through another connection, so a read error rolls
// the whole acceptance back and leaves no verdict, settlement, or transition
// (component-risk-review invariants 16 and 19; component-review invariant 21).
func riskAcceptanceHoldTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, ratings []core.RiskRating) (bool, error) {
	_, hold, err := store.RiskAcceptanceHold(task, ratings, store.RiskAcceptanceReads{
		Claims: func() ([]core.Event, []core.WorkOrder, error) {
			return riskAcceptanceClaimsTx(ctx, tx, task.ID)
		},
		TrustedAuthors: func() ([]string, error) {
			row, err := q.GetWorkspaceConfig(ctx, workspace(ctx))
			if err != nil {
				return nil, notFound(err, "workspace %s", workspace(ctx))
			}
			return store.StoredTrustedAuthors([]byte(row.ConfigYaml), task.Repo)
		},
		OperateGates: func(userID string) (bool, error) {
			var role core.WorkspaceRole
			err := tx.QueryRow(ctx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspace(ctx), userID).Scan(&role)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return core.RoleAllows(role, core.CapabilityOperateGates), nil
		},
	})
	return hold, err
}

// riskAcceptanceClaimsTx reads the inputs of store.TaskAuthors through tx:
// every recorded work_order.claimed event of the task and the claimant of
// each current implementation order.
func riskAcceptanceClaimsTx(ctx context.Context, tx pgx.Tx, taskID string) ([]core.Event, []core.WorkOrder, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='work_order.claimed' ORDER BY id`, workspace(ctx), taskID)
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
	rows, err = tx.Query(ctx, `SELECT id, claimant_id, worker_id FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='implement' ORDER BY created_at, id`, workspace(ctx), taskID)
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
