package store

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestLostRiskCompletionError: a completion whose conditional write changed no
// row answers ErrWorkOrderCancelled only when the order it reads again is
// cancelled, and ErrWorkOrderClaimLost for every other state
// (req-risk-based-review AC-12.2, AC-12.3).
func TestLostRiskCompletionError(t *testing.T) {
	for _, state := range []core.WorkOrderState{
		core.WorkOrderCancelled, core.WorkOrderQueued, core.WorkOrderClaimed, core.WorkOrderCompleted,
		core.WorkOrderStale, core.WorkOrderTimedOut, core.WorkOrderSubmitted,
	} {
		err := LostRiskCompletionError(state)
		want := ErrWorkOrderClaimLost
		if state == core.WorkOrderCancelled {
			want = ErrWorkOrderCancelled
		}
		if err != want {
			t.Errorf("state %s: err=%v, want %v", state, err, want)
		}
		if errors.Is(err, ErrWorkOrderCancelled) && errors.Is(err, ErrWorkOrderClaimLost) {
			t.Errorf("state %s: err=%v matches both sentinels", state, err)
		}
	}
}
