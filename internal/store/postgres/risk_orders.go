package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Risk orders (feature-risk-based-review RR-11, RR-15; req-risk-based-review
// REQ-12; component-persistence). Every command runs in one transaction that
// holds lockWorkOrderTaskTx, the lock ClaimWorkOrderCommand and
// RaiseTaskRiskTier take. createRiskOrderTx and cancelRiskOrdersTx compose
// inside another act's transaction that holds the same lock.

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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), request.TaskID); err != nil {
			return err
		}
		row, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		order, created, err = createRiskOrderTx(ctx, tx, q, taskFromDB(row), request)
		return err
	})
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	return order, created, nil
}

// createRiskOrderTx creates one risk order in the caller's transaction, which
// holds lockWorkOrderTaskTx for task: the job, the order, its subject row, and
// their events.
func createRiskOrderTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, request store.RiskOrderCreateRequest) (core.WorkOrder, bool, error) {
	ratings, err := listRiskRatings(ctx, tx, task.ID)
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	existing, err := taskRiskOrdersTx(ctx, tx, workspace(ctx), task.ID)
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
	if _, err = q.InsertJob(ctx, jobInsertParams(plan.Job)); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: plan.Job.ID, Kind: "job.created", Payload: core.JSONPayload(plan.Job), At: now}); err != nil {
		return core.WorkOrder{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_orders (
		id, workspace_id, task_id, job_id, stage, state, claimant_id,
		session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
		review_round, review_seat, required_model, required_harness, execution_timeout, model_enforcement,
		reason_code, review_kind, review_scope, baseline_sha, head_sha, verification_context_id,
		queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
		last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
		automatic_retry_count, next_retry_at, retry_suppressed,
		redispatch_count, progress, cost_usd, tokens_in, tokens_out,
		usage_reported, self_reported, created_at, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,0,0,'','',$7,'','','','',$8,$9,'',$10,$11,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,false,false,$12,$12)`,
		order.ID, workspace(ctx), order.TaskID, order.JobID, core.StageRisk, core.WorkOrderQueued, order.ExecutionTimeoutText,
		order.BaselineSHA, order.HeadSHA, order.QueueEnteredAt, order.QueueDeadline, now); err != nil {
		return core.WorkOrder{}, false, err
	}
	subject := plan.Order.Subject
	if _, err = tx.Exec(ctx, `INSERT INTO risk_orders (workspace_id,work_order_id,task_id,subject_key,plan_version,head_sha,base_sha,changed_paths,matched_rules,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, workspace(ctx), order.ID, task.ID, subject.Key(), subject.PlanVersion, subject.HeadSHA, subject.BaseSHA,
		core.JSONPayload(plan.Order.ChangedPaths), core.JSONPayload(plan.Order.MatchedRules), now); err != nil {
		return core.WorkOrder{}, false, err
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "work_order.created", Payload: store.WorkOrderEventPayload(order), At: now}); err != nil {
		return core.WorkOrder{}, false, err
	}
	stored, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), order.ID))
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), request.WorkOrderID)
		if err != nil {
			return err
		}
		if !lease.ValidForCommand(taskID, string(core.WorkOrderCmdSubmitRiskRating)) {
			return fmt.Errorf("risk order completion requires a valid taskops lease")
		}
		task, ratings, err := riskTaskTx(ctx, tx, q, taskID)
		if err != nil {
			return err
		}
		risk, err := riskOrderTx(ctx, tx, workspace(ctx), request.WorkOrderID)
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
		order := risk.Order
		tag, err := tx.Exec(ctx, `UPDATE work_orders SET state='completed',updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND state='claimed' AND session_id=$4`,
			now, workspace(ctx), order.ID, order.SessionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return store.ErrWorkOrderClaimLost
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='done',ended_at=$1,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
			return err
		}
		order.State, order.Claimable, order.UpdatedAt = core.WorkOrderCompleted, false, now
		if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: store.WorkOrderEventPayload(order), At: now}); err != nil {
			return err
		}
		if err = insertRiskRatingTx(ctx, tx, q, rating); err != nil {
			return err
		}
		// A rating that brings the tier to high cancels every other open
		// risk order in the same transaction (req-risk-based-review AC-12.3).
		counted, err := listRiskRatings(ctx, tx, taskID)
		if err != nil {
			return err
		}
		result = store.CountedRiskRating(counted, rating)
		if store.RiskTierReachesHigh(counted) {
			if _, err = cancelRiskOrdersTx(ctx, tx, q, taskID, "", store.RiskTierHighCancelReason, now); err != nil {
				return err
			}
		}
		return s.wakeTaskAfterRiskOrderTx(ctx, tx, task)
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		taskID, err := workOrderTaskIDTx(ctx, tx, workspace(ctx), request.WorkOrderID)
		if err != nil {
			return err
		}
		if !lease.ValidForCommand(taskID, string(core.WorkOrderCmdCancel)) {
			return fmt.Errorf("risk order cancellation requires a valid taskops lease")
		}
		if err = lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
			return err
		}
		order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), request.WorkOrderID))
		if err != nil {
			return notFound(err, "work order %s", request.WorkOrderID)
		}
		if order.Stage != core.StageRisk {
			return fmt.Errorf("%w: %s", store.ErrNotRiskOrder, order.ID)
		}
		if order.State != core.WorkOrderCompleted && order.State != core.WorkOrderCancelled {
			now := request.Now.UTC()
			if request.Now.IsZero() {
				now = time.Now().UTC()
			}
			ids, cancelErr := cancelRiskOrdersTx(ctx, tx, q, taskID, order.ID, request.Reason, now)
			if cancelErr != nil {
				return cancelErr
			}
			cancelled = len(ids) == 1
			if cancelled {
				row, taskErr := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
				if taskErr != nil {
					return notFound(taskErr, "task %s", taskID)
				}
				if err = s.wakeTaskAfterRiskOrderTx(ctx, tx, taskFromDB(row)); err != nil {
					return err
				}
			}
		}
		result, err = scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), request.WorkOrderID))
		return err
	})
	if err != nil {
		return core.WorkOrder{}, false, err
	}
	return result, cancelled, nil
}

func (s *Store) ListTaskRiskOrders(ctx context.Context, taskID string) ([]core.RiskOrder, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskRiskOrders")
	if _, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)}); err != nil {
		return nil, notFound(err, "task %s", taskID)
	}
	orders, err := taskRiskOrdersTx(ctx, s.boundary, workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range orders {
		orders[i].Order = store.ProjectWorkOrderAt(orders[i].Order, now)
	}
	return orders, nil
}

// cancelRiskOrdersTx cancels the task's open risk orders in the caller's
// transaction, which holds lockWorkOrderTaskTx: only onlyID when it is set.
// Each cancellation ends the live claim, fails the job, and appends
// work_order.cancelled under the request's actor.
func cancelRiskOrdersTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID, onlyID, reason string, at time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='risk'
		AND state NOT IN ('completed','cancelled') AND ($3='' OR id=$3) ORDER BY id FOR UPDATE`, workspace(ctx), taskID, onlyID)
	if err != nil {
		return nil, err
	}
	type open struct {
		id, jobID, attemptID, sessionID string
		state                           core.WorkOrderState
	}
	var orders []open
	for rows.Next() {
		var order open
		if err = rows.Scan(&order.id, &order.jobID, &order.state, &order.attemptID, &order.sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err = core.TransitionWorkOrder(order.state, core.WorkOrderCmdCancel); err != nil {
			rows.Close()
			return nil, err
		}
		orders = append(orders, order)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	actor := store.ActorFromContext(ctx)
	cancelled := []string{}
	for _, order := range orders {
		tag, updateErr := tx.Exec(ctx, `UPDATE work_orders SET state='cancelled',
			last_attempt_outcome=CASE WHEN state='claimed' THEN 'cancelled' ELSE last_attempt_outcome END,
			last_attempt_id=CASE WHEN attempt_id<>'' THEN attempt_id ELSE last_attempt_id END,
			attempt_id='',
			claimant_id=CASE WHEN attempt_id<>'' THEN '' ELSE claimant_id END,
			session_id=CASE WHEN attempt_id<>'' THEN '' ELSE session_id END,
			client_token_hash=CASE WHEN attempt_id<>'' THEN '' ELSE client_token_hash END,
			agent=CASE WHEN attempt_id<>'' THEN '' ELSE agent END,
			model=CASE WHEN attempt_id<>'' THEN '' ELSE model END,
			worker_id=CASE WHEN attempt_id<>'' THEN '' ELSE worker_id END,
			model_enforcement=CASE WHEN attempt_id<>'' THEN '' ELSE model_enforcement END,
			lease_expires_at=CASE WHEN attempt_id<>'' THEN NULL ELSE lease_expires_at END,
			execution_started_at=CASE WHEN attempt_id<>'' THEN NULL ELSE execution_started_at END,
			execution_deadline=CASE WHEN attempt_id<>'' THEN NULL ELSE execution_deadline END,
			retry_suppressed=true,retry_suppression_reason=$5,next_retry_at=NULL,
			updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND state=$4`, at, workspace(ctx), order.id, order.state, reason)
		if updateErr != nil {
			return nil, updateErr
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("risk order %s changed during cancellation", order.id)
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1,updated_at=$1 WHERE task_id=$2 AND id=$3 AND state<>'done'`, at, taskID, order.jobID); err != nil {
			return nil, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: order.jobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: store.RiskOrderCancelledPayload(order.id, order.attemptID, order.sessionID, order.state, reason), At: at}); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, order.id)
	}
	return cancelled, nil
}

// cancelRiskOrdersIfHighTx cancels the task's open risk orders when its
// counted tier is high, in the caller's transaction, which holds
// lockWorkOrderTaskTx. A plan approval that makes a risk agent plan rating
// count calls it (req-risk-based-review AC-4.3, AC-12.3).
func cancelRiskOrdersIfHighTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, at time.Time) error {
	counted, err := listRiskRatings(ctx, tx, taskID)
	if err != nil || !store.RiskTierReachesHigh(counted) {
		return err
	}
	_, err = cancelRiskOrdersTx(ctx, tx, q, taskID, "", store.RiskTierHighCancelReason, at)
	return err
}

// riskOrderTx reads one risk order and its subject row, locking the order.
func riskOrderTx(ctx context.Context, tx pgx.Tx, workspaceID, id string) (core.RiskOrder, error) {
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspaceID, id))
	if err != nil {
		return core.RiskOrder{}, notFound(err, "work order %s", id)
	}
	if order.Stage != core.StageRisk {
		return core.RiskOrder{}, fmt.Errorf("%w: %s", store.ErrNotRiskOrder, id)
	}
	risk, found, err := scanRiskOrderSubject(tx.QueryRow(ctx, `SELECT plan_version,head_sha,base_sha,changed_paths,matched_rules,created_at
		FROM risk_orders WHERE workspace_id=$1 AND work_order_id=$2`, workspaceID, id))
	if err != nil {
		return core.RiskOrder{}, err
	}
	if !found {
		return core.RiskOrder{}, fmt.Errorf("%w: %s has no subject", store.ErrNotRiskOrder, id)
	}
	risk.Order = order
	return risk.Clone(), nil
}

func scanRiskOrderSubject(row pgx.Row) (core.RiskOrder, bool, error) {
	var risk core.RiskOrder
	var paths, rules []byte
	err := row.Scan(&risk.Subject.PlanVersion, &risk.Subject.HeadSHA, &risk.Subject.BaseSHA, &paths, &rules, &risk.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.RiskOrder{}, false, nil
	}
	if err != nil {
		return core.RiskOrder{}, false, err
	}
	if err = json.Unmarshal(paths, &risk.ChangedPaths); err != nil {
		return core.RiskOrder{}, false, err
	}
	if err = json.Unmarshal(rules, &risk.MatchedRules); err != nil {
		return core.RiskOrder{}, false, err
	}
	risk.CreatedAt = risk.CreatedAt.UTC()
	return risk, true, nil
}

// taskRiskOrdersTx reads a task's risk orders with their subjects, oldest
// first.
func taskRiskOrdersTx(ctx context.Context, q boundaryQuerier, workspaceID, taskID string) ([]core.RiskOrder, error) {
	rows, err := q.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='risk' ORDER BY created_at,id", workspaceID, taskID)
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
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	subjects := map[string]core.RiskOrder{}
	subjectRows, err := q.Query(ctx, `SELECT work_order_id,plan_version,head_sha,base_sha,changed_paths,matched_rules,created_at
		FROM risk_orders WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	for subjectRows.Next() {
		var id string
		var risk core.RiskOrder
		var paths, rules []byte
		if err = subjectRows.Scan(&id, &risk.Subject.PlanVersion, &risk.Subject.HeadSHA, &risk.Subject.BaseSHA, &paths, &rules, &risk.CreatedAt); err != nil {
			subjectRows.Close()
			return nil, err
		}
		if err = json.Unmarshal(paths, &risk.ChangedPaths); err != nil {
			subjectRows.Close()
			return nil, err
		}
		if err = json.Unmarshal(rules, &risk.MatchedRules); err != nil {
			subjectRows.Close()
			return nil, err
		}
		risk.CreatedAt = risk.CreatedAt.UTC()
		subjects[id] = risk
	}
	subjectRows.Close()
	if err = subjectRows.Err(); err != nil {
		return nil, err
	}
	result := []core.RiskOrder{}
	for _, order := range orders {
		risk, ok := subjects[order.ID]
		if !ok {
			continue
		}
		risk.Order = order
		result = append(result, risk.Clone())
	}
	store.SortRiskOrders(result)
	return result, nil
}

// recordClaimIdentityTx appends one accepted claim to the claim ledger in the
// claim transaction.
func recordClaimIdentityTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, tokenHash string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO work_order_claim_identities (workspace_id,task_id,work_order_id,attempt_id,stage,session_id,client_token_hash,claimed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, workspace(ctx), order.TaskID, order.ID, order.AttemptID, string(order.Stage), order.SessionID, tokenHash, at)
	return err
}

// riskClaimCheckTx applies the risk claim refusal in the claim transaction
// from the claim ledger, the task's order rows, and its claim events, and
// returns the earlier claims whose token fingerprint is no longer stored
// (req-risk-based-review AC-12.1).
func riskClaimCheckTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, sessionID, tokenHash string, taskOrders []core.WorkOrder) ([]store.RiskTokenHistoryGap, error) {
	if order.Stage != core.StageRisk {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT task_id,work_order_id,attempt_id,stage,session_id,client_token_hash,claimed_at
		FROM work_order_claim_identities WHERE workspace_id=$1 AND task_id=$2 AND stage IN ('spec','implement','review')`, workspace(ctx), order.TaskID)
	if err != nil {
		return nil, err
	}
	var ledger []store.ClaimIdentity
	for rows.Next() {
		var identity store.ClaimIdentity
		if err = rows.Scan(&identity.TaskID, &identity.WorkOrderID, &identity.AttemptID, &identity.Stage, &identity.SessionID, &identity.ClientTokenHash, &identity.ClaimedAt); err != nil {
			rows.Close()
			return nil, err
		}
		ledger = append(ledger, identity)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	eventRows, err := tx.Query(ctx, `SELECT payload_json FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='work_order.claimed'`, workspace(ctx), order.TaskID)
	if err != nil {
		return nil, err
	}
	var events []core.Event
	for eventRows.Next() {
		var payload []byte
		if err = eventRows.Scan(&payload); err != nil {
			eventRows.Close()
			return nil, err
		}
		events = append(events, core.Event{Kind: "work_order.claimed", Payload: payload})
	}
	eventRows.Close()
	if err = eventRows.Err(); err != nil {
		return nil, err
	}
	return store.RiskClaimCheck(order, sessionID, tokenHash, ledger, taskOrders, events)
}

// wakeTaskAfterRiskOrderTx enqueues an enforcing task's dispatch job in the
// caller's transaction when one of its risk orders completed or was cancelled
// while the task is queued, so a review round that waited for the order opens
// with the tier as it then stands (feature-risk-based-review RR-12;
// req-risk-based-review AC-5.10). A task that does not enforce has no round
// waiting, so nothing is enqueued (component-risk-review invariant 13).
func (s *Store) wakeTaskAfterRiskOrderTx(ctx context.Context, tx pgx.Tx, task core.Task) error {
	if !store.RiskRoundHoldApplies(task) {
		return nil
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), task.ID).Scan(&state); err != nil {
		return notFound(err, "task %s", task.ID)
	}
	if core.TaskState(state) != core.TaskQueued {
		return nil
	}
	_, err := s.enqueueTaskTx(ctx, tx, task.ID, workspace(ctx))
	return err
}

// applyHeadRiskOrderTx applies a head handoff's risk effects in the caller's
// transaction, which holds lockWorkOrderTaskTx for the task: it cancels open
// risk orders of other heads and creates the head's risk order when it needs
// one, from ratings and orders read in the same transaction
// (feature-risk-based-review RR-12; req-risk-based-review AC-5.8, AC-5.9,
// AC-5.11).
func applyHeadRiskOrderTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, request store.HeadRiskOrderRequest) (store.HeadRiskOrderResult, error) {
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return store.HeadRiskOrderResult{}, notFound(err, "task %s", taskID)
	}
	task := taskFromDB(row)
	ratings, err := listRiskRatings(ctx, tx, taskID)
	if err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	orders, err := taskRiskOrdersTx(ctx, tx, workspace(ctx), taskID)
	if err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	plan, err := store.PlanHeadRiskOrder(task, ratings, orders, request)
	if err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	now := request.Now.UTC()
	if request.Now.IsZero() {
		now = time.Now().UTC()
	}
	result := store.HeadRiskOrderResult{Cancelled: []string{}}
	for _, id := range plan.CancelIDs {
		ids, cancelErr := cancelRiskOrdersTx(ctx, tx, q, taskID, id, store.RiskEarlierHeadCancelReason, now)
		if cancelErr != nil {
			return store.HeadRiskOrderResult{}, cancelErr
		}
		result.Cancelled = append(result.Cancelled, ids...)
	}
	if plan.Create != nil {
		if result.Order, result.Created, err = createRiskOrderTx(ctx, tx, q, task, *plan.Create); err != nil {
			return store.HeadRiskOrderResult{}, err
		}
	} else if existing, ok := store.HeadRiskOrderFor(orders, request.Subject.HeadSHA); ok && existing.Subject.Key() == request.Subject.Key() {
		result.Order = existing.Order
	}
	return result, nil
}

func (s *Store) EnsureHeadRiskOrderCommand(ctx context.Context, lease taskops.TaskLease, request store.HeadRiskOrderRequest) (store.HeadRiskOrderResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "EnsureHeadRiskOrderCommand")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	if err := request.Validate(); err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return store.HeadRiskOrderResult{}, fmt.Errorf("head risk order requires a valid taskops lease")
	}
	var result store.HeadRiskOrderResult
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), request.TaskID); err != nil {
			return err
		}
		var err error
		result, err = applyHeadRiskOrderTx(ctx, tx, q, request.TaskID, request)
		return err
	})
	if err != nil {
		return store.HeadRiskOrderResult{}, err
	}
	return result, nil
}
