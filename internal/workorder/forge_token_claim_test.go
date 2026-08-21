package workorder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

type claimForgeTokens struct{ configured bool }

func (*claimForgeTokens) StoreForgeToken(context.Context, string, string, string) (core.ForgeTokenStatus, error) {
	return core.ForgeTokenStatus{}, nil
}
func (*claimForgeTokens) DeleteForgeToken(context.Context, string) error { return nil }
func (f *claimForgeTokens) GetForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error) {
	return core.ForgeTokenStatus{Configured: f.configured}, nil
}
func (*claimForgeTokens) GetForgeTokenForUse(context.Context, string) (core.ForgeTokenCredential, error) {
	return core.ForgeTokenCredential{}, nil
}
func (*claimForgeTokens) ListForgeTokensForRedaction(context.Context) ([]string, error) {
	return nil, nil
}

func TestClaimRequiresStoredForgeTokenAndLeavesOrderQueued(t *testing.T) {
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
	tokens := &claimForgeTokens{}
	service := &Service{Store: st, ForgeTokens: tokens, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	claim := core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner"}
	if _, err := service.Claim(ctx, job.ID, claim); !errors.Is(err, store.ErrForgeTokenRequired) {
		t.Fatalf("missing-token claim error=%v", err)
	}
	queued, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || queued.State != core.WorkOrderQueued {
		t.Fatalf("refused order=%+v err=%v", queued, err)
	}
	tokens.configured = true
	if _, err = service.Claim(ctx, job.ID, claim); err != nil {
		t.Fatalf("configured-token claim: %v", err)
	}
}
