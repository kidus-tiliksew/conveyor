package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestRequestChangesBouncesThroughSharedContextAndAttention(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "merge-gate-request", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-merge-gate-request", State: core.TaskRunning,
		NextStage: core.StageReview, RecoveryStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now().UTC()}
	if err := st.CreateJob(ctx, reviewJob); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", MaxBounces: 3, WorkOrderQueueTimeout: time.Hour,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Model: "implementer", Harness: "codex", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Harnesses: []config.Harness{{Name: "codex"}},
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	}
	dispatcher := dispatch.New(st, cfg, nil)
	server := NewServer(st)
	server.BearerToken, server.Workspace = "user-token", "demo"
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	var dispatchErr error
	server.OnCreate = func(callCtx context.Context, id string) { dispatchErr = dispatcher.DispatchNow(callCtx, id) }

	feedback := "Keep this line exactly.\n\n  Preserve the indentation too."
	body, _ := json.Marshal(map[string]string{"feedback": feedback})
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/request-changes", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || dispatchErr != nil {
		t.Fatalf("status=%d body=%s dispatch=%v", response.Code, response.Body.String(), dispatchErr)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskQueued || updated.NextStage != core.StageImplement || updated.Hold {
		t.Fatalf("task=%+v err=%v", updated, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement || orders[0].State != core.WorkOrderQueued {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].ActorID != "user:local-operator" || interventions[0].Comment != feedback {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	markers, err := st.ListActivityMarkers(ctx)
	if err != nil || len(markers) != 1 || !markers[0].UserChangesRequested || !store.TaskNeedsAttention(updated, markers[0], false) {
		t.Fatalf("markers=%+v err=%v", markers, err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := (&workorder.Service{Store: st, Pack: bundle, ConfigProvider: server.ConfigProvider}).GetVisible(ctx, orders[0].ID)
	if err != nil || len(assembled.PriorFeedback) != 1 || assembled.PriorFeedback[0] != feedback {
		t.Fatalf("prior feedback=%q err=%v", assembled.PriorFeedback, err)
	}
	duplicate := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/request-changes", bytes.NewReader(body))
	duplicate.Header.Set("Authorization", "Bearer user-token")
	duplicateResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", duplicateResponse.Code, duplicateResponse.Body.String())
	}
}

func TestRequestChangesRejectsNonAssignee(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "assigned-merge-gate", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, RecoveryStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, task.ID, "someone-else"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken = "user-token"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/request-changes", bytes.NewBufferString(`{"feedback":"fix it"}`))
	request.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
