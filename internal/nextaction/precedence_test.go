package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestOverlappingRowsKeepEarlierWinner(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	cases := []struct {
		name, kind, secondary string
		in                    Input
		absent                string
	}{
		{
			name: "rows 5 and 6", kind: KindMergeConflict, secondary: KindMergeExecution,
			in: conflictInput(true, now),
		},
		{
			name: "rows 8 and 9", kind: KindMerging, secondary: KindDocumentProposal,
			in: Input{Task: core.Task{State: core.TaskApproved}, ClaimWaiting: []ProposalRef{{Tier: "requirement", ID: "REQ-1", Version: 1}}, Now: now},
		},
		{
			name: "rows 9 and 10", kind: KindDocumentProposal, secondary: KindAgentQuestion,
			in: waitingAndQuestion(now),
		},
		{
			name: "rows 11 and 12", kind: KindVerificationDisposition, secondary: KindForgeFailure,
			in: Input{Task: core.Task{State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true, ForgeFailure: true}, Orders: []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify}}, Now: now},
		},
		{
			name: "rows 13 and 14", kind: KindReviewRecovery, secondary: KindDependencyUnsatisfiable,
			in: Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, UnsatisfiableTaskIDs: []string{"dep"}}}, Markers: Markers{ReviewRecovery: true}, Now: now},
		},
		{
			name: "rows 14 and 15", kind: KindDependencyUnsatisfiable, secondary: KindStalled,
			in: unsatisfiableAndSuppressed(now),
		},
		{
			name: "rows 15 and 17", kind: KindStalled, secondary: KindRetrying,
			in: Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "stalled", State: core.WorkOrderTimedOut}, {ID: "retry", State: core.WorkOrderQueued}}, Recovery: map[string]AutomaticRecovery{"retry": {NextAt: &later}}, Now: now},
		},
		{
			name: "rows 16 and 17", kind: KindBlocked, secondary: KindRetrying,
			in: Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, BlockingTaskIDs: []string{"dep"}}}, Recovery: map[string]AutomaticRecovery{"o": {NextAt: &later}}, Now: now},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Kind != tc.kind {
				t.Fatalf("kind=%s want %s", got.Kind, tc.kind)
			}
			if tc.secondary != "" && !hasSecondary(got, tc.secondary) {
				t.Fatalf("secondary=%v want %s", got.Details.Secondary, tc.secondary)
			}
			if tc.absent != "" && hasSecondary(got, tc.absent) {
				t.Fatalf("secondary=%v includes %s", got.Details.Secondary, tc.absent)
			}
		})
	}
}
