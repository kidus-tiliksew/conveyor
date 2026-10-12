package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runAutomaticRecoveryRemaining(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	runQueuedExpiredRecovery(t, st, ctx, workspace)
	runQueuedRetryLimitRecovery(t, st, ctx, workspace)
	runNeverRecovered(t, st, ctx, workspace)
	runWindowRollover(t, st, ctx, workspace)
	runOperatorRedispatchReset(t, st, ctx, workspace)
	runQueuePositionAndIdentity(t, st, ctx, workspace)
	runPolicyChangeOnNextTick(t, st, ctx, workspace)
	runConcurrentDueTicks(t, st, ctx, workspace)
	if _, err := st.ApplyWorkOrderClock(ctx, taskops.TaskLease{}, "missing", time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("non-command lease err=%v", err)
	}
}

func runQueuedExpiredRecovery(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "lease", ClientToken: "lease-token", Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	tick := time.Now().UTC().Add(time.Second)
	if _, err := taskops.New(st).TickOrderClock(ctx, tick); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, id)
	if err != nil || scheduled.State != core.WorkOrderQueued || scheduled.LastAttemptOutcome != core.WorkOrderOutcomeExpired || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("expired schedule=%+v err=%v", scheduled, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, id)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != 1 || recovered.LastAttemptOutcome != "" {
		t.Fatalf("expired recover=%+v err=%v", recovered, err)
	}
}

func runQueuedRetryLimitRecovery(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	claimAndRelease(t, st, ctx, id, "retry", core.WorkOrderRelease{SessionID: "retry", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "boom", FailureDetail: "once", AutomaticRetryLimit: 1, InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
	time.Sleep(time.Millisecond)
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "retry-2", ClientToken: "retry-2", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	released, err := ReleaseWorkerClaim(ctx, st, id, "worker", core.WorkOrderRelease{SessionID: "retry-2", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "boom-2", FailureDetail: "twice", AutomaticRetryLimit: 1})
	if err != nil || released.RetrySuppressionReason != core.AttemptRetriesExhaustedReason {
		t.Fatalf("retry limit=%+v err=%v", released, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, id)
	if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("retry-limit schedule=%+v err=%v", scheduled, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, id)
	if err != nil || recovered.AutomaticRecoveryCount != 1 || recovered.RetrySuppressed {
		t.Fatalf("retry-limit recover=%+v err=%v", recovered, err)
	}
}

func runNeverRecovered(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	live := seedRunningOrder(t, st, ctx, workspace)
	if _, err := ClaimWorkOrder(ctx, st, live, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "live", ClientToken: "live-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	assertUnscheduled(t, st, ctx, live, time.Now().UTC().Add(time.Minute))

	identical := seedRunningOrder(t, st, ctx, workspace)
	claimAndRelease(t, st, ctx, identical, "same", core.WorkOrderRelease{SessionID: "same", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "same", FailureDetail: "same-output", AutomaticRetryLimit: 3, InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
	time.Sleep(time.Millisecond)
	claimAndRelease(t, st, ctx, identical, "same-2", core.WorkOrderRelease{SessionID: "same-2", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "same", FailureDetail: "same-output", AutomaticRetryLimit: 3})
	got, err := st.GetWorkOrder(ctx, identical)
	if err != nil || got.RetrySuppressionReason != core.IdenticalFailureSuppressionReason {
		t.Fatalf("identical=%+v err=%v", got, err)
	}
	assertUnscheduled(t, st, ctx, identical, time.Now().UTC())

	checkpoint := seedRunningOrder(t, st, ctx, workspace)
	if _, err = ClaimWorkOrder(ctx, st, checkpoint, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "checkpoint", ClientToken: "checkpoint-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReleaseWorkerClaim(ctx, st, checkpoint, "worker", core.WorkOrderRelease{SessionID: "checkpoint", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "hold"}}); err != nil {
		t.Fatal(err)
	}
	assertUnscheduled(t, st, ctx, checkpoint, time.Now().UTC())

	revision := seedRunningOrder(t, st, ctx, workspace)
	if _, err = ClaimWorkOrder(ctx, st, revision, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "revision", ClientToken: "revision-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReleaseWorkerClaim(ctx, st, revision, "worker", core.WorkOrderRelease{SessionID: "revision", Reason: core.WorkOrderReleaseReasonPlanRevisionRequested, Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}
	assertUnscheduled(t, st, ctx, revision, time.Now().UTC())

	ordinary := seedRunningOrder(t, st, ctx, workspace)
	if _, err = ClaimWorkOrder(ctx, st, ordinary, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "ordinary", ClientToken: "ordinary-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReleaseWorkerClaim(ctx, st, ordinary, "worker", core.WorkOrderRelease{SessionID: "ordinary", Reason: "operator release", Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}
	assertUnscheduled(t, st, ctx, ordinary, time.Now().UTC())

	awaiting := core.NewTaskID()
	now := time.Now().UTC()
	if err = st.CreateTask(ctx, core.Task{ID: awaiting, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + awaiting, State: core.TaskAwaiting, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	awaitingOrder := awaiting + "-implement"
	if err = st.CreateJob(ctx, core.Job{ID: awaitingOrder, TaskID: awaiting, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = CreateWorkOrder(ctx, st, core.WorkOrder{ID: awaitingOrder, TaskID: awaiting, JobID: awaitingOrder, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Minute), QueueDeadline: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	assertUnscheduled(t, st, ctx, awaitingOrder, now)

	older := seedRunningOrder(t, st, ctx, workspace)
	if _, err = ClaimWorkOrder(ctx, st, older, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "older", ClientToken: "older-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	successor := older + "-successor"
	taskID := older[:len(older)-len("-implement")]
	if err = st.CreateJob(ctx, core.Job{ID: successor, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = CreateWorkOrder(ctx, st, core.WorkOrder{ID: successor, TaskID: taskID, JobID: successor, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	refused, err := st.GetWorkOrder(ctx, older)
	if err != nil || refused.AutomaticRecoveryCount != 0 || !refused.NextAutomaticRecoveryAt.IsZero() || refused.State == core.WorkOrderQueued {
		t.Fatalf("successor refusal=%+v err=%v", refused, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	still, err := st.GetWorkOrder(ctx, older)
	if err != nil || still.AutomaticRecoveryCount != 0 || still.State == core.WorkOrderQueued {
		t.Fatalf("successor recovered=%+v err=%v", still, err)
	}
}

func runWindowRollover(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "first", ClientToken: "first-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	recoverAutomatically(t, st, ctx, id)
	first, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "rollover", ClientToken: "rollover-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	later := first.AutomaticRecoveryWindowStartedAt.Add(24*time.Hour + 2*time.Minute)
	if _, err = taskops.New(st).TickOrderClock(ctx, later); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, id)
	if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("rollover schedule=%+v err=%v", scheduled, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	rolled, err := st.GetWorkOrder(ctx, id)
	if err != nil || rolled.AutomaticRecoveryCount != 1 {
		t.Fatalf("rollover=%+v err=%v", rolled, err)
	}
}

func runOperatorRedispatchReset(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	restoreQueueTimeout := setWorkOrderQueueTimeout(t, st, ctx, time.Minute)
	defer restoreQueueTimeout()
	id := seedStaleOrder(t, st, ctx, workspace)
	at := time.Now().UTC()
	for range 2 {
		if _, err := taskops.New(st).TickOrderClock(ctx, at); err != nil {
			t.Fatal(err)
		}
		scheduled, err := st.GetWorkOrder(ctx, id)
		if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
			t.Fatalf("stale schedule=%+v err=%v", scheduled, err)
		}
		if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
			t.Fatal(err)
		}
		current, err := st.GetWorkOrder(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		at = current.QueueDeadline.Add(time.Second)
	}
	if _, err := taskops.New(st).TickOrderClock(ctx, at); err != nil {
		t.Fatal(err)
	}
	exhausted, err := st.GetWorkOrder(ctx, id)
	if err != nil || exhausted.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason {
		t.Fatalf("stale exhausted=%+v err=%v", exhausted, err)
	}
	events, err := st.ListEvents(ctx, exhausted.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	exhaustedEvents := 0
	for _, event := range events {
		if event.Kind != "work_order.recovery_exhausted" {
			continue
		}
		exhaustedEvents++
		var payload struct {
			Reason string `json:"reason"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil || payload.Reason != core.AutomaticRecoveryExhaustedReason {
			t.Fatalf("exhausted event=%s err=%v", event.Payload, err)
		}
	}
	if exhaustedEvents != 1 {
		t.Fatalf("exhausted events=%d", exhaustedEvents)
	}
	reset, err := RedispatchWorkOrder(ctx, st, id, time.Hour)
	if err != nil || reset.AutomaticRecoveryCount != 0 || reset.RetrySuppressed {
		t.Fatalf("operator redispatch=%+v err=%v", reset, err)
	}
	if _, err = ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "after-reset", ClientToken: "after-reset-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	recovered := recoverAutomatically(t, st, ctx, id)
	if recovered.AutomaticRecoveryCount != 1 {
		t.Fatalf("recovery after redispatch=%+v", recovered)
	}
}

func setWorkOrderQueueTimeout(t *testing.T, st store.Store, ctx context.Context, timeout time.Duration) func() {
	t.Helper()
	updater, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
		UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := updater.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := current.Document.WorkOrderQueueTimeoutText
	write := func(version int64, text string, recovery config.RecoveryPolicy) {
		t.Helper()
		next := &config.Config{Workspace: current.Document.Workspace, MaxBounces: current.Document.MaxBounces, WorkOrderQueueTimeoutText: text, Recovery: recovery}
		if _, err := updater.UpdateWorkspaceConfig(ctx, version, next); err != nil {
			t.Fatal(err)
		}
	}
	write(current.Version, timeout.String(), current.Document.Recovery)
	return func() {
		record, err := updater.WorkspaceConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		write(record.Version, previous, record.Document.Recovery)
	}
}

func runQueuePositionAndIdentity(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now, SetupContract: config.ExecutionSetup{Name: "kept"}}); err != nil {
		t.Fatal(err)
	}
	waiting := taskID + "-waiting"
	moving := taskID + "-moving"
	for _, id := range []string{waiting, moving} {
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, HeadSHA: "head-kept", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ClaimWorkOrder(ctx, st, moving, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "moving", ClientToken: "moving-token", Lease: time.Minute, ExecutionTimeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.GetWorkOrder(ctx, moving)
	if err != nil {
		t.Fatal(err)
	}
	recovered := recoverAutomatically(t, st, ctx, moving)
	waiter, err := st.GetWorkOrder(ctx, waiting)
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.QueueEnteredAt.Before(waiter.QueueEnteredAt) || recovered.Stage != core.StageImplement || recovered.HeadSHA != "head-kept" || recovered.OperatorDirection != "" || recovered.ExecutionTimeoutText != claimed.ExecutionTimeoutText {
		t.Fatalf("identity recovered=%+v waiting=%+v claimedTimeout=%s", recovered, waiter, claimed.ExecutionTimeoutText)
	}
	if after.SetupContract.Name != before.SetupContract.Name || after.SetupContract.Name != "kept" {
		t.Fatalf("setup contract before=%+v after=%+v", before.SetupContract, after.SetupContract)
	}
	listed, err := st.GetWorkOrder(ctx, moving)
	if err != nil || listed.AutomaticRecoveryCount != recovered.AutomaticRecoveryCount || !listed.AutomaticRecoveryWindowStartedAt.Equal(recovered.AutomaticRecoveryWindowStartedAt) {
		t.Fatalf("ledger round-trip=%+v err=%v", listed, err)
	}
}

func runPolicyChangeOnNextTick(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	updater, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
		UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := updater.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := &config.Config{Workspace: current.Document.Workspace, MaxBounces: current.Document.MaxBounces, WorkOrderQueueTimeoutText: current.Document.WorkOrderQueueTimeoutText, Recovery: config.RecoveryPolicy{AutomaticRecoveries: 0, WindowText: "24h"}}
	if _, err = updater.UpdateWorkspaceConfig(ctx, current.Version, next); err != nil {
		t.Fatal(err)
	}
	id := seedRunningOrder(t, st, ctx, workspace)
	if _, err = ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "policy", ClientToken: "policy-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	stopped, err := st.GetWorkOrder(ctx, id)
	if err != nil || stopped.State != core.WorkOrderTimedOut || !stopped.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("disabled policy=%+v err=%v", stopped, err)
	}
	restore := next
	restore.Recovery = config.DefaultRecoveryPolicy()
	record, err := updater.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = updater.UpdateWorkspaceConfig(ctx, record.Version, restore); err != nil {
		t.Fatal(err)
	}
}

func runConcurrentDueTicks(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "race", ClientToken: "race-token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	tick := time.Now().UTC().Add(2 * time.Minute)
	if _, err := taskops.New(st).TickOrderClock(ctx, tick); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Add(2)
	for range 2 {
		go func() {
			defer group.Done()
			_, _ = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt)
		}()
	}
	group.Wait()
	recovered, err := st.GetWorkOrder(ctx, id)
	if err != nil || recovered.AutomaticRecoveryCount != 1 || recovered.State != core.WorkOrderQueued {
		t.Fatalf("concurrent=%+v err=%v", recovered, err)
	}
}

func seedRunningOrder(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := taskID + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedStaleOrder(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := taskID + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Minute), QueueDeadline: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	return id
}

func recoverAutomatically(t *testing.T, st store.Store, ctx context.Context, id string) core.WorkOrder {
	t.Helper()
	if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, id)
	if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("schedule %s=%+v err=%v", id, scheduled, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return recovered
}

func claimAndRelease(t *testing.T, st store.Store, ctx context.Context, id, token string, release core.WorkOrderRelease) {
	t.Helper()
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: release.SessionID, ClientToken: token, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseWorkerClaim(ctx, st, id, "worker", release); err != nil {
		t.Fatal(err)
	}
}

func assertUnscheduled(t *testing.T, st store.Store, ctx context.Context, id string, now time.Time) {
	t.Helper()
	before, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, now); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetWorkOrder(ctx, id)
	if err != nil || after.AutomaticRecoveryCount != before.AutomaticRecoveryCount || !after.NextAutomaticRecoveryAt.Equal(before.NextAutomaticRecoveryAt) {
		t.Fatalf("scheduled unexpectedly before=%+v after=%+v err=%v", before, after, err)
	}
}
