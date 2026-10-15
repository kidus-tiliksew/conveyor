package nextaction

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// A pending task-context proposal keeps the task in operator attention
// through its own row, with confirm and dismiss on the task-context routes
// under operate_gates alone (req-intake-and-triage AC-7.3;
// req-operator-next-action AC-1.4, AC-1.8; feature-operator-next-action NA-3,
// NA-4).
func TestContextProposalRowAndChoices(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	in := Input{
		Task: core.Task{ID: "task-1", State: core.TaskRunning},
		ContextProposals: []ContextProposalRef{
			{Kind: "system_design", TargetID: "component-work-orders"},
			{Kind: "requirement", TargetID: "req operator/next"},
			{Kind: "system_design", TargetID: "component-work-orders"},
		},
		Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true},
		Now:          now,
	}
	got := Project(in)
	if got.Kind != KindContextProposal || got.Actor != ActorOperator || !got.Judgment {
		t.Fatalf("next action = %s/%s judgment=%v", got.Actor, got.Kind, got.Judgment)
	}
	if got.Label != "Context proposal" || got.Summary != "A context proposal is waiting for a decision." ||
		got.Question != "Confirm or dismiss the proposed context?" || got.IfIgnored != "The proposal stays pending." {
		t.Fatalf("copy = %q / %q / %q / %q", got.Label, got.Summary, got.Question, got.IfIgnored)
	}
	if len(got.Details.PendingProposals) != 0 {
		t.Fatalf("context proposals leaked into pending_proposals: %+v", got.Details.PendingProposals)
	}
	gates := string(core.CapabilityOperateGates)
	want := []Choice{
		{ID: "confirm:task_context:requirement:req operator/next", Label: "Confirm context requirement req operator/next", Effect: "Attaches the proposed context to this task.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/requirement/req%20operator%2Fnext/confirm", Capability: gates, Allowed: true},
		{ID: "dismiss:task_context:requirement:req operator/next", Label: "Dismiss context requirement req operator/next", Effect: "Dismisses the proposed context.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/requirement/req%20operator%2Fnext/dismiss", Capability: gates, Allowed: true},
		{ID: "confirm:task_context:system_design:component-work-orders", Label: "Confirm context system_design component-work-orders", Effect: "Attaches the proposed context to this task.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/system_design/component-work-orders/confirm", Capability: gates, Allowed: true},
		{ID: "dismiss:task_context:system_design:component-work-orders", Label: "Dismiss context system_design component-work-orders", Effect: "Dismisses the proposed context.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/system_design/component-work-orders/dismiss", Capability: gates, Allowed: true},
	}
	if !reflect.DeepEqual(got.Choices, want) {
		t.Fatalf("choices = %+v\nwant %+v", got.Choices, want)
	}

	// confirm_documents grants no task-context act.
	in.Capabilities = map[core.Capability]bool{core.CapabilityConfirmDocuments: true, core.CapabilityRecoverWork: true}
	for _, choice := range Project(in).Choices {
		if choice.Allowed {
			t.Fatalf("choice %s allowed without operate_gates", choice.ID)
		}
	}
	// operate_gates grants no document act.
	documents := Project(Input{
		Task:         core.Task{ID: "task-1", State: core.TaskRunning},
		ClaimWaiting: []ProposalRef{{Tier: "system_design", ID: "component-work-orders", Version: 51}},
		Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true},
		Now:          now,
	})
	for _, choice := range documents.Choices {
		if choice.Allowed {
			t.Fatalf("document choice %s allowed with operate_gates alone", choice.ID)
		}
	}
}

// The context row sits after stalled and before blocked: every operator row
// above it wins and records it as secondary, and it wins over dependency,
// scheduled-recovery, agent, and factory rows (feature-operator-next-action
// NA-3).
func TestContextProposalPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	pending := []ContextProposalRef{{Kind: "requirement", TargetID: "req-x"}}
	withContext := func(in Input) Input {
		in.ContextProposals = pending
		return in
	}
	cases := []struct {
		name, kind, secondary string
		in                    Input
	}{
		{name: "claim wait over context", kind: KindDocumentProposal, secondary: KindContextProposal,
			in: withContext(Input{Task: core.Task{State: core.TaskRunning}, ClaimWaiting: []ProposalRef{{Tier: "requirement", ID: "req-y", Version: 2}}, Now: now})},
		{name: "agent question over context", kind: KindAgentQuestion, secondary: KindContextProposal,
			in: withContext(checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, []core.WorkOrderAuthorityConflictCitation{{DocumentID: "component-x", CitedVersion: 1}}, nil))},
		{name: "stalled over context", kind: KindStalled, secondary: KindContextProposal,
			in: withContext(timedOut(now))},
		{name: "context over blocked", kind: KindContextProposal, secondary: KindBlocked,
			in: withContext(Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, BlockingTaskIDs: []string{"dep"}}}, Now: now})},
		{name: "context over scheduled recovery", kind: KindContextProposal, secondary: KindRetrying,
			in: withContext(Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "retry", State: core.WorkOrderQueued}}, Recovery: map[string]AutomaticRecovery{"retry": {NextAt: &later}}, Now: now})},
		{name: "context over claimed work", kind: KindContextProposal, secondary: KindWorking,
			in: withContext(Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "c", State: core.WorkOrderClaimed}}, Now: now})},
		{name: "context over queued work", kind: KindContextProposal, secondary: KindQueued,
			in: withContext(Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, Claimable: true}}, Now: now})},
		{name: "merging over context", kind: KindMerging, secondary: KindContextProposal,
			in: withContext(Input{Task: core.Task{State: core.TaskApproved}, Now: now})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Kind != tc.kind {
				t.Fatalf("kind=%s want %s", got.Kind, tc.kind)
			}
			if !hasSecondary(got, tc.secondary) {
				t.Fatalf("secondary=%v want %s", got.Details.Secondary, tc.secondary)
			}
		})
	}
	without := Project(Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, Claimable: true}}, Now: now})
	if without.Kind != KindQueued || hasSecondary(without, KindContextProposal) {
		t.Fatalf("no pending context proposal: kind=%s secondary=%v", without.Kind, without.Details.Secondary)
	}
}
