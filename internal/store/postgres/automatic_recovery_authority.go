package postgres

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// answeredAuthorityElapsedSQL selects a released authority_conflict implement
// order on every tick, whatever the recovery bound
// (feature-bounded-automatic-recovery AR-3a).
const answeredAuthorityElapsedSQL = ` OR (
			stage='implement' AND state='queued' AND last_attempt_outcome='released'
			AND checkpoint->>'class'='authority_conflict'
			AND EXISTS (SELECT 1 FROM tasks t WHERE t.workspace_id=work_orders.workspace_id AND t.id=work_orders.task_id AND t.state IN ('queued','running'))
		)`

// recoverAnsweredAuthorityTx runs inside ApplyWorkOrderClock, which already
// holds the task-operation lock and the order rows. It locks the cited
// document rows FOR SHARE before reading them, so a concurrent proposal,
// confirmation, or dismissal either commits first or waits (component-persistence).
func (s *Store) recoverAnsweredAuthorityTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, order core.WorkOrder, queueTimeout time.Duration, now time.Time) (bool, error) {
	documents, err := authorityDocumentsTx(ctx, tx, store.AnsweredAuthorityDocumentIDs(order))
	if err != nil {
		return false, err
	}
	decision := store.DecideAnsweredAuthorityRecovery(task, order, documents)
	if !decision.Recover {
		return false, nil
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_order_recoveries WHERE workspace_id=$1 AND work_order_id=$2 AND request_id=$3)`, workspace(ctx), order.ID, decision.RequestID).Scan(&duplicate); err != nil || duplicate {
		return false, err
	}
	var releaseEventID int64
	err = tx.QueryRow(ctx, `SELECT id FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='work_order.released' AND payload_json->>'attempt_id'=$3 ORDER BY id DESC LIMIT 1`, workspace(ctx), order.TaskID, order.LastAttemptID).Scan(&releaseEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The ledger columns are not written: the act does not count against the
	// DEC-62 bound (req-260820-394cac AC-2.6).
	updated, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET claimant_id='',session_id='',attempt_id='',client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome='',retry_suppressed=false,retry_suppression_reason='',automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,operator_direction=$3,checkpoint='{}'::jsonb,`+clearExecutionPinsSQL+`,updated_at=$1 WHERE workspace_id=$4 AND id=$5 AND state='queued' AND last_attempt_outcome='released' RETURNING `+workOrderColumns,
		now, now.Add(queueTimeout), decision.Direction, workspace(ctx), order.ID))
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES ($1,$2,$3,$4)`, workspace(ctx), order.ID, decision.RequestID, now); err != nil {
		return false, err
	}
	if err = repinTaskDesignContextTx(ctx, tx, q, workspace(ctx), updated.TaskID, now); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, updated.JobID); err != nil {
		return false, err
	}
	eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+updated.JobID))
	payload := store.AuthorityConfirmedEventPayload(workspace(ctx), order, decision, releaseEventID)
	if err = insertEvent(eventCtx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: "work_order.recovered", Payload: core.JSONPayload(payload), At: now}); err != nil {
		return false, err
	}
	return true, nil
}

// authorityDocumentsTx locks and reads each cited document in kind and ID
// order: requirements first, then System Designs. Pending versions count
// every origin, as ListPendingProposals does (req-260820-394cac AC-4.2).
func authorityDocumentsTx(ctx context.Context, tx pgx.Tx, ids []string) (map[string]store.AuthorityDocument, error) {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	out := make(map[string]store.AuthorityDocument, len(sorted))
	read := func(table, kind, pendingSQL string, aboveCurrent bool) error {
		for _, id := range sorted {
			if _, done := out[id]; done {
				continue
			}
			var current *int
			err := tx.QueryRow(ctx, `SELECT current_version FROM `+table+` WHERE workspace_id=$1 AND id=$2 FOR SHARE`, workspace(ctx), id).Scan(&current)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			archived, err := documentArchivedTx(ctx, tx, table, workspace(ctx), id)
			if err != nil {
				return err
			}
			document := store.AuthorityDocument{ID: id, Kind: kind, Found: !archived}
			if current != nil {
				document.CurrentVersion = *current
			}
			args := []any{workspace(ctx), id}
			if aboveCurrent {
				args = append(args, document.CurrentVersion)
			}
			if err = tx.QueryRow(ctx, pendingSQL, args...).Scan(&document.PendingVersions); err != nil {
				return err
			}
			out[id] = document
		}
		return nil
	}
	if err := read("requirements", "requirement", `SELECT count(*) FROM requirement_versions WHERE workspace_id=$1 AND requirement_id=$2 AND NOT confirmed AND NOT retired AND version > $3`, true); err != nil {
		return nil, err
	}
	if err := read("system_designs", "system_design", `SELECT count(*) FROM system_design_versions WHERE workspace_id=$1 AND document_id=$2 AND NOT confirmed AND NOT dismissed`, false); err != nil {
		return nil, err
	}
	return out, nil
}
