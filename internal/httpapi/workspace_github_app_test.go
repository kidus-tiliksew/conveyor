package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/go-chi/chi/v5/middleware"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func TestAppManifestStateAtomicSessionBindingAndExpiry(t *testing.T) {
	now := time.Now()
	states := manifestStates{now: func() time.Time { return now }}
	value, err := states.issue("alpha", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if states.consume("beta", "session-a", value) || states.consume("alpha", "session-b", value) {
		t.Fatal("foreign session/workspace consumed state")
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if states.consume("alpha", "session-a", value) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("state was not atomic single use")
	}
	value, err = states.issue("alpha", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if states.consume("alpha", "session-a", value) {
		t.Fatal("expired state accepted")
	}
}
func TestWorkspaceGitHubAppHTTPFlow(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	cfg := &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "covered", GitHub: "org/repo"}, {Name: "missing", GitHub: "org/missing"}}}
	if _, err := st.BootstrapWorkspaceConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{41}, 32))
	memberships := &membershipFixture{workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}}, roles: map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}, "maintainer": {"alpha": core.WorkspaceRoleMaintainer}}}
	server := NewServer(st)
	server.Workspaces = memberships
	server.Memberships = memberships
	server.InvitationDelivery.PublicURL = "https://conveyor.example"
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	sessions := &invitationSessionFixture{credential: core.AuthenticatedCredential{ID: "session-1", OwnerUserID: "operator", Kind: core.CredentialUser, Method: core.CredentialMethodSession, Scope: core.CredentialScopeOperator}}
	server.InvitationSessions = sessions
	server.Credentials = staticCredentialVerifier{"operator": {ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}, "maintainer": {ID: "pat2", OwnerUserID: "maintainer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}, "agent": {ID: "agent", OwnerUserID: "operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var conversions int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/code/conversions":
			conversions++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 41, "slug": "conveyor-alpha", "client_id": "client", "pem": private})
		case "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
		case "/app/installations/99":
			fmt.Fprint(w, `{"id":99,"app_id":999,"account":{"login":"stranger"}}`)
		case "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "http-installation-private-token", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
		default:
			w.WriteHeader(401)
		}
	}))
	defer provider.Close()
	server.GitHubApps = github.NewAppClient(provider.Client(), provider.URL)
	call := func(method, path, cookie, bearer string, proof bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://conveyor.example/v1/workspaces/alpha/github-app"+path, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: cookie})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if proof {
			r.Header.Set("Origin", "https://conveyor.example")
			r.Header.Set("X-Conveyor-CSRF", "1")
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), private) || strings.Contains(w.Body.String(), "http-installation-private-token") {
			t.Fatal("HTTP response contains app secret")
		}
		return w
	}
	for _, route := range []struct{ method, path string }{{"POST", "/manifest"}, {"GET", "/callback?code=code&state=bad"}, {"GET", "/setup?installation_id=12&setup_action=install"}, {"GET", ""}, {"DELETE", ""}} {
		if got := call(route.method, route.path, "", "agent", false); got.Code != 401 {
			t.Fatalf("agent %s %s: %d", route.method, route.path, got.Code)
		}
		if got := call(route.method, route.path, "", "", false); got.Code != 401 {
			t.Fatalf("anonymous %s: %d", route.path, got.Code)
		}
		got := call(route.method, route.path, "", "maintainer", false)
		if route.method == "GET" && route.path == "" {
			if got.Code != 200 {
				t.Fatalf("viewer status: %d", got.Code)
			}
		} else if got.Code != 404 {
			t.Fatalf("maintainer mutation %s: %d", route.path, got.Code)
		}
	}
	if got := call("POST", "/manifest", "session-a", "", false); got.Code != 403 {
		t.Fatal("manifest bypasses CSRF")
	}
	if got := call("POST", "/manifest", "", "operator", false); got.Code != 401 {
		t.Fatal("manifest not session bound")
	}
	for _, publicURL := range []string{"", "conveyor.example", "ftp://conveyor.example", "https://conveyor.example?query=1", "https://conveyor.example/#fragment", "https://%"} {
		t.Run("invalid public URL "+publicURL, func(t *testing.T) {
			server.InvitationDelivery.PublicURL = publicURL
			before := len(server.appStates.values)
			// Exercise URL validation directly: malformed configured origins can
			// otherwise be refused by CSRF middleware before this handler runs.
			r := httptest.NewRequest("POST", "/v1/workspaces/alpha/github-app/manifest", nil)
			r = r.WithContext(store.WithCredential(store.WithWorkspace(r.Context(), "alpha"), sessions.credential))
			r.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-a"})
			w := httptest.NewRecorder()
			server.createWorkspaceGitHubAppManifest(w, r)
			if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "public_url_required") {
				t.Fatalf("invalid public URL: %d %s", w.Code, w.Body.String())
			}
			if len(server.appStates.values) != before || len(w.Result().Cookies()) != 0 {
				t.Fatal("invalid public URL issued state or return cookie")
			}
		})
	}
	server.InvitationDelivery.PublicURL = ""
	if w := call("POST", "/manifest", "session-a", "", true); w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 {
		t.Fatal("missing public URL must refuse through the HTTP router without a return cookie")
	}
	server.InvitationDelivery.PublicURL = "https://conveyor.example"
	manifest := func() string {
		t.Helper()
		w := call("POST", "/manifest", "session-a", "", true)
		if w.Code != 200 {
			t.Fatalf("manifest %d %s", w.Code, w.Body.String())
		}
		var scoped *http.Cookie
		for _, cookie := range w.Result().Cookies() {
			if cookie.Path == "/v1/workspaces/alpha/github-app" {
				scoped = cookie
			}
		}
		if scoped == nil || !scoped.HttpOnly || !scoped.Secure || scoped.SameSite != http.SameSiteLaxMode || scoped.MaxAge != 600 {
			t.Fatal("GitHub return cookie contract")
		}
		var result struct {
			State    string         `json:"state"`
			Manifest map[string]any `json:"manifest"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Manifest["name"] != "Conveyor Alpha" || result.Manifest["public"] != true || result.Manifest["setup_on_update"] != true || result.Manifest["redirect_url"] != "https://conveyor.example/v1/workspaces/alpha/github-app/callback" {
			t.Fatal("manifest contract")
		}
		// GitHub requires hook_attributes.url even with active=false.
		// Assert the serialized HTTP response, not the construction map.
		hook, ok := result.Manifest["hook_attributes"].(map[string]any)
		if !ok || hook["active"] != false || hook["url"] != "https://conveyor.example" {
			t.Fatal("GitHub requires a URL for the disabled webhook")
		}
		if result.Manifest["url"] != "https://conveyor.example" || result.Manifest["setup_url"] != "https://conveyor.example/v1/workspaces/alpha/github-app/setup" {
			t.Fatal("manifest homepage/setup URLs")
		}
		if result.State == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("manifest state/cache contract")
		}
		return result.State
	}
	state := manifest()
	callback := "/callback?code=code&state=" + url.QueryEscape(state)
	if got := call("GET", callback, "session-b", "", false); got.Code != 400 {
		t.Fatal("cross-session callback")
	}
	got := call("GET", callback, "session-a", "", false)
	if got.Code != 302 || !strings.HasPrefix(got.Header().Get("Location"), "https://github.com/apps/conveyor-alpha/installations/new?state=") {
		t.Fatalf("callback %d %s", got.Code, got.Body.String())
	}
	installationURL, err := url.Parse(got.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	setupState := installationURL.Query().Get("state")
	if got := call("GET", callback, "session-a", "", false); got.Code != 400 || conversions != 1 {
		t.Fatal("callback replay")
	}
	if got := call("GET", "/setup?installation_id=99&setup_action=install&state="+setupState, "session-a", "", false); got.Code != 422 {
		t.Fatal("foreign app installation accepted")
	}
	status, err := st.GetWorkspaceGitHubAppStatus(t.Context(), "alpha")
	if err != nil || status.InstallationID != 0 {
		t.Fatal("failed setup stored installation")
	}
	// A failed installation consumes state; the settings status offers a fresh URL.
	statusResponse := call("GET", "", "session-a", "", false)
	var resume struct {
		URL string `json:"installation_url"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &resume); err != nil {
		t.Fatal(err)
	}
	resumeURL, err := url.Parse(resume.URL)
	if err != nil {
		t.Fatal(err)
	}
	setupState = resumeURL.Query().Get("state")
	got = call("GET", "/setup?installation_id=12&setup_action=install&state="+setupState, "session-a", "", false)
	if got.Code != 302 || got.Header().Get("Location") != "https://conveyor.example/settings?workspace=alpha" {
		t.Fatalf("setup %d %s", got.Code, got.Body.String())
	}
	got = call("GET", "", "", "maintainer", false)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"covered":true`) || !strings.Contains(got.Body.String(), `"covered":false`) {
		t.Fatalf("coverage %d %s", got.Code, got.Body.String())
	}
	state = manifest()
	got = call("GET", "/callback?code=bad-code&state="+state, "session-a", "", false)
	if got.Code != 422 {
		t.Fatal("conversion failure code")
	}
	after, err := st.GetWorkspaceGitHubAppStatus(t.Context(), "alpha")
	if err != nil || after.InstallationID != 12 {
		t.Fatal("failed conversion changed app")
	}
	if got = call("DELETE", "", "session-a", "", true); got.Code != 204 {
		t.Fatalf("delete: %d", got.Code)
	}
	if got = call("GET", "/setup?installation_id=12&setup_action=install", "session-a", "", false); got.Code != 404 {
		t.Fatal("setup unknown app accepted")
	}
}

func TestAppRequestLoggerOmitsHandshakeQueries(t *testing.T) {
	var logs bytes.Buffer
	original := middleware.DefaultLogger
	middleware.DefaultLogger = middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: log.New(&logs, "", 0), NoColor: true})
	t.Cleanup(func() { middleware.DefaultLogger = original })
	handler := githubAppRequestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("code") != "one-use-conversion-code" || r.URL.Query().Get("state") != "session-bound-state" {
			t.Error("handler lost original handshake")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest("GET", "/v1/workspaces/demo/github-app/callback?code=one-use-conversion-code&state=session-bound-state", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if strings.Contains(logs.String(), "one-use-conversion-code") || strings.Contains(logs.String(), "session-bound-state") {
		t.Fatal("access log persisted handshake credentials")
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("handshake response can leak by caching or referrer")
	}
}
