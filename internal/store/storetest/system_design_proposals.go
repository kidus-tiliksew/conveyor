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
	taskVersions, err := st.ListPendingSystemDesignVersionsForTask(ctx, "proposal-task")
	if err != nil || len(taskVersions) != 2 || taskVersions[0].Version != first.Version || taskVersions[1].Version != different.Version {
		t.Fatalf("task-scoped pending versions=%+v err=%v", taskVersions, err)
	}
	taskEvents, err := st.ListSystemDesignProposalEventsForTask(ctx, "proposal-task")
	if err != nil || len(taskEvents) != 2 {
		t.Fatalf("task-scoped proposal events=%+v err=%v", taskEvents, err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, different.Version); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.ListSystemDesignProposalVersionsForTask(ctx, "proposal-task")
	if err != nil || len(evidence) != 1 || evidence[0].Version != different.Version || !evidence[0].Confirmed {
		t.Fatalf("task-scoped proposal evidence=%+v err=%v", evidence, err)
	}
	pendingAfterConfirm, err := st.ListPendingSystemDesignVersionsForTask(ctx, "proposal-task")
	if err != nil || len(pendingAfterConfirm) != 0 {
		t.Fatalf("task-scoped pending versions after confirmation=%+v err=%v", pendingAfterConfirm, err)
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
