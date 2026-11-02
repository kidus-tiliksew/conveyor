package singlestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The workspace GitHub webhook secret and accepted deliveries
// (component-identity-membership, The webhook secret; req-quiet-idle-runtime
// AC-3.2, AC-3.3, AC-3.6). Mutations of the secret serialize on the workspace
// row like App mutations; the secret is sealed like the App private key under
// its own additional data.

// storeWebhookSecretTx seals and upserts secret, or removes the row when
// secret is empty, inside the caller's transaction.
func (s *Store) storeWebhookSecretTx(ctx context.Context, tx *sql.Tx, workspace, secret string, at time.Time) error {
	if secret == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspace_github_webhook_secrets WHERE workspace_id=?`, workspace)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_github_webhook_secrets (workspace_id,secret_nonce,secret_ciphertext,set_at) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE secret_nonce=VALUES(secret_nonce),secret_ciphertext=VALUES(secret_ciphertext),set_at=VALUES(set_at)`, workspace, nonce, ciphertext, at.UTC().Truncate(time.Microsecond))
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
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var appID int64
		if err := tx.QueryRowContext(ctx, `SELECT app_id FROM workspace_github_apps WHERE workspace_id=?`, id).Scan(&appID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		if err := s.storeWebhookSecretTx(ctx, tx, id, secret, at); err != nil {
			return err
		}
		_, err := appendWorkspaceEvent(ctx, tx, id, store.GitHubWebhookSecretSetEvent, store.GitHubWebhookSecretEvent(id, appID, at))
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
	err := s.db.QueryRowContext(ctx, `SELECT secret_nonce,secret_ciphertext FROM workspace_github_webhook_secrets WHERE workspace_id=?`, id).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
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
	var exists string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ErrNotFound
		}
		return false, translateBackendConflict(err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO github_webhook_deliveries (workspace_id,delivery_id,event,received_at) VALUES (?,?,?,?)`, id, deliveryID, event, receivedAt.UTC().Truncate(time.Microsecond))
	if err != nil {
		return false, translateBackendConflict(err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, translateBackendConflict(err)
	}
	return inserted == 1, nil
}

func (s *Store) ForgetGitHubWebhookDelivery(ctx context.Context, id, deliveryID string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "ForgetGitHubWebhookDelivery")
	_, err := s.db.ExecContext(ctx, `DELETE FROM github_webhook_deliveries WHERE workspace_id=? AND delivery_id=?`, id, deliveryID)
	return translateBackendConflict(err)
}

func (s *Store) GitHubWebhookStatus(ctx context.Context, id string) (core.GitHubWebhookStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GitHubWebhookStatus")
	var status core.GitHubWebhookStatus
	var exists string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status, store.ErrNotFound
		}
		return status, translateBackendConflict(err)
	}
	var setAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT set_at FROM workspace_github_webhook_secrets WHERE workspace_id=?`, id).Scan(&setAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return status, translateBackendConflict(err)
	}
	if setAt.Valid {
		at := setAt.Time.UTC()
		status.SecretConfigured, status.SecretSetAt = true, &at
	}
	var lastAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM github_webhook_deliveries WHERE workspace_id=?`, id).Scan(&lastAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return status, translateBackendConflict(err)
	}
	if lastAt.Valid {
		at := lastAt.Time.UTC()
		status.LastDeliveryAt = &at
	}
	return status, nil
}
