package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestWorkOrderPreemptionPersistenceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "preempt-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "operator", Timeout: time.Hour, Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Hold: true, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	queueEnteredAt := now.Add(-time.Minute)
	queueDeadline := now.Add(time.Hour)
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: queueEnteredAt, QueueDeadline: queueDeadline, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "pg-preempt-session", ClientToken: "secret", ClaimantID: "pg-worker", WorkerID: "pg-worker", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed.AutomaticRetryCount = 2
	claimed.CostUSD, claimed.TokensIn, claimed.TokensOut = 2.5, 200, 80
	claimed.UsageReported, claimed.SelfReported = true, true
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}

	service := &workorder.Service{Store: st}
	result, err := service.Preempt(ctx, job.ID, "replace execution setup", "pg-preempt-request")
	if err != nil {
		t.Fatal(err)
	}
	preempted := result.WorkOrder
	if preempted.State != core.WorkOrderQueued || preempted.LastAttemptOutcome != core.WorkOrderOutcomePreempted ||
		preempted.LastAttemptID != claimed.AttemptID || !preempted.QueueEnteredAt.Equal(queueEnteredAt) || !preempted.QueueDeadline.Equal(queueDeadline) ||
		preempted.AutomaticRetryCount != 2 || preempted.CostUSD != 2.5 || preempted.TokensIn != 200 || preempted.TokensOut != 80 || !preempted.UsageReported || !preempted.SelfReported {
		t.Fatalf("preempted=%+v", preempted)
	}
	if duplicate, duplicateErr := service.Preempt(ctx, job.ID, "replace execution setup", "pg-preempt-request"); duplicateErr != nil || duplicate.RevokedAttemptID != result.RevokedAttemptID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, duplicateErr)
	}
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, job.ID, "pg-worker", "pg-preempt-session", time.Minute); !errors.Is(err, store.ErrWorkOrderPreempted) {
		t.Fatalf("preempt renewal err=%v", err)
	}
	checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: "pg-preempt-session", AttemptID: claimed.AttemptID, TerminationReason: "work order was preempted by an operator", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PushResult: "pushed"}
	if created, checkpointErr := st.RecordWorkOrderAttemptCheckpoint(ctx, job.ID, "pg-worker", checkpoint); checkpointErr != nil || !created {
		t.Fatalf("checkpoint created=%v err=%v", created, checkpointErr)
	}

	successor, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "pg-successor", ClientToken: "successor-secret", ClaimantID: "pg-worker-2", WorkerID: "pg-worker-2", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, job.ID, "pg-worker", "pg-preempt-session", time.Minute); !errors.Is(err, store.ErrWorkOrderPreempted) {
		t.Fatalf("stale preempted renewal against successor err=%v", err)
	}
	if renewed, renewErr := storetest.For(st).RenewWorkerClaim(ctx, job.ID, "pg-worker-2", "pg-successor", time.Minute); renewErr != nil || renewed.AttemptID != successor.AttemptID {
		t.Fatalf("successor renewal=%+v err=%v", renewed, renewErr)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.preempted"); countErr != nil || count != 1 {
		t.Fatalf("preempt events=%d err=%v", count, countErr)
	}
	persistedTask, err := st.GetTask(ctx, task.ID)
	if err != nil || !persistedTask.Hold {
		t.Fatalf("held task=%+v err=%v", persistedTask, err)
	}
}

func TestWorkOrderPreemptionExpiresDeadClaimBeforeEligibility(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "preempt-expired-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(context.Background(), workspace), store.Actor{ID: "operator-pg", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "operator", Timeout: time.Hour, Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "dead-session", ClientToken: "secret", ClaimantID: "dead-worker", WorkerID: "dead-worker", Agent: "codex", Model: "gpt", Lease: time.Nanosecond, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	service := &workorder.Service{Store: st}
	if _, err = service.Preempt(ctx, job.ID, "worker is gone", "expired-preempt-request"); !errors.Is(err, store.ErrWorkOrderPreemptConflict) {
		t.Fatalf("expired preempt err=%v", err)
	}
	expired, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || expired.State != core.WorkOrderQueued || expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired || expired.LastAttemptID != claimed.AttemptID || !expired.RetrySuppressed {
		t.Fatalf("expired order=%+v err=%v", expired, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.preempted"); countErr != nil || count != 0 {
		t.Fatalf("preempt events=%d err=%v", count, countErr)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.expired"); countErr != nil || count != 1 {
		t.Fatalf("expiry events=%d err=%v", count, countErr)
	}
}
