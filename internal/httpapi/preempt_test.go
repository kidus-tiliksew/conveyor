package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestWorkOrderPreemptHTTPRequiresOperatorReasonAndIdempotency(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "preempt-http-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "preempt-http-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "http-session", ClientToken: "secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	handler := server.Handler()

	request := func(body, token, key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+job.ID+"/preempt?workspace_id=demo", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("X-Idempotency-Key", key)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conveyor-Actor", "operator-http")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	if response := request(`{"reason":"stop"}`, "", "request-1"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body)
	}
	if response := request(`{"request_id":"request-1"}`, "operator-token", "request-1"); response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status=%d body=%s", response.Code, response.Body)
	}
	if response := request(`{"reason":"stop","request_id":"body-key"}`, "operator-token", "header-key"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "must match") {
		t.Fatalf("mismatched key status=%d body=%s", response.Code, response.Body)
	}
	response := request(`{"reason":"replace setup"}`, "operator-token", "request-1")
	if response.Code != http.StatusOK {
		t.Fatalf("preempt status=%d body=%s", response.Code, response.Body)
	}
	var result store.WorkOrderPreemptResult
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.WorkOrder.State != core.WorkOrderQueued || result.RevokedAttemptID != claimed.AttemptID || result.RevokedSessionID != "http-session" {
		t.Fatalf("result=%+v", result)
	}
	duplicate := request(`{"reason":"replace setup","request_id":"request-1"}`, "operator-token", "request-1")
	if duplicate.Code != http.StatusOK || duplicate.Body.String() != response.Body.String() {
		t.Fatalf("duplicate status=%d body=%s want=%s", duplicate.Code, duplicate.Body, response.Body)
	}
	events, err := st.ListEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "work_order.preempted" && (event.ActorID != "operator-http" || event.ActorRole != core.ActorHuman) {
			t.Fatalf("preempt actor=%+v", event)
		}
	}
}
