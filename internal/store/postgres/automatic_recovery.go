package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) readClockPolicy(ctx context.Context) (config.RecoveryPolicy, time.Duration, error) {
	var raw string
	err := s.boundary.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspace(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || raw == "" {
		return config.DefaultRecoveryPolicy(), config.DefaultWorkOrderQueueTimeout, nil
	}
	if err != nil {
		return config.RecoveryPolicy{}, 0, err
	}
	return config.ReadClockPolicy([]byte(raw))
}

func readClockPolicyTx(ctx context.Context, tx pgx.Tx) (config.RecoveryPolicy, time.Duration, error) {
	var raw string
	err := tx.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspace(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || raw == "" {
		return config.DefaultRecoveryPolicy(), config.DefaultWorkOrderQueueTimeout, nil
	}
	if err != nil {
		return config.RecoveryPolicy{}, 0, err
	}
	return config.ReadClockPolicy([]byte(raw))
}

const automaticRecoveryElapsedSQL = ` OR (
			EXISTS (SELECT 1 FROM tasks t WHERE t.workspace_id=work_orders.workspace_id AND t.id=work_orders.task_id AND t.state IN ('queued','running'))
			AND (retry_suppression_reason='' OR retry_suppression_reason='attempt retries exhausted')
			AND checkpoint='{}'::jsonb
			AND (
				state IN ('timed_out','stale')
				OR (state='queued' AND last_attempt_outcome='expired')
				OR (state='queued' AND retry_suppressed AND retry_suppression_reason='attempt retries exhausted')
				OR (state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at<=$2)
			)
			AND (next_automatic_recovery_at IS NULL OR next_automatic_recovery_at<=$2)
		)`

func writeRecoveredWorkOrder(ctx context.Context, tx pgx.Tx, id string, now time.Time, queueTimeout time.Duration, direction, timeoutText string, count int, window time.Time) (core.WorkOrder, error) {
	var windowValue any
	if !window.IsZero() {
		windowValue = window
	}
	return scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',attempt_id='',client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome='',retry_suppressed=false,retry_suppression_reason='',automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,operator_direction=$3,`+clearExecutionPinsSQL+`,execution_timeout=$4,automatic_recovery_count=$7,automatic_recovery_window_started_at=$8,next_automatic_recovery_at=NULL,updated_at=$1 WHERE workspace_id=$5 AND id=$6 AND state IN ('queued','stale','timed_out') RETURNING `+workOrderColumns,
		now, now.Add(queueTimeout), direction, timeoutText, workspace(ctx), id, count, windowValue))
}

func writeRedispatchedWorkOrder(ctx context.Context, tx pgx.Tx, id string, now time.Time, queueTimeout time.Duration, count int, window time.Time, clearExhaustion bool) (core.WorkOrder, error) {
	var windowValue any
	if !window.IsZero() {
		windowValue = window
	}
	clear := "retry_suppression_reason"
	suppressed := "retry_suppressed"
	if clearExhaustion {
		clear = `CASE WHEN retry_suppression_reason='automatic recovery exhausted' THEN '' ELSE retry_suppression_reason END`
		suppressed = `CASE WHEN retry_suppression_reason='automatic recovery exhausted' THEN false ELSE retry_suppressed END`
	}
	return scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued', claimant_id='',
		session_id='', client_token_hash='', agent='', model='', worker_id='', lease_expires_at=NULL, model_enforcement='',
		`+clearExecutionPinsSQL+`,
		queue_entered_at=$1, queue_deadline=$2, execution_started_at=NULL,
		execution_deadline=NULL, redispatch_count=redispatch_count+1, progress='',
		automatic_recovery_count=$5, automatic_recovery_window_started_at=$6, next_automatic_recovery_at=NULL,
		retry_suppression_reason=`+clear+`, retry_suppressed=`+suppressed+`, updated_at=$1
		WHERE workspace_id=$3 AND id=$4 RETURNING `+workOrderColumns,
		now, now.Add(queueTimeout), workspace(ctx), id, count, windowValue))
}

func (s *Store) applyAutomaticRecoveryTx(ctx context.Context, tx pgx.Tx, taskID string, now time.Time) (int, error) {
	policy, queueTimeout, err := readClockPolicyTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	q := s.queries.WithTx(tx)
	taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return 0, err
	}
	task := taskFromDB(taskRow)
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 FOR UPDATE", workspace(ctx), taskID)
	if err != nil {
		return 0, err
	}
	var orders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		orders = append(orders, order)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	sort.Slice(orders, func(i, j int) bool {
		if !orders[i].QueueEnteredAt.Equal(orders[j].QueueEnteredAt) {
			return orders[i].QueueEnteredAt.Before(orders[j].QueueEnteredAt)
		}
		return orders[i].ID < orders[j].ID
	})
	acted := 0
	for _, order := range orders {
		order.RecoveryRefusal = ""
		if refusal := store.WorkOrderRecoveryRefusal(task, order, orders); refusal != nil {
			order.RecoveryRefusal = refusal.Error()
		}
		decision := store.DecideAutomaticRecovery(policy, task, order, now)
		switch decision.Kind {
		case store.AutomaticRecoverySchedule:
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET next_automatic_recovery_at=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4`, decision.At, now, workspace(ctx), order.ID); err != nil {
				return acted, err
			}
		case store.AutomaticRecoveryExhausted:
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET retry_suppressed=true,retry_suppression_reason=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4`, core.AutomaticRecoveryExhaustedReason, now, workspace(ctx), order.ID); err != nil {
				return acted, err
			}
			eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+order.JobID))
			if err = insertEvent(eventCtx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.recovery_exhausted", Payload: core.JSONPayload(map[string]any{
				"work_order_id": order.ID, "recovery_count": decision.Number, "window_started_at": decision.WindowStartedAt, "last_failure_message": order.LastFailureMessage,
			}), At: now}); err != nil {
				return acted, err
			}
		case store.AutomaticRecoveryRecover, store.AutomaticRecoveryRedispatch:
			updated, actErr := s.actAutomaticRecoveryTx(ctx, tx, q, order, decision, queueTimeout, now, policy)
			if actErr != nil {
				return acted, actErr
			}
			if updated.State == core.WorkOrderQueued {
				acted++
			}
		}
	}
	return acted, nil
}

func (s *Store) actAutomaticRecoveryTx(ctx context.Context, tx pgx.Tx, q *db.Queries, order core.WorkOrder, decision store.AutomaticRecoveryDecision, queueTimeout time.Duration, now time.Time, policy config.RecoveryPolicy) (core.WorkOrder, error) {
	requestID := store.AutomaticRecoveryRequestID(order.ID, order.RedispatchCount)
	var duplicate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_order_recoveries WHERE workspace_id=$1 AND work_order_id=$2 AND request_id=$3)`, workspace(ctx), order.ID, requestID).Scan(&duplicate); err != nil {
		return core.WorkOrder{}, err
	}
	if duplicate {
		return order, nil
	}
	count, window := store.StampAutomaticRecovery(&order, policy, now)
	var updated core.WorkOrder
	var err error
	kind := "work_order.recovered"
	command := core.WorkOrderCmdRecover
	reason := core.AutomaticRecoveryReason
	if decision.Kind == store.AutomaticRecoveryRedispatch {
		updated, err = writeRedispatchedWorkOrder(ctx, tx, order.ID, now, queueTimeout, count, window, true)
		kind = "work_order.redispatched"
		command = core.WorkOrderCmdRedispatch
	} else {
		if order.State == core.WorkOrderQueued {
			command = ""
			kind = "work_order.redispatched"
		}
		updated, err = writeRecoveredWorkOrder(ctx, tx, order.ID, now, queueTimeout, "", order.ExecutionTimeoutText, count, window)
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES ($1,$2,$3,$4)`, workspace(ctx), order.ID, requestID, now); err != nil {
		return core.WorkOrder{}, err
	}
	if updated.Stage == core.StageImplement {
		if err = repinTaskDesignContextTx(ctx, tx, q, workspace(ctx), updated.TaskID, now); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, updated.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+updated.JobID))
	payload := map[string]any{
		"work_order_id": updated.ID, "prior_state": order.State, "new_state": updated.State, "command": command,
		"reason": reason, "request_id": requestID, "automatic": true, "recovery_number": count,
		"window_started_at": window, "next_automatic_recovery_at": nil,
	}
	if err = insertEvent(eventCtx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: kind, Payload: core.JSONPayload(payload), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if decision.Kind == store.AutomaticRecoveryRedispatch {
		if err = retireWorkOrderSiblingsTx(ctx, tx, q, workspace(ctx), updated, "superseded by stale redispatch", now, false); err != nil {
			return core.WorkOrder{}, err
		}
	}
	return updated, nil
}

func clockPolicyBound(ctx context.Context, s *Store) (int, error) {
	policy, _, err := s.readClockPolicy(ctx)
	if err != nil {
		return 0, fmt.Errorf("read recovery policy: %w", err)
	}
	return policy.AutomaticRecoveries, nil
}
