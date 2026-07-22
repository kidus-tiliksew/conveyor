package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskCancellationAndRecoveryRefreezeIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "cancel-refreeze-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "old", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP}}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	cancelTask := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err = st.CreateTask(ctx, cancelTask); err != nil {
		t.Fatal(err)
	}
	cancelJob := core.Job{ID: cancelTask.ID + "-implement-1", TaskID: cancelTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, cancelJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: cancelJob.ID, TaskID: cancelTask.ID, JobID: cancelJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimWorkOrder(ctx, cancelJob.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	closed, err := st.CancelTask(ctx, core.Intervention{TaskID: cancelTask.ID, JobID: cancelJob.ID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	if err != nil || closed.State != core.TaskClosed {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	cancelled, _ := st.GetWorkOrder(ctx, cancelJob.ID)
	if cancelled.State != core.WorkOrderCancelled || cancelled.SessionID != "session" {
		t.Fatalf("cancelled=%+v", cancelled)
	}
	if _, err = st.RenewWorkerClaim(ctx, cancelJob.ID, "worker", "session", time.Minute); !errors.Is(err, store.ErrWorkOrderCancelled) {
		t.Fatalf("renew error=%v", err)
	}

	prior := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "old", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "1h"}}}
	next := config.ExecutionSetup{Name: "default", ExecutionSettings: config.ContextualExecutionSettings{Implementation: config.ImplementationSettings{Harness: "codex", Model: "new", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"}}}
	recoverTask := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, SetupName: "default", SetupContract: prior, CreatedAt: now}
	if err = st.CreateTask(ctx, recoverTask); err != nil {
		t.Fatal(err)
	}
	recoverJob := core.Job{ID: recoverTask.ID + "-implement-1", TaskID: recoverTask.ID, Stage: core.StageImplement, State: core.JobFailed}
	if err = st.CreateJob(ctx, recoverJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: recoverJob.ID, TaskID: recoverTask.ID, JobID: recoverJob.ID, Stage: core.StageImplement, State: core.WorkOrderStale, RequiredModel: "old", RequiredHarness: "codex", ExecutionTimeoutText: "1h", QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	change := &store.RecoveryRefreeze{Setup: next, RequiredModel: "new", RequiredHarness: "codex", RequiredEffort: "high", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"codex", "exec"}, Effort: "high", EffortArgv: []string{"--effort", "high"}}, ExecutionTimeoutText: "2h"}
	recovered, err := st.RecoverWorkOrder(ctx, recoverJob.ID, "recover-refreeze", time.Hour, change)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.RequiredModel != "new" || recovered.RequiredEffort != "high" {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	persisted, _ := st.GetTask(ctx, recoverTask.ID)
	count, _ := st.CountEvents(ctx, recoverTask.ID, "task.setup.refrozen")
	if persisted.SetupContract.ExecutionSettings.Implementation.Model != "new" || count != 1 {
		t.Fatalf("setup=%+v events=%d", persisted.SetupContract, count)
	}
}
