package storetest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunSystemDesignProposalConformance verifies the shared proposal identity
// contract against memory and PostgreSQL stores.
func RunSystemDesignProposalConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	document, _, err := st.CreateSystemDesign(ctx,
		core.SystemDesign{ID: "design-proposal-dedup", Title: "Proposal dedup", Category: "Architecture"},
		core.SystemDesignVersion{Content: designProposalContent("initial"), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
		DocumentID: document.ID, Content: " \r\n" + designProposalContent("pending") + "\r\n\t",
		Origin: core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := st.ListSystemDesignEvents(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
		DocumentID: document.ID, Content: "\n" + strings.ReplaceAll(designProposalContent("pending"), "\n", "\r\n") + "  ",
		Origin: core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reused.Deduplicated || reused.Version != first.Version || reused.Content != first.Content {
		t.Fatalf("reused=%+v first=%+v", reused, first)
	}
	eventsAfter, err := st.ListSystemDesignEvents(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("repeat proposal appended event: before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
	// A document-bounded selection returns the pending revisions of exactly
	// the named documents from any origin: the document's unconfirmed
	// operator-origin initial version and its task-origin proposal, and
	// nothing of other documents (req-operator-next-action AC-1.6).
	all, err := st.ListPendingProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var want []core.PendingProposal
	for _, proposal := range all {
		if proposal.Tier == "system_design" && proposal.ID == document.ID {
			want = append(want, proposal)
		}
	}
	bounded, err := st.ListPendingProposalsForDocuments(ctx, []string{document.ID, "missing-document"})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || len(bounded) != len(want) {
		t.Fatalf("global document queue=%+v, bounded=%+v", want, bounded)
	}
	origins := map[string]bool{}
	for i := range bounded {
		if bounded[i].ID != want[i].ID || bounded[i].Version != want[i].Version || bounded[i].Tier != want[i].Tier ||
			bounded[i].OriginType != want[i].OriginType || bounded[i].OriginID != want[i].OriginID {
			t.Fatalf("bounded[%d]=%+v, global=%+v", i, bounded[i], want[i])
		}
		origins[bounded[i].OriginType] = true
	}
	if !origins["operator"] || !origins["task"] || bounded[len(bounded)-1].Version != first.Version {
		t.Fatalf("bounded proposals omit an origin: %+v", bounded)
	}
	if none, err := st.ListPendingProposalsForDocuments(ctx, []string{"missing-document"}); err != nil || len(none) != 0 {
		t.Fatalf("unnamed document bound=%+v err=%v", none, err)
	}
	if empty, err := st.ListPendingProposalsForDocuments(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty bound=%+v err=%v", empty, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate, proposalErr := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
				DocumentID: document.ID, Content: designProposalContent("pending"),
				Origin: core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
			})
			if proposalErr != nil {
				errs <- proposalErr
				return
			}
			if !candidate.Deduplicated || candidate.Version != first.Version {
				errs <- &proposalConformanceError{candidate: candidate, wantVersion: first.Version}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for proposalErr := range errs {
		t.Fatal(proposalErr)
	}
	eventsConcurrent, err := st.ListSystemDesignEvents(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsConcurrent) != len(eventsBefore) {
		t.Fatalf("concurrent repeats appended events: before=%d after=%d", len(eventsBefore), len(eventsConcurrent))
	}
	different, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
		DocumentID: document.ID, Content: designProposalContent("materially different"),
		Origin: core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if different.Deduplicated || different.Version != first.Version+1 {
		t.Fatalf("different=%+v first=%+v", different, first)
	}
	pendingForTask := func() []int {
		t.Helper()
		proposals, err := st.ListPendingProposals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		versions := []int{}
		for _, proposal := range proposals {
			if proposal.Tier == "system_design" && proposal.ID == document.ID && proposal.OriginType == "task" && proposal.OriginID == "proposal-task" {
				versions = append(versions, proposal.Version)
			}
		}
		return versions
	}
	if got := pendingForTask(); len(got) != 2 || got[0] != first.Version || got[1] != different.Version {
		t.Fatalf("task-origin pending versions=%v want %d, %d", got, first.Version, different.Version)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, different.Version); err != nil {
		t.Fatal(err)
	}
	if got := pendingForTask(); len(got) != 0 {
		t.Fatalf("task-origin pending versions after confirmation=%v", got)
	}
	// The confirmed version keeps its recorded task origin in history
	// (req-task-authority-boundary AC-1.4).
	confirmed, err := st.GetSystemDesignVersion(ctx, document.ID, different.Version)
	if err != nil || !confirmed.Confirmed || confirmed.Origin != core.SystemDesignOriginImplementation || confirmed.OriginTaskID != "proposal-task" {
		t.Fatalf("confirmed version=%+v err=%v", confirmed, err)
	}
}

type proposalConformanceError struct {
	candidate   core.SystemDesignVersion
	wantVersion int
}

func (err *proposalConformanceError) Error() string {
	return fmt.Sprintf("deduplicated proposal=%+v want version=%d", err.candidate, err.wantVersion)
}

func designProposalContent(label string) string {
	return "# " + label + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/workorder/**\n```"
}
