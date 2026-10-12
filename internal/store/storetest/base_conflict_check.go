package storetest

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runBaseConflictCheck proves the pre-dispatch mergeability stamp is a
// workspace-scoped compare-and-set on every backend
// (feature-bounded-automatic-recovery AR-4; req-bounded-automatic-recovery AC-3.2).
func runBaseConflictCheck(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	before, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if !before.BaseConflictCheckedAt.IsZero() {
		t.Fatalf("new order already stamped: %s", before.BaseConflictCheckedAt)
	}

	if claimed, err := st.ClaimBaseConflictCheck(store.WithWorkspace(ctx, "foreign-"+x.Workspace), order.ID, time.Now().UTC()); err != nil || claimed {
		t.Fatalf("foreign workspace claim = %v, %v; want false, nil", claimed, err)
	}
	if claimed, err := st.ClaimBaseConflictCheck(ctx, "missing-order", time.Now().UTC()); err != nil || claimed {
		t.Fatalf("missing order claim = %v, %v; want false, nil", claimed, err)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	claimed, err := st.ClaimBaseConflictCheck(ctx, order.ID, at)
	requireOK(t, err)
	if !claimed {
		t.Fatal("first claim was refused")
	}
	again, err := st.ClaimBaseConflictCheck(ctx, order.ID, at.Add(time.Minute))
	requireOK(t, err)
	if again {
		t.Fatal("second claim on the same submission succeeded")
	}

	got, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if !got.BaseConflictCheckedAt.Equal(at) {
		t.Fatalf("GetWorkOrder base_conflict_checked_at = %s, want %s", got.BaseConflictCheckedAt, at)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("claim moved updated_at from %s to %s", before.UpdatedAt, got.UpdatedAt)
	}
	encoded, err := json.Marshal(got)
	requireOK(t, err)
	var wire struct {
		BaseConflictCheckedAt time.Time `json:"base_conflict_checked_at"`
	}
	requireOK(t, json.Unmarshal(encoded, &wire))
	if !wire.BaseConflictCheckedAt.Equal(at) {
		t.Fatalf("work-order JSON base_conflict_checked_at = %s, want %s", wire.BaseConflictCheckedAt, at)
	}
	listed, err := st.ListTaskWorkOrders(ctx, order.TaskID)
	requireOK(t, err)
	found := false
	for _, candidate := range listed {
		if candidate.ID == order.ID {
			found = true
			if !candidate.BaseConflictCheckedAt.Equal(at) {
				t.Fatalf("ListTaskWorkOrders base_conflict_checked_at = %s, want %s", candidate.BaseConflictCheckedAt, at)
			}
		}
	}
	if !found {
		t.Fatalf("order %s missing from its task listing", order.ID)
	}

	racing := newAggregateOrder(t, x)
	const contenders = 8
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, claimErr := st.ClaimBaseConflictCheck(ctx, racing.ID, time.Now().UTC())
			if claimErr != nil {
				t.Errorf("concurrent claim: %v", claimErr)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent claims won %d times, want exactly 1", wins)
	}
}
