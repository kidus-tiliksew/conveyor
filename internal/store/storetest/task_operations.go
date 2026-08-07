package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TaskOperationsFixture is a store already holding WantTotal tasks in the
// fixture workspace, all matching the unfiltered TaskOperationsQuery. The
// conformance run below only reads, so one fixture serves every case.
type TaskOperationsFixture struct {
	Store     store.Store
	Context   context.Context
	WantTotal int
}

// RunTaskOperationsPaginationConformance asserts that every store answers the
// pagination bounds identically. The Postgres page query binds LIMIT/OFFSET as
// int32, so an offset past store.MaxTaskOperationsOffset used to wrap — into a
// negative bound Postgres rejects outright, or into a small positive one that
// pages from an unrelated row — while memory returned an empty page. Both
// stores now reject it, and both serve the boundary offset itself as an
// ordinary past-the-end page (spec §21.58).
func RunTaskOperationsPaginationConformance(t *testing.T, fixture TaskOperationsFixture) {
	t.Helper()

	page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{
		Limit: 1, Offset: store.MaxTaskOperationsOffset,
	})
	if err != nil {
		t.Fatalf("boundary offset: %v", err)
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("boundary offset returned %d tasks: %+v", len(page.Tasks), page.Tasks)
	}
	if page.Total != fixture.WantTotal {
		t.Fatalf("boundary offset total=%d want %d", page.Total, fixture.WantTotal)
	}

	for name, query := range map[string]store.TaskOperationsQuery{
		"offset one past the bound": {Limit: 1, Offset: store.MaxTaskOperationsOffset + 1},
		"negative offset":           {Limit: 1, Offset: -1},
		"limit past the bound":      {Limit: store.MaxTaskOperationsLimit + 1},
		"negative limit":            {Limit: -1},
	} {
		rejected, rejectErr := fixture.Store.ListTaskOperations(fixture.Context, query)
		if rejectErr == nil {
			t.Fatalf("%s: accepted, returning %d tasks", name, len(rejected.Tasks))
		}
		if !strings.Contains(rejectErr.Error(), "task operations") {
			t.Fatalf("%s: unexpected error %v", name, rejectErr)
		}
	}
}
