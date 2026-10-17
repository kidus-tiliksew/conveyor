package nextaction

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The agent question presents an authority_conflict checkpoint with its
// cited pending proposals and per-proposal decisions before the answer
// (req-operator-next-action AC-1.4, AC-1.6, AC-1.8; req-260820-394cac AC-2.3,
// AC-2.5, AC-3.2; feature-operator-next-action NA-3, NA-4).
func TestAgentQuestionAuthorityConflictPresentation(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{
		{DocumentID: "component-http-api", CitedVersion: 43, StatementOrSectionID: "Live task event stream"},
		{DocumentID: "component-http-api", CitedVersion: 43, StatementOrSectionID: "Route index"},
		{DocumentID: "req-checkpoints", CitedVersion: 2, StatementOrSectionID: "AC-2.3"},
	}
	pending := []ProposalRef{
		{Tier: "system_design", ID: "component-http-api", Version: 45},
		{Tier: "task_context", ID: "component-http-api"},
		{Tier: "system_design", ID: "component-unrelated", Version: 2},
		{Tier: "system_design", ID: "component-http-api", Version: 44},
		{Tier: "decision", ID: "DEC-90"},
		{Tier: "requirement", ID: "req-checkpoints", Version: 3},
		{Tier: "system_design", ID: "component-http-api", Version: 44},
	}
	in := checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, citations, pending)
	in.Capabilities = map[core.Capability]bool{core.CapabilityConfirmDocuments: true}
	got := Project(in)
	if got.Kind != KindAgentQuestion || got.Actor != ActorOperator || !got.Judgment {
		t.Fatalf("next action = %s/%s judgment=%v", got.Actor, got.Kind, got.Judgment)
	}
	wantProposals := []ProposalRef{
		{Tier: "requirement", ID: "req-checkpoints", Version: 3},
		{Tier: "system_design", ID: "component-http-api", Version: 44},
		{Tier: "system_design", ID: "component-http-api", Version: 45},
	}
	if !reflect.DeepEqual(got.Details.PendingProposals, wantProposals) {
		t.Fatalf("pending_proposals = %+v, want %+v", got.Details.PendingProposals, wantProposals)
	}
	if got.Label != "Cited revisions need a decision" ||
		got.Summary != "The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction." ||
		got.Question != "Will you confirm or dismiss the cited revisions, then recover the order with direction?" {
		t.Fatalf("copy = %q / %q / %q", got.Label, got.Summary, got.Question)
	}
	if got.Details.Checkpoint != "Confirm component-http-api v44." || got.Details.CheckpointClass != core.WorkOrderCheckpointClassAuthorityConflict || !reflect.DeepEqual(got.Details.Citations, citations) {
		t.Fatalf("details = %+v", got.Details)
	}
	wantChoices := []Choice{
		{ID: "confirm:requirement:req-checkpoints:3", Label: "Confirm req-checkpoints v3", Effect: citedConfirmEffect, Method: "POST", Path: "/v1/requirements/req-checkpoints/versions/3/confirm", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "dismiss:requirement:req-checkpoints:3", Label: "Dismiss req-checkpoints v3", Effect: citedDismissEffect, Method: "POST", Path: "/v1/requirements/req-checkpoints/versions/3/dismiss", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "confirm:system_design:component-http-api:44", Label: "Confirm component-http-api v44", Effect: citedConfirmEffect, Method: "POST", Path: "/v1/system-designs/component-http-api/versions/44/confirm", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "dismiss:system_design:component-http-api:44", Label: "Dismiss component-http-api v44", Effect: citedDismissEffect, Method: "POST", Path: "/v1/system-designs/component-http-api/versions/44/dismiss", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "confirm:system_design:component-http-api:45", Label: "Confirm component-http-api v45", Effect: citedConfirmEffect, Method: "POST", Path: "/v1/system-designs/component-http-api/versions/45/confirm", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "dismiss:system_design:component-http-api:45", Label: "Dismiss component-http-api v45", Effect: citedDismissEffect, Method: "POST", Path: "/v1/system-designs/component-http-api/versions/45/dismiss", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: "answer", Label: "Answer…", Effect: "Recovers the order with direction.", Method: "POST", Path: "/v1/work-orders/question/recover", Capability: string(core.CapabilityRecoverWork), Allowed: false},
	}
	if !reflect.DeepEqual(got.Choices, wantChoices) {
		t.Fatalf("choices = %+v\nwant %+v", got.Choices, wantChoices)
	}

	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true}
	recoverOnly := Project(in)
	for _, choice := range recoverOnly.Choices {
		if choice.Allowed != (choice.ID == "answer") {
			t.Fatalf("recover_work only: choice %s allowed=%v", choice.ID, choice.Allowed)
		}
	}
}

func TestAgentQuestionAuthorityConflictWithoutProposalsAsksForAuthorship(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "component-http-api", CitedVersion: 43}}
	pending := []ProposalRef{{Tier: "system_design", ID: "component-unrelated", Version: 2}, {Tier: "task_context", ID: "component-http-api"}}
	in := checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, citations, pending)
	in.Capabilities = allCaps()
	got := Project(in)
	if got.Kind != KindAgentQuestion || got.Label != "Cited documents need a revision" ||
		got.Summary != "The agent asks you to author a revision of the cited documents, then recover the order with direction." ||
		got.Question != "Will you author a revision of the cited documents, then recover the order with direction?" {
		t.Fatalf("next action = %s %q / %q / %q", got.Kind, got.Label, got.Summary, got.Question)
	}
	if len(got.Details.PendingProposals) != 0 || len(got.Choices) != 1 || got.Choices[0].ID != "answer" || !got.Choices[0].Allowed {
		t.Fatalf("proposals = %+v choices = %+v", got.Details.PendingProposals, got.Choices)
	}
}

// An unclassified or other-class checkpoint keeps the base entry and never
// lists proposals, even when a cited document has one.
func TestAgentQuestionUnclassifiedCheckpointKeepsBaseEntry(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "component-http-api", CitedVersion: 43}}
	pending := []ProposalRef{{Tier: "system_design", ID: "component-http-api", Version: 44}}
	for _, class := range []string{"", "other_class"} {
		in := checkpointInput(now, class, citations, pending)
		in.Capabilities = allCaps()
		got := Project(in)
		if got.Kind != KindAgentQuestion || got.Label != "Agent question" || got.Summary != "The agent stopped for a decision." || got.Question != "Answer the agent?" {
			t.Fatalf("class %q: %s %q / %q / %q", class, got.Kind, got.Label, got.Summary, got.Question)
		}
		if len(got.Details.PendingProposals) != 0 || len(got.Choices) != 1 || got.Choices[0].ID != "answer" {
			t.Fatalf("class %q: proposals = %+v choices = %+v", class, got.Details.PendingProposals, got.Choices)
		}
	}
}

// Cited proposals are presentation only: they never select row 8. A
// checkpoint release that also matches row 14 stays an agent question and
// records the stall as a secondary signal.
func TestAgentQuestionPrecedenceWithCitedProposalsAndStall(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "component-http-api", CitedVersion: 43}}
	in := checkpointInput(now, core.WorkOrderCheckpointClassAuthorityConflict, citations, []ProposalRef{{Tier: "system_design", ID: "component-http-api", Version: 44}})
	in.Orders[0].RetrySuppressed = true
	got := Project(in)
	if got.Kind != KindAgentQuestion {
		t.Fatalf("kind = %s, want agent_question", got.Kind)
	}
	if hasSecondary(got, KindDocumentProposal) || !hasSecondary(got, KindStalled) {
		t.Fatalf("secondary = %+v, want stalled and no document_proposal", got.Details.Secondary)
	}
}

func TestCitedProposalsDeduplicateAndOrder(t *testing.T) {
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "b"}, {DocumentID: "a"}, {DocumentID: "b"}}
	pending := []ProposalRef{
		{Tier: "system_design", ID: "b", Version: 2}, {Tier: "system_design", ID: "a", Version: 9},
		{Tier: "system_design", ID: "b", Version: 1}, {Tier: "system_design", ID: "b", Version: 2},
		{Tier: "requirement", ID: "b", Version: 4}, {Tier: "system_design", ID: "c", Version: 1},
	}
	want := []ProposalRef{
		{Tier: "requirement", ID: "b", Version: 4}, {Tier: "system_design", ID: "a", Version: 9},
		{Tier: "system_design", ID: "b", Version: 1}, {Tier: "system_design", ID: "b", Version: 2},
	}
	if got := CitedProposals(citations, pending); !reflect.DeepEqual(got, want) {
		t.Fatalf("cited = %+v, want %+v", got, want)
	}
}

// An operator recovery clears the released outcome and suppression, records
// the direction, and keeps the checkpoint for history. The resumed order then
// projects as ordinary queued work with no recover-route choice and no row-9
// secondary, for every checkpoint class, even when a caller supplies a stale
// checkpoint marker (feature-operator-next-action NA-3 row 9;
// req-operator-next-action AC-1.4, AC-1.6).
func TestAgentQuestionResumedOrderProjectsAsQueuedWork(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 25, 46, 0, time.UTC)
	citations := []core.WorkOrderAuthorityConflictCitation{
		{DocumentID: "feature-operator-next-action", CitedVersion: 20},
		{DocumentID: "component-work-orders", CitedVersion: 52, StatementOrSectionID: "Next action"},
	}
	for _, class := range []string{core.WorkOrderCheckpointClassAuthorityConflict, ""} {
		released := checkpointInput(now, class, citations, nil)
		released.Capabilities = allCaps()
		if got := Project(released); got.Kind != KindAgentQuestion || !hasChoice(got, "answer") {
			t.Fatalf("class %q released: %s choices %+v, want agent_question with answer", class, got.Kind, got.Choices)
		}

		resumed := resumedInput(released)
		got := Project(resumed)
		if got.Kind != KindQueued || got.Actor != ActorAgent || got.Judgment {
			t.Fatalf("class %q resumed: %s/%s judgment=%v, want agent queued", class, got.Actor, got.Kind, got.Judgment)
		}
		if got.CurrentOrder == nil || got.CurrentOrder.ID != "question" {
			t.Fatalf("class %q resumed: current order %+v, want question", class, got.CurrentOrder)
		}
		if hasSecondary(got, KindAgentQuestion) {
			t.Fatalf("class %q resumed: secondary %+v includes agent_question", class, got.Details.Secondary)
		}
		for _, id := range []string{"answer", "recover", "applied", "not_applied"} {
			if hasChoice(got, id) {
				t.Fatalf("class %q resumed: offered %s in %+v", class, id, got.Choices)
			}
		}
		if got.Details.Checkpoint != "" || got.Details.CheckpointClass != "" || len(got.Details.Citations) != 0 {
			t.Fatalf("class %q resumed: details carry the answered checkpoint %+v", class, got.Details)
		}
	}
}

// resumedInput is the 261016-cdb507 shape after an operator recovery: the
// order is queued and claimable, its outcome and suppression are cleared, the
// direction and redispatch count are set, and the checkpoint stays. The stale
// marker from the released input stays too, so the projection's own guard is
// what keeps row 9 from matching.
func resumedInput(released Input) Input {
	resumed := released
	order := released.Orders[0]
	order.LastAttemptOutcome = ""
	order.RetrySuppressed = false
	order.Claimable = true
	order.OperatorDirection = "Continue under the newly confirmed authority."
	order.RedispatchCount = 1
	order.LastFailureMessage = core.WorkOrderReleaseReasonOperatorCheckpointReached
	order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: released.Markers.Checkpoint, Class: released.Markers.CheckpointClass, Citations: released.Markers.Citations}
	resumed.Orders = []core.WorkOrder{order}
	return resumed
}

func checkpointInput(now time.Time, class string, citations []core.WorkOrderAuthorityConflictCitation, pending []ProposalRef) Input {
	return Input{
		Task:             core.Task{ID: "task", State: core.TaskRunning},
		Orders:           []core.WorkOrder{{ID: "question", State: core.WorkOrderQueued, Stage: core.StageImplement, LastAttemptID: "attempt", LastAttemptOutcome: core.WorkOrderOutcomeReleased}},
		Markers:          Markers{CheckpointDecision: true, Checkpoint: "Confirm component-http-api v44.", CheckpointClass: class, Citations: citations},
		PendingProposals: pending,
		Now:              now,
	}
}
