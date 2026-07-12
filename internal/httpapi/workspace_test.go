package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestWorkspaceSnapshotIsServedWithoutCredentialRefs(t *testing.T) {
	cfg := &config.Config{
		Workspace:  "demo",
		Image:      "conveyor-base:dev",
		MaxBounces: 2,
		Database:   config.Database{Backend: "postgres"},
		Credentials: []config.Credential{{
			ID: "local-codex", OwnerID: "op", OwnerKind: "user",
			Kind: "personal_sub", Vendor: "openai", Harness: "codex",
			Ref: "/home/op/.codex",
		}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Harnesses: []string{"codex"}, ModelTier: "subscription", BudgetUSD: 3, Timeout: 2 * time.Hour},
			"triage":    {Harnesses: []string{"claude-code", "codex"}, ModelTier: "strong", BudgetUSD: 0.75, Timeout: 20 * time.Minute},
		}},
		Repos: []config.Repo{{
			Name: "conveyor", URL: "https://github.com/kidus-tiliksew/conveyor",
			GitHub: "kidus-tiliksew/conveyor", Base: "main", Image: "conveyor-dev:dev",
			SecretRefs: []string{"secretref://demo/default/DATABASE_URL"},
		}},
	}
	s := NewServer(store.NewMemory())
	s.WorkspaceInfo = NewWorkspaceInfo(cfg)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	var got WorkspaceInfo
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Workspace != "demo" || len(got.Repos) != 1 || len(got.Credentials) != 1 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
	if got.Repos[0].SecretRefCount != 1 {
		t.Fatalf("secret_ref_count = %d, want 1", got.Repos[0].SecretRefCount)
	}
	// Stage order is stable (pipeline order), not map order.
	if got.Routing[0].Stage != "triage" || got.Routing[1].Stage != "implement" {
		t.Fatalf("routing order = %v", got.Routing)
	}
	if got.Routing[0].Timeout != "20m0s" {
		t.Fatalf("timeout = %q", got.Routing[0].Timeout)
	}
	// Refs name host paths and env vars; they must never reach the wire.
	for _, leak := range []string{".codex", "secretref://", "DATABASE_URL"} {
		if strings.Contains(res.Body.String(), leak) {
			t.Fatalf("response leaks %q: %s", leak, res.Body.String())
		}
	}
}

func TestWorkspaceReturns404WithoutConfig(t *testing.T) {
	s := NewServer(store.NewMemory())
	h := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}
