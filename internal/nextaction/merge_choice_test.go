package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestMergeApprovalOffersMergeOnlyAtMergeable pins feature-operator-next-action
// v6 row merge_approval (DEC-64(3); DEC-61): the server offers merge only at
// readiness MERGEABLE and otherwise refuses it naming the readiness.
func TestMergeApprovalOffersMergeOnlyAtMergeable(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		readiness *MergeReadiness
		want      string
	}{
		{name: "mergeable", readiness: &MergeReadiness{State: "MERGEABLE"}},
		{name: "conflicting", readiness: &MergeReadiness{State: "CONFLICTING"}, want: "merge readiness CONFLICTING"},
		{name: "stale", readiness: &MergeReadiness{State: "STALE"}, want: "merge readiness STALE"},
		{name: "unknown", readiness: &MergeReadiness{State: "UNKNOWN"}, want: "merge readiness UNKNOWN"},
		{name: "unread", want: "merge readiness UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gateInput(core.GateKindMergeApproval, now)
			in.Task.ID = "gate"
			in.MergeReadiness = tc.readiness
			in.Capabilities = map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityRequestChanges: true}
			na := Project(in)
			var merge *Choice
			for i := range na.Choices {
				if na.Choices[i].ID == "merge" {
					merge = &na.Choices[i]
				}
			}
			refusal := ""
			for _, r := range na.Details.Refusals {
				if r.Choice == "merge" {
					refusal = r.Text
				}
			}
			if tc.want == "" {
				if merge == nil || !merge.Allowed || merge.Path != "/v1/tasks/gate/merge" || merge.Method != "POST" || refusal != "" {
					t.Fatalf("merge=%+v refusal=%q", merge, refusal)
				}
			} else if merge != nil || refusal != tc.want {
				t.Fatalf("merge=%+v refusal=%q want %q", merge, refusal, tc.want)
			}
			approve := false
			for _, c := range na.Choices {
				approve = approve || (c.ID == "approve" && c.Label == "Approve only")
			}
			if !approve {
				t.Fatalf("approve only missing: %+v", na.Choices)
			}
		})
	}
}

// TestApprovedTaskMergeChoiceIgnoresGateReadiness keeps the approved-task
// merge execution choice unrefused by the gate readiness rule.
func TestApprovedTaskMergeChoiceIgnoresGateReadiness(t *testing.T) {
	if got := mergeAtGateRefusal(Input{Task: core.Task{State: core.TaskApproved}}); got != "" {
		t.Fatalf("refusal=%q", got)
	}
}
