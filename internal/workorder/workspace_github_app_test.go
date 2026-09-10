package workorder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type appTestTransport func(*http.Request) (*http.Response, error)

func (f appTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWorkspaceGitHubAppResolverUsesInstallationIdentity(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-token-must-not-be-used")
	t.Setenv("PATH", t.TempDir())
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{41}, 32))
	if _, err := st.StoreWorkspaceForgeToken(ctx, "demo", "legacy-token-must-not-be-used", "legacy"); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-test", ClientID: "client"}, PrivateKey: private}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, "demo", app); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, "demo", 41, 12, "org"); err != nil {
		t.Fatal(err)
	}
	var forgeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
		case "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "resolver-installation-secret", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
		case "/repos/org/repo/pulls/1/files":
			forgeCalls++
			if r.Header.Get("Authorization") != "Bearer resolver-installation-secret" {
				t.Error("forge act did not authenticate with minted token")
			}
			fmt.Fprint(w, `[{"filename":"README.md"}]`)
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := github.NewAppClient(server.Client(), server.URL)
	originalRunner := client.Runner
	client.Runner = func(token, identity string) github.AppRunner {
		if identity != "workspace demo GitHub App" {
			t.Error("wrong identity attribution")
		}
		return originalRunner(token, identity)
	}
	originalTransport := http.DefaultTransport
	http.DefaultTransport = appTestTransport(func(r *http.Request) (*http.Response, error) {
		cloned := r.Clone(r.Context())
		u := *r.URL
		if u.Host == "api.github.com" {
			u.Scheme = "http"
			u.Host = strings.TrimPrefix(server.URL, "http://")
		}
		cloned.URL = &u
		return originalTransport.RoundTrip(cloned)
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	s := &Service{Store: st, WorkspaceGitHubApps: st, GitHubApps: client}
	resolve := func(c context.Context, repo string) (context.Context, error) { return s.workspaceForgeContext(c, repo) }
	forgeCtx, e := resolve(ctx, "org/repo")
	if e != nil {
		t.Fatal(e)
	}
	_, err = github.PullRequestFiles(forgeCtx, "org/repo", 1)

	if err != nil {
		t.Fatal(err)
	}
	if forgeCalls != 1 {
		t.Fatal("forge act was not executed")
	}
	if _, err = resolve(context.Background(), "org/repo"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("implicit workspace accepted")
	}
	if _, err = resolve(ctx, "org/uncovered"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("uncovered repository accepted")
	}
	if err = st.DeleteWorkspaceGitHubApp(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err = resolve(ctx, "org/repo"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("deleted app fell back to cache or legacy token")
	}
	events, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resolver-installation-secret") || strings.Contains(string(raw), private) {
		t.Fatal("forge credential in event")
	}
}
