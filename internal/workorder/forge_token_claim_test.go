package workorder

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestClaimSucceedsWithoutStoredForgeTokens(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "forge-gated", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	claim := core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner"}
	if _, err := service.Claim(ctx, job.ID, claim); err != nil {
		t.Fatalf("claim without stored credentials: %v", err)
	}
	claimed, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claimed order=%+v err=%v", claimed, err)
	}
}
