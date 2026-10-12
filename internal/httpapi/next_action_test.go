package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestNextActionRouteStatesLabel(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	task := core.Task{ID: "task-next", Title: "Need a plan", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, Workspace: "demo"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-next/next-action", nil).WithContext(ctx)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", task.ID)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	server.getNextAction(response, request)
	var action nextaction.NextAction
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &action) != nil || action.Kind != nextaction.KindPlanApproval || action.Label == "" || action.Actor != nextaction.ActorOperator {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
