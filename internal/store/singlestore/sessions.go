package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	signInLinkLifetime       = 30 * time.Minute
	dashboardSessionLifetime = 7 * 24 * time.Hour
)

func (s *Store) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IssuedSignInLink{}, translateBackendConflict(err)
	}
	id, err := randomIdentityID("sil", 12)
	if err != nil {
		return core.IssuedSignInLink{}, translateBackendConflict(err)
	}
	value, err := randomIdentityID("cv_signin_"+id, 32)
	if err != nil {
		return core.IssuedSignInLink{}, translateBackendConflict(err)
	}
	hash := sha256.Sum256([]byte(value))
	expires := time.Now().UTC().Add(signInLinkLifetime).Truncate(time.Microsecond)
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		var allowed bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE email=? AND status='active') OR EXISTS(SELECT 1 FROM workspace_membership_invitations WHERE email=?)", email, email).Scan(&allowed); err != nil {
			return translateBackendConflict(err)
		}
		if !allowed {
			return store.ErrNotFound
		}
		var uid any
		var userID string
		err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email=? AND status='active'", email).Scan(&userID)
		if err == nil {
			uid = userID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return translateBackendConflict(err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM invitation_signin_tokens WHERE token_hash=?", hash[:]).Scan(&n); err != nil {
			return translateBackendConflict(err)
		}
		if n > 0 {
			return errors.New("sign-in hash already exists")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invitation_signin_tokens SET redeemed_at=CURRENT_TIMESTAMP(6) WHERE email=? AND redeemed_at IS NULL", email); err != nil {
			return translateBackendConflict(err)
		}
		if _, err := writeRow(ctx, tx, rowWrite{table: "invitation_signin_tokens", operation: "INSERT", values: map[string]any{"id": id, "email": email, "user_id": uid, "token_hash": hash[:], "expires_at": expires}}); err != nil {
			return translateBackendConflict(err)
		}
		return appendDeploymentEvent(ctx, tx, "identity.signin_link_issued", map[string]any{"signin_link_id": id, "email": email})
	})
	if err != nil {
		return core.IssuedSignInLink{}, translateBackendConflict(err)
	}
	return core.IssuedSignInLink{Email: email, Value: value, ExpiresAt: expires}, nil
}
func mintDashboardSession(ctx context.Context, tx *sql.Tx, uid string, byLink bool) (core.DashboardSession, error) {
	id, err := randomIdentityID("ses", 12)
	if err != nil {
		return core.DashboardSession{}, translateBackendConflict(err)
	}
	value, err := randomIdentityID("cv_session_"+id, 32)
	if err != nil {
		return core.DashboardSession{}, translateBackendConflict(err)
	}
	hash := sha256.Sum256([]byte(value))
	expires := time.Now().UTC().Add(dashboardSessionLifetime).Truncate(time.Microsecond)
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM dashboard_sessions WHERE session_hash=?", hash[:]).Scan(&n); err != nil {
		return core.DashboardSession{}, translateBackendConflict(err)
	}
	if n > 0 {
		return core.DashboardSession{}, errors.New("session hash already exists")
	}
	if _, err = writeRow(ctx, tx, rowWrite{table: "dashboard_sessions", operation: "INSERT", values: map[string]any{"id": id, "user_id": uid, "session_hash": hash[:], "expires_at": expires, "established_by_link": byLink}}); err != nil {
		return core.DashboardSession{}, translateBackendConflict(err)
	}
	return core.DashboardSession{ID: id, UserID: uid, Value: value, ExpiresAt: expires}, nil
}
func userActorContext(ctx context.Context, uid string) context.Context {
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID(uid), Role: core.ActorUser})
}
func (s *Store) RedeemSignInLink(ctx context.Context, candidate string) (core.DashboardSession, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	var session core.DashboardSession
	var user core.IdentityUser
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		var id, email string
		var uid sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT id,email,user_id FROM invitation_signin_tokens WHERE token_hash=? AND redeemed_at IS NULL AND expires_at>CURRENT_TIMESTAMP(6)", hash[:]).Scan(&id, &email, &uid); err != nil {
			return translateBackendConflict(err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invitation_signin_tokens SET redeemed_at=CURRENT_TIMESTAMP(6) WHERE id=?", id); err != nil {
			return translateBackendConflict(err)
		}
		if !uid.Valid {
			u, err := provisionIdentity(ctx, tx, email, email)
			if err != nil {
				return translateBackendConflict(err)
			}
			uid = sql.NullString{String: u.ID, Valid: true}
			if _, err = tx.ExecContext(ctx, "UPDATE invitation_signin_tokens SET user_id=? WHERE id=?", u.ID, id); err != nil {
				return translateBackendConflict(err)
			}
		}
		var err error
		user, err = scanIdentity(tx.QueryRowContext(ctx, "SELECT id,email,display_name,status,created_at FROM users WHERE id=? AND status='active'", uid.String))
		if err != nil {
			return translateBackendConflict(err)
		}
		session, err = mintDashboardSession(ctx, tx, user.ID, true)
		if err != nil {
			return translateBackendConflict(err)
		}
		actorCtx := userActorContext(ctx, user.ID)
		if err = appendDeploymentEvent(actorCtx, tx, "identity.signin_link_redeemed", map[string]any{"signin_link_id": id}); err != nil {
			return translateBackendConflict(err)
		}
		return appendDeploymentEvent(actorCtx, tx, "identity.dashboard_session_created", map[string]any{"session_id": session.ID})
	})
	if err != nil {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	return session, user, nil
}
func (s *Store) SignInWithPassword(ctx context.Context, email, password string) (core.DashboardSession, core.IdentityUser, error) {
	email, normalizeErr := normalizeIdentityEmail(email)
	dummy := fixedDummyPasswordHash()
	encoded := dummy
	var uid string
	if normalizeErr == nil {
		var hash sql.NullString
		err := s.db.QueryRowContext(ctx, "SELECT id,password_hash FROM users WHERE email=? AND status='active'", email).Scan(&uid, &hash)
		if err == nil && hash.Valid {
			encoded = hash.String
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.DashboardSession{}, core.IdentityUser{}, translateBackendConflict(err)
		}
	}
	if !verifyPassword(encoded, password) || encoded == dummy {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	var session core.DashboardSession
	var user core.IdentityUser
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		var hash sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT id,email,display_name,status,created_at,password_hash FROM users WHERE id=? AND status='active' FOR UPDATE", uid).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.CreatedAt, &hash); err != nil {
			return translateBackendConflict(err)
		}
		if !hash.Valid || subtle.ConstantTimeCompare([]byte(hash.String), []byte(encoded)) != 1 {
			return core.ErrInvalidCredential
		}
		var err error
		session, err = mintDashboardSession(ctx, tx, user.ID, false)
		if err != nil {
			return translateBackendConflict(err)
		}
		return appendDeploymentEvent(userActorContext(ctx, user.ID), tx, "identity.dashboard_session_created", map[string]any{"session_id": session.ID, "method": "password"})
	})
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrInvalidCredential
	}
	if err != nil {
		return core.DashboardSession{}, core.IdentityUser{}, translateBackendConflict(err)
	}
	return session, user, nil
}
func (s *Store) SetOwnPassword(ctx context.Context, userID, sessionID, currentPassword, newPassword string) error {
	if !validNewPassword(newPassword) {
		return store.ErrInvalidPassword
	}
	encoded, err := hashPassword(newPassword)
	if err != nil {
		return translateBackendConflict(err)
	}
	return s.identityTx(ctx, func(tx *sql.Tx) error {
		var current sql.NullString
		var byLink bool
		err := tx.QueryRowContext(ctx, "SELECT u.password_hash,s.established_by_link FROM users u JOIN dashboard_sessions s ON s.user_id=u.id WHERE u.id=? AND u.status='active' AND s.id=? AND s.revoked_at IS NULL AND s.expires_at>CURRENT_TIMESTAMP(6)", userID, sessionID).Scan(&current, &byLink)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrInvalidCredential
		}
		if err != nil {
			return translateBackendConflict(err)
		}
		if current.Valid && !byLink && !verifyPassword(current.String, currentPassword) {
			return store.ErrInvalidCurrentPassword
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET password_hash=? WHERE id=?", encoded, userID); err != nil {
			return translateBackendConflict(err)
		}
		kind := "identity.password_set"
		if current.Valid {
			kind = "identity.password_changed"
		}
		return appendDeploymentEvent(userActorContext(ctx, userID), tx, kind, map[string]any{"session_id": sessionID})
	})
}
func (s *Store) SetOwnDisplayName(ctx context.Context, userID, sessionID, name string) (core.CallerIdentity, error) {
	var r core.CallerIdentity
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT u.id,u.email,u.display_name FROM users u JOIN dashboard_sessions s ON s.user_id=u.id WHERE u.id=? AND u.status='active' AND s.id=? AND s.revoked_at IS NULL AND s.expires_at>CURRENT_TIMESTAMP(6)", userID, sessionID).Scan(&r.ID, &r.Email, &r.DisplayName)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrInvalidCredential
		}
		if err != nil {
			return translateBackendConflict(err)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET display_name=? WHERE id=?", name, userID); err != nil {
			return translateBackendConflict(err)
		}
		r.DisplayName = name
		return appendDeploymentEvent(ctx, tx, "identity.display_name_changed", map[string]any{"user_id": userID, "session_id": sessionID})
	})
	return r, translateBackendConflict(err)
}
func (s *Store) VerifyDashboardSession(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	hash := sha256.Sum256([]byte(candidate))
	var c core.AuthenticatedCredential
	err := s.identityTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT s.id,s.user_id,s.established_by_link FROM dashboard_sessions s JOIN users u ON u.id=s.user_id WHERE s.session_hash=? AND s.revoked_at IS NULL AND s.expires_at>CURRENT_TIMESTAMP(6) AND u.status='active'", hash[:]).Scan(&c.ID, &c.OwnerUserID, &c.SessionEstablishedByLink)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrInvalidCredential
		}
		if err != nil {
			return translateBackendConflict(err)
		}
		c.SessionExpiresAt = time.Now().UTC().Add(dashboardSessionLifetime).Truncate(time.Microsecond)
		if _, err = tx.ExecContext(ctx, "UPDATE dashboard_sessions SET last_used_at=CURRENT_TIMESTAMP(6),expires_at=? WHERE id=?", c.SessionExpiresAt, c.ID); err != nil {
			return translateBackendConflict(err)
		}
		var operator bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM workspace_role_bindings WHERE user_id=? AND role='operator')", c.OwnerUserID).Scan(&operator); err != nil {
			return translateBackendConflict(err)
		}
		c.Kind = core.CredentialUser
		c.Scope = core.CredentialScopeUser
		c.Method = core.CredentialMethodSession
		if operator {
			c.Scope = core.CredentialScopeOperator
		}
		return nil
	})
	if err != nil {
		return core.AuthenticatedCredential{}, translateBackendConflict(err)
	}
	return c, nil
}
func (s *Store) RevokeDashboardSession(ctx context.Context, uid, id string) error {
	return s.identityTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE dashboard_sessions SET revoked_at=CURRENT_TIMESTAMP(6) WHERE id=? AND user_id=? AND revoked_at IS NULL", id, uid)
		if err != nil {
			return translateBackendConflict(err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return translateBackendConflict(err)
		}
		if n != 1 {
			return store.ErrNotFound
		}
		return appendDeploymentEvent(userActorContext(ctx, uid), tx, "identity.dashboard_session_revoked", map[string]any{"session_id": id})
	})
}
func (s *Store) RecordInvitationDelivery(ctx context.Context, email, outcome string) error {
	if outcome != "sent" && outcome != "failed" && outcome != "fallback" {
		return errors.New("invalid invitation delivery outcome")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return appendDeploymentEvent(ctx, tx, "identity.invitation_delivery_"+outcome, map[string]any{"email": email})
	})
}
