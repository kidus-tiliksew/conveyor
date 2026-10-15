package nextaction

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Row 8 lists one confirm and one dismiss choice per withholding version on
// its version route, with allowed taken from confirm_documents alone
// (req-operator-next-action AC-1.4, AC-1.8; feature-operator-next-action NA-3,
// NA-4).
func TestDocumentProposalChoicesUseVersionRoutes(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	in := Input{
		Task: core.Task{ID: "task-1", State: core.TaskRunning},
		ClaimWaiting: []ProposalRef{
			{Tier: "system_design", ID: "component-work-orders", Version: 50},
			{Tier: "requirement", ID: "req-operator-next-action", Version: 5},
		},
		Orders:       []core.WorkOrder{{ID: "task-1-review-1", TaskID: "task-1", Stage: core.StageReview, State: core.WorkOrderQueued}},
		Capabilities: map[core.Capability]bool{core.CapabilityConfirmDocuments: true},
		Now:          now,
	}
	got := Project(in)
	if got.Kind != KindDocumentProposal || got.Actor != ActorOperator || !got.Judgment {
		t.Fatalf("next action = %s/%s judgment=%v", got.Actor, got.Kind, got.Judgment)
	}
	if got.Label != "Document proposal" || got.Summary != "A document proposal is withholding the next claim." ||
		got.Question != "Confirm or dismiss the proposal?" || got.IfIgnored != "The claim stays withheld." {
		t.Fatalf("copy = %q / %q / %q / %q", got.Label, got.Summary, got.Question, got.IfIgnored)
	}
	if !reflect.DeepEqual(got.Details.PendingProposals, in.ClaimWaiting) {
		t.Fatalf("pending_proposals = %+v", got.Details.PendingProposals)
	}
	confirm := string(core.CapabilityConfirmDocuments)
	want := []Choice{
		{ID: "confirm:system_design:component-work-orders:50", Label: "Confirm component-work-orders v50", Effect: "Confirms this pending revision.", Method: "POST", Path: "/v1/system-designs/component-work-orders/versions/50/confirm", Capability: confirm, Allowed: true},
		{ID: "dismiss:system_design:component-work-orders:50", Label: "Dismiss component-work-orders v50", Effect: "Dismisses this pending revision.", Method: "POST", Path: "/v1/system-designs/component-work-orders/versions/50/dismiss", Capability: confirm, Allowed: true},
		{ID: "confirm:requirement:req-operator-next-action:5", Label: "Confirm req-operator-next-action v5", Effect: "Confirms this pending revision.", Method: "POST", Path: "/v1/requirements/req-operator-next-action/versions/5/confirm", Capability: confirm, Allowed: true},
		{ID: "dismiss:requirement:req-operator-next-action:5", Label: "Dismiss req-operator-next-action v5", Effect: "Dismisses this pending revision.", Method: "POST", Path: "/v1/requirements/req-operator-next-action/versions/5/dismiss", Capability: confirm, Allowed: true},
	}
	if !reflect.DeepEqual(got.Choices, want) {
		t.Fatalf("choices = %+v\nwant %+v", got.Choices, want)
	}

	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true, core.CapabilityOperateGates: true}
	for _, choice := range Project(in).Choices {
		if choice.Allowed {
			t.Fatalf("choice %s allowed without confirm_documents", choice.ID)
		}
	}
}

// No next-action choice names a /v1/documents/ route, because the server
// serves none (req-operator-next-action AC-1.4; feature-operator-next-action
// NA-3).
func TestNoChoiceNamesDocumentsRoute(t *testing.T) {
	for key, entry := range catalogueTable {
		for _, choice := range entry.Choices {
			if strings.Contains(choice.Path, "/v1/documents/") {
				t.Fatalf("catalogue entry %+v choice %s names %s", key, choice.ID, choice.Path)
			}
		}
	}
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	refs := []ProposalRef{{Tier: "system_design", ID: "design-x", Version: 3}, {Tier: "requirement", ID: "req-x", Version: 2}}
	inputs := []Input{
		{Task: core.Task{ID: "t", State: core.TaskRunning}, ClaimWaiting: refs, Now: now},
		checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, []core.WorkOrderAuthorityConflictCitation{{DocumentID: "design-x", CitedVersion: 2}, {DocumentID: "req-x", CitedVersion: 1}}, refs),
	}
	for _, in := range inputs {
		got := Project(in)
		for _, choice := range got.Choices {
			if strings.Contains(choice.Path, "/v1/documents/") {
				t.Fatalf("%s choice %s names %s", got.Kind, choice.ID, choice.Path)
			}
		}
	}
}

// The 261015-676ea5 shape: with row 8 fed only by the claim-wait selection,
// a task-context proposal no longer masks a released authority-conflict
// checkpoint. The task resolves to its agent question with only the cited
// versions listed and no document_proposal secondary (req-operator-next-action
// AC-1.6, AC-1.7; req-260810-70ce2f AC-1.3).
func TestTaskContextProposalDoesNotMaskAgentQuestion(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 9, 12, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{
		{DocumentID: "component-web-task-surfaces", CitedVersion: 11, StatementOrSectionID: "Task detail composition"},
		{DocumentID: "component-web-dashboard", CitedVersion: 27, StatementOrSectionID: "Route tree"},
	}
	pending := []ProposalRef{
		{Tier: "task_context", ID: "component-verification-strategy"},
		{Tier: "system_design", ID: "component-web-task-surfaces", Version: 12},
		{Tier: "system_design", ID: "component-web-dashboard", Version: 28},
	}
	in := checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, citations, pending)
	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true}
	got := Project(in)
	if got.Kind != KindAgentQuestion || got.Actor != ActorOperator {
		t.Fatalf("next action = %s/%s, want operator agent_question", got.Actor, got.Kind)
	}
	want := []ProposalRef{
		{Tier: "system_design", ID: "component-web-dashboard", Version: 28},
		{Tier: "system_design", ID: "component-web-task-surfaces", Version: 12},
	}
	if !reflect.DeepEqual(got.Details.PendingProposals, want) {
		t.Fatalf("pending_proposals = %+v, want %+v", got.Details.PendingProposals, want)
	}
	if hasSecondary(got, KindDocumentProposal) {
		t.Fatalf("secondary = %+v, want no document_proposal", got.Details.Secondary)
	}
	answer := got.Choices[len(got.Choices)-1]
	if answer.ID != "answer" || !answer.Allowed {
		t.Fatalf("answer choice = %+v, want recovery with direction allowed", answer)
	}
}
