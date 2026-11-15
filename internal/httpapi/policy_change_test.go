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
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
		{`{"review_distinct_models":true,"reason":"panel repeats a model","request_id":"distinct-1"}`, http.StatusOK},
		{`{"review_distinct_models":true,"reason":"panel repeats a model","request_id":"distinct-1"}`, http.StatusOK},
		{`{"review_distinct_models":false,"reason":"panel repeats a model","request_id":"distinct-1"}`, http.StatusConflict},
		{`{"review_distinct_models":"on","reason":"x","request_id":"distinct-2"}`, http.StatusBadRequest},
		{`{"review_distinct_models":false,"reason":" ","request_id":"distinct-3"}`, http.StatusBadRequest},
		{`{"reason":"no policy field","request_id":"distinct-4"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/policy-task/policy", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer user-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.body, response.Code, response.Body)
		}
	}
	stored, err := st.GetTask(ctx, "policy-task")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.SetupContract.Review.DistinctModels {
		t.Fatalf("frozen policy after the change: %+v", stored.SetupContract.Review)
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

// feature-delta-review-rounds DR-6; req-review-gates-evidence AC-12.8: the
// route admits later_round_review of full or delta under a user credential,
// a nonblank reason, and a request ID. It accepts the change while an order
// is claimed and on an approved task, refuses any other value and any
// refresh_review field, and keeps strict decoding.
func TestTaskPolicyLaterRoundReviewHTTP(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	contract := config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, StageTimeouts: config.PolicyStageTimeouts{Implement: "1h"}, RefreshReview: config.RefreshReviewFull}
	if err := st.CreateTask(ctx, core.Task{ID: "later-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: contract, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, core.Task{ID: "approved-task", Workspace: "demo", Repo: "conveyor", State: core.TaskApproved, SetupContract: contract, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateJob(ctx, core.Job{ID: "later-task-implement-1", TaskID: "later-task", Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: "later-task-implement-1", TaskID: "later-task", JobID: "later-task-implement-1", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if _, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, order)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).ClaimWorkOrder(ctx, order.TaskID, order.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "session", ClientToken: "session-token", Agent: "test", Model: "m", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	post := func(task, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task+"/policy", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for _, tc := range []struct {
		task, body, token string
		status            int
	}{
		{"later-task", `{"later_round_review":"delta","reason":"review only new changes","request_id":"later-1"}`, "user-token", http.StatusOK},
		{"later-task", `{"later_round_review":"delta","reason":"review only new changes","request_id":"later-1"}`, "user-token", http.StatusOK},
		{"later-task", `{"later_round_review":"full","reason":"review only new changes","request_id":"later-1"}`, "user-token", http.StatusConflict},
		{"later-task", `{"later_round_review":"none","reason":"x","request_id":"later-2"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"","reason":"x","request_id":"later-3"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":true,"reason":"x","request_id":"later-4"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","reason":" ","request_id":"later-5"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","reason":"x"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","refresh_review":"full","reason":"x","request_id":"later-6"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"refresh_review":"full","reason":"x","request_id":"later-7"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","reason":"x","request_id":"later-8"} {}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","plan_state":"needed","reason":"x","request_id":"later-9"}`, "user-token", http.StatusBadRequest},
		{"later-task", `{"later_round_review":"full","review_distinct_models":true,"reason":"x","request_id":"later-10"}`, "user-token", http.StatusConflict},
		{"later-task", `{"later_round_review":"full","reason":"x","request_id":"later-11"}`, "", http.StatusUnauthorized},
		{"later-task", `{"later_round_review":"full","reason":"x","request_id":"later-12"}`, "other-token", http.StatusUnauthorized},
		{"approved-task", `{"later_round_review":"delta","reason":"approved task","request_id":"later-13"}`, "user-token", http.StatusOK},
	} {
		if response := post(tc.task, tc.body, tc.token); response.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.task, tc.body, response.Code, response.Body)
		}
	}
	for _, id := range []string{"later-task", "approved-task"} {
		stored, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SetupContract.LaterRoundReview != config.LaterRoundReviewDelta || stored.SetupContract.RefreshReview != config.RefreshReviewFull || stored.SetupContract.Review.DistinctModels {
			t.Fatalf("%s frozen policy after the change: %+v", id, stored.SetupContract)
		}
	}
	claimed, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != core.WorkOrderClaimed || claimed.SessionID != "session" {
		t.Fatalf("claimed order after the change: state=%s session=%s", claimed.State, claimed.SessionID)
	}
}
