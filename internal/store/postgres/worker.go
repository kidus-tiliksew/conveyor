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
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

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

func (s *Store) RenewWorkerClaim(ctx context.Context, workOrderID, workerID string, lease time.Duration) (core.WorkOrder, error) {
	now := time.Now().UTC()
	expires := now.Add(lease)
	row := s.pool.QueryRow(ctx, `UPDATE work_orders SET lease_expires_at=CASE WHEN execution_deadline IS NULL THEN $1 ELSE LEAST($1,execution_deadline) END,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND worker_id=$5 AND state='claimed' AND (execution_deadline IS NULL OR execution_deadline>$2) RETURNING `+workOrderColumns, expires, now, workspace(ctx), workOrderID, workerID)
	order, err := scanWorkOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := s.GetWorkOrder(ctx, workOrderID)
		if getErr == nil && current.WorkerID == workerID && (current.State == core.WorkOrderSubmitted || current.State == core.WorkOrderCompleted) {
			return current, nil
		}
		return core.WorkOrder{}, store.ErrWorkerUnauthorized
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	_ = s.AppendEvent(store.WithActor(ctx, store.Actor{ID: workerID, Role: core.ActorRunner}), core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"lease_expires_at": order.LeaseExpiresAt})})
	return order, nil
}

func (s *Store) ReleaseWorkerClaim(ctx context.Context, workOrderID, workerID, reason string) (core.WorkOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now := time.Now().UTC()
	order, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',updated_at=$1 WHERE workspace_id=$2 AND id=$3 AND worker_id=$4 AND state='claimed' RETURNING `+workOrderColumns, now, workspace(ctx), workOrderID, workerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, store.ErrWorkerUnauthorized
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	eventCtx := store.WithActor(ctx, store.Actor{ID: workerID, Role: core.ActorRunner})
	if err = insertEvent(eventCtx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]string{"reason": reason})}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	order.Claimable = true
	return order, nil
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
