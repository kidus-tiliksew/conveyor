package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestWorkspaceGitHubWebhookSettings: POST .../github-app/webhook-secret
// requires manage_workspace and a connected App, returns a new secret once
// with the receiver URL and the six events, and regenerates it on a second
// call; GET .../github-app reports the webhook object without the secret,
// and configured turns true with the first accepted delivery. No response,
// workspace event, or log line after creation carries the secret
// (component-identity-membership, The webhook secret; req-quiet-idle-runtime
// AC-3.2, AC-3.6).
func TestWorkspaceGitHubWebhookSettings(t *testing.T) {
	logs := captureLog(t)
	st := store.NewVolatileBackend()
	defer st.Close()
	cfg := &config.Config{Workspace: "alpha"}
	if _, err := st.BootstrapWorkspaceConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{41}, 32))
	memberships := &membershipFixture{workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}}, roles: map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}, "maintainer": {"alpha": core.WorkspaceRoleMaintainer}}}
	server := NewServer(st)
	server.Workspaces = memberships
	server.Memberships = memberships
	server.InvitationDelivery.PublicURL = "https://conveyor.example/"
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	server.Credentials = staticCredentialVerifier{"operator": {ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}, "maintainer": {ID: "pat2", OwnerUserID: "maintainer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	var bodies []string
	call := func(method, path, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://conveyor.example/v1/workspaces/alpha/github-app"+path, nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		bodies = append(bodies, w.Body.String())
		return w
	}
	if w := call("POST", "/webhook-secret", "operator"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "github_app_not_connected") {
		t.Fatalf("secret without an App: %d %s", w.Code, w.Body.String())
	}
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 7, AppSlug: "conveyor-alpha", ClientID: "client"}, PrivateKey: "settings-app-private-key"}
	if _, err := st.StoreWorkspaceGitHubApp(store.WithActor(t.Context(), store.SystemActor()), "alpha", app); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/webhook-secret", "maintainer"); w.Code != http.StatusNotFound {
		t.Fatalf("maintainer generated a secret: %d", w.Code)
	}
	type view struct {
		Webhook struct {
			ReceiverURL      string     `json:"receiver_url"`
			Events           []string   `json:"events"`
			SecretConfigured bool       `json:"secret_configured"`
			LastDeliveryAt   *time.Time `json:"last_delivery_at"`
			Configured       bool       `json:"configured"`
		} `json:"webhook"`
	}
	read := func() view {
		t.Helper()
		w := call("GET", "", "maintainer")
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		var got view
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := read()
	if before.Webhook.ReceiverURL != "https://conveyor.example/v1/github/webhook/alpha" || strings.Join(before.Webhook.Events, ",") != "pull_request,check_suite,check_run,status,push,issues" || before.Webhook.SecretConfigured || before.Webhook.Configured || before.Webhook.LastDeliveryAt != nil {
		t.Fatalf("webhook before a secret=%+v", before.Webhook)
	}
	created := call("POST", "/webhook-secret", "operator")
	if created.Code != http.StatusOK || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create secret: %d %s", created.Code, created.Body.String())
	}
	var first struct {
		Secret      string   `json:"secret"`
		ReceiverURL string   `json:"receiver_url"`
		Events      []string `json:"events"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Secret) != 64 || first.ReceiverURL != before.Webhook.ReceiverURL || len(first.Events) != 6 {
		t.Fatalf("created secret response=%+v", first)
	}
	stored, err := st.GetWorkspaceGitHubWebhookSecretForUse(t.Context(), "alpha")
	if err != nil || stored != first.Secret {
		t.Fatalf("stored secret differs: err=%v", err)
	}
	afterSecret := read()
	if !afterSecret.Webhook.SecretConfigured || afterSecret.Webhook.Configured {
		t.Fatalf("webhook after a secret=%+v", afterSecret.Webhook)
	}
	if _, err := st.RecordGitHubWebhookDelivery(t.Context(), "alpha", "first-delivery", "ping", time.Now()); err != nil {
		t.Fatal(err)
	}
	if delivered := read(); !delivered.Webhook.Configured || delivered.Webhook.LastDeliveryAt == nil {
		t.Fatalf("webhook after a delivery=%+v", delivered.Webhook)
	}
	regenerated := call("POST", "/webhook-secret", "operator")
	var second struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(regenerated.Body.Bytes(), &second); err != nil || second.Secret == "" || second.Secret == first.Secret {
		t.Fatalf("regenerated secret: err=%v", err)
	}
	// Only the two creation responses carry a secret.
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "alpha"), "")
	if err != nil {
		t.Fatal(err)
	}
	var later []string
	for _, body := range bodies {
		if body != created.Body.String() && body != regenerated.Body.String() {
			later = append(later, body)
		}
	}
	for _, event := range events {
		later = append(later, string(event.Payload))
	}
	later = append(later, logs.String())
	for _, secret := range []string{first.Secret, second.Secret} {
		if strings.Contains(strings.Join(later, "\n"), secret) {
			t.Fatal("webhook secret emitted after creation")
		}
	}
}
