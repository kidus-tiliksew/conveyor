package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

func TestReconcileVerificationDeliveriesConvergesGenerations(t *testing.T) {
	st := NewVolatileBackend().(*volatileMemory)
	defer st.Close()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "ws")
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	st.mu.Lock()
	if st.verificationDeliveries == nil {
		st.verificationDeliveries = map[string]core.VerificationDelivery{}
	}
	st.putDeliveriesLocked([]core.VerificationDelivery{
		{ID: "old", WorkspaceID: "ws", Repository: "o/r", TaskID: "task", PullRequestNumber: 1, Generation: 1, State: "pending", CreatedAt: now, UpdatedAt: now},
		{ID: "new", WorkspaceID: "ws", Repository: "o/r", TaskID: "task", PullRequestNumber: 1, Generation: 2, State: "pending", CreatedAt: now, UpdatedAt: now},
		{ID: "mismatch", WorkspaceID: "ws", Repository: "o/r2", TaskID: "task-2", PullRequestNumber: 2, Generation: 1, State: "failed", ErrorClass: "head_mismatch", NextAttemptAt: &past, CreatedAt: now, UpdatedAt: now},
	})
	st.mu.Unlock()
	if err := st.ReconcileVerificationDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.verificationDeliveries["ws\x00old"].State != "superseded" {
		t.Fatalf("stale=%+v", st.verificationDeliveries["ws\x00old"])
	}
	if st.verificationDeliveries["ws\x00new"].State != "pending" {
		t.Fatalf("latest=%+v", st.verificationDeliveries["ws\x00new"])
	}
	if got := st.verificationDeliveries["ws\x00mismatch"]; got.State != "failed" || got.NextAttemptAt != nil {
		t.Fatalf("mismatch=%+v", got)
	}
}

func TestReconcileVerificationDeliveriesFailsDiscardedJob(t *testing.T) {
	st := NewVolatileBackend().(*volatileMemory)
	defer st.Close()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "ws")
	now := time.Now().UTC()
	d := core.VerificationDelivery{ID: "disc", WorkspaceID: "ws", Repository: "o/r", TaskID: "task", PullRequestNumber: 9, Generation: 1, State: "pending", CreatedAt: now, UpdatedAt: now}
	st.mu.Lock()
	st.putDeliveriesLocked([]core.VerificationDelivery{d})
	st.mu.Unlock()
	a := VerificationDeliveryArgs(d)
	stream := logqueue.StreamFor(a.Kind(), a.UniqueKey())
	if inserted, err := logqueue.Enqueue(ctx, st.log, "ws", a.Kind(), a.UniqueKey(), a, 1, now); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%t err=%v", inserted, err)
	}
	claimed, err := json.Marshal(jobindex.ClaimedPayload{Attempt: 1, Worker: "w", ClaimedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.log.Append(ctx, "ws", stream, 1, []eventlog.NewEvent{{Kind: logqueue.KindClaimed, Payload: claimed, At: now}}); err != nil {
		t.Fatal(err)
	}
	discarded, err := json.Marshal(jobindex.OutcomePayload{Attempt: 1, Error: "exhausted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.log.Append(ctx, "ws", stream, 2, []eventlog.NewEvent{{Kind: logqueue.KindDiscarded, Payload: discarded, At: now}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileVerificationDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	got := st.verificationDeliveries["ws\x00disc"]
	if got.State != "failed" || got.ErrorClass != "forge" || got.NextAttemptAt != nil {
		t.Fatalf("discarded delivery=%+v", got)
	}
}
