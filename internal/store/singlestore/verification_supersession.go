package singlestore

import (
	"context"
	"database/sql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"time"
)

func (s *Store) supersedeVerificationTx(ctx context.Context, tx *sql.Tx, taskID, head string) error {
	orders, err := s.listOrders(ctx, tx, []string{taskID})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, old := range orders {
		o, changed := core.SupersedeVerificationOrder(old, head, now)
		if !changed {
			continue
		}
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=? WHERE workspace_id=? AND id=?`, now, documentWorkspace(ctx), o.JobID); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, JobID: o.JobID, Kind: "work_order.cancelled", Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "session_id": old.SessionID, "attempt_id": old.AttemptID, "reason": "superseded verification head", "head_sha": head}), At: now}); err != nil {
			return err
		}
	}
	return nil
}
