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
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, raw store.WorkOrderPreemptRequest) (store.WorkOrderPreemptResult, error) {
	request, validationErr := store.PrepareWorkOrderPreemptRequest(raw)
	if validationErr != nil {
		return store.WorkOrderPreemptResult{}, validationErr
	}
	workspaceID := workspace(ctx)
	tx, err := s.begin(ctx)
	if err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "conveyor:work-order-preempt:"+request.RequestID); err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	identityJSON, _ := json.Marshal(request)
	var priorWorkspace string
	var priorRequest, priorResult []byte
	err = tx.QueryRow(ctx, `SELECT workspace_id,request_json,result_json FROM work_order_preemptions WHERE request_id=$1`, request.RequestID).Scan(&priorWorkspace, &priorRequest, &priorResult)
	if err == nil {
		if priorWorkspace != workspaceID || !equalJSON(priorRequest, identityJSON) {
			return store.WorkOrderPreemptResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", store.ErrWorkOrderPreemptConflict, request.RequestID)
		}
		var result store.WorkOrderPreemptResult
		if err = json.Unmarshal(priorResult, &result); err != nil {
			return store.WorkOrderPreemptResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.WorkOrderPreemptResult{}, err
	}
	current, err := scanWorkOrder(tx.QueryRow(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, request.WorkOrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.WorkOrderPreemptResult{}, fmt.Errorf("work order %s not found", request.WorkOrderID)
	}
	if err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	if !lease.ValidForCommand(current.TaskID, string(core.WorkOrderCmdPreempt)) {
		return store.WorkOrderPreemptResult{}, fmt.Errorf("work-order preempt requires a valid taskops lease")
	}
	now := time.Now().UTC()
	if current.State == core.WorkOrderClaimed && !current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now) {
		if _, err = s.transitionWorkOrderTx(ctx, tx, current, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
			return store.WorkOrderPreemptResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return store.WorkOrderPreemptResult{}, err
		}
		return store.WorkOrderPreemptResult{}, fmt.Errorf("%w: work order %s does not have an active claimed attempt", store.ErrWorkOrderPreemptConflict, current.ID)
	}
	if current.State == core.WorkOrderClaimed && !current.LeaseExpiresAt.After(now) {
		if _, err = s.expireWorkOrderClaimTx(ctx, tx, current, now); err != nil {
			return store.WorkOrderPreemptResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return store.WorkOrderPreemptResult{}, err
		}
		return store.WorkOrderPreemptResult{}, fmt.Errorf("%w: work order %s does not have an active claimed attempt", store.ErrWorkOrderPreemptConflict, current.ID)
	}
	retirement := current.State == core.WorkOrderQueued || current.State == core.WorkOrderStale
	if !retirement && (current.State != core.WorkOrderClaimed || current.SessionID == "" || current.AttemptID == "") {
		return store.WorkOrderPreemptResult{}, fmt.Errorf("%w: work order %s does not have an active claimed attempt", store.ErrWorkOrderPreemptConflict, current.ID)
	}
	if _, transitionErr := core.TransitionWorkOrder(current.State, core.WorkOrderCmdPreempt); transitionErr != nil {
		return store.WorkOrderPreemptResult{}, transitionErr
	}
	result := store.WorkOrderPreemptResult{RequestID: request.RequestID}
	lastAttemptID := current.AttemptID
	if lastAttemptID == "" {
		lastAttemptID = current.LastAttemptID
	}
	if !retirement {
		result.RevokedAttemptID, result.RevokedSessionID, result.RevokedWorkerID = current.AttemptID, current.SessionID, current.WorkerID
		result.GraceBound = "one renewal interval"
	}
	nextState, outcome, retrySuppressed, suppressionReason := core.WorkOrderQueued, core.WorkOrderOutcomePreempted, false, ""
	if retirement {
		nextState, outcome, retrySuppressed, suppressionReason = core.WorkOrderCancelled, core.WorkOrderOutcomeCancelled, true, "operator retirement"
	}
	updated, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET
		state=$1,claimant_id='',session_id='',attempt_id='',last_attempt_id=$2,
		client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',
		execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome=$3,
		last_failure_category='',last_failure_message='',last_failure_detail='',last_failure_exit_status=NULL,last_failure_at=NULL,
		next_retry_at=NULL,retry_suppressed=$4,retry_suppression_reason=$5,updated_at=$6
		WHERE workspace_id=$7 AND id=$8 AND state=$9
		RETURNING `+workOrderColumns,
		nextState, lastAttemptID, outcome, retrySuppressed, suppressionReason, now, workspaceID, current.ID, current.State))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.WorkOrderPreemptResult{}, fmt.Errorf("%w: work order %s changed concurrently", store.ErrWorkOrderPreemptConflict, current.ID)
	}
	if err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	updated.Claimable = updated.ClaimableAt(now)
	result.WorkOrder = updated
	jobState := core.JobPending
	var endedAt any
	if retirement {
		jobState, endedAt = core.JobFailed, now
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state=$1,started_at=CASE WHEN $1='pending' THEN NULL ELSE started_at END,ended_at=$2,updated_at=$3 WHERE id=$4`, jobState, endedAt, now, current.JobID); err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	actor := store.ActorFromContext(ctx)
	q := s.queries.WithTx(tx)
	eventKind := "work_order.preempted"
	if retirement {
		eventKind = "work_order.retired"
	}
	if err = insertEvent(ctx, q, core.Event{TaskID: current.TaskID, JobID: current.JobID, Kind: eventKind, ActorID: actor.ID, ActorRole: actor.Role,
		Payload: core.JSONPayload(map[string]any{
			"work_order_id": current.ID, "request_id": request.RequestID, "reason": request.Reason,
			"attempt_id": result.RevokedAttemptID, "session_id": result.RevokedSessionID, "worker_id": result.RevokedWorkerID,
			"prior_state": current.State, "new_state": updated.State, "command": core.WorkOrderCmdPreempt,
			"queue_entered_at": updated.QueueEnteredAt, "queue_deadline": updated.QueueDeadline, "grace_bound": result.GraceBound,
		}), At: now}); err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.Exec(ctx, `INSERT INTO work_order_preemptions
		(workspace_id,request_id,work_order_id,request_json,result_json,revoked_attempt_id,revoked_session_id,revoked_worker_id,actor_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, workspaceID, request.RequestID, current.ID, identityJSON, resultJSON,
		result.RevokedAttemptID, result.RevokedSessionID, result.RevokedWorkerID, actor.ID, now); err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	return result, nil
}
