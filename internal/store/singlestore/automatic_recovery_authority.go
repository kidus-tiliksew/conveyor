package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// answeredAuthorityElapsedSQL selects a released authority_conflict implement
// order on every tick, whatever the recovery bound
// (feature-bounded-automatic-recovery AR-3a).
const answeredAuthorityElapsedSQL = ` OR (
			stage='implement' AND state='queued' AND last_attempt_outcome='released'
			AND JSON_EXTRACT_STRING(checkpoint,'class')='authority_conflict'
			AND EXISTS (SELECT 1 FROM tasks t WHERE t.workspace_id=work_orders.workspace_id AND t.id=work_orders.task_id AND t.state IN ('queued','running'))
		)`

// recoverAnsweredAuthorityTx runs inside ApplyWorkOrderClock's task
// transaction, which holds the task-operation lock row. It then takes
// document-corpus:<workspace>, the key every document write takes, so the
// citation read and the recovery commit see one corpus state
// (component-persistence).
func (s *Store) recoverAnsweredAuthorityTx(ctx context.Context, tx *sql.Tx, task core.Task, order core.WorkOrder, queueTimeout time.Duration, now time.Time) (bool, error) {
	ws := documentWorkspace(ctx)
	if err := lockKey(ctx, tx, "document-corpus:"+ws); err != nil {
		return false, err
	}
	documents, err := authorityDocumentsTx(ctx, tx, ws, store.AnsweredAuthorityDocumentIDs(order))
	if err != nil {
		return false, err
	}
	decision := store.DecideAnsweredAuthorityRecovery(task, order, documents)
	if !decision.Recover {
		return false, nil
	}
	var duplicate bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_order_recoveries WHERE workspace_id=? AND work_order_id=? AND request_id=?)`, ws, order.ID, decision.RequestID).Scan(&duplicate); err != nil || duplicate {
		return false, err
	}
	var releaseEventID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE workspace_id=? AND task_id=? AND kind='work_order.released' AND JSON_EXTRACT_STRING(payload_json,'attempt_id')=? ORDER BY id DESC LIMIT 1`, ws, order.TaskID, order.LastAttemptID).Scan(&releaseEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	payload := store.AuthorityConfirmedEventPayload(ws, order, decision, releaseEventID)
	// The ledger fields are not written: the act does not count against the
	// DEC-62 bound (req-260820-394cac AC-2.6).
	clearOrderClaim(&order)
	order.ClearExecutionPins()
	order.ExecutionStartedAt = time.Time{}
	order.ExecutionDeadline = time.Time{}
	order.LastAttemptOutcome = ""
	order.RetrySuppressed = false
	order.RetrySuppressionReason = ""
	order.AutomaticRetryCount = 0
	order.NextRetryAt = time.Time{}
	order.QueueEnteredAt = now
	order.QueueDeadline = now.Add(queueTimeout)
	order.RedispatchCount++
	order.OperatorDirection = decision.Direction
	order.Checkpoint = nil
	order.UpdatedAt = now
	if err = orderWrite(ctx, tx, order); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES (?,?,?,?)`, ws, order.ID, decision.RequestID, now); err != nil {
		return false, err
	}
	if err = repinTaskDesignContextTx(ctx, tx, ws, order.TaskID, now); err != nil {
		return false, err
	}
	if err = resetOrderJob(ctx, tx, order, now); err != nil {
		return false, err
	}
	eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+order.JobID))
	if err = taskEvent(eventCtx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.recovered", Payload: core.JSONPayload(payload), At: now}); err != nil {
		return false, err
	}
	return true, nil
}

// authorityDocumentsTx reads each cited document under the corpus lock:
// a requirement first, then a System Design. Pending versions count every
// origin (req-260820-394cac AC-4.2).
func authorityDocumentsTx(ctx context.Context, tx *sql.Tx, ws string, ids []string) (map[string]store.AuthorityDocument, error) {
	out := make(map[string]store.AuthorityDocument, len(ids))
	for _, id := range ids {
		var current *int
		var archivedAt *time.Time
		kind := "requirement"
		err := documentRow(ctx, tx, `SELECT current_version,archived_at FROM requirements WHERE workspace_id=? AND id=?`, ws, id).Scan(&current, &archivedAt)
		pendingSQL := `SELECT COUNT(*) FROM requirement_versions WHERE workspace_id=? AND requirement_id=? AND NOT confirmed AND NOT retired AND version > ?`
		aboveCurrent := true
		if errors.Is(err, sql.ErrNoRows) {
			kind = "system_design"
			err = documentRow(ctx, tx, `SELECT current_version,archived_at FROM system_designs WHERE workspace_id=? AND id=?`, ws, id).Scan(&current, &archivedAt)
			pendingSQL = `SELECT COUNT(*) FROM system_design_versions WHERE workspace_id=? AND document_id=? AND NOT confirmed AND NOT dismissed`
			aboveCurrent = false
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		document := store.AuthorityDocument{ID: id, Kind: kind, Found: archivedAt == nil}
		if current != nil {
			document.CurrentVersion = *current
		}
		args := []any{ws, id}
		if aboveCurrent {
			args = append(args, document.CurrentVersion)
		}
		if err = tx.QueryRowContext(ctx, pendingSQL, args...).Scan(&document.PendingVersions); err != nil {
			return nil, err
		}
		out[id] = document
	}
	return out, nil
}
