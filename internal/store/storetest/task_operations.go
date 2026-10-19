package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	ViewerUserID   string
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
	if _, err = taskops.New(fixture.Store).SetAssignee(fixture.Context, fixture.TaskID, fixture.ViewerUserID); err == nil || !strings.Contains(err.Error(), "viewer lacks claim_work capability") {
		t.Fatalf("viewer assignment error=%v", err)
	}
	current, getErr := fixture.Store.GetTask(fixture.Context, fixture.TaskID)
	if getErr != nil || current.Assignee == nil || current.Assignee.UserID != fixture.ActiveUserID {
		t.Fatalf("viewer replacement changed assignee=%+v err=%v", current.Assignee, getErr)
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

// TaskOperationsGroupReadFixture is a store whose fixture workspace holds
// tasks with a dependency, operations events, and a plan, and whose second
// workspace holds ForeignTaskID. Filter is an active filter selecting a
// nonempty subset of the fixture workspace.
type TaskOperationsGroupReadFixture struct {
	Store          store.Store
	Context        context.Context
	ForeignContext context.Context
	ForeignTaskID  string
	Filter         store.TaskFilter
}

// RunTaskOperationsGroupReadConformance pins the grouped task-operations
// store reads to ListTaskOperations on every backend (component-task-lifecycle,
// Task-operations reads): candidates are its rows with only the detail fields
// cleared, and the detail read returns its rows, events, and plans for any
// selection, with Total equal to the returned count.
func RunTaskOperationsGroupReadConformance(t *testing.T, fixture TaskOperationsGroupReadFixture) {
	t.Helper()
	ctx := fixture.Context
	for name, filter := range map[string]store.TaskFilter{"inactive filter": {}, "active filter": fixture.Filter} {
		rows, err := fixture.Store.ListTaskOperations(ctx, store.TaskOperationsQuery{TaskFilter: filter})
		requireOK(t, err)
		candidates, err := fixture.Store.ListTaskOperationsCandidates(ctx, filter)
		requireOK(t, err)
		if len(rows.Tasks) == 0 {
			t.Fatalf("%s: fixture selects no task", name)
		}
		assertTaskOrder(t, name+" candidates", candidates, taskIDs(rows.Tasks))
		for i := range candidates {
			if want := store.TaskOperationsCandidate(rows.Tasks[i]); !reflect.DeepEqual(candidates[i], want) {
				t.Fatalf("%s: candidate %s\n got %s\nwant %s", name, want.ID, taskOperationsJSON(candidates[i]), taskOperationsJSON(want))
			}
			if candidates[i].Body != "" || candidates[i].Dependencies != nil || candidates[i].GitHub != nil {
				t.Fatalf("%s: candidate %s carries detail fields: %s", name, candidates[i].ID, taskOperationsJSON(candidates[i]))
			}
		}
	}
	if _, err := fixture.Store.ListTaskOperationsCandidates(ctx, store.TaskFilter{UpdatedFrom: time.Unix(10, 0), UpdatedTo: time.Unix(10, 0)}); err == nil {
		t.Fatal("candidate read accepted an empty Updated range")
	}

	full, err := fixture.Store.ListTaskOperations(ctx, store.TaskOperationsQuery{})
	requireOK(t, err)
	all := taskIDs(full.Tasks)
	bodies, dependencies := false, false
	for _, task := range full.Tasks {
		bodies = bodies || task.Body != ""
		dependencies = dependencies || len(task.Dependencies) > 0
	}
	if !bodies || !dependencies || len(full.Events) == 0 || len(full.Plans) == 0 {
		t.Fatalf("fixture lacks a body, dependency, operations event, or plan: events=%d plans=%d", len(full.Events), len(full.Plans))
	}
	selection := slices.Clone(all)
	slices.Reverse(selection)
	selection = append(selection, all[0], "", "missing-"+all[0], fixture.ForeignTaskID)
	detail, err := fixture.Store.ListTaskOperationsForIDs(ctx, selection)
	requireOK(t, err)
	if detail.Total != len(detail.Tasks) || detail.Total != len(full.Tasks) {
		t.Fatalf("detail total=%d tasks=%d want %d", detail.Total, len(detail.Tasks), len(full.Tasks))
	}
	if !reflect.DeepEqual(detail.Tasks, full.Tasks) {
		t.Fatalf("detail rows\n got %s\nwant %s", taskOperationsJSON(detail.Tasks), taskOperationsJSON(full.Tasks))
	}
	if !reflect.DeepEqual(detail.Events, full.Events) || !reflect.DeepEqual(detail.Plans, full.Plans) {
		t.Fatalf("detail events or plans differ:\n got %s %s\nwant %s %s", taskOperationsJSON(detail.Events), taskOperationsJSON(detail.Plans), taskOperationsJSON(full.Events), taskOperationsJSON(full.Plans))
	}
	one, err := fixture.Store.ListTaskOperationsForIDs(ctx, []string{all[1]})
	requireOK(t, err)
	if one.Total != 1 || len(one.Tasks) != 1 || !reflect.DeepEqual(one.Tasks[0], full.Tasks[1]) {
		t.Fatalf("single selection total=%d tasks=%s", one.Total, taskOperationsJSON(one.Tasks))
	}
	for name, ids := range map[string][]string{"nil": nil, "empty": {}, "empty strings": {"", ""}, "missing and foreign": {"missing-" + all[0], fixture.ForeignTaskID}} {
		empty, err := fixture.Store.ListTaskOperationsForIDs(ctx, ids)
		requireOK(t, err)
		if empty.Total != 0 || len(empty.Tasks) != 0 || empty.Events == nil || empty.Plans == nil || len(empty.Events) != 0 || len(empty.Plans) != 0 {
			t.Fatalf("%s selection = %s", name, taskOperationsJSON(empty))
		}
	}
	bound := slices.Clone(all)
	for len(bound) < store.MaxTaskOperationsLimit {
		bound = append(bound, fmt.Sprintf("missing-%03d", len(bound)))
	}
	accepted, err := fixture.Store.ListTaskOperationsForIDs(ctx, bound)
	requireOK(t, err)
	if accepted.Total != len(all) {
		t.Fatalf("bound selection total=%d want %d", accepted.Total, len(all))
	}
	if _, err = fixture.Store.ListTaskOperationsForIDs(ctx, append(bound, "one-too-many")); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", store.MaxTaskOperationsLimit)) {
		t.Fatalf("oversized selection error=%v", err)
	}

	foreign, err := fixture.Store.ListTaskOperationsCandidates(fixture.ForeignContext, store.TaskFilter{})
	requireOK(t, err)
	assertTaskOrder(t, "foreign candidates", foreign, []string{fixture.ForeignTaskID})
	crossed, err := fixture.Store.ListTaskOperationsForIDs(fixture.ForeignContext, all)
	requireOK(t, err)
	if crossed.Total != 0 || len(crossed.Tasks) != 0 {
		t.Fatalf("foreign workspace read fixture tasks: %s", taskOperationsJSON(crossed.Tasks))
	}
	own, err := fixture.Store.ListTaskOperationsForIDs(fixture.ForeignContext, []string{fixture.ForeignTaskID})
	requireOK(t, err)
	assertTaskOrder(t, "foreign detail", own.Tasks, []string{fixture.ForeignTaskID})
}

func taskOperationsJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%+v", value)
	}
	return string(data)
}
