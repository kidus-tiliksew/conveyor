package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestTaskPolicyHTTPBoundary(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "policy-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"verify_stage":true,"reason":"operator exception","request_id":"policy-1"}`, http.StatusOK},
		{`{"verify_stage":true,"reason":"operator exception","request_id":"policy-1"}`, http.StatusOK},
		{`{"verify_stage":false,"reason":"operator exception","request_id":"policy-1"}`, http.StatusConflict},
		{`{"verify_stage":false,"reason":" ","request_id":"blank"}`, http.StatusBadRequest},
		{`{"verify_stage":false,"reason":"operator exception"}`, http.StatusBadRequest},
		{`{"verify_stage":false,"reason":"x","request_id":"h","harness":"codex"}`, http.StatusBadRequest},
		{`{"stage_timeouts":{"implement":"1h"},"reason":"x","request_id":"timeout"}`, http.StatusBadRequest},
		{`{"stage_timeouts":{"verify":"0s"},"reason":"x","request_id":"timeout"}`, http.StatusBadRequest},
		{`{"setup":"default","reason":"x","request_id":"setup"}`, http.StatusBadRequest},
		{`{"verify_stage":true,"reason":"x","request_id":"two"} {}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/policy-task/policy", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer user-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.body, response.Code, response.Body)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/policy-task/setup", strings.NewReader(`{"setup":"default"}`))
	req.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusGone {
		t.Fatalf("retired setup status: %d", response.Code)
	}
}

func TestVerifyPolicyRejectsLocalExecutionFields(t *testing.T) {
	for _, field := range []string{"harness", "model", "effort", "argv", "verify_concurrency"} {
		if forbiddenExecutionField(map[string]any{"execution": map[string]any{field: "local"}}) == "" {
			t.Fatalf("policy accepts %s", field)
		}
	}
}

// policyHTTPFixture serves the router over a memory store with the shared
// bearer token "user-token".
func policyHTTPFixture(t *testing.T) (*Server, store.Store, http.Handler) {
	t.Helper()
	st := store.NewMemory()
	cfg := &config.Config{
		Workspace: "demo",
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "user-token"
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	return server, st, server.Handler()
}
