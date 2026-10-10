package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var ErrInvalidPersonalAccessToken = core.ErrInvalidCredential

type IdentityUser = core.IdentityUser

// The credential shapes are core types so the self-service store boundary in
// internal/store can name them without importing this package.
type PersonalAccessToken = core.PersonalAccessToken

type IssuedPersonalAccessToken = core.IssuedPersonalAccessToken

type IssuedAgentCredential = store.IssuedAgentCredential

// GetCallerIdentity resolves only the credential-derived user. A workspace
// role is joined only when the HTTP boundary has supplied an authorized
// workspace context; the join also closes membership-revocation races.
func (s *Store) GetCallerIdentity(ctx context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	var identity core.CallerIdentity
	if workspaceID == "" {
		err := s.boundary.QueryRow(ctx, `SELECT id,email,display_name FROM users WHERE id=$1 AND status='active'`, userID).
			Scan(&identity.ID, &identity.Email, &identity.DisplayName)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.CallerIdentity{}, store.ErrNotFound
		}
		return identity, err
	}
	err := s.boundary.QueryRow(ctx, `SELECT u.id,u.email,u.display_name,b.role
		FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id
		WHERE u.id=$1 AND u.status='active' AND b.workspace_id=$2`, userID, workspaceID).
		Scan(&identity.ID, &identity.Email, &identity.DisplayName, &identity.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.CallerIdentity{}, store.ErrNotFound
	}
	return identity, err
}

// SetOwnDisplayName updates only the active user joined to the verified live
// dashboard session and records the credential-derived self-service act in the
// same transaction (REQ-10/AC-10.8).
func (s *Store) SetOwnDisplayName(ctx context.Context, userID, sessionID, displayName string) (core.CallerIdentity, error) {
	var identity core.CallerIdentity
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := tx.QueryRow(ctx, `UPDATE users u SET display_name=$3
			FROM dashboard_sessions s
			WHERE u.id=$1 AND u.status='active' AND s.id=$2 AND s.user_id=u.id
				AND s.revoked_at IS NULL AND s.expires_at>now()
			RETURNING u.id,u.email,u.display_name`, userID, sessionID, displayName).
			Scan(&identity.ID, &identity.Email, &identity.DisplayName); err != nil {
			return notFound(err, "dashboard session")
		}
		actor := store.ActorFromContext(ctx)
		return insertDeploymentEvent(ctx, q, db.InsertDeploymentEventParams{
			Kind: "identity.display_name_changed", ActorID: actor.ID, ActorRole: string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"user_id": userID, "session_id": sessionID}),
			At:          pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		})
	})
	if errors.Is(err, store.ErrNotFound) {
		return core.CallerIdentity{}, core.ErrInvalidCredential
	}
	return identity, err
}

// BootstrapIdentity maps the configured deployment token to the
// instance-administration principal, the owner of the sole
// deployment_credential marker. The advisory lock makes rotation idempotent
// and serializes first-workspace seeding. A live marker keeps its owner
// across restart, configuration change, and rotation; without a marker the
// configured first operator becomes the owner. Bootstrap never writes a
// workspace binding, so demotions and exclusions survive every restart
// (DEC-63(3), DEC-63(4)).
func (s *Store) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, legacyToken string) (bool, error) {
	identity.Email = strings.ToLower(strings.TrimSpace(identity.Email))
	identity.DisplayName = strings.TrimSpace(identity.DisplayName)
	identity.OrganizationName = strings.TrimSpace(identity.OrganizationName)
	address, err := mail.ParseAddress(identity.Email)
	if err != nil || address.Address != identity.Email || !strings.Contains(identity.Email, "@") {
		return false, errors.New("first operator email must be a valid email address")
	}
	if identity.DisplayName == "" || identity.OrganizationName == "" {
		return false, errors.New("organization name and first operator display name are required")
	}
	if legacyToken == "" {
		return false, errors.New("legacy API token is required for identity bootstrap")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('conveyor:identity-bootstrap'))"); err != nil {
		return false, fmt.Errorf("lock identity bootstrap: %w", err)
	}
	if err := store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapLocked); err != nil {
		return false, err
	}
	q := s.queries.WithTx(tx)
	hash := sha256.Sum256([]byte(legacyToken))
	var legacy struct {
		id, userID, status, kind, scope string
		tokenHash                       []byte
		revoked                         pgtype.Timestamptz
	}
	legacyErr := tx.QueryRow(ctx, `SELECT t.id,t.user_id,t.token_hash,t.kind,t.scope,t.revoked_at,u.status
		FROM user_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.deployment_credential FOR UPDATE OF t`).Scan(
		&legacy.id, &legacy.userID, &legacy.tokenHash, &legacy.kind, &legacy.scope, &legacy.revoked, &legacy.status,
	)
	if legacyErr != nil && !errors.Is(legacyErr, pgx.ErrNoRows) {
		return false, fmt.Errorf("read legacy API token mapping: %w", legacyErr)
	}
	marked := legacyErr == nil
	sameHash := marked && subtle.ConstantTimeCompare(legacy.tokenHash, hash[:]) == 1
	if marked && legacy.revoked.Valid && sameHash {
		return false, errors.New("legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT")
	}
	if marked && legacy.status != "active" {
		// An inactive owner fails closed; no other operator replaces it.
		return false, errors.New("deployment owner account is deactivated; reactivate it before startup")
	}
	commit := func() (bool, error) {
		if err := store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapBeforeCommit); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit identity bootstrap: %w", err)
		}
		return true, nil
	}
	if marked && !legacy.revoked.Valid {
		if sameHash && legacy.kind == string(core.CredentialUser) && legacy.scope == string(core.CredentialScopeOperator) {
			if err := store.RunIdentityTestHook(ctx, store.IdentityHookBootstrapBeforeCommit); err != nil {
				return false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
		if sameHash {
			if _, err := tx.Exec(ctx, `UPDATE user_tokens
				SET kind='user',scope='operator'
				WHERE id=$1`, legacy.id); err != nil {
				return false, fmt.Errorf("repair legacy API token mapping: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `UPDATE user_tokens
			SET token_hash=$1,kind='user',scope='operator',last_used_at=NULL
			WHERE id=$2 AND revoked_at IS NULL`, hash[:], legacy.id); err != nil {
			return false, fmt.Errorf("rotate legacy API token mapping: %w", err)
		}
		if err := auditLegacyTokenLifecycle(ctx, q, legacy.id, "identity.legacy_token_rotated"); err != nil {
			return false, err
		}
		return commit()
	}

	ownerID := legacy.userID
	if marked {
		// A revoked marker keeps its owner; the new deployment token is
		// reissued to the same account (DEC-63(3)).
		if _, err := tx.Exec(ctx, `UPDATE user_tokens
			SET label='retired legacy API token',deployment_credential=false
			WHERE id=$1`, legacy.id); err != nil {
			return false, fmt.Errorf("retire revoked legacy API token mapping: %w", err)
		}
	} else {
		row, err := provisionIdentityUserInTx(ctx, tx, q, identity.Email, identity.DisplayName, false)
		if err != nil {
			if err.Error() == "provisioned account is deactivated" {
				return false, errors.New("configured first operator account is deactivated")
			}
			return false, fmt.Errorf("seed first operator: %w", err)
		}
		ownerID = row.ID
		if _, err := tx.Exec(ctx, `UPDATE orgs SET name=$1 WHERE singleton=true AND name='Conveyor'`, identity.OrganizationName); err != nil {
			return false, fmt.Errorf("configure deployment organization: %w", err)
		}
	}
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return false, err
	}
	if _, err := q.InsertDeploymentCredential(ctx, db.InsertDeploymentCredentialParams{
		ID: tokenID, UserID: ownerID, Label: "legacy API token", TokenHash: hash[:],
		Kind: string(core.CredentialUser), Scope: string(core.CredentialScopeOperator),
	}); err != nil {
		return false, fmt.Errorf("map legacy API token: %w", err)
	}
	if marked {
		if err := auditLegacyTokenLifecycle(ctx, q, tokenID, "identity.legacy_token_rotated"); err != nil {
			return false, err
		}
	}
	return commit()
}

// ProvisionIdentityUser creates or resolves one normalized account and redeems
// all of its pending workspace invitations in the same transaction. This is an
// instance-administration seam only; it is intentionally not exposed as a
// self-registration HTTP route (REQ-1/AC-1.2).
func (s *Store) ProvisionIdentityUser(ctx context.Context, email, displayName string) (IdentityUser, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return IdentityUser{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return IdentityUser{}, errors.New("display name is required")
	}
	var result db.User
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var provisionErr error
		result, provisionErr = provisionIdentityUserInTx(ctx, tx, q, email, displayName, false)
		return provisionErr
	})
	if err != nil {
		return IdentityUser{}, err
	}
	return identityUser(result), nil
}

// provisionIdentityUserInTx resolves or creates the account and consumes its
// pending invitations. byLink selects sign-in link redemption, whose grants are
// attributed to the redeemed account rather than to the inviter
// (req-accounts-and-membership AC-3.1).
func provisionIdentityUserInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, email, displayName string, byLink bool) (db.User, error) {
	if err := lockIdentityEmail(ctx, tx, email); err != nil {
		return db.User{}, err
	}
	var row db.User
	err := tx.QueryRow(ctx, `SELECT id,email,display_name,status,created_at FROM users WHERE email=$1 FOR UPDATE`, email).Scan(
		&row.ID, &row.Email, &row.DisplayName, &row.Status, &row.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		userID, idErr := randomIdentityID("usr", 12)
		if idErr != nil {
			return db.User{}, idErr
		}
		row, err = q.InsertIdentityUser(ctx, db.InsertIdentityUserParams{ID: userID, Email: email, DisplayName: displayName})
	}
	if err != nil {
		return db.User{}, err
	}
	if row.Status != "active" {
		return db.User{}, errors.New("provisioned account is deactivated")
	}
	if err := redeemWorkspaceInvitations(ctx, tx, q, row, byLink); err != nil {
		return db.User{}, err
	}
	return row, nil
}

func redeemWorkspaceInvitations(ctx context.Context, tx pgx.Tx, q *db.Queries, user db.User, byLink bool) error {
	rows, err := tx.Query(ctx, `SELECT workspace_id,role,invited_by
		FROM workspace_membership_invitations WHERE email=$1 ORDER BY workspace_id FOR UPDATE`, user.Email)
	if err != nil {
		return fmt.Errorf("list pending workspace invitations: %w", err)
	}
	type invitation struct {
		workspaceID string
		role        core.WorkspaceRole
		invitedBy   string
	}
	var pending []invitation
	for rows.Next() {
		var item invitation
		if err := rows.Scan(&item.workspaceID, &item.role, &item.invitedBy); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range pending {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role)
			VALUES($1,$2,$3) ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=excluded.role,updated_at=now()`,
			item.workspaceID, user.ID, item.role); err != nil {
			return fmt.Errorf("redeem workspace invitation binding: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_membership_invitations
			WHERE workspace_id=$1 AND email=$2`, item.workspaceID, user.Email); err != nil {
			return fmt.Errorf("consume workspace invitation: %w", err)
		}
		eventCtx := store.WithWorkspace(ctx, item.workspaceID)
		actorID := store.UserActorID(item.invitedBy)
		payload := map[string]any{
			"workspace_id": item.workspaceID, "user_id": user.ID, "email": user.Email, "role": item.role,
			"invitation": false, "redemption": true, "granted_by": item.invitedBy,
		}
		if byLink {
			actorID = store.UserActorID(user.ID)
			payload["invited_by"] = item.invitedBy
		}
		// The redemption establishes its own attributed actor, so the event
		// context binds it explicitly (component-persistence, Actor context).
		eventCtx = store.WithActor(eventCtx, store.Actor{ID: actorID, Role: core.ActorUser})
		if err := insertWorkspaceEvent(eventCtx, q, core.Event{
			Kind: "workspace.membership_granted", ActorID: actorID, ActorRole: core.ActorUser,
			Payload: core.JSONPayload(payload),
		}); err != nil {
			return fmt.Errorf("audit workspace invitation redemption: %w", err)
		}
	}
	return nil
}

func normalizeIdentityEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("email must be a valid normalized address")
	}
	return email, nil
}

func lockIdentityEmail(ctx context.Context, tx pgx.Tx, email string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('conveyor:identity-email:' || $1))`, email); err != nil {
		return fmt.Errorf("lock identity email: %w", err)
	}
	return nil
}

// userActorContext binds the user actor that an identity operation itself
// establishes, such as a sign-in or a session revocation.
func userActorContext(ctx context.Context, userID string) context.Context {
	return store.WithActor(ctx, store.Actor{ID: store.UserActorID(userID), Role: core.ActorUser})
}

func auditLegacyTokenLifecycle(ctx context.Context, q *db.Queries, credentialID, kind string) error {
	if err := insertDeploymentEvent(store.WithActor(ctx, store.SystemActor("system")), q, db.InsertDeploymentEventParams{
		Kind: kind, ActorID: "system", ActorRole: string(core.ActorSystem),
		PayloadJson: core.JSONPayload(map[string]any{"credential_id": credentialID}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("audit legacy API token lifecycle: %w", err)
	}
	return nil
}

func auditPersonalAccessTokenLifecycle(ctx context.Context, q *db.Queries, row db.UserToken, kind string) error {
	actorUserID := row.UserID
	if credential, ok := store.CredentialFromContext(ctx); ok {
		actorUserID = credential.OwnerUserID
	}
	if err := insertDeploymentEvent(store.WithActor(ctx, store.Actor{ID: store.UserActorID(actorUserID), Role: core.ActorUser}), q, db.InsertDeploymentEventParams{
		Kind: kind, ActorID: store.UserActorID(actorUserID), ActorRole: string(core.ActorUser),
		PayloadJson: core.JSONPayload(map[string]any{"credential_id": row.ID, "label": row.Label}),
		At:          pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("audit personal access token lifecycle: %w", err)
	}
	return nil
}

func (s *Store) IssuePersonalAccessToken(ctx context.Context, userID, label string) (IssuedPersonalAccessToken, error) {
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return IssuedPersonalAccessToken{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return IssuedPersonalAccessToken{}, fmt.Errorf("generate personal access token: %w", err)
	}
	value := "cv_pat_" + tokenID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(value))
	var row db.UserToken
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); err != nil {
			return notFound(err, "identity user %s", userID)
		}
		if status != "active" {
			return errors.New("cannot issue a token for a deactivated user")
		}
		scope := core.CredentialScopeUser
		var operator bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_role_bindings WHERE user_id=$1 AND role='operator')`, userID).Scan(&operator); err != nil {
			return err
		}
		if operator {
			scope = core.CredentialScopeOperator
		}
		var insertErr error
		row, insertErr = q.InsertUserToken(ctx, db.InsertUserTokenParams{
			ID: tokenID, UserID: userID, Label: strings.TrimSpace(label), TokenHash: hash[:],
			Kind: string(core.CredentialUser), Scope: string(scope),
		})
		if insertErr != nil {
			return insertErr
		}
		return auditPersonalAccessTokenLifecycle(ctx, q, row, "identity.personal_token_issued")
	})
	if err != nil {
		return IssuedPersonalAccessToken{}, err
	}
	return IssuedPersonalAccessToken{PersonalAccessToken: personalAccessToken(row), Value: value}, nil
}

// IssueAgentCredential creates an execution-only credential under an owning
// user. It reuses user_tokens so ownership remains enforced by the existing
// foreign key and no post-083 schema change is required (REQ-2/AC-2.2).
func (s *Store) IssueAgentCredential(ctx context.Context, userID, label string) (IssuedAgentCredential, error) {
	id, err := randomIdentityID("agt", 12)
	if err != nil {
		return IssuedAgentCredential{}, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return IssuedAgentCredential{}, fmt.Errorf("generate agent credential: %w", err)
	}
	value := "cv_agent_" + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(value))
	var row db.UserToken
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); err != nil {
			return notFound(err, "identity user %s", userID)
		}
		if status != "active" {
			return errors.New("cannot issue a credential for a deactivated user")
		}
		var insertErr error
		row, insertErr = q.InsertUserToken(ctx, db.InsertUserTokenParams{
			ID: id, UserID: userID, Label: strings.TrimSpace(label), TokenHash: hash[:],
			Kind: string(core.CredentialAgent), Scope: string(core.CredentialScopeUser),
		})
		return insertErr
	})
	if err != nil {
		return IssuedAgentCredential{}, err
	}
	return IssuedAgentCredential{ID: row.ID, UserID: row.UserID, Label: row.Label, Value: value}, nil
}

// RevokeAgentCredential revokes only an agent-class credential owned by the
// authenticated run parent. The kind and owner predicates make human PATs and
// credentials belonging to another user unaddressable through this boundary.
func (s *Store) RevokeAgentCredential(ctx context.Context, userID, credentialID string) error {
	result, err := s.boundary.Exec(ctx, `UPDATE user_tokens SET revoked_at=COALESCE(revoked_at, now())
		WHERE id=$1 AND user_id=$2 AND kind=$3`, credentialID, userID, string(core.CredentialAgent))
	if err != nil {
		return fmt.Errorf("revoke agent credential: %w", err)
	}
	if result.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}

// RevokeRunAgentCredential revokes only the credential issued for the exact
// attended-run workspace, work order, and session. The row is locked while
// its persisted label is parsed and matched so a parent cannot use another
// live claim to revoke a sibling child credential owned by the same user.
func (s *Store) RevokeRunAgentCredential(ctx context.Context, userID, credentialID string, expected store.RunAgentCredentialBinding) error {
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var label string
		err := tx.QueryRow(ctx, `SELECT label FROM user_tokens
			WHERE id=$1 AND user_id=$2 AND kind=$3 FOR UPDATE`, credentialID, userID, string(core.CredentialAgent)).Scan(&label)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read run agent credential: %w", err)
		}
		binding, ok := store.ParseRunAgentCredentialLabel(label)
		if !ok || binding != expected {
			return store.ErrRunAgentCredentialBindingMismatch
		}
		if _, err = tx.Exec(ctx, `UPDATE user_tokens SET revoked_at=COALESCE(revoked_at, now()) WHERE id=$1`, credentialID); err != nil {
			return fmt.Errorf("revoke run agent credential: %w", err)
		}
		return nil
	})
}

func (s *Store) VerifyPersonalAccessToken(ctx context.Context, candidate string) (IdentityUser, error) {
	credential, user, err := s.verifyCredential(ctx, candidate)
	if err != nil {
		return IdentityUser{}, err
	}
	if credential.Kind != core.CredentialUser {
		return IdentityUser{}, ErrInvalidPersonalAccessToken
	}
	return user, nil
}

// VerifyCredential resolves identity and scope solely from the persisted
// credential record. The presented secret is never returned or logged.
func (s *Store) VerifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	credential, _, err := s.verifyCredential(ctx, candidate)
	if err == nil && credential.Method == "" {
		credential.Method = core.CredentialMethodBearer
	}
	return credential, err
}

const (
	signInLinkLifetime       = 30 * time.Minute
	dashboardSessionLifetime = 7 * 24 * time.Hour
)

// IssueSignInLink rotates prior unredeemed links and only succeeds for an
// existing account or pending invitation. That predicate is the no-self-
// registration boundary. It serves only host-local issuance
// (req-invitations-and-sign-in REQ-3).
func (s *Store) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	return s.issueSignInLink(ctx, email, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1 AND status='active') OR EXISTS(SELECT 1 FROM workspace_membership_invitations WHERE email=$1)`, email).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return store.ErrNotFound
		}
		return nil
	}, nil)
}

// IssueInvitationSignInLink issues a link only while workspaceID holds a
// pending invitation for email. The check, rotation, insert, and the resend
// audit event share one transaction under the identity email lock, which
// revocation and redemption also take (component-identity-membership).
func (s *Store) IssueInvitationSignInLink(ctx context.Context, workspaceID, email string, purpose store.SignInLinkPurpose) (core.IssuedSignInLink, error) {
	if !purpose.Valid() {
		return core.IssuedSignInLink{}, errors.New("invalid sign-in link purpose")
	}
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IssuedSignInLink{}, fmt.Errorf("%w: workspace membership invitation", store.ErrNotFound)
	}
	var resentBy string
	if purpose == store.SignInLinkResend {
		credential, ok := store.CredentialFromContext(ctx)
		if !ok || credential.OwnerUserID == "" {
			return core.IssuedSignInLink{}, errors.New("authenticated user credential is required")
		}
		resentBy = credential.OwnerUserID
	}
	return s.issueSignInLink(ctx, email, func(tx pgx.Tx) error {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2)`, workspaceID, email).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return fmt.Errorf("%w: workspace membership invitation", store.ErrNotFound)
		}
		return store.RunIdentityTestHook(ctx, store.IdentityHookInvitationLinkLocked)
	}, func(q *db.Queries) error {
		if purpose != store.SignInLinkResend {
			return nil
		}
		if err := store.RunIdentityTestHook(ctx, store.IdentityHookInvitationResentAudit); err != nil {
			return err
		}
		return insertWorkspaceEvent(store.WithWorkspace(ctx, workspaceID), q, core.Event{
			Kind: "workspace.invitation_resent", ActorID: store.UserActorID(resentBy), ActorRole: core.ActorUser,
			Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "email": email, "resent_by": resentBy}),
		})
	})
}

// issueSignInLink holds the identity email lock, runs admit, rotates earlier
// unredeemed links, stores the new hash, appends identity.signin_link_issued,
// and then runs audit, all in one transaction.
func (s *Store) issueSignInLink(ctx context.Context, email string, admit func(pgx.Tx) error, audit func(*db.Queries) error) (core.IssuedSignInLink, error) {
	id, err := randomIdentityID("sil", 12)
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return core.IssuedSignInLink{}, err
	}
	value := "cv_signin_" + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(value))
	expires := time.Now().UTC().Add(signInLinkLifetime)
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockIdentityEmail(ctx, tx, email); err != nil {
			return err
		}
		if err := admit(tx); err != nil {
			return err
		}
		var userID *string
		var uid string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email=$1 AND status='active'`, email).Scan(&uid); err == nil {
			userID = &uid
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invitation_signin_tokens SET redeemed_at=COALESCE(redeemed_at,now()) WHERE email=$1 AND redeemed_at IS NULL`, email); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO invitation_signin_tokens(id,email,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, id, email, userID, hash[:], expires); err != nil {
			return err
		}
		actor := store.ActorFromContext(ctx)
		if err := insertDeploymentEvent(ctx, q, db.InsertDeploymentEventParams{Kind: "identity.signin_link_issued", ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]any{"signin_link_id": id, "email": email}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}); err != nil {
			return err
		}
		if audit != nil {
			return audit(q)
		}
		return nil
	})
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	return core.IssuedSignInLink{Email: email, Value: value, ExpiresAt: expires}, nil
}

// RedeemSignInLink consumes the link and creates its browser session in one
// transaction. Concurrent or repeated redemption therefore has one winner.
func (s *Store) RedeemSignInLink(ctx context.Context, candidate string) (core.DashboardSession, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	var session core.DashboardSession
	var user core.IdentityUser
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var linkID, email string
		var userID *string
		// Take the email lock before any token-row lock, the same order
		// issuance uses, so redemption cannot deadlock against a concurrent
		// issuance or resend. The conditional UPDATE still decides the winner.
		if err := tx.QueryRow(ctx, `SELECT email FROM invitation_signin_tokens WHERE token_hash=$1`, hash[:]).Scan(&email); err != nil {
			return notFound(err, "sign-in link")
		}
		if err := lockIdentityEmail(ctx, tx, email); err != nil {
			return err
		}
		if err := store.RunIdentityTestHook(ctx, store.IdentityHookSignInRedeemLocked); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE invitation_signin_tokens SET redeemed_at=now() WHERE token_hash=$1 AND redeemed_at IS NULL AND expires_at>now() RETURNING id,email,user_id`, hash[:]).Scan(&linkID, &email, &userID); err != nil {
			return notFound(err, "sign-in link")
		}
		if userID == nil {
			row, err := provisionIdentityUserInTx(ctx, tx, q, email, email, true)
			if err != nil {
				return err
			}
			uid := row.ID
			userID = &uid
			if _, err = tx.Exec(ctx, `UPDATE invitation_signin_tokens SET user_id=$1 WHERE id=$2`, uid, linkID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT id,email,display_name,status,created_at FROM users WHERE id=$1 AND status='active'`, *userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.CreatedAt); err != nil {
			return err
		}
		var mintErr error
		session, mintErr = mintDashboardSession(ctx, tx, *userID, true)
		if mintErr != nil {
			return mintErr
		}
		at := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		if eventErr := insertDeploymentEvent(userActorContext(ctx, *userID), q, db.InsertDeploymentEventParams{Kind: "identity.signin_link_redeemed", ActorID: store.UserActorID(*userID), ActorRole: string(core.ActorUser), PayloadJson: core.JSONPayload(map[string]any{"signin_link_id": linkID}), At: at}); eventErr != nil {
			return eventErr
		}
		return insertDeploymentEvent(userActorContext(ctx, *userID), q, db.InsertDeploymentEventParams{Kind: "identity.dashboard_session_created", ActorID: store.UserActorID(*userID), ActorRole: string(core.ActorUser), PayloadJson: core.JSONPayload(map[string]any{"session_id": session.ID}), At: at})
	})
	if err != nil {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	return session, user, nil
}

// SignInWithPassword never provisions an account. Missing, deactivated, and
// passwordless accounts all take the same Argon2id path and return the same
// credential error as an incorrect password (REQ-10/AC-10.2, AC-10.5).
func (s *Store) SignInWithPassword(ctx context.Context, email, password string) (core.DashboardSession, core.IdentityUser, error) {
	normalized, normalizeErr := normalizeIdentityEmail(email)
	dummyHash := fixedDummyPasswordHash()
	encoded := dummyHash
	var candidateID string
	if normalizeErr == nil {
		var stored *string
		if err := s.boundary.QueryRow(ctx, `SELECT id,password_hash FROM users WHERE email=$1 AND status='active'`, normalized).Scan(&candidateID, &stored); err == nil && stored != nil {
			encoded = *stored
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return core.DashboardSession{}, core.IdentityUser{}, err
		}
	}
	if !verifyPassword(encoded, password) || encoded == dummyHash {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}

	var session core.DashboardSession
	var user core.IdentityUser
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current *string
		if err := tx.QueryRow(ctx, `SELECT id,email,display_name,status,created_at,password_hash FROM users WHERE id=$1 AND status='active' FOR UPDATE`, candidateID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.CreatedAt, &current); err != nil {
			return err
		}
		if current == nil || subtle.ConstantTimeCompare([]byte(*current), []byte(encoded)) != 1 {
			return core.ErrInvalidCredential
		}
		var err error
		session, err = mintDashboardSession(ctx, tx, user.ID, false)
		if err != nil {
			return err
		}
		return insertDeploymentEvent(userActorContext(ctx, user.ID), q, db.InsertDeploymentEventParams{Kind: "identity.dashboard_session_created", ActorID: store.UserActorID(user.ID), ActorRole: string(core.ActorUser), PayloadJson: core.JSONPayload(map[string]any{"session_id": session.ID, "method": "password"}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, core.ErrInvalidCredential) {
			return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
		}
		return core.DashboardSession{}, core.IdentityUser{}, err
	}
	return session, user, nil
}

// SetOwnPassword binds both the user and session IDs supplied by the verified
// cookie. Link-established sessions authorize recovery; every other session
// must prove the current password when one is already present (AC-10.4-10.6).
func (s *Store) SetOwnPassword(ctx context.Context, userID, sessionID, currentPassword, newPassword string) error {
	if !validNewPassword(newPassword) {
		return store.ErrInvalidPassword
	}
	encoded, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var existing *string
		var establishedByLink bool
		if err := tx.QueryRow(ctx, `SELECT u.password_hash,s.established_by_link
			FROM users u JOIN dashboard_sessions s ON s.user_id=u.id
			WHERE u.id=$1 AND u.status='active' AND s.id=$2 AND s.revoked_at IS NULL AND s.expires_at>now()
			FOR UPDATE OF u,s`, userID, sessionID).Scan(&existing, &establishedByLink); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return core.ErrInvalidCredential
			}
			return err
		}
		if existing != nil && !establishedByLink && !verifyPassword(*existing, currentPassword) {
			return store.ErrInvalidCurrentPassword
		}
		kind := "identity.password_set"
		if existing != nil {
			kind = "identity.password_changed"
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, userID, encoded); err != nil {
			return err
		}
		return insertDeploymentEvent(userActorContext(ctx, userID), q, db.InsertDeploymentEventParams{Kind: kind, ActorID: store.UserActorID(userID), ActorRole: string(core.ActorUser), PayloadJson: core.JSONPayload(map[string]any{"session_id": sessionID}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
	})
}

func mintDashboardSession(ctx context.Context, tx pgx.Tx, userID string, establishedByLink bool) (core.DashboardSession, error) {
	sid, err := randomIdentityID("ses", 12)
	if err != nil {
		return core.DashboardSession{}, err
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return core.DashboardSession{}, err
	}
	value := "cv_session_" + sid + "_" + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(value))
	expires := time.Now().UTC().Add(dashboardSessionLifetime)
	if _, err = tx.Exec(ctx, `INSERT INTO dashboard_sessions(id,user_id,session_hash,expires_at,established_by_link) VALUES($1,$2,$3,$4,$5)`, sid, userID, hash[:], expires, establishedByLink); err != nil {
		return core.DashboardSession{}, err
	}
	return core.DashboardSession{ID: sid, UserID: userID, Value: value, ExpiresAt: expires}, nil
}

func (s *Store) VerifyDashboardSession(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	hash := sha256.Sum256([]byte(candidate))
	var id, userID string
	var expiresAt time.Time
	var establishedByLink bool
	if err := s.boundary.QueryRow(ctx, `UPDATE dashboard_sessions s SET last_used_at=now(),expires_at=now()+interval '7 days' FROM users u WHERE s.session_hash=$1 AND s.user_id=u.id AND s.revoked_at IS NULL AND s.expires_at>now() AND u.status='active' RETURNING s.id,s.user_id,s.expires_at,s.established_by_link`, hash[:]).Scan(&id, &userID, &expiresAt, &establishedByLink); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.AuthenticatedCredential{}, core.ErrInvalidCredential
		}
		return core.AuthenticatedCredential{}, err
	}
	var operator bool
	if err := s.boundary.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_role_bindings WHERE user_id=$1 AND role='operator')`, userID).Scan(&operator); err != nil {
		return core.AuthenticatedCredential{}, err
	}
	scope := core.CredentialScopeUser
	if operator {
		scope = core.CredentialScopeOperator
	}
	return core.AuthenticatedCredential{ID: id, OwnerUserID: userID, Kind: core.CredentialUser, Scope: scope, Method: core.CredentialMethodSession, SessionExpiresAt: expiresAt, SessionEstablishedByLink: establishedByLink}, nil
}

func (s *Store) RevokeDashboardSession(ctx context.Context, userID, sessionID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		result, err := tx.Exec(ctx, `UPDATE dashboard_sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, sessionID, userID)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return store.ErrNotFound
		}
		return insertDeploymentEvent(userActorContext(ctx, userID), q, db.InsertDeploymentEventParams{Kind: "identity.dashboard_session_revoked", ActorID: store.UserActorID(userID), ActorRole: string(core.ActorUser), PayloadJson: core.JSONPayload(map[string]any{"session_id": sessionID}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
	})
}

func (s *Store) RecordInvitationDelivery(ctx context.Context, email, outcome string) error {
	if outcome != "sent" && outcome != "failed" && outcome != "fallback" {
		return errors.New("invalid invitation delivery outcome")
	}
	actor := store.ActorFromContext(ctx)
	return insertDeploymentEvent(ctx, s.queries, db.InsertDeploymentEventParams{
		Kind: "identity.invitation_delivery_" + outcome, ActorID: actor.ID, ActorRole: string(actor.Role),
		PayloadJson: core.JSONPayload(map[string]any{"email": email}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
}

func (s *Store) verifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	row, err := s.queries.GetUserTokenByHash(ctx, hash[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.AuthenticatedCredential{}, IdentityUser{}, ErrInvalidPersonalAccessToken
		}
		return core.AuthenticatedCredential{}, IdentityUser{}, err
	}
	if subtle.ConstantTimeCompare(row.TokenHash, hash[:]) != 1 || row.RevokedAt.Valid || row.Status != "active" {
		return core.AuthenticatedCredential{}, IdentityUser{}, ErrInvalidPersonalAccessToken
	}
	used, err := s.queries.MarkUserTokenUsed(ctx, db.MarkUserTokenUsedParams{ID: row.ID, TokenHash: hash[:]})
	if err != nil {
		return core.AuthenticatedCredential{}, IdentityUser{}, err
	}
	if used != 1 {
		return core.AuthenticatedCredential{}, IdentityUser{}, ErrInvalidPersonalAccessToken
	}
	kind, scope := core.CredentialKind(row.Kind), core.CredentialScope(row.Scope)
	credential := core.AuthenticatedCredential{ID: row.ID, OwnerUserID: row.UserID, Kind: kind, Scope: scope}
	if kind == core.CredentialAgent {
		if binding, ok := store.ParseRunAgentCredentialLabel(row.Label); ok {
			credential.RunWorkspaceID = binding.WorkspaceID
			credential.RunWorkOrderID = binding.WorkOrderID
			credential.RunSessionID = binding.SessionID
		}
	}
	user := IdentityUser{ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status}
	return credential, user, nil
}

func (s *Store) RevokePersonalAccessToken(ctx context.Context, tokenID string) (PersonalAccessToken, error) {
	var row db.UserToken
	err := s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		var revokeErr error
		row, revokeErr = q.RevokeUserToken(ctx, tokenID)
		if revokeErr != nil {
			return revokeErr
		}
		return auditPersonalAccessTokenLifecycle(ctx, q, row, "identity.personal_token_revoked")
	})
	if err != nil {
		return PersonalAccessToken{}, notFound(err, "personal access token %s", tokenID)
	}
	return personalAccessToken(row), nil
}

// ListOwnPersonalAccessTokens returns the caller's human credentials without
// their values: the query selects no hash column, and the cleartext value only
// ever existed in the issuance response (REQ-2, req-security-boundaries AC-2.1).
func (s *Store) ListOwnPersonalAccessTokens(ctx context.Context, userID string) ([]PersonalAccessToken, error) {
	rows, err := s.queries.ListOwnUserTokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	items := make([]PersonalAccessToken, 0, len(rows))
	for _, row := range rows {
		items = append(items, listedPersonalAccessToken(row))
	}
	return items, nil
}

// IssueOwnPersonalAccessToken mints a credential for the caller through the
// existing opaque-hash issuer; self-service adds no second issuance path.
func (s *Store) IssueOwnPersonalAccessToken(ctx context.Context, userID, label string) (IssuedPersonalAccessToken, error) {
	return s.IssuePersonalAccessToken(ctx, userID, label)
}

// RevokeOwnPersonalAccessToken revokes a credential the caller owns. Ownership
// is a predicate of the statement rather than a check around it, so another
// user's token is reported exactly like a token that does not exist.
func (s *Store) RevokeOwnPersonalAccessToken(ctx context.Context, userID, tokenID string) (PersonalAccessToken, error) {
	var row db.UserToken
	err := s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		var revokeErr error
		row, revokeErr = q.RevokeOwnUserToken(ctx, db.RevokeOwnUserTokenParams{ID: tokenID, UserID: userID})
		if revokeErr != nil {
			return revokeErr
		}
		return auditPersonalAccessTokenLifecycle(ctx, q, row, "identity.personal_token_revoked")
	})
	if err != nil {
		return PersonalAccessToken{}, notFound(err, "personal access token %s", tokenID)
	}
	return personalAccessToken(row), nil
}

func (s *Store) DeactivateIdentityUser(ctx context.Context, userID string) (IdentityUser, error) {
	var row db.User
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := tx.QueryRow(ctx, `UPDATE users SET status='deactivated' WHERE id=$1
			RETURNING id,email,display_name,status,created_at`, userID).Scan(
			&row.ID, &row.Email, &row.DisplayName, &row.Status, &row.CreatedAt,
		); err != nil {
			return err
		}
		return revokeOwnedWorkersTx(ctx, tx, q, userID, "", "identity_user_deactivated")
	})
	if err != nil {
		return IdentityUser{}, notFound(err, "identity user %s", userID)
	}
	return identityUser(row), nil
}

func randomIdentityID(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s identity: %w", prefix, err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func identityUser(row db.User) IdentityUser {
	return IdentityUser{ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status, CreatedAt: row.CreatedAt.Time}
}

func personalAccessToken(row db.UserToken) PersonalAccessToken {
	return credentialLifecycle(
		PersonalAccessToken{ID: row.ID, UserID: row.UserID, Label: row.Label, CreatedAt: row.CreatedAt.Time},
		row.LastUsedAt, row.RevokedAt,
	)
}

func listedPersonalAccessToken(row db.ListOwnUserTokensRow) PersonalAccessToken {
	return credentialLifecycle(
		PersonalAccessToken{
			ID: row.ID, UserID: row.UserID, Label: row.Label,
			DeploymentCredential: row.DeploymentCredential, CreatedAt: row.CreatedAt.Time,
		},
		row.LastUsedAt, row.RevokedAt,
	)
}

func credentialLifecycle(item PersonalAccessToken, lastUsedAt, revokedAt pgtype.Timestamptz) PersonalAccessToken {
	if lastUsedAt.Valid {
		value := lastUsedAt.Time
		item.LastUsedAt = &value
	}
	if revokedAt.Valid {
		value := revokedAt.Time
		item.RevokedAt = &value
	}
	return item
}
