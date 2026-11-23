package storetest

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// OAuthTable names one deployment OAuth table for the test-only fixture
// hooks (component-persistence invariant 21).
type OAuthTable string

const (
	OAuthClientsTable            OAuthTable = "oauth_clients"
	OAuthAuthorizationCodesTable OAuthTable = "oauth_authorization_codes"
	OAuthGrantsTable             OAuthTable = "oauth_grants"
	OAuthTokensTable             OAuthTable = "oauth_tokens"
)

// OAuthStoredRow is one stored OAuth row as a test binary reads it: Digest is
// the row's credential digest column, empty for a table without one, and Text
// renders every other column.
type OAuthStoredRow struct {
	Table  OAuthTable
	ID     string
	Digest []byte
	Text   string
}

// OAuth fixture hooks. No production method exposes the deployment audit
// ledger, a stored digest, or a stored time, so each test binary supplies
// these by raw SQL or its own state. The OAuth cases require all three and
// never skip (component-identity-membership invariants 36, 52 to 54).
//
//   - OAuthDeploymentEvents returns the deployment events whose kind starts
//     with identity.oauth_, ascending by (at, id).
//   - AgeOAuthRecord moves every stored time of one row back by the given
//     duration, as if that much time passed for the row alone.
//   - OAuthStoredRows returns every row of the four OAuth tables.
type oauthHooks struct {
	events func(*testing.T, context.Context) []core.Event
	age    func(*testing.T, context.Context, OAuthTable, string, time.Duration)
	rows   func(*testing.T, context.Context) []OAuthStoredRow
}

const (
	oauthResource = "https://conveyor.example.test/mcp"
	oauthRedirect = "http://127.0.0.1:43123/callback"
	oauthVerifier = "conformance-verifier-0123456789-abcdefghijklmnopqrstuvwxyz"
	oauthCIMD     = "https://client.example.test/oauth/client.json"
)

// oauthCase is one fresh fixture with an active owner.
type oauthCase struct {
	x      Fixture
	st     store.Backend
	ctx    context.Context
	owner  core.IdentityUser
	hooks  oauthHooks
	mu     sync.Mutex
	issued []string
}

// note records issued wire values so the secret-free checks can look for
// them. Concurrent cases call it from several goroutines.
func (c *oauthCase) note(values ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issued = append(c.issued, values...)
}

func (c *oauthCase) issuedValues() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.issued)
}

func newOAuthCase(t *testing.T, factory Factory) *oauthCase {
	t.Helper()
	x := factory.fresh(t, nil)
	if x.OAuthDeploymentEvents == nil || x.AgeOAuthRecord == nil || x.OAuthStoredRows == nil {
		t.Fatal("conformance fixture must provide OAuthDeploymentEvents, AgeOAuthRecord, and OAuthStoredRows for the OAuth cases")
	}
	ctx, owner := bootstrapOwner(t, x)
	return &oauthCase{x: x, st: x.Backend, ctx: ctx, owner: owner, hooks: oauthHooks{x.OAuthDeploymentEvents, x.AgeOAuthRecord, x.OAuthStoredRows}}
}

func (c *oauthCase) user(t *testing.T, email string) core.IdentityUser {
	t.Helper()
	user, err := c.st.ProvisionIdentityUser(c.ctx, email, "OAuth")
	requireOK(t, err)
	return user
}

func (c *oauthCase) client(t *testing.T, name string) core.OAuthClient {
	t.Helper()
	client, err := c.st.RegisterOAuthClient(c.ctx, store.OAuthClientRegistration{Name: name, RedirectURIs: []string{oauthRedirect}, GrantTypes: []string{"authorization_code", "refresh_token"}})
	requireOK(t, err)
	return client
}

func (c *oauthCase) code(t *testing.T, userID, clientID, clientName string) (core.OAuthAuthorizationCode, string) {
	t.Helper()
	code, value, err := c.st.IssueOAuthAuthorizationCode(c.ctx, store.OAuthCodeRequest{
		UserID: userID, ClientID: clientID, ClientName: clientName, RedirectURI: oauthRedirect, Resource: oauthResource, CodeChallenge: core.OAuthS256Challenge(oauthVerifier),
	})
	requireOK(t, err)
	c.note(value)
	return code, value
}

func oauthExchange(code, clientID string) store.OAuthCodeExchange {
	return store.OAuthCodeExchange{Code: code, ClientID: clientID, RedirectURI: oauthRedirect, Resource: oauthResource, CodeVerifier: oauthVerifier}
}

func (c *oauthCase) remember(pair core.IssuedOAuthTokenPair) core.IssuedOAuthTokenPair {
	c.note(pair.AccessToken, pair.RefreshToken)
	return pair
}

// grant issues a code for userID and exchanges it.
func (c *oauthCase) grant(t *testing.T, userID, clientID, clientName string) core.IssuedOAuthTokenPair {
	t.Helper()
	_, value := c.code(t, userID, clientID, clientName)
	pair, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(value, clientID))
	requireOK(t, err)
	return c.remember(pair)
}

func (c *oauthCase) refresh(ctx context.Context, pair core.IssuedOAuthTokenPair) (core.IssuedOAuthTokenPair, error) {
	next, err := c.st.RefreshOAuthTokens(ctx, store.OAuthRefreshExchange{RefreshToken: pair.RefreshToken, ClientID: pair.ClientID, Resource: pair.Resource})
	if err == nil {
		c.note(next.AccessToken, next.RefreshToken)
	}
	return next, err
}

func (c *oauthCase) requireAccess(t *testing.T, value string, grant core.OAuthTokenPair) core.AuthenticatedCredential {
	t.Helper()
	credential, err := c.st.VerifyOAuthAccessToken(c.ctx, value)
	requireOK(t, err)
	if credential.ID != grant.GrantID || credential.OwnerUserID != grant.UserID || credential.Method != core.CredentialMethodOAuthMCP || credential.Kind != core.CredentialUser {
		t.Fatalf("access verification=%+v, want grant %s of %s by OAuth", credential, grant.GrantID, grant.UserID)
	}
	return credential
}

func (c *oauthCase) requireAccessRefused(t *testing.T, value, what string) {
	t.Helper()
	if _, err := c.st.VerifyOAuthAccessToken(c.ctx, value); !errors.Is(err, store.ErrOAuthTokenInvalid) || !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("%s: access verification err=%v, want an invalid OAuth token", what, err)
	}
}

func (c *oauthCase) grantOf(t *testing.T, userID, grantID string) (core.OAuthGrant, bool) {
	t.Helper()
	grants, err := c.st.ListOwnOAuthGrants(c.ctx, userID)
	requireOK(t, err)
	for _, grant := range grants {
		if grant.ID == grantID {
			return grant, true
		}
	}
	return core.OAuthGrant{}, false
}

func (c *oauthCase) events(t *testing.T, kind string) []core.Event {
	t.Helper()
	var out []core.Event
	for _, event := range c.hooks.events(t, c.ctx) {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func requireOAuthError(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err=%v, want %v", what, err, want)
	}
}

// noActor is a context with no actor: every OAuth writer that appends an
// audit event must refuse it before any state changes (component-persistence
// invariant 17).
func noActor(t *testing.T) context.Context { return t.Context() }

// runOAuth proves the OAuth store contract on every backend
// (req-mcp-oauth AC-2.2 to AC-2.3, AC-3.3, AC-4.1 to AC-4.5;
// req-security-boundaries AC-1.4, AC-2.1, AC-2.3; DEC-146; DEC-147;
// component-identity-membership invariants 30, 35, 36, 48, and 51 to 54).
func runOAuth(t *testing.T, factory Factory) {
	t.Run("DynamicClients", func(t *testing.T) { runOAuthDynamicClients(t, newOAuthCase(t, factory)) })
	t.Run("CodeExchange", func(t *testing.T) { runOAuthCodeExchange(t, newOAuthCase(t, factory)) })
	t.Run("ConcurrentCodeExchange", func(t *testing.T) { runOAuthConcurrentCodeExchange(t, newOAuthCase(t, factory)) })
	t.Run("Refresh", func(t *testing.T) { runOAuthRefresh(t, newOAuthCase(t, factory)) })
	t.Run("ConcurrentRefresh", func(t *testing.T) { runOAuthConcurrentRefresh(t, newOAuthCase(t, factory)) })
	t.Run("RefreshRacesReplay", func(t *testing.T) { runOAuthRefreshRacesReplay(t, newOAuthCase(t, factory)) })
	t.Run("AccessVerification", func(t *testing.T) { runOAuthAccessVerification(t, newOAuthCase(t, factory)) })
	t.Run("OwnerGrants", func(t *testing.T) { runOAuthOwnerGrants(t, newOAuthCase(t, factory)) })
	t.Run("TokenRevocation", func(t *testing.T) { runOAuthTokenRevocation(t, newOAuthCase(t, factory)) })
	t.Run("SecretFreeStorageAndAudit", func(t *testing.T) { runOAuthSecretFree(t, newOAuthCase(t, factory)) })
	t.Run("LongClientID", func(t *testing.T) { runOAuthLongClientID(t, newOAuthCase(t, factory)) })
	t.Run("AuditFailureRollsBack", func(t *testing.T) { runOAuthAuditFailure(t, newOAuthCase(t, factory)) })
}

// runOAuthDynamicClients proves registration, live lookup, the 24-hour
// unused deadline, cleanup, and that a used client outlives its deadline
// with its grant (req-mcp-oauth AC-2.2, AC-2.3).
func runOAuthDynamicClients(t *testing.T, c *oauthCase) {
	client := c.client(t, "Conformance Client")
	if client.ID == "" || core.IsOAuthMetadataDocumentClientID(client.ID) || client.Name != "Conformance Client" ||
		!slices.Equal(client.RedirectURIs, []string{oauthRedirect}) || !slices.Equal(client.GrantTypes, []string{"authorization_code", "refresh_token"}) ||
		client.FirstUsedAt != nil || client.UnusedExpiresAt.Sub(client.CreatedAt) != core.OAuthUnusedClientLifetime {
		t.Fatalf("registered client=%+v", client)
	}
	read, err := c.st.GetOAuthClient(c.ctx, client.ID)
	requireOK(t, err)
	if read.ID != client.ID || read.Name != client.Name || !slices.Equal(read.RedirectURIs, client.RedirectURIs) ||
		!read.CreatedAt.Equal(client.CreatedAt) || !read.UnusedExpiresAt.Equal(client.UnusedExpiresAt) || read.FirstUsedAt != nil {
		t.Fatalf("client lookup=%+v, want %+v", read, client)
	}
	if _, err := c.st.RegisterOAuthClient(c.ctx, store.OAuthClientRegistration{Name: "No Redirect"}); !errors.Is(err, store.ErrRefusal) {
		t.Fatalf("registration without a redirect URI err=%v, want a refusal", err)
	}
	if _, err := c.st.GetOAuthClient(c.ctx, "ocl_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown client lookup err=%v", err)
	}

	used := c.client(t, "Used Client")
	pair := c.grant(t, c.owner.ID, used.ID, used.Name)
	read, err = c.st.GetOAuthClient(c.ctx, used.ID)
	requireOK(t, err)
	if read.FirstUsedAt == nil {
		t.Fatal("a client's first grant left its first use unset")
	}
	stale := c.client(t, "Stale Client")
	_, staleCode := c.code(t, c.owner.ID, stale.ID, stale.Name)
	for _, id := range []string{client.ID, used.ID, stale.ID} {
		c.hooks.age(t, c.ctx, OAuthClientsTable, id, core.OAuthUnusedClientLifetime+time.Minute)
	}
	if _, err := c.st.GetOAuthClient(c.ctx, client.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired unused client lookup err=%v, want not found", err)
	}
	if _, err := c.st.GetOAuthClient(c.ctx, used.ID); err != nil {
		t.Fatalf("a used client past its unused deadline is refused: %v", err)
	}
	requireOAuthError(t, func() error {
		_, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(staleCode, stale.ID))
		return err
	}(),
		store.ErrOAuthCodeInvalid, "grant creation for an expired unused client")
	removed, err := c.st.DeleteExpiredOAuthClients(c.ctx)
	requireOK(t, err)
	if removed != 2 {
		t.Fatalf("cleanup removed %d clients, want the two expired unused clients", removed)
	}
	if _, err := c.st.GetOAuthClient(c.ctx, used.ID); err != nil {
		t.Fatalf("cleanup removed a client backing a grant: %v", err)
	}
	if _, err := c.st.GetOAuthClient(c.ctx, stale.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleaned client lookup err=%v", err)
	}
	c.requireAccess(t, pair.AccessToken, pair.OAuthTokenPair)
	if _, err := c.refresh(c.ctx, pair); err != nil {
		t.Fatalf("cleanup affected an active grant: %v", err)
	}
	if removed, err = c.st.DeleteExpiredOAuthClients(c.ctx); err != nil || removed != 0 {
		t.Fatalf("second cleanup removed=%d err=%v", removed, err)
	}
}

// runOAuthCodeExchange proves code issuance, the 60-second deadline, every
// binding refusal, single use, the first pair's lifetimes, live owner status,
// and that a missing actor consumes nothing (req-mcp-oauth AC-3.3, AC-4.1).
func runOAuthCodeExchange(t *testing.T, c *oauthCase) {
	client := c.client(t, "Exchange Client")
	before := time.Now().UTC()
	code, value := c.code(t, c.owner.ID, client.ID, client.Name)
	if kind, ok := core.OAuthCredentialKindOf(value); !ok || kind != core.OAuthCredentialCode || !strings.HasPrefix(value, "cv_"+code.ID+"_") {
		t.Fatalf("code value does not have the cv_oac_<id>_<secret> shape for %s", code.ID)
	}
	if code.UserID != c.owner.ID || code.ClientID != client.ID || code.ClientName != client.Name || code.RedirectURI != oauthRedirect ||
		code.Resource != oauthResource || code.CodeChallenge != core.OAuthS256Challenge(oauthVerifier) ||
		code.ExpiresAt.Sub(code.CreatedAt) != core.OAuthAuthorizationCodeLifetime {
		t.Fatalf("issued code=%+v", code)
	}
	for _, refused := range []struct {
		name     string
		exchange store.OAuthCodeExchange
	}{
		{"unknown code", oauthExchange("cv_oac_unknown_value", client.ID)},
		{"other client", store.OAuthCodeExchange{Code: value, ClientID: oauthCIMD, RedirectURI: oauthRedirect, Resource: oauthResource, CodeVerifier: oauthVerifier}},
		{"other redirect URI", store.OAuthCodeExchange{Code: value, ClientID: client.ID, RedirectURI: oauthRedirect + "/other", Resource: oauthResource, CodeVerifier: oauthVerifier}},
		{"other resource", store.OAuthCodeExchange{Code: value, ClientID: client.ID, RedirectURI: oauthRedirect, Resource: oauthResource + "/other", CodeVerifier: oauthVerifier}},
		{"wrong verifier", store.OAuthCodeExchange{Code: value, ClientID: client.ID, RedirectURI: oauthRedirect, Resource: oauthResource, CodeVerifier: oauthVerifier + "x"}},
		{"no verifier", store.OAuthCodeExchange{Code: value, ClientID: client.ID, RedirectURI: oauthRedirect, Resource: oauthResource}},
	} {
		_, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, refused.exchange)
		requireOAuthError(t, err, store.ErrOAuthCodeInvalid, refused.name)
	}
	// A missing actor fails before anything commits: the code stays unused.
	if _, err := c.st.ExchangeOAuthAuthorizationCode(noActor(t), oauthExchange(value, client.ID)); err == nil {
		t.Fatal("an exchange without an actor succeeded")
	}
	if got := c.events(t, store.OAuthEventGrantCreated); len(got) != 0 {
		t.Fatalf("refused exchanges appended %d grant events", len(got))
	}
	pair, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(value, client.ID))
	requireOK(t, err)
	after := time.Now().UTC()
	c.remember(pair)
	if pair.UserID != c.owner.ID || pair.ClientID != client.ID || pair.Resource != oauthResource || pair.GrantID == "" {
		t.Fatalf("first pair=%+v", pair.OAuthTokenPair)
	}
	if kind, _ := core.OAuthCredentialKindOf(pair.AccessToken); kind != core.OAuthCredentialAccess || !strings.HasPrefix(pair.AccessToken, "cv_"+pair.AccessTokenID+"_") {
		t.Fatalf("access token does not have the cv_oat_<id>_<secret> shape for %s", pair.AccessTokenID)
	}
	if kind, _ := core.OAuthCredentialKindOf(pair.RefreshToken); kind != core.OAuthCredentialRefresh || !strings.HasPrefix(pair.RefreshToken, "cv_"+pair.RefreshTokenID+"_") {
		t.Fatalf("refresh token does not have the cv_ort_<id>_<secret> shape for %s", pair.RefreshTokenID)
	}
	if pair.RefreshExpiresAt.Sub(pair.AccessExpiresAt) != core.OAuthRefreshTokenLifetime-core.OAuthAccessTokenLifetime {
		t.Fatalf("pair lifetimes differ: access=%s refresh=%s", pair.AccessExpiresAt, pair.RefreshExpiresAt)
	}
	// The database clock may differ from the test clock by a little.
	const skew = time.Minute
	if pair.AccessExpiresAt.Before(before.Add(core.OAuthAccessTokenLifetime-skew)) || pair.AccessExpiresAt.After(after.Add(core.OAuthAccessTokenLifetime+skew)) {
		t.Fatalf("access expiry %s is not one hour after issuance between %s and %s", pair.AccessExpiresAt, before, after)
	}
	c.requireAccess(t, pair.AccessToken, pair.OAuthTokenPair)
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(value, client.ID))
	requireOAuthError(t, err, store.ErrOAuthCodeInvalid, "replayed code")
	created := c.events(t, store.OAuthEventGrantCreated)
	if len(created) != 1 {
		t.Fatalf("grant events=%d, want one", len(created))
	}
	requirePayload(t, created[0], map[string]any{"grant_id": pair.GrantID, "user_id": c.owner.ID, "client_id": client.ID, "client_name": client.Name})

	_, expired := c.code(t, c.owner.ID, client.ID, client.Name)
	expiredCode, _ := c.codeID(t, expired)
	c.hooks.age(t, c.ctx, OAuthAuthorizationCodesTable, expiredCode, core.OAuthAuthorizationCodeLifetime+time.Second)
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(expired, client.ID))
	requireOAuthError(t, err, store.ErrOAuthCodeInvalid, "expired code")

	// A metadata-document client needs no stored registration; an unknown
	// dynamic client ID is refused.
	pairCIMD := c.grant(t, c.owner.ID, oauthCIMD, "Metadata Client")
	if grant, ok := c.grantOf(t, c.owner.ID, pairCIMD.GrantID); !ok || grant.ClientID != oauthCIMD || grant.ClientName != "Metadata Client" {
		t.Fatalf("metadata-document grant=%+v listed=%t", grant, ok)
	}
	_, unknown := c.code(t, c.owner.ID, "ocl_unregistered", "Unregistered")
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(unknown, "ocl_unregistered"))
	requireOAuthError(t, err, store.ErrOAuthCodeInvalid, "unregistered dynamic client")

	// Live owner status is checked again at the exchange.
	member := c.user(t, "oauth-exchange-member@example.test")
	_, memberCode := c.code(t, member.ID, client.ID, client.Name)
	deactivateUser(t, c.x, c.ctx, member.ID)
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(memberCode, client.ID))
	requireOAuthError(t, err, store.ErrOAuthCodeInvalid, "deactivated owner")
	if _, _, err := c.st.IssueOAuthAuthorizationCode(c.ctx, store.OAuthCodeRequest{UserID: member.ID, ClientID: client.ID, ClientName: client.Name, RedirectURI: oauthRedirect, Resource: oauthResource, CodeChallenge: core.OAuthS256Challenge(oauthVerifier)}); err == nil {
		t.Fatal("a code was issued for a deactivated user")
	}
	if _, _, err := c.st.IssueOAuthAuthorizationCode(c.ctx, store.OAuthCodeRequest{UserID: c.owner.ID, ClientID: client.ID, ClientName: client.Name, RedirectURI: oauthRedirect, Resource: oauthResource, CodeChallenge: "plain"}); !errors.Is(err, store.ErrRefusal) {
		t.Fatalf("a code without an S256 challenge err=%v, want a refusal", err)
	}
}

// codeID finds a stored code row by its wire value's digest.
func (c *oauthCase) codeID(t *testing.T, value string) (string, bool) {
	t.Helper()
	digest := core.OAuthCredentialDigest(value)
	for _, row := range c.hooks.rows(t, c.ctx) {
		if row.Table == OAuthAuthorizationCodesTable && string(row.Digest) == string(digest) {
			return row.ID, true
		}
	}
	t.Fatal("issued code row is missing")
	return "", false
}

// runOAuthConcurrentCodeExchange proves exactly one of many concurrent
// exchanges of one code wins, with one grant and one audit event
// (req-mcp-oauth AC-4.1).
func runOAuthConcurrentCodeExchange(t *testing.T, c *oauthCase) {
	client := c.client(t, "Race Client")
	_, value := c.code(t, c.owner.ID, client.ID, client.Name)
	const callers = 8
	var wg sync.WaitGroup
	results := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pair, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(value, client.ID))
			if err == nil && pair.AccessToken == "" {
				err = errors.New("empty pair")
			}
			results[i] = err
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrOAuthCodeInvalid), errors.Is(err, store.ErrRetryable):
		default:
			t.Fatalf("concurrent exchange err=%v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent exchanges won, want exactly one: %v", winners, results)
	}
	grants, err := c.st.ListOwnOAuthGrants(c.ctx, c.owner.ID)
	requireOK(t, err)
	if len(grants) != 1 || len(c.events(t, store.OAuthEventGrantCreated)) != 1 {
		t.Fatalf("one consumed code created %d grants and %d events", len(grants), len(c.events(t, store.OAuthEventGrantCreated)))
	}
}

// runOAuthRefresh proves rotation, binding refusals without rotation, owner
// and grant state refusals, expiry, and replay revocation of the whole grant
// that commits even though the call fails (req-mcp-oauth AC-4.2, AC-4.4).
func runOAuthRefresh(t *testing.T, c *oauthCase) {
	client := c.client(t, "Refresh Client")
	first := c.grant(t, c.owner.ID, client.ID, client.Name)
	for _, refused := range []struct {
		name     string
		exchange store.OAuthRefreshExchange
	}{
		{"other client", store.OAuthRefreshExchange{RefreshToken: first.RefreshToken, ClientID: oauthCIMD, Resource: oauthResource}},
		{"other resource", store.OAuthRefreshExchange{RefreshToken: first.RefreshToken, ClientID: client.ID, Resource: oauthResource + "/other"}},
		{"access token", store.OAuthRefreshExchange{RefreshToken: first.AccessToken, ClientID: client.ID, Resource: oauthResource}},
		{"unknown token", store.OAuthRefreshExchange{RefreshToken: "cv_ort_unknown_value", ClientID: client.ID, Resource: oauthResource}},
	} {
		_, err := c.st.RefreshOAuthTokens(c.ctx, refused.exchange)
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, refused.name)
	}
	if _, err := c.refresh(noActor(t), first); err == nil {
		t.Fatal("a refresh without an actor succeeded")
	}
	second, err := c.refresh(c.ctx, first)
	requireOK(t, err)
	if second.GrantID != first.GrantID || second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatalf("rotation did not issue a fresh pair on the same grant: %+v", second.OAuthTokenPair)
	}
	c.requireAccess(t, second.AccessToken, second.OAuthTokenPair)
	// A rotated token presented with the wrong client or resource is refused
	// without revoking anything.
	_, err = c.st.RefreshOAuthTokens(c.ctx, store.OAuthRefreshExchange{RefreshToken: first.RefreshToken, ClientID: oauthCIMD, Resource: oauthResource})
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "rotated token for another client")
	c.requireAccess(t, second.AccessToken, second.OAuthTokenPair)
	// A replay whose audit cannot commit leaves the grant live and does not
	// claim a committed reuse refusal.
	if _, err := c.refresh(noActor(t), first); err == nil || errors.Is(err, store.ErrOAuthRefreshTokenReused) {
		t.Fatalf("a replay without an actor err=%v, want a failure that is not a committed reuse refusal", err)
	}
	c.requireAccess(t, second.AccessToken, second.OAuthTokenPair)
	if got := c.events(t, store.OAuthEventRefreshReuseDetected); len(got) != 0 {
		t.Fatalf("a rolled-back replay appended %d reuse events", len(got))
	}

	_, err = c.refresh(c.ctx, first)
	requireOAuthError(t, err, store.ErrOAuthRefreshTokenReused, "replayed refresh token")
	c.requireAccessRefused(t, first.AccessToken, "first access token after replay")
	c.requireAccessRefused(t, second.AccessToken, "rotated access token after replay")
	_, err = c.refresh(c.ctx, second)
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "current refresh token after replay")
	_, err = c.refresh(c.ctx, first)
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "second replay of a revoked grant")
	if _, ok := c.grantOf(t, c.owner.ID, first.GrantID); ok {
		t.Fatal("a revoked grant is still listed")
	}
	reuse := c.events(t, store.OAuthEventRefreshReuseDetected)
	if len(reuse) != 1 {
		t.Fatalf("reuse events=%d, want one", len(reuse))
	}
	requirePayload(t, reuse[0], map[string]any{"grant_id": first.GrantID, "user_id": c.owner.ID, "client_id": client.ID, "client_name": client.Name})

	t.Run("InactiveOwner", func(t *testing.T) {
		member := c.user(t, "oauth-refresh-member@example.test")
		pair := c.grant(t, member.ID, client.ID, client.Name)
		deactivateUser(t, c.x, c.ctx, member.ID)
		_, err := c.refresh(c.ctx, pair)
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh for a deactivated owner")
		c.requireAccessRefused(t, pair.AccessToken, "access for a deactivated owner")
	})
	t.Run("RevokedGrant", func(t *testing.T) {
		pair := c.grant(t, c.owner.ID, client.ID, client.Name)
		_, err := c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, pair.GrantID)
		requireOK(t, err)
		_, err = c.refresh(c.ctx, pair)
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh of a revoked grant")
	})
	t.Run("Expired", func(t *testing.T) {
		pair := c.grant(t, c.owner.ID, client.ID, client.Name)
		c.hooks.age(t, c.ctx, OAuthTokensTable, pair.RefreshTokenID, core.OAuthRefreshTokenLifetime+time.Minute)
		_, err := c.refresh(c.ctx, pair)
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "expired refresh token")
	})
}

// runOAuthConcurrentRefresh proves one rotation winner among concurrent
// refreshes of one token. Every loser presents a rotated token, so the grant
// family ends revoked and the winner's pair fails at next use
// (req-mcp-oauth AC-4.2).
func runOAuthConcurrentRefresh(t *testing.T, c *oauthCase) {
	client := c.client(t, "Concurrent Refresh Client")
	pair := c.grant(t, c.owner.ID, client.ID, client.Name)
	const callers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []core.IssuedOAuthTokenPair
	reused := 0
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			next, err := c.refresh(c.ctx, pair)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, next)
			case errors.Is(err, store.ErrOAuthRefreshTokenReused):
				reused++
			case errors.Is(err, store.ErrOAuthTokenInvalid), errors.Is(err, store.ErrRetryable):
			default:
				t.Errorf("concurrent refresh err=%v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d concurrent refreshes rotated, want exactly one", len(winners))
	}
	if events := len(c.events(t, store.OAuthEventRefreshReuseDetected)); reused > 0 && events != 1 || reused == 0 && events != 0 {
		t.Fatalf("reuse refusals=%d events=%d, want one event for any reuse", reused, events)
	}
	if reused > 0 {
		c.requireAccessRefused(t, winners[0].AccessToken, "winning pair after a concurrent replay")
		_, err := c.refresh(c.ctx, winners[0])
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "winning refresh token after a concurrent replay")
	}
}

// runOAuthRefreshRacesReplay races a legitimate refresh with a replay of the
// rotated token. The replay always commits the revocation, so neither the
// old nor any newly issued token survives (req-mcp-oauth AC-4.2).
func runOAuthRefreshRacesReplay(t *testing.T, c *oauthCase) {
	client := c.client(t, "Replay Race Client")
	first := c.grant(t, c.owner.ID, client.ID, client.Name)
	second, err := c.refresh(c.ctx, first)
	requireOK(t, err)
	var wg sync.WaitGroup
	var legit core.IssuedOAuthTokenPair
	var legitErr, replayErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; legit, legitErr = c.refresh(c.ctx, second) }()
	go func() { defer wg.Done(); <-start; _, replayErr = c.refresh(c.ctx, first) }()
	close(start)
	wg.Wait()
	requireOAuthError(t, replayErr, store.ErrOAuthRefreshTokenReused, "racing replay")
	values := []string{first.AccessToken, second.AccessToken}
	if legitErr == nil {
		values = append(values, legit.AccessToken)
		_, err := c.refresh(c.ctx, legit)
		requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh token issued before the replay committed")
	} else {
		requireOAuthError(t, legitErr, store.ErrOAuthTokenInvalid, "refresh after the replay committed")
	}
	for _, value := range values {
		c.requireAccessRefused(t, value, "access token after a racing replay")
	}
}

// runOAuthAccessVerification proves live verification and the throttled
// grant stamp, and that the PAT verifier and the OAuth verifier accept only
// their own credentials (req-mcp-oauth AC-4.3, AC-4.4;
// req-security-boundaries AC-1.4; component-identity-membership invariants
// 30, 35, and 54).
func runOAuthAccessVerification(t *testing.T, c *oauthCase) {
	client := c.client(t, "Access Client")
	pair := c.grant(t, c.owner.ID, client.ID, client.Name)
	if grant, _ := c.grantOf(t, c.owner.ID, pair.GrantID); grant.LastUsedAt != nil {
		t.Fatalf("a new grant has a last use %s", grant.LastUsedAt)
	}
	c.requireAccess(t, pair.AccessToken, pair.OAuthTokenPair)
	grant, _ := c.grantOf(t, c.owner.ID, pair.GrantID)
	if grant.LastUsedAt == nil {
		t.Fatal("the first verification did not stamp the grant")
	}
	stamped := *grant.LastUsedAt
	var wg sync.WaitGroup
	concurrent := make([]error, 4)
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credential, err := c.st.VerifyOAuthAccessToken(c.ctx, pair.AccessToken)
			if err == nil && credential.ID != pair.GrantID {
				err = errors.New("verification resolved another grant")
			}
			concurrent[i] = err
		}()
	}
	wg.Wait()
	for _, err := range concurrent {
		requireOK(t, err)
	}
	// Every token of the grant shares the one-minute cutoff.
	rotated, err := c.refresh(c.ctx, pair)
	requireOK(t, err)
	c.requireAccess(t, rotated.AccessToken, rotated.OAuthTokenPair)
	if grant, _ = c.grantOf(t, c.owner.ID, pair.GrantID); grant.LastUsedAt == nil || !grant.LastUsedAt.Equal(stamped) {
		t.Fatalf("verifications inside one minute moved the stamp from %s to %v", stamped, grant.LastUsedAt)
	}
	c.hooks.age(t, c.ctx, OAuthGrantsTable, pair.GrantID, 2*core.OAuthGrantUseGranularity)
	aged, _ := c.grantOf(t, c.owner.ID, pair.GrantID)
	c.requireAccess(t, rotated.AccessToken, rotated.OAuthTokenPair)
	if grant, _ = c.grantOf(t, c.owner.ID, pair.GrantID); grant.LastUsedAt == nil || !grant.LastUsedAt.After(*aged.LastUsedAt) {
		t.Fatalf("a verification after the cutoff did not stamp: before=%v after=%v", aged.LastUsedAt, grant.LastUsedAt)
	}

	c.requireAccessRefused(t, rotated.RefreshToken, "refresh token as access token")
	_, code := c.code(t, c.owner.ID, client.ID, client.Name)
	c.requireAccessRefused(t, code, "authorization code as access token")
	c.requireAccessRefused(t, "cv_oat_unknown_value", "unknown access token")
	// The PAT verifier refuses an OAuth token, and the OAuth verifier
	// refuses a PAT, while the PAT keeps its bearer method.
	if _, err := c.st.VerifyCredential(c.ctx, rotated.AccessToken); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("VerifyCredential accepted an OAuth access token: %v", err)
	}
	pat, err := c.st.IssueOwnPersonalAccessToken(c.ctx, c.owner.ID, "oauth-conformance")
	requireOK(t, err)
	c.requireAccessRefused(t, pat.Value, "personal access token")
	if credential, err := c.st.VerifyCredential(c.ctx, pat.Value); err != nil || credential.Method != core.CredentialMethodBearer || credential.ID != pat.ID {
		t.Fatalf("personal access token verification=%+v err=%v", credential, err)
	}

	expiring := c.grant(t, c.owner.ID, client.ID, client.Name)
	c.hooks.age(t, c.ctx, OAuthTokensTable, expiring.AccessTokenID, core.OAuthAccessTokenLifetime+time.Second)
	c.requireAccessRefused(t, expiring.AccessToken, "expired access token")

	member := c.user(t, "oauth-access-member@example.test")
	memberPair := c.grant(t, member.ID, client.ID, client.Name)
	c.requireAccess(t, memberPair.AccessToken, memberPair.OAuthTokenPair)
	deactivateUser(t, c.x, c.ctx, member.ID)
	c.requireAccessRefused(t, memberPair.AccessToken, "access token of a deactivated owner")
}

// runOAuthOwnerGrants proves owner-scoped, hash-free listing and owner
// revocation, with ErrNotFound for foreign and missing grants
// (req-mcp-oauth AC-4.4, AC-4.5).
func runOAuthOwnerGrants(t *testing.T, c *oauthCase) {
	client := c.client(t, "Listed Client")
	other := c.user(t, "oauth-other@example.test")
	mine := c.grant(t, c.owner.ID, client.ID, client.Name)
	theirs := c.grant(t, other.ID, client.ID, client.Name)
	listed, err := c.st.ListOwnOAuthGrants(c.ctx, c.owner.ID)
	requireOK(t, err)
	if len(listed) != 1 || listed[0].ID != mine.GrantID || listed[0].UserID != c.owner.ID || listed[0].ClientID != client.ID ||
		listed[0].ClientName != client.Name || listed[0].Resource != oauthResource || listed[0].CreatedAt.IsZero() || listed[0].RevokedAt != nil {
		t.Fatalf("owner listing=%+v", listed)
	}
	requireSecretFree(t, c, "grant listing", listed)
	if _, err := c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, theirs.GrantID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign grant revocation err=%v, want not found", err)
	}
	if _, err := c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, "ogr_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing grant revocation err=%v, want not found", err)
	}
	c.requireAccess(t, theirs.AccessToken, theirs.OAuthTokenPair)
	if _, err := c.st.RevokeOwnOAuthGrant(noActor(t), c.owner.ID, mine.GrantID); err == nil {
		t.Fatal("a revocation without an actor succeeded")
	}
	c.requireAccess(t, mine.AccessToken, mine.OAuthTokenPair)
	revoked, err := c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, mine.GrantID)
	requireOK(t, err)
	if revoked.ID != mine.GrantID || revoked.RevokedAt == nil {
		t.Fatalf("revoked grant=%+v", revoked)
	}
	c.requireAccessRefused(t, mine.AccessToken, "access token of a revoked grant")
	_, err = c.refresh(c.ctx, mine)
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh token of a revoked grant")
	if again, err := c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, mine.GrantID); err != nil || again.RevokedAt == nil {
		t.Fatalf("repeated revocation=%+v err=%v", again, err)
	}
	if listed, err = c.st.ListOwnOAuthGrants(c.ctx, c.owner.ID); err != nil || len(listed) != 0 {
		t.Fatalf("listing after revocation=%+v err=%v", listed, err)
	}
	revokedEvents := c.events(t, store.OAuthEventGrantRevoked)
	if len(revokedEvents) != 1 {
		t.Fatalf("revocation events=%d, want one for the one revocation", len(revokedEvents))
	}
	requirePayload(t, revokedEvents[0], map[string]any{"grant_id": mine.GrantID, "user_id": c.owner.ID, "client_id": client.ID, "client_name": client.Name})
	c.requireAccess(t, theirs.AccessToken, theirs.OAuthTokenPair)
}

// runOAuthTokenRevocation proves RFC 7009 revocation by either token kind:
// the whole grant fails at next use, and an unknown token, another client's
// token, or a repeat changes nothing (req-mcp-oauth AC-4.4).
func runOAuthTokenRevocation(t *testing.T, c *oauthCase) {
	client := c.client(t, "Revoking Client")
	byAccess := c.grant(t, c.owner.ID, client.ID, client.Name)
	byRefresh := c.grant(t, c.owner.ID, client.ID, client.Name)
	requireOK(t, c.st.RevokeOAuthToken(c.ctx, client.ID, "cv_oat_unknown_value"))
	requireOK(t, c.st.RevokeOAuthToken(c.ctx, oauthCIMD, byAccess.AccessToken))
	c.requireAccess(t, byAccess.AccessToken, byAccess.OAuthTokenPair)
	if err := c.st.RevokeOAuthToken(noActor(t), client.ID, byAccess.AccessToken); err == nil {
		t.Fatal("a token revocation without an actor succeeded")
	}
	c.requireAccess(t, byAccess.AccessToken, byAccess.OAuthTokenPair)
	if got := c.events(t, store.OAuthEventGrantRevoked); len(got) != 0 {
		t.Fatalf("no-op revocations appended %d events", len(got))
	}

	requireOK(t, c.st.RevokeOAuthToken(c.ctx, client.ID, byAccess.AccessToken))
	c.requireAccessRefused(t, byAccess.AccessToken, "revoked access token")
	_, err := c.refresh(c.ctx, byAccess)
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh token of a grant revoked by its access token")
	requireOK(t, c.st.RevokeOAuthToken(c.ctx, client.ID, byAccess.RefreshToken))

	requireOK(t, c.st.RevokeOAuthToken(c.ctx, client.ID, byRefresh.RefreshToken))
	c.requireAccessRefused(t, byRefresh.AccessToken, "access token of a grant revoked by its refresh token")
	_, err = c.refresh(c.ctx, byRefresh)
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "revoked refresh token")
	requireOK(t, c.st.RevokeOAuthToken(c.ctx, client.ID, byRefresh.AccessToken))
	if got := c.events(t, store.OAuthEventGrantRevoked); len(got) != 2 {
		t.Fatalf("revocation events=%d, want one per revoked grant", len(got))
	}
}

// runOAuthSecretFree proves that every stored credential is a 32-byte
// SHA-256 digest of its wire value, that no stored column holds a value, and
// that every OAuth audit event carries exactly the allowed fields
// (req-security-boundaries AC-2.1; component-identity-membership invariant
// 36; component-persistence invariant 41).
func runOAuthSecretFree(t *testing.T, c *oauthCase) {
	client := c.client(t, "Secret Client")
	pair := c.grant(t, c.owner.ID, client.ID, client.Name)
	_, err := c.refresh(c.ctx, pair)
	requireOK(t, err)
	_, err = c.refresh(c.ctx, pair)
	requireOAuthError(t, err, store.ErrOAuthRefreshTokenReused, "replay")
	revoked := c.grant(t, c.owner.ID, client.ID, client.Name)
	_, err = c.st.RevokeOwnOAuthGrant(c.ctx, c.owner.ID, revoked.GrantID)
	requireOK(t, err)
	_, cleanCode := c.code(t, c.owner.ID, client.ID, client.Name)
	// Untrusted client metadata that the shared redactor would change is
	// refused before any write, so no stored row, grant listing, or audit
	// event can carry it (req-security-boundaries REQ-3;
	// component-identity-membership invariants 36 and 45). Clean values keep
	// exact-equality binding, which the other cases prove.
	const (
		leakedGitHub = "ghp_ConformanceLeak0123456789abcdefABCDEF"
		leakedKey    = "sk-conformance-leak-0123456789"
	)
	leakyID := "https://client.example.test/" + leakedGitHub + "/client.json"
	leakyName := "Leaky " + leakedKey
	leakyRedirect := oauthRedirect + "?k=" + leakedGitHub
	rowsBefore := len(c.hooks.rows(t, c.ctx))
	eventsBefore := len(c.hooks.events(t, c.ctx))
	requireRefused := func(t *testing.T, err, want error, what string) {
		t.Helper()
		if !errors.Is(err, want) || !errors.Is(err, store.ErrRefusal) {
			t.Fatalf("%s err=%v, want %v as a refusal", what, err, want)
		}
	}
	_, err = c.st.RegisterOAuthClient(c.ctx, store.OAuthClientRegistration{Name: leakyName, RedirectURIs: []string{oauthRedirect}})
	requireRefused(t, err, store.ErrOAuthClientMetadataRefused, "registration with a secret-shaped name")
	_, err = c.st.RegisterOAuthClient(c.ctx, store.OAuthClientRegistration{Name: "Clean", RedirectURIs: []string{oauthRedirect, leakyRedirect}})
	requireRefused(t, err, store.ErrOAuthRedirectURIRefused, "registration with a secret-shaped redirect URI")
	for _, refused := range []struct {
		name    string
		request store.OAuthCodeRequest
		want    error
	}{
		{"code with a secret-shaped client ID", store.OAuthCodeRequest{ClientID: leakyID, ClientName: "Clean", RedirectURI: oauthRedirect}, store.ErrOAuthClientMetadataRefused},
		{"code with a secret-shaped client name", store.OAuthCodeRequest{ClientID: oauthCIMD, ClientName: leakyName, RedirectURI: oauthRedirect}, store.ErrOAuthClientMetadataRefused},
		{"code with a secret-shaped redirect URI", store.OAuthCodeRequest{ClientID: oauthCIMD, ClientName: "Clean", RedirectURI: leakyRedirect}, store.ErrOAuthRedirectURIRefused},
	} {
		refused.request.UserID, refused.request.Resource, refused.request.CodeChallenge = c.owner.ID, oauthResource, core.OAuthS256Challenge(oauthVerifier)
		_, _, err = c.st.IssueOAuthAuthorizationCode(c.ctx, refused.request)
		requireRefused(t, err, refused.want, refused.name)
	}
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(cleanCode, leakyID))
	requireRefused(t, err, store.ErrOAuthClientMetadataRefused, "grant with a secret-shaped client ID")
	_, err = c.st.ExchangeOAuthAuthorizationCode(c.ctx, store.OAuthCodeExchange{Code: cleanCode, ClientID: client.ID, RedirectURI: leakyRedirect, Resource: oauthResource, CodeVerifier: oauthVerifier})
	requireRefused(t, err, store.ErrOAuthRedirectURIRefused, "grant with a secret-shaped redirect URI")
	if rows, events := len(c.hooks.rows(t, c.ctx)), len(c.hooks.events(t, c.ctx)); rows != rowsBefore || events != eventsBefore {
		t.Fatalf("refusals changed stored rows from %d to %d and events from %d to %d", rowsBefore, rows, eventsBefore, events)
	}
	listed, err := c.st.ListOwnOAuthGrants(c.ctx, c.owner.ID)
	requireOK(t, err)
	for _, grant := range listed {
		if strings.Contains(grant.ClientID+grant.ClientName, leakedGitHub) || strings.Contains(grant.ClientID+grant.ClientName, leakedKey) {
			t.Fatalf("grant listing carries secret-shaped client metadata: %+v", grant)
		}
	}

	digests := map[string]bool{}
	for _, value := range c.issuedValues() {
		digests[string(core.OAuthCredentialDigest(value))] = true
	}
	stored := map[OAuthTable]int{}
	for _, row := range c.hooks.rows(t, c.ctx) {
		stored[row.Table]++
		switch row.Table {
		case OAuthAuthorizationCodesTable, OAuthTokensTable:
			if len(row.Digest) != 32 || !digests[string(row.Digest)] {
				t.Fatalf("%s row %s stores %d bytes that are not the SHA-256 of an issued value", row.Table, row.ID, len(row.Digest))
			}
		default:
			if len(row.Digest) != 0 {
				t.Fatalf("%s row %s stores a digest", row.Table, row.ID)
			}
		}
		requireSecretFree(t, c, string(row.Table)+" row "+row.ID, row.Text)
		if strings.Contains(row.Text, leakedGitHub) || strings.Contains(row.Text, leakedKey) {
			t.Fatalf("%s row %s stores secret-shaped client metadata", row.Table, row.ID)
		}
	}
	if stored[OAuthClientsTable] != 1 || stored[OAuthAuthorizationCodesTable] != 3 || stored[OAuthGrantsTable] != 2 || stored[OAuthTokensTable] != 6 {
		t.Fatalf("stored rows=%v, want 1 client, 3 codes, 2 grants, and 6 tokens", stored)
	}
	allowed := []string{"client_id", "client_name", "grant_id", "user_id"}
	kinds := map[string]int{}
	for _, event := range c.hooks.events(t, c.ctx) {
		kinds[event.Kind]++
		keys := make([]string, 0, len(eventPayload(t, event)))
		for key := range eventPayload(t, event) {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, allowed) {
			t.Fatalf("%s payload fields=%v, want %v", event.Kind, keys, allowed)
		}
		requireSecretFree(t, c, event.Kind, event)
		raw, err := json.Marshal(event)
		requireOK(t, err)
		if strings.Contains(string(raw), leakedGitHub) || strings.Contains(string(raw), leakedKey) {
			t.Fatalf("%s carries a secret-shaped value from client metadata", event.Kind)
		}
	}
	// The clean code the refused exchanges named was never consumed.
	if _, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(cleanCode, client.ID)); err != nil {
		t.Fatalf("the clean code after the refused exchanges: %v", err)
	}
	if kinds[store.OAuthEventGrantCreated] != 2 || kinds[store.OAuthEventRefreshReuseDetected] != 1 || kinds[store.OAuthEventGrantRevoked] != 1 || len(kinds) != 3 {
		t.Fatalf("OAuth audit kinds=%v", kinds)
	}
}

// runOAuthLongClientID proves that every backend keeps a verified
// metadata-document client ID longer than 255 characters exactly, through
// the exchange and a refresh, and refuses a truncated one (req-mcp-oauth
// AC-2.1, AC-4.1, AC-4.2; component-persistence invariant 3).
func runOAuthLongClientID(t *testing.T, c *oauthCase) {
	longID := "https://client.example.test/" + strings.Repeat("long-client-metadata-path/", 40) + "client.json"
	if len(longID) <= 1000 {
		t.Fatalf("fixture client ID has %d characters, want more than 1000", len(longID))
	}
	pair := c.grant(t, c.owner.ID, longID, "Long Client")
	if pair.ClientID != longID {
		t.Fatalf("first pair client ID has %d characters, want the exact %d", len(pair.ClientID), len(longID))
	}
	grant, ok := c.grantOf(t, c.owner.ID, pair.GrantID)
	if !ok || grant.ClientID != longID {
		t.Fatalf("listed grant client ID has %d characters, want the exact %d", len(grant.ClientID), len(longID))
	}
	_, err := c.st.RefreshOAuthTokens(c.ctx, store.OAuthRefreshExchange{RefreshToken: pair.RefreshToken, ClientID: longID[:255], Resource: oauthResource})
	requireOAuthError(t, err, store.ErrOAuthTokenInvalid, "refresh with a truncated client ID")
	next, err := c.refresh(c.ctx, pair)
	requireOK(t, err)
	if next.ClientID != longID || next.GrantID != pair.GrantID {
		t.Fatalf("refreshed pair client ID has %d characters, want the exact %d", len(next.ClientID), len(longID))
	}
	c.requireAccess(t, next.AccessToken, next.OAuthTokenPair)
	requireOK(t, c.st.RevokeOAuthToken(c.ctx, longID, next.AccessToken))
	c.requireAccessRefused(t, next.AccessToken, "access token revoked by the long client ID")
}

// errInjectedOAuthAudit is the failure the audit test hook returns.
var errInjectedOAuthAudit = errors.New("injected OAuth audit append failure")

// runOAuthAuditFailure makes the audit append fail after each writer's state
// change, through the identity test hook every backend runs before its OAuth
// audit append. The exchange, the replay revocation, the owner revocation,
// and the token revocation each roll back: the code stays usable, the grant
// stays live, and no event commits (component-persistence invariants 5 and
// 17; req-mcp-oauth AC-4.1, AC-4.2, AC-4.4).
func runOAuthAuditFailure(t *testing.T, c *oauthCase) {
	failing := store.WithIdentityTestHook(c.ctx, func(stage string) error {
		if stage == store.IdentityHookOAuthAudit {
			return errInjectedOAuthAudit
		}
		return nil
	})
	client := c.client(t, "Audit Failure Client")
	_, value := c.code(t, c.owner.ID, client.ID, client.Name)
	_, err := c.st.ExchangeOAuthAuthorizationCode(failing, oauthExchange(value, client.ID))
	requireOAuthError(t, err, errInjectedOAuthAudit, "exchange whose audit append fails")
	if grants, err := c.st.ListOwnOAuthGrants(c.ctx, c.owner.ID); err != nil || len(grants) != 0 {
		t.Fatalf("a failed exchange left grants=%+v err=%v", grants, err)
	}
	if read, err := c.st.GetOAuthClient(c.ctx, client.ID); err != nil || read.FirstUsedAt != nil {
		t.Fatalf("a failed exchange marked the client used: %+v err=%v", read, err)
	}
	pair, err := c.st.ExchangeOAuthAuthorizationCode(c.ctx, oauthExchange(value, client.ID))
	requireOK(t, err)
	c.remember(pair)

	rotated, err := c.refresh(c.ctx, pair)
	requireOK(t, err)
	_, err = c.refresh(failing, pair)
	requireOAuthError(t, err, errInjectedOAuthAudit, "replay whose audit append fails")
	if errors.Is(err, store.ErrOAuthRefreshTokenReused) {
		t.Fatal("a rolled-back replay claimed a committed reuse refusal")
	}
	c.requireAccess(t, rotated.AccessToken, rotated.OAuthTokenPair)

	_, err = c.st.RevokeOwnOAuthGrant(failing, c.owner.ID, pair.GrantID)
	requireOAuthError(t, err, errInjectedOAuthAudit, "owner revocation whose audit append fails")
	c.requireAccess(t, rotated.AccessToken, rotated.OAuthTokenPair)

	err = c.st.RevokeOAuthToken(failing, client.ID, rotated.AccessToken)
	requireOAuthError(t, err, errInjectedOAuthAudit, "token revocation whose audit append fails")
	c.requireAccess(t, rotated.AccessToken, rotated.OAuthTokenPair)
	if grant, ok := c.grantOf(t, c.owner.ID, pair.GrantID); !ok || grant.RevokedAt != nil {
		t.Fatalf("grant after failed revocations=%+v listed=%t, want live", grant, ok)
	}
	if _, err := c.refresh(c.ctx, rotated); err != nil {
		t.Fatalf("the current refresh token failed after rolled-back revocations: %v", err)
	}
	kinds := map[string]int{}
	for _, event := range c.hooks.events(t, c.ctx) {
		kinds[event.Kind]++
	}
	if kinds[store.OAuthEventGrantCreated] != 1 || len(kinds) != 1 {
		t.Fatalf("OAuth audit kinds=%v, want only the one committed grant event", kinds)
	}
}

// requireSecretFree fails when value, rendered as text or JSON, holds an
// issued wire value, its secret tail, or its digest in hex or base64.
func requireSecretFree(t *testing.T, c *oauthCase, what string, value any) {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		raw, err := json.Marshal(value)
		requireOK(t, err)
		text = string(raw)
	}
	for _, issued := range c.issuedValues() {
		digest := core.OAuthCredentialDigest(issued)
		// The secret follows cv_<kind>_<16-character id>_; its base64url
		// text can itself hold an underscore.
		secret := issued[len("cv_oat_")+17:]
		for _, form := range []string{issued, secret, hex.EncodeToString(digest), base64.StdEncoding.EncodeToString(digest), base64.RawURLEncoding.EncodeToString(digest)} {
			if strings.Contains(text, form) {
				t.Fatalf("%s holds a credential value or digest", what)
			}
		}
	}
}
