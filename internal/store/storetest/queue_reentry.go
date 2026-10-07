package storetest

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// legacyExecutionPins stamps the harness, model, and effort pins that
// pre-DEC-23 dispatch persisted on an order.
func legacyExecutionPins(order *core.WorkOrder) {
	order.RequiredHarness, order.RequiredModel, order.RequiredEffort = "legacy-harness", "legacy-model", "high"
	order.RequiredHarnessConfig = &core.HarnessSnapshot{Name: "legacy-harness", Command: []string{"legacy", "exec", "{prompt}"}, Effort: "high", EffortArgv: []string{"--effort", "high"}}
}

func requireExecutionPins(t *testing.T, label string, order core.WorkOrder) {
	t.Helper()
	if order.RequiredHarness != "legacy-harness" || order.RequiredModel != "legacy-model" || order.RequiredEffort != "high" ||
		order.RequiredHarnessConfig == nil || order.RequiredHarnessConfig.Name != "legacy-harness" {
		t.Fatalf("%s: fixture did not persist legacy execution pins: harness=%q model=%q effort=%q snapshot=%+v",
			label, order.RequiredHarness, order.RequiredModel, order.RequiredEffort, order.RequiredHarnessConfig)
	}
}

func requireNoExecutionPins(t *testing.T, label string, order core.WorkOrder) {
	t.Helper()
	if order.RequiredHarness != "" || order.RequiredModel != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
		t.Fatalf("%s: queue re-entry kept execution pins: harness=%q model=%q effort=%q snapshot=%+v",
			label, order.RequiredHarness, order.RequiredModel, order.RequiredEffort, order.RequiredHarnessConfig)
	}
}

// requireStoredWithoutPins checks both the command result and the persisted
// row, so a backend cannot clear only its returned copy.
func requireStoredWithoutPins(t *testing.T, x Fixture, label string, returned core.WorkOrder) core.WorkOrder {
	t.Helper()
	if returned.State != core.WorkOrderQueued {
		t.Fatalf("%s: state=%s, want queued", label, returned.State)
	}
	requireNoExecutionPins(t, label+" result", returned)
	stored, err := x.Backend.GetWorkOrder(x.Context, returned.ID)
	requireOK(t, err)
	requireNoExecutionPins(t, label+" stored", stored)
	return stored
}

func newPinnedImplementOrder(t *testing.T, x Fixture) core.WorkOrder {
	t.Helper()
	task := newAggregateTask(t, x)
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	legacyExecutionPins(&order)
	created, err := CreateStageWorkOrder(x.Context, x.Backend, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("pinned order was not created")
	}
	stored, err := x.Backend.GetWorkOrder(x.Context, order.ID)
	requireOK(t, err)
	requireExecutionPins(t, "created implement order", stored)
	return stored
}

// retryableRelease returns a child-failure release whose retry delay has
// already elapsed, so the released order is immediately claimable again.
func retryableRelease(session string) core.WorkOrderRelease {
	return core.WorkOrderRelease{SessionID: session, Reason: "machine handoff", Outcome: core.WorkOrderOutcomeChildFailure, FailureDetail: "harness exited",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond, AutomaticRetryLimit: 3}
}

// runQueueReentryPins proves that every queue re-entry command clears legacy
// harness, model, and effort pins while keeping review round and seat, and
// that the next claim takes its execution identity from the claiming machine
// (req-worker AC-2.2, AC-2.3; DEC-56).
func runQueueReentryPins(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	t.Run("ReleaseThenNextMachineClaims", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "first-machine", ClientToken: "first", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		released, err := ReleaseWorkerClaim(ctx, st, order.ID, "worker-a", retryableRelease("first-machine"))
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "release", released)
		time.Sleep(time.Millisecond)
		next, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "next-machine", ClientToken: "next", ClaimantID: "worker-b", WorkerID: "worker-b", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		identity := core.WorkOrderClaimIdentity{WorkerID: "worker-b", ClaimantID: "worker-b", SessionID: "next-machine"}
		// The continuation harness falls back to the order's pinned harness
		// when the claim names no agent; a cleared pin admits the next
		// machine's own harness.
		if _, err = st.RecordWorkOrderContinuation(ctx, order.ID, identity, core.WorkOrderContinuation{SessionID: "native", AttemptID: next.AttemptID, Harness: "next-harness", LaunchEnvironment: "test"}); err != nil {
			t.Fatalf("next machine harness refused after re-entry: %v", err)
		}
	})
	t.Run("LeaseExpiry", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "expiring", ClientToken: "expiring", Lease: time.Nanosecond, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		time.Sleep(time.Millisecond)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		expired, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
			t.Fatalf("lease expiry outcome=%q", expired.LastAttemptOutcome)
		}
		requireStoredWithoutPins(t, x, "lease expiry", expired)
	})
	t.Run("StaleRedispatch", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
			return st.ApplyWorkOrderClock(ctx, lease, order.TaskID, order.QueueDeadline.Add(time.Second))
		})
		requireOK(t, err)
		stale, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if stale.State != core.WorkOrderStale {
			t.Fatalf("clock state=%s, want stale", stale.State)
		}
		redispatched, err := RedispatchWorkOrder(ctx, st, order.ID, time.Hour)
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "stale redispatch", redispatched)
	})
	t.Run("RecoveryAfterTimeout", func(t *testing.T) {
		for _, refreeze := range []*store.RecoveryRefreeze{nil, {ExecutionTimeoutText: "2h"}} {
			order := newPinnedImplementOrder(t, x)
			_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "timing-out", ClientToken: "timing-out", Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
			requireOK(t, err)
			time.Sleep(time.Millisecond)
			_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
			requireOK(t, err)
			timedOut, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			if timedOut.State != core.WorkOrderTimedOut {
				t.Fatalf("clock state=%s, want timed_out", timedOut.State)
			}
			var refreezes []*store.RecoveryRefreeze
			if refreeze != nil {
				refreezes = append(refreezes, refreeze)
			}
			recovered, err := RecoverWorkOrder(ctx, st, order.ID, "recover-"+core.NewTaskID(), time.Hour, refreezes...)
			requireOK(t, err)
			stored := requireStoredWithoutPins(t, x, "recovery", recovered)
			if refreeze != nil && stored.ExecutionTimeoutText != "2h" {
				t.Fatalf("recovery refreeze timeout=%q", stored.ExecutionTimeoutText)
			}
		}
	})
	t.Run("Preempt", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "preempted", ClientToken: "preempted", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		result, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
			return st.PreemptWorkOrderCommand(ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: order.ID, RequestID: "preempt-" + core.NewTaskID(), Reason: "fixture preempt"})
		})
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "preempt", result.WorkOrder)
	})
	t.Run("ReviewSeatRequeue", func(t *testing.T) {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		for i := range orders {
			legacyExecutionPins(&orders[i])
		}
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		seat := orders[1]
		stored, err := st.GetWorkOrder(ctx, seat.ID)
		requireOK(t, err)
		requireExecutionPins(t, "created review seat", stored)
		// The legacy pin is still enforced before re-entry: a worker with a
		// different model cannot claim the pinned seat.
		if _, err = ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "wrong-model", ClientToken: "wrong-model", ClaimantID: "worker-a", WorkerID: "worker-a", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour}); err == nil {
			t.Fatal("pinned review seat accepted a different worker model before re-entry")
		}
		_, err = ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "seat-two-first", ClientToken: "seat-two-first", ClaimantID: "worker-a", WorkerID: "worker-a", Model: "legacy-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		released, err := ReleaseWorkerClaim(ctx, st, seat.ID, "worker-a", retryableRelease("seat-two-first"))
		requireOK(t, err)
		requeued := requireStoredWithoutPins(t, x, "review seat release", released)
		if released.ReviewRound != 1 || released.ReviewSeat != 2 || requeued.ReviewRound != 1 || requeued.ReviewSeat != 2 {
			t.Fatalf("review seat identity changed: result=%d/%d stored=%d/%d", released.ReviewRound, released.ReviewSeat, requeued.ReviewRound, requeued.ReviewSeat)
		}
		time.Sleep(time.Millisecond)
		next, err := ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "seat-two-next", ClientToken: "seat-two-next", ClaimantID: "worker-b", WorkerID: "worker-b", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatalf("next machine model refused after re-entry: %v", err)
		}
		if next.ReviewRound != 1 || next.ReviewSeat != 2 || next.Model != "next-model" {
			t.Fatalf("next claim seat=%d/%d model=%q", next.ReviewRound, next.ReviewSeat, next.Model)
		}
		other, err := st.GetWorkOrder(ctx, orders[0].ID)
		requireOK(t, err)
		if other.ReviewSeat != 1 || other.State != core.WorkOrderQueued {
			t.Fatalf("sibling seat changed: seat=%d state=%s", other.ReviewSeat, other.State)
		}
	})
	t.Run("InterruptedReviewRecovery", func(t *testing.T) {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		for i := range orders {
			legacyExecutionPins(&orders[i])
		}
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		first.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
		_, err = ClaimWorkOrder(ctx, st, orders[1].ID, core.WorkOrderClaim{SessionID: "seat-two", ClientToken: "two", Lease: time.Nanosecond, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		time.Sleep(time.Millisecond)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		result, err := RecoverInterruptedReviewRound(ctx, st, store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: "recover-" + core.NewTaskID(), Round: 1}, time.Hour)
		requireOK(t, err)
		if len(result.RecoveredOrders) != 1 || result.RecoveredOrders[0].ID != orders[1].ID {
			t.Fatalf("recovered orders=%+v", result.RecoveredOrders)
		}
		recovered := requireStoredWithoutPins(t, x, "interrupted review recovery", result.RecoveredOrders[0])
		if recovered.ReviewRound != 1 || recovered.ReviewSeat != 2 || recovered.RetrySuppressed {
			t.Fatalf("recovered seat=%d/%d suppressed=%v", recovered.ReviewRound, recovered.ReviewSeat, recovered.RetrySuppressed)
		}
		retained, err := st.GetWorkOrder(ctx, orders[0].ID)
		requireOK(t, err)
		if retained.State != core.WorkOrderCompleted || retained.ReviewSeat != 1 {
			t.Fatalf("completed seat changed: state=%s seat=%d", retained.State, retained.ReviewSeat)
		}
	})
	t.Run("ClaimedAttemptKeepsSnapshot", func(t *testing.T) {
		// Active-attempt freezing is unchanged: renewal does not clear the
		// pins of an attempt that is still running.
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "running", ClientToken: "running", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		renewed, err := RenewWorkerClaim(ctx, st, order.ID, "worker-a", "running", 2*time.Minute)
		requireOK(t, err)
		if renewed.State != core.WorkOrderClaimed {
			t.Fatalf("renewed state=%s", renewed.State)
		}
		requireExecutionPins(t, "claimed attempt", renewed)
	})
}
