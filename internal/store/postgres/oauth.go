package postgres

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// The PostgreSQL OAuth store is the reference behavior for the shared OAuth
// suite (component-persistence invariant 3). Every contended transition
// locks its authoritative row, then reads fresh database time with
// clock_timestamp() for its expiry decision: the code for consumption, and the
// grant for rotation and both revocation paths (req-mcp-oauth AC-4.1, AC-4.2,
// AC-4.4; component-identity-membership invariants 52 to 54).

func microseconds(d time.Duration) int64 { return d.Microseconds() }

func utcPtr(at pgtype.Timestamptz) *time.Time {
	if !at.Valid {
		return nil
	}
	value := at.Time.UTC()
	return &value
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
	err = s.boundary.QueryRow(ctx, `INSERT INTO oauth_clients (id,name,redirect_uris,grant_types,created_at,unused_expires_at)
		VALUES ($1,$2,$3,$4,now(),now()+$5::bigint*interval '1 microsecond') RETURNING created_at,unused_expires_at`,
		client.ID, client.Name, uris, grants, microseconds(core.OAuthUnusedClientLifetime)).Scan(&client.CreatedAt, &client.UnusedExpiresAt)
	if err != nil {
		return core.OAuthClient{}, translateBackendConflict(err)
	}
	client.CreatedAt, client.UnusedExpiresAt = client.CreatedAt.UTC(), client.UnusedExpiresAt.UTC()
	return client, nil
}

// GetOAuthClient implements store.OAuthStore. An unused client past its
// deadline reads as missing (req-mcp-oauth AC-2.3).
func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (core.OAuthClient, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetOAuthClient")
	var client core.OAuthClient
	var uris, grants []byte
	var firstUsed pgtype.Timestamptz
	err := s.boundary.QueryRow(ctx, `SELECT id,name,redirect_uris,grant_types,created_at,unused_expires_at,first_used_at FROM oauth_clients
		WHERE id=$1 AND (first_used_at IS NOT NULL OR unused_expires_at>now())`, clientID).
		Scan(&client.ID, &client.Name, &uris, &grants, &client.CreatedAt, &client.UnusedExpiresAt, &firstUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.OAuthClient{}, fmt.Errorf("OAuth client %s: %w", clientID, store.ErrNotFound)
	}
	if err != nil {
		return core.OAuthClient{}, translateBackendConflict(err)
	}
	if err = json.Unmarshal(uris, &client.RedirectURIs); err != nil {
		return core.OAuthClient{}, fmt.Errorf("decode OAuth client redirect URIs: %w", err)
	}
	if err = json.Unmarshal(grants, &client.GrantTypes); err != nil {
		return core.OAuthClient{}, fmt.Errorf("decode OAuth client grant types: %w", err)
	}
	client.CreatedAt, client.UnusedExpiresAt, client.FirstUsedAt = client.CreatedAt.UTC(), client.UnusedExpiresAt.UTC(), utcPtr(firstUsed)
	return client, nil
}

// DeleteExpiredOAuthClients implements store.OAuthStore. A client backing a
// grant has first_used_at, and the statement also checks for a grant, so it
// never removes one. An exchange holds the client row while it marks first
// use, so the delete rechecks that row after the exchange commits.
func (s *Store) DeleteExpiredOAuthClients(ctx context.Context) (int, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "DeleteExpiredOAuthClients")
	tag, err := s.boundary.Exec(ctx, `DELETE FROM oauth_clients c WHERE c.first_used_at IS NULL AND c.unused_expires_at<=now()
		AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id=c.id)`)
	if err != nil {
		return 0, translateBackendConflict(err)
	}
	return int(tag.RowsAffected()), nil
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
	err = s.boundary.QueryRow(ctx, `INSERT INTO oauth_authorization_codes (id,code_hash,user_id,client_id,client_name,redirect_uri,resource,code_challenge,created_at,expires_at)
		SELECT $1,$2,u.id,$4,$5,$6,$7,$8,now(),now()+$9::bigint*interval '1 microsecond' FROM users u WHERE u.id=$3 AND u.status='active'
		RETURNING created_at,expires_at`,
		code.ID, digest, code.UserID, code.ClientID, code.ClientName, code.RedirectURI, code.Resource, code.CodeChallenge,
		microseconds(core.OAuthAuthorizationCodeLifetime)).Scan(&code.CreatedAt, &code.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.OAuthAuthorizationCode{}, "", fmt.Errorf("active identity user %s: %w", request.UserID, store.ErrNotFound)
	}
	if err != nil {
		return core.OAuthAuthorizationCode{}, "", translateBackendConflict(err)
	}
	code.CreatedAt, code.ExpiresAt = code.CreatedAt.UTC(), code.ExpiresAt.UTC()
	return code, value, nil
}

// decisionNow reads fresh database time. A caller reads it after it holds
// every lock its decision needs: now() is fixed at transaction start, so a
// lock wait would carry it past a code, client, or token deadline
// (req-mcp-oauth AC-2.3, AC-3.3, AC-4.1).
func decisionNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

// consumeOAuthCode is the single-use primitive. It locks the code row, reads
// fresh database time, checks expiry, every binding, and the S256 verifier,
// and marks the code consumed. A concurrent exchange waits on the row lock
// and then reads the consumed row.
func consumeOAuthCode(ctx context.Context, tx pgx.Tx, exchange store.OAuthCodeExchange) (core.OAuthAuthorizationCode, error) {
	var code core.OAuthAuthorizationCode
	var stored []byte
	var consumed pgtype.Timestamptz
	hash := core.OAuthCredentialDigest(exchange.Code)
	err := tx.QueryRow(ctx, `SELECT id,code_hash,user_id,client_id,client_name,redirect_uri,resource,code_challenge,created_at,expires_at,
		consumed_at FROM oauth_authorization_codes WHERE code_hash=$1 FOR UPDATE`, hash).
		Scan(&code.ID, &stored, &code.UserID, &code.ClientID, &code.ClientName, &code.RedirectURI, &code.Resource, &code.CodeChallenge, &code.CreatedAt, &code.ExpiresAt, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.OAuthAuthorizationCode{}, store.ErrOAuthCodeInvalid
	}
	if err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	now, err := decisionNow(ctx, tx)
	if err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	if consumed.Valid || !now.Before(code.ExpiresAt) || subtle.ConstantTimeCompare(stored, hash) != 1 || !store.OAuthCodeBindingsMatch(code, exchange) {
		return core.OAuthAuthorizationCode{}, store.ErrOAuthCodeInvalid
	}
	if _, err = tx.Exec(ctx, `UPDATE oauth_authorization_codes SET consumed_at=$2 WHERE id=$1`, code.ID, now); err != nil {
		return core.OAuthAuthorizationCode{}, err
	}
	return code, nil
}

// lockActiveOAuthOwner rechecks the owner's live status under a share lock,
// so a deactivation either commits first and refuses issuance or waits for
// the issuing transaction (req-security-boundaries AC-2.3).
func lockActiveOAuthOwner(ctx context.Context, tx pgx.Tx, userID string) (bool, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return status == "active", err
}

// issueOAuthPair inserts a fresh access and refresh token for the grant and
// makes the refresh token the grant's current one.
func issueOAuthPair(ctx context.Context, tx pgx.Tx, grant core.OAuthGrant, now time.Time) (core.IssuedOAuthTokenPair, error) {
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
	if _, err = tx.Exec(ctx, `INSERT INTO oauth_tokens (id,grant_id,kind,token_hash,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6),($7,$2,$8,$9,$5,$10)`,
		accessID, grant.ID, string(core.OAuthCredentialAccess), accessHash, now, pair.AccessExpiresAt,
		refreshID, string(core.OAuthCredentialRefresh), refreshHash, pair.RefreshExpiresAt); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE oauth_grants SET current_refresh_token_id=$2 WHERE id=$1`, grant.ID, refreshID); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	return pair, nil
}

// insertOAuthEvent appends one OAuth audit event with the context actor. Its
// payload carries IDs and the client name only (component-identity-membership
// invariant 36).
// The identity test hook runs first, after the caller's state change, so a
// test can fail the append and prove that the transaction rolls back.
func insertOAuthEvent(ctx context.Context, q *db.Queries, kind string, grant core.OAuthGrant, at time.Time) error {
	if err := store.RunIdentityTestHook(ctx, store.IdentityHookOAuthAudit); err != nil {
		return err
	}
	actor := store.ActorFromContext(ctx)
	return insertDeploymentEvent(ctx, q, db.InsertDeploymentEventParams{
		Kind: kind, ActorID: actor.ID, ActorRole: string(actor.Role),
		PayloadJson: core.JSONPayload(store.OAuthGrantEventPayload(grant)), At: pgtype.Timestamptz{Time: at, Valid: true},
	})
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
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		code, err := consumeOAuthCode(ctx, tx, exchange)
		if err != nil {
			return err
		}
		if active, err := lockActiveOAuthOwner(ctx, tx, code.UserID); err != nil {
			return err
		} else if !active {
			return store.ErrOAuthCodeInvalid
		}
		var firstUsed pgtype.Timestamptz
		var unusedExpires time.Time
		dynamic := !core.IsOAuthMetadataDocumentClientID(code.ClientID)
		if dynamic {
			err := tx.QueryRow(ctx, `SELECT first_used_at,unused_expires_at FROM oauth_clients WHERE id=$1 FOR UPDATE`, code.ClientID).Scan(&firstUsed, &unusedExpires)
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrOAuthCodeInvalid
			}
			if err != nil {
				return err
			}
		}
		// Fresh time after the code, owner, and client locks decides the
		// unused-client deadline and stamps the grant.
		now, err := decisionNow(ctx, tx)
		if err != nil {
			return err
		}
		if dynamic && !firstUsed.Valid {
			if !now.Before(unusedExpires) {
				return store.ErrOAuthCodeInvalid
			}
			if _, err = tx.Exec(ctx, `UPDATE oauth_clients SET first_used_at=$2 WHERE id=$1 AND first_used_at IS NULL`, code.ClientID, now); err != nil {
				return err
			}
		}
		grant := core.OAuthGrant{ID: grantID, UserID: code.UserID, ClientID: code.ClientID, ClientName: code.ClientName, Resource: code.Resource, CreatedAt: now}
		if _, err = tx.Exec(ctx, `INSERT INTO oauth_grants (id,user_id,client_id,client_name,resource,current_refresh_token_id,created_at) VALUES ($1,$2,$3,$4,$5,'',$6)`,
			grant.ID, grant.UserID, grant.ClientID, grant.ClientName, grant.Resource, now); err != nil {
			return err
		}
		if pair, err = issueOAuthPair(ctx, tx, grant, now); err != nil {
			return err
		}
		return insertOAuthEvent(ctx, q, store.OAuthEventGrantCreated, grant, now)
	})
	if err != nil {
		return core.IssuedOAuthTokenPair{}, translateBackendConflict(err)
	}
	return pair, nil
}

// lockOAuthGrant reads and locks one grant row. It serializes rotation with
// both revocation paths.
func lockOAuthGrant(ctx context.Context, tx pgx.Tx, grantID string) (core.OAuthGrant, string, bool, error) {
	var grant core.OAuthGrant
	var current string
	var lastUsed, revoked pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT id,user_id,client_id,client_name,resource,current_refresh_token_id,created_at,last_used_at,revoked_at FROM oauth_grants WHERE id=$1 FOR UPDATE`, grantID).
		Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.ClientName, &grant.Resource, &current, &grant.CreatedAt, &lastUsed, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.OAuthGrant{}, "", false, nil
	}
	if err != nil {
		return core.OAuthGrant{}, "", false, err
	}
	grant.CreatedAt, grant.LastUsedAt, grant.RevokedAt = grant.CreatedAt.UTC(), utcPtr(lastUsed), utcPtr(revoked)
	return grant, current, true, nil
}

// revokeOAuthGrant revokes a locked, unrevoked grant and appends kind.
func revokeOAuthGrant(ctx context.Context, tx pgx.Tx, q *db.Queries, grant *core.OAuthGrant, kind string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE oauth_grants SET revoked_at=$2 WHERE id=$1`, grant.ID, now); err != nil {
		return err
	}
	grant.RevokedAt = &now
	return insertOAuthEvent(ctx, q, kind, *grant, now)
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var tokenID, grantID, kind string
		var stored []byte
		var expiresAt time.Time
		err := tx.QueryRow(ctx, `SELECT id,grant_id,kind,token_hash,expires_at FROM oauth_tokens WHERE token_hash=$1`, hash).Scan(&tokenID, &grantID, &kind, &stored, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrOAuthTokenInvalid
		}
		if err != nil {
			return err
		}
		if kind != string(core.OAuthCredentialRefresh) || subtle.ConstantTimeCompare(stored, hash) != 1 {
			return store.ErrOAuthTokenInvalid
		}
		grant, current, ok, err := lockOAuthGrant(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if !ok || grant.ClientID != exchange.ClientID || grant.Resource != exchange.Resource || grant.RevokedAt != nil {
			return store.ErrOAuthTokenInvalid
		}
		if active, err := lockActiveOAuthOwner(ctx, tx, grant.UserID); err != nil {
			return err
		} else if !active {
			return store.ErrOAuthTokenInvalid
		}
		now, err := decisionNow(ctx, tx)
		if err != nil {
			return err
		}
		if current != tokenID {
			reused = true
			return revokeOAuthGrant(ctx, tx, q, &grant, store.OAuthEventRefreshReuseDetected, now)
		}
		if !now.Before(expiresAt) {
			return store.ErrOAuthTokenInvalid
		}
		if _, err = tx.Exec(ctx, `UPDATE oauth_tokens SET rotated_at=$2 WHERE id=$1`, tokenID, now); err != nil {
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
// active owner in one indexed read outside any transaction, with no row lock
// (component-identity-membership invariants 30 and 54).
func (s *Store) readOAuthAccess(ctx context.Context, hash []byte) (oauthAccessRead, error) {
	var r oauthAccessRead
	var stored []byte
	err := s.boundary.QueryRow(ctx, `SELECT t.token_hash,g.id,g.user_id,g.client_id,g.client_name,g.resource,g.created_at,
		g.last_used_at IS NULL OR g.last_used_at<now()-$2::bigint*interval '1 microsecond'
		FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id JOIN users u ON u.id=g.user_id
		WHERE t.token_hash=$1 AND t.kind=$3 AND t.expires_at>now() AND g.revoked_at IS NULL AND u.status='active'`,
		hash, microseconds(core.OAuthGrantUseGranularity), string(core.OAuthCredentialAccess)).
		Scan(&stored, &r.grant.ID, &r.grant.UserID, &r.grant.ClientID, &r.grant.ClientName, &r.grant.Resource, &r.grant.CreatedAt, &r.stale)
	if errors.Is(err, pgx.ErrNoRows) {
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
// writes. A stale grant gets one conditional UPDATE that repeats every
// validity predicate; a call whose UPDATE changes nothing rereads, because a
// concurrent call stamped first or the token lost its validity in between.
func (s *Store) VerifyOAuthAccessToken(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "VerifyOAuthAccessToken")
	hash := core.OAuthCredentialDigest(candidate)
	read, err := s.readOAuthAccess(ctx, hash)
	if err != nil {
		return core.AuthenticatedCredential{}, err
	}
	if read.stale {
		tag, err := s.boundary.Exec(ctx, `UPDATE oauth_grants g SET last_used_at=now() FROM oauth_tokens t, users u
			WHERE g.id=$1 AND t.token_hash=$2 AND t.grant_id=g.id AND t.kind=$4 AND t.expires_at>now() AND g.revoked_at IS NULL
			AND u.id=g.user_id AND u.status='active' AND (g.last_used_at IS NULL OR g.last_used_at<now()-$3::bigint*interval '1 microsecond')`,
			read.grant.ID, hash, microseconds(core.OAuthGrantUseGranularity), string(core.OAuthCredentialAccess))
		if err != nil {
			return core.AuthenticatedCredential{}, translateBackendConflict(err)
		}
		if tag.RowsAffected() != 1 {
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
	rows, err := s.boundary.Query(ctx, `SELECT id,user_id,client_id,client_name,resource,created_at,last_used_at,revoked_at FROM oauth_grants
		WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC,id`, userID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	items := []core.OAuthGrant{}
	for rows.Next() {
		var grant core.OAuthGrant
		var lastUsed, revoked pgtype.Timestamptz
		if err = rows.Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.ClientName, &grant.Resource, &grant.CreatedAt, &lastUsed, &revoked); err != nil {
			return nil, translateBackendConflict(err)
		}
		grant.CreatedAt, grant.LastUsedAt, grant.RevokedAt = grant.CreatedAt.UTC(), utcPtr(lastUsed), utcPtr(revoked)
		items = append(items, grant)
	}
	return items, translateBackendConflict(rows.Err())
}

// RevokeOwnOAuthGrant implements store.OAuthStore. Ownership is part of the
// locking read, so another user's grant answers like a missing one.
func (s *Store) RevokeOwnOAuthGrant(ctx context.Context, userID, grantID string) (core.OAuthGrant, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RevokeOwnOAuthGrant")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.OAuthGrant{}, err
	}
	var grant core.OAuthGrant
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
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
		now, err := decisionNow(ctx, tx)
		if err != nil {
			return err
		}
		return revokeOAuthGrant(ctx, tx, q, &grant, store.OAuthEventGrantRevoked, now)
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var grantID string
		err := tx.QueryRow(ctx, `SELECT grant_id FROM oauth_tokens WHERE token_hash=$1`, hash).Scan(&grantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		grant, _, ok, err := lockOAuthGrant(ctx, tx, grantID)
		if err != nil || !ok || grant.ClientID != clientID || grant.RevokedAt != nil {
			return err
		}
		now, err := decisionNow(ctx, tx)
		if err != nil {
			return err
		}
		return revokeOAuthGrant(ctx, tx, q, &grant, store.OAuthEventGrantRevoked, now)
	})
	return translateBackendConflict(err)
}
