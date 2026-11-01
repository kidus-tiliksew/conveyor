package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func nullableTimeValue(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}

func claimMatches(o core.WorkOrder, c core.WorkOrderClaimIdentity) bool {
	return o.WorkerID == c.WorkerID && o.ClaimantID == c.ClaimantID && o.SessionID == c.SessionID
}
func claimAlive(o core.WorkOrder, c core.WorkOrderClaimIdentity, now time.Time) bool {
	return o.State == core.WorkOrderClaimed && o.SessionID != "" && claimMatches(o, c) && o.LeaseExpiresAt.After(now) && (o.ExecutionDeadline.IsZero() || o.ExecutionDeadline.After(now))
}
func cancelledSessionMatches(ctx context.Context, tx *sql.Tx, ws, task, job, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	var match bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind='work_order.cancelled' AND JSON_EXTRACT_STRING(payload_json,'session_id')=?)`, ws, task, job, session).Scan(&match)
	return match, err
}
func (s *Store) RenewWorkerClaimCommand(ctx context.Context, lease taskops.TaskLease, id string, c core.WorkOrderClaimIdentity, duration time.Duration) (core.WorkOrder, error) {
	var result core.WorkOrder
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if !lease.ValidForCommand(o.TaskID, string(core.WorkOrderCmdRenew)) {
			return fmt.Errorf("work-order renewal requires a valid taskops lease")
		}
		now := time.Now().UTC()
		if claimAlive(o, c, now) {
			o.LeaseExpiresAt = now.Add(duration)
			if !o.ExecutionDeadline.IsZero() && o.LeaseExpiresAt.After(o.ExecutionDeadline) {
				o.LeaseExpiresAt = o.ExecutionDeadline
			}
			o.UpdatedAt = now
			if err := orderWrite(ctx, tx, o); err != nil {
				return err
			}
			var err error
			result, err = getOrderRow(ctx, tx, id)
			if err != nil {
				return err
			}
			return taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": o.AttemptID, "lease_expires_at": result.LeaseExpiresAt})})
		}
		if o.State == core.WorkOrderCancelled {
			match, err := cancelledSessionMatches(ctx, tx, documentWorkspace(ctx), o.TaskID, o.JobID, c.SessionID)
			if err != nil {
				return err
			}
			if (o.LastAttemptID != "" && o.LastAttemptID == c.SessionID) || match || claimMatches(o, c) {
				return store.ErrWorkOrderCancelled
			}
		}
		if claimMatches(o, c) && (o.State == core.WorkOrderSubmitted || o.State == core.WorkOrderCompleted) {
			result = o
			return nil
		}
		if o.State == core.WorkOrderQueued && o.WorkerID == "" && o.ClaimantID == "" && o.SessionID == "" && (o.LastFailureMessage == core.WorkOrderReleaseReasonOperatorCheckpointReached || o.LastFailureMessage == core.WorkOrderReleaseReasonPlanRevisionRequested) && o.LastAttemptID != "" {
			events, err := documentTaskEvents(ctx, tx, o.TaskID)
			if err != nil {
				return err
			}
			for _, e := range events {
				if e.Kind != "work_order.claimed" || e.JobID != o.JobID {
					continue
				}
				var previous core.WorkOrder
				if err = json.Unmarshal(e.Payload, &previous); err != nil {
					return err
				}
				if previous.ID == id && previous.AttemptID == o.LastAttemptID && previous.SessionID == c.SessionID && previous.ClaimantID == c.ClaimantID && ((c.WorkerID != "" && previous.WorkerID == c.WorkerID) || (c.WorkerID == "" && core.IsTaskRunClaimantID(c.ClaimantID) && previous.WorkerID == "")) {
					return store.ErrWorkOrderReleasedAtCheckpoint
				}
			}
		}
		var preempted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_order_preemptions WHERE workspace_id=? AND work_order_id=? AND revoked_worker_id=? AND revoked_session_id=?)`, documentWorkspace(ctx), id, c.WorkerID, c.SessionID).Scan(&preempted); err != nil {
			return err
		}
		if preempted {
			return store.ErrWorkOrderPreempted
		}
		return store.ErrWorkOrderClaimLost
	})
	return result, err
}
func (s *Store) ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, id string, claim core.WorkOrderClaimIdentity, release core.WorkOrderRelease) (core.WorkOrder, error) {
	var result core.WorkOrder
	var lifecycleErr error
	err := s.orderTx(ctx, id, func(tx *sql.Tx, current core.WorkOrder) error {
		if !taskLease.ValidForCommand(current.TaskID, string(core.WorkOrderCmdRelease)) {
			return fmt.Errorf("work-order release requires a valid taskops lease")
		}
		if current.Stage == core.StageReview {
			accepted, err := reviewSeatAcceptedTx(ctx, tx, documentWorkspace(ctx), current.TaskID, id)
			if err != nil {
				return err
			}
			if accepted {
				return fmt.Errorf("accepted review seat %s is terminal", id)
			}
		}
		if current.State == core.WorkOrderCancelled {
			matches, err := cancelledSessionMatches(ctx, tx, documentWorkspace(ctx), current.TaskID, current.JobID, release.SessionID)
			if err != nil {
				return err
			}
			if (current.LastAttemptID != "" && current.LastAttemptID == release.SessionID) || matches || (current.WorkerID == claim.WorkerID && current.SessionID == release.SessionID) {
				return store.ErrWorkOrderCancelled
			}
		}
		if current.SessionID == "" || current.SessionID != release.SessionID || current.State != core.WorkOrderClaimed {
			return store.ErrWorkOrderClaimLost
		}
		if _, err := core.TransitionWorkOrder(current.State, core.WorkOrderCmdRelease); err != nil {
			return err
		}
		now := time.Now().UTC()
		var err error
		if !current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, current, core.WorkOrderCmdTimeout, "work_order.timed_out", now); err != nil {
				return err
			}
			lifecycleErr = store.ErrWorkOrderClaimLost
			return nil
		}
		if !current.LeaseExpiresAt.After(now) {
			if _, err = s.expireWorkOrderClaimTx(ctx, tx, current, now); err != nil {
				return err
			}
			lifecycleErr = store.ErrWorkOrderClaimLost
			return nil
		}
		if current.WorkerID != claim.WorkerID || current.ClaimantID != claim.ClaimantID {
			return store.ErrWorkOrderClaimUnauthorized
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
		if err = documentRow(ctx, tx, `SELECT COALESCE((SELECT kind='work_order.progress_reported' FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind IN ('work_order.claimed','work_order.progress_reported') ORDER BY at DESC,id DESC LIMIT 1), false)`, documentWorkspace(ctx), current.TaskID, current.JobID).Scan(&progressed); err != nil {
			return err
		}
		previousTransientFailures := 0
		if current.LastFailureCategory == core.WorkOrderFailureTransientConnectivity {
			if err = documentRow(ctx, tx, `SELECT COALESCE(JSON_EXTRACT_BIGINT(payload_json,'consecutive_transient_failures'),0) FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind IN ('work_order.child_failed','work_order.stalled') ORDER BY at DESC,id DESC LIMIT 1`, documentWorkspace(ctx), current.TaskID, current.JobID).Scan(&previousTransientFailures); errors.Is(err, sql.ErrNoRows) {
				previousTransientFailures = 0
			} else if err != nil {
				return err
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
					delay := workerRetryDelay(release, retryCount)
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
		order := current
		clearOrderClaim(&order)
		order.ClearExecutionPins()
		order.State = core.WorkOrderQueued
		order.LastAttemptID = attemptID
		order.ExecutionStartedAt = time.Time{}
		order.ExecutionDeadline = time.Time{}
		order.LastAttemptOutcome = release.Outcome
		order.LastFailureCategory = lastFailureCategory
		order.LastFailureMessage = lastFailureMessage
		order.LastFailureDetail = lastFailureDetail
		order.LastFailureExitStatus = lastFailureExitStatus
		order.LastFailureAt = lastFailureAt
		order.AutomaticRetryCount = retryCount
		order.NextRetryAt = nextRetry
		order.RetrySuppressed = suppressed
		order.RetrySuppressionReason = suppressionReason
		order.QueueEnteredAt = now
		order.QueueDeadline = now.Add(queueTimeout)
		order.OperatorDirection = ""
		order.Checkpoint = release.Checkpoint
		order.UpdatedAt = now
		if err = orderWrite(ctx, tx, order); err != nil {
			return err
		}
		if err = resetOrderJob(ctx, tx, order, now); err != nil {
			return err
		}
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
		if err = taskEvent(ctx, tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, At: now, Payload: core.JSONPayload(payload)}); err != nil {
			return err
		}
		result, err = getOrderRow(ctx, tx, id)
		return err
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return result, lifecycleErr
}
func workerRetryDelay(release core.WorkOrderRelease, retry int) time.Duration {
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

// workspaceRowExists reports a missing workspace row as not found.
func workspaceRowExists(ctx context.Context, tx *sql.Tx, ws string) error {
	var exists int
	return notFound(tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id=?`, ws).Scan(&exists), "workspace %s", ws)
}
