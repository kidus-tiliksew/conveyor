package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Risk orders (feature-risk-based-review RR-11, RR-15; req-risk-based-review
// REQ-12; component-persistence). Every command runs in taskTx, under the task
// operation lock that ClaimWorkOrderCommand and RaiseTaskRiskTier take.
// Store.createRiskOrderTx and cancelRiskOrdersTx compose inside another act's
// taskTx. SingleStore has no CHECK, so the shared store helpers validate the
// stage, the source, and the subject before any write.

func (s *Store) CreateRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateRiskOrderCommand")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err := request.Validate(); err != nil {
		return core.WorkOrder{}, false, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return core.WorkOrder{}, false, fmt.Errorf("risk order creation requires a valid taskops lease")
	}
	var order core.WorkOrder
	created := false
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		task, err := getTaskRow(ctx, tx, request.TaskID)
		if err != nil {
			return err
		}
		order, created, err = s.createRiskOrderTx(ctx, tx, task, request)
		return err
	})
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	return order, created, nil
}

// createRiskOrderTx creates one risk order inside taskTx: the job, the order,
// its subject row, and their events.
func (s *Store) createRiskOrderTx(ctx context.Context, tx *sql.Tx, task core.Task, request store.RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
	ratings, err := lockRiskRatingsTx(ctx, tx, task.ID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	existing, err := taskRiskOrdersTx(ctx, tx, task.ID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	plan, err := store.PlanRiskOrderCreate(task, ratings, existing, request)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	if plan.Existing != nil {
		return plan.Existing.Order, false, nil
	}
	if !plan.Created {
		return core.WorkOrder{}, false, nil
	}
	order, now := plan.Order.Order, plan.Order.CreatedAt
	if err = insertJobRow(ctx, tx, plan.Job); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err = taskEvent(ctx, tx, core.Event{TaskID: task.ID, JobID: plan.Job.ID, Kind: "job.created", Payload: core.JSONPayload(plan.Job), At: now}); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err = s.insertOrderTx(ctx, tx, orderDefaults(order), false); err != nil {
		return core.WorkOrder{}, false, err
	}
	subject := plan.Order.Subject
	if _, err = documentExec(ctx, tx, `INSERT INTO risk_orders (workspace_id,task_id,work_order_id,subject_key,plan_version,head_sha,base_sha,changed_paths,matched_rules,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, documentWorkspace(ctx), task.ID, order.ID, subject.Key(), subject.PlanVersion, subject.HeadSHA, subject.BaseSHA,
		core.JSONPayload(plan.Order.ChangedPaths), core.JSONPayload(plan.Order.MatchedRules), now); err != nil {
		return core.WorkOrder{}, false, err
	}
	stored, err := getOrderRow(ctx, tx, order.ID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	stored.Claimable = stored.ClaimableAt(time.Now().UTC())
	return stored, true, nil
}

func (s *Store) CompleteRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.RiskOrderCompleteRequest) (core.RiskRating, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CompleteRiskOrderCommand")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.RiskRating{}, err
	}
	var result core.RiskRating
	err := s.orderTx(ctx, request.WorkOrderID, func(tx *sql.Tx, order core.WorkOrder) error {
		if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdSubmitRiskRating)) {
			return fmt.Errorf("risk order completion requires a valid taskops lease")
		}
		task, ratings, err := riskTaskTx(ctx, tx, order.TaskID)
		if err != nil {
			return err
		}
		risk, err := riskOrderSubjectTx(ctx, tx, order)
		if err != nil {
			return err
		}
		now := request.Now.UTC()
		if request.Now.IsZero() {
			now = time.Now().UTC()
		}
		rating, created, err := store.PrepareRiskOrderCompletion(task, ratings, risk, request, now)
		if err != nil {
			return err
		}
		result = rating
		if !created {
			return nil
		}
		written, err := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE",
			values: map[string]any{"state": core.WorkOrderCompleted, "updated_at": now},
			where:  map[string]any{"workspace_id": documentWorkspace(ctx), "id": order.ID, "state": core.WorkOrderClaimed, "session_id": order.SessionID}})
		if err != nil {
			return err
		}
		if affected, affectedErr := written.RowsAffected(); affectedErr != nil || affected != 1 {
			return store.ErrWorkOrderClaimLost
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='done',ended_at=?,updated_at=? WHERE workspace_id=? AND id=?`, now, now, documentWorkspace(ctx), order.JobID); err != nil {
			return err
		}
		order.State, order.Claimable, order.UpdatedAt = core.WorkOrderCompleted, false, now
		if err = taskEvent(ctx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: store.WorkOrderEventPayload(order), At: now}); err != nil {
			return err
		}
		if err = insertRiskRatingTx(ctx, tx, rating); err != nil {
			return err
		}
		// A rating that brings the tier to high cancels every other open
		// risk order in the same transaction (req-risk-based-review AC-12.3).
		counted, err := markPendingPlanRiskRatings(ctx, tx, order.TaskID, append(ratings, rating))
		if err != nil {
			return err
		}
		result = store.CountedRiskRating(counted, rating)
		if store.RiskTierReachesHigh(counted) {
			_, err = cancelRiskOrdersTx(ctx, tx, order.TaskID, "", store.RiskTierHighCancelReason, now)
		}
		return err
	})
	if err != nil {
		return core.RiskRating{}, err
	}
	return result, nil
}

func (s *Store) CancelRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.RiskOrderCancelRequest) (core.WorkOrder, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CancelRiskOrderCommand")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err := store.ValidateRiskOrderCancel(request); err != nil {
		return core.WorkOrder{}, false, err
	}
	var result core.WorkOrder
	cancelled := false
	err := s.orderTx(ctx, request.WorkOrderID, func(tx *sql.Tx, order core.WorkOrder) error {
		if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdCancel)) {
			return fmt.Errorf("risk order cancellation requires a valid taskops lease")
		}
		if order.Stage != core.StageRisk {
			return fmt.Errorf("%w: %s", store.ErrNotRiskOrder, order.ID)
		}
		if order.State != core.WorkOrderCompleted && order.State != core.WorkOrderCancelled {
			now := request.Now.UTC()
			if request.Now.IsZero() {
				now = time.Now().UTC()
			}
			ids, err := cancelRiskOrdersTx(ctx, tx, order.TaskID, order.ID, request.Reason, now)
			if err != nil {
				return err
			}
			cancelled = len(ids) == 1
		}
		var err error
		result, err = getOrderRow(ctx, tx, order.ID)
		return err
	})
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	return result, cancelled, nil
}

func (s *Store) ListTaskRiskOrders(ctx context.Context, taskID string) ([]core.RiskOrder, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskRiskOrders")
	if _, err := getTaskRow(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	orders, err := taskRiskOrdersTx(ctx, s.db, taskID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range orders {
		orders[i].Order = store.ProjectWorkOrderAt(orders[i].Order, now)
	}
	return orders, nil
}

// cancelRiskOrdersTx cancels the task's open risk orders inside taskTx: only
// onlyID when it is set. Each cancellation ends the live claim, fails the
// job, and appends work_order.cancelled under the request's actor.
func cancelRiskOrdersTx(ctx context.Context, tx *sql.Tx, taskID, onlyID, reason string, at time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='risk'
		AND state NOT IN ('completed','cancelled') AND (?='' OR id=?) ORDER BY id FOR UPDATE`, documentWorkspace(ctx), taskID, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	type open struct {
		id, job, attempt, session string
		state                     core.WorkOrderState
	}
	var orders []open
	for rows.Next() {
		var o open
		if err = rows.Scan(&o.id, &o.job, &o.state, &o.attempt, &o.session); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err = core.TransitionWorkOrder(o.state, core.WorkOrderCmdCancel); err != nil {
			rows.Close()
			return nil, err
		}
		orders = append(orders, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	actor := store.ActorFromContext(ctx)
	cancelled := []string{}
	for _, o := range orders {
		v := map[string]any{"state": core.WorkOrderCancelled, "attempt_id": "", "updated_at": at,
			"retry_suppressed": true, "retry_suppression_reason": reason, "next_retry_at": nil}
		if o.state == core.WorkOrderClaimed {
			v["last_attempt_outcome"] = core.WorkOrderOutcomeCancelled
		}
		if o.attempt != "" {
			v["last_attempt_id"] = o.attempt
			for _, key := range []string{"claimant_id", "session_id", "client_token_hash", "agent", "model", "worker_id", "model_enforcement"} {
				v[key] = ""
			}
			for _, key := range []string{"lease_expires_at", "execution_started_at", "execution_deadline"} {
				v[key] = nil
			}
		}
		result, writeErr := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: v, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.id, "state": o.state}})
		if writeErr != nil {
			return nil, writeErr
		}
		if affected, affectedErr := result.RowsAffected(); affectedErr != nil || affected != 1 {
			return nil, fmt.Errorf("risk order %s changed during cancellation", o.id)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=?,updated_at=? WHERE workspace_id=? AND task_id=? AND id=? AND state<>'done'`, at, at, documentWorkspace(ctx), taskID, o.job); err != nil {
			return nil, err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, JobID: o.job, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: store.RiskOrderCancelledPayload(o.id, o.attempt, o.session, o.state, reason), At: at}); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, o.id)
	}
	return cancelled, nil
}

// cancelRiskOrdersIfHighTx cancels the task's open risk orders inside taskTx
// when its counted tier is high. A plan approval that makes a risk agent plan
// rating count calls it (req-risk-based-review AC-4.3, AC-12.3).
func cancelRiskOrdersIfHighTx(ctx context.Context, tx *sql.Tx, taskID string, at time.Time) error {
	counted, err := lockRiskRatingsTx(ctx, tx, taskID)
	if err != nil || !store.RiskTierReachesHigh(counted) {
		return err
	}
	_, err = cancelRiskOrdersTx(ctx, tx, taskID, "", store.RiskTierHighCancelReason, at)
	return err
}

// riskOrderSubjectTx reads the subject row of one risk order.
func riskOrderSubjectTx(ctx context.Context, db s2log.Executor, order core.WorkOrder) (core.RiskOrder, error) {
	if order.Stage != core.StageRisk {
		return core.RiskOrder{}, fmt.Errorf("%w: %s", store.ErrNotRiskOrder, order.ID)
	}
	var risk core.RiskOrder
	var paths, rules []byte
	err := documentRow(ctx, db, `SELECT plan_version,head_sha,base_sha,changed_paths,matched_rules,created_at FROM risk_orders WHERE workspace_id=? AND task_id=? AND work_order_id=?`,
		documentWorkspace(ctx), order.TaskID, order.ID).Scan(&risk.Subject.PlanVersion, &risk.Subject.HeadSHA, &risk.Subject.BaseSHA, &paths, &rules, &risk.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RiskOrder{}, fmt.Errorf("%w: %s has no subject", store.ErrNotRiskOrder, order.ID)
	}
	if err != nil {
		return core.RiskOrder{}, err
	}
	if err = json.Unmarshal(paths, &risk.ChangedPaths); err != nil {
		return core.RiskOrder{}, err
	}
	if err = json.Unmarshal(rules, &risk.MatchedRules); err != nil {
		return core.RiskOrder{}, err
	}
	risk.CreatedAt = risk.CreatedAt.UTC()
	risk.Order = order
	return risk.Clone(), nil
}

// taskRiskOrdersTx reads a task's risk orders with their subjects, oldest
// first.
func taskRiskOrdersTx(ctx context.Context, db s2log.Executor, taskID string) ([]core.RiskOrder, error) {
	rows, err := documentRows(ctx, db, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='risk' ORDER BY created_at,id`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	var orders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []core.RiskOrder{}
	for _, order := range orders {
		risk, subjectErr := riskOrderSubjectTx(ctx, db, order)
		if errors.Is(subjectErr, store.ErrNotRiskOrder) {
			continue
		}
		if subjectErr != nil {
			return nil, subjectErr
		}
		result = append(result, risk)
	}
	store.SortRiskOrders(result)
	return result, nil
}

// markPendingPlanRiskRatings derives each risk agent plan rating's
// PlanPending from the task's approved plan versions
// (req-risk-based-review AC-4.3).
func markPendingPlanRiskRatings(ctx context.Context, db s2log.Executor, taskID string, ratings []core.RiskRating) ([]core.RiskRating, error) {
	approved := map[int]bool{}
	if store.RiskRatingsNeedPlanApprovals(ratings) {
		rows, err := documentRows(ctx, db, `SELECT version FROM task_specs WHERE workspace_id=? AND task_id=? AND approved`, documentWorkspace(ctx), taskID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var version int
			if err = rows.Scan(&version); err != nil {
				rows.Close()
				return nil, err
			}
			approved[version] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return store.MarkPendingPlanRiskRatings(ratings, func(version int) bool { return approved[version] }), nil
}

// recordClaimIdentityTx appends one accepted claim to the claim ledger in the
// claim transaction.
func recordClaimIdentityTx(ctx context.Context, tx *sql.Tx, order core.WorkOrder, tokenHash string, at time.Time) error {
	_, err := documentExec(ctx, tx, `INSERT IGNORE INTO work_order_claim_identities (workspace_id,task_id,work_order_id,attempt_id,stage,session_id,client_token_hash,claimed_at)
		VALUES (?,?,?,?,?,?,?,?)`, documentWorkspace(ctx), order.TaskID, order.ID, order.AttemptID, string(order.Stage), order.SessionID, tokenHash, at)
	return err
}

// riskClaimRefusalTx applies the risk claim refusal inside the claim's taskTx
// from the claim ledger, the task's order rows, and its claim events
// (req-risk-based-review AC-12.1).
func riskClaimRefusalTx(ctx context.Context, tx *sql.Tx, order core.WorkOrder, sessionID, tokenHash string, taskOrders []core.WorkOrder) error {
	if order.Stage != core.StageRisk {
		return nil
	}
	rows, err := documentRows(ctx, tx, `SELECT task_id,work_order_id,attempt_id,stage,session_id,client_token_hash,claimed_at
		FROM work_order_claim_identities WHERE workspace_id=? AND task_id=? AND stage IN ('spec','implement','review')`, documentWorkspace(ctx), order.TaskID)
	if err != nil {
		return err
	}
	var ledger []store.ClaimIdentity
	for rows.Next() {
		var identity store.ClaimIdentity
		if err = rows.Scan(&identity.TaskID, &identity.WorkOrderID, &identity.AttemptID, &identity.Stage, &identity.SessionID, &identity.ClientTokenHash, &identity.ClaimedAt); err != nil {
			rows.Close()
			return err
		}
		ledger = append(ledger, identity)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	events, err := documentTaskEvents(ctx, tx, order.TaskID)
	if err != nil {
		return err
	}
	return store.RiskClaimRefusal(order, sessionID, tokenHash, ledger, taskOrders, events)
}
