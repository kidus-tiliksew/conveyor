package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestFoldActivityValidationTablesBindsEveryField(t *testing.T) {
	base := []ActivityTableSignature{{Table: "tasks", Rows: 2, Sum: "17"}, {Table: "work_orders", Rows: 1, Sum: "-4"}}
	fold := FoldActivityValidationTables(base)
	if again := FoldActivityValidationTables(append([]ActivityTableSignature(nil), base...)); again != fold {
		t.Fatal("fold is not deterministic")
	}
	for name, changed := range map[string][]ActivityTableSignature{
		"rows":  {{Table: "tasks", Rows: 3, Sum: "17"}, base[1]},
		"sum":   {{Table: "tasks", Rows: 2, Sum: "18"}, base[1]},
		"table": {{Table: "jobs", Rows: 2, Sum: "17"}, base[1]},
		"order": {base[1], base[0]},
		// Without separators, "1"+"7" and "17"+"" would collide.
		"boundary": {{Table: "tasks", Rows: 2, Sum: "1"}, {Table: "7work_orders", Rows: 1, Sum: "-4"}},
	} {
		if FoldActivityValidationTables(changed) == fold {
			t.Fatalf("changing %s left the signature unchanged", name)
		}
	}
}

func TestActivityReviewLeaseHorizonDecodesLifecycleLeases(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	payload := func(value any) json.RawMessage { body, _ := json.Marshal(value); return body }
	review := core.WorkOrder{ID: "review-1", JobID: "review-1", Stage: core.StageReview, State: core.WorkOrderQueued}
	events := []core.Event{
		{JobID: "review-1", Kind: "work_order.claimed", Payload: payload(map[string]any{"lease_expires_at": now.Add(time.Minute)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: payload(map[string]any{"lease_expires_at": now.Add(30 * time.Second)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: payload(map[string]any{"lease_expires_at": now.Add(-time.Second)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: json.RawMessage(`{"lease_expires_at":"not a time"}`)},
		{JobID: "other", Kind: "work_order.claimed", Payload: payload(map[string]any{"lease_expires_at": now.Add(time.Second)})},
	}
	if got := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("horizon = %s, want the earliest future lease", got)
	}
	for name, order := range map[string]core.WorkOrder{
		"implement order": {ID: "review-1", JobID: "review-1", Stage: core.StageImplement, State: core.WorkOrderQueued},
		"terminal review": {ID: "review-1", JobID: "review-1", Stage: core.StageReview, State: core.WorkOrderCompleted},
	} {
		if got := ActivityReviewLeaseHorizon([]core.WorkOrder{order}, events, now); !got.IsZero() {
			t.Fatalf("%s horizon = %s, want zero", name, got)
		}
	}
	if got := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events[2:4], now); !got.IsZero() {
		t.Fatalf("past and malformed leases horizon = %s, want zero", got)
	}
}

func TestPassedWorkOrderDeadlinesCountsEveryTimeField(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	order := core.WorkOrder{
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, QueueDeadline: now.Add(time.Second),
		LeaseExpiresAt: now.Add(-time.Second), NextRetryAt: now.Add(time.Minute), NextAutomaticRecoveryAt: now.Add(-time.Minute),
	}
	if got := passedWorkOrderDeadlines(order, now); got != 4 {
		t.Fatalf("passed = %d, want created, updated (at now), lease, and recovery", got)
	}
	if got := passedWorkOrderDeadlines(order, now.Add(time.Minute)); got != 6 {
		t.Fatalf("passed a minute later = %d, want 6", got)
	}
	if got := passedWorkOrderDeadlines(core.WorkOrder{}, now); got != 0 {
		t.Fatalf("zero times passed = %d", got)
	}
}
