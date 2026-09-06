package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Identity writes share a deployment lock because email and hash uniqueness
// span shards. Workspace bootstrap takes workspace-registry first too.
func (s *Store) identityTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "workspace-registry"); err != nil {
			return translateBackendConflict(err)
		}
		if err := lockKey(ctx, tx, "identity-registry"); err != nil {
			return translateBackendConflict(err)
		}
		return fn(tx)
	})
}
func normalizeIdentityEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("email must be a valid normalized address")
	}
	return email, nil
}
func randomIdentityID(prefix string, size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", translateBackendConflict(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}
func checkSingletonOrganization(ids []string) error {
	if len(ids) > 1 || len(ids) == 1 && ids[0] != "deployment" {
		return errors.New("deployment must contain exactly one organization with id deployment")
	}
	return nil
}
func ensureOrganization(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM orgs ORDER BY id")
	if err != nil {
		return false, translateBackendConflict(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, translateBackendConflict(err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, translateBackendConflict(err)
	}
	if err = checkSingletonOrganization(ids); err != nil {
		return false, translateBackendConflict(err)
	}
	if len(ids) == 0 {
		_, err = writeRow(ctx, tx, rowWrite{table: "orgs", operation: "INSERT", values: map[string]any{"id": "deployment", "name": name, "singleton": true}})
		return err == nil, translateBackendConflict(err)
	}
	result, err := tx.ExecContext(ctx, "UPDATE orgs SET singleton=TRUE WHERE id='deployment' AND singleton=FALSE")
	if err != nil {
		return false, translateBackendConflict(err)
	}
	n, err := result.RowsAffected()
	return n > 0, translateBackendConflict(err)
}
func scanIdentity(row interface{ Scan(...any) error }) (core.IdentityUser, error) {
	var u core.IdentityUser
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &u.CreatedAt)
	return u, identityNotFound(err)
}
func provisionIdentity(ctx context.Context, tx *sql.Tx, email, name string) (core.IdentityUser, error) {
	u, err := scanIdentity(tx.QueryRowContext(ctx, "SELECT id,email,display_name,status,created_at FROM users WHERE email=?", email))
	if errors.Is(err, store.ErrNotFound) {
		id, e := randomIdentityID("usr", 12)
		if e != nil {
			return u, translateBackendConflict(e)
		}
		u = core.IdentityUser{ID: id, Email: email, DisplayName: name, Status: "active", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		_, err = writeRow(ctx, tx, rowWrite{table: "users", operation: "INSERT", values: map[string]any{"id": u.ID, "email": email, "display_name": name, "status": "active", "created_at": u.CreatedAt}})
	}
	if err != nil {
		return u, translateBackendConflict(err)
	}
	if u.Status != "active" {
		return u, errors.New("provisioned account is deactivated")
	}
	rows, err := tx.QueryContext(ctx, "SELECT workspace_id,role,invited_by FROM workspace_membership_invitations WHERE email=? ORDER BY workspace_id", email)
	if err != nil {
		return u, translateBackendConflict(err)
	}
	type invitation struct{ ws, role, by string }
	var items []invitation
	for rows.Next() {
		var i invitation
		if err = rows.Scan(&i.ws, &i.role, &i.by); err != nil {
			rows.Close()
			return u, translateBackendConflict(err)
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return u, translateBackendConflict(err)
	}
	for _, i := range items {
		if err = identityWorkspace(ctx, tx, i.ws); err != nil {
			return u, translateBackendConflict(err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES(?,?,?) ON DUPLICATE KEY UPDATE role=VALUES(role),updated_at=CURRENT_TIMESTAMP(6)", i.ws, u.ID, i.role); err != nil {
			return u, translateBackendConflict(err)
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM workspace_membership_invitations WHERE workspace_id=? AND email=?", i.ws, email); err != nil {
			return u, translateBackendConflict(err)
		}
		actorCtx := store.WithActor(ctx, store.Actor{ID: store.UserActorID(i.by), Role: core.ActorUser})
		if _, err = appendWorkspaceEvent(actorCtx, tx, i.ws, "workspace.membership_granted", map[string]any{"workspace_id": i.ws, "user_id": u.ID, "email": u.Email, "role": i.role, "invitation": false, "redemption": true, "granted_by": i.by}); err != nil {
			return u, translateBackendConflict(err)
		}
	}
	return u, nil
}
func identityWorkspace(ctx context.Context, tx *sql.Tx, ws string) error {
	var org string
	err := tx.QueryRowContext(ctx, "SELECT org_id FROM workspaces WHERE id=? FOR UPDATE", ws).Scan(&org)
	if err != nil {
		return identityNotFound(err)
	}
	if org != "deployment" {
		return errors.New("workspace organization differs from deployment")
	}
	return nil
}
func (s *Store) ProvisionIdentityUser(ctx context.Context, email, name string) (core.IdentityUser, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IdentityUser{}, translateBackendConflict(err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return core.IdentityUser{}, errors.New("display name is required")
	}
	var u core.IdentityUser
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		if _, err := ensureOrganization(ctx, tx, "Conveyor"); err != nil {
			return translateBackendConflict(err)
		}
		var err error
		u, err = provisionIdentity(ctx, tx, email, name)
		return translateBackendConflict(err)
	})
	return u, translateBackendConflict(err)
}
func seedOperatorBindings(ctx context.Context, tx *sql.Tx, userID string) error {
	var invalid int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM workspaces WHERE org_id<>'deployment'").Scan(&invalid); err != nil {
		return translateBackendConflict(err)
	}
	if invalid != 0 {
		return errors.New("workspace organization differs from deployment")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO workspace_role_bindings(workspace_id,user_id,role) SELECT id,?,'operator' FROM workspaces ON DUPLICATE KEY UPDATE role='operator',updated_at=CURRENT_TIMESTAMP(6)", userID)
	return translateBackendConflict(err)
}
func (s *Store) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, legacyToken string) (bool, error) {
	email, err := normalizeIdentityEmail(identity.Email)
	if err != nil {
		return false, translateBackendConflict(err)
	}
	name, org := strings.TrimSpace(identity.DisplayName), strings.TrimSpace(identity.OrganizationName)
	if name == "" || org == "" {
		return false, errors.New("organization name and first operator display name are required")
	}
	if legacyToken == "" {
		return false, errors.New("legacy API token is required for identity bootstrap")
	}
	hash := sha256.Sum256([]byte(legacyToken))
	changed := false
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		orgCreated, err := ensureOrganization(ctx, tx, org)
		if err != nil {
			return translateBackendConflict(err)
		}
		changed = orgCreated
		var id, userID, status, kind, scope string
		var oldHash []byte
		var revoked sql.NullTime
		legacyErr := tx.QueryRowContext(ctx, "SELECT t.id,t.user_id,t.token_hash,t.kind,t.scope,t.revoked_at,u.status FROM user_tokens t JOIN users u ON u.id=t.user_id WHERE t.deployment_credential=TRUE").Scan(&id, &userID, &oldHash, &kind, &scope, &revoked, &status)
		if legacyErr != nil && !errors.Is(legacyErr, sql.ErrNoRows) {
			return legacyErr
		}
		same := legacyErr == nil && subtle.ConstantTimeCompare(oldHash, hash[:]) == 1
		if same && revoked.Valid {
			return errors.New("legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT")
		}
		if legacyErr == nil && status == "active" && !revoked.Valid {
			var uncovered int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM workspaces w WHERE NOT EXISTS(SELECT 1 FROM workspace_role_bindings b WHERE b.workspace_id=w.id AND b.user_id=? AND b.role='operator')", userID).Scan(&uncovered); err != nil {
				return translateBackendConflict(err)
			}
			if same && kind == "user" && scope == "operator" && uncovered == 0 {
				return nil
			}
			if err := checkTokenHash(ctx, tx, hash[:], id); err != nil {
				return translateBackendConflict(err)
			}
			values := map[string]any{"token_hash": hash[:], "kind": "user", "scope": "operator", "deployment_credential": true}
			if !same {
				values["last_used_at"] = nil
			}
			if _, err := writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "UPDATE", values: values, where: map[string]any{"id": id}}); err != nil {
				return translateBackendConflict(err)
			}
			if err := seedOperatorBindings(ctx, tx, userID); err != nil {
				return translateBackendConflict(err)
			}
			auditKind := "identity.legacy_token_rotated"
			if same {
				auditKind = "identity.legacy_bindings_healed"
			}
			changed = true
			return appendDeploymentEvent(store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem}), tx, auditKind, map[string]any{"credential_id": id})
		}
		u, err := scanIdentity(tx.QueryRowContext(ctx, "SELECT u.id,u.email,u.display_name,u.status,u.created_at FROM users u JOIN user_tokens t ON t.user_id=u.id WHERE t.kind='user' AND t.scope='operator' AND t.revoked_at IS NULL AND u.status='active' AND EXISTS(SELECT 1 FROM workspace_role_bindings b WHERE b.user_id=u.id AND b.role='operator') ORDER BY t.created_at,t.id LIMIT 1"))
		if errors.Is(err, store.ErrNotFound) {
			u, err = provisionIdentity(ctx, tx, email, name)
			if err == nil {
				_, err = tx.ExecContext(ctx, "UPDATE orgs SET name=? WHERE id='deployment' AND name='Conveyor'", org)
			}
		}
		if err != nil {
			return translateBackendConflict(err)
		}
		if legacyErr == nil {
			if _, err = writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "UPDATE", values: map[string]any{"label": "retired legacy API token", "deployment_credential": false}, where: map[string]any{"id": id}}); err != nil {
				return translateBackendConflict(err)
			}
		}
		tokenID, err := randomIdentityID("pat", 12)
		if err != nil {
			return translateBackendConflict(err)
		}
		if err = checkTokenHash(ctx, tx, hash[:], ""); err != nil {
			return translateBackendConflict(err)
		}
		if _, err = writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": tokenID, "user_id": u.ID, "label": "legacy API token", "token_hash": hash[:], "kind": "user", "scope": "operator", "deployment_credential": true}}); err != nil {
			return translateBackendConflict(err)
		}
		if legacyErr == nil {
			if err = appendDeploymentEvent(store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem}), tx, "identity.legacy_token_rotated", map[string]any{"credential_id": tokenID}); err != nil {
				return translateBackendConflict(err)
			}
		}
		changed = true
		return seedOperatorBindings(ctx, tx, u.ID)
	})
	return changed && err == nil, translateBackendConflict(err)
}
func (s *Store) GetCallerIdentity(ctx context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	var r core.CallerIdentity
	var err error
	if workspaceID == "" {
		err = s.db.QueryRowContext(ctx, "SELECT id,email,display_name FROM users WHERE id=? AND status='active'", userID).Scan(&r.ID, &r.Email, &r.DisplayName)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT u.id,u.email,u.display_name,b.role FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id WHERE u.id=? AND u.status='active' AND b.workspace_id=?", userID, workspaceID).Scan(&r.ID, &r.Email, &r.DisplayName, &r.Role)
	}
	return r, identityNotFound(err)
}
func checkTokenHash(ctx context.Context, tx *sql.Tx, hash []byte, id string) error {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_tokens WHERE token_hash=? AND id<>?", hash, id).Scan(&n); err != nil {
		return translateBackendConflict(err)
	}
	if n > 0 {
		return fmt.Errorf("credential hash already exists")
	}
	return nil
}
