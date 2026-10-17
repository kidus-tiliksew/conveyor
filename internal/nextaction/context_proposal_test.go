package nextaction

import (
	"encoding/json"
	"reflect"
	"strings"
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
			{Kind: "system_design", TargetID: "component-work-orders", TargetTitle: "Work orders", Source: "triage", Justification: "Claims change.", ProposedBy: "system:triage", CreatedAt: now.Add(-time.Hour)},
			{Kind: "requirement", TargetID: "req operator/next", Source: "planning", Justification: "Serves the inbox.", ProposedBy: "user:usr-1", CreatedAt: now.Add(-2 * time.Hour)},
			// A second record of one target with other metadata is the same
			// proposal: identity alone deduplicates.
			{Kind: "system_design", TargetID: "component-work-orders", TargetTitle: "Work orders (old)", Source: "operator", Justification: "Other.", CreatedAt: now},
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
		{ID: "confirm:task_context:requirement:req operator/next", Label: "Confirm proposed requirement", Effect: "Attaches the proposed context to this task.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/requirement/req%20operator%2Fnext/confirm", Capability: gates, Allowed: true},
		{ID: "dismiss:task_context:requirement:req operator/next", Label: "Dismiss proposed requirement", Effect: "Dismisses the proposed context.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/requirement/req%20operator%2Fnext/dismiss", Capability: gates, Allowed: true},
		{ID: "confirm:task_context:system_design:component-work-orders", Label: "Confirm Work orders (System Design)", Effect: "Attaches the proposed context to this task.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/system_design/component-work-orders/confirm", Capability: gates, Allowed: true},
		{ID: "dismiss:task_context:system_design:component-work-orders", Label: "Dismiss Work orders (System Design)", Effect: "Dismisses the proposed context.", Method: "POST", Path: "/v1/tasks/task-1/context/proposals/system_design/component-work-orders/dismiss", Capability: gates, Allowed: true},
	}
	if !reflect.DeepEqual(got.Choices, want) {
		t.Fatalf("choices = %+v\nwant %+v", got.Choices, want)
	}
	for _, choice := range got.Choices {
		if strings.Contains(choice.Label, "req operator/next") || strings.Contains(choice.Label, "component-work-orders") {
			t.Fatalf("label %q carries a target ID", choice.Label)
		}
	}
	// details.context_proposals states each entry in choice order with the
	// stored metadata (feature-operator-next-action NA-3, Context-proposal
	// presentation).
	wantDetails := []ContextProposalDetail{
		{Kind: "requirement", TargetID: "req operator/next", TierLabel: "requirement", Justification: "Serves the inbox.", Source: "planning", ProposedBy: "user:usr-1", CreatedAt: now.Add(-2 * time.Hour)},
		{Kind: "system_design", TargetID: "component-work-orders", TargetTitle: "Work orders", TierLabel: "System Design", Justification: "Claims change.", Source: "triage", ProposedBy: "system:triage", CreatedAt: now.Add(-time.Hour)},
	}
	if !reflect.DeepEqual(got.Details.ContextProposals, wantDetails) {
		t.Fatalf("details.context_proposals = %+v\nwant %+v", got.Details.ContextProposals, wantDetails)
	}
	if got.Details.MonitorOrigin != nil {
		t.Fatalf("non-monitor task carries an origin: %+v", got.Details.MonitorOrigin)
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

// A monitor-filed task's context decision states its origin from the
// structured observation, never from the task body; another row carries
// neither presentation field (feature-operator-next-action NA-3, Monitor
// origin; NA-4).
func TestContextProposalMonitorOrigin(t *testing.T) {
	now := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	observed := now.Add(-time.Hour)
	base := func(source string, observation *MonitorObservationRef) Input {
		return Input{
			Task:               core.Task{ID: "task-1", State: core.TaskRunning, Source: source, Body: "CI run https://example.invalid/body-link failed"},
			ContextProposals:   []ContextProposalRef{{Kind: "requirement", TargetID: "req-x", TargetTitle: "X"}},
			MonitorObservation: observation,
			Capabilities:       map[core.Capability]bool{core.CapabilityOperateGates: true},
			Now:                now,
		}
	}
	run := "https://github.com/acme/app/actions/runs/1"
	cases := []struct {
		name string
		in   Input
		want *MonitorOrigin
	}{
		{name: "linked observation", in: base("monitor:post_merge_failure", &MonitorObservationRef{Kind: "post_merge_failure", SourceURL: run, ObservedAt: observed}),
			want: &MonitorOrigin{Kind: "post_merge_failure", Summary: "Filed by the monitor: post-merge failure", SourceURL: run, ObservedAt: &observed}},
		{name: "no observation", in: base("monitor:direct_push", nil),
			want: &MonitorOrigin{Kind: "direct_push", Summary: "Filed by the monitor: direct push"}},
		{name: "observation of another kind", in: base("monitor:revert", &MonitorObservationRef{Kind: "direct_push", SourceURL: run, ObservedAt: observed}),
			want: &MonitorOrigin{Kind: "revert", Summary: "Filed by the monitor: revert"}},
		{name: "non-HTTP source URL", in: base("monitor:external_pr_merge", &MonitorObservationRef{Kind: "external_pr_merge", SourceURL: "javascript:alert(1)", ObservedAt: observed}),
			want: &MonitorOrigin{Kind: "external_pr_merge", Summary: "Filed by the monitor: external pull request merge", ObservedAt: &observed}},
		{name: "credentialed source URL", in: base("monitor:post_merge_failure", &MonitorObservationRef{Kind: "post_merge_failure", SourceURL: "https://user:secret@github.com/run", ObservedAt: observed}),
			want: &MonitorOrigin{Kind: "post_merge_failure", Summary: "Filed by the monitor: post-merge failure", ObservedAt: &observed}},
		{name: "non-monitor task", in: base("mcp", &MonitorObservationRef{Kind: "post_merge_failure", SourceURL: run, ObservedAt: observed})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Kind != KindContextProposal {
				t.Fatalf("kind = %s", got.Kind)
			}
			if !reflect.DeepEqual(got.Details.MonitorOrigin, tc.want) {
				t.Fatalf("monitor_origin = %+v, want %+v", got.Details.MonitorOrigin, tc.want)
			}
			raw, err := json.Marshal(got.Details)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "body-link") {
				t.Fatalf("details read the task body: %s", raw)
			}
		})
	}

	// Another row carries neither field, even with a pending context proposal.
	other := base("monitor:post_merge_failure", &MonitorObservationRef{Kind: "post_merge_failure", SourceURL: run, ObservedAt: observed})
	other.Task.State = core.TaskAwaiting
	other.Task.GateKind = core.GateKindPlanApproval
	got := Project(other)
	if got.Kind == KindContextProposal || got.Details.ContextProposals != nil || got.Details.MonitorOrigin != nil {
		t.Fatalf("row %s carries context presentation: %+v / %+v", got.Kind, got.Details.ContextProposals, got.Details.MonitorOrigin)
	}
	raw, _ := json.Marshal(got.Details)
	if strings.Contains(string(raw), "context_proposals") || strings.Contains(string(raw), "monitor_origin") {
		t.Fatalf("absent fields serialized: %s", raw)
	}
}
