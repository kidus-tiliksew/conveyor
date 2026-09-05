package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Volatile identity state is used only by the explicitly selected test backend.

type identityUser struct {
	core.IdentityUser
	PasswordHash string `json:"password_hash,omitempty"`
}

type identityCredential struct {
	ID                   string               `json:"id"`
	UserID               string               `json:"user_id"`
	Label                string               `json:"label"`
	TokenHash            []byte               `json:"token_hash"`
	Kind                 core.CredentialKind  `json:"kind"`
	Scope                core.CredentialScope `json:"scope"`
	DeploymentCredential bool                 `json:"deployment_credential"`
	CreatedAt            time.Time            `json:"created_at"`
	LastUsedAt           *time.Time           `json:"last_used_at,omitempty"`
	RevokedAt            *time.Time           `json:"revoked_at,omitempty"`
}

type signInLink struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	UserID     string     `json:"user_id,omitempty"`
	TokenHash  []byte     `json:"token_hash"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
}

type dashboardSession struct {
	ID                string     `json:"id"`
	UserID            string     `json:"user_id"`
	SessionHash       []byte     `json:"session_hash"`
	ExpiresAt         time.Time  `json:"expires_at"`
	LastUsedAt        time.Time  `json:"last_used_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	EstablishedByLink bool       `json:"established_by_link"`
}

type workspaceBinding struct {
	Role      core.WorkspaceRole `json:"role"`
	CreatedAt time.Time          `json:"created_at"`
}

type workspaceInvitation struct {
	Role      core.WorkspaceRole `json:"role"`
	InvitedBy string             `json:"invited_by"`
	CreatedAt time.Time          `json:"created_at"`
}

// forgeTokenRecord is the sealed credential; Owner is the AEAD's associated
// data, so a token cannot be replayed under another owner.
type forgeTokenRecord struct {
	Owner      string    `json:"owner"`
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
	ForgeLogin string    `json:"forge_login"`
	StoredAt   time.Time `json:"stored_at"`
}

const (
	signInLinkLifetime       = 30 * time.Minute
	dashboardSessionLifetime = 7 * 24 * time.Hour
	// credentialUseGranularity bounds how often a verification writes: the
	// last-used timestamp moves at most once per interval, so a busy
	// credential does not append a state event per request.
	credentialUseGranularity = time.Minute
)

func normalizeIdentityEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("email must be a valid normalized address")
	}
	return email, nil
}

func randomIdentityID(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s identity: %w", prefix, err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomSecret(prefix, id string) (value string, hash []byte, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, fmt.Errorf("generate %s secret: %w", prefix, err)
	}
	value = prefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(value))
	return value, sum[:], nil
}

func timePtr(t time.Time) *time.Time { return &t }

func (m *volatileMemory) userActive(userID string) bool {
	user, ok := m.users[userID]
	return ok && user.Status == "active"
}

// memberActive reports whether a bound user may act: an unknown account
// (a membership seeded without one) counts as active, a deactivated one
// does not.
func (m *volatileMemory) memberActive(userID string) bool {
	user, ok := m.users[userID]
	return !ok || user.Status == "active"
}

func (m *volatileMemory) userByEmail(email string) (identityUser, bool) {
	for _, user := range m.users {
		if user.Email == email {
			return user, true
		}
	}
	return identityUser{}, false
}

func (m *volatileMemory) hasOperatorBinding(userID string) bool {
	for key, binding := range m.memberships {
		if key.id == userID && binding.Role == core.WorkspaceRoleOperator {
			return true
		}
	}
	return false
}

func (m *volatileMemory) operatorCount(workspaceID string) int {
	count := 0
	for key, binding := range m.memberships {
		if key.workspace == workspaceID && binding.Role == core.WorkspaceRoleOperator {
			count++
		}
	}
	return count
}

// deploymentEventLocked records an audit event that belongs to no workspace.
func (m *volatileMemory) deploymentEventLocked(ctx context.Context, kind string, payload any) core.Event {
	actor := ActorFromContext(ctx)
	return m.recordEventLocked("", core.Event{Kind: kind, ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(payload), At: time.Now().UTC()})
}

func (m *volatileMemory) userEventLocked(userID, kind string, payload any) core.Event {
	return m.recordEventLocked("", core.Event{Kind: kind, ActorID: UserActorID(userID), ActorRole: core.ActorUser, Payload: core.JSONPayload(payload), At: time.Now().UTC()})
}

// workspaceEventLocked records an event in the named workspace, whatever
// workspace the call itself ran in.
func (m *volatileMemory) workspaceEventLocked(ctx context.Context, workspaceID string, event core.Event) core.Event {
	actor := ActorFromContext(ctx)
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = core.JSONPayload(struct{}{})
	}
	return m.recordEventLocked(workspaceID, event)
}

// GetCallerIdentity implements CallerIdentityStore.
func (m *volatileMemory) GetCallerIdentity(_ context.Context, userID, workspaceID string) (core.CallerIdentity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.users[userID]
	if !ok || user.Status != "active" {
		return core.CallerIdentity{}, ErrNotFound
	}
	identity := core.CallerIdentity{ID: user.ID, Email: user.Email, DisplayName: user.DisplayName}
	if workspaceID != "" {
		binding, member := m.memberships[memoryScopedKey{workspace: workspaceID, id: userID}]
		if !member {
			return core.CallerIdentity{}, ErrNotFound
		}
		identity.Role = binding.Role
	}
	return identity, nil
}

// SetOwnDisplayName implements OwnProfileStore.
func (m *volatileMemory) SetOwnDisplayName(ctx context.Context, userID, sessionID, displayName string) (core.CallerIdentity, error) {
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok || user.Status != "active" || !m.sessionLive(sessionID, userID, time.Now().UTC()) {
		return core.CallerIdentity{}, core.ErrInvalidCredential
	}
	user.DisplayName = displayName
	m.users[userID] = user
	m.deploymentEventLocked(ctx, "identity.display_name_changed", map[string]any{"user_id": userID, "session_id": sessionID})
	return core.CallerIdentity{ID: user.ID, Email: user.Email, DisplayName: user.DisplayName}, nil
}

func (m *volatileMemory) sessionLive(sessionID, userID string, now time.Time) bool {
	session, ok := m.sessions[sessionID]
	return ok && session.UserID == userID && session.RevokedAt == nil && session.ExpiresAt.After(now)
}

// BootstrapIdentity makes the configured deployment token map to a usable
// operator: it heals or rotates the mapping when one exists, and otherwise
// seeds the first operator and maps the token to that account.
func (m *volatileMemory) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, legacyToken string) (bool, error) {
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
	m.lock()
	defer m.unlock()
	hash := sha256.Sum256([]byte(legacyToken))
	var legacy *identityCredential
	for id := range m.credentials {
		credential := m.credentials[id]
		if credential.DeploymentCredential {
			legacy = &credential
			break
		}
	}
	sameHash := legacy != nil && subtle.ConstantTimeCompare(legacy.TokenHash, hash[:]) == 1
	if legacy != nil && legacy.RevokedAt != nil && sameHash {
		return false, errors.New("legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT")
	}
	legacyActive := legacy != nil && legacy.RevokedAt == nil && m.userActive(legacy.UserID)
	if legacy != nil {
		coversWorkspaces := true
		for workspaceID := range m.workspaces {
			if m.memberships[memoryScopedKey{workspace: workspaceID, id: legacy.UserID}].Role != core.WorkspaceRoleOperator {
				coversWorkspaces = false
			}
		}
		if coversWorkspaces && sameHash && legacyActive && legacy.Scope == core.CredentialScopeOperator {
			return false, nil
		}
	}
	if legacyActive {
		credential := *legacy
		credential.Kind, credential.Scope = core.CredentialUser, core.CredentialScopeOperator
		auditKind := "identity.legacy_bindings_healed"
		if !sameHash {
			credential.TokenHash, credential.LastUsedAt = hash[:], nil
			auditKind = "identity.legacy_token_rotated"
		}
		m.credentials[credential.ID] = credential
		m.seedOperatorBindingsLocked(credential.UserID)
		m.recordEventLocked("", core.Event{Kind: auditKind, ActorID: "system", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"credential_id": credential.ID}), At: time.Now().UTC()})
		return true, nil
	}

	var owner identityUser
	if usable, ok := m.usableOperatorLocked(); ok {
		owner = usable
	} else {
		owner, err = m.provisionUserLocked(ctx, identity.Email, identity.DisplayName)
		if err != nil {
			return false, fmt.Errorf("seed first operator: %w", err)
		}
		if m.orgName == "Conveyor" {
			m.orgName = identity.OrganizationName
		}
	}
	if legacy != nil {
		retired := *legacy
		retired.Label, retired.DeploymentCredential = "retired legacy API token", false
		m.credentials[retired.ID] = retired
	}
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return false, err
	}
	m.credentials[tokenID] = identityCredential{
		ID: tokenID, UserID: owner.ID, Label: "legacy API token", TokenHash: hash[:],
		Kind: core.CredentialUser, Scope: core.CredentialScopeOperator, DeploymentCredential: true, CreatedAt: time.Now().UTC(),
	}
	if legacy != nil {
		m.recordEventLocked("", core.Event{Kind: "identity.legacy_token_rotated", ActorID: "system", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"credential_id": tokenID}), At: time.Now().UTC()})
	}
	m.seedOperatorBindingsLocked(owner.ID)
	return true, nil
}

// usableOperatorLocked returns the active user behind the oldest live
// operator-scoped credential who holds an operator binding somewhere.
func (m *volatileMemory) usableOperatorLocked() (identityUser, bool) {
	var candidates []identityCredential
	for _, credential := range m.credentials {
		if credential.Kind == core.CredentialUser && credential.Scope == core.CredentialScopeOperator && credential.RevokedAt == nil &&
			m.userActive(credential.UserID) && m.hasOperatorBinding(credential.UserID) {
			candidates = append(candidates, credential)
		}
	}
	if len(candidates) == 0 {
		return identityUser{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	return m.users[candidates[0].UserID], true
}

func (m *volatileMemory) seedOperatorBindingsLocked(userID string) {
	for workspaceID := range m.workspaces {
		key := memoryScopedKey{workspace: workspaceID, id: userID}
		binding, ok := m.memberships[key]
		if !ok {
			binding.CreatedAt = time.Now().UTC()
		}
		binding.Role = core.WorkspaceRoleOperator
		m.memberships[key] = binding
	}
}

// ProvisionIdentityUser implements IdentityProvisioner.
func (m *volatileMemory) ProvisionIdentityUser(ctx context.Context, email, displayName string) (core.IdentityUser, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IdentityUser{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return core.IdentityUser{}, errors.New("display name is required")
	}
	m.lock()
	defer m.unlock()
	user, err := m.provisionUserLocked(ctx, email, displayName)
	if err != nil {
		return core.IdentityUser{}, err
	}
	return user.IdentityUser, nil
}

// provisionUserLocked creates or resolves the account for email and redeems
// every pending invitation addressed to it.
func (m *volatileMemory) provisionUserLocked(ctx context.Context, email, displayName string) (identityUser, error) {
	user, ok := m.userByEmail(email)
	if !ok {
		id, err := randomIdentityID("usr", 12)
		if err != nil {
			return identityUser{}, err
		}
		user = identityUser{IdentityUser: core.IdentityUser{ID: id, Email: email, DisplayName: displayName, Status: "active", CreatedAt: time.Now().UTC()}}
		m.users[id] = user
	}
	if user.Status != "active" {
		return identityUser{}, errors.New("provisioned account is deactivated")
	}
	m.redeemInvitationsLocked(ctx, user)
	return user, nil
}

func (m *volatileMemory) redeemInvitationsLocked(_ context.Context, user identityUser) {
	var keys []memoryScopedKey
	for key := range m.invitations {
		if key.id == user.Email {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].workspace < keys[j].workspace })
	for _, key := range keys {
		invitation := m.invitations[key]
		delete(m.invitations, key)
		m.memberships[memoryScopedKey{workspace: key.workspace, id: user.ID}] = workspaceBinding{Role: invitation.Role, CreatedAt: time.Now().UTC()}
		m.recordEventLocked(key.workspace, core.Event{
			Kind: "workspace.membership_granted", ActorID: UserActorID(invitation.InvitedBy), ActorRole: core.ActorUser, At: time.Now().UTC(),
			Payload: core.JSONPayload(map[string]any{
				"workspace_id": key.workspace, "user_id": user.ID, "email": user.Email, "role": invitation.Role,
				"invitation": false, "redemption": true, "granted_by": invitation.InvitedBy,
			}),
		})
	}
}

func (m *volatileMemory) personalTokenEventLocked(ctx context.Context, credential identityCredential, kind string) {
	actorUserID := credential.UserID
	if caller, ok := CredentialFromContext(ctx); ok {
		actorUserID = caller.OwnerUserID
	}
	m.userEventLocked(actorUserID, kind, map[string]any{"credential_id": credential.ID, "label": credential.Label})
}

// IssuePersonalAccessToken mints a human credential; the value is returned
// once and only its hash is kept.
func (m *volatileMemory) IssuePersonalAccessToken(ctx context.Context, userID, label string) (core.IssuedPersonalAccessToken, error) {
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return core.IssuedPersonalAccessToken{}, err
	}
	value, hash, err := randomSecret("cv_pat_", tokenID)
	if err != nil {
		return core.IssuedPersonalAccessToken{}, err
	}
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok {
		return core.IssuedPersonalAccessToken{}, fmt.Errorf("%w: identity user %s", ErrNotFound, userID)
	}
	if user.Status != "active" {
		return core.IssuedPersonalAccessToken{}, errors.New("cannot issue a token for a deactivated user")
	}
	scope := core.CredentialScopeUser
	if m.hasOperatorBinding(userID) {
		scope = core.CredentialScopeOperator
	}
	credential := identityCredential{ID: tokenID, UserID: userID, Label: strings.TrimSpace(label), TokenHash: hash, Kind: core.CredentialUser, Scope: scope, CreatedAt: time.Now().UTC()}
	m.credentials[tokenID] = credential
	m.personalTokenEventLocked(ctx, credential, "identity.personal_token_issued")
	return core.IssuedPersonalAccessToken{PersonalAccessToken: personalAccessToken(credential), Value: value}, nil
}

// IssueAgentCredential implements AgentCredentialStore.
func (m *volatileMemory) IssueAgentCredential(_ context.Context, userID, label string) (IssuedAgentCredential, error) {
	id, err := randomIdentityID("agt", 12)
	if err != nil {
		return IssuedAgentCredential{}, err
	}
	value, hash, err := randomSecret("cv_agent_", id)
	if err != nil {
		return IssuedAgentCredential{}, err
	}
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok {
		return IssuedAgentCredential{}, fmt.Errorf("%w: identity user %s", ErrNotFound, userID)
	}
	if user.Status != "active" {
		return IssuedAgentCredential{}, errors.New("cannot issue a credential for a deactivated user")
	}
	label = strings.TrimSpace(label)
	m.credentials[id] = identityCredential{ID: id, UserID: userID, Label: label, TokenHash: hash, Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, CreatedAt: time.Now().UTC()}
	return IssuedAgentCredential{ID: id, UserID: userID, Label: label, Value: value}, nil
}

// RevokeAgentCredential revokes an agent credential the user owns; human
// credentials and other users' credentials are unaddressable here.
func (m *volatileMemory) RevokeAgentCredential(_ context.Context, userID, credentialID string) error {
	m.lock()
	defer m.unlock()
	credential, ok := m.credentials[credentialID]
	if !ok || credential.UserID != userID || credential.Kind != core.CredentialAgent {
		return ErrNotFound
	}
	if credential.RevokedAt == nil {
		credential.RevokedAt = timePtr(time.Now().UTC())
		m.credentials[credentialID] = credential
	}
	return nil
}

// RevokeRunAgentCredential implements AgentCredentialStore.
func (m *volatileMemory) RevokeRunAgentCredential(_ context.Context, userID, credentialID string, expected RunAgentCredentialBinding) error {
	m.lock()
	defer m.unlock()
	credential, ok := m.credentials[credentialID]
	if !ok || credential.UserID != userID || credential.Kind != core.CredentialAgent {
		return ErrNotFound
	}
	binding, ok := ParseRunAgentCredentialLabel(credential.Label)
	if !ok || binding != expected {
		return ErrRunAgentCredentialBindingMismatch
	}
	if credential.RevokedAt == nil {
		credential.RevokedAt = timePtr(time.Now().UTC())
		m.credentials[credentialID] = credential
	}
	return nil
}

// VerifyPersonalAccessToken resolves a human credential's user.
func (m *volatileMemory) VerifyPersonalAccessToken(ctx context.Context, candidate string) (core.IdentityUser, error) {
	credential, user, err := m.verifyCredential(ctx, candidate)
	if err != nil {
		return core.IdentityUser{}, err
	}
	if credential.Kind != core.CredentialUser {
		return core.IdentityUser{}, core.ErrInvalidCredential
	}
	return user, nil
}

// VerifyCredential resolves identity and scope from the stored credential
// record. The presented secret is never returned or logged.
func (m *volatileMemory) VerifyCredential(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	credential, _, err := m.verifyCredential(ctx, candidate)
	if err == nil && credential.Method == "" {
		credential.Method = core.CredentialMethodBearer
	}
	return credential, err
}

func (m *volatileMemory) credentialByHash(hash []byte) (identityCredential, bool) {
	for _, credential := range m.credentials {
		if subtle.ConstantTimeCompare(credential.TokenHash, hash) == 1 {
			return credential, true
		}
	}
	return identityCredential{}, false
}

func (m *volatileMemory) verifyCredential(_ context.Context, candidate string) (core.AuthenticatedCredential, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	now := time.Now().UTC()
	m.mu.RLock()
	credential, ok := m.credentialByHash(hash[:])
	user := m.users[credential.UserID]
	m.mu.RUnlock()
	if !ok || credential.RevokedAt != nil || user.Status != "active" {
		return core.AuthenticatedCredential{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	if credential.LastUsedAt == nil || now.Sub(*credential.LastUsedAt) >= credentialUseGranularity {
		m.lock()
		if current, ok := m.credentials[credential.ID]; ok && current.RevokedAt == nil {
			current.LastUsedAt = timePtr(now)
			m.credentials[credential.ID] = current
		}
		m.unlock()
	}
	authenticated := core.AuthenticatedCredential{ID: credential.ID, OwnerUserID: credential.UserID, Kind: credential.Kind, Scope: credential.Scope}
	if credential.Kind == core.CredentialAgent {
		if binding, ok := ParseRunAgentCredentialLabel(credential.Label); ok {
			authenticated.RunWorkspaceID, authenticated.RunWorkOrderID, authenticated.RunSessionID = binding.WorkspaceID, binding.WorkOrderID, binding.SessionID
		}
	}
	return authenticated, user.IdentityUser, nil
}

// IssueSignInLink implements InvitationSessionStore. It succeeds only for an
// existing account or a pending invitation: there is no self-registration.
func (m *volatileMemory) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	id, err := randomIdentityID("sil", 12)
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	value, hash, err := randomSecret("cv_signin_", id)
	if err != nil {
		return core.IssuedSignInLink{}, err
	}
	m.lock()
	defer m.unlock()
	user, hasUser := m.userByEmail(email)
	hasUser = hasUser && user.Status == "active"
	invited := false
	for key := range m.invitations {
		if key.id == email {
			invited = true
		}
	}
	if !hasUser && !invited {
		return core.IssuedSignInLink{}, ErrNotFound
	}
	now := time.Now().UTC()
	for linkID, link := range m.signInLinks {
		if link.Email == email && link.RedeemedAt == nil {
			link.RedeemedAt = timePtr(now)
			m.signInLinks[linkID] = link
		}
	}
	link := signInLink{ID: id, Email: email, TokenHash: hash, ExpiresAt: now.Add(signInLinkLifetime)}
	if hasUser {
		link.UserID = user.ID
	}
	m.signInLinks[id] = link
	m.deploymentEventLocked(ctx, "identity.signin_link_issued", map[string]any{"signin_link_id": id, "email": email})
	return core.IssuedSignInLink{Email: email, Value: value, ExpiresAt: link.ExpiresAt}, nil
}

// RedeemSignInLink consumes the link and creates its browser session; a
// repeated redemption finds the link already consumed.
func (m *volatileMemory) RedeemSignInLink(ctx context.Context, candidate string) (core.DashboardSession, core.IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	m.lock()
	defer m.unlock()
	now := time.Now().UTC()
	var link signInLink
	found := false
	for _, item := range m.signInLinks {
		if subtle.ConstantTimeCompare(item.TokenHash, hash[:]) == 1 {
			link, found = item, true
			break
		}
	}
	if !found || link.RedeemedAt != nil || !link.ExpiresAt.After(now) {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	link.RedeemedAt = timePtr(now)
	if link.UserID == "" {
		user, err := m.provisionUserLocked(ctx, link.Email, link.Email)
		if err != nil {
			return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
		}
		link.UserID = user.ID
	}
	m.signInLinks[link.ID] = link
	user, ok := m.users[link.UserID]
	if !ok || user.Status != "active" {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	session, err := m.mintSessionLocked(user.ID, true)
	if err != nil {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	m.userEventLocked(user.ID, "identity.signin_link_redeemed", map[string]any{"signin_link_id": link.ID})
	m.userEventLocked(user.ID, "identity.dashboard_session_created", map[string]any{"session_id": session.ID})
	return session, user.IdentityUser, nil
}

// SignInWithPassword never provisions an account. Unknown, deactivated, and
// passwordless accounts all take the same Argon2id path and the same error.
func (m *volatileMemory) SignInWithPassword(_ context.Context, email, password string) (core.DashboardSession, core.IdentityUser, error) {
	normalized, normalizeErr := normalizeIdentityEmail(email)
	dummyHash := volatileDummyPasswordHash()
	encoded := dummyHash
	var candidateID string
	if normalizeErr == nil {
		m.mu.RLock()
		if user, ok := m.userByEmail(normalized); ok && user.Status == "active" && user.PasswordHash != "" {
			candidateID, encoded = user.ID, user.PasswordHash
		}
		m.mu.RUnlock()
	}
	if !verifyVolatilePassword(encoded, password) || encoded == dummyHash {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	m.lock()
	defer m.unlock()
	user, ok := m.users[candidateID]
	if !ok || user.Status != "active" || subtle.ConstantTimeCompare([]byte(user.PasswordHash), []byte(encoded)) != 1 {
		return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
	}
	session, err := m.mintSessionLocked(user.ID, false)
	if err != nil {
		return core.DashboardSession{}, core.IdentityUser{}, err
	}
	m.userEventLocked(user.ID, "identity.dashboard_session_created", map[string]any{"session_id": session.ID, "method": "password"})
	return session, user.IdentityUser, nil
}

// SetOwnPassword implements InvitationSessionStore. A link-established
// session authorizes recovery; any other session must prove the current
// password when one is set.
func (m *volatileMemory) SetOwnPassword(_ context.Context, userID, sessionID, currentPassword, newPassword string) error {
	if !validVolatilePassword(newPassword) {
		return ErrInvalidPassword
	}
	encoded, err := hashVolatilePassword(newPassword)
	if err != nil {
		return err
	}
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok || user.Status != "active" || !m.sessionLive(sessionID, userID, time.Now().UTC()) {
		return core.ErrInvalidCredential
	}
	session := m.sessions[sessionID]
	if user.PasswordHash != "" && !session.EstablishedByLink && !verifyVolatilePassword(user.PasswordHash, currentPassword) {
		return ErrInvalidCurrentPassword
	}
	kind := "identity.password_set"
	if user.PasswordHash != "" {
		kind = "identity.password_changed"
	}
	user.PasswordHash = encoded
	m.users[userID] = user
	m.userEventLocked(userID, kind, map[string]any{"session_id": sessionID})
	return nil
}

func (m *volatileMemory) mintSessionLocked(userID string, establishedByLink bool) (core.DashboardSession, error) {
	id, err := randomIdentityID("ses", 12)
	if err != nil {
		return core.DashboardSession{}, err
	}
	value, hash, err := randomSecret("cv_session_", id)
	if err != nil {
		return core.DashboardSession{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(dashboardSessionLifetime)
	m.sessions[id] = dashboardSession{ID: id, UserID: userID, SessionHash: hash, ExpiresAt: expires, LastUsedAt: now, EstablishedByLink: establishedByLink}
	return core.DashboardSession{ID: id, UserID: userID, Value: value, ExpiresAt: expires}, nil
}

// VerifyDashboardSession implements InvitationSessionStore. The session
// slides forward on use, at most once per interval.
func (m *volatileMemory) VerifyDashboardSession(_ context.Context, candidate string) (core.AuthenticatedCredential, error) {
	hash := sha256.Sum256([]byte(candidate))
	now := time.Now().UTC()
	m.mu.RLock()
	var session dashboardSession
	found := false
	for _, item := range m.sessions {
		if subtle.ConstantTimeCompare(item.SessionHash, hash[:]) == 1 {
			session, found = item, true
			break
		}
	}
	active := found && session.RevokedAt == nil && session.ExpiresAt.After(now) && m.userActive(session.UserID)
	operator := active && m.hasOperatorBinding(session.UserID)
	m.mu.RUnlock()
	if !active {
		return core.AuthenticatedCredential{}, core.ErrInvalidCredential
	}
	if now.Sub(session.LastUsedAt) >= credentialUseGranularity {
		m.lock()
		if current, ok := m.sessions[session.ID]; ok && current.RevokedAt == nil {
			current.LastUsedAt, current.ExpiresAt = now, now.Add(dashboardSessionLifetime)
			m.sessions[session.ID] = current
			session = current
		}
		m.unlock()
	}
	scope := core.CredentialScopeUser
	if operator {
		scope = core.CredentialScopeOperator
	}
	return core.AuthenticatedCredential{
		ID: session.ID, OwnerUserID: session.UserID, Kind: core.CredentialUser, Scope: scope, Method: core.CredentialMethodSession,
		SessionExpiresAt: session.ExpiresAt, SessionEstablishedByLink: session.EstablishedByLink,
	}, nil
}

// RevokeDashboardSession implements InvitationSessionStore.
func (m *volatileMemory) RevokeDashboardSession(_ context.Context, userID, sessionID string) error {
	m.lock()
	defer m.unlock()
	session, ok := m.sessions[sessionID]
	if !ok || session.UserID != userID || session.RevokedAt != nil {
		return ErrNotFound
	}
	session.RevokedAt = timePtr(time.Now().UTC())
	m.sessions[sessionID] = session
	m.userEventLocked(userID, "identity.dashboard_session_revoked", map[string]any{"session_id": sessionID})
	return nil
}

// RecordInvitationDelivery implements InvitationSessionStore.
func (m *volatileMemory) RecordInvitationDelivery(ctx context.Context, email, outcome string) error {
	if outcome != "sent" && outcome != "failed" && outcome != "fallback" {
		return errors.New("invalid invitation delivery outcome")
	}
	m.lock()
	defer m.unlock()
	m.deploymentEventLocked(ctx, "identity.invitation_delivery_"+outcome, map[string]any{"email": email})
	return nil
}

// RevokePersonalAccessToken is administrative revocation by token id.
func (m *volatileMemory) RevokePersonalAccessToken(ctx context.Context, tokenID string) (core.PersonalAccessToken, error) {
	m.lock()
	defer m.unlock()
	credential, ok := m.credentials[tokenID]
	if !ok || credential.Kind != core.CredentialUser {
		return core.PersonalAccessToken{}, fmt.Errorf("%w: personal access token %s", ErrNotFound, tokenID)
	}
	return m.revokePersonalTokenLocked(ctx, credential), nil
}

func (m *volatileMemory) revokePersonalTokenLocked(ctx context.Context, credential identityCredential) core.PersonalAccessToken {
	if credential.RevokedAt == nil {
		credential.RevokedAt = timePtr(time.Now().UTC())
		m.credentials[credential.ID] = credential
	}
	m.personalTokenEventLocked(ctx, credential, "identity.personal_token_revoked")
	return personalAccessToken(credential)
}

// ListOwnPersonalAccessTokens implements PersonalAccessTokenStore; values
// are never listed, only the issuance response ever carried one.
func (m *volatileMemory) ListOwnPersonalAccessTokens(_ context.Context, userID string) ([]core.PersonalAccessToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []core.PersonalAccessToken{}
	for _, credential := range m.credentials {
		if credential.UserID == userID && credential.Kind == core.CredentialUser {
			items = append(items, personalAccessToken(credential))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

// IssueOwnPersonalAccessToken implements PersonalAccessTokenStore.
func (m *volatileMemory) IssueOwnPersonalAccessToken(ctx context.Context, userID, label string) (core.IssuedPersonalAccessToken, error) {
	return m.IssuePersonalAccessToken(ctx, userID, label)
}

// RevokeOwnPersonalAccessToken implements PersonalAccessTokenStore: another
// user's token is reported exactly like a token that does not exist.
func (m *volatileMemory) RevokeOwnPersonalAccessToken(ctx context.Context, userID, tokenID string) (core.PersonalAccessToken, error) {
	m.lock()
	defer m.unlock()
	credential, ok := m.credentials[tokenID]
	if !ok || credential.UserID != userID || credential.Kind != core.CredentialUser {
		return core.PersonalAccessToken{}, fmt.Errorf("%w: personal access token %s", ErrNotFound, tokenID)
	}
	return m.revokePersonalTokenLocked(ctx, credential), nil
}

// DeactivateIdentityUser closes the account and revokes its workers.
func (m *volatileMemory) DeactivateIdentityUser(ctx context.Context, userID string) (core.IdentityUser, error) {
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok {
		return core.IdentityUser{}, fmt.Errorf("%w: identity user %s", ErrNotFound, userID)
	}
	user.Status = "deactivated"
	m.users[userID] = user
	m.revokeOwnedWorkersLocked(ctx, userID, "", "identity_user_deactivated")
	return user.IdentityUser, nil
}

func personalAccessToken(credential identityCredential) core.PersonalAccessToken {
	return core.PersonalAccessToken{
		ID: credential.ID, UserID: credential.UserID, Label: credential.Label, DeploymentCredential: credential.DeploymentCredential,
		LastUsedAt: credential.LastUsedAt, RevokedAt: credential.RevokedAt, CreatedAt: credential.CreatedAt,
	}
}

// AuthorizeDeployment implements MembershipStore: deployment authority is
// the operator bundle, held by any active user with a live operator binding.
func (m *volatileMemory) AuthorizeDeployment(_ context.Context, userID string, capability core.Capability) (bool, error) {
	if !core.RoleAllows(core.WorkspaceRoleOperator, capability) {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.userActive(userID) && m.hasOperatorBinding(userID), nil
}

// AuthorizeWorkspace implements MembershipStore.
func (m *volatileMemory) AuthorizeWorkspace(_ context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	binding, ok := m.memberships[memoryScopedKey{workspace: workspaceID, id: userID}]
	if !ok {
		return false, nil
	}
	return core.RoleAllows(binding.Role, capability), nil
}

// ListWorkspacesForUser implements MembershipStore.
func (m *volatileMemory) ListWorkspacesForUser(_ context.Context, userID string) ([]core.Workspace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []core.Workspace
	for key := range m.memberships {
		if key.id != userID {
			continue
		}
		if record, ok := m.workspaces[key.workspace]; ok {
			result = append(result, record.Workspace)
		}
	}
	sortWorkspaces(result)
	return result, nil
}

func sortWorkspaces(items []core.Workspace) {
	sort.Slice(items, func(i, j int) bool {
		if a, b := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name); a != b {
			return a < b
		}
		return items[i].ID < items[j].ID
	})
}

// ListWorkspaceMembers implements MembershipStore.
func (m *volatileMemory) ListWorkspaceMembers(ctx context.Context, requesterUserID, workspaceID string) ([]core.WorkspaceMembership, error) {
	allowed, err := m.AuthorizeWorkspace(ctx, requesterUserID, workspaceID, core.CapabilityViewWorkspace)
	if err != nil || !allowed {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []core.WorkspaceMembership
	for key, binding := range m.memberships {
		if key.workspace != workspaceID {
			continue
		}
		user := m.users[key.id]
		result = append(result, core.WorkspaceMembership{WorkspaceID: workspaceID, UserID: key.id, Email: user.Email, DisplayName: user.DisplayName, Role: binding.Role, CreatedAt: binding.CreatedAt})
	}
	sort.Slice(result, func(i, j int) bool {
		if a, b := strings.ToLower(result[i].DisplayName), strings.ToLower(result[j].DisplayName); a != b {
			return a < b
		}
		return result[i].UserID < result[j].UserID
	})
	return result, nil
}

// ListWorkspaceInvitations implements MembershipStore; every stored
// invitation is pending, redemption and revocation both remove it.
func (m *volatileMemory) ListWorkspaceInvitations(_ context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []core.WorkspaceInvitation
	for key, invitation := range m.invitations {
		if key.workspace != workspaceID {
			continue
		}
		result = append(result, core.WorkspaceInvitation{
			WorkspaceID: workspaceID, Email: key.id, Role: invitation.Role, InvitedBy: invitation.InvitedBy,
			InvitedByDisplayName: m.users[invitation.InvitedBy].DisplayName, CreatedAt: invitation.CreatedAt,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].Email < result[j].Email
	})
	return result, nil
}

// GrantWorkspaceRole implements MembershipStore. An address without an
// account gets a pending invitation; an account gets the binding now.
func (m *volatileMemory) GrantWorkspaceRole(ctx context.Context, email, workspaceID string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.MembershipGrant{}, err
	}
	switch role {
	case core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator:
	default:
		return core.MembershipGrant{}, errors.New("role must be viewer, executor, contributor, maintainer, or operator")
	}
	credential, ok := CredentialFromContext(ctx)
	if !ok {
		return core.MembershipGrant{}, errors.New("authenticated user credential is required")
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[workspaceID]; !ok {
		return core.MembershipGrant{}, fmt.Errorf("%w: workspace membership", ErrNotFound)
	}
	now := time.Now().UTC()
	user, hasUser := m.userByEmail(email)
	invitation := !hasUser || user.Status != "active"
	if invitation {
		m.invitations[memoryScopedKey{workspace: workspaceID, id: email}] = workspaceInvitation{Role: role, InvitedBy: credential.OwnerUserID, CreatedAt: now}
	} else {
		key := memoryScopedKey{workspace: workspaceID, id: user.ID}
		previous, bound := m.memberships[key]
		if bound && previous.Role == core.WorkspaceRoleOperator && role != core.WorkspaceRoleOperator && m.operatorCount(workspaceID) <= 1 {
			return core.MembershipGrant{}, ErrLastWorkspaceOperator
		}
		binding := workspaceBinding{Role: role, CreatedAt: now}
		if bound {
			binding.CreatedAt = previous.CreatedAt
		}
		m.memberships[key] = binding
		if bound && core.RoleAllows(previous.Role, core.CapabilityClaimWork) && !core.RoleAllows(role, core.CapabilityClaimWork) {
			m.clearMemberAssignmentsLocked(ctx, workspaceID, user.ID)
			m.revokeOwnedWorkersLocked(ctx, user.ID, workspaceID, "workspace_membership_demoted")
		}
		delete(m.invitations, memoryScopedKey{workspace: workspaceID, id: email})
	}
	m.workspaceEventLocked(ctx, workspaceID, core.Event{Kind: "workspace.membership_granted", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspaceID, "email": email, "role": role, "invitation": invitation, "granted_by": credential.OwnerUserID,
	})})
	return core.MembershipGrant{Email: email, Role: role}, nil
}

// RevokeWorkspaceInvitation implements MembershipStore.
func (m *volatileMemory) RevokeWorkspaceInvitation(ctx context.Context, email, workspaceID string) error {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return err
	}
	credential, ok := CredentialFromContext(ctx)
	if !ok {
		return errors.New("authenticated user credential is required")
	}
	m.lock()
	defer m.unlock()
	key := memoryScopedKey{workspace: workspaceID, id: email}
	if _, ok := m.invitations[key]; !ok {
		return fmt.Errorf("%w: workspace membership invitation", ErrNotFound)
	}
	delete(m.invitations, key)
	m.workspaceEventLocked(ctx, workspaceID, core.Event{Kind: "workspace.membership_revoked", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspaceID, "email": email, "invitation": true, "revoked_by": credential.OwnerUserID,
	})})
	return nil
}

// RevokeWorkspaceRole implements MembershipStore; the last operator of a
// workspace cannot be removed.
func (m *volatileMemory) RevokeWorkspaceRole(ctx context.Context, userID, workspaceID string) error {
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[workspaceID]; !ok {
		return fmt.Errorf("%w: workspace membership", ErrNotFound)
	}
	key := memoryScopedKey{workspace: workspaceID, id: userID}
	binding, ok := m.memberships[key]
	if !ok {
		return fmt.Errorf("%w: workspace membership", ErrNotFound)
	}
	if binding.Role == core.WorkspaceRoleOperator && m.operatorCount(workspaceID) <= 1 {
		return ErrLastWorkspaceOperator
	}
	delete(m.memberships, key)
	m.clearMemberAssignmentsLocked(ctx, workspaceID, userID)
	m.revokeOwnedWorkersLocked(ctx, userID, workspaceID, "workspace_membership_revoked")
	m.workspaceEventLocked(ctx, workspaceID, core.Event{Kind: "workspace.membership_revoked", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspaceID, "user_id": userID,
	})})
	return nil
}

func (m *volatileMemory) clearMemberAssignmentsLocked(ctx context.Context, workspaceID, userID string) {
	var ids []string
	for id, task := range m.tasks {
		if task.Workspace == workspaceID && task.Assignee != nil && task.Assignee.UserID == userID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		task := m.tasks[id]
		task.Assignee = nil
		m.tasks[id] = task
		m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.assignee.cleared", Payload: core.JSONPayload(map[string]any{
			"assignee_user_id": "", "revoked_user_id": userID,
		})})
	}
}

// revokeOwnedWorkersLocked revokes the user's live workers, in one workspace
// or, with an empty workspace, everywhere.
func (m *volatileMemory) revokeOwnedWorkersLocked(ctx context.Context, userID, workspaceID, reason string) {
	actor := ActorFromContext(ctx)
	if actor.ID == "" {
		actor = Actor{ID: "system", Role: core.ActorSystem}
	}
	now := time.Now().UTC()
	var ids []string
	for id, worker := range m.workers {
		if worker.OwnerUserID == userID && worker.RevokedAt.IsZero() && (workspaceID == "" || worker.Workspace == workspaceID) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		worker := m.workers[id]
		worker.RevokedAt, worker.LeaseExpiresAt = now, time.Time{}
		m.workers[id] = worker
		m.recordEventLocked(worker.Workspace, core.Event{
			Kind: "worker.revoked", ActorID: actor.ID, ActorRole: actor.Role, At: now,
			Payload: core.JSONPayload(map[string]string{"worker_id": id, "owner_user_id": userID, "reason": reason}),
		})
	}
}

// ConfigureForgeTokenEncryptionKey installs the process-only AES-256 key. The
// key is process-only and never returned by metadata reads.
func (m *volatileMemory) ConfigureForgeTokenEncryptionKey(key []byte) {
	m.mu.Lock()
	defer m.unlock()
	m.forgeTokenKey = append([]byte(nil), key...)
}

func (m *volatileMemory) forgeTokenAEAD() (cipher.AEAD, error) {
	if len(m.forgeTokenKey) != 32 {
		return nil, ErrForgeTokenKey
	}
	block, err := aes.NewCipher(m.forgeTokenKey)
	if err != nil {
		return nil, ErrForgeTokenKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrForgeTokenKey
	}
	return aead, nil
}

func (m *volatileMemory) sealForgeToken(owner, token, login string) (forgeTokenRecord, error) {
	aead, err := m.forgeTokenAEAD()
	if err != nil {
		return forgeTokenRecord{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return forgeTokenRecord{}, fmt.Errorf("generate forge token nonce: %w", err)
	}
	return forgeTokenRecord{Owner: owner, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, []byte(token), []byte(owner)), ForgeLogin: login, StoredAt: time.Now().UTC()}, nil
}

func (m *volatileMemory) openForgeToken(record forgeTokenRecord) (string, error) {
	aead, err := m.forgeTokenAEAD()
	if err != nil {
		return "", err
	}
	if len(record.Nonce) != aead.NonceSize() {
		return "", ErrForgeTokenDecrypt
	}
	plaintext, err := aead.Open(nil, record.Nonce, record.Ciphertext, []byte(record.Owner))
	if err != nil {
		return "", ErrForgeTokenDecrypt
	}
	return string(plaintext), nil
}

func forgeTokenStatus(record forgeTokenRecord) core.ForgeTokenStatus {
	return core.ForgeTokenStatus{Configured: true, ForgeLogin: record.ForgeLogin, StoredAt: record.StoredAt}
}

// StoreForgeToken implements ForgeTokenStore.
func (m *volatileMemory) StoreForgeToken(ctx context.Context, userID, token, login string) (core.ForgeTokenStatus, error) {
	login = strings.TrimSpace(login)
	if userID == "" || token == "" || login == "" {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	m.lock()
	defer m.unlock()
	user, ok := m.users[userID]
	if !ok {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	if user.Status != "active" {
		return core.ForgeTokenStatus{}, ErrForgeTokenOwnerInactive
	}
	record, err := m.sealForgeToken(userID, token, login)
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	kind := "identity.forge_token_stored"
	if _, existed := m.forgeTokens[userID]; existed {
		kind = "identity.forge_token_replaced"
	}
	m.forgeTokens[userID] = record
	m.deploymentEventLocked(ctx, kind, map[string]any{"user_id": userID, "forge_login": login})
	return forgeTokenStatus(record), nil
}

// DeleteForgeToken implements ForgeTokenStore.
func (m *volatileMemory) DeleteForgeToken(ctx context.Context, userID string) error {
	m.lock()
	defer m.unlock()
	if _, ok := m.forgeTokens[userID]; !ok {
		return nil
	}
	delete(m.forgeTokens, userID)
	m.deploymentEventLocked(ctx, "identity.forge_token_deleted", map[string]any{"user_id": userID})
	return nil
}

// GetForgeTokenStatus implements ForgeTokenStore: metadata only.
func (m *volatileMemory) GetForgeTokenStatus(_ context.Context, userID string) (core.ForgeTokenStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.users[userID]; !ok {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	record, ok := m.forgeTokens[userID]
	if !ok {
		return core.ForgeTokenStatus{}, nil
	}
	return forgeTokenStatus(record), nil
}

// GetForgeTokenForUse implements ForgeTokenStore; it fails closed for an
// inactive owner and for a cipher failure.
func (m *volatileMemory) GetForgeTokenForUse(_ context.Context, userID string) (core.ForgeTokenCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.users[userID]
	if !ok {
		return core.ForgeTokenCredential{}, ErrNotFound
	}
	if user.Status != "active" {
		return core.ForgeTokenCredential{}, ErrForgeTokenOwnerInactive
	}
	record, ok := m.forgeTokens[userID]
	if !ok {
		return core.ForgeTokenCredential{}, ErrNotFound
	}
	token, err := m.openForgeToken(record)
	if err != nil {
		return core.ForgeTokenCredential{}, err
	}
	return core.ForgeTokenCredential{UserID: userID, Token: token, ForgeTokenStatus: forgeTokenStatus(record)}, nil
}

// ListForgeTokensForRedaction implements ForgeTokenStore.
func (m *volatileMemory) ListForgeTokensForRedaction(context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := make([]string, 0, len(m.forgeTokens)+len(m.workspaceForgeTokens))
	for _, record := range m.forgeTokens {
		value, err := m.openForgeToken(record)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	for _, record := range m.workspaceForgeTokens {
		value, err := m.openForgeToken(record)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

func workspaceForgeTokenOwner(workspaceID string) string { return "workspace:" + workspaceID }

// StoreWorkspaceForgeToken implements WorkspaceForgeTokenStore.
func (m *volatileMemory) StoreWorkspaceForgeToken(ctx context.Context, workspaceID, token, login string) (core.ForgeTokenStatus, error) {
	workspaceID, login = strings.TrimSpace(workspaceID), strings.TrimSpace(login)
	if workspaceID == "" || token == "" || login == "" {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[workspaceID]; !ok {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	record, err := m.sealForgeToken(workspaceForgeTokenOwner(workspaceID), token, login)
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	kind := "workspace.forge_token_stored"
	if _, existed := m.workspaceForgeTokens[workspaceID]; existed {
		kind = "workspace.forge_token_replaced"
	}
	m.workspaceForgeTokens[workspaceID] = record
	m.workspaceEventLocked(ctx, workspaceID, core.Event{Kind: kind, Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "forge_login": login})})
	return forgeTokenStatus(record), nil
}

// DeleteWorkspaceForgeToken implements WorkspaceForgeTokenStore.
func (m *volatileMemory) DeleteWorkspaceForgeToken(ctx context.Context, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return ErrNotFound
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaceForgeTokens[workspaceID]; !ok {
		return nil
	}
	delete(m.workspaceForgeTokens, workspaceID)
	m.workspaceEventLocked(ctx, workspaceID, core.Event{Kind: "workspace.forge_token_deleted", Payload: core.JSONPayload(map[string]any{"workspace_id": workspaceID})})
	return nil
}

// GetWorkspaceForgeTokenStatus implements WorkspaceForgeTokenStore.
func (m *volatileMemory) GetWorkspaceForgeTokenStatus(_ context.Context, workspaceID string) (core.ForgeTokenStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.workspaces[workspaceID]; !ok {
		return core.ForgeTokenStatus{}, ErrNotFound
	}
	record, ok := m.workspaceForgeTokens[workspaceID]
	if !ok {
		return core.ForgeTokenStatus{}, nil
	}
	return forgeTokenStatus(record), nil
}

// GetWorkspaceForgeTokenForUse implements WorkspaceForgeTokenStore.
func (m *volatileMemory) GetWorkspaceForgeTokenForUse(_ context.Context, workspaceID string) (core.WorkspaceForgeTokenCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.workspaceForgeTokens[workspaceID]
	if !ok {
		return core.WorkspaceForgeTokenCredential{}, ErrNotFound
	}
	token, err := m.openForgeToken(record)
	if err != nil {
		return core.WorkspaceForgeTokenCredential{}, err
	}
	return core.WorkspaceForgeTokenCredential{WorkspaceID: workspaceID, Token: token, ForgeTokenStatus: forgeTokenStatus(record)}, nil
}
