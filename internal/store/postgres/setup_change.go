package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) ChangeTaskSetupCommand(ctx context.Context, lease taskops.TaskLease, raw store.SetupChangeRequest) (store.SetupChangeResult, error) {
	request, validationErr := store.PrepareSetupChangeRequest(raw)
	if !lease.ValidForCommand(request.TaskID, taskops.SetupChangeCommand) {
		return store.SetupChangeResult{}, fmt.Errorf("taskops lease does not authorize setup change for task %s", request.TaskID)
	}
	if validationErr != nil {
		return store.SetupChangeResult{}, validationErr
	}
	workspaceID := workspace(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:task-setup-request:"+request.RequestID); err != nil {
		return store.SetupChangeResult{}, err
	}
	identityJSON, _ := json.Marshal(store.SetupChangeIdentity(request))
	var priorWorkspace string
	var priorRequest, priorResult []byte
	err = tx.QueryRow(ctx, `SELECT workspace_id,request_json,result_json FROM task_setup_changes WHERE request_id=$1`, request.RequestID).Scan(&priorWorkspace, &priorRequest, &priorResult)
	if err == nil {
		if priorWorkspace != workspaceID || !equalJSON(priorRequest, identityJSON) {
			return store.SetupChangeResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrSetupChangeConflict, request.RequestID)
		}
		var result store.SetupChangeResult
		if err = json.Unmarshal(priorResult, &result); err != nil {
			return store.SetupChangeResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.SetupChangeResult{}, err
	}
	// Claim and setup change share this lock, closing the claim-versus-change
	// race across control-plane instances (spec §21.35 change 2).
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", fmt.Sprintf("conveyor:work-order-claim:%s:%s", workspaceID, request.TaskID)); err != nil {
		return store.SetupChangeResult{}, err
	}
	q := s.queries.WithTx(tx)
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: request.TaskID, WorkspaceID: workspaceID})
	if err != nil {
		return store.SetupChangeResult{}, notFound(err, "task %s", request.TaskID)
	}
	task := taskFromDB(row)
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
		return store.SetupChangeResult{}, fmt.Errorf("%w: terminal task %s cannot change setup", store.ErrSetupChangeConflict, task.ID)
	}
	// Submitted spec/implement attempts are delivered, not executing; only
	// claimed attempts and in-flight review verdicts block (spec §21.36).
	var claimedAttempt, inFlightVerdict bool
	if err = tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND state='claimed'),
			EXISTS (SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND state='submitted')`, workspaceID, task.ID).Scan(&claimedAttempt, &inFlightVerdict); err != nil {
		return store.SetupChangeResult{}, err
	}
	if claimedAttempt {
		return store.SetupChangeResult{}, fmt.Errorf("%w: task %s has a claimed attempt", store.ErrSetupChangeConflict, task.ID)
	}
	if inFlightVerdict {
		return store.SetupChangeResult{}, fmt.Errorf("%w: task %s has an in-flight review verdict", store.ErrSetupChangeConflict, task.ID)
	}
	for _, desired := range request.WorkOrderUpdates {
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND state='queued' AND session_id='' AND worker_id='')`, workspaceID, task.ID, desired.ID).Scan(&valid); err != nil {
			return store.SetupChangeResult{}, err
		}
		if !valid {
			return store.SetupChangeResult{}, fmt.Errorf("%w: work order %s is not an unclaimed queued order", store.ErrSetupChangeConflict, desired.ID)
		}
	}
	priorSetup := task.SetupContract
	if _, err = tx.Exec(ctx, `UPDATE tasks SET setup_name=$1,setup_contract=$2,updated_at=now() WHERE workspace_id=$3 AND id=$4`, request.Setup.Name, setupContractJSON(request.Setup), workspaceID, task.ID); err != nil {
		return store.SetupChangeResult{}, err
	}
	task.SetupName, task.SetupContract = request.Setup.Name, request.Setup
	result := store.SetupChangeResult{RequestID: request.RequestID, Task: task, ReviewTransition: request.ReviewTransition,
		UpdatedWorkOrders: []string{}, CreatedWorkOrders: []string{}, RetainedWorkOrders: append([]string{}, request.RetainedWorkOrderIDs...), SupersededWorkOrders: append([]string{}, request.SupersedeWorkOrderIDs...)}
	now := time.Now().UTC()
	actor := store.ActorFromContext(ctx)
	for _, desired := range request.WorkOrderUpdates {
		command, updateErr := tx.Exec(ctx, `UPDATE work_orders SET required_model=$1,required_harness=$2,required_effort=$3,required_harness_config=$4,execution_timeout=$5,
			last_attempt_outcome='',last_failure_message='',last_failure_detail='',last_failure_exit_status=NULL,last_failure_at=NULL,
			automatic_retry_count=0,next_retry_at=NULL,retry_suppressed=false,retry_suppression_reason='',queue_entered_at=$6,queue_deadline=$7,
			redispatch_count=redispatch_count+1,updated_at=$8 WHERE workspace_id=$9 AND task_id=$10 AND id=$11 AND state='queued' AND session_id='' AND worker_id=''`,
			desired.RequiredModel, desired.RequiredHarness, desired.RequiredEffort, harnessSnapshotJSON(desired.RequiredHarnessConfig), desired.ExecutionTimeoutText,
			desired.QueueEnteredAt, desired.QueueDeadline, now, workspaceID, task.ID, desired.ID)
		if updateErr != nil {
			return store.SetupChangeResult{}, updateErr
		}
		if command.RowsAffected() != 1 {
			return store.SetupChangeResult{}, fmt.Errorf("%w: work order %s changed concurrently", store.ErrSetupChangeConflict, desired.ID)
		}
		result.UpdatedWorkOrders = append(result.UpdatedWorkOrders, desired.ID)
		if desired.Stage == core.StageReview {
			if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: desired.JobID, Kind: "review.seat.setup_rebuilt", ActorID: actor.ID, ActorRole: actor.Role,
				Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "request_id": request.RequestID, "review_round": desired.ReviewRound, "review_seat": desired.ReviewSeat, "work_order_id": desired.ID, "outcome": "rebuilt_future_work", "previous_setup": priorSetup, "new_setup": request.Setup}), At: now}); err != nil {
				return store.SetupChangeResult{}, err
			}
		}
	}
	for _, id := range request.SupersedeWorkOrderIDs {
		var jobID, state string
		if err = tx.QueryRow(ctx, `SELECT job_id,state FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND id=$3 FOR UPDATE`, workspaceID, task.ID, id).Scan(&jobID, &state); err != nil {
			return store.SetupChangeResult{}, notFound(err, "work order %s", id)
		}
		resultingState := state
		if _, err = tx.Exec(ctx, `UPDATE work_orders SET review_superseded=true,updated_at=$1 WHERE workspace_id=$2 AND id=$3`, now, workspaceID, id); err != nil {
			return store.SetupChangeResult{}, err
		}
		if state == string(core.WorkOrderQueued) {
			resulting, transitionErr := core.TransitionWorkOrder(core.WorkOrderState(state), core.WorkOrderCmdCancel)
			if transitionErr != nil {
				return store.SetupChangeResult{}, transitionErr
			}
			resultingState = string(resulting)
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET state='cancelled',updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND state='queued'`, now, workspaceID, id); err != nil {
				return store.SetupChangeResult{}, err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: jobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
				Payload: core.JSONPayload(map[string]any{"id": id, "state": core.WorkOrderCancelled, "from": state, "command": core.WorkOrderCmdCancel}), At: now}); err != nil {
				return store.SetupChangeResult{}, err
			}
			if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1,updated_at=$1 WHERE id=$2`, now, jobID); err != nil {
				return store.SetupChangeResult{}, err
			}
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: jobID, Kind: "review.seat.setup_superseded", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "request_id": request.RequestID, "work_order_id": id, "prior_state": state,
				"resulting_state": resultingState, "outcome": "historical_only", "previous_setup": priorSetup, "new_setup": request.Setup}), At: now}); err != nil {
			return store.SetupChangeResult{}, err
		}
	}
	for i, job := range request.NewJobs {
		order := request.NewWorkOrders[i]
		createdState, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if transitionErr != nil {
			return store.SetupChangeResult{}, transitionErr
		}
		order.State = createdState
		if job.TaskID != task.ID || order.TaskID != task.ID || order.JobID != job.ID || order.Stage != job.Stage {
			return store.SetupChangeResult{}, fmt.Errorf("invalid setup-change review member %d", i)
		}
		if _, err = q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return store.SetupChangeResult{}, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now}); err != nil {
			return store.SetupChangeResult{}, err
		}
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = now
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = now.Add(config.DefaultWorkOrderQueueTimeout)
		}
		_, err = tx.Exec(ctx, `INSERT INTO work_orders (
			id,workspace_id,task_id,job_id,stage,state,claimant_id,session_id,client_token_hash,agent,model,worker_id,lease_expires_at,
			review_round,review_seat,required_model,required_harness,required_effort,required_harness_config,execution_timeout,model_enforcement,
			reason_code,review_kind,review_scope,baseline_sha,head_sha,queue_entered_at,queue_deadline,execution_started_at,execution_deadline,
			last_attempt_outcome,last_failure_message,last_failure_detail,last_failure_exit_status,last_failure_at,automatic_retry_count,next_retry_at,
			retry_suppressed,retry_suppression_reason,redispatch_count,progress,cost_usd,tokens_in,tokens_out,usage_reported,self_reported,created_at,updated_at,served_requirement_snapshot,governance_snapshot)
			VALUES ($1,$2,$3,$4,$5,'queued','','','','','','',NULL,$6,$7,$8,$9,$10,$11,$12,'',$13,$14,$15,$16,$17,$18,$19,NULL,NULL,'','','',NULL,NULL,0,NULL,false,'',0,'',0,0,0,false,false,$20,$20,$21,$22)`,
			order.ID, workspaceID, task.ID, job.ID, order.Stage, order.ReviewRound, order.ReviewSeat, order.RequiredModel, order.RequiredHarness,
			order.RequiredEffort, harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText, order.ReasonCode, order.ReviewKind, order.ReviewScope, order.BaselineSHA, order.HeadSHA,
			order.QueueEnteredAt, order.QueueDeadline, order.CreatedAt, servedRequirementSnapshotJSON(order.ServedRequirementSnapshot), governanceSnapshotJSON(order.GovernanceSnapshot))
		if err != nil {
			return store.SetupChangeResult{}, err
		}
		result.CreatedWorkOrders = append(result.CreatedWorkOrders, order.ID)
		if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now}); err != nil {
			return store.SetupChangeResult{}, err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "review.seat.setup_rebuilt", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "request_id": request.RequestID, "review_round": order.ReviewRound, "review_seat": order.ReviewSeat, "work_order_id": order.ID, "outcome": "created_under_new_setup", "previous_setup": priorSetup, "new_setup": request.Setup}), At: now}); err != nil {
			return store.SetupChangeResult{}, err
		}
	}
	for _, id := range result.RetainedWorkOrders {
		if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: "review.seat.setup_retained", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "request_id": request.RequestID, "work_order_id": id, "outcome": "retained_compatible_verdict", "previous_setup": priorSetup, "new_setup": request.Setup}), At: now}); err != nil {
			return store.SetupChangeResult{}, err
		}
	}
	payload := map[string]any{"workspace_id": workspaceID, "task_id": task.ID, "actor": actor.ID, "request_id": request.RequestID, "reason": request.Reason,
		"previous_setup": priorSetup, "new_setup": request.Setup, "lifecycle_boundary": "future_work", "stage": task.NextStage,
		"updated_work_order_ids": result.UpdatedWorkOrders, "review_transition": map[string]any{"kind": request.ReviewTransition, "prior_round": request.PriorReviewRound,
			"resulting_round": request.ResultingReviewRound, "retained_work_order_ids": result.RetainedWorkOrders, "superseded_work_order_ids": result.SupersededWorkOrders, "created_work_order_ids": result.CreatedWorkOrders}}
	if err = insertEvent(ctx, q, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(payload), At: now}); err != nil {
		return store.SetupChangeResult{}, err
	}
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `INSERT INTO task_setup_changes (workspace_id,request_id,task_id,request_json,result_json,actor_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, workspaceID, request.RequestID, task.ID, identityJSON, resultJSON, actor.ID, now); err != nil {
		return store.SetupChangeResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.SetupChangeResult{}, err
	}
	return result, nil
}

func equalJSON(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

func supersededReviewWorkOrdersTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT e.payload_json FROM events e JOIN tasks t ON t.id=e.task_id WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='task.setup.changed' ORDER BY e.id`, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, err
		}
		events = append(events, core.Event{Kind: "task.setup.changed", Payload: payload})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return store.SupersededReviewWorkOrders(events), nil
}
