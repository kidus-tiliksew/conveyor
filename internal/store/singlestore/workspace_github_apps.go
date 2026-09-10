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
)

// DEC-41; component-identity-membership. Mutations serialize on the workspace
// and append metadata-only events in the same transaction as encrypted storage.
const appStatusColumns = `workspace_id,app_id,app_slug,client_id,COALESCE(installation_id,0),COALESCE(installation_account,''),connected_by,connected_at,installation_recorded_at`

type appScanner interface{ Scan(...any) error }

func scanAppStatus(row appScanner) (core.WorkspaceGitHubAppStatus, error) {
	var r core.WorkspaceGitHubAppStatus
	var at sql.NullTime
	err := row.Scan(&r.WorkspaceID, &r.AppID, &r.AppSlug, &r.ClientID, &r.InstallationID, &r.InstallationAccount, &r.ConnectedBy, &r.ConnectedAt, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return r, store.ErrNotFound
	}
	if err != nil {
		return r, translateBackendConflict(err)
	}
	r.Connected = true
	if at.Valid {
		r.InstallationRecordedAt = &at.Time
	}
	return r, nil
}
func (s *Store) StoreWorkspaceGitHubApp(ctx context.Context, id string, app core.WorkspaceGitHubAppCredential) (core.WorkspaceGitHubAppStatus, error) {
	if err := store.ValidateWorkspaceGitHubApp(id, app); err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	a, err := s.forgeTokenAEAD()
	if err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	ciphertext := a.Seal(nil, nonce, []byte(app.PrivateKey), []byte("workspace-app:"+id))
	result := core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: id, AppID: app.AppID, AppSlug: app.AppSlug, ClientID: app.ClientID, ConnectedBy: store.ActorFromContext(ctx).ID, ConnectedAt: time.Now().UTC().Truncate(time.Microsecond)}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_github_apps WHERE workspace_id=?`, id).Scan(&count); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO workspace_github_apps (workspace_id,app_id,app_slug,client_id,private_key_nonce,private_key_ciphertext,connected_by,connected_at) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE app_id=VALUES(app_id),app_slug=VALUES(app_slug),client_id=VALUES(client_id),private_key_nonce=VALUES(private_key_nonce),private_key_ciphertext=VALUES(private_key_ciphertext),connected_by=VALUES(connected_by),connected_at=VALUES(connected_at),installation_id=NULL,installation_account=NULL,installation_recorded_at=NULL`, id, app.AppID, app.AppSlug, app.ClientID, nonce, ciphertext, result.ConnectedBy, result.ConnectedAt)
		if err != nil {
			return err
		}
		kind := "workspace.github_app_connected"
		if count > 0 {
			kind = "workspace.github_app_replaced"
		}
		return s.appendAppEvent(ctx, tx, kind, result)
	})
	if err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	redact.RegisterSecret(app.PrivateKey, time.Time{})
	return result, nil
}
func (s *Store) RecordWorkspaceGitHubAppInstallation(ctx context.Context, id string, appID, installationID int64, account string) (core.WorkspaceGitHubAppStatus, error) {
	var result core.WorkspaceGitHubAppStatus
	if installationID <= 0 || account == "" {
		return result, store.ErrNotFound
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var err error
		result, err = scanAppStatus(tx.QueryRowContext(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=?`, id))
		if err != nil {
			return err
		}
		if result.AppID != appID {
			return store.ErrGitHubAppChanged
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err = tx.ExecContext(ctx, `UPDATE workspace_github_apps SET installation_id=?,installation_account=?,installation_recorded_at=? WHERE workspace_id=?`, installationID, account, now, id); err != nil {
			return err
		}
		result.InstallationID = installationID
		result.InstallationAccount = account
		result.InstallationRecordedAt = &now
		kind := "workspace.github_app_installation_recorded"
		return s.appendAppEvent(ctx, tx, kind, result)
	})
	if err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	return result, nil
}
func (s *Store) GetWorkspaceGitHubAppStatus(ctx context.Context, id string) (core.WorkspaceGitHubAppStatus, error) {
	result, err := scanAppStatus(s.db.QueryRowContext(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=?`, id))
	if errors.Is(err, store.ErrNotFound) {
		var exists string
		if e := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=?`, id).Scan(&exists); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return core.WorkspaceGitHubAppStatus{}, store.ErrNotFound
			}
			return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(e)
		}
		return core.WorkspaceGitHubAppStatus{}, nil
	}
	return result, translateBackendConflict(err)
}
func (s *Store) GetWorkspaceGitHubAppForUse(ctx context.Context, id string) (core.WorkspaceGitHubAppCredential, error) {
	var r core.WorkspaceGitHubAppCredential
	var at sql.NullTime
	var nonce, ciphertext []byte
	err := s.db.QueryRowContext(ctx, `SELECT `+appStatusColumns+`,private_key_nonce,private_key_ciphertext FROM workspace_github_apps WHERE workspace_id=?`, id).Scan(&r.WorkspaceID, &r.AppID, &r.AppSlug, &r.ClientID, &r.InstallationID, &r.InstallationAccount, &r.ConnectedBy, &r.ConnectedAt, &at, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return r, store.ErrNotFound
	}
	if err != nil {
		return r, translateBackendConflict(err)
	}
	a, err := s.forgeTokenAEAD()
	if err != nil {
		return core.WorkspaceGitHubAppCredential{}, translateBackendConflict(err)
	}
	if len(nonce) != a.NonceSize() {
		return core.WorkspaceGitHubAppCredential{}, store.ErrForgeTokenDecrypt
	}
	key, err := a.Open(nil, nonce, ciphertext, []byte("workspace-app:"+id))
	if err != nil {
		return core.WorkspaceGitHubAppCredential{}, store.ErrForgeTokenDecrypt
	}
	r.PrivateKey = string(key)
	r.Connected = true
	if at.Valid {
		r.InstallationRecordedAt = &at.Time
	}
	redact.RegisterSecret(r.PrivateKey, time.Time{})
	return r, nil
}
func (s *Store) DeleteWorkspaceGitHubApp(ctx context.Context, id string) error {
	if id == "" {
		return store.ErrNotFound
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		result, err := scanAppStatus(tx.QueryRowContext(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=?`, id))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM workspace_github_apps WHERE workspace_id=?`, id); err != nil {
			return err
		}
		kind := "workspace.github_app_disconnected"
		return s.appendAppEvent(ctx, tx, kind, result)
	})
}

func (s *Store) appendAppEvent(ctx context.Context, tx *sql.Tx, kind string, status core.WorkspaceGitHubAppStatus) error {
	_, err := appendWorkspaceEvent(ctx, tx, status.WorkspaceID, kind, store.WorkspaceGitHubAppEvent(status))
	return err
}
