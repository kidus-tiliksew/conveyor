package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) issueCredential(ctx context.Context, userID, label string, agent bool) (core.IssuedPersonalAccessToken, error) {
	prefix, idPrefix := "pat", "pat"
	if agent {
		prefix, idPrefix = "agent", "agt"
	}
	id, err := randomIdentityID(idPrefix, 12)
	if err != nil {
		return core.IssuedPersonalAccessToken{}, translateBackendConflict(err)
	}
	secret, err := randomIdentityID("cv_"+prefix+"_"+id, 32)
	if err != nil {
		return core.IssuedPersonalAccessToken{}, translateBackendConflict(err)
	}
	hash := sha256.Sum256([]byte(secret))
	row := core.PersonalAccessToken{ID: id, UserID: userID, Label: strings.TrimSpace(label), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, "SELECT status FROM users WHERE id=? FOR UPDATE", userID).Scan(&status); err != nil {
			return identityNotFound(err)
		}
		if status != "active" {
			return core.ErrInvalidCredential
		}
		kind, scope := "user", "user"
		if agent {
			kind = "agent"
		} else {
			var operator bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM workspace_role_bindings WHERE user_id=? AND role='operator')", userID).Scan(&operator); err != nil {
				return translateBackendConflict(err)
			}
			if operator {
				scope = "operator"
			}
		}
		if err := checkTokenHash(ctx, tx, hash[:], ""); err != nil {
			return translateBackendConflict(err)
		}
		if _, err := writeRow(ctx, tx, rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": id, "user_id": userID, "label": row.Label, "token_hash": hash[:], "kind": kind, "scope": scope, "deployment_credential": false, "created_at": row.CreatedAt}}); err != nil {
			return translateBackendConflict(err)
		}
		if agent {
			return nil
		}
		return auditPersonalToken(ctx, tx, row, "identity.personal_token_issued")
	})
	if err != nil {
		return core.IssuedPersonalAccessToken{}, translateBackendConflict(err)
	}
	return core.IssuedPersonalAccessToken{PersonalAccessToken: row, Value: secret}, nil
}
func auditPersonalToken(ctx context.Context, tx *sql.Tx, row core.PersonalAccessToken, kind string) error {
	owner := row.UserID
	if c, ok := store.CredentialFromContext(ctx); ok {
		owner = c.OwnerUserID
	}
	return appendDeploymentEvent(store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner), Role: core.ActorUser}), tx, kind, map[string]any{"credential_id": row.ID, "label": row.Label})
}
func (s *Store) IssueOwnPersonalAccessToken(ctx context.Context, userID, label string) (core.IssuedPersonalAccessToken, error) {
	return s.issueCredential(ctx, userID, label, false)
}
func (s *Store) IssueAgentCredential(ctx context.Context, userID, label string) (store.IssuedAgentCredential, error) {
	r, err := s.issueCredential(ctx, userID, label, true)
	return store.IssuedAgentCredential{ID: r.ID, UserID: r.UserID, Label: r.Label, Value: r.Value}, translateBackendConflict(err)
}
func (s *Store) RevokeRunAgentCredential(ctx context.Context, userID, id string, expected store.RunAgentCredentialBinding) error {
	return s.identityTx(ctx, func(tx *sql.Tx) error {
		var label string
		if err := tx.QueryRowContext(ctx, "SELECT label FROM user_tokens WHERE id=? AND user_id=? AND kind='agent' FOR UPDATE", id, userID).Scan(&label); err != nil {
			return identityNotFound(err)
		}
		binding, ok := store.ParseRunAgentCredentialLabel(label)
		if !ok || binding != expected {
			return store.ErrRunAgentCredentialBindingMismatch
		}
		_, err := tx.ExecContext(ctx, "UPDATE user_tokens SET revoked_at=COALESCE(revoked_at,CURRENT_TIMESTAMP(6)) WHERE id=?", id)
		return translateBackendConflict(err)
	})
}

// credentialUseGranularity bounds how often bearer verification writes a
// credential row: a credential used within it is verified by a read alone,
// so last_used_at is accurate to this interval (component-identity-membership).
const credentialUseGranularity = time.Minute

// credentialRead is one verification read of a live bearer credential.
type credentialRead struct {
	credential core.AuthenticatedCredential
	user       core.IdentityUser
	stale      bool
}

// readCredential reads a live credential by its hash on the ordinary handle,
// outside identityTx and its registry locks: unrevoked and owned by an active
// user. It reports whether last_used_at is unset or older than the stamp
// interval by the database clock.
func (s *Store) readCredential(ctx context.Context, hash []byte) (credentialRead, error) {
	var r credentialRead
	var stored []byte
	var revoked sql.NullTime
	var label string
	err := s.db.QueryRowContext(ctx, "SELECT t.id,t.user_id,t.kind,t.scope,t.token_hash,t.revoked_at,t.label,u.email,u.display_name,u.status,t.last_used_at IS NULL OR t.last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND) FROM user_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=?",
		credentialUseGranularity.Microseconds(), hash).Scan(&r.credential.ID, &r.credential.OwnerUserID, &r.credential.Kind, &r.credential.Scope, &stored, &revoked, &label, &r.user.Email, &r.user.DisplayName, &r.user.Status, &r.stale)
	if errors.Is(err, sql.ErrNoRows) {
		return credentialRead{}, core.ErrInvalidCredential
	}
	if err != nil {
		return credentialRead{}, translateBackendConflict(err)
	}
	if revoked.Valid || r.user.Status != "active" || subtle.ConstantTimeCompare(stored, hash) != 1 {
		return credentialRead{}, core.ErrInvalidCredential
	}
	r.user.ID = r.credential.OwnerUserID
	r.credential.Method = core.CredentialMethodBearer
	if r.credential.Kind == core.CredentialAgent {
		if b, ok := store.ParseRunAgentCredentialLabel(label); ok {
			r.credential.RunWorkspaceID = b.WorkspaceID
			r.credential.RunWorkOrderID = b.WorkOrderID
			r.credential.RunSessionID = b.SessionID
		}
	}
	return r, nil
}

// verifyCredential reads before it writes and never enters identityTx, so a
// bearer call takes neither registry lock row. Every call's read refuses a
// revocation or deactivation that completed first. A stale credential gets
// one conditional UPDATE that repeats every validity predicate; a call whose
// UPDATE changes nothing rereads, because a concurrent call stamped first or
// the credential lost its validity in between (component-identity-membership;
// req-security-boundaries AC-2.1, AC-2.3).
func (s *Store) verifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	read, err := s.readCredential(ctx, hash[:])
	if err != nil {
		return core.AuthenticatedCredential{}, core.IdentityUser{}, err
	}
	if err = store.RunIdentityTestHook(ctx, store.IdentityHookCredentialRead); err != nil {
		return core.AuthenticatedCredential{}, core.IdentityUser{}, err
	}
	if read.stale {
		if err = store.RunIdentityTestHook(ctx, store.IdentityHookCredentialStamping); err != nil {
			return core.AuthenticatedCredential{}, core.IdentityUser{}, err
		}
		result, err := s.db.ExecContext(ctx, "UPDATE user_tokens SET last_used_at=CURRENT_TIMESTAMP(6) WHERE id=? AND token_hash=? AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND)) AND user_id IN (SELECT id FROM users WHERE status='active')",
			read.credential.ID, hash[:], credentialUseGranularity.Microseconds())
		if err != nil {
			return core.AuthenticatedCredential{}, core.IdentityUser{}, translateBackendConflict(err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return core.AuthenticatedCredential{}, core.IdentityUser{}, translateBackendConflict(err)
		}
		if changed == 1 {
			if err = store.RunIdentityTestHook(ctx, store.IdentityHookCredentialStamped); err != nil {
				return core.AuthenticatedCredential{}, core.IdentityUser{}, err
			}
		} else if read, err = s.readCredential(ctx, hash[:]); err != nil {
			// A concurrent verification stamped first, or the credential
			// lost its validity in between; the reread decides which.
			return core.AuthenticatedCredential{}, core.IdentityUser{}, err
		}
	}
	return read.credential, read.user, nil
}
func (s *Store) VerifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	c, _, err := s.verifyCredential(ctx, candidate)
	return c, translateBackendConflict(err)
}
func (s *Store) VerifyPersonalAccessToken(ctx context.Context, candidate string) (core.IdentityUser, error) {
	c, u, err := s.verifyCredential(ctx, candidate)
	if err != nil {
		return core.IdentityUser{}, translateBackendConflict(err)
	}
	if c.Kind != core.CredentialUser {
		return core.IdentityUser{}, core.ErrInvalidCredential
	}
	return u, nil
}
func scanPersonalToken(row interface{ Scan(...any) error }) (core.PersonalAccessToken, error) {
	var r core.PersonalAccessToken
	var used, revoked sql.NullTime
	err := row.Scan(&r.ID, &r.UserID, &r.Label, &r.DeploymentCredential, &r.CreatedAt, &used, &revoked)
	if used.Valid {
		r.LastUsedAt = &used.Time
	}
	if revoked.Valid {
		r.RevokedAt = &revoked.Time
	}
	return r, identityNotFound(err)
}

const personalTokenColumns = "id,user_id,label,deployment_credential,created_at,last_used_at,revoked_at"

func (s *Store) ListOwnPersonalAccessTokens(ctx context.Context, userID string) ([]core.PersonalAccessToken, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+personalTokenColumns+" FROM user_tokens WHERE user_id=? AND kind='user' ORDER BY created_at DESC,id", userID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	items := []core.PersonalAccessToken{}
	for rows.Next() {
		r, e := scanPersonalToken(rows)
		if e != nil {
			return nil, translateBackendConflict(e)
		}
		items = append(items, r)
	}
	return items, translateBackendConflict(rows.Err())
}
func (s *Store) RevokeOwnPersonalAccessToken(ctx context.Context, userID, id string) (core.PersonalAccessToken, error) {
	var r core.PersonalAccessToken
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		var err error
		r, err = scanPersonalToken(tx.QueryRowContext(ctx, "SELECT "+personalTokenColumns+" FROM user_tokens WHERE id=? AND user_id=? AND kind='user' FOR UPDATE", id, userID))
		if err != nil {
			return translateBackendConflict(err)
		}
		if r.RevokedAt == nil {
			now := time.Now().UTC().Truncate(time.Microsecond)
			r.RevokedAt = &now
			if _, err = tx.ExecContext(ctx, "UPDATE user_tokens SET revoked_at=? WHERE id=?", now, id); err != nil {
				return translateBackendConflict(err)
			}
		}
		return auditPersonalToken(ctx, tx, r, "identity.personal_token_revoked")
	})
	return r, translateBackendConflict(err)
}
