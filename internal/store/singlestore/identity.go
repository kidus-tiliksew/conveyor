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

// provisionIdentity resolves or creates the account and consumes its pending
// invitations. byLink selects sign-in link redemption, whose grants are
// attributed to the redeemed account rather than to the inviter
// (req-accounts-and-membership AC-3.1).
func provisionIdentity(ctx context.Context, tx *sql.Tx, email, name string, byLink bool) (core.IdentityUser, error) {
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
		actorID := store.UserActorID(i.by)
		payload := map[string]any{"workspace_id": i.ws, "user_id": u.ID, "email": u.Email, "role": i.role, "invitation": false, "redemption": true, "granted_by": i.by}
		if byLink {
			actorID = store.UserActorID(u.ID)
			payload["invited_by"] = i.by
		}
		actorCtx := store.WithActor(ctx, store.Actor{ID: actorID, Role: core.ActorUser})
		if _, err = appendWorkspaceEvent(actorCtx, tx, i.ws, "workspace.membership_granted", payload); err != nil {
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
		u, err = provisionIdentity(ctx, tx, email, name, false)
		return translateBackendConflict(err)
	})
	return u, translateBackendConflict(err)
}

// BootstrapIdentity maps the configured deployment token to the
// instance-administration principal, the owner of the sole
// deployment_credential marker. A live marker keeps its owner across restart,
// configuration change, and rotation; without a marker the configured first
// operator becomes the owner. It never writes a workspace binding, so
// demotions and exclusions survive every restart (DEC-63(3), DEC-63(4)).
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
	systemCtx := store.WithActor(ctx, store.Actor{ID: "system", Role: core.ActorSystem})
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		if err := store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapLocked); err != nil {
			return err
		}
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
		marked := legacyErr == nil
		same := marked && subtle.ConstantTimeCompare(oldHash, hash[:]) == 1
		if same && revoked.Valid {
			return errors.New("legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT")
		}
		if marked && status != "active" {
			// An inactive owner fails closed; no other operator replaces it.
			return errors.New("deployment owner account is deactivated; reactivate it before startup")
		}
		if marked && !revoked.Valid {
			if same && kind == "user" && scope == "operator" {
				return store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapBeforeCommit)
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
			changed = true
			if err := appendDeploymentEvent(systemCtx, tx, "identity.legacy_token_rotated", map[string]any{"credential_id": id}); err != nil {
				return translateBackendConflict(err)
			}
			return store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapBeforeCommit)
		}
		ownerID := userID
		if marked {
			// A revoked marker keeps its owner; the new deployment token is
			// reissued to the same account (DEC-63(3)).
			if _, err = writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "UPDATE", values: map[string]any{"label": "retired legacy API token", "deployment_credential": false}, where: map[string]any{"id": id}}); err != nil {
				return translateBackendConflict(err)
			}
		} else {
			u, err := provisionIdentity(ctx, tx, email, name, false)
			if err != nil {
				if err.Error() == "provisioned account is deactivated" {
					return errors.New("configured first operator account is deactivated")
				}
				return translateBackendConflict(err)
			}
			ownerID = u.ID
			if _, err = tx.ExecContext(ctx, "UPDATE orgs SET name=? WHERE id='deployment' AND name='Conveyor'", org); err != nil {
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
		if _, err = writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": tokenID, "user_id": ownerID, "label": "legacy API token", "token_hash": hash[:], "kind": "user", "scope": "operator", "deployment_credential": true}}); err != nil {
			return translateBackendConflict(err)
		}
		if marked {
			if err = appendDeploymentEvent(systemCtx, tx, "identity.legacy_token_rotated", map[string]any{"credential_id": tokenID}); err != nil {
				return translateBackendConflict(err)
			}
		}
		changed = true
		return store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapBeforeCommit)
	})
	return changed && err == nil, translateBackendConflict(err)
}

// deploymentOwner returns the active owner of the deployment marker inside a
// transaction that already holds the registry locks.
func deploymentOwner(ctx context.Context, tx *sql.Tx) (string, bool, error) {
	var owner string
	err := tx.QueryRowContext(ctx, "SELECT u.id FROM user_tokens t JOIN users u ON u.id=t.user_id WHERE t.deployment_credential=TRUE AND u.status='active'").Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, translateBackendConflict(err)
	}
	return owner, true, nil
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
