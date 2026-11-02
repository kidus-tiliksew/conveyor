package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type workOrderRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func cancelledSessionMatches(ctx context.Context, q workOrderRowQuerier, workspaceID, taskID, jobID, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var matches bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM events e
		JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.job_id=$3
		  AND e.kind='work_order.cancelled' AND e.payload_json->>'session_id'=$4
	)`, workspaceID, taskID, jobID, sessionID).Scan(&matches)
	return matches, err
}

func (s *Store) RenewWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, lease time.Duration) (core.WorkOrder, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RenewWorkerClaimCommand")
	// The renewal and its lease_renewed event are separate statements, so the
	// event actor (the worker, or the caller for a run claim) is checked before
	// the lease changes (component-persistence, Actor context).
	if _, err := store.RequireActor(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	current, err := s.GetWorkOrder(ctx, workOrderID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if !taskLease.ValidForCommand(current.TaskID, string(core.WorkOrderCmdRenew)) {
		return core.WorkOrder{}, fmt.Errorf("work-order renewal requires a valid taskops lease")
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	row := s.boundary.QueryRow(ctx, `UPDATE work_orders SET lease_expires_at=CASE WHEN execution_deadline IS NULL THEN $1 ELSE LEAST($1,execution_deadline) END,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND worker_id=$5 AND claimant_id=$6 AND session_id=$7 AND state='claimed' AND lease_expires_at>$2 AND (execution_deadline IS NULL OR execution_deadline>$2) RETURNING `+workOrderColumns, expires, now, workspace(ctx), workOrderID, claim.WorkerID, claim.ClaimantID, claim.SessionID)
	order, err := scanWorkOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := s.GetWorkOrder(ctx, workOrderID)
		if getErr == nil && current.State == core.WorkOrderCancelled {
			matches, matchErr := cancelledSessionMatches(ctx, s.boundary, workspace(ctx), current.TaskID, current.JobID, claim.SessionID)
			if matchErr != nil {
				return core.WorkOrder{}, matchErr
			}
			if (current.LastAttemptID != "" && current.LastAttemptID == claim.SessionID) || matches ||
				(current.WorkerID == claim.WorkerID && current.ClaimantID == claim.ClaimantID && current.SessionID == claim.SessionID) {
				return core.WorkOrder{}, store.ErrWorkOrderCancelled
			}
		}
		if getErr == nil && current.WorkerID == claim.WorkerID && current.ClaimantID == claim.ClaimantID && current.SessionID == claim.SessionID && (current.State == core.WorkOrderSubmitted || current.State == core.WorkOrderCompleted) {
			return current, nil
		}
		if getErr == nil && current.State == core.WorkOrderQueued && current.WorkerID == "" && current.ClaimantID == "" && current.SessionID == "" &&
			(current.LastFailureMessage == core.WorkOrderReleaseReasonOperatorCheckpointReached ||
				current.LastFailureMessage == core.WorkOrderReleaseReasonPlanRevisionRequested) && current.LastAttemptID != "" {
			var releasedByClaim bool
			if matchErr := s.boundary.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3
				AND kind='work_order.claimed' AND payload_json->>'id'=$4
				AND payload_json->>'attempt_id'=$5 AND (
					($6<>'' AND payload_json->>'worker_id'=$6) OR
					($6='' AND $7 LIKE 'run:%' AND length($7)>4 AND NOT (payload_json ? 'worker_id'))
				)
				AND payload_json->>'claimed_by'=$7 AND payload_json->>'session_id'=$8
			)`, workspace(ctx), current.TaskID, current.JobID, current.ID, current.LastAttemptID,
				claim.WorkerID, claim.ClaimantID, claim.SessionID).Scan(&releasedByClaim); matchErr != nil {
				return core.WorkOrder{}, matchErr
			}
			if releasedByClaim {
				return core.WorkOrder{}, store.ErrWorkOrderReleasedAtCheckpoint
			}
		}
		var preempted bool
		if preemptErr := s.boundary.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM work_order_preemptions
			WHERE workspace_id=$1 AND work_order_id=$2 AND revoked_worker_id=$3 AND revoked_session_id=$4
		)`, workspace(ctx), workOrderID, claim.WorkerID, claim.SessionID).Scan(&preempted); preemptErr != nil {
			return core.WorkOrder{}, preemptErr
		}
		if preempted {
			return core.WorkOrder{}, store.ErrWorkOrderPreempted
		}
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	_ = s.AppendEvent(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": order.AttemptID, "lease_expires_at": order.LeaseExpiresAt})})
	return order, nil
}

func (s *Store) ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, release core.WorkOrderRelease) (core.WorkOrder, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReleaseWorkerClaimCommand")
	tx, err := s.begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now := time.Now().UTC()
	current, err := scanWorkOrder(tx.QueryRow(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), workOrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if !taskLease.ValidForCommand(current.TaskID, string(core.WorkOrderCmdRelease)) {
		return core.WorkOrder{}, fmt.Errorf("work-order release requires a valid taskops lease")
	}
	if current.Stage == core.StageReview {
		accepted, acceptedErr := reviewSeatAcceptedTx(ctx, tx, workspace(ctx), current.TaskID, current.ID)
		if acceptedErr != nil {
			return core.WorkOrder{}, acceptedErr
		}
		if accepted {
			return core.WorkOrder{}, fmt.Errorf("accepted review seat %s is terminal", workOrderID)
		}
	}
	if current.State == core.WorkOrderCancelled {
		matches, matchErr := cancelledSessionMatches(ctx, tx, workspace(ctx), current.TaskID, current.JobID, release.SessionID)
		if matchErr != nil {
			return core.WorkOrder{}, matchErr
		}
		if (current.LastAttemptID != "" && current.LastAttemptID == release.SessionID) || matches ||
			(current.WorkerID == claim.WorkerID && current.SessionID == release.SessionID) {
			return core.WorkOrder{}, store.ErrWorkOrderCancelled
		}
	}
	if current.SessionID == "" || current.SessionID != release.SessionID || current.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if _, transitionErr := core.TransitionWorkOrder(current.State, core.WorkOrderCmdRelease); transitionErr != nil {
		return core.WorkOrder{}, transitionErr
	}
	if !current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now) {
		if _, err = s.transitionWorkOrderTx(ctx, tx, current, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if !current.LeaseExpiresAt.After(now) {
		if _, err = s.expireWorkOrderClaimTx(ctx, tx, current, now); err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if current.WorkerID != claim.WorkerID || current.ClaimantID != claim.ClaimantID {
		return core.WorkOrder{}, store.ErrWorkOrderClaimUnauthorized
	}
	queueTimeout := current.QueueDeadline.Sub(current.QueueEnteredAt)
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	retryCount := current.AutomaticRetryCount
	nextRetry := time.Time{}
	suppressed := true
	lastFailureMessage := current.LastFailureMessage
	lastFailureDetail := current.LastFailureDetail
	lastFailureExitStatus := current.LastFailureExitStatus
	lastFailureAt := current.LastFailureAt
	lastFailureCategory := current.LastFailureCategory
	suppressionReason := ""
	progressed := false
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT kind='work_order.progress_reported' FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind IN ('work_order.claimed','work_order.progress_reported') ORDER BY at DESC, id DESC LIMIT 1), false)`, workspace(ctx), current.TaskID, current.JobID).Scan(&progressed); err != nil {
		return core.WorkOrder{}, err
	}
	previousTransientFailures := 0
	if current.LastFailureCategory == core.WorkOrderFailureTransientConnectivity {
		if err = tx.QueryRow(ctx, `SELECT COALESCE((payload_json->>'consecutive_transient_failures')::integer, 0) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind IN ('work_order.child_failed','work_order.stalled') ORDER BY at DESC, id DESC LIMIT 1`, workspace(ctx), current.TaskID, current.JobID).Scan(&previousTransientFailures); errors.Is(err, pgx.ErrNoRows) {
			previousTransientFailures = 0
		} else if err != nil {
			return core.WorkOrder{}, err
		}
	}
	consecutiveTransientFailures := 0
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		detail := strings.TrimSpace(release.FailureDetail)
		lastFailureMessage = strings.TrimSpace(release.Reason)
		lastFailureCategory = strings.TrimSpace(release.FailureCategory)
		lastFailureDetail = detail
		lastFailureExitStatus = release.ExitStatus
		lastFailureAt = now
		limit := release.AutomaticRetryLimit
		if limit <= 0 {
			limit = 3
		}
		transientConnectivity := lastFailureCategory == core.WorkOrderFailureTransientConnectivity
		consecutiveTransientFailures = core.ConsecutiveTransientFailureCount(lastFailureCategory, previousTransientFailures, progressed, current.LastAttemptOutcome == release.Outcome)
		identical := detail != "" && current.LastAttemptOutcome == release.Outcome && detail == current.LastFailureDetail && !transientConnectivity
		if retryCount < limit {
			retryCount++
			if identical {
				suppressionReason = core.IdenticalFailureSuppressionReason
			} else {
				delay := postgresRetryDelay(release, retryCount)
				if transientConnectivity {
					delay = core.TransientConnectivityRetryDelay(consecutiveTransientFailures)
				}
				nextRetry = now.Add(delay)
				suppressed = false
			}
		} else {
			suppressionReason = core.AttemptRetriesExhaustedReason
		}
		if transientConnectivity {
			lastFailureDetail = core.TransientConnectivityFailureDetail(detail, consecutiveTransientFailures, nextRetry)
		}
	} else {
		lastFailureCategory = ""
		lastFailureMessage = strings.TrimSpace(release.Reason)
		lastFailureDetail = strings.TrimSpace(release.FailureDetail)
		lastFailureExitStatus = nil
		lastFailureAt = now
	}
	attemptID := current.AttemptID
	order, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',attempt_id='',last_attempt_id=$1,client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,`+clearExecutionPinsSQL+`,last_attempt_outcome=$2,last_failure_category=$3,last_failure_message=$4,last_failure_detail=$5,last_failure_exit_status=$6,last_failure_at=$7,automatic_retry_count=$8,next_retry_at=$9,retry_suppressed=$10,retry_suppression_reason=$11,queue_entered_at=$12,queue_deadline=$13,operator_direction='',checkpoint=$14,updated_at=$12 WHERE workspace_id=$15 AND id=$16 AND worker_id=$17 AND claimant_id=$18 AND session_id=$19 AND state='claimed' RETURNING `+workOrderColumns,
		attemptID, release.Outcome, lastFailureCategory, lastFailureMessage, lastFailureDetail, lastFailureExitStatus, nullableTimeValue(lastFailureAt), retryCount, nullableTimeValue(nextRetry), suppressed, suppressionReason, now, now.Add(queueTimeout), checkpointJSON(release.Checkpoint), workspace(ctx), workOrderID, claim.WorkerID, claim.ClaimantID, release.SessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	eventCtx := ctx
	kind := "work_order.released"
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		kind = "work_order.child_failed"
		if release.Outcome == core.WorkOrderOutcomeStalled {
			kind = "work_order.stalled"
		}
	}
	payload := map[string]any{"attempt_id": attemptID, "session_id": release.SessionID, "reason": release.Reason, "release_cause": release.Cause, "detail": order.LastFailureDetail, "outcome": release.Outcome, "failure_category": order.LastFailureCategory, "consecutive_transient_failures": consecutiveTransientFailures, "exit_status": release.ExitStatus, "automatic_retry_count": order.AutomaticRetryCount, "next_retry_at": order.NextRetryAt, "retry_suppressed": order.RetrySuppressed, "suppression_reason": order.RetrySuppressionReason}
	if release.Checkpoint != nil {
		payload["checkpoint"] = release.Checkpoint
	}
	if err = insertEvent(eventCtx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(payload), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	order.Claimable = order.ClaimableAt(now)
	return order, nil
}

func (s *Store) RequestPlanRevisionCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, rationale string) (store.PlanRevisionRequestResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RequestPlanRevisionCommand")
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("rationale is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var taskID string
	if err = tx.QueryRow(ctx, `SELECT task_id FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace(ctx), workOrderID).Scan(&taskID); errors.Is(err, pgx.ErrNoRows) {
		return store.PlanRevisionRequestResult{}, store.ErrWorkOrderClaimLost
	} else if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if !taskLease.ValidForCommand(taskID, string(core.WorkOrderCmdRequestPlanRevision)) {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("plan revision request requires a valid taskops lease")
	}
	key := "conveyor:task-operation:" + workspace(ctx) + ":" + taskID
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	now := time.Now().UTC()
	current, err := scanWorkOrder(tx.QueryRow(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), workOrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PlanRevisionRequestResult{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if current.SessionID == "" || current.SessionID != claim.SessionID || current.State != core.WorkOrderClaimed ||
		!current.LeaseExpiresAt.After(now) || (!current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now)) {
		return store.PlanRevisionRequestResult{}, store.ErrWorkOrderClaimLost
	}
	if current.WorkerID != claim.WorkerID || current.ClaimantID != claim.ClaimantID {
		return store.PlanRevisionRequestResult{}, store.ErrWorkOrderClaimUnauthorized
	}
	if current.Stage != core.StageImplement {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("request_plan_revision requires an implement-stage work order")
	}
	q := s.queries.WithTx(tx)
	taskRow, err := q.GetTask(ctx, db.GetTaskParams{ID: current.TaskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return store.PlanRevisionRequestResult{}, notFound(err, "task %s", current.TaskID)
	}
	planTaskID, planVersion := current.TaskID, 0
	err = tx.QueryRow(ctx, `SELECT s.version FROM task_specs s JOIN tasks t ON t.id=s.task_id WHERE s.task_id=$1 AND t.workspace_id=$2 AND s.approved ORDER BY s.version DESC LIMIT 1`, planTaskID, workspace(ctx)).Scan(&planVersion)
	if errors.Is(err, pgx.ErrNoRows) && taskRow.ParentTaskID.Valid && taskRow.OriginSpecVersion > 0 {
		planTaskID = taskRow.ParentTaskID.String
		err = tx.QueryRow(ctx, `SELECT s.version FROM task_specs s JOIN tasks t ON t.id=s.task_id WHERE s.task_id=$1 AND s.version=$2 AND t.workspace_id=$3 AND s.approved`, planTaskID, taskRow.OriginSpecVersion, workspace(ctx)).Scan(&planVersion)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("request_plan_revision requires an approved execution plan")
	}
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	nextOrder, err := core.TransitionWorkOrder(current.State, core.WorkOrderCmdRequestPlanRevision)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	nextTask, err := core.TransitionTask(core.TaskState(taskRow.State), core.TaskGatePlanRevision)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	queueTimeout := current.QueueDeadline.Sub(current.QueueEnteredAt)
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	attemptID := current.AttemptID
	order, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state=$1,claimant_id='',session_id='',attempt_id='',last_attempt_id=$2,client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,`+clearExecutionPinsSQL+`,last_attempt_outcome=$3,last_failure_category='',last_failure_message=$4,last_failure_detail='',last_failure_exit_status=NULL,last_failure_at=$5,next_retry_at=NULL,retry_suppressed=true,retry_suppression_reason='',queue_entered_at=$5,queue_deadline=$6,operator_direction='',updated_at=$5 WHERE workspace_id=$7 AND id=$8 AND worker_id=$9 AND claimant_id=$10 AND session_id=$11 AND state='claimed' RETURNING `+workOrderColumns,
		nextOrder, attemptID, core.WorkOrderOutcomeReleased, core.WorkOrderReleaseReasonPlanRevisionRequested, now, now.Add(queueTimeout), workspace(ctx), workOrderID, claim.WorkerID, claim.ClaimantID, claim.SessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PlanRevisionRequestResult{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	updatedTask, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: current.TaskID, WorkspaceID: workspace(ctx), State: string(nextTask), GateKind: string(core.GateKindFor(nextTask, core.TaskGatePlanRevision))})
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	eventCtx := ctx
	if err = insertEvent(eventCtx, q, core.Event{TaskID: current.TaskID, JobID: current.JobID, Kind: "work_order.plan_revision_requested", Payload: core.JSONPayload(map[string]any{"work_order_id": current.ID, "attempt_id": attemptID, "session_id": claim.SessionID, "rationale": rationale, "plan_version": planVersion}), At: now}); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if err = insertEvent(eventCtx, q, core.Event{TaskID: current.TaskID, JobID: current.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]any{"attempt_id": attemptID, "session_id": claim.SessionID, "reason": core.WorkOrderReleaseReasonPlanRevisionRequested, "release_cause": core.WorkOrderReleaseCauseOperatorAction, "outcome": core.WorkOrderOutcomeReleased, "automatic_retry_count": order.AutomaticRetryCount, "retry_suppressed": true}), At: now}); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if err = insertEvent(eventCtx, q, core.Event{TaskID: current.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": taskRow.State, "to": nextTask, "command": core.TaskGatePlanRevision}), At: now}); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	order.Claimable = false
	return store.PlanRevisionRequestResult{WorkOrder: order, Task: taskFromDB(updatedTask), PlanVersion: planVersion, Rationale: rationale}, nil
}

func (s *Store) CancelPlanRevisionWorkOrderCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, attemptID string) (core.WorkOrder, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CancelPlanRevisionWorkOrderCommand")
	tx, err := s.begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var taskID string
	if err = tx.QueryRow(ctx, `SELECT task_id FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace(ctx), workOrderID).Scan(&taskID); errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", workOrderID)
	} else if err != nil {
		return core.WorkOrder{}, err
	}
	if !taskLease.ValidForCommand(taskID, string(core.WorkOrderCmdCancel)) {
		return core.WorkOrder{}, fmt.Errorf("plan-revision cancellation requires a valid taskops lease")
	}
	key := "conveyor:task-operation:" + workspace(ctx) + ":" + taskID
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key); err != nil {
		return core.WorkOrder{}, err
	}
	current, err := scanWorkOrder(tx.QueryRow(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), workOrderID))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", workOrderID)
	}
	if current.Stage != core.StageImplement || current.LastAttemptID != attemptID || current.LastFailureMessage != core.WorkOrderReleaseReasonPlanRevisionRequested {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not the contested plan-revision attempt", workOrderID)
	}
	if current.State == core.WorkOrderCancelled {
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		current.Claimable = false
		return current, nil
	}
	next, err := core.TransitionWorkOrder(current.State, core.WorkOrderCmdCancel)
	if err != nil {
		return core.WorkOrder{}, err
	}
	now := time.Now().UTC()
	order, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND state=$5 RETURNING `+workOrderColumns,
		next, now, workspace(ctx), workOrderID, current.State))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, fmt.Errorf("work order %s changed during plan-revision cancellation", workOrderID)
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed',ended_at=$1,updated_at=$1 WHERE id=$2 AND state<>'done'`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	actor := store.ActorFromContext(ctx)
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID, "attempt_id": attemptID, "prior_state": current.State, "state": next,
		"command": core.WorkOrderCmdCancel, "reason": "plan revision approved",
	}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	order.Claimable = false
	return order, nil
}

func postgresRetryDelay(release core.WorkOrderRelease, retry int) time.Duration {
	initial := release.InitialRetryDelay
	if initial <= 0 {
		initial = time.Second
	}
	maximum := release.MaximumRetryDelay
	if maximum < initial {
		maximum = initial
	}
	delay := initial
	for attempt := 1; attempt < retry && delay < maximum; attempt++ {
		delay *= 2
		if delay > maximum {
			delay = maximum
		}
	}
	return delay
}

// ListClaimBlockingProposalsForTask is the listing's read of the claim
// predicate. The locked claim calls the same helper inside its transaction
// (req-260810-70ce2f AC-1.1; component-work-orders).
func (s *Store) ListClaimBlockingProposalsForTask(ctx context.Context, taskID string) ([]store.ClaimBlockingProposal, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListClaimBlockingProposalsForTask")
	return claimBlockingProposalsTx(ctx, s.boundary, workspace(ctx), taskID)
}

// claimBlockingProposalsTx returns the task's undecided implementation-origin
// System Design and requirement versions in one workspace, archived documents
// included, in store.SortClaimBlockingProposals order.
func claimBlockingProposalsTx(ctx context.Context, q boundaryQuerier, workspaceID, taskID string) ([]store.ClaimBlockingProposal, error) {
	rows, err := q.Query(ctx, `
SELECT 'system_design'::text AS tier,document_id AS id,version FROM system_design_versions
WHERE workspace_id=$1 AND origin=$2 AND origin_task_id=$3 AND NOT confirmed AND NOT dismissed
UNION ALL
SELECT 'requirement'::text,requirement_id,version FROM requirement_versions
WHERE workspace_id=$1 AND origin=$4 AND origin_task_id=$3 AND NOT confirmed AND NOT retired`, workspaceID, string(core.SystemDesignOriginImplementation), taskID, string(core.RequirementOriginImplementation))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.ClaimBlockingProposal{}
	for rows.Next() {
		var item store.ClaimBlockingProposal
		if err := rows.Scan(&item.Tier, &item.ID, &item.Version); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Go orders the result so every backend names the same first proposal,
	// independent of database collation.
	store.SortClaimBlockingProposals(out)
	return out, nil
}
