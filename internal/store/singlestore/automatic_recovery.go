package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) readClockPolicy(ctx context.Context) (config.RecoveryPolicy, time.Duration, error) {
	var raw string
	err := backendRow(ctx, s.db, `SELECT config_yaml FROM workspaces WHERE id=?`, documentWorkspace(ctx)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || raw == "" {
		return config.DefaultRecoveryPolicy(), config.DefaultWorkOrderQueueTimeout, nil
	}
	if err != nil {
		return config.RecoveryPolicy{}, 0, err
	}
	return config.ReadClockPolicy([]byte(raw))
}

func readClockPolicyTx(ctx context.Context, tx *sql.Tx) (config.RecoveryPolicy, time.Duration, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT config_yaml FROM workspaces WHERE id=?`, documentWorkspace(ctx)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || raw == "" {
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
			AND JSON_LENGTH(checkpoint)=0
			AND (
				state IN ('timed_out','stale')
				OR (state='queued' AND last_attempt_outcome='expired')
				OR (state='queued' AND retry_suppressed AND retry_suppression_reason='attempt retries exhausted')
				OR (state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
			)
			AND (next_automatic_recovery_at IS NULL OR next_automatic_recovery_at<=?)
		)`

// clockOrderRead names a point where the order clock has read a task's orders.
type clockOrderRead int

const (
	// clockOrderReadTransitions is the read behind the deadline transitions.
	clockOrderReadTransitions clockOrderRead = iota
	// clockOrderReadRecovery is the read behind the automatic recovery decision.
	clockOrderReadRecovery
)

// clockOrderReadObserver is a test-only hook; production callers leave it
// unset. unlockedRead skips the row lock, which is the read the clock used
// before it, so a control test can show its barrier exposes the double append.
type clockOrderReadObserver struct {
	unlockedRead bool
	read         func(clockOrderRead)
}

type clockOrderReadKey struct{}

func observeClockOrderReads(ctx context.Context, obs *clockOrderReadObserver) context.Context {
	return context.WithValue(ctx, clockOrderReadKey{}, obs)
}

func noteClockOrderRead(ctx context.Context, point clockOrderRead) {
	if obs, ok := ctx.Value(clockOrderReadKey{}).(*clockOrderReadObserver); ok && obs.read != nil {
		obs.read(point)
	}
}

// clockOrderLockWait bounds the wait for a task's order rows. It stays below the
// driver's 30-second read timeout, which the server's 30-second lock wait can
// lose to, and which fails the tick with a generic error instead of a retryable
// one. Tests shorten it.
var clockOrderLockWait = 10 * time.Second

// lockClockOrderRowsTx locks the task's order rows before the clock reads them.
// Holding the task-operation lock doesn't prove the previous holder's order
// write is visible, because its commit spans partitions and can release the
// lock row first (component-persistence invariant 8). A tick that read the rows
// in that gap acted on the image the previous tick replaced, wrote it back, and
// appended the previous tick's events again (component-work-orders invariants
// 47 and 48). A locked read waits for that write, and the plain reads after it
// in this transaction see it.
//
// The order is task operation, order rows, documents (component-persistence
// invariant 7). A wait that runs out returns store.ErrRetryable, which rolls the
// tick's transaction back with nothing written for the deadline alarm's retry.
func lockClockOrderRowsTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	if obs, ok := ctx.Value(clockOrderReadKey{}).(*clockOrderReadObserver); ok && obs.unlockedRead {
		return nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, clockOrderLockWait)
	defer cancel()
	err := func() error {
		rows, err := documentRows(lockCtx, tx, `SELECT id FROM work_orders WHERE workspace_id=? AND task_id=? FOR UPDATE`, documentWorkspace(ctx), taskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	}()
	if err != nil && lockCtx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: order rows of task %s stayed locked", store.ErrRetryable, taskID)
	}
	return err
}

func (s *Store) applyAutomaticRecoveryTx(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) (int, error) {
	policy, queueTimeout, err := readClockPolicyTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	task, err := scanTask(documentRow(ctx, tx, `SELECT `+taskColumns+` FROM tasks WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), taskID))
	if err != nil {
		return 0, err
	}
	orders, err := s.listOrders(ctx, tx, []string{taskID})
	if err != nil {
		return 0, err
	}
	noteClockOrderRead(ctx, clockOrderReadRecovery)
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
		if store.AnsweredAuthorityCandidate(task, order) {
			recovered, recoverErr := s.recoverAnsweredAuthorityTx(ctx, tx, task, order, queueTimeout, now)
			if recoverErr != nil {
				return acted, recoverErr
			}
			if recovered {
				acted++
			}
			continue
		}
		decision := store.DecideAutomaticRecovery(policy, task, order, now)
		switch decision.Kind {
		case store.AutomaticRecoverySchedule:
			store.ScheduleAutomaticRecovery(&order, decision)
			order.UpdatedAt = now
			if err = orderWrite(ctx, tx, order); err != nil {
				return acted, err
			}
		case store.AutomaticRecoveryExhausted:
			order.RetrySuppressed = true
			order.RetrySuppressionReason = core.AutomaticRecoveryExhaustedReason
			order.UpdatedAt = now
			if err = orderWrite(ctx, tx, order); err != nil {
				return acted, err
			}
			eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+order.JobID))
			if err = taskEvent(eventCtx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.recovery_exhausted", Payload: core.JSONPayload(map[string]any{
				"work_order_id": order.ID, "recovery_count": decision.Number, "window_started_at": decision.WindowStartedAt, "reason": core.AutomaticRecoveryExhaustedReason, "last_failure_message": order.LastFailureMessage,
			}), At: now}); err != nil {
				return acted, err
			}
		case store.AutomaticRecoveryRecover, store.AutomaticRecoveryRedispatch:
			updated, actErr := s.actAutomaticRecoveryTx(ctx, tx, order, decision, queueTimeout, now, policy)
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

func (s *Store) actAutomaticRecoveryTx(ctx context.Context, tx *sql.Tx, order core.WorkOrder, decision store.AutomaticRecoveryDecision, queueTimeout time.Duration, now time.Time, policy config.RecoveryPolicy) (core.WorkOrder, error) {
	requestID := store.AutomaticRecoveryRequestID(order.ID, order.RedispatchCount)
	var duplicate int
	if err := documentRow(ctx, tx, `SELECT COUNT(*) FROM work_order_recoveries WHERE workspace_id=? AND work_order_id=? AND request_id=?`, documentWorkspace(ctx), order.ID, requestID).Scan(&duplicate); err != nil {
		return core.WorkOrder{}, err
	}
	if duplicate > 0 {
		return order, nil
	}
	prior := order.State
	count, window := store.StampAutomaticRecovery(&order, policy, now)
	kind := "work_order.recovered"
	command := core.WorkOrderCmdRecover
	if decision.Kind == store.AutomaticRecoveryRedispatch {
		next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRedispatch)
		if err != nil {
			return core.WorkOrder{}, err
		}
		clearOrderClaim(&order)
		order.ClearExecutionPins()
		order.State = next
		order.QueueEnteredAt = now
		order.QueueDeadline = now.Add(queueTimeout)
		order.ExecutionStartedAt = time.Time{}
		order.ExecutionDeadline = time.Time{}
		order.RedispatchCount++
		order.Progress = ""
		order.UpdatedAt = now
		store.ClearAutomaticRecoveryExhaustion(&order)
		kind = "work_order.redispatched"
		command = core.WorkOrderCmdRedispatch
	} else {
		if order.State == core.WorkOrderQueued {
			command = ""
			kind = "work_order.redispatched"
		} else if _, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRecover); err != nil {
			return core.WorkOrder{}, err
		}
		clearOrderClaim(&order)
		order.ClearExecutionPins()
		order.State = core.WorkOrderQueued
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
		order.OperatorDirection = ""
		order.UpdatedAt = now
	}
	order.AutomaticRecoveryCount = count
	order.AutomaticRecoveryWindowStartedAt = window
	order.NextAutomaticRecoveryAt = time.Time{}
	if err := orderWrite(ctx, tx, order); err != nil {
		return core.WorkOrder{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES (?,?,?,?)`, documentWorkspace(ctx), order.ID, requestID, now); err != nil {
		return core.WorkOrder{}, err
	}
	if order.Stage == core.StageImplement {
		if err := repinTaskDesignContextTx(ctx, tx, documentWorkspace(ctx), order.TaskID, now); err != nil {
			return core.WorkOrder{}, err
		}
	}
	if err := resetOrderJob(ctx, tx, order, now); err != nil {
		return core.WorkOrder{}, err
	}
	eventCtx := store.WithActor(ctx, store.SystemActor("queue:"+order.JobID))
	if err := taskEvent(eventCtx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID, "prior_state": prior, "new_state": order.State, "command": command,
		"reason": core.AutomaticRecoveryReason, "request_id": requestID, "automatic": true, "recovery_number": count,
		"window_started_at": window, "next_automatic_recovery_at": nil,
	}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if decision.Kind == store.AutomaticRecoveryRedispatch {
		if err := s.retireOrderSiblingsTx(ctx, tx, order, "superseded by stale redispatch", now, false); err != nil {
			return core.WorkOrder{}, err
		}
	}
	return order, nil
}
