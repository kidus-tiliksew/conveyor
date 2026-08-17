package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestWorkerWarningFollowsEnrollmentLifecycleIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "serviceability-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	now := time.Now().UTC()
	service := &workerservice.Service{Store: st, Now: func() time.Time { return now }}
	cfg := &config.Config{Workspace: workspace, Harnesses: []config.Harness{{Name: "codex"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "codex", Execution: config.ExecutionMCP}}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "claimant-aware", Workspace: workspace, State: core.TaskRunning}
	orders := []core.WorkOrder{{ID: "claimant-aware-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredHarness: "codex"}}

	if status := service.TaskAvailability(ctx, cfg, task, orders); status != nil {
		t.Fatalf("zero-worker status=%+v", status)
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, Name: "stale-codex", LastSeenAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(-time.Second), CreatedAt: now}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	status := service.TaskAvailability(ctx, cfg, task, orders)
	if status == nil || status.Available || !strings.Contains(status.Reason, "no live enrolled worker") || len(status.RequiredHarnesses) != 0 {
		t.Fatalf("stale-worker status=%+v", status)
	}
	if err = st.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if status = service.TaskAvailability(ctx, cfg, task, orders); status != nil {
		t.Fatalf("revoked-worker status=%+v", status)
	}
}

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
	var eventsBefore int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1`, workspace).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	firstLease := now.Add(15 * time.Second)
	worker, err = st.HeartbeatWorker(ctx, worker.ID, firstLease, []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	if err != nil || !worker.Live(now) {
		t.Fatalf("worker=%+v err=%v", worker, err)
	}
	firstSeen := worker.LastSeenAt
	secondLease := now.Add(30 * time.Second)
	worker, err = st.HeartbeatWorker(ctx, worker.ID, secondLease, []core.HarnessProbe{{Harness: "codex", Healthy: false, Message: "unavailable", CheckedAt: now.Add(time.Second)}})
	if err != nil || !worker.LeaseExpiresAt.Equal(secondLease.Truncate(time.Microsecond)) || worker.LastSeenAt.Before(firstSeen) || len(worker.Probes) != 1 || worker.Probes[0].Healthy || worker.Probes[0].Message != "unavailable" {
		t.Fatalf("second heartbeat worker=%+v err=%v", worker, err)
	}
	var eventsAfter, heartbeatEvents int
	if err = st.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE kind='worker.heartbeat') FROM events WHERE workspace_id=$1`, workspace).Scan(&eventsAfter, &heartbeatEvents); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore || heartbeatEvents != 0 {
		t.Fatalf("events changed from %d to %d; worker.heartbeat events=%d, want 0", eventsBefore, eventsAfter, heartbeatEvents)
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
	checkpoint := core.WorkOrderAttemptCheckpoint{
		SessionID: "session", AttemptID: claimed.AttemptID, TerminationReason: "harness exited",
		CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PushResult: "pushed",
	}
	var checkpointWrites sync.WaitGroup
	checkpointResults := make(chan bool, 4)
	checkpointErrors := make(chan error, 4)
	for range 4 {
		checkpointWrites.Add(1)
		go func() {
			defer checkpointWrites.Done()
			created, checkpointErr := st.RecordWorkOrderAttemptCheckpoint(ctx, job.ID, worker.ID, checkpoint)
			checkpointResults <- created
			checkpointErrors <- checkpointErr
		}()
	}
	checkpointWrites.Wait()
	close(checkpointResults)
	close(checkpointErrors)
	for checkpointErr := range checkpointErrors {
		if checkpointErr != nil {
			t.Fatalf("concurrent attempt checkpoint: %v", checkpointErr)
		}
	}
	createdCount := 0
	for created := range checkpointResults {
		if created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created attempt checkpoints=%d, want 1", createdCount)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.attempt_checkpointed"); countErr != nil || count != 1 {
		t.Fatalf("attempt checkpoint events=%d err=%v", count, countErr)
	}
	released, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{SessionID: "session", Reason: "worker cancelled", Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeCancelled})
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.IsZero() || !released.ExecutionStartedAt.IsZero() || !released.RetrySuppressed || !released.QueueEnteredAt.After(now) || released.QueueDeadline.Sub(released.QueueEnteredAt) != time.Hour {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	if events, eventErr := st.ListEvents(ctx, task.ID); eventErr != nil {
		t.Fatal(eventErr)
	} else {
		found := false
		for _, event := range events {
			if event.Kind == "work_order.released" && event.JobID == job.ID {
				var payload struct {
					Cause string `json:"release_cause"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Cause == core.WorkOrderReleaseCauseOperatorAction {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("release event did not preserve structured cause: %+v", events)
		}
	}
	lateCheckpoint := checkpoint
	lateCheckpoint.CommitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lateCheckpoint.TerminationReason = "claim authority lost: server reports queued"
	if created, checkpointErr := st.RecordWorkOrderAttemptCheckpoint(ctx, job.ID, worker.ID, lateCheckpoint); checkpointErr != nil || !created {
		t.Fatalf("late authority-loss checkpoint created=%v err=%v", created, checkpointErr)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "work_order.attempt_checkpointed"); countErr != nil || count != 2 {
		t.Fatalf("attempt checkpoint events after authority loss=%d err=%v", count, countErr)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].State != core.JobPending || !jobs[0].StartedAt.IsZero() {
		t.Fatalf("released jobs=%+v err=%v", jobs, err)
	}
	direction := "Proceed with the accepted amendment."
	recovered, err := storetest.For(st).RecoverWorkOrderWithDirection(ctx, job.ID, "integration-recovery", "  "+direction+"  ", time.Hour)
	if err != nil || !recovered.Claimable || recovered.RetrySuppressed || recovered.OperatorDirection != direction {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	duplicate, err := storetest.For(st).RecoverWorkOrderWithDirection(ctx, job.ID, "integration-recovery", "replacement", time.Hour)
	if err != nil || duplicate.RedispatchCount != recovered.RedispatchCount || duplicate.OperatorDirection != direction {
		t.Fatalf("duplicate recovery=%+v err=%v", duplicate, err)
	}
	secondClaim, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session-2", ClientToken: "token-2", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || !secondClaim.ExecutionStartedAt.After(claimed.ExecutionStartedAt) || !secondClaim.ExecutionDeadline.After(deadline) || secondClaim.OperatorDirection != direction {
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
	if err != nil || failed.AutomaticRetryCount != 1 || failed.RetrySuppressed || failed.LastFailureExitStatus == nil || *failed.LastFailureExitStatus != 1 || failed.NextRetryAt.Sub(failed.LastFailureAt) != time.Second || failed.OperatorDirection != "" {
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
	thirdClaim, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session-2", ClientToken: "token-3", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil || thirdClaim.AttemptID == "" || thirdClaim.AttemptID == secondClaim.AttemptID {
		t.Fatalf("same-session reclaim=%+v prior_attempt=%q err=%v", thirdClaim, secondClaim.AttemptID, err)
	}
	thirdClosed, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{SessionID: "session-2", Outcome: core.WorkOrderOutcomeCancelled})
	if err != nil || thirdClosed.LastAttemptID != thirdClaim.AttemptID {
		t.Fatalf("same-session close=%+v err=%v", thirdClosed, err)
	}
	attemptEvents, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimAttemptIDs := integrationEventAttemptIDs(t, attemptEvents, "work_order.claimed", job.ID)
	closedAttemptIDs := append(integrationEventAttemptIDs(t, attemptEvents, "work_order.released", job.ID), integrationEventAttemptIDs(t, attemptEvents, "work_order.child_failed", job.ID)...)
	if len(claimAttemptIDs) != 3 || len(closedAttemptIDs) != 3 || claimAttemptIDs[1] != secondClaim.AttemptID || claimAttemptIDs[2] != thirdClaim.AttemptID {
		t.Fatalf("same-session attempt groups claims=%v closes=%v", claimAttemptIDs, closedAttemptIDs)
	}
	closedSet := map[string]bool{}
	for _, attemptID := range closedAttemptIDs {
		closedSet[attemptID] = true
	}
	if !closedSet[secondClaim.AttemptID] || !closedSet[thirdClaim.AttemptID] {
		t.Fatalf("same-session closures lost identity: %v", closedAttemptIDs)
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
	if events, eventErr := st.ListEvents(ctx, expiredTask.ID); eventErr != nil {
		t.Fatal(eventErr)
	} else {
		found := false
		for _, event := range events {
			if event.Kind == "work_order.expired" && event.JobID == expiredJob.ID {
				var payload struct {
					Cause string `json:"release_cause"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Cause == core.WorkOrderReleaseCauseLeaseLoss {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("expiry event did not preserve lease-loss cause: %+v", events)
		}
	}

	runTask := task
	runTask.ID = core.NewTaskID()
	runTask.Branch = "conveyor/task-" + runTask.ID
	if err = st.CreateTask(ctx, runTask); err != nil {
		t.Fatal(err)
	}
	runJob := core.Job{ID: runTask.ID + "-implement", TaskID: runTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, runJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: runJob.ID, TaskID: runTask.ID, JobID: runJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, runJob.ID, core.WorkOrderClaim{SessionID: "abandoned-run", ClientToken: "run-token", ClaimantID: core.TaskRunClaimantID("usr-local"), OwnerUserID: "usr-local", Agent: "codex", Model: "gpt", Lease: time.Nanosecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, runJob.ID, "", "abandoned-run", time.Minute); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("abandoned run renewed after expiry: %v", err)
	}
	reclaimable, err := st.GetWorkOrder(ctx, runJob.ID)
	if err != nil || reclaimable.State != core.WorkOrderQueued || reclaimable.RetrySuppressed || !reclaimable.Claimable || reclaimable.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
		t.Fatalf("abandoned run=%+v err=%v", reclaimable, err)
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

func TestTransientConnectivityBackoffPersistenceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "transient-backoff-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", State: core.TaskRunning, CreatedAt: now}
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
	workerID := "worker-" + core.NewTaskID()
	release := func(attempt int, progress bool) core.WorkOrder {
		t.Helper()
		sessionID := fmt.Sprintf("transient-session-%d", attempt)
		_, claimErr := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: sessionID, ClientToken: sessionID, WorkerID: workerID, Lease: time.Minute, ExecutionTimeout: time.Hour})
		if claimErr != nil {
			t.Fatalf("claim %d: %v", attempt, claimErr)
		}
		if progress {
			if appendErr := st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "work_order.progress_reported", Payload: core.JSONPayload(map[string]any{"message": "forward progress"}), At: time.Now().UTC()}); appendErr != nil {
				t.Fatalf("progress %d: %v", attempt, appendErr)
			}
		}
		result, releaseErr := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, workerID, core.WorkOrderRelease{
			SessionID: sessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "connection failed",
			FailureCategory: core.WorkOrderFailureTransientConnectivity, FailureDetail: fmt.Sprintf("network-%d", attempt),
			AutomaticRetryLimit: 5, InitialRetryDelay: time.Second, MaximumRetryDelay: 4 * time.Second,
		})
		if releaseErr != nil {
			t.Fatalf("release %d: %v", attempt, releaseErr)
		}
		return result
	}
	advance := func(order core.WorkOrder) {
		t.Helper()
		order.NextRetryAt = time.Now().Add(-time.Millisecond)
		if updateErr := storetest.For(st).UpdateWorkOrder(ctx, order); updateErr != nil {
			t.Fatal(updateErr)
		}
	}

	first := release(1, false)
	if got := first.NextRetryAt.Sub(first.LastFailureAt); got != 30*time.Second {
		t.Fatalf("first delay=%s", got)
	}
	advance(first)
	second := release(2, false)
	if got := second.NextRetryAt.Sub(second.LastFailureAt); got != 2*time.Minute {
		t.Fatalf("second delay=%s", got)
	}
	advance(second)
	reset := release(3, true)
	if got := reset.NextRetryAt.Sub(reset.LastFailureAt); got != 30*time.Second {
		t.Fatalf("progress reset delay=%s", got)
	}
	if !strings.Contains(reset.LastFailureDetail, "consecutive_transient_failures=1") {
		t.Fatalf("reset detail=%q", reset.LastFailureDetail)
	}

	reopened, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || !reopened.NextRetryAt.Equal(reset.NextRetryAt) || reopened.LastFailureDetail != reset.LastFailureDetail {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Category    string    `json:"failure_category"`
		Consecutive int       `json:"consecutive_transient_failures"`
		NextRetryAt time.Time `json:"next_retry_at"`
	}
	if err = json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload.Category != core.WorkOrderFailureTransientConnectivity || payload.Consecutive != 1 || !payload.NextRetryAt.Equal(reset.NextRetryAt) {
		t.Fatalf("failure payload=%+v err=%v", payload, err)
	}
}
