package workorder

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPreemptPreservesQueueTelemetryAndSignalsRevokedAttempt(t *testing.T) {
	ctx, st, service, order := newLifecycleService(t, "preempt")
	ctx = store.WithActor(store.WithWorkspace(ctx, "test"), store.Actor{ID: "operator-7", Role: core.ActorHuman})
	if _, err := st.SetTaskHold(ctx, order.TaskID, true); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{
		SessionID: "preempt-session", ClientToken: "secret", ClaimantID: "worker-7", WorkerID: "worker-7",
		Agent: "codex", Model: "gpt", Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed.AutomaticRetryCount = 2
	claimed.CostUSD = 1.25
	claimed.TokensIn = 120
	claimed.TokensOut = 45
	claimed.UsageReported = true
	claimed.SelfReported = true
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed); err != nil {
		t.Fatal(err)
	}

	result, err := service.Preempt(ctx, order.ID, "switch to the repaired setup", "preempt-request-1")
	if err != nil {
		t.Fatal(err)
	}
	got := result.WorkOrder
	if got.State != core.WorkOrderQueued || got.LastAttemptOutcome != core.WorkOrderOutcomePreempted ||
		got.LastAttemptID != claimed.AttemptID || got.SessionID != "" || got.WorkerID != "" || got.AttemptID != "" {
		t.Fatalf("preempted order=%+v", got)
	}
	if !got.QueueEnteredAt.Equal(claimed.QueueEnteredAt) || !got.QueueDeadline.Equal(claimed.QueueDeadline) ||
		got.AutomaticRetryCount != 2 || got.CostUSD != 1.25 || got.TokensIn != 120 || got.TokensOut != 45 ||
		!got.UsageReported || !got.SelfReported {
		t.Fatalf("preemption changed queue or telemetry: before=%+v after=%+v", claimed, got)
	}
	task, err := st.GetTask(ctx, order.TaskID)
	if err != nil || !task.Hold {
		t.Fatalf("held task=%+v err=%v", task, err)
	}
	if result.RevokedAttemptID != claimed.AttemptID || result.RevokedSessionID != "preempt-session" ||
		result.RevokedWorkerID != "worker-7" || result.GraceBound != "one renewal interval" {
		t.Fatalf("preempt result=%+v", result)
	}
	duplicate, err := service.Preempt(ctx, order.ID, "switch to the repaired setup", "preempt-request-1")
	if err != nil || duplicate.RevokedAttemptID != result.RevokedAttemptID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	if _, err = service.Preempt(ctx, order.ID, "different reason", "preempt-request-1"); !errors.Is(err, store.ErrWorkOrderPreemptConflict) {
		t.Fatalf("changed idempotency input err=%v", err)
	}
	if _, err = storetest.For(st).RenewWorkerClaim(ctx, order.ID, "worker-7", "preempt-session", time.Minute); !errors.Is(err, store.ErrWorkOrderPreempted) {
		t.Fatalf("revoked renewal err=%v", err)
	}
	checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: "preempt-session", AttemptID: claimed.AttemptID, TerminationReason: "work order was preempted by an operator", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PushResult: "pushed"}
	if created, checkpointErr := st.RecordWorkOrderAttemptCheckpoint(ctx, order.ID, "worker-7", checkpoint); checkpointErr != nil || !created {
		t.Fatalf("late preempt checkpoint created=%v err=%v", created, checkpointErr)
	}

	events, err := st.ListEvents(ctx, order.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var preempted int
	for _, event := range events {
		if event.Kind == "work_order.child_failed" || event.Kind == "work_order.stalled" {
			t.Fatalf("preempt emitted failure-shaped event %q", event.Kind)
		}
		if event.Kind != "work_order.preempted" {
			continue
		}
		preempted++
		var payload map[string]any
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if event.ActorID != "operator-7" || event.ActorRole != core.ActorHuman || payload["request_id"] != "preempt-request-1" ||
			payload["reason"] != "switch to the repaired setup" || payload["attempt_id"] != claimed.AttemptID || payload["session_id"] != "preempt-session" {
			t.Fatalf("preempt event=%+v payload=%v", event, payload)
		}
	}
	if preempted != 1 {
		t.Fatalf("preempt events=%d", preempted)
	}
	if violations := store.AuditLifecycleHistory(events); len(violations) != 0 {
		t.Fatalf("lifecycle audit violations=%+v", violations)
	}
}

func TestPreemptRetiresQueuedOrderIdempotently(t *testing.T) {
	ctx, st, service, order := newLifecycleService(t, "retire-queued")
	ctx = store.WithActor(store.WithWorkspace(ctx, "test"), store.Actor{ID: "operator", Role: core.ActorHuman})
	result, err := service.Preempt(ctx, order.ID, "retire zombie", "retire-queued-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkOrder.State != core.WorkOrderCancelled || result.WorkOrder.Claimable || result.RevokedAttemptID != "" {
		t.Fatalf("retired=%+v", result)
	}
	duplicate, err := service.Preempt(ctx, order.ID, "retire zombie", "retire-queued-1")
	if err != nil || duplicate.WorkOrder.State != core.WorkOrderCancelled {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	if _, err = service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "zombie", ClientToken: "secret"}); !errors.Is(err, store.ErrWorkOrderCancelled) {
		t.Fatalf("claim retired order err=%v", err)
	}
	if count, countErr := st.CountEvents(ctx, order.TaskID, "work_order.retired"); countErr != nil || count != 1 {
		t.Fatalf("retirement events=%d err=%v", count, countErr)
	}
}

func TestPreemptAfterDeadWorkerLeaseExpiryRetiresQueuedOrder(t *testing.T) {
	ctx, st, service, order := newLifecycleService(t, "preempt-dead")
	ctx = store.WithActor(store.WithWorkspace(ctx, "test"), store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{SessionID: "dead-session", ClientToken: "secret", WorkerID: "dead-worker", Lease: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := service.Preempt(ctx, order.ID, "worker is gone", "preempt-dead-1"); !errors.Is(err, store.ErrWorkOrderPreemptConflict) {
		t.Fatalf("initial expired preempt err=%v", err)
	}
	result, err := service.Preempt(ctx, order.ID, "worker is gone", "preempt-dead-1")
	if err != nil {
		t.Fatalf("queued retirement retry err=%v", err)
	}
	converged, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || converged.State != core.WorkOrderCancelled || converged.LastAttemptOutcome != core.WorkOrderOutcomeCancelled || !converged.RetrySuppressed || result.WorkOrder.State != core.WorkOrderCancelled {
		t.Fatalf("expired order=%+v err=%v", converged, err)
	}
	if count, countErr := st.CountEvents(ctx, order.TaskID, "work_order.preempted"); countErr != nil || count != 0 {
		t.Fatalf("preempt events=%d err=%v", count, countErr)
	}
	if count, countErr := st.CountEvents(ctx, order.TaskID, "work_order.retired"); countErr != nil || count != 1 {
		t.Fatalf("retirement events=%d err=%v", count, countErr)
	}
}
