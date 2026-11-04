package nextaction

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// A pending document proposal selects no row: the workspace's pending versions
// reach the projection for agent-question presentation only, so a task with a
// queued review or verify order stays on its queued row with no proposal in its
// details (req-260810-23b69f REQ-3; DEC-102(6); req-operator-next-action AC-1.3).
func TestPendingProposalsSelectNoRow(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	pending := []ProposalRef{
		{Tier: "system_design", ID: "component-work-orders", Version: 50},
		{Tier: "requirement", ID: "req-operator-next-action", Version: 5},
		{Tier: "decision", ID: "DEC-200"},
	}
	for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
		order := core.WorkOrder{ID: "task-1-" + string(stage) + "-1", TaskID: "task-1", Stage: stage, State: core.WorkOrderQueued, Claimable: true, HeadSHA: "head", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
		in := Input{Task: core.Task{ID: "task-1", State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "head"}, Orders: []core.WorkOrder{order}, Capabilities: map[core.Capability]bool{core.CapabilityConfirmDocuments: true}, Now: now}
		in.Task.SetupContract.VerifyStage = stage == core.StageVerify
		want := Project(in)
		in.PendingProposals = pending
		got := Project(in)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: pending proposals changed the next action:\n got %+v\nwant %+v", stage, got, want)
		}
		if got.Kind != KindQueued || got.Actor == ActorOperator || len(got.Details.PendingProposals) != 0 || len(got.Choices) != 0 {
			t.Fatalf("%s: next action=%s/%s proposals=%+v choices=%+v", stage, got.Actor, got.Kind, got.Details.PendingProposals, got.Choices)
		}
	}
	for _, kind := range Kinds() {
		if kind == "document_proposal" {
			t.Fatal("the catalogue still carries the retired document_proposal row")
		}
	}
}

// Cited pending versions keep one confirm and one dismiss choice each on the
// version route, with allowed taken from confirm_documents alone, and neither
// recovers the order (req-operator-next-action AC-1.6, AC-2.3; req-260820-394cac
// AC-2.3).
func TestCitedProposalChoicesUseVersionRoutes(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "component-work-orders", CitedVersion: 49}, {DocumentID: "req-operator-next-action", CitedVersion: 4}}
	in := checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, citations, []ProposalRef{
		{Tier: "system_design", ID: "component-work-orders", Version: 50},
		{Tier: "requirement", ID: "req-operator-next-action", Version: 5},
		{Tier: "system_design", ID: "component-uncited", Version: 2},
	})
	in.Capabilities = map[core.Capability]bool{core.CapabilityConfirmDocuments: true}
	got := Project(in)
	if got.Kind != KindAgentQuestion || got.Actor != ActorOperator || !got.Judgment {
		t.Fatalf("next action = %s/%s judgment=%v", got.Actor, got.Kind, got.Judgment)
	}
	wantPending := []ProposalRef{
		{Tier: "requirement", ID: "req-operator-next-action", Version: 5},
		{Tier: "system_design", ID: "component-work-orders", Version: 50},
	}
	if !reflect.DeepEqual(got.Details.PendingProposals, wantPending) {
		t.Fatalf("pending_proposals = %+v", got.Details.PendingProposals)
	}
	confirm := string(core.CapabilityConfirmDocuments)
	const confirmed, dismissed = "Confirms this pending revision. The order stays released until you recover it.", "Dismisses this pending revision. The order stays released until you recover it."
	want := []Choice{
		{ID: "confirm:requirement:req-operator-next-action:5", Label: "Confirm req-operator-next-action v5", Effect: confirmed, Method: "POST", Path: "/v1/requirements/req-operator-next-action/versions/5/confirm", Capability: confirm, Allowed: true},
		{ID: "dismiss:requirement:req-operator-next-action:5", Label: "Dismiss req-operator-next-action v5", Effect: dismissed, Method: "POST", Path: "/v1/requirements/req-operator-next-action/versions/5/dismiss", Capability: confirm, Allowed: true},
		{ID: "confirm:system_design:component-work-orders:50", Label: "Confirm component-work-orders v50", Effect: confirmed, Method: "POST", Path: "/v1/system-designs/component-work-orders/versions/50/confirm", Capability: confirm, Allowed: true},
		{ID: "dismiss:system_design:component-work-orders:50", Label: "Dismiss component-work-orders v50", Effect: dismissed, Method: "POST", Path: "/v1/system-designs/component-work-orders/versions/50/dismiss", Capability: confirm, Allowed: true},
	}
	if len(got.Choices) < len(want) || !reflect.DeepEqual(got.Choices[:len(want)], want) {
		t.Fatalf("choices = %+v\nwant %+v", got.Choices, want)
	}
	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true, core.CapabilityOperateGates: true}
	for _, choice := range Project(in).Choices[:len(want)] {
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
		{Task: core.Task{ID: "t", State: core.TaskRunning}, PendingProposals: refs, Now: now},
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

// The 261015-676ea5 shape: a task-context proposal never masks a released
// authority-conflict checkpoint; the context row ranks below it. The task
// resolves to its agent question with only the cited versions listed and no
// document_proposal secondary (req-operator-next-action AC-1.6).
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
	in.ContextProposals = []ContextProposalRef{{Kind: "system_design", TargetID: "component-verification-strategy"}}
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
	if hasSecondary(got, "document_proposal") || !hasSecondary(got, KindContextProposal) {
		t.Fatalf("secondary = %+v, want context_proposal and no document_proposal", got.Details.Secondary)
	}
	answer := got.Choices[len(got.Choices)-1]
	if answer.ID != "answer" || !answer.Allowed {
		t.Fatalf("answer choice = %+v, want recovery with direction allowed", answer)
	}
}
