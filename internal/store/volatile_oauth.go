package store

import (
	"context"
	"crypto/subtle"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Volatile OAuth state mirrors the four deployment tables of the durable
// backends (component-persistence invariant 21). Every transition runs under
// the memory write mutex, the volatile backend's critical section.

type oauthCodeRecord struct {
	core.OAuthAuthorizationCode
	CodeHash   []byte
	ConsumedAt *time.Time
}

type oauthGrantRecord struct {
	core.OAuthGrant
	// CurrentRefreshID names the one refresh token that may rotate. Every
	// other refresh token of the grant is a replay marker.
	CurrentRefreshID string
}

type oauthTokenRecord struct {
	ID        string
	GrantID   string
	Kind      core.OAuthCredentialKind
	TokenHash []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	RotatedAt *time.Time
}

type oauthState struct {
	clients map[string]core.OAuthClient
	codes   map[string]oauthCodeRecord
	grants  map[string]oauthGrantRecord
	tokens  map[string]oauthTokenRecord
}

func newOAuthState() oauthState {
	return oauthState{
		clients: map[string]core.OAuthClient{},
		codes:   map[string]oauthCodeRecord{},
		grants:  map[string]oauthGrantRecord{},
		tokens:  map[string]oauthTokenRecord{},
	}
}

func cloneOAuthClient(client core.OAuthClient) core.OAuthClient {
	client.RedirectURIs = slices.Clone(client.RedirectURIs)
	client.GrantTypes = slices.Clone(client.GrantTypes)
	if client.FirstUsedAt != nil {
		client.FirstUsedAt = timePtr(*client.FirstUsedAt)
	}
	return client
}

func oauthClientLive(client core.OAuthClient, now time.Time) bool {
	return client.FirstUsedAt != nil || now.Before(client.UnusedExpiresAt)
}

// RegisterOAuthClient implements OAuthStore.
func (m *volatileMemory) RegisterOAuthClient(_ context.Context, registration OAuthClientRegistration) (core.OAuthClient, error) {
	if err := ValidateOAuthClientRegistration(registration); err != nil {
		return core.OAuthClient{}, err
	}
	id, err := NewOAuthClientID()
	if err != nil {
		return core.OAuthClient{}, err
	}
	now := time.Now().UTC()
	client := core.OAuthClient{
		ID: id, Name: strings.TrimSpace(registration.Name), RedirectURIs: slices.Clone(registration.RedirectURIs),
		GrantTypes: slices.Clone(registration.GrantTypes), CreatedAt: now, UnusedExpiresAt: now.Add(core.OAuthUnusedClientLifetime),
	}
	if client.GrantTypes == nil {
		client.GrantTypes = []string{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.oauth.clients[id] = client
	return cloneOAuthClient(client), nil
}

// GetOAuthClient implements OAuthStore.
func (m *volatileMemory) GetOAuthClient(_ context.Context, clientID string) (core.OAuthClient, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.oauth.clients[clientID]
	if !ok || !oauthClientLive(client, time.Now().UTC()) {
		return core.OAuthClient{}, fmt.Errorf("%w: OAuth client %s", ErrNotFound, clientID)
	}
	return cloneOAuthClient(client), nil
}

// DeleteExpiredOAuthClients implements OAuthStore. A client backing a grant
// has FirstUsedAt and is never removed.
func (m *volatileMemory) DeleteExpiredOAuthClients(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	backing := map[string]bool{}
	for _, grant := range m.oauth.grants {
		backing[grant.ClientID] = true
	}
	now := time.Now().UTC()
	removed := 0
	for id, client := range m.oauth.clients {
		if !oauthClientLive(client, now) && !backing[id] {
			delete(m.oauth.clients, id)
			removed++
		}
	}
	return removed, nil
}

// IssueOAuthAuthorizationCode implements OAuthStore.
func (m *volatileMemory) IssueOAuthAuthorizationCode(_ context.Context, request OAuthCodeRequest) (core.OAuthAuthorizationCode, string, error) {
	if err := ValidateOAuthCodeRequest(request); err != nil {
		return core.OAuthAuthorizationCode{}, "", err
	}
	id, value, digest, err := core.NewOAuthCredential(core.OAuthCredentialCode)
	if err != nil {
		return core.OAuthAuthorizationCode{}, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.userActive(request.UserID) {
		return core.OAuthAuthorizationCode{}, "", fmt.Errorf("%w: active identity user %s", ErrNotFound, request.UserID)
	}
	now := time.Now().UTC()
	code := core.OAuthAuthorizationCode{
		ID: id, UserID: request.UserID, ClientID: request.ClientID, ClientName: request.ClientName, RedirectURI: request.RedirectURI,
		Resource: request.Resource, CodeChallenge: request.CodeChallenge, CreatedAt: now, ExpiresAt: now.Add(core.OAuthAuthorizationCodeLifetime),
	}
	m.oauth.codes[id] = oauthCodeRecord{OAuthAuthorizationCode: code, CodeHash: digest}
	return code, value, nil
}

func (m *volatileMemory) oauthCodeByHashLocked(hash []byte) (oauthCodeRecord, bool) {
	for _, code := range m.oauth.codes {
		if subtle.ConstantTimeCompare(code.CodeHash, hash) == 1 {
			return code, true
		}
	}
	return oauthCodeRecord{}, false
}

func (m *volatileMemory) oauthTokenByHashLocked(hash []byte) (oauthTokenRecord, bool) {
	for _, token := range m.oauth.tokens {
		if subtle.ConstantTimeCompare(token.TokenHash, hash) == 1 {
			return token, true
		}
	}
	return oauthTokenRecord{}, false
}

// consumeOAuthCodeLocked is the single-use primitive: it checks every binding
// and marks the code consumed. The caller holds the write mutex, so exactly
// one exchange consumes a code.
func (m *volatileMemory) consumeOAuthCodeLocked(exchange OAuthCodeExchange, now time.Time) (core.OAuthAuthorizationCode, error) {
	code, ok := m.oauthCodeByHashLocked(core.OAuthCredentialDigest(exchange.Code))
	if !ok || code.ConsumedAt != nil || !now.Before(code.ExpiresAt) || !OAuthCodeBindingsMatch(code.OAuthAuthorizationCode, exchange) {
		return core.OAuthAuthorizationCode{}, ErrOAuthCodeInvalid
	}
	code.ConsumedAt = timePtr(now)
	m.oauth.codes[code.ID] = code
	return code.OAuthAuthorizationCode, nil
}

// issueOAuthPairLocked stores a fresh access and refresh token for grant and
// makes the refresh token the grant's current one.
func (m *volatileMemory) issueOAuthPairLocked(grant *oauthGrantRecord, now time.Time) (core.IssuedOAuthTokenPair, error) {
	accessID, access, accessHash, err := core.NewOAuthCredential(core.OAuthCredentialAccess)
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	refreshID, refresh, refreshHash, err := core.NewOAuthCredential(core.OAuthCredentialRefresh)
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	pair := core.IssuedOAuthTokenPair{OAuthTokenPair: core.OAuthTokenPair{
		GrantID: grant.ID, UserID: grant.UserID, ClientID: grant.ClientID, Resource: grant.Resource,
		AccessTokenID: accessID, RefreshTokenID: refreshID,
		AccessExpiresAt: now.Add(core.OAuthAccessTokenLifetime), RefreshExpiresAt: now.Add(core.OAuthRefreshTokenLifetime),
	}, AccessToken: access, RefreshToken: refresh}
	m.oauth.tokens[accessID] = oauthTokenRecord{ID: accessID, GrantID: grant.ID, Kind: core.OAuthCredentialAccess, TokenHash: accessHash, CreatedAt: now, ExpiresAt: pair.AccessExpiresAt}
	m.oauth.tokens[refreshID] = oauthTokenRecord{ID: refreshID, GrantID: grant.ID, Kind: core.OAuthCredentialRefresh, TokenHash: refreshHash, CreatedAt: now, ExpiresAt: pair.RefreshExpiresAt}
	grant.CurrentRefreshID = refreshID
	m.oauth.grants[grant.ID] = *grant
	return pair, nil
}

// ExchangeOAuthAuthorizationCode implements OAuthStore. The consumption, the
// grant, the first pair, the client's first use, and the audit event commit
// together under one critical section, or nothing changes.
func (m *volatileMemory) ExchangeOAuthAuthorizationCode(ctx context.Context, exchange OAuthCodeExchange) (core.IssuedOAuthTokenPair, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	if err := ValidateOAuthCodeExchange(exchange); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	grantID, err := NewOAuthGrantID()
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.oauth.clone()
	pair, grant, err := m.exchangeOAuthCodeLocked(exchange, grantID)
	if err != nil {
		m.oauth = snapshot
		return core.IssuedOAuthTokenPair{}, err
	}
	if err := m.appendOAuthEventLocked(ctx, actor, OAuthEventGrantCreated, grant.OAuthGrant, time.Now().UTC()); err != nil {
		m.oauth = snapshot
		return core.IssuedOAuthTokenPair{}, err
	}
	return pair, nil
}

// appendOAuthEventLocked appends one OAuth audit event. The test hook runs
// first, so a failed append leaves the caller to restore its state change.
func (m *volatileMemory) appendOAuthEventLocked(ctx context.Context, actor Actor, kind string, grant core.OAuthGrant, at time.Time) error {
	if err := RunIdentityTestHook(ctx, IdentityHookOAuthAudit); err != nil {
		return err
	}
	m.recordEventLocked(actor, "", core.Event{Kind: kind, ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(OAuthGrantEventPayload(grant)), At: at})
	return nil
}

func (m *volatileMemory) exchangeOAuthCodeLocked(exchange OAuthCodeExchange, grantID string) (core.IssuedOAuthTokenPair, oauthGrantRecord, error) {
	now := time.Now().UTC()
	code, err := m.consumeOAuthCodeLocked(exchange, now)
	if err != nil {
		return core.IssuedOAuthTokenPair{}, oauthGrantRecord{}, err
	}
	if !m.userActive(code.UserID) {
		return core.IssuedOAuthTokenPair{}, oauthGrantRecord{}, ErrOAuthCodeInvalid
	}
	if !core.IsOAuthMetadataDocumentClientID(code.ClientID) {
		client, ok := m.oauth.clients[code.ClientID]
		if !ok || !oauthClientLive(client, now) {
			return core.IssuedOAuthTokenPair{}, oauthGrantRecord{}, ErrOAuthCodeInvalid
		}
		if client.FirstUsedAt == nil {
			client.FirstUsedAt = timePtr(now)
			m.oauth.clients[client.ID] = client
		}
	}
	grant := oauthGrantRecord{OAuthGrant: core.OAuthGrant{
		ID: grantID, UserID: code.UserID, ClientID: code.ClientID, ClientName: code.ClientName, Resource: code.Resource, CreatedAt: now,
	}}
	pair, err := m.issueOAuthPairLocked(&grant, now)
	return pair, grant, err
}

func (s oauthState) clone() oauthState {
	out := newOAuthState()
	for id, client := range s.clients {
		out.clients[id] = cloneOAuthClient(client)
	}
	for id, code := range s.codes {
		out.codes[id] = code
	}
	for id, grant := range s.grants {
		out.grants[id] = grant
	}
	for id, token := range s.tokens {
		out.tokens[id] = token
	}
	return out
}

// RefreshOAuthTokens implements OAuthStore. The grant's critical section
// serializes rotation with both revocation paths. A replay commits the
// grant's revocation and its audit event before the sentinel returns.
func (m *volatileMemory) RefreshOAuthTokens(ctx context.Context, exchange OAuthRefreshExchange) (core.IssuedOAuthTokenPair, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	token, ok := m.oauthTokenByHashLocked(core.OAuthCredentialDigest(exchange.RefreshToken))
	if !ok || token.Kind != core.OAuthCredentialRefresh {
		return core.IssuedOAuthTokenPair{}, ErrOAuthTokenInvalid
	}
	grant, ok := m.oauth.grants[token.GrantID]
	if !ok || grant.ClientID != exchange.ClientID || grant.Resource != exchange.Resource || grant.RevokedAt != nil || !m.userActive(grant.UserID) {
		return core.IssuedOAuthTokenPair{}, ErrOAuthTokenInvalid
	}
	if grant.CurrentRefreshID != token.ID {
		live := grant
		grant.RevokedAt = timePtr(now)
		m.oauth.grants[grant.ID] = grant
		if err := m.appendOAuthEventLocked(ctx, actor, OAuthEventRefreshReuseDetected, grant.OAuthGrant, now); err != nil {
			m.oauth.grants[grant.ID] = live
			return core.IssuedOAuthTokenPair{}, err
		}
		return core.IssuedOAuthTokenPair{}, ErrOAuthRefreshTokenReused
	}
	if !now.Before(token.ExpiresAt) {
		return core.IssuedOAuthTokenPair{}, ErrOAuthTokenInvalid
	}
	snapshot := m.oauth.clone()
	token.RotatedAt = timePtr(now)
	m.oauth.tokens[token.ID] = token
	pair, err := m.issueOAuthPairLocked(&grant, now)
	if err != nil {
		m.oauth = snapshot
		return core.IssuedOAuthTokenPair{}, err
	}
	return pair, nil
}

// oauthAccessRead is one lock-free read of a live access token.
type oauthAccessRead struct {
	token oauthTokenRecord
	grant oauthGrantRecord
	stale bool
}

func (m *volatileMemory) readOAuthAccessLocked(hash []byte, now time.Time) (oauthAccessRead, bool) {
	token, ok := m.oauthTokenByHashLocked(hash)
	if !ok || token.Kind != core.OAuthCredentialAccess || !now.Before(token.ExpiresAt) {
		return oauthAccessRead{}, false
	}
	grant, ok := m.oauth.grants[token.GrantID]
	if !ok || grant.RevokedAt != nil || !m.userActive(grant.UserID) {
		return oauthAccessRead{}, false
	}
	stale := grant.LastUsedAt == nil || now.Sub(*grant.LastUsedAt) >= core.OAuthGrantUseGranularity
	return oauthAccessRead{token: token, grant: grant, stale: stale}, true
}

// VerifyOAuthAccessToken implements OAuthStore. The read runs under the read
// lock. A stale grant is stamped under the write mutex only after every
// validity check repeats; a recheck that fails refuses.
func (m *volatileMemory) VerifyOAuthAccessToken(_ context.Context, candidate string) (core.AuthenticatedCredential, error) {
	hash := core.OAuthCredentialDigest(candidate)
	m.mu.RLock()
	read, ok := m.readOAuthAccessLocked(hash, time.Now().UTC())
	m.mu.RUnlock()
	if !ok {
		return core.AuthenticatedCredential{}, ErrOAuthTokenInvalid
	}
	if read.stale {
		m.mu.Lock()
		now := time.Now().UTC()
		current, live := m.readOAuthAccessLocked(hash, now)
		if live && current.stale {
			current.grant.LastUsedAt = timePtr(now)
			m.oauth.grants[current.grant.ID] = current.grant
		}
		m.mu.Unlock()
		if !live {
			return core.AuthenticatedCredential{}, ErrOAuthTokenInvalid
		}
		read = current
	}
	return OAuthCredential(read.grant.OAuthGrant), nil
}

// ListOwnOAuthGrants implements OAuthStore.
func (m *volatileMemory) ListOwnOAuthGrants(_ context.Context, userID string) ([]core.OAuthGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []core.OAuthGrant{}
	for _, grant := range m.oauth.grants {
		if grant.UserID == userID && grant.RevokedAt == nil {
			items = append(items, detachOAuthGrant(grant.OAuthGrant))
		}
	}
	sortOAuthGrants(items)
	return items, nil
}

// sortOAuthGrants orders grants newest first, then by ID.
func sortOAuthGrants(items []core.OAuthGrant) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

// RevokeOwnOAuthGrant implements OAuthStore. Ownership is part of the match,
// so another user's grant answers exactly like a missing one.
func (m *volatileMemory) RevokeOwnOAuthGrant(ctx context.Context, userID, grantID string) (core.OAuthGrant, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return core.OAuthGrant{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	grant, ok := m.oauth.grants[grantID]
	if !ok || grant.UserID != userID {
		return core.OAuthGrant{}, fmt.Errorf("%w: OAuth grant %s", ErrNotFound, grantID)
	}
	if err := m.revokeOAuthGrantLocked(ctx, actor, &grant); err != nil {
		return core.OAuthGrant{}, err
	}
	return detachOAuthGrant(grant.OAuthGrant), nil
}

// detachOAuthGrant copies a grant's timestamps, so a caller cannot change
// stored state through a returned projection.
func detachOAuthGrant(grant core.OAuthGrant) core.OAuthGrant {
	if grant.LastUsedAt != nil {
		grant.LastUsedAt = timePtr(*grant.LastUsedAt)
	}
	if grant.RevokedAt != nil {
		grant.RevokedAt = timePtr(*grant.RevokedAt)
	}
	return grant
}

// revokeOAuthGrantLocked revokes an unrevoked grant and appends its audit
// event. An already revoked grant changes nothing. A failed append restores
// the live grant.
func (m *volatileMemory) revokeOAuthGrantLocked(ctx context.Context, actor Actor, grant *oauthGrantRecord) error {
	if grant.RevokedAt != nil {
		return nil
	}
	live := *grant
	now := time.Now().UTC()
	grant.RevokedAt = timePtr(now)
	m.oauth.grants[grant.ID] = *grant
	if err := m.appendOAuthEventLocked(ctx, actor, OAuthEventGrantRevoked, grant.OAuthGrant, now); err != nil {
		m.oauth.grants[grant.ID] = live
		*grant = live
		return err
	}
	return nil
}

// RevokeOAuthToken implements OAuthStore.
func (m *volatileMemory) RevokeOAuthToken(ctx context.Context, clientID, candidate string) error {
	actor, err := RequireActor(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.oauthTokenByHashLocked(core.OAuthCredentialDigest(candidate))
	if !ok {
		return nil
	}
	grant, ok := m.oauth.grants[token.GrantID]
	if !ok || grant.ClientID != clientID {
		return nil
	}
	return m.revokeOAuthGrantLocked(ctx, actor, &grant)
}
