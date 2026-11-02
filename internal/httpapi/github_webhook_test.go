package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const webhookTestSecret = "receiver-webhook-secret-material-0123456789"

type fakeWebhookApplier struct {
	mu         sync.Mutex
	deliveries []github.WebhookDelivery
}

func (f *fakeWebhookApplier) Submit(delivery github.WebhookDelivery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, delivery)
}

func (f *fakeWebhookApplier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deliveries)
}

// captureLog sends the standard logger, which the router's access log and
// the receiver write to, into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	writer := lockedWriter{mu: &mu, buf: &buf}
	previous := log.Writer()
	log.SetOutput(writer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func newWebhookReceiverFixture(t *testing.T) (*Server, store.Backend, *fakeWebhookApplier) {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, workspace := range []string{"demo", "plain"} {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{41}, 32))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 7, AppSlug: "conveyor-demo", ClientID: "client"}, PrivateKey: "receiver-app-private-key", WebhookSecret: webhookTestSecret}
	if _, err := st.StoreWorkspaceGitHubApp(ctx, "demo", app); err != nil {
		t.Fatal(err)
	}
	// "plain" has an App and no webhook secret.
	app.WebhookSecret = ""
	if _, err := st.StoreWorkspaceGitHubApp(ctx, "plain", app); err != nil {
		t.Fatal(err)
	}
	applier := &fakeWebhookApplier{}
	server := NewServer(st)
	server.GitHubWebhookApplier = applier
	return server, st, applier
}

func signWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(server *Server, workspace, signature, delivery, event string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook/"+workspace, bytes.NewReader(body))
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)
	return w
}

// TestGitHubWebhookRefusesUnsignedDeliveriesBeforeParsing: a missing,
// malformed, wrong-algorithm, or wrong signature, an unknown workspace, and
// a workspace without a secret each answer 401 for a body that is not even
// JSON, so the receiver refuses before parsing, records no delivery, and
// submits nothing (req-quiet-idle-runtime AC-3.2; component-http-api,
// GitHub webhook receiver).
func TestGitHubWebhookRefusesUnsignedDeliveriesBeforeParsing(t *testing.T) {
	server, st, applier := newWebhookReceiverFixture(t)
	body := []byte("{not json")
	wrong := signWebhook("another-secret", body)
	valid := signWebhook(webhookTestSecret, body)
	cases := []struct{ name, workspace, signature string }{
		{"missing signature", "demo", ""},
		{"malformed hex", "demo", "sha256=zz"},
		{"truncated digest", "demo", valid[:len(valid)-2]},
		{"sha1 signature", "demo", "sha1=" + strings.TrimPrefix(valid, "sha256=")},
		{"wrong secret", "demo", wrong},
		{"unknown workspace", "absent", valid},
		{"workspace without a secret", "plain", signWebhook("", body)},
	}
	for _, tc := range cases {
		w := postWebhook(server, tc.workspace, tc.signature, "delivery-"+tc.name, "pull_request", body)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_signature") {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if applier.count() != 0 {
		t.Fatalf("refused deliveries submitted %d", applier.count())
	}
	for _, workspace := range []string{"demo", "plain"} {
		status, err := st.GitHubWebhookStatus(t.Context(), workspace)
		if err != nil || status.HasWebhookDelivery() {
			t.Fatalf("%s recorded a refused delivery: %+v err=%v", workspace, status, err)
		}
	}
}

// TestGitHubWebhookAcceptsOnceAndRefusesDuplicates: a signed delivery
// answers 202 and reaches the applier once with its workspace, ID, event,
// and raw body; the same delivery ID again answers 200 and applies nothing
// (req-quiet-idle-runtime AC-3.3). Missing delivery headers answer 400, an
// oversized body 413, and a receiver without an applier 503, each
// recording nothing.
func TestGitHubWebhookAcceptsOnceAndRefusesDuplicates(t *testing.T) {
	server, st, applier := newWebhookReceiverFixture(t)
	body := []byte(`{"action":"closed","pull_request":{"number":1}}`)
	signature := signWebhook(webhookTestSecret, body)
	if w := postWebhook(server, "demo", signature, "", "pull_request", body); w.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery ID: %d", w.Code)
	}
	if w := postWebhook(server, "demo", signature, "delivery-1", "", body); w.Code != http.StatusBadRequest {
		t.Fatalf("missing event: %d", w.Code)
	}
	if status, _ := st.GitHubWebhookStatus(t.Context(), "demo"); status.HasWebhookDelivery() {
		t.Fatal("a delivery without headers was recorded")
	}
	w := postWebhook(server, "demo", signature, "delivery-1", "pull_request", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("signed delivery: %d %s", w.Code, w.Body.String())
	}
	if applier.count() != 1 {
		t.Fatalf("submitted %d", applier.count())
	}
	got := applier.deliveries[0]
	if got.Workspace != "demo" || got.ID != "delivery-1" || got.Event != "pull_request" || !bytes.Equal(got.Body, body) || got.ReceivedAt.IsZero() {
		t.Fatalf("delivery=%+v", got)
	}
	status, err := st.GitHubWebhookStatus(t.Context(), "demo")
	if err != nil || !status.HasWebhookDelivery() {
		t.Fatalf("accepted delivery not recorded: %+v err=%v", status, err)
	}
	if w = postWebhook(server, "demo", signature, "delivery-1", "pull_request", body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	if applier.count() != 1 {
		t.Fatalf("duplicate submitted: %d", applier.count())
	}
	large := bytes.Repeat([]byte("x"), maxGitHubWebhookBody+1)
	if w = postWebhook(server, "demo", signWebhook(webhookTestSecret, large), "delivery-large", "push", large); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", w.Code)
	}
	server.GitHubWebhookApplier = nil
	if w = postWebhook(server, "demo", signature, "delivery-2", "pull_request", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no applier: %d", w.Code)
	}
	if applier.count() != 1 {
		t.Fatalf("submitted %d", applier.count())
	}
}

// TestGitHubWebhookNeverEmitsSecret: across refused, accepted, and
// duplicate deliveries, no log line, response, or workspace event carries
// the webhook secret (component-identity-membership, The webhook secret).
func TestGitHubWebhookNeverEmitsSecret(t *testing.T) {
	logs := captureLog(t)
	server, st, _ := newWebhookReceiverFixture(t)
	body := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/app"}}`)
	var responses []string
	for _, signature := range []string{"", signWebhook("wrong", body), signWebhook(webhookTestSecret, body), signWebhook(webhookTestSecret, body)} {
		w := postWebhook(server, "demo", signature, "delivery-secret", "push", body)
		responses = append(responses, w.Body.String()+fmtHeader(w.Header()))
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		responses = append(responses, string(event.Payload))
	}
	output := logs.String() + strings.Join(responses, "\n")
	if !strings.Contains(logs.String(), "delivery=delivery-secret") {
		t.Fatalf("receiver log missing: %s", logs.String())
	}
	if strings.Contains(output, webhookTestSecret) || strings.Contains(output, hex.EncodeToString([]byte(webhookTestSecret))) {
		t.Fatal("webhook secret emitted")
	}
}

func fmtHeader(h http.Header) string {
	var b strings.Builder
	for key, values := range h {
		b.WriteString(key + ":" + strings.Join(values, ",") + "\n")
	}
	return b.String()
}
