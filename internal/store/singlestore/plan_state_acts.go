package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// changeTaskPlanStateTx is the /policy plan_state branch of
// ChangeTaskPolicyCommand. The caller holds the task and request locks and has
// resolved replay. The effects row of component-plan-state invariant 4,
// task.setup.changed, and the replay record commit in the caller's
// transaction, or nothing does (req-task-plan-state AC-3.1; DEC-125(7)).
func (s *Store) changeTaskPlanStateTx(ctx context.Context, tx *sql.Tx, task core.Task, request store.SetupChangeRequest, identity []byte) (store.SetupChangeResult, error) {
	ws := documentWorkspace(ctx)
	orders, err := s.listOrders(ctx, tx, []string{task.ID})
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
	cancelled, err := cancelPlanActOrdersTx(ctx, tx, task.ID, effects.CancelWorkOrderIDs, store.PlanActSetPlanState, actor, now)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	if task, err = s.applyPlanStateEffectsTx(ctx, tx, task, effects); err != nil {
		return store.SetupChangeResult{}, err
	}
	result := store.SetupChangeResult{RequestID: request.RequestID, Task: task, ReviewTransition: request.ReviewTransition,
		UpdatedWorkOrders: []string{}, CreatedWorkOrders: []string{}, RetainedWorkOrders: []string{}, SupersededWorkOrders: []string{},
		CancelledWorkOrders: cancelled}
	if err = taskEvent(ctx, tx, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: actor.ID, ActorRole: actor.Role, At: now,
		Payload: core.JSONPayload(store.PlanStateSetupChangedPayload(ws, actor, task, request, effects))}); err != nil {
		return store.SetupChangeResult{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_setup_changes(workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES(?,?,?,?,?,?,?)`,
		ws, request.RequestID, task.ID, identity, core.JSONPayload(result), actor.ID, now)
	return result, err
}

// AttachTaskPlanCommand records an operator-attached plan in one task
// transaction, matching the memory and PostgreSQL backends
// (component-plan-state invariant 5; req-task-plan-state AC-3.2; DEC-125(6)).
func (s *Store) AttachTaskPlanCommand(ctx context.Context, lease taskops.TaskLease, raw store.AttachPlanRequest) (store.AttachPlanResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "AttachTaskPlanCommand")
	var result store.AttachPlanResult
	if _, err := store.RequireActor(ctx); err != nil {
		return result, err
	}
	request, validationErr := store.PrepareAttachPlanRequest(raw)
	request.Actor = store.ActorFromContext(ctx)
	if !lease.ValidForCommand(request.TaskID, taskops.SetupChangeCommand) {
		return result, fmt.Errorf("taskops lease does not authorize attaching a plan to task %s", request.TaskID)
	}
	if validationErr != nil {
		return result, validationErr
	}
	err := s.taskTx(ctx, request.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "task-setup-request:"+request.RequestID); err != nil {
			return err
		}
		identity := core.JSONPayload(store.AttachPlanIdentity(request))
		var priorWorkspace string
		var priorRequest, priorResult []byte
		err := tx.QueryRowContext(ctx, `SELECT workspace_id,request_json,result_json FROM task_setup_changes WHERE request_id=?`, request.RequestID).Scan(&priorWorkspace, &priorRequest, &priorResult)
		if err == nil {
			if priorWorkspace != ws || !equalJSON(identity, priorRequest) {
				return fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrSetupChangeConflict, request.RequestID)
			}
			return json.Unmarshal(priorResult, &result)
		}
		if err != sql.ErrNoRows {
			return err
		}
		task, err := getTaskRow(ctx, tx, request.TaskID)
		if err != nil {
			return notFound(err, "task %s", request.TaskID)
		}
		orders, err := s.listOrders(ctx, tx, []string{task.ID})
		if err != nil {
			return err
		}
		effects, err := store.PlanAttachPlan(task, orders)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		cancelled, err := cancelPlanActOrdersTx(ctx, tx, task.ID, effects.CancelWorkOrderIDs, store.PlanActAttachPlan, request.Actor, now)
		if err != nil {
			return err
		}
		var version int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_specs WHERE workspace_id=? AND task_id=?`, ws, task.ID).Scan(&version); err != nil {
			return err
		}
		spec := store.AttachedPlanSpecVersion(request, version, now)
		if _, err = writeRow(ctx, tx, rowWrite{table: "task_specs", operation: "INSERT", values: specRowValues(ctx, spec)}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, store.AttachedPlanVersionCreatedEvent(spec, request)); err != nil {
			return err
		}
		if task, err = s.applyPlanStateEffectsTx(ctx, tx, task, effects); err != nil {
			return err
		}
		result = store.AttachPlanResult{RequestID: request.RequestID, Task: task, PlanVersion: version, CancelledWorkOrders: cancelled}
		_, err = tx.ExecContext(ctx, `INSERT INTO task_setup_changes(workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES(?,?,?,?,?,?,?)`,
			ws, request.RequestID, task.ID, identity, core.JSONPayload(result), request.Actor.ID, now)
		return err
	})
	if err != nil {
		return store.AttachPlanResult{}, err
	}
	return result, nil
}

// applyPlanStateEffectsTx issues the act's lifecycle command with its
// plan-state move, or moves the state alone and appends its event.
func (s *Store) applyPlanStateEffectsTx(ctx context.Context, tx *sql.Tx, task core.Task, effects store.PlanStateEffects) (core.Task, error) {
	if command := effects.TaskCommand(); command != nil {
		return s.applyTaskCommandTx(ctx, tx, task.ID, *command)
	}
	move := effects.MoveEvent(task.ID)
	if move == nil {
		return task, nil
	}
	if err := taskWrite(ctx, tx, task.ID, map[string]any{"plan_state": string(effects.To)}); err != nil {
		return core.Task{}, err
	}
	task.PlanState = effects.To
	if err := taskEvent(ctx, tx, *move); err != nil {
		return core.Task{}, err
	}
	return task, s.applyPlanRiskMoveTx(ctx, tx, task.ID, effects.From, effects.Cause, time.Now().UTC())
}

// cancelPlanActOrdersTx cancels the named orders as the terminal sweep does,
// with retry suppressed for reason plan_state_changed (component-plan-state
// invariant 4; component-task-lifecycle invariant 32).
func cancelPlanActOrdersTx(ctx context.Context, tx *sql.Tx, taskID string, ids []string, act string, actor store.Actor, at time.Time) ([]string, error) {
	cancelled := []string{}
	if len(ids) == 0 {
		return cancelled, nil
	}
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,job_id,state,attempt_id,session_id FROM work_orders WHERE workspace_id=? AND task_id=? AND state NOT IN ('completed','cancelled') ORDER BY id FOR UPDATE`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	type planActOrder struct {
		id, job, attempt, session string
		state                     core.WorkOrderState
	}
	var orders []planActOrder
	for rows.Next() {
		var o planActOrder
		if err = rows.Scan(&o.id, &o.job, &o.state, &o.attempt, &o.session); err != nil {
			rows.Close()
			return nil, err
		}
		if selected[o.id] {
			orders = append(orders, o)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(orders) != len(selected) {
		return nil, fmt.Errorf("%w: a work order of task %s changed during the plan act", store.ErrSetupChangeConflict, taskID)
	}
	for _, o := range orders {
		if _, err = core.TransitionWorkOrder(o.state, core.WorkOrderCmdCancel); err != nil {
			return nil, err
		}
		v := map[string]any{"state": core.WorkOrderCancelled, "attempt_id": "", "updated_at": at,
			"retry_suppressed": true, "retry_suppression_reason": store.PlanStateChangedSuppressionReason, "next_retry_at": nil}
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
		written, writeErr := writeRow(ctx, tx, rowWrite{table: "work_orders", operation: "UPDATE", values: v, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": o.id, "state": o.state}})
		if writeErr != nil {
			return nil, writeErr
		}
		if affected, affectedErr := written.RowsAffected(); affectedErr != nil || affected != 1 {
			return nil, fmt.Errorf("work order %s changed during the plan act", o.id)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',ended_at=?,updated_at=? WHERE workspace_id=? AND task_id=? AND id=? AND state<>'done'`, at, at, documentWorkspace(ctx), taskID, o.job); err != nil {
			return nil, err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: taskID, JobID: o.job, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: store.PlanStateCancelPayload(o.id, o.attempt, o.session, o.state, act), At: at}); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, o.id)
	}
	return cancelled, nil
}
