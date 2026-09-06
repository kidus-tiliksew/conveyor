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
func (s *Store) verifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	var c core.AuthenticatedCredential
	var u core.IdentityUser
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		var stored []byte
		var revoked sql.NullTime
		var label string
		err := tx.QueryRowContext(ctx, "SELECT t.id,t.user_id,t.kind,t.scope,t.token_hash,t.revoked_at,t.label,u.email,u.display_name,u.status FROM user_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=?", hash[:]).Scan(&c.ID, &c.OwnerUserID, &c.Kind, &c.Scope, &stored, &revoked, &label, &u.Email, &u.DisplayName, &u.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrInvalidCredential
		}
		if err != nil {
			return translateBackendConflict(err)
		}
		if revoked.Valid || u.Status != "active" || subtle.ConstantTimeCompare(stored, hash[:]) != 1 {
			return core.ErrInvalidCredential
		}
		if _, err = tx.ExecContext(ctx, "UPDATE user_tokens SET last_used_at=CURRENT_TIMESTAMP(6) WHERE id=?", c.ID); err != nil {
			return translateBackendConflict(err)
		}
		u.ID = c.OwnerUserID
		c.Method = core.CredentialMethodBearer
		if c.Kind == core.CredentialAgent {
			if b, ok := store.ParseRunAgentCredentialLabel(label); ok {
				c.RunWorkspaceID = b.WorkspaceID
				c.RunWorkOrderID = b.WorkOrderID
				c.RunSessionID = b.SessionID
			}
		}
		return nil
	})
	if err != nil {
		return core.AuthenticatedCredential{}, core.IdentityUser{}, translateBackendConflict(err)
	}
	return c, u, nil
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
