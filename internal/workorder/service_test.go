package workorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type staticAgent struct{ output string }

func (agent staticAgent) Run(context.Context, string, string) (inprocess.Result, error) {
	return inprocess.Result{Output: agent.output, TokensIn: 10, TokensOut: 4}, nil
}

func TestUsagePersistsHighReportWithoutGating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: "job", TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, "job", core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	reported, err := service.Usage(ctx, claimed.ID, "session", 100_000_000, 25_000_000, 20_000)
	if err != nil {
		t.Fatalf("usage error = %v", err)
	}
	if reported.CostUSD != 20_000 {
		t.Fatalf("returned cost = %v", reported.CostUSD)
	}
	stored, getErr := st.GetWorkOrder(ctx, claimed.ID)
	if getErr != nil || stored.CostUSD != 20_000 || stored.TokensIn != 100_000_000 || stored.TokensOut != 25_000_000 {
		t.Fatalf("stored = %+v err=%v", stored, getErr)
	}
	if _, err = service.Progress(ctx, claimed.ID, "session", "continuing after high usage"); err != nil {
		t.Fatalf("high usage gated progress: %v", err)
	}
}

func TestWorkOrderTimeoutStillEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "timeout-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "timeout-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().Add(-2 * time.Hour)}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) {
		return &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}}
	_, err := service.Claim(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Agent: "codex", Model: "gpt", Lease: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "wall clock exhausted") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestExpiredLeaseReturnsWorkOrderToQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "lease-task", Workspace: "test", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "lease-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	_, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", Lease: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	queued, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || queued.State != core.WorkOrderQueued {
		t.Fatalf("expired order = %+v err=%v", queued, err)
	}
}

func TestSubmitForReviewReturnsSynchronousInProcessVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task-sync", Workspace: "test", Repo: "app", Title: "Change", Branch: "conveyor/task-sync", BaseBranch: "main", Level: core.L0, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, ModelTier: "implementer", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", Agent: "codex", Model: "implementer", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Model: "reviewer", Execution: config.ExecutionInProcess, Timeout: time.Minute},
	}}}
	dispatcher := dispatch.New(st, cfg, staticAgent{output: "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"all criteria pass\",\"feedback\":\"\"}\n```"})
	dispatcher.Pack = bundle
	service := &Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}

	if _, err = service.Usage(ctx, claimed.ID, "implement-session", 100_000_000, 25_000_000, 20_000); err != nil {
		t.Fatalf("high usage report failed: %v", err)
	}
	result, err := service.SubmitForReview(ctx, claimed.ID, "implement-session")
	if err != nil {
		t.Fatal(err)
	}
	if result["await_review"] != false || result["verdict"] != "approve" {
		t.Fatalf("result = %+v", result)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskApproved {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
}
