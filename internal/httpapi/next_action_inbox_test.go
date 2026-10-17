package httpapi

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

func TestInboxFoldsDocumentProposalTasks(t *testing.T) {
	task := core.Task{ID: "folded", State: core.TaskRunning}
	projected := map[string]nextaction.NextAction{
		task.ID: {Actor: nextaction.ActorOperator, Kind: nextaction.KindDocumentProposal, Judgment: true},
	}
	items := inboxTaskItems([]core.Task{task}, projected, map[string]time.Time{}, map[string]bool{task.ID: true})
	if len(items) != 0 {
		t.Fatalf("folded task still listed: %+v", items)
	}
}

// Each document-proposal item states its age at the read's clock and the
// NA-4 inbox copy; only a folded row-8 withholding version says the claim
// stays withheld (feature-operator-next-action NA-4, NA-5;
// req-operator-next-action AC-1.8, AC-2.6).
func TestDocumentProposalItemsStateAgeAndCopy(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	proposals := []core.PendingProposal{
		{ID: "req-a", Title: "Requirement A", Tier: "requirement", Version: 2, OriginType: "task", OriginID: "task-1", ProposedAt: now.Add(-90 * time.Minute)},
		{ID: "req-a", Title: "Requirement A", Tier: "requirement", Version: 3, OriginType: "operator", ProposedAt: now.Add(-1500 * time.Millisecond)},
		{ID: "design-b", Title: "Design B", Tier: "system_design", Version: 4, OriginType: "operator", ProposedAt: now.Add(time.Hour)},
		{ID: "DEC-9", Title: "Decision nine", Tier: "decision", OriginType: "session", OriginID: "session-1"},
		{ID: "ctx", Title: "Context", Tier: "task_context", ProposedAt: now},
	}
	withholding := map[string]bool{proposalKey("requirement", "req-a", 2): true}
	items := documentProposalItems(proposals, withholding, now)
	if len(items) != 4 {
		t.Fatalf("items=%+v", items)
	}
	type want struct {
		age                       *int64
		label, question, ifIgnore string
		sentence                  string
	}
	ages := []int64{5400, 1, 0}
	wants := []want{
		{&ages[0], "Requirement proposal", "Confirm or dismiss this proposed version?", "The claim stays withheld.", dismissEarlierVersions},
		{&ages[1], "Requirement proposal", "Confirm or dismiss this proposed version?", "The proposal stays pending.", dismissEarlierVersions},
		{&ages[2], "System Design proposal", "Confirm or dismiss this proposed version?", "The proposal stays pending.", ""},
		{nil, "Decision proposal", "Confirm or dismiss this proposed decision?", "The proposal stays pending.", ""},
	}
	for i, item := range items {
		w := wants[i]
		if item.Kind != "document_proposal" || item.Band != "decisions" || item.Proposal == nil {
			t.Fatalf("item %d shape=%+v", i, item)
		}
		if (w.age == nil) != (item.Proposal.AgeSeconds == nil) || (w.age != nil && *w.age != *item.Proposal.AgeSeconds) {
			t.Fatalf("item %d age=%v want %v", i, item.Proposal.AgeSeconds, w.age)
		}
		if item.Label != w.label || item.Question != w.question || item.IfIgnored != w.ifIgnore || item.Sentence != w.sentence {
			t.Fatalf("item %d copy=%q %q %q %q", i, item.Label, item.Question, item.IfIgnored, item.Sentence)
		}
	}
}

func TestDocumentProposalCopyUnknownTierReadsRowEight(t *testing.T) {
	label, question, ifIgnored := nextaction.DocumentProposalCopy("other", false)
	if label != "Document proposal" || question != "Confirm or dismiss the proposal?" || ifIgnored != "The proposal stays pending." {
		t.Fatalf("copy=%q %q %q", label, question, ifIgnored)
	}
}
