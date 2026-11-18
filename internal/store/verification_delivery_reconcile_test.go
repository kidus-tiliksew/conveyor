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
		{ID: "older-failed", WorkspaceID: "ws", Repository: "o/r3", TaskID: "task-3", PullRequestNumber: 3, Generation: 1, State: "failed", ErrorClass: "forge", NextAttemptAt: &past, CreatedAt: now, UpdatedAt: now},
		{ID: "older-unscheduled", WorkspaceID: "ws", Repository: "o/r3", TaskID: "task-3", PullRequestNumber: 3, Generation: 2, State: "failed", ErrorClass: "forge", CreatedAt: now, UpdatedAt: now},
		{ID: "older-retrying", WorkspaceID: "ws", Repository: "o/r3", TaskID: "task-3", PullRequestNumber: 3, Generation: 3, State: "retrying", CreatedAt: now, UpdatedAt: now},
		{ID: "newest", WorkspaceID: "ws", Repository: "o/r3", TaskID: "task-3", PullRequestNumber: 3, Generation: 4, State: "published", CreatedAt: now, UpdatedAt: now},
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
	// Older failed and retrying generations settle, with or without a retry
	// time, and leave the due window (component-verification-publication
	// invariant 27).
	for _, id := range []string{"older-failed", "older-unscheduled", "older-retrying"} {
		if got := st.verificationDeliveries["ws\x00"+id]; got.State != "superseded" || got.NextAttemptAt != nil {
			t.Fatalf("%s=%+v", id, got)
		}
	}
	if got := st.verificationDeliveries["ws\x00newest"]; got.State != "published" {
		t.Fatalf("newest=%+v", got)
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
	// The discard comes after the row's last change, so it stopped the row.
	if _, err := st.log.Append(ctx, "ws", stream, 2, []eventlog.NewEvent{{Kind: logqueue.KindDiscarded, Payload: discarded, At: now.Add(time.Second)}}); err != nil {
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

// TestVerificationDeliveryDiscardStops pins the discard rule's inputs: only a
// row read as pending or retrying, only a discard after the final attempt,
// and only a discard strictly after the row's last change at microsecond
// precision (component-verification-publication invariant 26).
func TestVerificationDeliveryDiscardStops(t *testing.T) {
	changed := time.Date(2026, 11, 18, 10, 0, 0, 500, time.UTC)
	job := logqueue.Job{DiscardedAt: changed.Add(time.Second)}
	for _, tc := range []struct {
		name      string
		state     string
		at        time.Time
		discarded bool
		want      bool
	}{
		{"pending after", "pending", changed.Add(time.Second), true, true},
		{"retrying after", "retrying", changed.Add(time.Microsecond), true, true},
		{"failed keeps its retry", "failed", changed.Add(time.Second), true, false},
		{"not the final attempt", "retrying", changed.Add(time.Second), false, false},
		{"older discard", "retrying", changed.Add(-time.Second), true, false},
		{"same microsecond", "retrying", changed.Add(100), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job.DiscardedAt = tc.at
			read := core.VerificationDelivery{State: tc.state, UpdatedAt: changed}
			if got := VerificationDeliveryDiscardStops(read, job, tc.discarded); got != tc.want {
				t.Fatalf("stops=%t, want %t", got, tc.want)
			}
		})
	}
}

// TestVerificationDeliveryStreamKey proves a delivery stream maps to the
// pull-request key its rows carry, and another stream maps to none.
func TestVerificationDeliveryStreamKey(t *testing.T) {
	a := VerificationDeliveryArgs(core.VerificationDelivery{WorkspaceID: "ws", Repository: "owner/name", PullRequestNumber: 42})
	key, ok := VerificationDeliveryStreamKey(string(logqueue.StreamFor(a.Kind(), a.UniqueKey())))
	if !ok || key != VerificationDeliveryKey("owner/name", 42) {
		t.Fatalf("key=%q ok=%t", key, ok)
	}
	for _, stream := range []string{"job/other:owner/name#42", string(logqueue.StreamFor(a.Kind(), "owner/name")), string(logqueue.StreamFor(a.Kind(), "owner/name#0"))} {
		if _, ok := VerificationDeliveryStreamKey(stream); ok {
			t.Fatalf("stream %q mapped to a key", stream)
		}
	}
}
