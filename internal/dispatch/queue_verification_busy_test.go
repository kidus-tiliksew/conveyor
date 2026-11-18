package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// busyDeliveryStore reports a held delivery lock on its first passes.
type busyDeliveryStore struct {
	store.Backend
	busy   int
	passes int
}

func (s *busyDeliveryStore) ReconcileVerificationDeliveries(context.Context) error {
	s.passes++
	if s.busy > 0 {
		s.busy--
		return store.ErrVerificationDeliveryBusy
	}
	return nil
}

// TestOrderClockReturnsABusyDeliveryPass proves a delivery pass that skipped a
// held pull-request lock fails the order clock job, so the queue runs the
// clock, and with it the pass, again; the next fire that finds no held lock
// succeeds (component-verification-publication invariants 12 and 19).
func TestOrderClockReturnsABusyDeliveryPass(t *testing.T) {
	st := &busyDeliveryStore{Backend: store.NewVolatileBackend(), busy: 1}
	clock := &orderClockWorker{dispatcher: New(st, &config.Config{Workspace: "test"}, nil)}
	err := clock.Work(t.Context(), testJob(queue.OrderClockArgs{WorkspaceID: "test"}, 1, 1, 5))
	if !errors.Is(err, store.ErrVerificationDeliveryBusy) {
		t.Fatalf("order clock with a busy delivery pass returned %v, want the busy error", err)
	}
	if err := clock.Work(t.Context(), testJob(queue.OrderClockArgs{WorkspaceID: "test"}, 1, 2, 5)); err != nil {
		t.Fatalf("order clock retry: %v", err)
	}
	if st.passes != 2 {
		t.Fatalf("delivery passes=%d, want one per fire", st.passes)
	}
}
