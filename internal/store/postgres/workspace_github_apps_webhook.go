package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// The workspace GitHub webhook secret and accepted deliveries
// (component-identity-membership, The webhook secret; req-quiet-idle-runtime
// AC-3.2, AC-3.3, AC-3.6). The secret is sealed like the App private key under
// its own additional data and is returned only by
// GetWorkspaceGitHubWebhookSecretForUse.

// storeWebhookSecretTx seals and upserts secret, or removes the row when
// secret is empty, inside the caller's transaction.
func (s *Store) storeWebhookSecretTx(ctx context.Context, tx pgx.Tx, workspace, secret string, at time.Time) error {
	if secret == "" {
		_, err := tx.Exec(ctx, `DELETE FROM workspace_github_webhook_secrets WHERE workspace_id=$1`, workspace)
		return err
	}
	if err := store.ValidateGitHubWebhookSecret(workspace, secret); err != nil {
		return err
	}
	a, err := s.gitHubAppKeyAEAD()
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	ciphertext := a.Seal(nil, nonce, []byte(secret), store.GitHubWebhookSecretAAD(workspace))
	_, err = tx.Exec(ctx, `INSERT INTO workspace_github_webhook_secrets (workspace_id,secret_nonce,secret_ciphertext,set_at) VALUES ($1,$2,$3,$4) ON CONFLICT (workspace_id) DO UPDATE SET secret_nonce=EXCLUDED.secret_nonce,secret_ciphertext=EXCLUDED.secret_ciphertext,set_at=EXCLUDED.set_at`, workspace, nonce, ciphertext, at)
	return err
}

func (s *Store) SetWorkspaceGitHubWebhookSecret(ctx context.Context, id, secret string) (time.Time, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SetWorkspaceGitHubWebhookSecret")
	if err := store.ValidateGitHubWebhookSecret(id, secret); err != nil {
		return time.Time{}, translateBackendConflict(err)
	}
	if _, err := s.gitHubAppKeyAEAD(); err != nil {
		return time.Time{}, translateBackendConflict(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var appID int64
		if err := tx.QueryRow(ctx, `SELECT app_id FROM workspace_github_apps WHERE workspace_id=$1`, id).Scan(&appID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		if err := s.storeWebhookSecretTx(ctx, tx, id, secret, at); err != nil {
			return err
		}
		actor := store.ActorFromContext(ctx)
		_, err := insertWorkspaceEventRow(ctx, q, db.InsertWorkspaceEventParams{WorkspaceID: id, Kind: store.GitHubWebhookSecretSetEvent, ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(store.GitHubWebhookSecretEvent(id, appID, at)), At: timestamp(at)})
		return err
	})
	if err != nil {
		return time.Time{}, translateBackendConflict(err)
	}
	redact.RegisterSecret(secret, time.Time{})
	return at, nil
}

func (s *Store) GetWorkspaceGitHubWebhookSecretForUse(ctx context.Context, id string) (string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetWorkspaceGitHubWebhookSecretForUse")
	var nonce, ciphertext []byte
	err := s.boundary.QueryRow(ctx, `SELECT secret_nonce,secret_ciphertext FROM workspace_github_webhook_secrets WHERE workspace_id=$1`, id).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", translateBackendConflict(err)
	}
	a, err := s.gitHubAppKeyAEAD()
	if err != nil {
		return "", translateBackendConflict(err)
	}
	if len(nonce) != a.NonceSize() {
		return "", store.ErrGitHubAppKeyDecrypt
	}
	plaintext, err := a.Open(nil, nonce, ciphertext, store.GitHubWebhookSecretAAD(id))
	if err != nil {
		return "", store.ErrGitHubAppKeyDecrypt
	}
	secret := string(plaintext)
	redact.RegisterSecret(secret, time.Time{})
	return secret, nil
}

func (s *Store) RecordGitHubWebhookDelivery(ctx context.Context, id, deliveryID, event string, receivedAt time.Time) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordGitHubWebhookDelivery")
	if err := store.ValidateGitHubWebhookDelivery(id, deliveryID, event); err != nil {
		return false, translateBackendConflict(err)
	}
	tag, err := s.boundary.Exec(ctx, `INSERT INTO github_webhook_deliveries (workspace_id,delivery_id,event,received_at) SELECT id,$2::text,$3::text,$4::timestamptz FROM workspaces WHERE id=$1 ON CONFLICT (workspace_id,delivery_id) DO NOTHING`, id, deliveryID, event, receivedAt.UTC().Truncate(time.Microsecond))
	if err != nil {
		return false, translateBackendConflict(err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists string
	if err := s.boundary.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1`, id).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, store.ErrNotFound
		}
		return false, translateBackendConflict(err)
	}
	return false, nil
}

func (s *Store) ForgetGitHubWebhookDelivery(ctx context.Context, id, deliveryID string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "ForgetGitHubWebhookDelivery")
	_, err := s.boundary.Exec(ctx, `DELETE FROM github_webhook_deliveries WHERE workspace_id=$1 AND delivery_id=$2`, id, deliveryID)
	return translateBackendConflict(err)
}

func (s *Store) GitHubWebhookStatus(ctx context.Context, id string) (core.GitHubWebhookStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GitHubWebhookStatus")
	var status core.GitHubWebhookStatus
	var exists string
	var setAt, lastAt *time.Time
	err := s.boundary.QueryRow(ctx, `SELECT w.id,
		(SELECT set_at FROM workspace_github_webhook_secrets WHERE workspace_id=w.id),
		(SELECT received_at FROM github_webhook_deliveries WHERE workspace_id=w.id ORDER BY received_at DESC LIMIT 1)
		FROM workspaces w WHERE w.id=$1`, id).Scan(&exists, &setAt, &lastAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, store.ErrNotFound
	}
	if err != nil {
		return status, translateBackendConflict(err)
	}
	if setAt != nil {
		at := setAt.UTC()
		status.SecretConfigured, status.SecretSetAt = true, &at
	}
	if lastAt != nil {
		at := lastAt.UTC()
		status.LastDeliveryAt = &at
	}
	return status, nil
}
