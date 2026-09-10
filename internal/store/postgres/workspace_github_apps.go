package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"time"
)

// DEC-41; component-identity-membership. Mutations serialize on the workspace
// and append metadata-only events in the same transaction as encrypted storage.
const appStatusColumns = `workspace_id,app_id,app_slug,client_id,COALESCE(installation_id,0),COALESCE(installation_account,''),connected_by,connected_at,installation_recorded_at`

type appScanner interface{ Scan(...any) error }

func scanAppStatus(row appScanner) (core.WorkspaceGitHubAppStatus, error) {
	var r core.WorkspaceGitHubAppStatus
	var at sql.NullTime
	err := row.Scan(&r.WorkspaceID, &r.AppID, &r.AppSlug, &r.ClientID, &r.InstallationID, &r.InstallationAccount, &r.ConnectedBy, &r.ConnectedAt, &at)
	if errors.Is(err, pgx.ErrNoRows) {
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
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_github_apps WHERE workspace_id=$1`, id).Scan(&count); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workspace_github_apps (workspace_id,app_id,app_slug,client_id,private_key_nonce,private_key_ciphertext,connected_by,connected_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (workspace_id) DO UPDATE SET app_id=EXCLUDED.app_id,app_slug=EXCLUDED.app_slug,client_id=EXCLUDED.client_id,private_key_nonce=EXCLUDED.private_key_nonce,private_key_ciphertext=EXCLUDED.private_key_ciphertext,connected_by=EXCLUDED.connected_by,connected_at=EXCLUDED.connected_at,installation_id=NULL,installation_account=NULL,installation_recorded_at=NULL`, id, app.AppID, app.AppSlug, app.ClientID, nonce, ciphertext, result.ConnectedBy, result.ConnectedAt)
		if err != nil {
			return err
		}
		kind := "workspace.github_app_connected"
		if count > 0 {
			kind = "workspace.github_app_replaced"
		}
		return s.appendAppEvent(ctx, q, kind, result)
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var err error
		result, err = scanAppStatus(tx.QueryRow(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=$1`, id))
		if err != nil {
			return err
		}
		if result.AppID != appID {
			return store.ErrGitHubAppChanged
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err = tx.Exec(ctx, `UPDATE workspace_github_apps SET installation_id=$2,installation_account=$3,installation_recorded_at=$4 WHERE workspace_id=$1`, id, installationID, account, now); err != nil {
			return err
		}
		result.InstallationID = installationID
		result.InstallationAccount = account
		result.InstallationRecordedAt = &now
		kind := "workspace.github_app_installation_recorded"
		return s.appendAppEvent(ctx, q, kind, result)
	})
	if err != nil {
		return core.WorkspaceGitHubAppStatus{}, translateBackendConflict(err)
	}
	return result, nil
}
func (s *Store) GetWorkspaceGitHubAppStatus(ctx context.Context, id string) (core.WorkspaceGitHubAppStatus, error) {
	result, err := scanAppStatus(s.pool.QueryRow(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=$1`, id))
	if errors.Is(err, store.ErrNotFound) {
		var exists string
		if e := s.pool.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1`, id).Scan(&exists); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
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
	err := s.pool.QueryRow(ctx, `SELECT `+appStatusColumns+`,private_key_nonce,private_key_ciphertext FROM workspace_github_apps WHERE workspace_id=$1`, id).Scan(&r.WorkspaceID, &r.AppID, &r.AppSlug, &r.ClientID, &r.InstallationID, &r.InstallationAccount, &r.ConnectedBy, &r.ConnectedAt, &at, &nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
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
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		result, err := scanAppStatus(tx.QueryRow(ctx, `SELECT `+appStatusColumns+` FROM workspace_github_apps WHERE workspace_id=$1`, id))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM workspace_github_apps WHERE workspace_id=$1`, id); err != nil {
			return err
		}
		kind := "workspace.github_app_disconnected"
		return s.appendAppEvent(ctx, q, kind, result)
	})
}

func (s *Store) appendAppEvent(ctx context.Context, q *db.Queries, kind string, status core.WorkspaceGitHubAppStatus) error {
	actor := store.ActorFromContext(ctx)
	_, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{WorkspaceID: status.WorkspaceID, Kind: kind, ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(store.WorkspaceGitHubAppEvent(status)), At: timestamp(time.Now().UTC())})
	return err
}
