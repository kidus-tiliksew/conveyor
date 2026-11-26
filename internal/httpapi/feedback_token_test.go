package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// tokenApps serves one connected GitHub App credential for workspace alpha.
type tokenApps struct {
	store.WorkspaceGitHubAppStore
	app core.WorkspaceGitHubAppCredential
}

func (a tokenApps) GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error) {
	return a.app, nil
}

// tokenProvider is a stubbed GitHub App endpoint whose token answer the test
// sets: "ok", "limited" (rate limited until reset), or "failed" (502).
type tokenProvider struct {
	mu       sync.Mutex
	mode     string
	reset    time.Time
	requests int
	clock    func() time.Time
}

func (p *tokenProvider) set(mode string, reset time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.reset = mode, reset
}

func (p *tokenProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func (p *tokenProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/app/installations/12":
		fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"acme"}}`)
	case "/app/installations/12/access_tokens":
		p.mu.Lock()
		p.requests++
		mode, reset := p.mode, p.reset
		p.mu.Unlock()
		switch mode {
		case "limited":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		case "failed":
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message":"bad gateway"}`)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "feedback-installation-token", "expires_at": p.clock().Add(time.Hour - time.Second)})
		}
	default:
		w.WriteHeader(http.StatusUnauthorized)
	}
}

// tokenHarness is the reads harness with the real App client in front of a
// stubbed endpoint, so the forge must get an installation token.
type tokenHarness struct {
	*readsHarness
	provider *tokenProvider
	tokens   []string
	mu       sync.Mutex
}

func newTokenHarness(t *testing.T, history ...string) *tokenHarness {
	t.Helper()
	h := &tokenHarness{readsHarness: newReadsHarness(t, history...)}
	h.provider = &tokenProvider{mode: "ok", clock: h.clock}
	endpoint := httptest.NewServer(h.provider)
	t.Cleanup(endpoint.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	client := github.NewAppClient(endpoint.Client(), endpoint.URL)
	client.Now = h.clock
	h.server.GitHubApps = client
	h.server.WorkspaceGitHubApps = tokenApps{app: core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: "alpha", AppID: 41, InstallationID: 12, ConnectedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, PrivateKey: private}}
	h.server.feedbackForge = nil
	h.server.feedbackForgeFor = func(token, identity string) feedback.Forge {
		h.mu.Lock()
		h.tokens = append(h.tokens, token)
		h.mu.Unlock()
		return h.forge
	}
	return h
}

func (h *tokenHarness) resolution(commit string) store.FeedbackResolution {
	h.t.Helper()
	inputs, err := h.st.ReadFeedbackInputs(h.ctx, []string{h.environment.ID})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, resolution := range inputs.Resolutions {
		if resolution.Commit == commit[:12] {
			return resolution
		}
	}
	h.t.Fatalf("no resolution for %s in %+v", commit, inputs.Resolutions)
	return store.FeedbackResolution{}
}

func (h *tokenHarness) send(commit string) {
	h.t.Helper()
	h.accept(feedbackSignalBody("api", commit))
	h.settle()
}

// TestFeedbackTokenFailureIsRecordedAsTheAttempt covers
// component-production-feedback invariants 7 to 9 and
// req-production-feedback AC-1.4: the forge gets the installation token only
// after a version's claim. When the cached token has expired and the token
// request is rate limited, the version records the reason, its attempt
// time, and GitHub's later retry time. A signal before that time asks
// nothing, and a signal after it resolves the version.
func TestFeedbackTokenFailureIsRecordedAsTheAttempt(t *testing.T) {
	first, second := readsCommit("a1a1a1a1a1a1"), readsCommit("b2b2b2b2b2b2")
	h := newTokenHarness(t, first, second)
	h.send(first)
	if got := h.resolution(first); !got.Resolved() || h.provider.count() != 1 {
		t.Fatalf("first resolution=%+v token requests=%d", got, h.provider.count())
	}
	// The cached token is used until five minutes before it expires.
	h.advance(56 * time.Minute)
	reset := h.clock().Add(30 * time.Minute).Truncate(time.Second)
	h.provider.set("limited", reset)
	h.send(second)
	got := h.resolution(second)
	if got.Resolved() || got.Reason != string(github.ForgeRateLimited) || !got.NextAttemptAt.Equal(reset) || got.AttemptedAt.IsZero() || h.provider.count() != 2 {
		t.Fatalf("resolution after the rate-limited token request=%+v token requests=%d", got, h.provider.count())
	}
	attempted := got.AttemptedAt
	// Before GitHub's retry time a later signal asks nothing.
	h.advance(11 * time.Minute)
	h.send(second)
	if again := h.resolution(second); again.Resolved() || !again.AttemptedAt.Equal(attempted) || h.provider.count() != 2 {
		t.Fatalf("resolution before the retry time=%+v token requests=%d", again, h.provider.count())
	}
	// After it, the version is tried again and resolves.
	h.provider.set("ok", time.Time{})
	h.advance(20 * time.Minute)
	h.send(second)
	if resolved := h.resolution(second); !resolved.Resolved() || resolved.ResolvedCommit != second || h.provider.count() != 3 {
		t.Fatalf("resolution after the retry time=%+v token requests=%d", resolved, h.provider.count())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, token := range h.tokens {
		if token != "feedback-installation-token" {
			t.Fatalf("forge token=%q", token)
		}
	}
}

// TestFeedbackFailedTokenRequestWaitsTenMinutes covers a token request that
// fails without a retry time: the version keeps the failure's category and
// waits the 10-minute rule.
func TestFeedbackFailedTokenRequestWaitsTenMinutes(t *testing.T) {
	commit := readsCommit("c3c3c3c3c3c3")
	h := newTokenHarness(t, commit)
	h.provider.set("failed", time.Time{})
	h.send(commit)
	got := h.resolution(commit)
	if got.Resolved() || got.Reason != string(github.ForgeStatus) || !got.NextAttemptAt.Equal(got.AttemptedAt.Add(store.FeedbackResolutionRetry)) || h.provider.count() != 1 {
		t.Fatalf("resolution after the failed token request=%+v token requests=%d", got, h.provider.count())
	}
	h.provider.set("ok", time.Time{})
	h.advance(5 * time.Minute)
	h.send(commit)
	if again := h.resolution(commit); again.Resolved() || h.provider.count() != 1 {
		t.Fatalf("resolution before the retry time=%+v token requests=%d", again, h.provider.count())
	}
	h.advance(6 * time.Minute)
	h.send(commit)
	if resolved := h.resolution(commit); !resolved.Resolved() || h.provider.count() != 2 {
		t.Fatalf("resolution after the retry time=%+v token requests=%d", resolved, h.provider.count())
	}
}

// TestFeedbackTokenBackoffKeepsTheLaterDeadline proves that a failure
// recorded after a rate-limited one never shortens GitHub's retry time.
func TestFeedbackTokenBackoffKeepsTheLaterDeadline(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	backoff := &feedbackTokenBackoff{until: map[string]feedbackTokenFailure{}}
	limited := github.RateLimitedError(now.Add(30*time.Minute), fmt.Errorf("rate limited"))
	backoff.record("alpha", limited, now)
	backoff.record("alpha", github.StatusError(http.StatusBadGateway, fmt.Errorf("bad gateway")), now)
	if err := backoff.failure("alpha", now.Add(29*time.Minute)); err != limited {
		t.Fatalf("failure before the retry time=%v, want the rate-limited failure", err)
	}
	if err := backoff.failure("alpha", now.Add(30*time.Minute)); err != nil {
		t.Fatalf("failure at the retry time=%v", err)
	}
	if err := backoff.failure("beta", now); err != nil {
		t.Fatalf("another workspace=%v", err)
	}
}
