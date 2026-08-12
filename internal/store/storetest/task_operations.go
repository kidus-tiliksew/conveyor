package storetest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TaskOperationsFixture is a store already holding WantTotal tasks in the
// fixture workspace, all matching the unfiltered TaskOperationsQuery. The
// conformance run below only reads, so one fixture serves every case.
type TaskOperationsFixture struct {
	Store     store.Store
	Context   context.Context
	WantTotal int
	WantOrder []string
	Filter    store.TaskFilter
}

type TaskAssigneeMembershipFixture struct {
	Store          store.Store
	Context        context.Context
	TaskID         string
	ActiveUserID   string
	InactiveUserID string
	NonMemberID    string
}

// RunTaskAssigneeMembershipConformance keeps the volatile and durable stores
// aligned at the command boundary: only an active workspace member may be
// assigned, and a rejected replacement must preserve the current assignee.
func RunTaskAssigneeMembershipConformance(t *testing.T, fixture TaskAssigneeMembershipFixture) {
	t.Helper()
	assigned, err := taskops.New(fixture.Store).SetAssignee(fixture.Context, fixture.TaskID, fixture.ActiveUserID)
	if err != nil || assigned.Assignee == nil || assigned.Assignee.UserID != fixture.ActiveUserID {
		t.Fatalf("assign active member: assignee=%+v err=%v", assigned.Assignee, err)
	}
	for name, userID := range map[string]string{"inactive member": fixture.InactiveUserID, "non-member": fixture.NonMemberID} {
		if _, err = taskops.New(fixture.Store).SetAssignee(fixture.Context, fixture.TaskID, userID); err == nil || !strings.Contains(err.Error(), "not an active member") {
			t.Fatalf("%s assignment error=%v", name, err)
		}
		current, getErr := fixture.Store.GetTask(fixture.Context, fixture.TaskID)
		if getErr != nil || current.Assignee == nil || current.Assignee.UserID != fixture.ActiveUserID {
			t.Fatalf("%s changed assignee=%+v err=%v", name, current.Assignee, getErr)
		}
	}
}

// RunTaskOperationsPaginationConformance asserts that every store answers the
// pagination bounds identically. The Postgres page query binds LIMIT/OFFSET as
// int32, so an offset past store.MaxTaskOperationsOffset used to wrap — into a
// negative bound Postgres rejects outright, or into a small positive one that
// pages from an unrelated row — while memory returned an empty page. Both
// stores now reject it, and both serve the boundary offset itself as an
// ordinary past-the-end page.
func RunTaskOperationsPaginationConformance(t *testing.T, fixture TaskOperationsFixture) {
	t.Helper()

	if len(fixture.WantOrder) > 0 {
		all, err := fixture.Store.ListTasks(fixture.Context)
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		assertTaskOrder(t, "unfiltered tasks", all, fixture.WantOrder)

		filtered, err := fixture.Store.ListTasksFiltered(fixture.Context, fixture.Filter)
		if err != nil {
			t.Fatalf("list filtered tasks: %v", err)
		}
		assertTaskOrder(t, "filtered tasks", filtered, fixture.WantOrder)

		operations, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{
			TaskFilter: fixture.Filter,
		})
		if err != nil {
			t.Fatalf("list task operations: %v", err)
		}
		assertTaskOrder(t, "task operations", operations.Tasks, fixture.WantOrder)

		for offset, wantID := range fixture.WantOrder {
			page, pageErr := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{
				TaskFilter: fixture.Filter, Limit: 1, Offset: offset,
			})
			if pageErr != nil {
				t.Fatalf("page at offset %d: %v", offset, pageErr)
			}
			assertTaskOrder(t, "paginated task operations", page.Tasks, []string{wantID})
		}
	}

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

func assertTaskOrder(t *testing.T, surface string, tasks []core.Task, want []string) {
	t.Helper()
	got := make([]string, len(tasks))
	for i := range tasks {
		got[i] = tasks[i].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s order=%v want %v", surface, got, want)
	}
}
