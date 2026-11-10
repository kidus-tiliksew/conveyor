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

// changeTaskPlanStateTx is the /policy plan_state branch of
// ChangeTaskPolicyCommand. The caller holds the task-operation, request, and
// claim locks and has resolved replay. The effects row of
// component-plan-state invariant 4, task.setup.changed, and the replay record
// commit in the caller's transaction, or nothing does (req-task-plan-state
// AC-3.1; DEC-125(7)).
func (s *Store) changeTaskPlanStateTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, request store.SetupChangeRequest, identityJSON []byte) (store.SetupChangeResult, error) {
	workspaceID := workspace(ctx)
	orders, err := planActOrdersTx(ctx, tx, workspaceID, task.ID)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	request, err = store.PlanTaskPolicyChange(task, orders, request, store.PolicyHandoffSeatInputs{})
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	effects := *request.PlanStateEffects
	actor := store.ActorFromContext(ctx)
	now := time.Now().UTC()
	cancelled, err := cancelPlanActOrdersTx(ctx, tx, q, task.ID, effects.CancelWorkOrderIDs, store.PlanActSetPlanState, actor, now)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	if task, err = s.applyPlanStateEffectsTx(ctx, tx, q, task, effects); err != nil {
		return store.SetupChangeResult{}, err
	}
	result := store.SetupChangeResult{RequestID: request.RequestID, Task: task, ReviewTransition: request.ReviewTransition,
		UpdatedWorkOrders: []string{}, CreatedWorkOrders: []string{}, RetainedWorkOrders: []string{}, SupersededWorkOrders: []string{},
		CancelledWorkOrders: cancelled}
	if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: actor.ID, ActorRole: actor.Role, At: now,
		Payload: core.JSONPayload(store.PlanStateSetupChangedPayload(workspaceID, actor, task, request, effects))}); err != nil {
		return store.SetupChangeResult{}, err
	}
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `INSERT INTO task_setup_changes (workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		workspaceID, request.RequestID, task.ID, identityJSON, resultJSON, actor.ID, now); err != nil {
		return store.SetupChangeResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.SetupChangeResult{}, err
	}
	return result, nil
}

// AttachTaskPlanCommand records an operator-attached plan in one
// transaction: replay through task_setup_changes, the predicate of
// component-plan-state invariant 5, order cancellation, the next plan version
// with origin operator, and the move to awaiting_approval, through gate.spec
// after triage (req-task-plan-state AC-3.2; DEC-125(6)).
func (s *Store) AttachTaskPlanCommand(ctx context.Context, lease taskops.TaskLease, raw store.AttachPlanRequest) (store.AttachPlanResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "AttachTaskPlanCommand")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.AttachPlanResult{}, err
	}
	request, validationErr := store.PrepareAttachPlanRequest(raw)
	request.Actor = store.ActorFromContext(ctx)
	if !lease.ValidForCommand(request.TaskID, taskops.SetupChangeCommand) {
		return store.AttachPlanResult{}, fmt.Errorf("taskops lease does not authorize attaching a plan to task %s", request.TaskID)
	}
	if validationErr != nil {
		return store.AttachPlanResult{}, validationErr
	}
	workspaceID := workspace(ctx)
	tx, err := s.beginInTaskSession(ctx)
	if err != nil {
		return store.AttachPlanResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = lockWorkOrderTaskTx(ctx, tx, workspaceID, request.TaskID); err != nil {
		return store.AttachPlanResult{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:task-setup-request:"+request.RequestID); err != nil {
		return store.AttachPlanResult{}, err
	}
	identityJSON, _ := json.Marshal(store.AttachPlanIdentity(request))
	var priorWorkspace string
	var priorRequest, priorResult []byte
	err = tx.QueryRow(ctx, `SELECT workspace_id,request_json,result_json FROM task_setup_changes WHERE request_id=$1`, request.RequestID).Scan(&priorWorkspace, &priorRequest, &priorResult)
	if err == nil {
		if priorWorkspace != workspaceID || !equalJSON(priorRequest, identityJSON) {
			return store.AttachPlanResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrSetupChangeConflict, request.RequestID)
		}
		var result store.AttachPlanResult
		if err = json.Unmarshal(priorResult, &result); err != nil {
			return store.AttachPlanResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.AttachPlanResult{}, err
	}
	// Claim and the attach share this lock, closing the claim-versus-attach
	// race across control-plane instances.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", fmt.Sprintf("conveyor:work-order-claim:%s:%s", workspaceID, request.TaskID)); err != nil {
		return store.AttachPlanResult{}, err
	}
	q := s.queries.WithTx(tx)
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
	if err != nil {
		return store.AttachPlanResult{}, notFound(err, "task %s", request.TaskID)
	}
	task := taskFromDB(row)
	orders, err := planActOrdersTx(ctx, tx, workspaceID, task.ID)
	if err != nil {
		return store.AttachPlanResult{}, err
	}
	effects, err := store.PlanAttachPlan(task, orders)
	if err != nil {
		return store.AttachPlanResult{}, err
	}
	now := time.Now().UTC()
	cancelled, err := cancelPlanActOrdersTx(ctx, tx, q, task.ID, effects.CancelWorkOrderIDs, store.PlanActAttachPlan, request.Actor, now)
	if err != nil {
		return store.AttachPlanResult{}, err
	}
	var version int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_specs WHERE task_id=$1`, task.ID).Scan(&version); err != nil {
		return store.AttachPlanResult{}, err
	}
	spec := store.AttachedPlanSpecVersion(request, version, now)
	if _, err = tx.Exec(ctx, `INSERT INTO task_specs (task_id,version,content,acceptance_count,acceptance,decomposition,approved,created_at,approved_at,agent,model,review_seats,origin,base_commit)
VALUES ($1,$2,$3,$4,$5,$6,false,$7,NULL,'','',NULL,$8,$9)`,
		spec.TaskID, spec.Version, spec.Content, spec.AcceptanceCount, spec.Acceptance, spec.Decomposition,
		timestamp(spec.CreatedAt), string(spec.Origin), spec.BaseCommit); err != nil {
		return store.AttachPlanResult{}, err
	}
	if err = insertEvent(ctx, q, store.AttachedPlanVersionCreatedEvent(spec, request)); err != nil {
		return store.AttachPlanResult{}, err
	}
	if task, err = s.applyPlanStateEffectsTx(ctx, tx, q, task, effects); err != nil {
		return store.AttachPlanResult{}, err
	}
	result := store.AttachPlanResult{RequestID: request.RequestID, Task: task, PlanVersion: version, CancelledWorkOrders: cancelled}
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `INSERT INTO task_setup_changes (workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		workspaceID, request.RequestID, task.ID, identityJSON, resultJSON, request.Actor.ID, now); err != nil {
		return store.AttachPlanResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.AttachPlanResult{}, err
	}
	return result, nil
}

// planActOrdersTx reads the task's orders inside the act's transaction.
func planActOrdersTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) ([]core.WorkOrder, error) {
	rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

// applyPlanStateEffectsTx issues the act's lifecycle command with its
// plan-state move, or moves the state alone and appends its event.
func (s *Store) applyPlanStateEffectsTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, effects store.PlanStateEffects) (core.Task, error) {
	if command := effects.TaskCommand(); command != nil {
		updated, err := s.applyTaskCommandTx(ctx, tx, q, task.ID, *command)
		if err != nil {
			return core.Task{}, err
		}
		return updated, nil
	}
	move := effects.MoveEvent(task.ID)
	if move == nil {
		return task, nil
	}
	if err := setTaskPlanStateTx(ctx, tx, task.ID, effects.To); err != nil {
		return core.Task{}, err
	}
	task.PlanState = effects.To
	return task, insertEvent(ctx, q, *move)
}

// cancelPlanActOrdersTx cancels the named orders as the terminal sweep does,
// a claimed one losing its claim identity, lease, and execution window, with
// retry suppressed for reason plan_state_changed, its unfinished job failed,
// and one work_order.cancelled each (component-plan-state invariant 4;
// component-task-lifecycle invariant 32).
func cancelPlanActOrdersTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, ids []string, act string, actor store.Actor, at time.Time) ([]string, error) {
	cancelled := []string{}
	if len(ids) == 0 {
		return cancelled, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND id=ANY($3) AND state NOT IN ('completed','cancelled') ORDER BY id FOR UPDATE`, workspace(ctx), taskID, ids)
	if err != nil {
		return nil, err
	}
	type planActOrder struct {
		id, jobID, attemptID, sessionID string
		state                           core.WorkOrderState
	}
	var orders []planActOrder
	for rows.Next() {
		var order planActOrder
		if err = rows.Scan(&order.id, &order.jobID, &order.state, &order.attemptID, &order.sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		orders = append(orders, order)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(orders) != len(ids) {
		return nil, fmt.Errorf("%w: a work order of task %s changed during the plan act", store.ErrSetupChangeConflict, taskID)
	}
	for _, order := range orders {
		if _, err = core.TransitionWorkOrder(order.state, core.WorkOrderCmdCancel); err != nil {
			return nil, err
		}
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
			updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND state=$4`, at, workspace(ctx), order.id, order.state, store.PlanStateChangedSuppressionReason)
		if updateErr != nil {
			return nil, updateErr
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("work order %s changed during the plan act", order.id)
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1,updated_at=$1 WHERE task_id=$2 AND id=$3 AND state<>'done'`, at, taskID, order.jobID); err != nil {
			return nil, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: order.jobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: store.PlanStateCancelPayload(order.id, order.attemptID, order.sessionID, order.state, act), At: at}); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, order.id)
	}
	return cancelled, nil
}
