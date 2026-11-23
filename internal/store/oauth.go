package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// OAuth sentinels. The HTTP server maps each to its OAuth error; none carries
// a credential value or digest (component-identity-membership invariants 52
// to 54).
var (
	// ErrOAuthCodeInvalid refuses an authorization code exchange: the code is
	// unknown, expired, already consumed, bound to other inputs, or its client
	// or owner can no longer receive a grant. Nothing is consumed.
	ErrOAuthCodeInvalid = errors.New("oauth authorization code invalid")
	// ErrOAuthTokenInvalid refuses an access or refresh token: unknown,
	// expired, revoked, bound to another client or resource, or owned by an
	// inactive user. It wraps core.ErrInvalidCredential, so a bearer boundary
	// answers it as unauthorized.
	ErrOAuthTokenInvalid = fmt.Errorf("oauth token invalid: %w", core.ErrInvalidCredential)
	// ErrOAuthRefreshTokenReused refuses a rotated refresh token presented
	// again. A store returns it only after the revocation of the whole grant
	// and its reuse audit event have committed (req-mcp-oauth AC-4.2).
	ErrOAuthRefreshTokenReused = errors.New("oauth refresh token reused")
	// ErrOAuthClientMetadataRefused refuses a client registration, an
	// authorization code, or a grant whose client ID or client name the
	// shared redactor would change or that holds any credential shape the
	// redactor knows, anywhere and after one percent-decoding. The store
	// writes nothing. The HTTP server
	// maps it to invalid_client. It is also a store.ErrRefusal.
	ErrOAuthClientMetadataRefused = Refuse(errors.New("oauth client ID or name holds a secret-shaped value"))
	// ErrOAuthRedirectURIRefused refuses the same writes when a redirect URI
	// holds a value the shared redactor would change or a credential shape
	// by the same rule. The store writes
	// nothing. The HTTP server maps it to invalid_redirect_uri. It is also a
	// store.ErrRefusal.
	ErrOAuthRedirectURIRefused = Refuse(errors.New("oauth redirect URI holds a secret-shaped value"))
)

// OAuthClientRegistration is the verified input of one dynamic client
// registration. The HTTP server owns the registration input policy
// (req-mcp-oauth AC-2.2); the store keeps only what it is given.
type OAuthClientRegistration struct {
	Name         string
	RedirectURIs []string
	GrantTypes   []string
}

// OAuthCodeRequest is the input of one approved authorization. ClientID and
// ClientName are the verified client identity, from a metadata document or a
// live dynamic registration. CodeChallenge is the S256 challenge.
type OAuthCodeRequest struct {
	UserID        string
	ClientID      string
	ClientName    string
	RedirectURI   string
	Resource      string
	CodeChallenge string
}

// OAuthCodeExchange is the input of grant_type=authorization_code.
type OAuthCodeExchange struct {
	Code         string
	ClientID     string
	RedirectURI  string
	Resource     string
	CodeVerifier string
}

// OAuthRefreshExchange is the input of grant_type=refresh_token.
type OAuthRefreshExchange struct {
	RefreshToken string
	ClientID     string
	Resource     string
}

// OAuthStore is the deployment-scoped OAuth persistence boundary for `/mcp`
// (req-mcp-oauth REQ-2 to REQ-4; DEC-146; component-identity-membership
// invariants 51 to 54). Every credential is stored only as the SHA-256 of its
// wire value, and only issuance returns a value (req-security-boundaries
// AC-2.1). Writers that append audit events require an explicit actor
// (component-persistence invariant 17). It is separate from VerifyCredential,
// which keeps refusing every OAuth value.
type OAuthStore interface {
	// RegisterOAuthClient stores a dynamic public client whose unused
	// deadline is core.OAuthUnusedClientLifetime from now (AC-2.2, AC-2.3).
	RegisterOAuthClient(context.Context, OAuthClientRegistration) (core.OAuthClient, error)
	// GetOAuthClient reads one live dynamic client. An unknown client, or an
	// unused one past its deadline, answers ErrNotFound (AC-2.3).
	GetOAuthClient(context.Context, string) (core.OAuthClient, error)
	// DeleteExpiredOAuthClients removes unused dynamic clients past their
	// deadline and returns how many it removed. It never removes a client
	// that backs a grant (AC-2.3).
	DeleteExpiredOAuthClients(context.Context) (int, error)
	// IssueOAuthAuthorizationCode mints a single-use code for an active user
	// and returns its record and its wire value (AC-3.3).
	IssueOAuthAuthorizationCode(context.Context, OAuthCodeRequest) (core.OAuthAuthorizationCode, string, error)
	// ExchangeOAuthAuthorizationCode consumes a code after it matches every
	// binding and the S256 verifier, and in the same transaction creates the
	// grant, issues its first pair, and marks a dynamic client used. Exactly
	// one concurrent exchange of a code succeeds (AC-4.1).
	ExchangeOAuthAuthorizationCode(context.Context, OAuthCodeExchange) (core.IssuedOAuthTokenPair, error)
	// RefreshOAuthTokens rotates the grant's current refresh token to a new
	// pair. A rotated refresh token presented again revokes the grant, commits
	// identity.oauth_refresh_reuse_detected, and then answers
	// ErrOAuthRefreshTokenReused (AC-4.2).
	RefreshOAuthTokens(context.Context, OAuthRefreshExchange) (core.IssuedOAuthTokenPair, error)
	// VerifyOAuthAccessToken resolves a live access token to its owner with
	// core.CredentialMethodOAuthMCP. It holds no lock and stamps the grant's
	// last_used_at at most once per core.OAuthGrantUseGranularity (AC-4.3,
	// AC-4.4; component-identity-membership invariant 30).
	VerifyOAuthAccessToken(context.Context, string) (core.AuthenticatedCredential, error)
	// ListOwnOAuthGrants lists the caller's unrevoked grants, newest first,
	// with no token value or digest (AC-4.5).
	ListOwnOAuthGrants(context.Context, string) ([]core.OAuthGrant, error)
	// RevokeOwnOAuthGrant revokes the caller's grant. Another user's grant
	// answers ErrNotFound, like a missing one (AC-4.4).
	RevokeOwnOAuthGrant(context.Context, string, string) (core.OAuthGrant, error)
	// RevokeOAuthToken implements RFC 7009 for the client that names it: an
	// access or refresh token of that client revokes its whole grant. An
	// unknown token, a token of another client, or an already revoked grant
	// changes nothing and answers nil (AC-4.4).
	RevokeOAuthToken(ctx context.Context, clientID, token string) error
}

// OAuth audit event kinds (component-identity-membership invariants 36, 53,
// and 54). Their payloads carry IDs and the client name only.
const (
	OAuthEventGrantCreated         = "identity.oauth_grant_created"
	OAuthEventGrantRevoked         = "identity.oauth_grant_revoked"
	OAuthEventRefreshReuseDetected = "identity.oauth_refresh_reuse_detected"
)

// IdentityHookOAuthAudit is the identity test-hook stage each backend runs
// immediately before it appends an OAuth audit event, after the state change
// in the same transaction or critical section. A hook error fails the append,
// so a test proves that the state change rolls back and no event commits
// (component-persistence invariant 5). No production caller sets a hook.
const IdentityHookOAuthAudit = "oauth_audit"

// OAuthGrantEventPayload is the complete payload of every OAuth audit event:
// IDs and the client name, never a value or digest
// (component-identity-membership invariant 36). The client ID and name are
// untrusted client metadata, so the shared redactor removes every
// secret-shaped value from them before they reach the deployment ledger
// (req-security-boundaries REQ-3; component-identity-membership invariant
// 45). The grant row keeps the exact client ID for binding checks.
func OAuthGrantEventPayload(grant core.OAuthGrant) map[string]any {
	redactor := redact.New(nil)
	clientID, _ := redactor.Redact(grant.ClientID)
	clientName, _ := redactor.Redact(grant.ClientName)
	return map[string]any{"grant_id": grant.ID, "user_id": grant.UserID, "client_id": clientID, "client_name": clientName}
}

// OAuthCredential is the identity an access token resolves to: its grant and
// live owner with the OAuth method. It carries no stored workspace role; the
// HTTP server resolves live workspace capabilities on each `/mcp` call
// (req-mcp-oauth AC-4.3; req-security-boundaries AC-1.4).
func OAuthCredential(grant core.OAuthGrant) core.AuthenticatedCredential {
	return core.AuthenticatedCredential{
		ID: grant.ID, OwnerUserID: grant.UserID, Kind: core.CredentialUser, Scope: core.CredentialScopeUser, Method: core.CredentialMethodOAuthMCP,
	}
}

// ValidateOAuthClientRegistration refuses a registration the store cannot
// keep. The HTTP server applies the full registration policy first.
func ValidateOAuthClientRegistration(registration OAuthClientRegistration) error {
	if len(registration.RedirectURIs) == 0 {
		return Refuse(errors.New("an OAuth client needs at least one redirect URI"))
	}
	for _, uri := range registration.RedirectURIs {
		if strings.TrimSpace(uri) == "" {
			return Refuse(errors.New("an OAuth redirect URI must not be empty"))
		}
	}
	return refuseSecretShapedOAuthMetadata("", registration.Name, registration.RedirectURIs...)
}

// refuseSecretShapedOAuthMetadata refuses client metadata that the shared
// redactor would change, before any write. Clean values are stored exactly,
// so binding checks keep exact equality, and no stored row, listing, or audit
// event can carry a secret-shaped client value (req-security-boundaries
// REQ-3; component-identity-membership invariant 45).
func refuseSecretShapedOAuthMetadata(clientID, clientName string, redirectURIs ...string) error {
	redactor := redact.New(nil)
	// secretShaped refuses a value the redactor would change, or one that
	// holds any credential shape anywhere, also after one percent-decoding.
	// The stored value stays exactly as given when both checks pass.
	secretShaped := func(value string) bool {
		clean, _ := redactor.Redact(value)
		return clean != value || redact.HoldsCredentialShape(value)
	}
	for _, value := range []string{clientID, clientName} {
		if secretShaped(value) {
			return ErrOAuthClientMetadataRefused
		}
	}
	for _, uri := range redirectURIs {
		if secretShaped(uri) {
			return ErrOAuthRedirectURIRefused
		}
	}
	return nil
}

// ValidateOAuthCodeRequest refuses an authorization whose bindings are
// incomplete. An S256 challenge is 43 base64url characters (RFC 7636).
func ValidateOAuthCodeRequest(request OAuthCodeRequest) error {
	if request.UserID == "" || request.ClientID == "" || request.RedirectURI == "" || request.Resource == "" {
		return Refuse(errors.New("an OAuth authorization code needs a user, client, redirect URI, and resource"))
	}
	if len(request.CodeChallenge) != 43 || strings.Trim(request.CodeChallenge, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		return Refuse(errors.New("an OAuth authorization code needs an S256 code challenge"))
	}
	return refuseSecretShapedOAuthMetadata(request.ClientID, request.ClientName, request.RedirectURI)
}

// ValidateOAuthCodeExchange refuses, before any write, a grant whose client
// ID or redirect URI the shared redactor would change. A grant takes its
// client name from the stored code, which issuance already checked.
func ValidateOAuthCodeExchange(exchange OAuthCodeExchange) error {
	return refuseSecretShapedOAuthMetadata(exchange.ClientID, "", exchange.RedirectURI)
}

// OAuthCodeBindingsMatch reports whether an exchange matches every binding of
// the stored code, including the S256 verifier (req-mcp-oauth AC-4.1).
func OAuthCodeBindingsMatch(code core.OAuthAuthorizationCode, exchange OAuthCodeExchange) bool {
	return code.ClientID == exchange.ClientID && code.RedirectURI == exchange.RedirectURI && code.Resource == exchange.Resource &&
		core.OAuthS256Matches(exchange.CodeVerifier, code.CodeChallenge)
}

// NewOAuthClientID returns a fresh dynamic client ID. It is not a secret.
func NewOAuthClientID() (string, error) { return randomIdentityID("ocl", 12) }

// NewOAuthGrantID returns a fresh grant ID.
func NewOAuthGrantID() (string, error) { return randomIdentityID("ogr", 12) }
