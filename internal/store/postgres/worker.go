package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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

func (s *Store) CreateWorkerPairing(ctx context.Context, pairing core.WorkerPairing) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO worker_pairings (token_hash,workspace_id,expires_at,created_at) VALUES ($1,$2,$3,$4)`, pairing.TokenHash, workspace(ctx), pairing.ExpiresAt, pairing.CreatedAt); err != nil {
			return err
		}
		actor := store.ActorFromContext(ctx)
		_, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: workspace(ctx), Kind: "worker.pairing_issued", ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]any{"expires_at": pairing.ExpiresAt}), At: timestamp(time.Now().UTC())})
		return err
	})
}

func (s *Store) ConsumeWorkerPairing(ctx context.Context, tokenHash string, now time.Time) (core.WorkerPairing, error) {
	var pairing core.WorkerPairing
	var consumed *time.Time
	err := s.pool.QueryRow(ctx, `UPDATE worker_pairings SET consumed_at=$2 WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>$2 RETURNING token_hash,workspace_id,expires_at,consumed_at,created_at`, tokenHash, now).Scan(&pairing.TokenHash, &pairing.Workspace, &pairing.ExpiresAt, &consumed, &pairing.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkerPairing{}, store.ErrPairingInvalid
	}
	if err != nil {
		return core.WorkerPairing{}, err
	}
	if consumed != nil {
		pairing.ConsumedAt = *consumed
	}
	return pairing, nil
}

func (s *Store) CreateWorker(ctx context.Context, worker core.Worker) error {
	probes, _ := json.Marshal(worker.Probes)
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workers (id,workspace_id,name,credential_hash,lease_expires_at,last_seen_at,probe_results,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, worker.ID, workspace(ctx), worker.Name, worker.CredentialHash, nullableTimeValue(worker.LeaseExpiresAt), nullableTimeValue(worker.LastSeenAt), probes, worker.CreatedAt); err != nil {
			return err
		}
		_, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: workspace(ctx), Kind: "worker.enrolled", ActorID: worker.ID, ActorRole: string(core.ActorRunner), PayloadJson: core.JSONPayload(map[string]string{"worker_id": worker.ID, "name": worker.Name}), At: timestamp(time.Now().UTC())})
		return err
	})
}

func (s *Store) ListWorkers(ctx context.Context) ([]core.Worker, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,name,credential_hash,lease_expires_at,last_seen_at,revoked_at,probe_results,created_at FROM workers WHERE workspace_id=$1 ORDER BY created_at,id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Worker
	for rows.Next() {
		worker, scanErr := scanWorker(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, worker)
	}
	return result, rows.Err()
}

func (s *Store) ListHarnessModelFailures(ctx context.Context) ([]core.HarnessModelFailure, error) {
	rows, err := s.pool.Query(ctx, `SELECT harness,model,detail,work_order_id,observed_at FROM harness_model_failures WHERE workspace_id=$1 ORDER BY harness,model`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.HarnessModelFailure
	for rows.Next() {
		var failure core.HarnessModelFailure
		if err = rows.Scan(&failure.Harness, &failure.Model, &failure.Detail, &failure.WorkOrderID, &failure.ObservedAt); err != nil {
			return nil, err
		}
		result = append(result, failure)
	}
	return result, rows.Err()
}

func (s *Store) AuthenticateWorker(ctx context.Context, credentialHash string) (core.Worker, error) {
	workspaceID := workspace(ctx)
	query := `SELECT id,workspace_id,name,credential_hash,lease_expires_at,last_seen_at,revoked_at,probe_results,created_at FROM workers WHERE credential_hash=$1 AND revoked_at IS NULL`
	args := []any{credentialHash}
	if workspaceID != "" {
		query += ` AND workspace_id=$2`
		args = append(args, workspaceID)
	}
	worker, err := scanWorker(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Worker{}, store.ErrWorkerUnauthorized
	}
	return worker, err
}

func (s *Store) HeartbeatWorker(ctx context.Context, id string, leaseExpires time.Time, probes []core.HarnessProbe) (core.Worker, error) {
	data, _ := json.Marshal(probes)
	now := time.Now().UTC()
	worker, err := scanWorker(s.pool.QueryRow(ctx, `UPDATE workers SET lease_expires_at=$1,last_seen_at=$2,probe_results=$3 WHERE workspace_id=$4 AND id=$5 AND revoked_at IS NULL RETURNING id,workspace_id,name,credential_hash,lease_expires_at,last_seen_at,revoked_at,probe_results,created_at`, leaseExpires, now, data, workspace(ctx), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Worker{}, store.ErrWorkerUnauthorized
	}
	if err != nil {
		return core.Worker{}, err
	}
	actor := store.ActorFromContext(ctx)
	_, _ = s.queries.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: workspace(ctx), Kind: "worker.heartbeat", ActorID: actor.ID, ActorRole: string(core.ActorRunner), PayloadJson: core.JSONPayload(map[string]any{"worker_id": id, "lease_expires_at": leaseExpires, "probes": probes}), At: timestamp(now)})
	return worker, nil
}

func (s *Store) RevokeWorker(ctx context.Context, id string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `UPDATE workers SET revoked_at=COALESCE(revoked_at,$1),lease_expires_at=NULL WHERE workspace_id=$2 AND id=$3`, now, workspace(ctx), id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("worker %s not found", id)
	}
	actor := store.ActorFromContext(ctx)
	_, err = s.queries.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: workspace(ctx), Kind: "worker.revoked", ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]string{"worker_id": id}), At: timestamp(now)})
	return err
}

func (s *Store) RenewWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, workerID, sessionID string, lease time.Duration) (core.WorkOrder, error) {
	current, err := s.GetWorkOrder(ctx, workOrderID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if !taskLease.ValidForCommand(current.TaskID, string(core.WorkOrderCmdRenew)) {
		return core.WorkOrder{}, fmt.Errorf("work-order renewal requires a valid taskops lease")
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	row := s.pool.QueryRow(ctx, `UPDATE work_orders SET lease_expires_at=CASE WHEN execution_deadline IS NULL THEN $1 ELSE LEAST($1,execution_deadline) END,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND worker_id=$5 AND session_id=$6 AND state='claimed' AND lease_expires_at>$2 AND (execution_deadline IS NULL OR execution_deadline>$2) RETURNING `+workOrderColumns, expires, now, workspace(ctx), workOrderID, workerID, sessionID)
	order, err := scanWorkOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := s.GetWorkOrder(ctx, workOrderID)
		if getErr == nil && current.State == core.WorkOrderCancelled {
			matches, matchErr := cancelledSessionMatches(ctx, s.pool, workspace(ctx), current.TaskID, current.JobID, sessionID)
			if matchErr != nil {
				return core.WorkOrder{}, matchErr
			}
			if (current.LastAttemptID != "" && current.LastAttemptID == sessionID) || matches ||
				(current.WorkerID == workerID && current.SessionID == sessionID) {
				return core.WorkOrder{}, store.ErrWorkOrderCancelled
			}
		}
		if getErr == nil && current.WorkerID == workerID && current.SessionID == sessionID && (current.State == core.WorkOrderSubmitted || current.State == core.WorkOrderCompleted) {
			return current, nil
		}
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	_ = s.AppendEvent(store.WithActor(ctx, store.Actor{ID: workerID, Role: core.ActorRunner}), core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": order.AttemptID, "lease_expires_at": order.LeaseExpiresAt})})
	return order, nil
}

func (s *Store) ReleaseWorkerClaimCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	tx, err := s.pool.Begin(ctx)
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
			(current.WorkerID == workerID && current.SessionID == release.SessionID) {
			return core.WorkOrder{}, store.ErrWorkOrderCancelled
		}
	}
	if current.WorkerID != workerID || current.SessionID == "" || current.SessionID != release.SessionID || current.State != core.WorkOrderClaimed {
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
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		detail := strings.TrimSpace(release.FailureDetail)
		identical := detail != "" && current.LastAttemptOutcome == release.Outcome && detail == current.LastFailureDetail
		lastFailureMessage = strings.TrimSpace(release.Reason)
		lastFailureCategory = strings.TrimSpace(release.FailureCategory)
		lastFailureDetail = detail
		lastFailureExitStatus = release.ExitStatus
		lastFailureAt = now
		limit := release.AutomaticRetryLimit
		if limit <= 0 {
			limit = 3
		}
		if retryCount < limit {
			retryCount++
			if identical {
				suppressionReason = core.IdenticalFailureSuppressionReason
			} else {
				nextRetry = now.Add(postgresRetryDelay(release, retryCount))
				suppressed = false
			}
		}
	} else {
		lastFailureCategory = ""
		lastFailureMessage = strings.TrimSpace(release.Reason)
		lastFailureDetail = strings.TrimSpace(release.FailureDetail)
		lastFailureExitStatus = nil
		lastFailureAt = now
	}
	attemptID := current.AttemptID
	order, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',attempt_id='',last_attempt_id=$1,client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome=$2,last_failure_category=$3,last_failure_message=$4,last_failure_detail=$5,last_failure_exit_status=$6,last_failure_at=$7,automatic_retry_count=$8,next_retry_at=$9,retry_suppressed=$10,retry_suppression_reason=$11,queue_entered_at=$12,queue_deadline=$13,updated_at=$12 WHERE workspace_id=$14 AND id=$15 AND worker_id=$16 AND session_id=$17 AND state='claimed' RETURNING `+workOrderColumns,
		attemptID, release.Outcome, lastFailureCategory, lastFailureMessage, lastFailureDetail, lastFailureExitStatus, nullableTimeValue(lastFailureAt), retryCount, nullableTimeValue(nextRetry), suppressed, suppressionReason, now, now.Add(queueTimeout), workspace(ctx), workOrderID, workerID, release.SessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	if release.ModelRejection && order.RequiredHarness != "" && order.RequiredModel != "" && order.LastFailureDetail != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO harness_model_failures (workspace_id,harness,model,detail,work_order_id,observed_at) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (workspace_id,harness,model) DO UPDATE SET detail=EXCLUDED.detail,work_order_id=EXCLUDED.work_order_id,observed_at=EXCLUDED.observed_at`, workspace(ctx), order.RequiredHarness, order.RequiredModel, order.LastFailureDetail, order.ID, now); err != nil {
			return core.WorkOrder{}, err
		}
	}
	q := s.queries.WithTx(tx)
	eventCtx := store.WithActor(ctx, store.Actor{ID: workerID, Role: core.ActorRunner})
	kind := "work_order.released"
	if core.WorkOrderOutcomeConsumesRetry(release.Outcome) {
		kind = "work_order.child_failed"
		if release.Outcome == core.WorkOrderOutcomeStalled {
			kind = "work_order.stalled"
		}
	}
	if err = insertEvent(eventCtx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(map[string]any{"attempt_id": attemptID, "session_id": release.SessionID, "reason": release.Reason, "detail": order.LastFailureDetail, "outcome": release.Outcome, "failure_category": order.LastFailureCategory, "exit_status": release.ExitStatus, "automatic_retry_count": order.AutomaticRetryCount, "next_retry_at": order.NextRetryAt, "retry_suppressed": order.RetrySuppressed, "suppression_reason": order.RetrySuppressionReason}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	order.Claimable = order.ClaimableAt(now)
	return order, nil
}

func (s *Store) RecordWorkOrderAttemptCheckpoint(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	order, err := scanWorkOrder(tx.QueryRow(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), workOrderID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !order.AuthorizesAttemptCheckpoint(workerID, checkpoint, time.Now().UTC())) {
		return false, store.ErrWorkOrderClaimLost
	}
	if err != nil {
		return false, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='work_order.attempt_checkpointed'
		AND payload_json->>'work_order_id'=$3 AND payload_json->>'attempt_id'=$4 AND payload_json->>'commit_sha'=$5
	)`, workspace(ctx), order.TaskID, order.ID, checkpoint.AttemptID, checkpoint.CommitSHA).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, tx.Commit(ctx)
	}
	q := s.queries.WithTx(tx)
	eventCtx := store.WithActor(ctx, store.Actor{ID: workerID, Role: core.ActorRunner})
	if err = insertEvent(eventCtx, q, core.Event{
		TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.attempt_checkpointed",
		Payload: store.AttemptCheckpointPayload(order, checkpoint), At: time.Now().UTC(),
	}); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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

func scanWorker(row interface{ Scan(...any) error }) (core.Worker, error) {
	var worker core.Worker
	var lease, seen, revoked *time.Time
	var probes []byte
	err := row.Scan(&worker.ID, &worker.Workspace, &worker.Name, &worker.CredentialHash, &lease, &seen, &revoked, &probes, &worker.CreatedAt)
	if lease != nil {
		worker.LeaseExpiresAt = *lease
	}
	if seen != nil {
		worker.LastSeenAt = *seen
	}
	if revoked != nil {
		worker.RevokedAt = *revoked
	}
	if len(probes) != 0 {
		_ = json.Unmarshal(probes, &worker.Probes)
	}
	return worker, err
}
