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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestRecoverRefusesUnclaimableVerify(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{
		ID: "recover-refusal", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement,
		ReviewedHeadSHA: "head", CreatedAt: now,
	}
	task.SetupContract.VerifyStage = true
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{
		ID: task.ID + "-verify-1", JobID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify,
		State: core.WorkOrderQueued, HeadSHA: "head", CreatedAt: now, QueueEnteredAt: now,
		QueueDeadline: now.Add(time.Hour),
	}
	if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{
			Workspace: "demo", WorkOrderQueueTimeout: time.Hour,
			Routing: config.Routing{Stages: map[string]config.StageRoute{string(core.StageVerify): {Timeout: time.Hour}}},
		}, nil
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	want := "verify order " + order.ID + " cannot be claimed: task recover-refusal next_stage is implement, not verify"
	mcpReq := auth(httptest.NewRequest(http.MethodPost, "/mcp?workspace_id=demo", nil))
	mcpReq = mcpReq.WithContext(withCredentialActor(mcpReq.Context(), core.AuthenticatedCredential{ID: "op", OwnerUserID: "op", Kind: core.CredentialUser}))
	_, mcpErr := server.callMCPTool(mcpReq, "claim_work_order", map[string]any{
		"workspace_id": "demo", "work_order_id": order.ID, "session_id": "mcp-claim",
		"client_token": "mcp-claim", "agent": "test", "model": "test",
	})
	if mcpErr == nil || mcpErr.Error() != want {
		t.Fatalf("mcp claim err=%v want %q", mcpErr, want)
	}
	if _, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
		return st.ApplyWorkOrderClock(ctx, lease, task.ID, now.Add(time.Hour+time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	req := auth(httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+order.ID+"/recover?workspace_id=demo", strings.NewReader(`{"request_id":"http-recover"}`)))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "next_stage is implement, not verify") {
		t.Fatalf("recover status=%d body=%s", rec.Code, rec.Body.String())
	}

	listReq := auth(httptest.NewRequest(http.MethodGet, "/v1/work-orders?workspace_id=demo", nil))
	listRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []core.WorkOrder
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].RecoveryRefusal != want || listed[0].Claimable {
		t.Fatalf("list=%+v want %q", listed, want)
	}

	actReq := auth(httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	actRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(actRec, actReq)
	if actRec.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", actRec.Code, actRec.Body.String())
	}
	if !strings.Contains(actRec.Body.String(), `"recovery_refusal"`) || !strings.Contains(actRec.Body.String(), want) {
		t.Fatalf("activity body=%s", actRec.Body.String())
	}
}
