package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPhase51WorkerPersistenceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "phase51-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess}, "spec": {Model: "gpt", TimeoutText: "1m", Timeout: time.Minute, Execution: config.ExecutionInProcess}, "implement": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP}, "review": {Model: "operator", TimeoutText: "1h", Timeout: time.Hour, Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pairing := core.WorkerPairing{TokenHash: "pair-" + core.NewTaskID(), Workspace: workspace, ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err = st.CreateWorkerPairing(ctx, pairing); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConsumeWorkerPairing(ctx, pairing.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConsumeWorkerPairing(ctx, pairing.TokenHash, now); err == nil {
		t.Fatal("pairing reuse succeeded")
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, Name: "integration", CredentialHash: "credential-" + core.NewTaskID(), CreatedAt: now}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	worker, err = st.HeartbeatWorker(ctx, worker.ID, now.Add(15*time.Second), []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	if err != nil || !worker.Live(now) {
		t.Fatalf("worker=%+v err=%v", worker, err)
	}
	if authenticated, authErr := st.AuthenticateWorker(ctx, worker.CredentialHash); authErr != nil || authenticated.ID != worker.ID {
		t.Fatalf("auth=%+v err=%v", authenticated, authErr)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Mode: core.TaskModeAuto, PolicyVersion: 1, SpecApproval: true, MergeApproval: true, Level: core.L2, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	deadline := claimed.ExecutionDeadline
	renewed, err := st.RenewWorkerClaim(ctx, job.ID, worker.ID, time.Minute)
	if err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	released, err := st.ReleaseWorkerClaim(ctx, job.ID, worker.ID, "integration")
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("released=%+v err=%v", released, err)
	}

	submittedTask := task
	submittedTask.ID = core.NewTaskID()
	submittedTask.Branch = "conveyor/task-" + submittedTask.ID
	if err = st.CreateTask(ctx, submittedTask); err != nil {
		t.Fatal(err)
	}
	submittedJob := core.Job{ID: submittedTask.ID + "-implement", TaskID: submittedTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, submittedJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWorkOrder(ctx, core.WorkOrder{ID: submittedJob.ID, TaskID: submittedTask.ID, JobID: submittedJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	submittedClaim, err := st.ClaimWorkOrder(ctx, submittedJob.ID, core.WorkOrderClaim{SessionID: "submitted-session", ClientToken: "submitted-token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	submittedClaim.State = core.WorkOrderSubmitted
	if err = st.UpdateWorkOrder(ctx, submittedClaim); err != nil {
		t.Fatal(err)
	}
	if submitted, renewErr := st.RenewWorkerClaim(ctx, submittedJob.ID, worker.ID, time.Minute); renewErr != nil || submitted.State != core.WorkOrderSubmitted || !submitted.ExecutionDeadline.Equal(submittedClaim.ExecutionDeadline) {
		t.Fatalf("submitted renew=%+v err=%v", submitted, renewErr)
	}
}
