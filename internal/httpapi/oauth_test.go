package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// These tests prove the built-in `/mcp` OAuth 2.1 server at the HTTP
// boundary through the real router over the volatile backend (req-mcp-oauth
// REQ-1 to REQ-4; req-security-boundaries AC-1.4, AC-2.1). The store's own
// atomicity, digests, expiry, and audit events are proven by the shared
// OAuth conformance suite on every backend (internal/store/storetest).

const (
	oauthTestIssuer   = "https://conveyor.example"
	oauthTestAudience = oauthTestIssuer + "/mcp"
	oauthTestRedirect = "https://app.example/callback"
	oauthTestOwnerPAT = "oauth-owner-token"
	oauthTestVerifier = "verifier-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGH"
)

// recordingOAuthStore wraps the real store. It counts calls, keeps issued
// records for lifetime checks, and can mark a dynamic client expired the way
// the store answers an unused client past its deadline (ErrNotFound).
type recordingOAuthStore struct {
	store.OAuthStore
	mu             sync.Mutex
	registrations  int
	cleanups       int
	issued         []core.OAuthAuthorizationCode
	exchanged      []core.IssuedOAuthTokenPair
	expiredClients map[string]bool
	verifyErr      error
	listErr        error
}

func (r *recordingOAuthStore) ListOwnOAuthGrants(ctx context.Context, userID string) ([]core.OAuthGrant, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.OAuthStore.ListOwnOAuthGrants(ctx, userID)
}

func (r *recordingOAuthStore) RegisterOAuthClient(ctx context.Context, registration store.OAuthClientRegistration) (core.OAuthClient, error) {
	r.mu.Lock()
	r.registrations++
	r.mu.Unlock()
	return r.OAuthStore.RegisterOAuthClient(ctx, registration)
}

func (r *recordingOAuthStore) GetOAuthClient(ctx context.Context, clientID string) (core.OAuthClient, error) {
	r.mu.Lock()
	expired := r.expiredClients[clientID]
	r.mu.Unlock()
	if expired {
		return core.OAuthClient{}, fmt.Errorf("%w: OAuth client %s", store.ErrNotFound, clientID)
	}
	return r.OAuthStore.GetOAuthClient(ctx, clientID)
}

func (r *recordingOAuthStore) DeleteExpiredOAuthClients(ctx context.Context) (int, error) {
	r.mu.Lock()
	r.cleanups++
	r.mu.Unlock()
	return r.OAuthStore.DeleteExpiredOAuthClients(ctx)
}

func (r *recordingOAuthStore) IssueOAuthAuthorizationCode(ctx context.Context, request store.OAuthCodeRequest) (core.OAuthAuthorizationCode, string, error) {
	code, value, err := r.OAuthStore.IssueOAuthAuthorizationCode(ctx, request)
	if err == nil {
		r.mu.Lock()
		r.issued = append(r.issued, code)
		r.mu.Unlock()
	}
	return code, value, err
}

func (r *recordingOAuthStore) ExchangeOAuthAuthorizationCode(ctx context.Context, exchange store.OAuthCodeExchange) (core.IssuedOAuthTokenPair, error) {
	pair, err := r.OAuthStore.ExchangeOAuthAuthorizationCode(ctx, exchange)
	if err == nil {
		r.mu.Lock()
		r.exchanged = append(r.exchanged, pair)
		r.mu.Unlock()
	}
	return pair, err
}

func (r *recordingOAuthStore) VerifyOAuthAccessToken(ctx context.Context, value string) (core.AuthenticatedCredential, error) {
	if r.verifyErr != nil {
		return core.AuthenticatedCredential{}, r.verifyErr
	}
	return r.OAuthStore.VerifyOAuthAccessToken(ctx, value)
}

type oauthHarness struct {
	t                           *testing.T
	st                          store.Backend
	server                      *Server
	oauth                       *recordingOAuthStore
	owner, member, other        core.IdentityUser
	ownerCtx                    context.Context
	memberPAT                   string
	memberSession, otherSession string
	// sources counts registrations so each comes from its own address and
	// the per-source limiter never interferes with an unrelated test.
	sources int
}

func newOAuthHarness(t *testing.T) *oauthHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	system := store.WithActor(t.Context(), store.SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(system, "alpha"), &config.Config{Workspace: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapIdentity(system, config.FirstOperatorIdentity{OrganizationName: "OAuth", Email: "owner@example.test", DisplayName: "Owner"}, oauthTestOwnerPAT); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(system, oauthTestOwnerPAT)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	member, err := st.ProvisionIdentityUser(ownerCtx, "member@example.test", "Member Person")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.ProvisionIdentityUser(ownerCtx, "other@example.test", "Other Person")
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct {
		email string
		role  core.WorkspaceRole
	}{{owner.Email, core.WorkspaceRoleOperator}, {member.Email, core.WorkspaceRoleOperator}, {other.Email, core.WorkspaceRoleViewer}} {
		if _, err := st.GrantWorkspaceRole(ownerCtx, grant.email, "alpha", grant.role); err != nil {
			t.Fatal(err)
		}
	}
	memberPAT, err := st.IssueOwnPersonalAccessToken(store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(member.ID), Role: core.ActorUser}), member.ID, "member agent")
	if err != nil {
		t.Fatal(err)
	}
	session := func(email string) string {
		link, err := st.IssueSignInLink(system, email)
		if err != nil {
			t.Fatal(err)
		}
		signedIn, _, err := st.RedeemSignInLink(system, link.Value)
		if err != nil {
			t.Fatal(err)
		}
		return signedIn.Value
	}
	server := NewServer(st)
	server.Workspaces = st
	server.ConfigStore = st
	server.InvitationDelivery.PublicURL = oauthTestIssuer
	recording := &recordingOAuthStore{OAuthStore: st, expiredClients: map[string]bool{}}
	server.OAuth = recording
	// No test reaches the network: a metadata-document client resolves only
	// through a fixture resolver that a test installs.
	server.oauthResolver = &oauthMetadataResolver{lookup: func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("tests resolve no names")
	}}
	return &oauthHarness{t: t, st: st, server: server, oauth: recording, owner: owner, member: member, other: other, ownerCtx: ownerCtx,
		memberPAT: memberPAT.Value, memberSession: session(member.Email), otherSession: session(other.Email)}
}

type oauthRequest struct {
	method, path, body, contentType string
	bearer, session                 string
	remote                          string
	header                          map[string]string
	// proof adds the session mutation proof a dashboard POST carries.
	proof bool
}

func (h *oauthHarness) nextSource() string {
	h.sources++
	return fmt.Sprintf("192.0.2.%d:1000", h.sources%250+1)
}

func (h *oauthHarness) do(c oauthRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.contentType != "" {
		request.Header.Set("Content-Type", c.contentType)
	}
	if c.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.session != "" {
		request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: c.session})
	}
	if c.proof {
		request.Header.Set("X-Conveyor-CSRF", "1")
		request.Header.Set("Origin", oauthTestIssuer)
	}
	if c.remote != "" {
		request.RemoteAddr = c.remote
	}
	for key, value := range c.header {
		// The handler reads Request.Host, not a Host header entry.
		if strings.EqualFold(key, "Host") {
			request.Host = value
			continue
		}
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	return response
}

func decodeOAuthJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %d %q: %v", response.Code, response.Body.String(), err)
	}
	return body
}

func expectOAuthError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	body := decodeOAuthJSON(t, response)
	if body["error"] != code {
		t.Fatalf("error=%v, want %s; body=%s", body["error"], code, response.Body.String())
	}
	return body
}

func expectNoStore(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy=%q, want no-referrer", got)
	}
	if got := response.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma=%q, want no-cache", got)
	}
}

func (h *oauthHarness) register(redirectURIs ...string) string {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": "Test Agent", "redirect_uris": redirectURIs, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": "ignored"})
	response := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/register", body: string(body), contentType: "application/json", remote: h.nextSource()})
	if response.Code != http.StatusCreated {
		h.t.Fatalf("register status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeOAuthJSON(h.t, response)["client_id"].(string)
}

func oauthAuthorizeQuery(clientID, redirectURI, state string, extra ...string) string {
	values := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code_challenge": {core.OAuthS256Challenge(oauthTestVerifier)}, "code_challenge_method": {"S256"}, "resource": {oauthTestAudience},
	}
	if state != "" {
		values.Set("state", state)
	}
	for index := 0; index+1 < len(extra); index += 2 {
		values.Set(extra[index], extra[index+1])
	}
	return values.Encode()
}

func oauthDecisionBody(decision, clientID, redirectURI, state string) string {
	body, _ := json.Marshal(map[string]string{
		"decision": decision, "response_type": "code", "client_id": clientID, "redirect_uri": redirectURI,
		"code_challenge": core.OAuthS256Challenge(oauthTestVerifier), "code_challenge_method": "S256", "state": state, "resource": oauthTestAudience,
	})
	return string(body)
}

// approve runs the member's consent and returns the redirect target.
func (h *oauthHarness) approve(session, clientID, redirectURI, state string) *url.URL {
	h.t.Helper()
	response := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, redirectURI, state), contentType: "application/json", session: session, proof: true})
	if response.Code != http.StatusOK {
		h.t.Fatalf("approve status=%d body=%s", response.Code, response.Body.String())
	}
	expectNoStore(h.t, response)
	target, err := url.Parse(decodeOAuthJSON(h.t, response)["redirect_to"].(string))
	if err != nil {
		h.t.Fatal(err)
	}
	return target
}

func (h *oauthHarness) token(form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(oauthRequest{method: http.MethodPost, path: "/oauth/token", body: form.Encode(), contentType: "application/x-www-form-urlencoded"})
}

func codeExchangeForm(code, clientID, redirectURI string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "code_verifier": {oauthTestVerifier}, "resource": {oauthTestAudience}}
}

func refreshForm(refreshToken, clientID string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}, "resource": {oauthTestAudience}}
}

func decodeTokenPair(t *testing.T, response *httptest.ResponseRecorder) oauthTokenResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", response.Code, response.Body.String())
	}
	var pair oauthTokenResponse
	if err := json.Unmarshal(response.Body.Bytes(), &pair); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pair.AccessToken, "cv_oat_") || !strings.HasPrefix(pair.RefreshToken, "cv_ort_") || pair.TokenType != "Bearer" {
		t.Fatalf("token pair = %+v", pair)
	}
	return pair
}

// grant runs a full dynamic-registration flow for the member and returns
// the client and the first pair.
func (h *oauthHarness) grant() (string, oauthTokenResponse) {
	h.t.Helper()
	clientID := h.register(oauthTestRedirect)
	target := h.approve(h.memberSession, clientID, oauthTestRedirect, "s1")
	return clientID, decodeTokenPair(h.t, h.token(codeExchangeForm(target.Query().Get("code"), clientID, oauthTestRedirect)))
}

type oauthMCPResult struct {
	status  int
	text    string
	isError bool
	header  http.Header
}

func (h *oauthHarness) mcp(bearer, tool string, args map[string]any) oauthMCPResult {
	h.t.Helper()
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	result := oauthMCPResult{status: response.Code, header: response.Header()}
	if response.Code != http.StatusOK {
		result.text = response.Body.String()
		return result
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		h.t.Fatal(err)
	}
	if len(envelope.Result.Content) > 0 {
		result.text = envelope.Result.Content[0].Text
	}
	result.isError = envelope.Result.IsError
	return result
}

const oauthResourceChallenge = `Bearer resource_metadata="https://conveyor.example/.well-known/oauth-protected-resource/mcp"`

// --- Public URL gate and discovery (AC-1.1 to AC-1.4) ---

// TestOAuthIssuerAcceptsOnlyCanonicalOrigins is the one table for the public
// URL rule (req-mcp-oauth AC-1.1, AC-1.4; component-identity-membership
// invariant 50). Every row runs through the router: a refused value answers
// the JSON discovery refusal and a bare Bearer challenge on /mcp, and a
// valid value enables discovery and names itself in the challenge.
func TestOAuthIssuerAcceptsOnlyCanonicalOrigins(t *testing.T) {
	valid := []string{
		"https://conveyor.example", "https://conveyor.example:8443", "http://127.0.0.1:8080", "http://[::1]:8080",
		"http://localhost:8080", "http://localhost", "https://10.0.0.1", "https://[2001:db8::1]", "https://a-b.example.co",
		"https://" + strings.Repeat("a", 63) + ".example",
		// A hexadecimal-looking label that is not the last one is a DNS name.
		"https://0x7f.example", "https://0xcafe.conveyor.example",
	}
	refused := []string{
		// Not an origin, or an unsupported scheme.
		"", "conveyor.example", "ftp://conveyor.example", "https://", " https://conveyor.example", "https://conveyor.example\\x",
		// HTTP is for loopback only.
		"http://conveyor.example", "http://127.0.0.2", "http://localhost.example", "http://10.0.0.1:8080",
		// A path, a trailing slash, a query, a fragment, or user information.
		"https://conveyor.example/", "https://conveyor.example/base", "https://conveyor.example?x=1", "https://conveyor.example?",
		"https://conveyor.example#frag", "https://conveyor.example#", "https://user@conveyor.example", "https://user:pass@conveyor.example",
		// Uppercase scheme or host.
		"HTTPS://conveyor.example", "Https://conveyor.example", "https://Conveyor.example", "https://CONVEYOR.EXAMPLE", "http://LOCALHOST:8080",
		// An explicit default port, an empty, zero, out-of-range, or padded port.
		"https://conveyor.example:443", "http://localhost:80", "http://127.0.0.1:80", "http://[::1]:80",
		"https://conveyor.example:", "http://[::1]:", "http://localhost:", "https://conveyor.example:0", "https://conveyor.example:65536",
		"https://conveyor.example:99999", "https://conveyor.example:08443",
		// A bracketed authority that is not an IPv6 literal, or a zoned one.
		"https://[conveyor.example]", "https://[conveyor.example]:8443", "http://[localhost]:8080", "https://[10.0.0.1]", "https://[fe80::1%25eth0]",
		// An unbracketed IPv6 host, or an empty host.
		"http://::1:8080", "http://::1", "https://2001:db8::1", "https://2001:db8::1:8443", "https://:8443",
		// A noncanonical IPv6 spelling.
		"https://[2001:0db8:0:0:0:0:0:1]", "https://[2001:DB8::1]", "http://[0:0:0:0:0:0:0:1]:8080", "http://[::0001]:8080",
		// An IPv4-mapped IPv6 literal.
		"http://[::ffff:127.0.0.1]:8080", "https://[::ffff:10.0.0.1]", "https://[::ffff:a00:1]",
		// A noncanonical IPv4 spelling, which is not a DNS name either.
		"http://127.000.000.001:8080", "http://127.0.0.01:8080", "https://010.0.0.1", "http://0x7f.0.0.1:8080", "https://10.0.1", "https://1.2.3.4.5",
		// Every other numeric IPv4 form a browser accepts: hexadecimal,
		// octal, a single integer, and fewer than four parts.
		"https://0x7f000001", "http://0x7f000001:8080", "https://0X7F000001", "https://0x0a000001", "https://127.0x1", "https://10.0.0.0x1",
		"https://0x", "https://0177.0.0.1", "http://0177.0.0.01:8080", "https://2130706433", "http://2130706433:8080", "https://127.1",
		"https://10.1", "https://10.0.1", "http://127.1:8080",
		// A trailing dot, an empty label, an invalid label, or an overlong name.
		"https://conveyor.example.", "http://localhost.:8080", "http://127.0.0.1.:8080", "https://a..b", "https://.conveyor.example",
		"https://-conveyor.example", "https://conveyor-.example", "https://conveyor_dev.example", "https://conveyor%2Eexample",
		"https://" + strings.Repeat("a", 64) + ".example",
		"https://" + strings.Repeat(strings.Repeat("a", 50)+".", 5) + "example",
	}
	if long := strings.Repeat(strings.Repeat("a", 50)+".", 5) + "example"; len(long) <= 253 {
		t.Fatalf("overlong name row is only %d bytes", len(long))
	}
	h := newOAuthHarness(t)
	discover := func(publicURL string) (*httptest.ResponseRecorder, oauthMCPResult) {
		h.server.InvitationDelivery.PublicURL = publicURL
		response := h.do(oauthRequest{method: http.MethodGet, path: "/.well-known/oauth-protected-resource/mcp", header: map[string]string{"Host": "attacker.example"}})
		return response, h.mcp("", getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
	}
	for _, value := range valid {
		if issuer, ok := oauthIssuerFromPublicURL(value); !ok || issuer != value {
			t.Errorf("oauthIssuerFromPublicURL(%q) = %q, %v; want the value itself", value, issuer, ok)
			continue
		}
		response, challenge := discover(value)
		if response.Code != http.StatusOK || decodeOAuthJSON(t, response)["resource"] != value+"/mcp" {
			t.Errorf("%q: discovery status=%d body=%s", value, response.Code, response.Body.String())
		}
		if want := `Bearer resource_metadata="` + value + `/.well-known/oauth-protected-resource/mcp"`; challenge.status != http.StatusUnauthorized || challenge.header.Get("WWW-Authenticate") != want {
			t.Errorf("%q: challenge=%q", value, challenge.header.Get("WWW-Authenticate"))
		}
	}
	for _, value := range refused {
		if issuer, ok := oauthIssuerFromPublicURL(value); ok {
			t.Errorf("oauthIssuerFromPublicURL(%q) = %q, accepted; want refused", value, issuer)
			continue
		}
		response, challenge := discover(value)
		body := response.Body.String()
		if response.Code != http.StatusNotFound || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") ||
			strings.Contains(strings.ToLower(body), "<html") || strings.Contains(body, "attacker.example") {
			t.Errorf("%q: discovery status=%d type=%q body=%s", value, response.Code, response.Header().Get("Content-Type"), body)
		} else if decodeOAuthJSON(t, response)["error"] != "oauth_unavailable" {
			t.Errorf("%q: discovery body=%s", value, body)
		}
		if challenge.status != http.StatusUnauthorized || challenge.header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: challenge status=%d header=%q", value, challenge.status, challenge.header.Get("WWW-Authenticate"))
		}
	}
}

func TestOAuthDiscoveryDocumentsAreExact(t *testing.T) {
	h := newOAuthHarness(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		response := h.do(oauthRequest{method: http.MethodGet, path: path, header: map[string]string{"Host": "attacker.example"}})
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s status=%d type=%q body=%s", path, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
		want := map[string]any{"resource": oauthTestAudience, "authorization_servers": []any{oauthTestIssuer}, "bearer_methods_supported": []any{"header"}}
		if got := decodeOAuthJSON(t, response); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s = %v, want %v", path, got, want)
		}
	}
	response := h.do(oauthRequest{method: http.MethodGet, path: "/.well-known/oauth-authorization-server", header: map[string]string{"Host": "attacker.example"}})
	if response.Code != http.StatusOK {
		t.Fatalf("authorization server metadata status=%d", response.Code)
	}
	want := map[string]any{
		"issuer":                                oauthTestIssuer,
		"authorization_endpoint":                oauthTestIssuer + "/oauth/authorize",
		"token_endpoint":                        oauthTestIssuer + "/oauth/token",
		"revocation_endpoint":                   oauthTestIssuer + "/oauth/revoke",
		"registration_endpoint":                 oauthTestIssuer + "/oauth/register",
		"response_types_supported":              []any{"code"},
		"grant_types_supported":                 []any{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []any{"S256"},
		"token_endpoint_auth_methods_supported": []any{"none"},
		"client_id_metadata_document_supported": true,
	}
	if got := decodeOAuthJSON(t, response); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("authorization server metadata = %v, want %v", got, want)
	}
	// Other well-known paths are JSON 404s, never dashboard HTML.
	other := h.do(oauthRequest{method: http.MethodGet, path: "/.well-known/openid-configuration"})
	if other.Code != http.StatusNotFound || strings.Contains(strings.ToLower(other.Body.String()), "<html") {
		t.Fatalf("openid-configuration status=%d body=%s", other.Code, other.Body.String())
	}
}

func TestOAuthMCPChallengeNamesResourceMetadataOnlyWhileEnabled(t *testing.T) {
	h := newOAuthHarness(t)
	for name, bearer := range map[string]string{"missing": "", "unknown PAT": "cv_pat_unknown_value", "unknown access": "cv_oat_oat_unknown_0123456789abcdef", "refresh token": "cv_ort_ort_unknown_0123456789abcdef", "code": "cv_oac_oac_unknown_0123456789abcdef"} {
		result := h.mcp(bearer, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
		if result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != oauthResourceChallenge {
			t.Fatalf("%s: status=%d challenge=%q", name, result.status, result.header.Get("WWW-Authenticate"))
		}
	}
	h.server.InvitationDelivery.PublicURL = ""
	result := h.mcp("", getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
	if result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("disabled: status=%d challenge=%q", result.status, result.header.Get("WWW-Authenticate"))
	}
}

func TestOAuthDisabledRefusesDiscoveryAndEndpointsWithoutHTML(t *testing.T) {
	for name, configure := range map[string]func(*Server){
		"unset public URL":           func(s *Server) { s.InvitationDelivery.PublicURL = "" },
		"insecure public URL":        func(s *Server) { s.InvitationDelivery.PublicURL = "http://conveyor.example" },
		"public URL path":            func(s *Server) { s.InvitationDelivery.PublicURL = "https://conveyor.example/base" },
		"bracketed DNS name":         func(s *Server) { s.InvitationDelivery.PublicURL = "https://[conveyor.example]" },
		"unbracketed IPv6 with port": func(s *Server) { s.InvitationDelivery.PublicURL = "http://::1:8080" },
		"unbracketed IPv6":           func(s *Server) { s.InvitationDelivery.PublicURL = "https://2001:db8::1" },
		"user information":           func(s *Server) { s.InvitationDelivery.PublicURL = "https://user@conveyor.example" },
		"empty host":                 func(s *Server) { s.InvitationDelivery.PublicURL = "https://:8443" },
		"trailing dot":               func(s *Server) { s.InvitationDelivery.PublicURL = "https://conveyor.example." },
		"backend without OAuth": func(s *Server) {
			s.OAuth = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOAuthHarness(t)
			configure(h.server)
			host := map[string]string{"Host": "attacker.example", "X-Forwarded-Proto": "https"}
			for _, request := range []oauthRequest{
				{method: http.MethodGet, path: "/.well-known/oauth-protected-resource", header: host},
				{method: http.MethodGet, path: "/.well-known/oauth-protected-resource/mcp", header: host},
				{method: http.MethodGet, path: "/.well-known/oauth-authorization-server", header: host},
				{method: http.MethodPost, path: "/oauth/register", body: `{}`, contentType: "application/json", header: host},
				{method: http.MethodPost, path: "/oauth/token", body: "grant_type=refresh_token", contentType: "application/x-www-form-urlencoded", header: host},
				{method: http.MethodPost, path: "/oauth/revoke", body: "token=x&client_id=y", contentType: "application/x-www-form-urlencoded", header: host},
				{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery("client", oauthTestRedirect, "s"), session: h.memberSession},
			} {
				response := h.do(request)
				body := response.Body.String()
				if response.Code != http.StatusNotFound || strings.Contains(strings.ToLower(body), "<html") || strings.Contains(body, "attacker.example") {
					t.Fatalf("%s %s status=%d body=%s", request.method, request.path, response.Code, body)
				}
				expectOAuthError(t, response, http.StatusNotFound, "oauth_unavailable")
			}
			result := h.mcp("", getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
			if result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("/mcp status=%d challenge=%q", result.status, result.header.Get("WWW-Authenticate"))
			}
		})
	}
}

// --- Dynamic client registration (AC-2.2, AC-2.3) ---

func TestOAuthRegistrationAdmitsOnlyBoundedPublicClients(t *testing.T) {
	h := newOAuthHarness(t)
	valid := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/register", contentType: "application/json", remote: "198.51.100.1:1",
		body: `{"client_name":"Desk Agent","redirect_uris":["http://127.0.0.1:33418/callback","https://app.example/cb?tenant=a","http://[::1]/cb","http://localhost:9/cb"],"token_endpoint_auth_method":"none","logo_uri":"https://app.example/logo.png"}`})
	if valid.Code != http.StatusCreated {
		t.Fatalf("valid registration status=%d body=%s", valid.Code, valid.Body.String())
	}
	expectNoStore(t, valid)
	registered := decodeOAuthJSON(t, valid)
	if registered["token_endpoint_auth_method"] != "none" || fmt.Sprint(registered["grant_types"]) != "[authorization_code]" || registered["client_name"] != "Desk Agent" {
		t.Fatalf("registration = %v", registered)
	}
	if _, err := h.st.GetOAuthClient(t.Context(), registered["client_id"].(string)); err != nil {
		t.Fatalf("registered client not stored: %v", err)
	}
	before := h.oauth.registrations
	longName := strings.Repeat("n", oauthClientNameMaxBytes+1)
	manyURIs, _ := json.Marshal(func() []string {
		uris := make([]string, oauthRegistrationMaxRedirectURIs+1)
		for index := range uris {
			uris[index] = fmt.Sprintf("https://app.example/cb%d", index)
		}
		return uris
	}())
	for name, invalid := range map[string]struct {
		body, contentType, code string
	}{
		"missing auth method":   {`{"redirect_uris":["https://app.example/cb"]}`, "application/json", "invalid_client_metadata"},
		"secret auth method":    {`{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, "application/json", "invalid_client_metadata"},
		"client credentials":    {`{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none","grant_types":["client_credentials"]}`, "application/json", "invalid_client_metadata"},
		"refresh only":          {`{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none","grant_types":["refresh_token"]}`, "application/json", "invalid_client_metadata"},
		"implicit response":     {`{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none","response_types":["token"]}`, "application/json", "invalid_client_metadata"},
		"no redirect URIs":      {`{"redirect_uris":[],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"absent redirect URIs":  {`{"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"insecure redirect":     {`{"redirect_uris":["http://app.example/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"fragment redirect":     {`{"redirect_uris":["https://app.example/cb#x"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"empty fragment":        {`{"redirect_uris":["https://app.example/cb#"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"script redirect":       {`{"redirect_uris":["javascript:alert(1)"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"custom scheme":         {`{"redirect_uris":["myapp://cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"user information":      {`{"redirect_uris":["https://user@app.example/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"relative redirect":     {`{"redirect_uris":["/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"too many redirects":    {`{"redirect_uris":` + string(manyURIs) + `,"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"oversized redirect":    {`{"redirect_uris":["https://app.example/` + strings.Repeat("a", oauthRedirectURIMaxBytes) + `"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
		"long name":             {`{"client_name":"` + longName + `","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_client_metadata"},
		"control in name":       {`{"client_name":"a\nb","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_client_metadata"},
		"form body":             {`redirect_uris=https://app.example/cb`, "application/x-www-form-urlencoded", "invalid_client_metadata"},
		"invalid JSON":          {`{`, "application/json", "invalid_client_metadata"},
		"oversized body":        {`{"client_name":"` + strings.Repeat("x", oauthRegistrationBodyLimit) + `"}`, "application/json", "invalid_client_metadata"},
		"secret-shaped name":    {`{"client_name":"Agent ghp_0123456789abcdefghijklmnopqrstuvwxyzAB","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_client_metadata"},
		"secret-shaped address": {`{"redirect_uris":["https://app.example/cb?token=0123456789abcdefghij"],"token_endpoint_auth_method":"none"}`, "application/json", "invalid_redirect_uri"},
	} {
		response := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/register", body: invalid.body, contentType: invalid.contentType, remote: h.nextSource()})
		status := http.StatusBadRequest
		if name == "oversized body" {
			// A larger body answers 413 (component-http-api invariant 17).
			status = http.StatusRequestEntityTooLarge
		}
		body := expectOAuthError(t, response, status, invalid.code)
		if strings.Contains(fmt.Sprint(body), "ghp_0123") || strings.Contains(fmt.Sprint(body), "0123456789abcdefghij") {
			t.Fatalf("%s: refusal echoed a secret-shaped value: %v", name, body)
		}
		expectNoStore(t, response)
		h.oauth.mu.Lock()
		registrations := h.oauth.registrations
		h.oauth.mu.Unlock()
		// Only the two secret-shaped cases reach the store, which refuses
		// them before any write.
		if registrations > before+2 {
			t.Fatalf("%s: invalid input reached the store %d times", name, registrations-before)
		}
		_ = name
	}
}

func TestOAuthRegistrationRateLimitIsPerSourceAddress(t *testing.T) {
	h := newOAuthHarness(t)
	register := func(remote string, forwarded string) *httptest.ResponseRecorder {
		header := map[string]string{}
		if forwarded != "" {
			header["X-Forwarded-For"] = forwarded
		}
		return h.do(oauthRequest{method: http.MethodPost, path: "/oauth/register", contentType: "application/json", remote: remote, header: header,
			body: `{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`})
	}
	for attempt := 1; attempt <= oauthRegistrationBudget; attempt++ {
		if response := register("198.51.100.10:1", "203.0.113.99"); response.Code != http.StatusCreated {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	before := h.oauth.registrations
	// An untrusted peer's forwarded header is ignored, so changing it does
	// not escape the limit.
	refused := register("198.51.100.10:2", "203.0.113.100")
	expectOAuthError(t, refused, http.StatusTooManyRequests, "too_many_requests")
	if refused.Header().Get("Retry-After") == "" || h.oauth.registrations != before {
		t.Fatalf("refusal Retry-After=%q store calls=%d", refused.Header().Get("Retry-After"), h.oauth.registrations-before)
	}
	if response := register("198.51.100.11:1", ""); response.Code != http.StatusCreated {
		t.Fatalf("another source status=%d", response.Code)
	}
	// Behind a trusted proxy, the nearest untrusted forwarded address is the
	// source.
	h.server.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for attempt := 1; attempt <= oauthRegistrationBudget; attempt++ {
		if response := register("10.0.0.1:1", "192.0.2.1, 198.51.100.50, 10.0.0.2"); response.Code != http.StatusCreated {
			t.Fatalf("proxied attempt %d status=%d", attempt, response.Code)
		}
	}
	expectOAuthError(t, register("10.0.0.3:1", "198.51.100.50"), http.StatusTooManyRequests, "too_many_requests")
	if response := register("10.0.0.1:1", "198.51.100.51"); response.Code != http.StatusCreated {
		t.Fatalf("another forwarded source status=%d", response.Code)
	}
}

func TestOAuthSourceAddressTrustsOnlyConfiguredProxies(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	for _, tc := range []struct{ remote, forwarded, want string }{
		{"198.51.100.1:5", "203.0.113.1", "198.51.100.1"},
		{"10.0.0.1:5", "", "10.0.0.1"},
		{"10.0.0.1:5", "203.0.113.1", "203.0.113.1"},
		{"10.0.0.1:5", "203.0.113.1, 198.51.100.7, 10.1.1.1", "198.51.100.7"},
		// A chain of trusted proxies names no client: the peer stays the
		// source.
		{"10.0.0.1:5", "10.2.2.2, 10.3.3.3", "10.0.0.1"},
		// A zone-qualified address makes the chain malformed.
		{"10.0.0.1:5", "203.0.113.1, fe80::1%eth0", "10.0.0.1"},
		{"10.0.0.1:5", "fe80::1%eth0, 198.51.100.7", "10.0.0.1"},
		{"10.0.0.1:5", "198.51.100.7, 2001:db8::7%1", "10.0.0.1"},
		{"10.0.0.1:5", "203.0.113.1, not-an-address", "10.0.0.1"},
		{"10.0.0.1:5", "203.0.113.1:443", "10.0.0.1"},
		{"[fd00::1]:5", "2001:db8::5", "2001:db8::5"},
		{"[::ffff:10.0.0.1]:5", "::ffff:198.51.100.9", "198.51.100.9"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
		request.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			request.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := server.oauthSourceAddress(request); got != tc.want {
			t.Errorf("source(%s, %q) = %q, want %q", tc.remote, tc.forwarded, got, tc.want)
		}
	}
	if prefixes, err := ParseTrustedProxies(" 10.0.0.1, 192.168.0.0/16 ,, ::1 "); err != nil || len(prefixes) != 3 || prefixes[0].String() != "10.0.0.1/32" {
		t.Fatalf("ParseTrustedProxies = %v, %v", prefixes, err)
	}
	if _, err := ParseTrustedProxies("10.0.0.1, proxy.internal"); err == nil {
		t.Fatal("an invalid entry was accepted")
	}
	// An IPv4-mapped prefix matches the unmapped peer it covers.
	mapped, err := ParseTrustedProxies("::ffff:10.0.0.0/104, ::ffff:192.0.2.7/128")
	if err != nil || len(mapped) != 2 || mapped[0].String() != "10.0.0.0/8" || mapped[1].String() != "192.0.2.7/32" {
		t.Fatalf("mapped prefixes = %v, %v", mapped, err)
	}
	mappedServer := NewServer(store.NewMemory())
	mappedServer.TrustedProxies = mapped
	mappedRequest := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
	mappedRequest.RemoteAddr = "10.9.9.9:1"
	mappedRequest.Header.Set("X-Forwarded-For", "198.51.100.77")
	if got := mappedServer.oauthSourceAddress(mappedRequest); got != "198.51.100.77" {
		t.Fatalf("mapped proxy source = %q", got)
	}
	if _, err := ParseTrustedProxies("::ffff:0:0/80"); err == nil {
		t.Fatal("a mapped prefix shorter than /96 was accepted")
	}
	t.Setenv(TrustedProxiesEnv, "10.0.0.0/8, nonsense")
	if prefixes := trustedProxiesFromEnvironment(); prefixes != nil {
		t.Fatalf("an invalid setting trusted %v", prefixes)
	}
	t.Setenv(TrustedProxiesEnv, "10.0.0.0/8")
	if prefixes := trustedProxiesFromEnvironment(); len(prefixes) != 1 {
		t.Fatalf("a valid setting trusted %v", prefixes)
	}
}

func TestOAuthLimiterMemoryIsBounded(t *testing.T) {
	limiter := newOAuthAttemptLimiter(1, time.Minute, 3)
	now := time.Unix(1000, 0)
	for index := range 3 {
		if !limiter.allow(fmt.Sprintf("source-%d", index), now.Add(time.Duration(index)*time.Second)) {
			t.Fatalf("first attempt of source %d refused", index)
		}
	}
	// A full table of live windows refuses a new source and never evicts a
	// live window, so an exhausted source cannot regain its budget.
	for index := 3; index < 10; index++ {
		if limiter.allow(fmt.Sprintf("source-%d", index), now.Add(10*time.Second)) {
			t.Fatalf("new source %d was admitted into a full table", index)
		}
		if len(limiter.windows) > 3 {
			t.Fatalf("limiter holds %d keys", len(limiter.windows))
		}
	}
	if limiter.allow("source-0", now.Add(20*time.Second)) {
		t.Fatal("an exhausted source regained its budget")
	}
	// Once the windows end, their slots are reclaimed.
	if !limiter.allow("source-9", now.Add(80*time.Second)) {
		t.Fatal("a new source was refused after the windows ended")
	}
	if !limiter.allow("source-0", now.Add(80*time.Second)) {
		t.Fatal("an attempt in a new window was refused")
	}
	if len(limiter.windows) > 3 {
		t.Fatalf("limiter holds %d keys", len(limiter.windows))
	}
}

func TestOAuthExpiredUnusedClientIsRefusedAndCleanupKeepsActiveGrants(t *testing.T) {
	h := newOAuthHarness(t)
	activeClient, pair := h.grant()
	unused := h.register(oauthTestRedirect)
	h.oauth.mu.Lock()
	h.oauth.expiredClients[unused] = true
	h.oauth.mu.Unlock()
	// The expired client cannot start an authorization, and Conveyor's own
	// page shows the error with no redirect (AC-2.3, AC-3.4).
	response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery(unused, oauthTestRedirect, "s"), session: h.memberSession})
	if body := expectOAuthError(t, response, http.StatusBadRequest, "invalid_client"); body["redirect_to"] != nil {
		t.Fatalf("expired client got a redirect: %v", body)
	}
	// A registration runs the store's expiry cleanup at most once per
	// interval; the active grant keeps working.
	h.oauth.mu.Lock()
	cleanups := h.oauth.cleanups
	h.oauth.mu.Unlock()
	if cleanups != 1 {
		t.Fatalf("cleanups after first registration = %d, want 1", cleanups)
	}
	h.server.oauthCleanupMu.Lock()
	h.server.oauthCleanupAt = time.Now().Add(-2 * oauthClientCleanupInterval)
	h.server.oauthCleanupMu.Unlock()
	h.register(oauthTestRedirect)
	h.register(oauthTestRedirect)
	h.oauth.mu.Lock()
	cleanups = h.oauth.cleanups
	h.oauth.mu.Unlock()
	if cleanups != 2 {
		t.Fatalf("cleanups = %d, want 2", cleanups)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK || result.isError {
		t.Fatalf("active grant after cleanup: %+v", result)
	}
	decodeTokenPair(t, h.token(refreshForm(pair.RefreshToken, activeClient)))
}

// --- Consent (AC-3.2 to AC-3.4) ---

// TestOAuthSessionRoutesRefuseAnyAuthorizationHeader proves that the four
// dashboard-session mounts refuse a request that carries any Authorization
// header beside a valid session cookie, whatever the scheme spelling, before
// the cookie is read: no code is issued and no grant changes. The
// cookie-only requests still succeed (approved plan v1;
// component-identity-membership invariants 52 and 54).
func TestOAuthSessionRoutesRefuseAnyAuthorizationHeader(t *testing.T) {
	h := newOAuthHarness(t)
	clientID, pair := h.grant()
	grants, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID)
	if err != nil || len(grants) != 1 {
		t.Fatal(grants, err)
	}
	grantID := grants[0].ID
	issued := len(h.oauth.issued)
	query := "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, "s")
	mounts := []oauthRequest{
		{method: http.MethodGet, path: query},
		{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"), contentType: "application/json", proof: true},
		{method: http.MethodGet, path: "/v1/oauth/grants"},
		{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, proof: true},
	}
	for _, authorization := range []string{
		"Bearer " + h.memberPAT, "bearer " + h.memberPAT, "BEARER " + h.memberPAT, "bEaReR " + h.memberPAT,
		"Basic bWVtYmVyOnBhc3N3b3Jk", "Token " + h.memberPAT, "",
	} {
		for _, mount := range mounts {
			request := mount
			request.session = h.memberSession
			request.header = map[string]string{"Authorization": authorization}
			response := h.do(request)
			if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s with Authorization %q: status=%d body=%s", mount.method, mount.path, strings.Fields(authorization + " -")[0], response.Code, response.Body.String())
			}
			expectNoStore(t, response)
		}
	}
	if len(h.oauth.issued) != issued {
		t.Fatalf("a refused consent issued %d codes", len(h.oauth.issued)-issued)
	}
	if listed, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID); err != nil || len(listed) != 1 || listed[0].ID != grantID {
		t.Fatalf("grants after refused requests = %+v, %v", listed, err)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
		t.Fatalf("token after refused requests: %+v", result)
	}
	// The same requests with the cookie alone succeed.
	for index, mount := range mounts {
		request := mount
		request.session = h.memberSession
		response := h.do(request)
		if response.Code != http.StatusOK {
			t.Fatalf("cookie-only %s %s: status=%d body=%s", mount.method, mount.path, response.Code, response.Body.String())
		}
		if index == 1 && len(h.oauth.issued) != issued+1 {
			t.Fatalf("cookie-only approval issued %d codes", len(h.oauth.issued)-issued)
		}
	}
	if listed, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID); err != nil || len(listed) != 0 {
		t.Fatalf("grants after cookie-only revoke = %+v, %v", listed, err)
	}
}

func TestOAuthConsentIsSessionOnly(t *testing.T) {
	h := newOAuthHarness(t)
	clientID, pair := h.grant()
	query := "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, "s")
	for name, request := range map[string]oauthRequest{
		"no credential":                   {method: http.MethodGet, path: query},
		"personal access token":           {method: http.MethodGet, path: query, bearer: h.memberPAT},
		"OAuth access token":              {method: http.MethodGet, path: query, bearer: pair.AccessToken},
		"OAuth access token with session": {method: http.MethodGet, path: query, bearer: pair.AccessToken, session: h.memberSession},
		"PAT with session":                {method: http.MethodGet, path: query, bearer: h.memberPAT, session: h.memberSession},
		"POST with personal access token": {method: http.MethodPost, path: "/v1/oauth/authorize", bearer: h.memberPAT, contentType: "application/json", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s")},
	} {
		response := h.do(request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	before := len(h.oauth.issued)
	noProof := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", session: h.memberSession, contentType: "application/json", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s")})
	if noProof.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF proof status=%d", noProof.Code)
	}
	wrongOrigin := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", session: h.memberSession, contentType: "application/json", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"),
		header: map[string]string{"X-Conveyor-CSRF": "1", "Origin": "https://attacker.example"}})
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("wrong Origin status=%d", wrongOrigin.Code)
	}
	if len(h.oauth.issued) != before {
		t.Fatal("a refused consent minted a code")
	}
	// The consent routes set no cookie, so the session stays at Path=/v1 and
	// SameSite=Strict.
	ok := h.do(oauthRequest{method: http.MethodGet, path: query, session: h.memberSession})
	if ok.Code != http.StatusOK || ok.Header().Get("Set-Cookie") != "" {
		t.Fatalf("session consent status=%d set-cookie=%q", ok.Code, ok.Header().Get("Set-Cookie"))
	}
}

func TestOAuthConsentShowsVerifiedClientAndUser(t *testing.T) {
	h := newOAuthHarness(t)
	clientID := h.register(oauthTestRedirect)
	response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, "s"), session: h.memberSession})
	if response.Code != http.StatusOK {
		t.Fatalf("consent status=%d body=%s", response.Code, response.Body.String())
	}
	expectNoStore(t, response)
	var view oauthConsentView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Client.ID != clientID || view.Client.Name != "Test Agent" || view.Client.Kind != oauthClientKindDynamicRegistration || view.Client.Origin != "" ||
		view.RedirectHost != "app.example" || view.User.ID != h.member.ID || view.User.Email != "member@example.test" || view.User.DisplayName != "Member Person" || view.Resource != oauthTestAudience {
		t.Fatalf("consent view = %+v", view)
	}
	// GET is read-only: it mints nothing.
	if len(h.oauth.issued) != 0 {
		t.Fatal("GET consent minted a code")
	}
	// An omitted resource names the MCP audience.
	omitted := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + strings.ReplaceAll(oauthAuthorizeQuery(clientID, oauthTestRedirect, "s"), "&resource="+url.QueryEscape(oauthTestAudience), ""), session: h.memberSession})
	if omitted.Code != http.StatusOK || decodeOAuthJSON(t, omitted)["resource"] != oauthTestAudience {
		t.Fatalf("omitted resource status=%d body=%s", omitted.Code, omitted.Body.String())
	}
}

func TestOAuthConsentNeverRedirectsToAnUnverifiedTarget(t *testing.T) {
	h := newOAuthHarness(t)
	clientID := h.register(oauthTestRedirect)
	for name, tc := range map[string]struct {
		query string
		code  string
	}{
		"unknown client":         {oauthAuthorizeQuery("ocl_unknown", oauthTestRedirect, "s"), "invalid_client"},
		"missing client":         {oauthAuthorizeQuery("", oauthTestRedirect, "s"), "invalid_client"},
		"unregistered redirect":  {oauthAuthorizeQuery(clientID, "https://attacker.example/cb", "s"), "invalid_redirect_uri"},
		"redirect prefix":        {oauthAuthorizeQuery(clientID, oauthTestRedirect+"/more", "s"), "invalid_redirect_uri"},
		"redirect case":          {oauthAuthorizeQuery(clientID, "https://APP.example/callback", "s"), "invalid_redirect_uri"},
		"missing redirect":       {oauthAuthorizeQuery(clientID, "", "s"), "invalid_redirect_uri"},
		"unresolvable document":  {oauthAuthorizeQuery("http://app.example/client.json", oauthTestRedirect, "s"), "invalid_client"},
		"duplicate client":       {oauthAuthorizeQuery(clientID, oauthTestRedirect, "s") + "&client_id=" + url.QueryEscape(clientID), "invalid_request"},
		"duplicate redirect":     {oauthAuthorizeQuery(clientID, oauthTestRedirect, "s") + "&redirect_uri=" + url.QueryEscape("https://attacker.example/cb"), "invalid_request"},
		"oversized state":        {oauthAuthorizeQuery(clientID, oauthTestRedirect, strings.Repeat("s", oauthStateMaxBytes+1)), "invalid_request"},
		"oversized metadata URL": {oauthAuthorizeQuery("https://app.example/"+strings.Repeat("a", oauthClientIDMaxBytes), oauthTestRedirect, "s"), "invalid_client"},
		"malformed resource":     {strings.Replace(oauthAuthorizeQuery(clientID, oauthTestRedirect, "s"), "resource="+url.QueryEscape(oauthTestAudience), "resource=%zz", 1), "invalid_request"},
	} {
		response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + tc.query, session: h.memberSession})
		body := expectOAuthError(t, response, http.StatusBadRequest, tc.code)
		if body["redirect_to"] != nil || response.Header().Get("Location") != "" {
			t.Fatalf("%s: unverified request offered a redirect: %v", name, body)
		}
		expectNoStore(t, response)
		// A decision body cannot repeat a field, so the repeated-field cases
		// apply to the query only.
		if strings.HasPrefix(name, "duplicate") || name == "malformed resource" {
			continue
		}
		post := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", session: h.memberSession, proof: true, contentType: "application/json",
			body: func() string {
				values, _ := url.ParseQuery(tc.query)
				decision := map[string]string{"decision": "approve"}
				for _, field := range oauthConsentFields {
					decision[field] = values.Get(field)
				}
				encoded, _ := json.Marshal(decision)
				return string(encoded)
			}()})
		if post.Code < 400 || decodeOAuthJSON(t, post)["redirect_to"] != nil {
			t.Fatalf("%s: POST status=%d body=%s", name, post.Code, post.Body.String())
		}
	}
	if len(h.oauth.issued) != 0 {
		t.Fatal("an unverified request minted a code")
	}
	// After the client and redirect URI are verified, protocol errors return
	// to the client with its state.
	for name, tc := range map[string]struct{ query, code string }{
		"response type":    {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "response_type", "token"), "unsupported_response_type"},
		"plain method":     {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "code_challenge_method", "plain"), "invalid_request"},
		"missing PKCE":     {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "code_challenge", ""), "invalid_request"},
		"short challenge":  {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "code_challenge", "abc"), "invalid_request"},
		"foreign resource": {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "resource", "https://other.example/mcp"), "invalid_target"},
		// An explicit empty resource is a value, not an omission.
		"empty resource": {oauthAuthorizeQuery(clientID, oauthTestRedirect, "keep me", "resource", ""), "invalid_target"},
	} {
		response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + tc.query, session: h.memberSession})
		body := expectOAuthError(t, response, http.StatusBadRequest, tc.code)
		target, err := url.Parse(fmt.Sprint(body["redirect_to"]))
		if err != nil || !strings.HasPrefix(target.String(), oauthTestRedirect+"?") || target.Query().Get("error") != tc.code || target.Query().Get("state") != "keep me" || target.Query().Get("code") != "" {
			t.Fatalf("%s: redirect_to=%v", name, body["redirect_to"])
		}
	}
	// On POST too, an explicit empty resource answers invalid_target and
	// mints no code, while an omitted resource takes the MCP audience.
	decision := func(resource *string) *httptest.ResponseRecorder {
		fields := map[string]any{
			"decision": "approve", "response_type": "code", "client_id": clientID, "redirect_uri": oauthTestRedirect,
			"code_challenge": core.OAuthS256Challenge(oauthTestVerifier), "code_challenge_method": "S256", "state": "keep me",
		}
		if resource != nil {
			fields["resource"] = *resource
		}
		body, _ := json.Marshal(fields)
		return h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: string(body), contentType: "application/json", session: h.memberSession, proof: true})
	}
	empty := ""
	refused := decision(&empty)
	body := expectOAuthError(t, refused, http.StatusBadRequest, "invalid_target")
	if target, err := url.Parse(fmt.Sprint(body["redirect_to"])); err != nil || target.Query().Get("error") != "invalid_target" || target.Query().Get("code") != "" || len(h.oauth.issued) != 0 {
		t.Fatalf("explicit empty resource on POST: %v, issued %d", body, len(h.oauth.issued))
	}
	// JSON null is an explicit value too: it is refused, not read as an
	// omission.
	nullBody, _ := json.Marshal(map[string]any{
		"decision": "approve", "response_type": "code", "client_id": clientID, "redirect_uri": oauthTestRedirect,
		"code_challenge": core.OAuthS256Challenge(oauthTestVerifier), "code_challenge_method": "S256", "state": "keep me", "resource": nil,
	})
	if !strings.Contains(string(nullBody), `"resource":null`) {
		t.Fatalf("null body = %s", nullBody)
	}
	nullRefused := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: string(nullBody), contentType: "application/json", session: h.memberSession, proof: true})
	if expectOAuthError(t, nullRefused, http.StatusBadRequest, "invalid_target"); len(h.oauth.issued) != 0 {
		t.Fatalf("null resource minted %d codes", len(h.oauth.issued))
	}
	approved := decision(nil)
	if approved.Code != http.StatusOK || len(h.oauth.issued) != 1 || h.oauth.issued[0].Resource != oauthTestAudience {
		t.Fatalf("omitted resource on POST: status=%d issued=%v", approved.Code, h.oauth.issued)
	}
}

func TestOAuthConsentDecisionMintsBoundSingleUseCode(t *testing.T) {
	h := newOAuthHarness(t)
	registeredRedirect := "https://app.example/cb?tenant=a%20b&code=stale&state=old&mode=x"
	clientID := h.register(registeredRedirect)
	for name, body := range map[string]string{
		"missing decision": strings.Replace(oauthDecisionBody("approve", clientID, registeredRedirect, "s"), `"decision":"approve",`, "", 1),
		"other decision":   oauthDecisionBody("maybe", clientID, registeredRedirect, "s"),
		"unknown field":    strings.Replace(oauthDecisionBody("approve", clientID, registeredRedirect, "s"), `"decision"`, `"extra":"x","decision"`, 1),
	} {
		response := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: body, contentType: "application/json", session: h.memberSession, proof: true})
		if got := expectOAuthError(t, response, http.StatusBadRequest, "invalid_request"); got["redirect_to"] != nil {
			t.Fatalf("%s: %v", name, got)
		}
	}
	// A larger decision body answers 413 and mints nothing
	// (component-http-api invariant 17).
	oversizedDecision := strings.Replace(oauthDecisionBody("approve", clientID, registeredRedirect, "s"), `"decision":"approve"`, `"decision":"approve","pad":"`+strings.Repeat("p", oauthConsentBodyLimit)+`"`, 1)
	oversized := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oversizedDecision, contentType: "application/json", session: h.memberSession, proof: true})
	if got := expectOAuthError(t, oversized, http.StatusRequestEntityTooLarge, "invalid_request"); got["redirect_to"] != nil || len(h.oauth.issued) != 0 {
		t.Fatalf("oversized decision: %v, issued %d", got, len(h.oauth.issued))
	}
	expectNoStore(t, oversized)
	state := "a b&c=d/é?+%"
	target := h.approve(h.memberSession, clientID, registeredRedirect, state)
	query := target.Query()
	code := query.Get("code")
	if !strings.HasPrefix(code, "cv_oac_") || query.Get("state") != state || query.Get("tenant") != "a b" || query.Get("mode") != "x" || len(query["code"]) != 1 || len(query["state"]) != 1 {
		t.Fatalf("redirect_to = %s", target)
	}
	if !strings.Contains(target.RawQuery, "tenant=a%20b") || target.Scheme+"://"+target.Host+target.Path != "https://app.example/cb" {
		t.Fatalf("redirect_to rewrote the registered URI: %s", target)
	}
	if len(h.oauth.issued) != 1 {
		t.Fatalf("issued %d codes", len(h.oauth.issued))
	}
	issued := h.oauth.issued[0]
	if issued.ExpiresAt.Sub(issued.CreatedAt) != core.OAuthAuthorizationCodeLifetime || core.OAuthAuthorizationCodeLifetime > 60*time.Second {
		t.Fatalf("code lifetime = %s", issued.ExpiresAt.Sub(issued.CreatedAt))
	}
	if issued.UserID != h.member.ID || issued.ClientID != clientID || issued.RedirectURI != registeredRedirect || issued.Resource != oauthTestAudience || issued.CodeChallenge != core.OAuthS256Challenge(oauthTestVerifier) {
		t.Fatalf("code bindings = %+v", issued)
	}
	denied := h.do(oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("deny", clientID, registeredRedirect, state), contentType: "application/json", session: h.memberSession, proof: true})
	if denied.Code != http.StatusOK {
		t.Fatalf("deny status=%d", denied.Code)
	}
	deniedTarget, _ := url.Parse(decodeOAuthJSON(t, denied)["redirect_to"].(string))
	if deniedTarget.Query().Get("error") != "access_denied" || deniedTarget.Query().Get("state") != state || deniedTarget.Query().Get("code") != "" || len(h.oauth.issued) != 1 {
		t.Fatalf("deny redirect_to = %s", deniedTarget)
	}
	// Without state, no state parameter is added.
	noState := h.approve(h.memberSession, clientID, registeredRedirect, "")
	if _, present := noState.Query()["state"]; present {
		t.Fatalf("redirect without state = %s", noState)
	}
	decodeTokenPair(t, h.token(codeExchangeForm(code, clientID, registeredRedirect)))
}

// --- Token exchange and refresh (AC-4.1, AC-4.2; security AC-2.1) ---

func TestOAuthCodeExchangeVerifiesEveryBindingAndIsSingleUse(t *testing.T) {
	h := newOAuthHarness(t)
	clientID := h.register(oauthTestRedirect, "https://app.example/other")
	otherClient := h.register(oauthTestRedirect)
	code := h.approve(h.memberSession, clientID, oauthTestRedirect, "s").Query().Get("code")
	logs := captureOAuthLogs(t)
	refusals := map[string]struct {
		form   url.Values
		status int
		code   string
	}{
		"wrong verifier":    {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "code_verifier", strings.Repeat("w", 43)), http.StatusBadRequest, "invalid_grant"},
		"short verifier":    {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "code_verifier", "short"), http.StatusBadRequest, "invalid_request"},
		"wrong client":      {codeExchangeForm(code, otherClient, oauthTestRedirect), http.StatusBadRequest, "invalid_grant"},
		"wrong redirect":    {codeExchangeForm(code, clientID, "https://app.example/other"), http.StatusBadRequest, "invalid_grant"},
		"foreign resource":  {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "resource", "https://other.example/mcp"), http.StatusBadRequest, "invalid_target"},
		"missing resource":  {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "resource", ""), http.StatusBadRequest, "invalid_target"},
		"missing client":    {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "client_id", ""), http.StatusBadRequest, "invalid_request"},
		"missing code":      {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "code", ""), http.StatusBadRequest, "invalid_request"},
		"unknown grant":     {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "grant_type", "password"), http.StatusBadRequest, "unsupported_grant_type"},
		"missing grant":     {withForm(codeExchangeForm(code, clientID, oauthTestRedirect), "grant_type", ""), http.StatusBadRequest, "invalid_request"},
		"unknown code":      {codeExchangeForm("cv_oac_oac_unknown_0123456789abcdefghijk", clientID, oauthTestRedirect), http.StatusBadRequest, "invalid_grant"},
		"metadata mismatch": {codeExchangeForm(code, "https://app.example/client.json", oauthTestRedirect), http.StatusBadRequest, "invalid_client"},
	}
	for name, refusal := range refusals {
		response := h.token(refusal.form)
		expectOAuthError(t, response, refusal.status, refusal.code)
		expectNoStore(t, response)
		if strings.Contains(response.Body.String(), code) || strings.Contains(response.Body.String(), oauthTestVerifier) {
			t.Fatalf("%s: refusal echoed the code or verifier: %s", name, response.Body.String())
		}
	}
	for name, request := range map[string]oauthRequest{
		"query parameters": {method: http.MethodPost, path: "/oauth/token?code=" + url.QueryEscape(code), body: codeExchangeForm(code, clientID, oauthTestRedirect).Encode(), contentType: "application/x-www-form-urlencoded"},
		"JSON body":        {method: http.MethodPost, path: "/oauth/token", body: `{"grant_type":"authorization_code"}`, contentType: "application/json"},
		"repeated code":    {method: http.MethodPost, path: "/oauth/token", body: codeExchangeForm(code, clientID, oauthTestRedirect).Encode() + "&code=other", contentType: "application/x-www-form-urlencoded"},
	} {
		expectOAuthError(t, h.do(request), http.StatusBadRequest, "invalid_request")
		_ = name
	}
	// A repeated parameter is refused with a fixed message that never echoes
	// the untrusted parameter name (req-security-boundaries REQ-2).
	secretName := "cv_oat_oat_echoedname_0123456789abcdefghij"
	repeated := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/token", body: codeExchangeForm(code, clientID, oauthTestRedirect).Encode() + "&" + secretName + "=1&" + secretName + "=2", contentType: "application/x-www-form-urlencoded"})
	if body := expectOAuthError(t, repeated, http.StatusBadRequest, "invalid_request"); strings.Contains(repeated.Body.String(), secretName) || body["error_description"] != "a parameter appears more than once" {
		t.Fatalf("repeated parameter refusal echoed input: %s", repeated.Body.String())
	}
	// A larger body answers 413, also when only its declared length is over
	// the bound (component-http-api invariant 17; DEC-105(3)).
	oversized := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/token", body: codeExchangeForm(code, clientID, oauthTestRedirect).Encode() + "&pad=" + strings.Repeat("p", oauthFormBodyLimit), contentType: "application/x-www-form-urlencoded"})
	expectOAuthError(t, oversized, http.StatusRequestEntityTooLarge, "invalid_request")
	expectNoStore(t, oversized)
	declared := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(codeExchangeForm(code, clientID, oauthTestRedirect).Encode()))
	declared.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	declared.ContentLength = oauthFormBodyLimit + 1
	declaredResponse := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(declaredResponse, declared)
	expectOAuthError(t, declaredResponse, http.StatusRequestEntityTooLarge, "invalid_request")
	// No refusal consumed the code: the exact bound exchange still wins.
	response := h.token(codeExchangeForm(code, clientID, oauthTestRedirect))
	pair := decodeTokenPair(t, response)
	expectNoStore(t, response)
	if pair.ExpiresIn < 3590 || pair.ExpiresIn > 3600 {
		t.Fatalf("expires_in = %d", pair.ExpiresIn)
	}
	issued := h.oauth.exchanged[len(h.oauth.exchanged)-1]
	if lifetime := time.Until(issued.AccessExpiresAt); lifetime > core.OAuthAccessTokenLifetime || lifetime < core.OAuthAccessTokenLifetime-time.Minute {
		t.Fatalf("access lifetime = %s", lifetime)
	}
	if lifetime := time.Until(issued.RefreshExpiresAt); lifetime > 30*24*time.Hour || lifetime < 30*24*time.Hour-time.Minute {
		t.Fatalf("refresh lifetime = %s", lifetime)
	}
	if issued.Resource != oauthTestAudience || issued.UserID != h.member.ID {
		t.Fatalf("pair = %+v", issued.OAuthTokenPair)
	}
	// Replay fails.
	expectOAuthError(t, h.token(codeExchangeForm(code, clientID, oauthTestRedirect)), http.StatusBadRequest, "invalid_grant")
	for _, secret := range []string{code, oauthTestVerifier, pair.AccessToken, pair.RefreshToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a log line carried a secret: %s", logs.String())
		}
	}
}

func TestOAuthConcurrentCodeExchangeHasOneWinner(t *testing.T) {
	h := newOAuthHarness(t)
	clientID := h.register(oauthTestRedirect)
	code := h.approve(h.memberSession, clientID, oauthTestRedirect, "s").Query().Get("code")
	const racers = 16
	type outcome struct {
		status int
		body   string
	}
	statuses := make(chan outcome, racers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			request := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(codeExchangeForm(code, clientID, oauthTestRedirect).Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			h.server.Handler().ServeHTTP(response, request)
			statuses <- outcome{status: response.Code, body: response.Body.String()}
		}()
	}
	start.Done()
	done.Wait()
	close(statuses)
	wins := 0
	for result := range statuses {
		switch result.status {
		case http.StatusOK:
			wins++
		case http.StatusBadRequest:
			// Every losing exchange is a replay of the consumed code.
			var refusal map[string]any
			if err := json.Unmarshal([]byte(result.body), &refusal); err != nil || refusal["error"] != "invalid_grant" {
				t.Fatalf("losing exchange answered %s, want invalid_grant", result.body)
			}
		default:
			t.Fatalf("racer status=%d", result.status)
		}
	}
	if wins != 1 {
		t.Fatalf("%d exchanges won, want exactly 1", wins)
	}
}

func TestOAuthRefreshRotatesAndReuseRevokesTheGrant(t *testing.T) {
	h := newOAuthHarness(t)
	clientID, first := h.grant()
	otherClient := h.register(oauthTestRedirect)
	expectOAuthError(t, h.token(refreshForm(first.RefreshToken, otherClient)), http.StatusBadRequest, "invalid_grant")
	expectOAuthError(t, h.token(withForm(refreshForm(first.RefreshToken, clientID), "resource", "https://other.example/mcp")), http.StatusBadRequest, "invalid_target")
	expectOAuthError(t, h.token(withForm(refreshForm(first.RefreshToken, clientID), "resource", "")), http.StatusBadRequest, "invalid_target")
	expectOAuthError(t, h.token(refreshForm(first.AccessToken, clientID)), http.StatusBadRequest, "invalid_grant")
	second := decodeTokenPair(t, h.token(refreshForm(first.RefreshToken, clientID)))
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatal("refresh did not rotate the pair")
	}
	if result := h.mcp(second.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK || result.isError {
		t.Fatalf("rotated access token: %+v", result)
	}
	// Presenting the rotated refresh token again refuses with a clear sign-in
	// message and revokes the whole grant at once.
	reused := expectOAuthError(t, h.token(refreshForm(first.RefreshToken, clientID)), http.StatusBadRequest, "invalid_grant")
	if !strings.Contains(fmt.Sprint(reused["error_description"]), "sign in again") {
		t.Fatalf("reuse description = %v", reused["error_description"])
	}
	expectOAuthError(t, h.token(refreshForm(second.RefreshToken, clientID)), http.StatusBadRequest, "invalid_grant")
	if result := h.mcp(second.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != oauthResourceChallenge {
		t.Fatalf("access token after reuse: %+v", result)
	}
	grants, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID)
	if err != nil || len(grants) != 0 {
		t.Fatalf("grants after reuse = %v, %v", grants, err)
	}
}

// faultOAuthStore fails exactly one named OAuthStore method with err and
// passes every other call to the real store.
type faultOAuthStore struct {
	store.OAuthStore
	method string
	err    error
}

func (f faultOAuthStore) fail(method string) error {
	if f.method == method {
		return f.err
	}
	return nil
}

func (f faultOAuthStore) RegisterOAuthClient(ctx context.Context, registration store.OAuthClientRegistration) (core.OAuthClient, error) {
	if err := f.fail("RegisterOAuthClient"); err != nil {
		return core.OAuthClient{}, err
	}
	return f.OAuthStore.RegisterOAuthClient(ctx, registration)
}

func (f faultOAuthStore) GetOAuthClient(ctx context.Context, clientID string) (core.OAuthClient, error) {
	if err := f.fail("GetOAuthClient"); err != nil {
		return core.OAuthClient{}, err
	}
	return f.OAuthStore.GetOAuthClient(ctx, clientID)
}

func (f faultOAuthStore) DeleteExpiredOAuthClients(ctx context.Context) (int, error) {
	if err := f.fail("DeleteExpiredOAuthClients"); err != nil {
		return 0, err
	}
	return f.OAuthStore.DeleteExpiredOAuthClients(ctx)
}

func (f faultOAuthStore) IssueOAuthAuthorizationCode(ctx context.Context, request store.OAuthCodeRequest) (core.OAuthAuthorizationCode, string, error) {
	if err := f.fail("IssueOAuthAuthorizationCode"); err != nil {
		return core.OAuthAuthorizationCode{}, "", err
	}
	return f.OAuthStore.IssueOAuthAuthorizationCode(ctx, request)
}

func (f faultOAuthStore) ExchangeOAuthAuthorizationCode(ctx context.Context, exchange store.OAuthCodeExchange) (core.IssuedOAuthTokenPair, error) {
	if err := f.fail("ExchangeOAuthAuthorizationCode"); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	return f.OAuthStore.ExchangeOAuthAuthorizationCode(ctx, exchange)
}

func (f faultOAuthStore) RefreshOAuthTokens(ctx context.Context, exchange store.OAuthRefreshExchange) (core.IssuedOAuthTokenPair, error) {
	if err := f.fail("RefreshOAuthTokens"); err != nil {
		return core.IssuedOAuthTokenPair{}, err
	}
	return f.OAuthStore.RefreshOAuthTokens(ctx, exchange)
}

func (f faultOAuthStore) VerifyOAuthAccessToken(ctx context.Context, value string) (core.AuthenticatedCredential, error) {
	if err := f.fail("VerifyOAuthAccessToken"); err != nil {
		return core.AuthenticatedCredential{}, err
	}
	return f.OAuthStore.VerifyOAuthAccessToken(ctx, value)
}

func (f faultOAuthStore) ListOwnOAuthGrants(ctx context.Context, userID string) ([]core.OAuthGrant, error) {
	if err := f.fail("ListOwnOAuthGrants"); err != nil {
		return nil, err
	}
	return f.OAuthStore.ListOwnOAuthGrants(ctx, userID)
}

func (f faultOAuthStore) RevokeOwnOAuthGrant(ctx context.Context, userID, grantID string) (core.OAuthGrant, error) {
	if err := f.fail("RevokeOwnOAuthGrant"); err != nil {
		return core.OAuthGrant{}, err
	}
	return f.OAuthStore.RevokeOwnOAuthGrant(ctx, userID, grantID)
}

func (f faultOAuthStore) RevokeOAuthToken(ctx context.Context, clientID, token string) error {
	if err := f.fail("RevokeOAuthToken"); err != nil {
		return err
	}
	return f.OAuthStore.RevokeOAuthToken(ctx, clientID, token)
}

// failingCallerIdentities fails the consent page's identity read.
type failingCallerIdentities struct{ err error }

func (f failingCallerIdentities) GetCallerIdentity(context.Context, string, string) (core.CallerIdentity, error) {
	return core.CallerIdentity{}, f.err
}

// failingDashboardSessions fails the dashboard session check that runs in
// front of the consent and grant handlers.
type failingDashboardSessions struct {
	store.InvitationSessionStore
	err error
}

func (f failingDashboardSessions) VerifyDashboardSession(context.Context, string) (core.AuthenticatedCredential, error) {
	return core.AuthenticatedCredential{}, f.err
}

// TestOAuthStoreFailuresAreGeneric injects a store failure into every
// store-backed OAuth route, once as an ordinary error that carries a
// credential and once as store.ErrRetryable. Each answer is the generic 500
// with no internal detail, and the log holds the redacted cause with the
// request ID (DEC-105(1), (5); component-http-api invariant 16). /mcp
// admission and the dashboard session check in front of the consent and
// grant handlers keep the 500 "credential verification failed" of the shared
// credential path (component-http-api invariant 4), with the same log.
func TestOAuthStoreFailuresAreGeneric(t *testing.T) {
	h := newOAuthHarness(t)
	clientID, pair := h.grant()
	grants, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID)
	if err != nil || len(grants) != 1 {
		t.Fatal(grants, err)
	}
	grantID := grants[0].ID
	code := h.approve(h.memberSession, clientID, oauthTestRedirect, "s").Query().Get("code")
	registration := `{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`
	query := "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, "s")
	type route struct {
		name, method string
		request      func() oauthRequest
		mcp          bool
		identities   bool
		sessions     bool
	}
	routes := []route{
		{name: "registration", method: "RegisterOAuthClient", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/oauth/register", body: registration, contentType: "application/json", remote: h.nextSource()}
		}},
		{name: "registration cleanup", method: "DeleteExpiredOAuthClients", request: func() oauthRequest {
			h.server.oauthCleanupMu.Lock()
			h.server.oauthCleanupAt = time.Time{}
			h.server.oauthCleanupMu.Unlock()
			return oauthRequest{method: http.MethodPost, path: "/oauth/register", body: registration, contentType: "application/json", remote: h.nextSource()}
		}},
		{name: "consent GET client lookup", method: "GetOAuthClient", request: func() oauthRequest {
			return oauthRequest{method: http.MethodGet, path: query, session: h.memberSession}
		}},
		{name: "consent GET identity", identities: true, request: func() oauthRequest {
			return oauthRequest{method: http.MethodGet, path: query, session: h.memberSession}
		}},
		{name: "consent POST client lookup", method: "GetOAuthClient", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"), contentType: "application/json", session: h.memberSession, proof: true}
		}},
		{name: "consent POST code issue", method: "IssueOAuthAuthorizationCode", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"), contentType: "application/json", session: h.memberSession, proof: true}
		}},
		{name: "token code exchange", method: "ExchangeOAuthAuthorizationCode", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/oauth/token", body: codeExchangeForm(code, clientID, oauthTestRedirect).Encode(), contentType: "application/x-www-form-urlencoded"}
		}},
		{name: "token refresh", method: "RefreshOAuthTokens", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/oauth/token", body: refreshForm(pair.RefreshToken, clientID).Encode(), contentType: "application/x-www-form-urlencoded"}
		}},
		{name: "revocation", method: "RevokeOAuthToken", request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/oauth/revoke", body: url.Values{"token": {pair.AccessToken}, "client_id": {clientID}}.Encode(), contentType: "application/x-www-form-urlencoded"}
		}},
		{name: "grant list", method: "ListOwnOAuthGrants", request: func() oauthRequest {
			return oauthRequest{method: http.MethodGet, path: "/v1/oauth/grants", session: h.memberSession}
		}},
		{name: "grant delete", method: "RevokeOwnOAuthGrant", request: func() oauthRequest {
			return oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: h.memberSession, proof: true}
		}},
		{name: "consent GET session check", sessions: true, request: func() oauthRequest {
			return oauthRequest{method: http.MethodGet, path: query, session: h.memberSession}
		}},
		{name: "consent POST session check", sessions: true, request: func() oauthRequest {
			return oauthRequest{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"), contentType: "application/json", session: h.memberSession, proof: true}
		}},
		{name: "grant list session check", sessions: true, request: func() oauthRequest {
			return oauthRequest{method: http.MethodGet, path: "/v1/oauth/grants", session: h.memberSession}
		}},
		{name: "grant delete session check", sessions: true, request: func() oauthRequest {
			return oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: h.memberSession, proof: true}
		}},
		{name: "mcp token verification", method: "VerifyOAuthAccessToken", mcp: true},
		{name: "mcp grant audience read", method: "ListOwnOAuthGrants", mcp: true},
	}
	secret := pair.RefreshToken
	for _, tc := range routes {
		for errName, injected := range map[string]error{
			"store error": errors.New("database refused query for " + secret),
			"retryable":   fmt.Errorf("serialization conflict: %w", store.ErrRetryable),
		} {
			t.Run(tc.name+"/"+errName, func(t *testing.T) {
				logs := captureOAuthLogs(t)
				h.server.OAuth = faultOAuthStore{OAuthStore: h.oauth, method: tc.method, err: injected}
				identities := h.server.CallerIdentities
				if tc.identities {
					h.server.CallerIdentities = failingCallerIdentities{err: injected}
				}
				sessions := h.server.InvitationSessions
				if tc.sessions {
					h.server.InvitationSessions = failingDashboardSessions{InvitationSessionStore: sessions, err: injected}
				}
				t.Cleanup(func() {
					h.server.OAuth = h.oauth
					h.server.CallerIdentities = identities
					h.server.InvitationSessions = sessions
				})
				requestID := "req-" + strings.ReplaceAll(tc.name, " ", "-") + "-" + strings.ReplaceAll(errName, " ", "-")
				var status int
				var body string
				var header http.Header
				if tc.mcp {
					request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_stage_preferences","arguments":{"workspace_id":"alpha"}}}`))
					request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
					request.Header.Set("X-Request-Id", requestID)
					response := httptest.NewRecorder()
					h.server.Handler().ServeHTTP(response, request)
					status, body, header = response.Code, response.Body.String(), response.Header()
					if strings.TrimSpace(body) != "credential verification failed" {
						t.Fatalf("body=%q", body)
					}
				} else {
					request := tc.request()
					request.header = map[string]string{"X-Request-Id": requestID}
					response := h.do(request)
					status, body, header = response.Code, response.Body.String(), response.Header()
					want := "internal server error"
					if tc.sessions {
						want = "credential verification failed"
					}
					if strings.TrimSpace(body) != want || header.Get("WWW-Authenticate") != "" {
						t.Fatalf("body=%q challenge=%q", body, header.Get("WWW-Authenticate"))
					}
					expectNoStore(t, response)
				}
				if status != http.StatusInternalServerError || header.Get("Retry-After") != "" {
					t.Fatalf("status=%d retry-after=%q body=%q", status, header.Get("Retry-After"), body)
				}
				text := logs.String()
				if !strings.Contains(text, `request_id="`+requestID+`"`) || strings.Contains(text, secret) {
					t.Fatalf("log does not hold the redacted cause with the request ID: %s", text)
				}
				cause := "serialization conflict"
				if errName == "store error" {
					cause = "database refused query for"
				}
				if !strings.Contains(text, cause) {
					t.Fatalf("log does not hold the cause %q: %s", cause, text)
				}
			})
		}
	}
	// An unknown session is still a refused credential: 401 with the bearer
	// challenge on all four session mounts, and no failure log.
	for _, request := range []oauthRequest{
		{method: http.MethodGet, path: query, session: "unknown-session"},
		{method: http.MethodPost, path: "/v1/oauth/authorize", body: oauthDecisionBody("approve", clientID, oauthTestRedirect, "s"), contentType: "application/json", session: "unknown-session", proof: true},
		{method: http.MethodGet, path: "/v1/oauth/grants", session: "unknown-session"},
		{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: "unknown-session", proof: true},
	} {
		logs := captureOAuthLogs(t)
		response := h.do(request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" || strings.Contains(logs.String(), "credential verification failed") {
			t.Fatalf("%s %s with an unknown session: status=%d challenge=%q log=%s", request.method, request.path, response.Code, response.Header().Get("WWW-Authenticate"), logs.String())
		}
		expectNoStore(t, response)
	}
	// Nothing changed: the grant and its pair still work, the code still
	// exchanges once, and the original refresh token still rotates.
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
		t.Fatalf("token after injected failures: %+v", result)
	}
	decodeTokenPair(t, h.token(codeExchangeForm(code, clientID, oauthTestRedirect)))
	decodeTokenPair(t, h.token(refreshForm(pair.RefreshToken, clientID)))
}

func withForm(form url.Values, key, value string) url.Values {
	copied := url.Values{}
	for k, v := range form {
		copied[k] = append([]string(nil), v...)
	}
	if value == "" {
		copied.Del(key)
	} else {
		copied.Set(key, value)
	}
	return copied
}

// captureOAuthLogs redirects the standard logger and the request logger to
// one buffer for a test that does not run in parallel.
func captureOAuthLogs(t *testing.T) *lockedLogBuffer {
	t.Helper()
	buffer := captureStandardLog(t)
	originalLogger := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: log.New(buffer, "", 0), NoColor: true})
	t.Cleanup(func() { middleware.DefaultLogger = originalLogger })
	return buffer
}

// --- Revocation and grant management (AC-4.4, AC-4.5) ---

func TestOAuthRevocationEndpointIsIdempotentAndRevokesTheGrant(t *testing.T) {
	h := newOAuthHarness(t)
	revoke := func(form url.Values) *httptest.ResponseRecorder {
		return h.do(oauthRequest{method: http.MethodPost, path: "/oauth/revoke", body: form.Encode(), contentType: "application/x-www-form-urlencoded"})
	}
	expectOAuthError(t, revoke(url.Values{"token": {"x"}}), http.StatusBadRequest, "invalid_request")
	expectOAuthError(t, revoke(url.Values{"client_id": {"x"}}), http.StatusBadRequest, "invalid_request")
	// A larger body answers 413 (component-http-api invariant 17).
	expectOAuthError(t, revoke(url.Values{"token": {"x"}, "client_id": {"y"}, "pad": {strings.Repeat("p", oauthFormBodyLimit)}}), http.StatusRequestEntityTooLarge, "invalid_request")
	// A repeated parameter is refused without echoing its name.
	secretName := "cv_ort_ort_echoedname_0123456789abcdefghij"
	repeated := h.do(oauthRequest{method: http.MethodPost, path: "/oauth/revoke", body: "token=x&client_id=y&" + secretName + "=1&" + secretName + "=2", contentType: "application/x-www-form-urlencoded"})
	if body := expectOAuthError(t, repeated, http.StatusBadRequest, "invalid_request"); strings.Contains(repeated.Body.String(), secretName) || body["error_description"] != "a parameter appears more than once" {
		t.Fatalf("repeated parameter refusal echoed input: %s", repeated.Body.String())
	}
	if response := revoke(url.Values{"token": {"cv_oat_oat_unknown_0123456789abcdef"}, "client_id": {"ocl_x"}}); response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("unknown token status=%d body=%s", response.Code, response.Body.String())
	}
	for _, kind := range []string{"access", "refresh"} {
		t.Run(kind, func(t *testing.T) {
			clientID, pair := h.grant()
			token := pair.AccessToken
			if kind == "refresh" {
				token = pair.RefreshToken
			}
			// Another client's request changes nothing.
			if response := revoke(url.Values{"token": {token}, "client_id": {"ocl_other"}}); response.Code != http.StatusOK {
				t.Fatalf("foreign client status=%d", response.Code)
			}
			if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
				t.Fatalf("token after foreign revocation: %+v", result)
			}
			response := revoke(url.Values{"token": {token}, "client_id": {clientID}, "token_type_hint": {"refresh_token"}})
			if response.Code != http.StatusOK {
				t.Fatalf("revoke status=%d", response.Code)
			}
			expectNoStore(t, response)
			if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusUnauthorized {
				t.Fatalf("access token after revocation: %+v", result)
			}
			expectOAuthError(t, h.token(refreshForm(pair.RefreshToken, clientID)), http.StatusBadRequest, "invalid_grant")
			if again := revoke(url.Values{"token": {token}, "client_id": {clientID}}); again.Code != http.StatusOK {
				t.Fatalf("repeat revocation status=%d", again.Code)
			}
		})
	}
}

// TestOAuthGrantListingFollowsThePageContract proves that the grant listing
// uses the shared page parameters and headers and stays owner-scoped and
// hash-free (component-http-api invariant 19; DEC-105(4)).
func TestOAuthGrantListingFollowsThePageContract(t *testing.T) {
	h := newOAuthHarness(t)
	var issued []oauthTokenResponse
	for range 3 {
		_, pair := h.grant()
		issued = append(issued, pair)
	}
	list := func(query, session string) ([]map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/grants" + query, session: session})
		if response.Code != http.StatusOK {
			return nil, response
		}
		var grants []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &grants); err != nil {
			t.Fatalf("%s: %v body=%s", query, err, response.Body.String())
		}
		for _, pair := range issued {
			if strings.Contains(response.Body.String(), pair.AccessToken) || strings.Contains(response.Body.String(), pair.RefreshToken) {
				t.Fatalf("%s: listing carried a token value", query)
			}
		}
		return grants, response
	}
	// Without limit, the whole array and no paging header.
	all, response := list("", h.memberSession)
	if len(all) != 3 || response.Header().Get("X-Conveyor-Total") != "" || response.Header().Get("X-Conveyor-Limit") != "" || response.Header().Get("X-Conveyor-Offset") != "" {
		t.Fatalf("unpaged listing: %d grants, headers %v", len(all), response.Header())
	}
	headers := func(response *httptest.ResponseRecorder) string {
		return response.Header().Get("X-Conveyor-Total") + "/" + response.Header().Get("X-Conveyor-Limit") + "/" + response.Header().Get("X-Conveyor-Offset")
	}
	first, response := list("?limit=2", h.memberSession)
	if len(first) != 2 || headers(response) != "3/2/0" {
		t.Fatalf("first page: %d grants, headers %s", len(first), headers(response))
	}
	second, response := list("?limit=2&offset=2", h.memberSession)
	if len(second) != 1 || headers(response) != "3/2/2" {
		t.Fatalf("second page: %d grants, headers %s", len(second), headers(response))
	}
	// The pages keep the unpaged order, newest first.
	for index, grant := range append(first, second...) {
		if grant["id"] != all[index]["id"] {
			t.Fatalf("page order differs at %d: %v vs %v", index, grant["id"], all[index]["id"])
		}
	}
	past, response := list("?limit=2&offset=10", h.memberSession)
	if len(past) != 0 || headers(response) != "3/2/10" || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("page past the end: %d grants, headers %s body %s", len(past), headers(response), response.Body.String())
	}
	// Another person's page is empty: the owner scope holds under paging.
	foreign, response := list("?limit=10", h.otherSession)
	if len(foreign) != 0 || headers(response) != "0/10/0" {
		t.Fatalf("foreign page: %d grants, headers %s", len(foreign), headers(response))
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=x", "?limit=1&limit=2", "?offset=1", "?limit=1&offset=-1", "?limit=1&offset=2147483648", "?limit=1&offset=1&offset=2"} {
		if _, response := list(query, h.memberSession); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", query, response.Code)
		}
	}
}

func TestOAuthGrantManagementIsOwnerScopedAndHashFree(t *testing.T) {
	h := newOAuthHarness(t)
	clientID, pair := h.grant()
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
		t.Fatalf("mcp: %+v", result)
	}
	for name, request := range map[string]oauthRequest{
		"session": {method: http.MethodGet, path: "/v1/oauth/grants", session: h.memberSession},
	} {
		response := h.do(request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		expectNoStore(t, response)
		body := response.Body.String()
		for _, forbidden := range []string{pair.AccessToken, pair.RefreshToken, "cv_oat_", "cv_ort_", "hash", "digest", "secret"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s: grant listing carried %q: %s", name, forbidden, body)
			}
		}
		var grants []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &grants); err != nil || len(grants) != 1 {
			t.Fatalf("%s: grants = %s", name, body)
		}
		grant := grants[0]
		if grant["client_id"] != clientID || grant["client_name"] != "Test Agent" || grant["created_at"] == nil || grant["last_used_at"] == nil || grant["resource"] != oauthTestAudience {
			t.Fatalf("%s: grant = %v", name, grant)
		}
	}
	grants, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID)
	if err != nil || len(grants) != 1 {
		t.Fatal(grants, err)
	}
	grantID := grants[0].ID
	// Grant management is dashboard-session only: a personal access token is
	// refused on both routes, also beside a valid session cookie, and
	// revokes nothing.
	for name, request := range map[string]oauthRequest{
		"list with token":               {method: http.MethodGet, path: "/v1/oauth/grants", bearer: h.memberPAT},
		"list with token and session":   {method: http.MethodGet, path: "/v1/oauth/grants", bearer: h.memberPAT, session: h.memberSession},
		"revoke with token":             {method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, bearer: h.memberPAT},
		"revoke with token and session": {method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, bearer: h.memberPAT, session: h.memberSession, proof: true},
		"list with access token":        {method: http.MethodGet, path: "/v1/oauth/grants", bearer: pair.AccessToken, session: h.memberSession},
		"revoke with access token":      {method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, bearer: pair.AccessToken, session: h.memberSession, proof: true},
	} {
		response := h.do(request)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), grantID) {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		expectNoStore(t, response)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
		t.Fatalf("token after refused token revocations: %+v", result)
	}
	// Another person sees no grant and cannot revoke this one.
	if response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/grants", session: h.otherSession}); response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("other listing status=%d body=%s", response.Code, response.Body.String())
	}
	foreign := h.do(oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: h.otherSession, proof: true})
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign revoke status=%d", foreign.Code)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK {
		t.Fatalf("token after foreign revoke: %+v", result)
	}
	// A session DELETE needs the mutation proof.
	if response := h.do(oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: h.memberSession}); response.Code != http.StatusForbidden {
		t.Fatalf("revoke without proof status=%d", response.Code)
	}
	revoked := h.do(oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/" + grantID, session: h.memberSession, proof: true})
	if revoked.Code != http.StatusOK || strings.Contains(revoked.Body.String(), "cv_o") {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusUnauthorized {
		t.Fatalf("access token after Settings revocation: %+v", result)
	}
	expectOAuthError(t, h.token(refreshForm(pair.RefreshToken, clientID)), http.StatusBadRequest, "invalid_grant")
	if response := h.do(oauthRequest{method: http.MethodDelete, path: "/v1/oauth/grants/ogr_missing", session: h.memberSession, proof: true}); response.Code != http.StatusNotFound {
		t.Fatalf("missing grant status=%d", response.Code)
	}
}

// --- /mcp admission and REST refusal (AC-4.3; security AC-1.4) ---

// TestOAuthAccessTokenIsBoundToTheCurrentAudience proves that /mcp admits an
// OAuth access token only while its grant's resource equals the configured
// <public-url>/mcp (DEC-146(4); req-mcp-oauth AC-4.3).
func TestOAuthAccessTokenIsBoundToTheCurrentAudience(t *testing.T) {
	h := newOAuthHarness(t)
	_, pair := h.grant()
	call := func() oauthMCPResult {
		return h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
	}
	if result := call(); result.status != http.StatusOK || result.isError {
		t.Fatalf("token at its own audience: %+v", result)
	}
	// The operator moves the server to a new public URL. The otherwise valid
	// token was issued for the earlier audience, so it is refused, and the
	// challenge names the new metadata.
	h.server.InvitationDelivery.PublicURL = "https://conveyor-new.example"
	result := call()
	if result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != `Bearer resource_metadata="https://conveyor-new.example/.well-known/oauth-protected-resource/mcp"` {
		t.Fatalf("token after a public URL change: status=%d challenge=%q", result.status, result.header.Get("WWW-Authenticate"))
	}
	// A personal access token is not audience-bound and still works.
	if personal := h.mcp(h.memberPAT, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); personal.status != http.StatusOK || personal.isError {
		t.Fatalf("PAT after a public URL change: %+v", personal)
	}
	// The refusal reads the audience and revokes nothing: back at the
	// original URL the token works again.
	h.server.InvitationDelivery.PublicURL = oauthTestIssuer
	if result := call(); result.status != http.StatusOK || result.isError {
		t.Fatalf("token back at its own audience: %+v", result)
	}
	// A store failure while reading the grant is a server failure, not a
	// refusal.
	h.oauth.listErr = errors.New("database offline")
	if result := call(); result.status != http.StatusInternalServerError || !strings.Contains(result.text, "credential verification failed") {
		t.Fatalf("grant read failure: %+v", result)
	}
}

// TestOAuthPostEndpointsRefuseOtherMethodsWithoutHTML proves that the three
// POST endpoints are mounted for every method, so GET and HEAD answer 405
// with Allow: POST and never dashboard HTML, with OAuth on and off
// (component-http-api invariant 2).
func TestOAuthPostEndpointsRefuseOtherMethodsWithoutHTML(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		h := newOAuthHarness(t)
		if !enabled {
			h.server.InvitationDelivery.PublicURL = ""
		}
		for _, path := range []string{"/oauth/register", "/oauth/token", "/oauth/revoke"} {
			for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
				response := h.do(oauthRequest{method: method, path: path})
				body := response.Body.String()
				if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost ||
					strings.Contains(strings.ToLower(body), "<html") || strings.Contains(response.Header().Get("Content-Type"), "text/html") {
					t.Fatalf("enabled=%t %s %s: status=%d allow=%q type=%q body=%s", enabled, method, path, response.Code, response.Header().Get("Allow"), response.Header().Get("Content-Type"), body)
				}
				expectNoStore(t, response)
				if method != http.MethodHead {
					expectOAuthError(t, response, http.StatusMethodNotAllowed, "method_not_allowed")
				}
			}
			// A POST still reaches the endpoint, and the disabled state
			// still answers oauth_unavailable.
			post := h.do(oauthRequest{method: http.MethodPost, path: path, body: "x=y", contentType: "application/x-www-form-urlencoded"})
			if !enabled {
				expectOAuthError(t, post, http.StatusNotFound, "oauth_unavailable")
			} else if post.Code == http.StatusMethodNotAllowed {
				t.Fatalf("POST %s answered 405", path)
			}
		}
	}
}

func TestOAuthAccessTokenActsAsItsOwnerWithLiveCapabilities(t *testing.T) {
	h := newOAuthHarness(t)
	_, pair := h.grant()
	set := h.mcp(pair.AccessToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "verify", "value": map[string]any{"harness": "codex", "model": "oauth-model"}})
	if set.status != http.StatusOK || set.isError {
		t.Fatalf("set_stage_preference: %+v", set)
	}
	// The act is recorded as the token's owner: the owner's own personal
	// access token reads the new preference, and another person's does not.
	// A personal access token still works on /mcp.
	readVerify := func(token string) *store.StagePreferenceEntry {
		t.Helper()
		result := h.mcp(token, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
		if result.status != http.StatusOK || result.isError {
			t.Fatalf("get_stage_preferences: %+v", result)
		}
		var set store.StagePreferences
		if err := json.Unmarshal([]byte(result.text), &set); err != nil {
			t.Fatalf("decode %s: %v", result.text, err)
		}
		return set.Stages.Verify
	}
	if verify := readVerify(h.memberPAT); verify == nil || verify.Model != "oauth-model" {
		t.Fatalf("member verify preference = %+v", verify)
	}
	if verify := readVerify(oauthTestOwnerPAT); verify != nil {
		t.Fatalf("the act was recorded for another person: %+v", verify)
	}
	// A removed workspace role applies on the next call.
	if err := h.st.RevokeWorkspaceRole(h.ownerCtx, h.member.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK || !result.isError || !strings.Contains(result.text, "workspace_not_found") {
		t.Fatalf("call after role removal: %+v", result)
	}
	// Deactivation refuses the token at its next use.
	deactivator, ok := h.st.(interface {
		DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
	})
	if !ok {
		t.Fatal("backend cannot deactivate users")
	}
	if _, err := deactivator.DeactivateIdentityUser(store.WithActor(t.Context(), store.Actor{ID: "system", Role: core.ActorSystem}), h.member.ID); err != nil {
		t.Fatal(err)
	}
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusUnauthorized || result.header.Get("WWW-Authenticate") != oauthResourceChallenge {
		t.Fatalf("call after deactivation: %+v", result)
	}
}

func TestOAuthAccessTokenIsRefusedOnEveryNonMCPRoute(t *testing.T) {
	h := newOAuthHarness(t)
	_, pair := h.grant()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/v1/version"},
		{http.MethodGet, "/v1/me"},
		{http.MethodGet, "/v1/tokens"},
		{http.MethodPost, "/v1/tokens"},
		{http.MethodGet, "/v1/workspaces"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodGet, "/v1/workspaces/alpha/members"},
		{http.MethodGet, "/v1/me/stage-preferences"},
		{http.MethodGet, "/v1/oauth/grants"},
		{http.MethodGet, "/v1/oauth/authorize"},
		{http.MethodGet, "/v1/work-orders"},
		{http.MethodPost, "/v1/work-orders/wo-1/submit-for-review"},
		{http.MethodPost, "/v1/work-orders/wo-1/context-refresh"},
		{http.MethodGet, "/v1/work-orders/wo-1/pull-request-template"},
		{http.MethodPost, "/v1/work-orders/wo-1/verification/attempts"},
		{http.MethodPost, "/v1/sign-in/password"},
		{http.MethodPost, "/oauth/register"},
		{http.MethodPost, "/oauth/token"},
		{http.MethodPost, "/oauth/revoke"},
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/tasks"},
		{http.MethodGet, "/oauth/authorize"},
		{http.MethodGet, "/mcp/"},
		{http.MethodGet, "/%6dcp"},
		{http.MethodPost, "/%6Dcp"},
	} {
		for _, session := range []string{"", h.memberSession} {
			response := h.do(oauthRequest{method: route.method, path: route.path, bearer: pair.AccessToken, session: session, proof: route.method != http.MethodGet, contentType: "application/json", body: "{}"})
			if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s (session=%t): status=%d challenge=%q body=%s", route.method, route.path, session != "", response.Code, response.Header().Get("WWW-Authenticate"), response.Body.String())
			}
			// An OAuth route keeps its privacy headers on the early refusal.
			if oauthLoggedPath(route.path) {
				expectNoStore(t, response)
			}
		}
	}
	// The scheme is matched without regard to case, so a lowercase scheme
	// cannot slip the token past the refusal beside a session.
	for _, scheme := range []string{"bearer", "BEARER"} {
		response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/me", session: h.memberSession, header: map[string]string{"Authorization": scheme + " " + pair.AccessToken}})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s scheme: status=%d", scheme, response.Code)
		}
	}
	// The same person's personal access token still reaches REST routes.
	if response := h.do(oauthRequest{method: http.MethodGet, path: "/v1/me", bearer: h.memberPAT}); response.Code != http.StatusOK {
		t.Fatalf("PAT /v1/me status=%d", response.Code)
	}
	// /mcp itself admits the token, with any letter case of the scheme and
	// more than one separating space.
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK || result.isError {
		t.Fatalf("/mcp: %+v", result)
	}
	for _, header := range []string{"bearer " + pair.AccessToken, "BEARER  " + pair.AccessToken, "Bearer   " + h.memberPAT} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_stage_preferences","arguments":{"workspace_id":"alpha"}}}`))
		request.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"isError":true`) {
			t.Fatalf("/mcp with %q: status=%d body=%s", strings.Fields(header)[0], response.Code, response.Body.String())
		}
	}
}

func TestOAuthQueryStringsStayOutOfTheAccessLog(t *testing.T) {
	h := newOAuthHarness(t)
	clientID := h.register(oauthTestRedirect)
	logs := captureOAuthLogs(t)
	state := "very-private-state-value"
	h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, state), session: h.memberSession})
	h.do(oauthRequest{method: http.MethodGet, path: "/oauth/authorize?" + oauthAuthorizeQuery(clientID, oauthTestRedirect, state)})
	h.do(oauthRequest{method: http.MethodPost, path: "/oauth/token?code=query-code-value", body: "grant_type=authorization_code", contentType: "application/x-www-form-urlencoded"})
	text := logs.String()
	if !strings.Contains(text, "/v1/oauth/authorize") || !strings.Contains(text, "/oauth/token") {
		t.Fatalf("access log missing OAuth paths: %s", text)
	}
	for _, secret := range []string{state, "query-code-value", "code_challenge", url.QueryEscape(clientID)} {
		if strings.Contains(text, secret) {
			t.Fatalf("access log carried %q: %s", secret, text)
		}
	}
}

// --- Client ID Metadata Documents (AC-2.1) ---

// metadataFixture serves metadata documents over TLS on 127.0.0.1 with the
// test certificate for example.com, and resolves names through a
// controlled table.
type metadataFixture struct {
	server    *httptest.Server
	port      string
	documents map[string]string
	requests  map[string]int
	mu        sync.Mutex
	dialed    []string
	hosts     map[string][]netip.Addr
}

func newMetadataFixture(t *testing.T) *metadataFixture {
	t.Helper()
	fixture := &metadataFixture{documents: map[string]string{}, requests: map[string]int{}, hosts: map[string][]netip.Addr{
		"example.com":           {netip.MustParseAddr("127.0.0.1")},
		"client.test":           {netip.MustParseAddr("127.0.0.1")},
		"internal.example.com":  {netip.MustParseAddr("10.0.0.5")},
		"docs6.example.com":     {netip.MustParseAddr("2001:db8::1")},
		"docs4.example.com":     {netip.MustParseAddr("203.0.113.9")},
		"teredo.example.com":    {netip.MustParseAddr("2001::5efe:a00:1")},
		"scoped.example.com":    {netip.MustParseAddr("2001:db8::1%eth0")},
		"scoped-lo.example.com": {netip.MustParseAddr("::1%lo")},
		"compat.example.com":    {netip.MustParseAddr("::a00:5")},
		"mapped.example.com":    {netip.MustParseAddr("::ffff:192.0.2.1")},
	}}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.requests[r.URL.Path]++
		body, ok := fixture.documents[r.URL.Path]
		fixture.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/redirect/"):
			http.Redirect(w, r, strings.Replace(strings.TrimPrefix(r.URL.Path, "/redirect/"), "PORT", fixture.port, 1), http.StatusFound)
		case r.URL.Path == "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		case r.URL.Path == "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case ok:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	_, fixture.port, _ = net.SplitHostPort(fixture.server.Listener.Addr().String())
	return fixture
}

func (f *metadataFixture) url(host, path string) string {
	return "https://" + host + ":" + f.port + path
}

func (f *metadataFixture) document(path, clientID, method string, redirects ...string) {
	body := map[string]any{"client_id": clientID, "client_name": "Metadata Agent", "redirect_uris": redirects, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
	if method != "" {
		body["token_endpoint_auth_method"] = method
	}
	encoded, _ := json.Marshal(body)
	f.mu.Lock()
	f.documents[path] = string(encoded)
	f.mu.Unlock()
}

// resolver returns a resolver that may dial only loopback (the fixture's
// own address) and records every dial.
func (f *metadataFixture) resolver(allowLoopback bool) *oauthMetadataResolver {
	roots := x509.NewCertPool()
	roots.AddCert(f.server.Certificate())
	resolver := &oauthMetadataResolver{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			if addresses, ok := f.hosts[host]; ok {
				return addresses, nil
			}
			return nil, errors.New("no such host")
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			f.mu.Lock()
			f.dialed = append(f.dialed, address)
			f.mu.Unlock()
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
		tlsConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	if allowLoopback {
		resolver.allowAddress = func(address netip.Addr) bool { return address.IsLoopback() || oauthPublicAddress(address) }
	}
	return resolver
}

func (f *metadataFixture) dials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

func TestOAuthMetadataAddressPolicyRefusesNonPublicRanges(t *testing.T) {
	for _, address := range []string{
		"10.1.2.3", "172.16.0.1", "192.168.1.1", "127.0.0.1", "127.8.8.8", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "0.1.2.3",
		"224.0.0.1", "239.1.1.1", "ff02::1", "ff05::2", "fc00::1", "fd12::1", "100.64.0.1", "198.18.0.1", "192.0.0.8", "255.255.255.255", "240.0.0.1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "192.88.99.1", "2001:db8::1", "2001::1", "100::1", "3fff::1", "fec0::1",
		// Zone-qualified addresses, even with a public address part.
		"2001:db8::1%eth0", "fe80::1%eth0", "2606:4700:4700::1111%eth0", "::1%lo",
		// IPv4-compatible IPv6 addresses, whatever IPv4 address they carry.
		"::a00:1", "::7f00:1", "::808:808", "::c000:201",
		// IPv4-mapped addresses whose IPv4 address is in a blocked range.
		"::ffff:192.0.2.1", "::ffff:100.64.0.1", "::ffff:198.18.0.1", "::ffff:203.0.113.9", "::ffff:240.0.0.1",
	} {
		if oauthPublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s was admitted", address)
		}
	}
	for _, address := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "::ffff:93.184.216.34"} {
		if !oauthPublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s was refused", address)
		}
	}
}

// TestOAuthMetadataCacheKeepsTheLaterObservation proves that when two
// overlapping fetches of one client ID publish out of order, the document
// from the later observation stays cached.
func TestOAuthMetadataCacheKeepsTheLaterObservation(t *testing.T) {
	resolver := &oauthMetadataResolver{}
	start := time.Unix(1000, 0)
	older := oauthMetadataDocument{ClientID: "https://app.example/client.json", RedirectURIs: []string{"https://app.example/removed"}}
	newer := oauthMetadataDocument{ClientID: "https://app.example/client.json", RedirectURIs: []string{"https://app.example/current"}}
	if kept := resolver.publish(newer.ClientID, newer, start.Add(time.Second)); !kept.allowsRedirect("https://app.example/current") {
		t.Fatalf("first publication = %+v", kept)
	}
	// The older observation finishes last and must not replace the newer.
	if kept := resolver.publish(older.ClientID, older, start); kept.allowsRedirect("https://app.example/removed") || !kept.allowsRedirect("https://app.example/current") {
		t.Fatalf("older observation replaced the newer: %+v", kept)
	}
	if cached := resolver.cache[newer.ClientID].document; cached.allowsRedirect("https://app.example/removed") {
		t.Fatalf("cache holds the older document: %+v", cached)
	}
	// A later observation does replace the cached document.
	if kept := resolver.publish(older.ClientID, older, start.Add(2*time.Second)); !kept.allowsRedirect("https://app.example/removed") {
		t.Fatalf("later observation was not cached: %+v", kept)
	}
}

func TestOAuthMetadataResolverFetchesOverCheckedAddresses(t *testing.T) {
	fixture := newMetadataFixture(t)
	clientID := fixture.url("example.com", "/client.json")
	fixture.document("/client.json", clientID, "none", "http://127.0.0.1:33418/callback", "https://app.example/cb")
	resolver := fixture.resolver(true)
	document, err := resolver.resolve(t.Context(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	if document.ClientID != clientID || document.ClientName != "Metadata Agent" || !document.allowsRedirect("https://app.example/cb") ||
		document.allowsRedirect("https://app.example/cb/") || document.allowsRedirect("https://app.example/CB") || document.allowsRedirect("http://127.0.0.1:33419/callback") {
		t.Fatalf("document = %+v", document)
	}
	// The dial went to the checked address, not to the hostname, and TLS
	// still verified example.com.
	if dials := fixture.dials(); len(dials) != 1 || dials[0] != "127.0.0.1:"+fixture.port {
		t.Fatalf("dials = %v", dials)
	}
	// A cached document is reused within its lifetime and fetched again
	// after it.
	now := time.Now()
	resolver.now = func() time.Time { return now }
	resolver.cache = nil
	for range 3 {
		if _, err := resolver.resolve(t.Context(), clientID); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(oauthMetadataDocumentCacheLifetime + time.Second)
	if _, err := resolver.resolve(t.Context(), clientID); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	requests := fixture.requests["/client.json"]
	fixture.mu.Unlock()
	if requests != 3 {
		t.Fatalf("document fetched %d times, want 3", requests)
	}
	// A redirect to another public HTTPS document is followed.
	redirected := fixture.url("example.com", "/redirect/https://example.com:PORT/final.json")
	fixture.document("/final.json", redirected, "none", "https://app.example/cb")
	if _, err := resolver.resolve(t.Context(), redirected); err != nil {
		t.Fatalf("redirected document: %v", err)
	}
}

func TestOAuthMetadataResolverRefusesUnsafeOrInvalidDocuments(t *testing.T) {
	fixture := newMetadataFixture(t)
	good := fixture.url("example.com", "/client.json")
	fixture.document("/client.json", good, "none", "https://app.example/cb")
	fixture.document("/mismatch.json", fixture.url("example.com", "/other.json"), "none", "https://app.example/cb")
	fixture.document("/secret.json", fixture.url("example.com", "/secret.json"), "client_secret_basic", "https://app.example/cb")
	fixture.document("/no-method.json", fixture.url("example.com", "/no-method.json"), "", "https://app.example/cb")
	fixture.document("/no-redirects.json", fixture.url("example.com", "/no-redirects.json"), "none")
	fixture.document("/bad-redirect.json", fixture.url("example.com", "/bad-redirect.json"), "none", "http://app.example/cb")
	fixture.mu.Lock()
	fixture.documents["/huge.json"] = `{"client_id":"` + fixture.url("example.com", "/huge.json") + `","pad":"` + strings.Repeat("x", oauthMetadataDocumentMaxBytes) + `"}`
	fixture.documents["/not-json.json"] = `<html>`
	fixture.documents["/implicit.json"] = `{"client_id":"` + fixture.url("example.com", "/implicit.json") + `","token_endpoint_auth_method":"none","redirect_uris":["https://app.example/cb"],"grant_types":["implicit"]}`
	fixture.mu.Unlock()
	for name, clientID := range map[string]string{
		"document names another client": fixture.url("example.com", "/mismatch.json"),
		"secret client":                 fixture.url("example.com", "/secret.json"),
		"no method":                     fixture.url("example.com", "/no-method.json"),
		"no redirect URIs":              fixture.url("example.com", "/no-redirects.json"),
		"insecure redirect URI":         fixture.url("example.com", "/bad-redirect.json"),
		"implicit grant":                fixture.url("example.com", "/implicit.json"),
		"oversized document":            fixture.url("example.com", "/huge.json"),
		"not JSON":                      fixture.url("example.com", "/not-json.json"),
		"missing document":              fixture.url("example.com", "/missing.json"),
		"redirect to http":              fixture.url("example.com", "/redirect/http://example.com:PORT/client.json"),
		"redirect loop":                 fixture.url("example.com", "/loop"),
		"unknown host":                  fixture.url("nowhere.example", "/client.json"),
	} {
		resolver := fixture.resolver(true)
		if _, err := resolver.resolve(t.Context(), clientID); !errors.Is(err, errOAuthClientUnverified) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	// A redirect to a private name stops before a second dial: only the
	// first hop's checked address is dialed.
	before := len(fixture.dials())
	if _, err := fixture.resolver(true).resolve(t.Context(), fixture.url("example.com", "/redirect/https://internal.example.com:PORT/client.json")); !errors.Is(err, errOAuthClientUnverified) {
		t.Errorf("redirect to private address: err=%v", err)
	}
	if dials := fixture.dials()[before:]; len(dials) != 1 || dials[0] != "127.0.0.1:"+fixture.port {
		t.Fatalf("redirect to a private address dialed %v", dials)
	}
	// TLS identity alone refuses this document: it names exactly the
	// requested client ID and is otherwise valid, but the certificate does
	// not name client.test. A control resolver that verifies the same
	// certificate as example.com accepts it, so the document is not why the
	// first fetch fails.
	tlsClientID := fixture.url("client.test", "/tls-identity.json")
	fixture.document("/tls-identity.json", tlsClientID, "none", "https://app.example/cb")
	if _, err := fixture.resolver(true).resolve(t.Context(), tlsClientID); !errors.Is(err, errOAuthClientUnverified) {
		t.Errorf("TLS identity mismatch: err=%v", err)
	}
	control := fixture.resolver(true)
	control.tlsConfig = control.tlsConfig.Clone()
	control.tlsConfig.ServerName = "example.com"
	if document, err := control.resolve(t.Context(), tlsClientID); err != nil || document.ClientID != tlsClientID {
		t.Fatalf("TLS control: document=%+v err=%v", document, err)
	}
	slow := fixture.resolver(true)
	slow.timeout = 100 * time.Millisecond
	fixture.document("/slow", fixture.url("example.com", "/slow"), "none", "https://app.example/cb")
	started := time.Now()
	if _, err := slow.resolve(t.Context(), fixture.url("example.com", "/slow")); !errors.Is(err, errOAuthClientUnverified) || time.Since(started) > time.Second {
		t.Fatalf("slow document: err=%v after %s", err, time.Since(started))
	}
	// With the production policy, a loopback or private address is never
	// dialed, by name or literal.
	before = len(fixture.dials())
	strict := fixture.resolver(false)
	for _, clientID := range []string{
		good, fixture.url("127.0.0.1", "/client.json"), fixture.url("[::1]", "/client.json"), fixture.url("internal.example.com", "/client.json"),
		fixture.url("docs6.example.com", "/client.json"), fixture.url("docs4.example.com", "/client.json"), fixture.url("teredo.example.com", "/client.json"),
		fixture.url("[2001:db8::1]", "/client.json"),
		// A scoped address in a blocked range, an IPv4-compatible address,
		// and an IPv4-mapped address in a blocked range, by name and literal.
		fixture.url("scoped.example.com", "/client.json"), fixture.url("compat.example.com", "/client.json"),
		fixture.url("mapped.example.com", "/client.json"), fixture.url("[::ffff:192.0.2.1]", "/client.json"),
		fixture.url("[::a00:5]", "/client.json"), fixture.url("[fe80::1%25eth0]", "/client.json"),
	} {
		if _, err := strict.resolve(t.Context(), clientID); !errors.Is(err, errOAuthClientUnverified) {
			t.Errorf("%s resolved under the production policy: %v", clientID, err)
		}
	}
	// A zone is refused before the address policy runs: even a policy that
	// admits every address never dials a scoped answer.
	permissive := fixture.resolver(false)
	permissive.allowAddress = func(netip.Addr) bool { return true }
	for _, clientID := range []string{fixture.url("scoped-lo.example.com", "/client.json"), fixture.url("scoped.example.com", "/client.json")} {
		if _, err := permissive.resolve(t.Context(), clientID); !errors.Is(err, errOAuthClientUnverified) {
			t.Errorf("%s resolved with a zone: %v", clientID, err)
		}
	}
	// One forbidden address among the answers refuses the whole name.
	fixture.hosts["mixed.example.com"] = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.9")}
	mixed := fixture.resolver(false)
	mixed.allowAddress = func(address netip.Addr) bool { return address.IsLoopback() }
	if _, err := mixed.resolve(t.Context(), fixture.url("mixed.example.com", "/client.json")); !errors.Is(err, errOAuthClientUnverified) {
		t.Errorf("mixed answers resolved: %v", err)
	}
	if dials := fixture.dials(); len(dials) != before {
		t.Fatalf("refused addresses were dialed: %v", dials[before:])
	}
	for _, clientID := range []string{
		"http://example.com/client.json", "https://example.com", "https://example.com/", "https://example.com/a/../client.json",
		"https://example.com/./client.json", "https://user@example.com/client.json", "https://example.com/client.json#x",
		"https://example.com/" + strings.Repeat("a", oauthClientIDMaxBytes), "https://example.com/client json",
	} {
		if err := validateOAuthMetadataClientID(clientID); err == nil {
			t.Errorf("client ID %q was accepted", clientID)
		}
	}
}

func TestOAuthMetadataDocumentClientCompletesTheFlow(t *testing.T) {
	h := newOAuthHarness(t)
	fixture := newMetadataFixture(t)
	clientID := fixture.url("example.com", "/client.json")
	redirect := "http://127.0.0.1:33418/callback"
	fixture.document("/client.json", clientID, "none", redirect)
	h.server.oauthResolver = fixture.resolver(true)
	consent := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, redirect, "cimd"), session: h.memberSession})
	if consent.Code != http.StatusOK {
		t.Fatalf("consent status=%d body=%s", consent.Code, consent.Body.String())
	}
	var view oauthConsentView
	if err := json.Unmarshal(consent.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Client.Kind != oauthClientKindMetadataDocument || view.Client.Name != "Metadata Agent" || view.Client.Origin != "https://example.com:"+fixture.port || view.RedirectHost != "127.0.0.1:33418" {
		t.Fatalf("consent view = %+v", view)
	}
	unlisted := h.do(oauthRequest{method: http.MethodGet, path: "/v1/oauth/authorize?" + oauthAuthorizeQuery(clientID, "http://127.0.0.1:33419/callback", "cimd"), session: h.memberSession})
	if body := expectOAuthError(t, unlisted, http.StatusBadRequest, "invalid_redirect_uri"); body["redirect_to"] != nil {
		t.Fatalf("unlisted redirect: %v", body)
	}
	target := h.approve(h.memberSession, clientID, redirect, "cimd")
	if target.Query().Get("state") != "cimd" {
		t.Fatalf("redirect_to = %s", target)
	}
	pair := decodeTokenPair(t, h.token(codeExchangeForm(target.Query().Get("code"), clientID, redirect)))
	if result := h.mcp(pair.AccessToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); result.status != http.StatusOK || result.isError {
		t.Fatalf("metadata client token on /mcp: %+v", result)
	}
	grants, err := h.st.ListOwnOAuthGrants(t.Context(), h.member.ID)
	if err != nil || len(grants) != 1 || grants[0].ClientID != clientID || grants[0].ClientName != "Metadata Agent" {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	decodeTokenPair(t, h.token(refreshForm(pair.RefreshToken, clientID)))
}
