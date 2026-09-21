package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"time"
)

func (s *Store) supersedeVerificationTx(ctx context.Context, tx pgx.Tx, taskID, head string) error {
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='verify' AND state IN ('queued','claimed') FOR UPDATE", workspace(ctx), taskID)
	if err != nil {
		return err
	}
	var orders []core.WorkOrder
	for rows.Next() {
		o, e := scanWorkOrder(rows)
		if e != nil {
			rows.Close()
			return e
		}
		orders = append(orders, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, old := range orders {
		o, changed := core.SupersedeVerificationOrder(old, head, now)
		if !changed {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE work_orders SET state='cancelled',last_attempt_id=$1,claimant_id='',session_id='',attempt_id='',client_token_hash='',worker_id='',lease_expires_at=NULL,retry_suppressed=true,retry_suppression_reason='superseded head',updated_at=$2 WHERE workspace_id=$3 AND id=$4`, o.LastAttemptID, now, workspace(ctx), o.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1 WHERE id=$3 AND task_id IN (SELECT id FROM tasks WHERE workspace_id=$2)`, now, workspace(ctx), o.JobID); err != nil {
			return err
		}
		if err = insertEvent(ctx, s.queries.WithTx(tx), core.Event{TaskID: taskID, JobID: o.JobID, Kind: "work_order.cancelled", Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "session_id": old.SessionID, "attempt_id": old.AttemptID, "reason": "superseded verification head", "head_sha": head}), At: now}); err != nil {
			return err
		}
	}
	return nil
}
