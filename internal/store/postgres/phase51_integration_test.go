package postgres

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"sync"
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
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	deadline := claimed.ExecutionDeadline
	renewed, err := storetest.For(st).RenewWorkerClaim(ctx, job.ID, worker.ID, "session", time.Minute)
	if err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	released, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{SessionID: "session", Reason: "worker cancelled", Outcome: core.WorkOrderOutcomeCancelled})
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.IsZero() || !released.ExecutionStartedAt.IsZero() || !released.RetrySuppressed || !released.QueueEnteredAt.After(now) || released.QueueDeadline.Sub(released.QueueEnteredAt) != time.Hour {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].State != core.JobPending || !jobs[0].StartedAt.IsZero() {
		t.Fatalf("released jobs=%+v err=%v", jobs, err)
	}
	recovered, err := storetest.For(st).RecoverWorkOrder(ctx, job.ID, "integration-recovery", time.Hour)
	if err != nil || !recovered.Claimable || recovered.RetrySuppressed {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	duplicate, err := storetest.For(st).RecoverWorkOrder(ctx, job.ID, "integration-recovery", time.Hour)
	if err != nil || duplicate.RedispatchCount != recovered.RedispatchCount {
		t.Fatalf("duplicate recovery=%+v err=%v", duplicate, err)
	}
	secondClaim, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session-2", ClientToken: "token-2", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || !secondClaim.ExecutionStartedAt.After(claimed.ExecutionStartedAt) || !secondClaim.ExecutionDeadline.After(deadline) {
		t.Fatalf("second claim=%+v err=%v", secondClaim, err)
	}
	if _, staleErr := storetest.For(st).RenewWorkerClaim(ctx, job.ID, worker.ID, "session", time.Minute); staleErr == nil {
		t.Fatal("stale session renewed newer claim")
	}
	if _, staleErr := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{SessionID: "session", Outcome: core.WorkOrderOutcomeCancelled}); staleErr == nil {
		t.Fatal("stale session released newer claim")
	}
	exit := 1
	failed, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{SessionID: "session-2", Reason: "harness exited: status 1", Outcome: core.WorkOrderOutcomeChildFailure, ExitStatus: &exit, InitialRetryDelay: time.Second, MaximumRetryDelay: 4 * time.Second, AutomaticRetryLimit: 3})
	if err != nil || failed.AutomaticRetryCount != 1 || failed.RetrySuppressed || failed.LastFailureExitStatus == nil || *failed.LastFailureExitStatus != 1 || failed.NextRetryAt.Sub(failed.LastFailureAt) != time.Second {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	var recoveries sync.WaitGroup
	recoveryErrors := make(chan error, 4)
	for range 4 {
		recoveries.Add(1)
		go func() {
			defer recoveries.Done()
			_, recoverErr := storetest.For(st).RecoverWorkOrder(ctx, job.ID, "concurrent-recovery", time.Hour)
			recoveryErrors <- recoverErr
		}()
	}
	recoveries.Wait()
	close(recoveryErrors)
	for recoverErr := range recoveryErrors {
		if recoverErr != nil {
			t.Fatalf("concurrent recovery: %v", recoverErr)
		}
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.redispatched"); countErr != nil || count != 2 {
		t.Fatalf("redispatch events=%d err=%v", count, countErr)
	}

	expiredTask := task
	expiredTask.ID = core.NewTaskID()
	expiredTask.Branch = "conveyor/task-" + expiredTask.ID
	if err = st.CreateTask(ctx, expiredTask); err != nil {
		t.Fatal(err)
	}
	expiredJob := core.Job{ID: expiredTask.ID + "-review", TaskID: expiredTask.ID, Stage: core.StageReview, State: core.JobPending}
	if err = st.CreateJob(ctx, expiredJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: expiredJob.ID, TaskID: expiredTask.ID, JobID: expiredJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, expiredJob.ID, core.WorkOrderClaim{SessionID: "expiring-session", ClientToken: "expiring-token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Nanosecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, expiredJob.ID, worker.ID, "expiring-session", time.Minute); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("expired session renewed directly: %v", err)
	}
	if _, err = storetest.For(st).ReleaseWorkerClaim(ctx, expiredJob.ID, worker.ID, core.WorkOrderRelease{SessionID: "expiring-session", Outcome: core.WorkOrderOutcomeReleased}); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("expired session released directly: %v", err)
	}
	expired, err := st.GetWorkOrder(ctx, expiredJob.ID)
	if err != nil || expired.State != core.WorkOrderQueued || !expired.RetrySuppressed || expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired || expired.WorkerID != "" || expired.SessionID != "" || !expired.ExecutionStartedAt.IsZero() || !expired.ExecutionDeadline.IsZero() {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	expiredJobs, err := st.ListJobs(ctx, expiredTask.ID)
	if err != nil || len(expiredJobs) != 1 || expiredJobs[0].State != core.JobPending || !expiredJobs[0].StartedAt.IsZero() {
		t.Fatalf("expired jobs=%+v err=%v", expiredJobs, err)
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
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: submittedJob.ID, TaskID: submittedTask.ID, JobID: submittedJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	submittedClaim, err := storetest.For(st).ClaimWorkOrder(ctx, submittedJob.ID, core.WorkOrderClaim{SessionID: "submitted-session", ClientToken: "submitted-token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	submittedClaim.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, submittedClaim); err != nil {
		t.Fatal(err)
	}
	if submitted, renewErr := storetest.For(st).RenewWorkerClaim(ctx, submittedJob.ID, worker.ID, "submitted-session", time.Minute); renewErr != nil || submitted.State != core.WorkOrderSubmitted || !submitted.ExecutionDeadline.Equal(submittedClaim.ExecutionDeadline) {
		t.Fatalf("submitted renew=%+v err=%v", submitted, renewErr)
	}
}
