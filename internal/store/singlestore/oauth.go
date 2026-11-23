package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The SingleStore OAuth store matches the PostgreSQL reference on the shared
// OAuth suite (component-persistence invariant 3). A commit spanning
// partitions can release a lock row before its other rows are visible, so
// every contended decision reads only the row it locked (invariant 8): the
// code row for consumption, the client row for first use, and the grant row,
// which names its one current refresh token, for rotation and both
// revocation paths (req-mcp-oauth AC-4.1, AC-4.2, AC-4.4).

// oauthDigestSize is the only stored credential length: a SHA-256 digest
// (req-security-boundaries AC-2.1; component-persistence invariant 41).
const oauthDigestSize = 32

// checkOAuthDigest is the SingleStore write boundary for OAuth digests. The
// PostgreSQL schema enforces the same length with a CHECK.
func checkOAuthDigest(digest []byte) error {
	if len(digest) != oauthDigestSize {
		return fmt.Errorf("an OAuth credential digest must be %d bytes, not %d", oauthDigestSize, len(digest))
	}
	return nil
}

// writeOAuthCredential inserts one code or token row after the digest check.
func writeOAuthCredential(ctx context.Context, tx *sql.Tx, table, column string, values map[string]any) error {
	digest, _ := values[column].([]byte)
	if err := checkOAuthDigest(digest); err != nil {
		return err
	}
	_, err := writeRow(ctx, tx, rowWrite{table: table, operation: "INSERT", values: values})
	return err
}

func nullTimePtr(at sql.NullTime) *time.Time {
	if !at.Valid {
		return nil
	}
	value := at.Time.UTC()
	return &value
}

// databaseNow reads the database clock. A caller reads it after it holds
// every lock its decision needs, so a lock wait cannot carry an old time past
// a code, client, or token deadline (req-mcp-oauth AC-2.3, AC-3.3, AC-4.1).
func databaseNow(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6)").Scan(&now); err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

// RegisterOAuthClient implements store.OAuthStore.
func (s *Store) RegisterOAuthClient(ctx context.Context, registration store.OAuthClientRegistration) (core.OAuthClient, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RegisterOAuthClient")
	if err := store.ValidateOAuthClientRegistration(registration); err != nil {
		return core.OAuthClient{}, err
	}
	id, err := store.NewOAuthClientID()
	if err != nil {
		return core.OAuthClient{}, err
	}
	client := core.OAuthClient{ID: id, Name: strings.TrimSpace(registration.Name), RedirectURIs: slices.Clone(registration.RedirectURIs), GrantTypes: slices.Clone(registration.GrantTypes)}
	if client.GrantTypes == nil {
		client.GrantTypes = []string{}
	}
	uris, err := json.Marshal(client.RedirectURIs)
	if err != nil {
		return core.OAuthClient{}, err
	}
	grants, err := json.Marshal(client.GrantTypes)
	if err != nil {
		return core.OAuthClient{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		client.CreatedAt, client.UnusedExpiresAt = now, now.Add(core.OAuthUnusedClientLifetime)
		_, err = writeRow(ctx, tx, rowWrite{table: "oauth_clients", operation: "INSERT", values: map[string]any{
			"id": client.ID, "name": client.Name, "redirect_uris": string(uris), "grant_types": string(grants),
			"created_at": client.CreatedAt, "unused_expires_at": client.UnusedExpiresAt,
		}})
		return err
	})
	if err != nil {
		return core.OAuthClient{}, translateBackendConflict(err)
	}
	return client, nil
}

// GetOAuthClient implements store.OAuthStore. An unused client past its
// deadline reads as missing (req-mcp-oauth AC-2.3).
func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (core.OAuthClient, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetOAuthClient")
	var client core.OAuthClient
	var uris, grants string
	var firstUsed sql.NullTime
	err := backendRow(ctx, s.db, "SELECT id,name,redirect_uris,grant_types,created_at,unused_expires_at,first_used_at FROM oauth_clients WHERE id=? AND (first_used_at IS NOT NULL OR unused_expires_at>CURRENT_TIMESTAMP(6))", clientID).
		Scan(&client.ID, &client.Name, &uris, &grants, &client.CreatedAt, &client.UnusedExpiresAt, &firstUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OAuthClient{}, fmt.Errorf("OAuth client %s: %w", clientID, store.ErrNotFound)
	}
	if err != nil {
		return core.OAuthClient{}, translateBackendConflict(err)
	}
	if err = json.Unmarshal([]byte(uris), &client.RedirectURIs); err != nil {
		return core.OAuthClient{}, fmt.Errorf("decode OAuth client redirect URIs: %w", err)
	}
	if err = json.Unmarshal([]byte(grants), &client.GrantTypes); err != nil {
		return core.OAuthClient{}, fmt.Errorf("decode OAuth client grant types: %w", err)
	}
	client.CreatedAt, client.UnusedExpiresAt, client.FirstUsedAt = client.CreatedAt.UTC(), client.UnusedExpiresAt.UTC(), nullTimePtr(firstUsed)
	return client, nil
}

// DeleteExpiredOAuthClients implements store.OAuthStore. Each candidate is
// rechecked under its own row lock, which an exchange marking first use also
// holds, and is kept when a grant names it.
func (s *Store) DeleteExpiredOAuthClients(ctx context.Context) (int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "DeleteExpiredOAuthClients")
	rows, err := backendRows(ctx, s.db, "SELECT id FROM oauth_clients WHERE first_used_at IS NULL AND unused_expires_at<=CURRENT_TIMESTAMP(6) ORDER BY id")
	if err != nil {
		return 0, translateBackendConflict(err)
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, translateBackendConflict(err)
		}
		candidates = append(candidates, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, translateBackendConflict(err)
	}
	removed := 0
	for _, id := range candidates {
		err = s.withTx(ctx, func(tx *sql.Tx) error {
			var expired bool
			err := tx.QueryRowContext(ctx, "SELECT first_used_at IS NULL AND unused_expires_at<=CURRENT_TIMESTAMP(6) FROM oauth_clients WHERE id=? FOR UPDATE", id).Scan(&expired)
			if errors.Is(err, sql.ErrNoRows) || err == nil && !expired {
				return nil
			}
			if err != nil {
				return err
			}
			var backing int
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM oauth_grants WHERE client_id=?", id).Scan(&backing); err != nil || backing > 0 {
				return err
			}
			if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_clients", operation: "DELETE", where: map[string]any{"id": id}}); err != nil {
				return err
			}
			removed++
			return nil
		})
		if err != nil {
			return removed, translateBackendConflict(err)
		}
	}
	return removed, nil
}

// activeOAuthOwner reads the owner's live status in the transaction
// (req-security-boundaries AC-2.3).
func activeOAuthOwner(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var status string
	err := tx.QueryRowContext(ctx, "SELECT status FROM users WHERE id=?", userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return status == "active", err
}

// IssueOAuthAuthorizationCode implements store.OAuthStore.
func (s *Store) IssueOAuthAuthorizationCode(ctx context.Context, request store.OAuthCodeRequest) (core.OAuthAuthorizationCode, string, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "IssueOAuthAuthorizationCode")
	if err := store.ValidateOAuthCodeRequest(request); err != nil {
		return core.OAuthAuthorizationCode{}, "", err
	}
	id, value, digest, err := core.NewOAuthCredential(core.OAuthCredentialCode)
	if err != nil {
		return core.OAuthAuthorizationCode{}, "", err
	}
	code := core.OAuthAuthorizationCode{ID: id, UserID: request.UserID, ClientID: request.ClientID, ClientName: request.ClientName,
		RedirectURI: request.RedirectURI, Resource: request.Resource, CodeChallenge: request.CodeChallenge}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if active, err := activeOAuthOwner(ctx, tx, code.UserID); err != nil {
			return err
		} else if !active {
			return fmt.Errorf("active identity user %s: %w", code.UserID, store.ErrNotFound)
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		code.CreatedAt, code.ExpiresAt = now, now.Add(core.OAuthAuthorizationCodeLifetime)
		return writeOAuthCredential(ctx, tx, "oauth_authorization_codes", "code_hash", map[string]any{
			"id": code.ID, "code_hash": digest, "user_id": code.UserID, "client_id": code.ClientID, "client_name": code.ClientName,
			"redirect_uri": code.RedirectURI, "resource": code.Resource, "code_challenge": code.CodeChallenge,
			"created_at": code.CreatedAt, "expires_at": code.ExpiresAt,
		})
	})
	if err != nil {
		return core.OAuthAuthorizationCode{}, "", translateBackendConflict(err)
	}
	return code, value, nil
}

// consumeOAuthCode is the single-use primitive. It finds the code by digest,
// locks its row, reads the database clock, and decides only from that locked
// row: unconsumed, unexpired, and matching every binding and the S256
// verifier. A concurrent exchange waits on the row lock and then reads the
// consumed row.
func consumeOAuthCode(ctx context.Context, tx *sql.Tx, exchange store.OAuthCodeExchange) (core.OAuthAuthorizationCode, error) {
	hash := core.OAuthCredentialDigest(exchange.Code)
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM oauth_authorization_codes WHERE code_hash=?", hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OAuthAuthorizationCode{}, store.ErrOAuthCodeInvalid
	}
	if err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	var code core.OAuthAuthorizationCode
	var stored []byte
	var consumed sql.NullTime
	err = tx.QueryRowContext(ctx, "SELECT id,code_hash,user_id,client_id,client_name,redirect_uri,resource,code_challenge,created_at,expires_at,consumed_at FROM oauth_authorization_codes WHERE id=? FOR UPDATE", id).
		Scan(&code.ID, &stored, &code.UserID, &code.ClientID, &code.ClientName, &code.RedirectURI, &code.Resource, &code.CodeChallenge, &code.CreatedAt, &code.ExpiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OAuthAuthorizationCode{}, store.ErrOAuthCodeInvalid
	}
	if err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	if consumed.Valid || !now.Before(code.ExpiresAt.UTC()) || subtle.ConstantTimeCompare(stored, hash) != 1 || !store.OAuthCodeBindingsMatch(code, exchange) {
		return core.OAuthAuthorizationCode{}, store.ErrOAuthCodeInvalid
	}
	if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_authorization_codes", operation: "UPDATE", values: map[string]any{"consumed_at": now}, where: map[string]any{"id": code.ID}}); err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	return code, nil
}

// issueOAuthPair inserts a fresh access and refresh token for the grant and
// makes the refresh token the grant's current one. The grant ID is every
// token row's shard key, so the pair and the grant share one partition.
func issueOAuthPair(ctx context.Context, tx *sql.Tx, grant core.OAuthGrant, now time.Time) (core.IssuedOAuthTokenPair, error) {
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
	for _, token := range []struct {
		id      string
		kind    core.OAuthCredentialKind
		digest  []byte
		expires time.Time
	}{
		{accessID, core.OAuthCredentialAccess, accessHash, pair.AccessExpiresAt},
		{refreshID, core.OAuthCredentialRefresh, refreshHash, pair.RefreshExpiresAt},
	} {
		if err = writeOAuthCredential(ctx, tx, "oauth_tokens", "token_hash", map[string]any{
			"grant_id": grant.ID, "id": token.id, "kind": string(token.kind), "token_hash": token.digest, "created_at": now, "expires_at": token.expires,
		}); err != nil {
			return core.IssuedOAuthTokenPair{}, err
		}
	}
	if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_grants", operation: "UPDATE", values: map[string]any{"current_refresh_token_id": refreshID}, where: map[string]any{"id": grant.ID}}); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	return pair, nil
}

// appendOAuthEvent appends one OAuth audit event through the deployment
// ledger's insert-only write path. Its payload carries IDs and the client
// name only (component-identity-membership invariant 36). The identity test
// hook runs first, after the caller's state change, so a test can fail the
// append and prove that the transaction rolls back.
func appendOAuthEvent(ctx context.Context, tx *sql.Tx, kind string, grant core.OAuthGrant) error {
	if err := store.RunIdentityTestHook(ctx, store.IdentityHookOAuthAudit); err != nil {
		return err
	}
	return appendDeploymentEvent(ctx, tx, kind, store.OAuthGrantEventPayload(grant))
}

// ExchangeOAuthAuthorizationCode implements store.OAuthStore. Consumption,
// the client's first use, the grant, its first pair, and the audit event
// commit in one transaction, or nothing does (component-persistence
// invariant 5).
func (s *Store) ExchangeOAuthAuthorizationCode(ctx context.Context, exchange store.OAuthCodeExchange) (core.IssuedOAuthTokenPair, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ExchangeOAuthAuthorizationCode")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	if err := store.ValidateOAuthCodeExchange(exchange); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	grantID, err := store.NewOAuthGrantID()
	if err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	var pair core.IssuedOAuthTokenPair
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		code, err := consumeOAuthCode(ctx, tx, exchange)
		if err != nil {
			return err
		}
		if active, err := activeOAuthOwner(ctx, tx, code.UserID); err != nil {
			return err
		} else if !active {
			return store.ErrOAuthCodeInvalid
		}
		var firstUsed sql.NullTime
		var unusedExpires time.Time
		dynamic := !core.IsOAuthMetadataDocumentClientID(code.ClientID)
		if dynamic {
			err := tx.QueryRowContext(ctx, "SELECT first_used_at,unused_expires_at FROM oauth_clients WHERE id=? FOR UPDATE", code.ClientID).Scan(&firstUsed, &unusedExpires)
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrOAuthCodeInvalid
			}
			if err != nil {
				return err
			}
		}
		// The clock read after the code and client locks decides the
		// unused-client deadline and stamps the grant.
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if dynamic {
			if !firstUsed.Valid {
				if !now.Before(unusedExpires.UTC()) {
					return store.ErrOAuthCodeInvalid
				}
				if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_clients", operation: "UPDATE", values: map[string]any{"first_used_at": now}, where: map[string]any{"id": code.ClientID}}); err != nil {
					return err
				}
			}
		}
		grant := core.OAuthGrant{ID: grantID, UserID: code.UserID, ClientID: code.ClientID, ClientName: code.ClientName, Resource: code.Resource, CreatedAt: now}
		if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_grants", operation: "INSERT", values: map[string]any{
			"id": grant.ID, "user_id": grant.UserID, "client_id": grant.ClientID, "client_name": grant.ClientName, "resource": grant.Resource,
			"current_refresh_token_id": "", "created_at": now,
		}}); err != nil {
			return err
		}
		if pair, err = issueOAuthPair(ctx, tx, grant, now); err != nil {
			return err
		}
		return appendOAuthEvent(ctx, tx, store.OAuthEventGrantCreated, grant)
	})
	if err != nil {
		return core.IssuedOAuthTokenPair{}, translateBackendConflict(err)
	}
	return pair, nil
}

// lockOAuthGrant reads and locks one grant row, the authoritative row for
// rotation and both revocation paths.
func lockOAuthGrant(ctx context.Context, tx *sql.Tx, grantID string) (core.OAuthGrant, string, bool, error) {
	var grant core.OAuthGrant
	var current string
	var lastUsed, revoked sql.NullTime
	err := tx.QueryRowContext(ctx, "SELECT id,user_id,client_id,client_name,resource,current_refresh_token_id,created_at,last_used_at,revoked_at FROM oauth_grants WHERE id=? FOR UPDATE", grantID).
		Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.ClientName, &grant.Resource, &current, &grant.CreatedAt, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OAuthGrant{}, "", false, nil
	}
	if err != nil {
		return core.OAuthGrant{}, "", false, err
	}
	grant.CreatedAt, grant.LastUsedAt, grant.RevokedAt = grant.CreatedAt.UTC(), nullTimePtr(lastUsed), nullTimePtr(revoked)
	return grant, current, true, nil
}

// revokeOAuthGrant revokes a locked, unrevoked grant and appends kind.
func revokeOAuthGrant(ctx context.Context, tx *sql.Tx, grant *core.OAuthGrant, kind string, now time.Time) error {
	if _, err := writeRow(ctx, tx, rowWrite{table: "oauth_grants", operation: "UPDATE", values: map[string]any{"revoked_at": now}, where: map[string]any{"id": grant.ID}}); err != nil {
		return err
	}
	grant.RevokedAt = &now
	return appendOAuthEvent(ctx, tx, kind, *grant)
}

// oauthTokenRef is one token row found by digest without a lock.
type oauthTokenRef struct {
	grantID, id string
	kind        core.OAuthCredentialKind
	expiresAt   time.Time
}

func findOAuthToken(ctx context.Context, tx *sql.Tx, hash []byte) (oauthTokenRef, bool, error) {
	var ref oauthTokenRef
	var kind string
	var stored []byte
	err := tx.QueryRowContext(ctx, "SELECT grant_id,id,kind,token_hash,expires_at FROM oauth_tokens WHERE token_hash=?", hash).Scan(&ref.grantID, &ref.id, &kind, &stored, &ref.expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return oauthTokenRef{}, false, nil
	}
	if err != nil {
		return oauthTokenRef{}, false, err
	}
	if subtle.ConstantTimeCompare(stored, hash) != 1 {
		return oauthTokenRef{}, false, nil
	}
	ref.kind, ref.expiresAt = core.OAuthCredentialKind(kind), ref.expiresAt.UTC()
	return ref, true, nil
}

// RefreshOAuthTokens implements store.OAuthStore. A replay revokes the grant
// and appends its audit event in a transaction that commits; only then does
// the method return ErrOAuthRefreshTokenReused. A failed audit rolls the
// revocation back and returns that failure instead (req-mcp-oauth AC-4.2).
func (s *Store) RefreshOAuthTokens(ctx context.Context, exchange store.OAuthRefreshExchange) (core.IssuedOAuthTokenPair, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RefreshOAuthTokens")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	hash := core.OAuthCredentialDigest(exchange.RefreshToken)
	var pair core.IssuedOAuthTokenPair
	reused := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		token, ok, err := findOAuthToken(ctx, tx, hash)
		if err != nil {
			return err
		}
		if !ok || token.kind != core.OAuthCredentialRefresh {
			return store.ErrOAuthTokenInvalid
		}
		grant, current, ok, err := lockOAuthGrant(ctx, tx, token.grantID)
		if err != nil {
			return err
		}
		if !ok || grant.ClientID != exchange.ClientID || grant.Resource != exchange.Resource || grant.RevokedAt != nil {
			return store.ErrOAuthTokenInvalid
		}
		if active, err := activeOAuthOwner(ctx, tx, grant.UserID); err != nil {
			return err
		} else if !active {
			return store.ErrOAuthTokenInvalid
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if current != token.id {
			reused = true
			return revokeOAuthGrant(ctx, tx, &grant, store.OAuthEventRefreshReuseDetected, now)
		}
		if !now.Before(token.expiresAt) {
			return store.ErrOAuthTokenInvalid
		}
		if _, err = writeRow(ctx, tx, rowWrite{table: "oauth_tokens", operation: "UPDATE", values: map[string]any{"rotated_at": now}, where: map[string]any{"grant_id": grant.ID, "id": token.id}}); err != nil {
			return err
		}
		pair, err = issueOAuthPair(ctx, tx, grant, now)
		return err
	})
	if err != nil {
		return core.IssuedOAuthTokenPair{}, translateBackendConflict(err)
	}
	if reused {
		return core.IssuedOAuthTokenPair{}, store.ErrOAuthRefreshTokenReused
	}
	return pair, nil
}

// oauthAccessRead is one lock-free verification read of an access token.
type oauthAccessRead struct {
	grant core.OAuthGrant
	stale bool
}

// readOAuthAccess reads a live access token, its unrevoked grant, and its
// active owner on the ordinary handle, outside any transaction and without a
// lock (component-identity-membership invariants 30 and 54).
func (s *Store) readOAuthAccess(ctx context.Context, hash []byte) (oauthAccessRead, error) {
	var r oauthAccessRead
	var stored []byte
	err := backendRow(ctx, s.db, "SELECT t.token_hash,g.id,g.user_id,g.client_id,g.client_name,g.resource,g.created_at,g.last_used_at IS NULL OR g.last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND) FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id JOIN users u ON u.id=g.user_id WHERE t.token_hash=? AND t.kind=? AND t.expires_at>CURRENT_TIMESTAMP(6) AND g.revoked_at IS NULL AND u.status='active'",
		core.OAuthGrantUseGranularity.Microseconds(), hash, string(core.OAuthCredentialAccess)).
		Scan(&stored, &r.grant.ID, &r.grant.UserID, &r.grant.ClientID, &r.grant.ClientName, &r.grant.Resource, &r.grant.CreatedAt, &r.stale)
	if errors.Is(err, sql.ErrNoRows) {
		return oauthAccessRead{}, store.ErrOAuthTokenInvalid
	}
	if err != nil {
		return oauthAccessRead{}, translateBackendConflict(err)
	}
	if subtle.ConstantTimeCompare(stored, hash) != 1 {
		return oauthAccessRead{}, store.ErrOAuthTokenInvalid
	}
	return r, nil
}

// VerifyOAuthAccessToken implements store.OAuthStore. It reads before it
// writes and never enters a transaction. A stale grant gets one conditional
// UPDATE that repeats every validity predicate; a call whose UPDATE changes
// nothing rereads, because a concurrent call stamped first or the token lost
// its validity in between.
func (s *Store) VerifyOAuthAccessToken(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "VerifyOAuthAccessToken")
	hash := core.OAuthCredentialDigest(candidate)
	read, err := s.readOAuthAccess(ctx, hash)
	if err != nil {
		return core.AuthenticatedCredential{}, err
	}
	if read.stale {
		result, err := backendExec(ctx, s.db, "UPDATE oauth_grants SET last_used_at=CURRENT_TIMESTAMP(6) WHERE id=? AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND)) AND user_id IN (SELECT id FROM users WHERE status='active') AND id IN (SELECT grant_id FROM oauth_tokens WHERE token_hash=? AND kind=? AND expires_at>CURRENT_TIMESTAMP(6))",
			read.grant.ID, core.OAuthGrantUseGranularity.Microseconds(), hash, string(core.OAuthCredentialAccess))
		if err != nil {
			return core.AuthenticatedCredential{}, translateBackendConflict(err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return core.AuthenticatedCredential{}, translateBackendConflict(err)
		}
		if changed != 1 {
			if read, err = s.readOAuthAccess(ctx, hash); err != nil {
				return core.AuthenticatedCredential{}, err
			}
		}
	}
	return store.OAuthCredential(read.grant), nil
}

// ListOwnOAuthGrants implements store.OAuthStore. The statement selects no
// digest column.
func (s *Store) ListOwnOAuthGrants(ctx context.Context, userID string) ([]core.OAuthGrant, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListOwnOAuthGrants")
	rows, err := backendRows(ctx, s.db, "SELECT id,user_id,client_id,client_name,resource,created_at,last_used_at,revoked_at FROM oauth_grants WHERE user_id=? AND revoked_at IS NULL ORDER BY created_at DESC,id", userID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	items := []core.OAuthGrant{}
	for rows.Next() {
		var grant core.OAuthGrant
		var lastUsed, revoked sql.NullTime
		if err = rows.Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.ClientName, &grant.Resource, &grant.CreatedAt, &lastUsed, &revoked); err != nil {
			return nil, translateBackendConflict(err)
		}
		grant.CreatedAt, grant.LastUsedAt, grant.RevokedAt = grant.CreatedAt.UTC(), nullTimePtr(lastUsed), nullTimePtr(revoked)
		items = append(items, grant)
	}
	return items, translateBackendConflict(rows.Err())
}

// RevokeOwnOAuthGrant implements store.OAuthStore. Ownership is checked on
// the locked row, so another user's grant answers like a missing one.
func (s *Store) RevokeOwnOAuthGrant(ctx context.Context, userID, grantID string) (core.OAuthGrant, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RevokeOwnOAuthGrant")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.OAuthGrant{}, err
	}
	var grant core.OAuthGrant
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		locked, _, ok, err := lockOAuthGrant(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if !ok || locked.UserID != userID {
			return fmt.Errorf("OAuth grant %s: %w", grantID, store.ErrNotFound)
		}
		grant = locked
		if grant.RevokedAt != nil {
			return nil
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		return revokeOAuthGrant(ctx, tx, &grant, store.OAuthEventGrantRevoked, now)
	})
	if err != nil {
		return core.OAuthGrant{}, translateBackendConflict(err)
	}
	return grant, nil
}

// RevokeOAuthToken implements store.OAuthStore (RFC 7009).
func (s *Store) RevokeOAuthToken(ctx context.Context, clientID, candidate string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RevokeOAuthToken")
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	hash := core.OAuthCredentialDigest(candidate)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		token, ok, err := findOAuthToken(ctx, tx, hash)
		if err != nil || !ok {
			return err
		}
		grant, _, ok, err := lockOAuthGrant(ctx, tx, token.grantID)
		if err != nil || !ok || grant.ClientID != clientID || grant.RevokedAt != nil {
			return err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		return revokeOAuthGrant(ctx, tx, &grant, store.OAuthEventGrantRevoked, now)
	})
	return translateBackendConflict(err)
}
