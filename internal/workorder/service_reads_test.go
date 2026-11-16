package workorder

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// readCountingStore counts the work-order reads one service path makes and
// reports whether a snapshot-free read answered with an authority snapshot, so
// a test can prove an internal state check selects the lean read
// (component-work-orders, Snapshot-free fold reads).
type readCountingStore struct {
	store.Store
	full, lean        int
	leanWithSnapshots int
}

func (s *readCountingStore) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	s.full++
	return s.Store.GetWorkOrder(ctx, id)
}

func (s *readCountingStore) GetWorkOrderWithoutSnapshots(ctx context.Context, id string) (core.WorkOrder, error) {
	s.lean++
	order, err := s.Store.GetWorkOrderWithoutSnapshots(ctx, id)
	if order.GovernanceSnapshot != nil || len(order.ServedRequirementSnapshot) != 0 {
		s.leanWithSnapshots++
	}
	return order, err
}

// TestStateCheckReadsWithoutAuthoritySnapshots drives a session write through
// its admission check and proves the re-check reads the snapshot-free order
// while the same-session admission keeps the full read, because a verify
// order's pinned authority is read from it
// (component-work-orders, Snapshot-free fold reads).
func TestStateCheckReadsWithoutAuthoritySnapshots(t *testing.T) {
	t.Parallel()
	ctx, st, service, order := newLifecycleService(t, "snapshot-free")
	claimed, err := service.Claim(ctx, order.ID, core.WorkOrderClaim{
		SessionID: "session", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("snapshot-free"), Agent: "omp", Model: "deepseek",
	})
	if err != nil {
		t.Fatal(err)
	}
	counting := &readCountingStore{Store: st}
	service.Store = counting
	if _, err = service.Progress(ctx, claimed.ID, "session", "still working"); err != nil {
		t.Fatal(err)
	}
	if counting.full != 1 {
		t.Fatalf("progress made %d full work-order reads, want 1 for same-session admission", counting.full)
	}
	if counting.lean < 1 {
		t.Fatal("the state re-check read the full order, want the snapshot-free read")
	}
	if counting.leanWithSnapshots != 0 {
		t.Fatalf("%d snapshot-free reads answered with an authority snapshot, want 0", counting.leanWithSnapshots)
	}
}
