package httpapi

import (
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
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func taskRunHTTPFixture(t *testing.T) (*Server, store.Store, http.Handler) {
	t.Helper()
	st := store.NewMemory()
	cfg := &config.Config{
		Workspace: "demo",
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "user-token"
	server.ConfigProvider = provider
	server.WorkOrders = orders
	server.Workers = workers
	return server, st, server.Handler()
}

func createTaskRunOrder(t *testing.T, st store.Store, taskID string) core.WorkOrder {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	now := time.Now().UTC()
	task := core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	return order
}

func taskRunHTTPCall(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestTaskRunHTTPIsExplicitlyTaskScopedAndUsesUserLeaseLifecycle(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	target := createTaskRunOrder(t, st, "target")
	createTaskRunOrder(t, st, "other")

	next := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/target/run-order", "")
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"id":"`+target.ID+`"`) || strings.Contains(next.Body.String(), `other-implement-1`) {
		t.Fatalf("next status=%d body=%s", next.Code, next.Body.String())
	}
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"local-codex","model":"local-model"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed core.WorkOrder
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.TaskID != "target" || claimed.WorkerID != "" || claimed.ClaimantID != "run:local-operator" || claimed.Agent != "local-codex" || claimed.Model != "local-model" {
		t.Fatalf("claimed=%+v", claimed)
	}
	renew := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/renew", `{"session_id":"run-session"}`)
	if renew.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renew.Code, renew.Body.String())
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), "target")
	if err != nil {
		t.Fatal(err)
	}
	foundUserRenewal := false
	for _, event := range events {
		foundUserRenewal = foundUserRenewal || (event.Kind == "work_order.lease_renewed" && event.ActorID == "user:local-operator" && event.ActorRole == core.ActorUser)
	}
	if !foundUserRenewal {
		t.Fatalf("renewal did not retain credential-derived user actor: %+v", events)
	}
	if crossTask := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/other/run-orders/"+target.ID+"/renew", `{"session_id":"run-session"}`); crossTask.Code != http.StatusConflict {
		t.Fatalf("cross-task renewal status=%d body=%s", crossTask.Code, crossTask.Body.String())
	}
}

func TestTaskRunHTTPReturnsNoWorkAndSurfacesAssigneeRefusal(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	if response := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/missing/run-order", ""); response.Code != http.StatusNotFound {
		t.Fatalf("missing task status=%d body=%s", response.Code, response.Body.String())
	}
	order := createTaskRunOrder(t, st, "assigned")
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "user:operator", Role: core.ActorUser}), "demo")
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, "assigned", "usr-alice"); err != nil {
		t.Fatal(err)
	}
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/assigned/run-orders/"+order.ID+"/claim", `{"session_id":"bob","client_token":"secret","agent":"codex","model":"gpt"}`)
	if claim.Code != http.StatusConflict || !strings.Contains(claim.Body.String(), "task assigned is assigned to usr-alice; only that assignee may claim its work orders") {
		t.Fatalf("assignment status=%d body=%s", claim.Code, claim.Body.String())
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(store.WithWorkspace(t.Context(), "demo"), order.ID, core.WorkOrderClaim{SessionID: "done", ClientToken: "done", OwnerUserID: "usr-alice", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(store.WithWorkspace(t.Context(), "demo"), claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	if response := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/assigned/run-order", ""); response.Code != http.StatusNoContent {
		t.Fatalf("no-work status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskRunAbandonedInvocationBecomesClaimableAfterLeaseExpiry(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	order := createTaskRunOrder(t, st, "abandoned")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/abandoned/run-orders/"+order.ID+"/claim", `{"session_id":"dead-process","client_token":"secret","agent":"codex","model":"gpt","lease_seconds":1}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	time.Sleep(1100 * time.Millisecond)
	next := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/abandoned/run-order", "")
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"id":"`+order.ID+`"`) {
		t.Fatalf("expired run status=%d body=%s", next.Code, next.Body.String())
	}
}
