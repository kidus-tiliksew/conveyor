package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestOverlappingRowsKeepEarlierWinner(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	order := core.WorkOrder{ID: "o", State: core.WorkOrderQueued, BlockingTaskIDs: []string{"dep"}}
	cases := []struct {
		name string
		in   Input
		kind string
	}{
		{"rows 8 and 9", Input{Task: core.Task{State: core.TaskApproved}, ClaimWaiting: []ProposalRef{{Tier: "requirement", ID: "REQ-1", Version: 1}}, Now: now}, KindMerging},
		{"rows 13 and 14", Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, UnsatisfiableTaskIDs: []string{"dep"}}}, Markers: Markers{ReviewRecovery: true}, Now: now}, KindReviewRecovery},
		{"rows 16 and 17", Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{order}, Recovery: map[string]AutomaticRecovery{"o": {NextAt: &later}}, Now: now}, KindBlocked},
	}
	for _, tc := range cases {
		got := Project(tc.in)
		if got.Kind != tc.kind {
			t.Fatalf("%s kind=%s", tc.name, got.Kind)
		}
	}
}
