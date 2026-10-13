package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestReviewAcceptAdmitted pins the DEC-64(8) admission set
// (component-task-lifecycle v13): A1-A6 take review.accept, N1-N3 do not.
func TestReviewAcceptAdmitted(t *testing.T) {
	tests := []struct {
		name    string
		verdict string
		policy  int
		gate    bool
		level   core.EscalationLevel
		kind    string
		want    bool
	}{
		{"A1 gate off first acceptance", "approve", 1, false, core.L2, "", true},
		{"A2 refresh gate on", "approve", 1, true, core.L2, "refresh", true},
		{"A3 refresh gate off", "approve", 1, false, core.L2, "refresh", true},
		{"A4 legacy L0 gate flag on", "approve", 0, true, core.L0, "", true},
		{"A5 legacy L0 gate off", "approve", 0, false, core.L0, "", true},
		{"A6 non-advancing refresh", "approve", 1, true, core.L2, "refresh", true},
		{"N1 first acceptance gate on", "approve", 1, true, core.L2, "", false},
		{"N2 legacy non-L0 gate off", "approve", 0, false, core.L2, "", false},
		{"N2 legacy L1 gate off", "approve", 0, false, core.L1, "", false},
		{"N3 refresh changes_requested", "changes_requested", 1, true, core.L2, "refresh", false},
		{"changes_requested gate off", "changes_requested", 1, false, core.L2, "", false},
		{"changes_requested legacy L0", "changes_requested", 0, false, core.L0, "", false},
		{"policy 2 gate off", "approve", 2, false, core.L1, "", true},
		{"policy 2 gate on L0", "approve", 2, true, core.L0, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := core.ReviewDecision{PolicyVersion: test.policy, MergeApproval: test.gate, Level: test.level, ReviewKind: test.kind}
			if got := ReviewAcceptAdmitted(decision, test.verdict); got != test.want {
				t.Fatalf("ReviewAcceptAdmitted = %v, want %v", got, test.want)
			}
		})
	}
}
