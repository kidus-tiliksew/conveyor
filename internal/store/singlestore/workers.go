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

// Worker persistence follows the PostgreSQL reference (DEC-38,
// component-persistence). Pairings are consumed once under a row lock.
const workerColumns = `id,workspace_id,COALESCE(owner_user_id,''),name,credential_hash,lease_expires_at,last_seen_at,revoked_at,probe_results,created_at`
const activeWorkerOwner = `(owner_user_id IS NULL OR EXISTS (SELECT 1 FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id AND b.workspace_id=workers.workspace_id WHERE u.id=workers.owner_user_id AND u.status='active'))`

func (s *Store) CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := workerWorkspaceExists(ctx, tx, ws); err != nil {
			return err
		}
		if pairing.OwnerUserID != "" {
			if err := deploymentUserExists(ctx, tx, pairing.OwnerUserID); err != nil {
				return err
			}
		}
		if err := lockKey(ctx, tx, "worker-pairing:"+pairing.TokenHash); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_pairings WHERE token_hash=?`, pairing.TokenHash).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("worker pairing already exists")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_pairings (token_hash,workspace_id,owner_user_id,expires_at,created_at) VALUES (?,?,?,?,?)`, pairing.TokenHash, ws, nullString(pairing.OwnerUserID), pairing.ExpiresAt, pairing.CreatedAt); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "worker.pairing_issued", Payload: core.JSONPayload(map[string]any{"expires_at": pairing.ExpiresAt})})
	})
}
func deploymentUserExists(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	return notFound(tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=?`, id).Scan(&exists), "user %s", id)
}
func (s *Store) ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error) {
	var p core.WorkerPairing
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "worker-pairing:"+tokenHash); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT token_hash,workspace_id,COALESCE(owner_user_id,''),expires_at,created_at FROM worker_pairings WHERE token_hash=? AND consumed_at IS NULL AND expires_at>? FOR UPDATE`, tokenHash, now).Scan(&p.TokenHash, &p.Workspace, &p.OwnerUserID, &p.ExpiresAt, &p.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrPairingInvalid
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE worker_pairings SET consumed_at=? WHERE workspace_id=? AND token_hash=?`, now, p.Workspace, tokenHash)
		p.ConsumedAt = now.UTC().Truncate(time.Microsecond)
		return err
	})
	if err != nil {
		return core.WorkerPairing{}, err
	}
	return p, nil
}
func (s *Store) CreateWorker(ctx context.Context, worker core.Worker) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	probes, err := json.Marshal(worker.Probes)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := workerWorkspaceExists(ctx, tx, ws); err != nil {
			return err
		}
		if worker.OwnerUserID != "" {
			if err := deploymentUserExists(ctx, tx, worker.OwnerUserID); err != nil {
				return err
			}
		}
		// The PostgreSQL credential and worker identifiers are deployment-unique.
		// SingleStore's sharding key cannot enforce those two constraints.
		for _, key := range []string{"worker-id:" + worker.ID, "worker-credential:" + worker.CredentialHash} {
			if err := lockKey(ctx, tx, key); err != nil {
				return err
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE id=? OR credential_hash=?`, worker.ID, worker.CredentialHash).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("worker identifier or credential already exists")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workers (id,workspace_id,owner_user_id,name,credential_hash,lease_expires_at,last_seen_at,probe_results,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, worker.ID, ws, nullString(worker.OwnerUserID), worker.Name, worker.CredentialHash, nullableTimeValue(worker.LeaseExpiresAt), nullableTimeValue(worker.LastSeenAt), probes, worker.CreatedAt); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "worker.enrolled", ActorID: store.WorkerActorID(worker.ID), ActorRole: core.ActorWorker, Payload: core.JSONPayload(map[string]string{"worker_id": worker.ID, "name": worker.Name})})
	})
}
func (s *Store) ListWorkers(ctx context.Context) ([]core.Worker, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE workspace_id=? ORDER BY created_at,id`, ws)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []core.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}
func (s *Store) ListHarnessModelFailures(ctx context.Context) ([]core.HarnessModelFailure, error) {
	return nil, nil
}
func (s *Store) AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error) {
	ws, _ := store.WorkspaceFromContext(ctx)
	query := `SELECT ` + workerColumns + ` FROM workers WHERE credential_hash=? AND revoked_at IS NULL AND ` + activeWorkerOwner
	args := []any{credentialHash}
	if ws != "" {
		query += ` AND workspace_id=?`
		args = append(args, ws)
	}
	w, err := scanWorker(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Worker{}, store.ErrWorkerUnauthorized
	}
	return w, translateBackendConflict(err)
}
func (s *Store) HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return core.Worker{}, err
	}
	data, err := json.Marshal(probes)
	if err != nil {
		return core.Worker{}, err
	}
	var w core.Worker
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		w, err = scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE workspace_id=? AND id=? AND revoked_at IS NULL FOR UPDATE`, ws, id))
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrWorkerUnauthorized
		}
		if err != nil {
			return err
		}
		if w.OwnerUserID != "" {
			var active bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id WHERE u.id=? AND u.status='active' AND b.workspace_id=?)`, w.OwnerUserID, ws).Scan(&active); err != nil {
				return err
			}
			if !active {
				return store.ErrWorkerUnauthorized
			}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err = tx.ExecContext(ctx, `UPDATE workers SET lease_expires_at=?,last_seen_at=?,probe_results=? WHERE workspace_id=? AND id=?`, leaseExpires, now, data, ws, id); err != nil {
			return err
		}
		w.LeaseExpiresAt = leaseExpires.UTC().Truncate(time.Microsecond)
		w.LastSeenAt = now
		w.Probes = probes
		return nil
	})
	if err != nil {
		return core.Worker{}, err
	}
	return w, nil
}
func (s *Store) RevokeWorker(ctx context.Context, id string) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workers WHERE workspace_id=? AND id=? FOR UPDATE`, ws, id).Scan(&exists); err != nil {
			return notFound(err, "worker %s", id)
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET revoked_at=COALESCE(revoked_at,?),lease_expires_at=NULL WHERE workspace_id=? AND id=?`, now, ws, id); err != nil {
			return err
		}
		actor := store.ActorFromContext(ctx)
		if actor.ID == "" {
			actor = store.Actor{ID: "system", Role: core.ActorSystem}
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "worker.revoked", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]string{"worker_id": id}), At: now})
	})
}
func scanWorker(row interface{ Scan(...any) error }) (core.Worker, error) {
	var worker core.Worker
	var lease, seen, revoked *time.Time
	var probes []byte
	err := row.Scan(&worker.ID, &worker.Workspace, &worker.OwnerUserID, &worker.Name, &worker.CredentialHash, &lease, &seen, &revoked, &probes, &worker.CreatedAt)
	if err != nil {
		return core.Worker{}, err
	}
	if lease != nil {
		worker.LeaseExpiresAt = *lease
	}
	if seen != nil {
		worker.LastSeenAt = *seen
	}
	if revoked != nil {
		worker.RevokedAt = *revoked
	}
	if len(probes) > 0 {
		if err = json.Unmarshal(probes, &worker.Probes); err != nil {
			return core.Worker{}, err
		}
	}
	return worker, nil
}
func nullableTimeValue(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}

func workerWorkspaceExists(ctx context.Context, tx *sql.Tx, ws string) error {
	var exists int
	return notFound(tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id=?`, ws).Scan(&exists), "workspace %s", ws)
}

func workerClaimActorContext(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(id), Role: core.ActorWorker})
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
			return taskEvent(workerClaimActorContext(ctx, c.WorkerID), tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": o.AttemptID, "lease_expires_at": result.LeaseExpiresAt})})
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
func (s *Store) RecordWorkOrderContinuation(ctx context.Context, id string, c core.WorkOrderClaimIdentity, v core.WorkOrderContinuation) (core.WorkOrder, error) {
	var result core.WorkOrder
	err := s.orderTx(ctx, id, func(tx *sql.Tx, o core.WorkOrder) error {
		if o.Stage != core.StageImplement || !claimAlive(o, c, time.Now().UTC()) {
			return store.ErrWorkOrderClaimLost
		}
		if v.AttemptID != o.AttemptID {
			return fmt.Errorf("continuation attempt does not match the active work-order attempt")
		}
		h := o.Agent
		if h == "" {
			h = o.RequiredHarness
		}
		if h != "" && v.Harness != h {
			return fmt.Errorf("continuation harness %q does not match active harness %q", v.Harness, h)
		}
		o.ContinuationSessionID = v.SessionID
		o.ContinuationAttemptID = v.AttemptID
		o.ContinuationHarness = v.Harness
		o.ContinuationLaunchEnvironment = v.LaunchEnvironment
		o.UpdatedAt = time.Now().UTC()
		if err := orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.continuation_reported", At: o.UpdatedAt, Payload: core.JSONPayload(map[string]any{"work_order_id": o.ID, "attempt_id": v.AttemptID, "harness": v.Harness, "launch_environment": v.LaunchEnvironment})}); err != nil {
			return err
		}
		var err error
		result, err = getOrderRow(ctx, tx, id)
		return err
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
		if err = documentRow(ctx, tx, `SELECT COALESCE((SELECT kind='work_order.progress_reported' FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind IN ('work_order.claimed','work_order.progress_reported') ORDER BY id DESC LIMIT 1), false)`, documentWorkspace(ctx), current.TaskID, current.JobID).Scan(&progressed); err != nil {
			return err
		}
		previousTransientFailures := 0
		if current.LastFailureCategory == core.WorkOrderFailureTransientConnectivity {
			if err = documentRow(ctx, tx, `SELECT COALESCE(JSON_EXTRACT_BIGINT(payload_json,'consecutive_transient_failures'),0) FROM events WHERE workspace_id=? AND task_id=? AND job_id=? AND kind IN ('work_order.child_failed','work_order.stalled') ORDER BY id DESC LIMIT 1`, documentWorkspace(ctx), current.TaskID, current.JobID).Scan(&previousTransientFailures); errors.Is(err, sql.ErrNoRows) {
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
		if err = taskEvent(workerClaimActorContext(ctx, claim.WorkerID), tx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, At: now, Payload: core.JSONPayload(payload)}); err != nil {
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
