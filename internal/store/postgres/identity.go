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

type IssuedAgentCredential struct {
	ID     string
	UserID string
	Label  string
	Value  string
}

// GetCallerIdentity resolves only the credential-derived user. A workspace
// role is joined only when the HTTP boundary has supplied an authorized
// workspace context; the join also closes membership-revocation races.
func (s *Store) GetCallerIdentity(ctx context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	var identity core.CallerIdentity
	if workspaceID == "" {
		err := s.pool.QueryRow(ctx, `SELECT id,email,display_name FROM users WHERE id=$1 AND status='active'`, userID).
			Scan(&identity.ID, &identity.Email, &identity.DisplayName)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.CallerIdentity{}, store.ErrNotFound
		}
		return identity, err
	}
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.email,u.display_name,b.role
		FROM users u JOIN workspace_role_bindings b ON b.user_id=u.id
		WHERE u.id=$1 AND u.status='active' AND b.workspace_id=$2`, userID, workspaceID).
		Scan(&identity.ID, &identity.Email, &identity.DisplayName, &identity.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.CallerIdentity{}, store.ErrNotFound
	}
	return identity, err
}

// BootstrapIdentity ensures that the configured legacy token maps to a usable
// operator. The advisory lock makes upgrade recovery and rotation idempotent.
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('conveyor:identity-bootstrap'))"); err != nil {
		return false, fmt.Errorf("lock identity bootstrap: %w", err)
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
		WHERE t.label='legacy API token' AND t.kind='user'`).Scan(
		&legacy.id, &legacy.userID, &legacy.tokenHash, &legacy.kind, &legacy.scope, &legacy.revoked, &legacy.status,
	)
	if legacyErr != nil && !errors.Is(legacyErr, pgx.ErrNoRows) {
		return false, fmt.Errorf("read legacy API token mapping: %w", legacyErr)
	}
	if legacyErr == nil && legacy.revoked.Valid && subtle.ConstantTimeCompare(legacy.tokenHash, hash[:]) == 1 {
		return false, errors.New("legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT")
	}
	var legacyCoversWorkspaces bool
	if legacyErr == nil {
		if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (
			SELECT 1 FROM workspaces w
			WHERE NOT EXISTS (
				SELECT 1 FROM workspace_role_bindings b
				WHERE b.workspace_id=w.id AND b.user_id=$1 AND b.role='operator'
			)
		)`, legacy.userID).Scan(&legacyCoversWorkspaces); err != nil {
			return false, fmt.Errorf("check legacy operator workspace bindings: %w", err)
		}
	}
	var usableOperator bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.kind='user' AND t.scope='operator' AND t.revoked_at IS NULL AND u.status='active'
		  AND EXISTS (SELECT 1 FROM workspace_role_bindings b WHERE b.user_id=t.user_id AND b.role='operator')
	)`).Scan(&usableOperator); err != nil {
		return false, fmt.Errorf("check usable operator credential: %w", err)
	}
	if legacyCoversWorkspaces && legacyErr == nil && subtle.ConstantTimeCompare(legacy.tokenHash, hash[:]) == 1 &&
		legacy.status == "active" && !legacy.revoked.Valid && legacy.scope == string(core.CredentialScopeOperator) {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}

	if legacyErr == nil && legacy.status == "active" && !legacy.revoked.Valid {
		sameHash := subtle.ConstantTimeCompare(legacy.tokenHash, hash[:]) == 1
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
		if err := seedOperatorBindings(ctx, tx, legacy.userID); err != nil {
			return false, err
		}
		auditKind := "identity.legacy_token_rotated"
		if sameHash {
			auditKind = "identity.legacy_bindings_healed"
		}
		if err := auditLegacyTokenLifecycle(ctx, q, legacy.id, auditKind); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit legacy API token rotation: %w", err)
		}
		return true, nil
	}

	var owner IdentityUser
	if usableOperator {
		row := tx.QueryRow(ctx, `SELECT u.id,u.email,u.display_name,u.status,u.created_at
			FROM users u JOIN user_tokens t ON t.user_id=u.id
			WHERE t.kind='user' AND t.scope='operator' AND t.revoked_at IS NULL AND u.status='active'
			  AND EXISTS (SELECT 1 FROM workspace_role_bindings b WHERE b.user_id=u.id AND b.role='operator')
			ORDER BY t.created_at,t.id LIMIT 1 FOR UPDATE OF u`)
		var createdAt time.Time
		if err := row.Scan(&owner.ID, &owner.Email, &owner.DisplayName, &owner.Status, &createdAt); err != nil {
			return false, fmt.Errorf("select usable operator: %w", err)
		}
		owner.CreatedAt = createdAt
	} else {
		row, err := provisionIdentityUserInTx(ctx, tx, q, identity.Email, identity.DisplayName)
		if err != nil {
			return false, fmt.Errorf("seed first operator: %w", err)
		}
		owner = identityUser(row)
		if owner.Status != "active" {
			return false, errors.New("configured first operator account is deactivated")
		}
		if _, err := tx.Exec(ctx, `UPDATE orgs SET name=$1 WHERE singleton=true AND name='Conveyor'`, identity.OrganizationName); err != nil {
			return false, fmt.Errorf("configure deployment organization: %w", err)
		}
	}

	if legacyErr == nil {
		if _, err := tx.Exec(ctx, `UPDATE user_tokens SET label='retired legacy API token' WHERE id=$1`, legacy.id); err != nil {
			return false, fmt.Errorf("retire unusable legacy API token mapping: %w", err)
		}
	}
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return false, err
	}
	if _, err := q.InsertUserToken(ctx, db.InsertUserTokenParams{
		ID: tokenID, UserID: owner.ID, Label: "legacy API token", TokenHash: hash[:],
		Kind: string(core.CredentialUser), Scope: string(core.CredentialScopeOperator),
	}); err != nil {
		return false, fmt.Errorf("map legacy API token: %w", err)
	}
	if legacyErr == nil {
		if err := auditLegacyTokenLifecycle(ctx, q, tokenID, "identity.legacy_token_rotated"); err != nil {
			return false, err
		}
	}
	if err := seedOperatorBindings(ctx, tx, owner.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit identity bootstrap: %w", err)
	}
	return true, nil
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
		result, provisionErr = provisionIdentityUserInTx(ctx, tx, q, email, displayName)
		return provisionErr
	})
	if err != nil {
		return IdentityUser{}, err
	}
	return identityUser(result), nil
}

func provisionIdentityUserInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, email, displayName string) (db.User, error) {
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
	if err := redeemWorkspaceInvitations(ctx, tx, q, row); err != nil {
		return db.User{}, err
	}
	return row, nil
}

func redeemWorkspaceInvitations(ctx context.Context, tx pgx.Tx, q *db.Queries, user db.User) error {
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
		if err := insertWorkspaceEvent(eventCtx, q, core.Event{
			Kind: "workspace.membership_granted", ActorID: store.UserActorID(item.invitedBy), ActorRole: core.ActorUser,
			Payload: core.JSONPayload(map[string]any{
				"workspace_id": item.workspaceID, "user_id": user.ID, "email": user.Email, "role": item.role,
				"invitation": false, "redemption": true, "granted_by": item.invitedBy,
			}),
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

func seedOperatorBindings(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role)
		SELECT id,$1,'operator' FROM workspaces
		ON CONFLICT(workspace_id,user_id) DO UPDATE SET role='operator',updated_at=now()`, userID); err != nil {
		return fmt.Errorf("seed legacy workspace memberships: %w", err)
	}
	return nil
}

func auditLegacyTokenLifecycle(ctx context.Context, q *db.Queries, credentialID, kind string) error {
	if err := q.InsertDeploymentEvent(ctx, db.InsertDeploymentEventParams{
		Kind: kind, ActorID: "system", ActorRole: string(core.ActorSystem),
		PayloadJson: core.JSONPayload(map[string]any{"credential_id": credentialID}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("audit legacy API token lifecycle: %w", err)
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
		return insertErr
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
	return credential, err
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
	user := IdentityUser{ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status}
	return credential, user, nil
}

func (s *Store) RevokePersonalAccessToken(ctx context.Context, tokenID string) (PersonalAccessToken, error) {
	row, err := s.queries.RevokeUserToken(ctx, tokenID)
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
	row, err := s.queries.RevokeOwnUserToken(ctx, db.RevokeOwnUserTokenParams{ID: tokenID, UserID: userID})
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
		PersonalAccessToken{ID: row.ID, UserID: row.UserID, Label: row.Label, CreatedAt: row.CreatedAt.Time},
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
