package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// OAuth lifetimes for the built-in `/mcp` authorization server
// (req-mcp-oauth AC-2.3, AC-3.3, AC-4.1; component-identity-membership
// invariants 51 to 54).
const (
	// OAuthAuthorizationCodeLifetime bounds a code from issuance to exchange.
	OAuthAuthorizationCodeLifetime = 60 * time.Second
	// OAuthAccessTokenLifetime is the lifetime of one access token.
	OAuthAccessTokenLifetime = time.Hour
	// OAuthRefreshTokenLifetime is the lifetime of one refresh token.
	OAuthRefreshTokenLifetime = 30 * 24 * time.Hour
	// OAuthUnusedClientLifetime is how long a dynamically registered client
	// stays usable before its first grant.
	OAuthUnusedClientLifetime = 24 * time.Hour
	// OAuthGrantUseGranularity bounds how often access verification writes a
	// grant's last_used_at, shared by every token of the grant
	// (component-identity-membership invariants 30 and 54).
	OAuthGrantUseGranularity = time.Minute
)

// OAuthCredentialKind names one opaque OAuth credential shape. The wire value
// is cv_<kind>_<id>_<secret> (req-security-boundaries AC-2.1).
type OAuthCredentialKind string

const (
	OAuthCredentialCode    OAuthCredentialKind = "oac"
	OAuthCredentialAccess  OAuthCredentialKind = "oat"
	OAuthCredentialRefresh OAuthCredentialKind = "ort"
)

// Valid reports whether kind is one of the three OAuth credential kinds.
func (kind OAuthCredentialKind) Valid() bool {
	return kind == OAuthCredentialCode || kind == OAuthCredentialAccess || kind == OAuthCredentialRefresh
}

const (
	oauthCredentialIDBytes     = 12
	oauthCredentialSecretBytes = 32
)

// NewOAuthCredential mints one opaque OAuth credential from cryptographic
// randomness. id is the stored row ID, <kind>_<random>. value is the wire
// value cv_<id>_<secret>, which only the issuance response carries. digest is
// the 32-byte SHA-256 of the complete wire value, the only stored form
// (req-security-boundaries AC-2.1; component-persistence invariant 41).
func NewOAuthCredential(kind OAuthCredentialKind) (id, value string, digest []byte, err error) {
	if !kind.Valid() {
		return "", "", nil, fmt.Errorf("unknown OAuth credential kind %q", kind)
	}
	raw := make([]byte, oauthCredentialIDBytes+oauthCredentialSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", nil, fmt.Errorf("generate OAuth %s credential: %w", kind, err)
	}
	id = string(kind) + "_" + base64.RawURLEncoding.EncodeToString(raw[:oauthCredentialIDBytes])
	value = "cv_" + id + "_" + base64.RawURLEncoding.EncodeToString(raw[oauthCredentialIDBytes:])
	return id, value, OAuthCredentialDigest(value), nil
}

// OAuthCredentialDigest returns the stored form of a presented OAuth
// credential: the SHA-256 of the complete wire value.
func OAuthCredentialDigest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// OAuthCredentialKindOf reports the kind a wire value claims by its prefix.
// The claim is not verification: only a stored digest match verifies a value.
func OAuthCredentialKindOf(value string) (OAuthCredentialKind, bool) {
	for _, kind := range []OAuthCredentialKind{OAuthCredentialCode, OAuthCredentialAccess, OAuthCredentialRefresh} {
		if strings.HasPrefix(value, "cv_"+string(kind)+"_") {
			return kind, true
		}
	}
	return "", false
}

// OAuthS256Challenge derives the PKCE S256 code challenge of a verifier:
// BASE64URL(SHA256(verifier)) without padding (RFC 7636 section 4.2).
func OAuthS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// OAuthS256Matches reports whether verifier derives challenge, compared in
// constant time.
func OAuthS256Matches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(OAuthS256Challenge(verifier)), []byte(challenge)) == 1
}

// IsOAuthMetadataDocumentClientID reports whether clientID names a Client ID
// Metadata Document. Such a client has no stored registration row; every
// other client ID must name a dynamic registration (req-mcp-oauth AC-2.1,
// AC-2.2).
func IsOAuthMetadataDocumentClientID(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// OAuthClient is one dynamically registered public client (RFC 7591). It holds
// no secret: every client authenticates with token_endpoint_auth_method none.
// FirstUsedAt is set atomically with the client's first grant, and an unused
// client stops working at UnusedExpiresAt (req-mcp-oauth AC-2.2, AC-2.3).
type OAuthClient struct {
	ID              string     `json:"client_id"`
	Name            string     `json:"client_name"`
	RedirectURIs    []string   `json:"redirect_uris"`
	GrantTypes      []string   `json:"grant_types"`
	CreatedAt       time.Time  `json:"created_at"`
	UnusedExpiresAt time.Time  `json:"unused_expires_at"`
	FirstUsedAt     *time.Time `json:"first_used_at,omitempty"`
}

// OAuthAuthorizationCode is one issued authorization code without its value.
// It is bound to the user, the verified client ID and name, the exact
// redirect URI, the resource, and the S256 code challenge, and it expires
// OAuthAuthorizationCodeLifetime after issuance (req-mcp-oauth AC-3.3).
type OAuthAuthorizationCode struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	RedirectURI   string    `json:"redirect_uri"`
	Resource      string    `json:"resource"`
	CodeChallenge string    `json:"code_challenge"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// OAuthGrant is the hash-free projection of one grant: the family of access
// and refresh tokens that one approved authorization started. It carries the
// verified client ID and name so a metadata-document client needs no stored
// registration (req-mcp-oauth AC-4.5).
type OAuthGrant struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	Resource   string     `json:"resource"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// OAuthTokenPair describes one issued access and refresh token pair without
// either value.
type OAuthTokenPair struct {
	GrantID          string    `json:"grant_id"`
	UserID           string    `json:"user_id"`
	ClientID         string    `json:"client_id"`
	Resource         string    `json:"resource"`
	AccessTokenID    string    `json:"access_token_id"`
	RefreshTokenID   string    `json:"refresh_token_id"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

// IssuedOAuthTokenPair carries the one-time wire values of a pair. Only a
// successful code exchange or refresh produces it; no read returns a value.
type IssuedOAuthTokenPair struct {
	OAuthTokenPair
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}
