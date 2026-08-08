package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The shared Tasks/Board filter is written twice — once as Go over the memory
// store's maps, once as SQL over Postgres — so it is asserted from one place.
// The fixture and every case live here, and each store only supplies itself: a
// member that means one thing in Go and another in SQL fails in both suites at
// once (AC-2.4).

// TaskFilterFixture is the workspace the shared cases assert over. Suffix keeps
// the seeded task IDs unique, because Postgres task IDs are global.
type TaskFilterFixture struct {
	Store     store.Store
	Context   context.Context
	Workspace string
	Repo      string
	Suffix    string
}

func (f TaskFilterFixture) id(name string) string { return name + "-" + f.Suffix }

// filterInstant keeps the fixture's timestamps far from any wall clock, so a
// bound never accidentally matches because a test ran at the wrong moment.
func filterInstant(month, day int) time.Time {
	return time.Date(2026, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

// SeedTaskFilterFixture writes three tasks whose text, last activity, and
// attached context differ along exactly one axis at a time.
func SeedTaskFilterFixture(t *testing.T, fixture TaskFilterFixture) {
	t.Helper()
	for _, seed := range []struct {
		name   string
		title  string
		source string
		state  core.TaskState
	}{
		{"alpha", "Alpha ledger sweep", "operator", core.TaskQueued},
		{"beta", "Beta ledger audit", "github", core.TaskRunning},
		{"gamma", "Gamma rollout", "operator", core.TaskQueued},
	} {
		id := fixture.id(seed.name)
		if err := fixture.Store.CreateTask(fixture.Context, core.Task{
			ID: id, Workspace: fixture.Workspace, Title: seed.title, Source: seed.source,
			Repo: fixture.Repo, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: seed.state, NextStage: core.StageImplement, CreatedAt: filterInstant(1, 1),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// Alpha keeps the requirement it was given; Beta gives one back and takes a
	// design instead. Both stores must read the latest add-or-remove per
	// document rather than the presence of any attachment event. These land
	// before the activity events below, so they never stand in as a task's last
	// activity.
	for _, attachment := range []struct {
		task string
		kind string
		id   string
	}{
		{"alpha", store.TaskContextRequirementAdded, "req-ledger"},
		{"beta", store.TaskContextRequirementAdded, "req-ledger"},
		{"beta", store.TaskContextRequirementRemoved, "req-ledger"},
		{"beta", store.TaskContextDesignAdded, "design-ledger"},
	} {
		if err := fixture.Store.AppendEvent(fixture.Context, core.Event{
			TaskID: fixture.id(attachment.task), Kind: attachment.kind, At: filterInstant(1, 2),
			Payload: core.JSONPayload(map[string]any{"id": attachment.id, "version": 1}),
		}); err != nil {
			t.Fatalf("seed %s attachment: %v", attachment.task, err)
		}
	}
	// Gamma is deliberately left without an event, so its last activity is its
	// creation time — the fallback the surfaces render and the filter shares.
	for _, activity := range []struct {
		task string
		at   time.Time
	}{
		{"alpha", filterInstant(3, 1)},
		{"beta", filterInstant(6, 1)},
	} {
		if err := fixture.Store.AppendEvent(fixture.Context, core.Event{
			TaskID: fixture.id(activity.task), Kind: "task.state_changed", At: activity.at,
			Payload: core.JSONPayload(map[string]any{"state": "running"}),
		}); err != nil {
			t.Fatalf("seed %s activity: %v", activity.task, err)
		}
	}
}

// RunTaskFilterConformance asserts that both entry points onto the shared
// predicate — the Board's ListTasksFiltered and the Tasks list's
// ListTaskOperations — select the same rows for the same filter.
func RunTaskFilterConformance(t *testing.T, fixture TaskFilterFixture) {
	t.Helper()
	for _, testCase := range []struct {
		name   string
		filter store.TaskFilter
		want   []string
	}{
		{"state", store.TaskFilter{States: []core.TaskState{core.TaskRunning}}, []string{"beta"}},
		// A list member is a disjunction: any listed value matches (AC-2.4).
		{"several states", store.TaskFilter{
			States: []core.TaskState{core.TaskRunning, core.TaskQueued},
		}, []string{"alpha", "beta", "gamma"}},
		{"repository", store.TaskFilter{Repositories: []string{fixture.Repo}}, []string{"alpha", "beta", "gamma"}},
		{"unknown repository", store.TaskFilter{Repositories: []string{"absent"}}, nil},
		{"several repositories", store.TaskFilter{
			Repositories: []string{"absent", fixture.Repo},
		}, []string{"alpha", "beta", "gamma"}},
		{"free text on title", store.TaskFilter{Query: "ledger"}, []string{"alpha", "beta"}},
		{"free text ignores case", store.TaskFilter{Query: "LeDgEr"}, []string{"alpha", "beta"}},
		{"free text on source", store.TaskFilter{Query: "github"}, []string{"beta"}},
		{"free text on id", store.TaskFilter{Query: fixture.id("gamma")}, []string{"gamma"}},
		{"free text on branch", store.TaskFilter{Query: "conveyor/task-" + fixture.id("alpha")}, []string{"alpha"}},
		// A needle is a literal, so SQL wildcards an operator types are searched
		// for rather than expanded into "everything".
		{"free text treats wildcards literally", store.TaskFilter{Query: "%"}, nil},
		{"free text underscore is literal", store.TaskFilter{Query: "_"}, nil},
		{"updated from is inclusive", store.TaskFilter{UpdatedFrom: filterInstant(6, 1)}, []string{"beta"}},
		{"updated to is exclusive", store.TaskFilter{UpdatedTo: filterInstant(6, 1)}, []string{"alpha", "gamma"}},
		{"updated range", store.TaskFilter{
			UpdatedFrom: filterInstant(2, 1), UpdatedTo: filterInstant(5, 1),
		}, []string{"alpha"}},
		// Gamma has no events at all, so its creation time is what the range
		// must see.
		{"updated range falls back to creation", store.TaskFilter{
			UpdatedFrom: filterInstant(1, 1), UpdatedTo: filterInstant(2, 1),
		}, []string{"gamma"}},
		{"served requirement", store.TaskFilter{ServesRequirementIDs: []string{"req-ledger"}}, []string{"alpha"}},
		{"detached requirement", store.TaskFilter{ServesRequirementIDs: []string{"req-absent"}}, nil},
		// Beta removed req-ledger, so listing it alongside an absent document
		// still selects only the task that kept it: the fold stays per-document
		// even when several are listed.
		{"several requirements", store.TaskFilter{
			ServesRequirementIDs: []string{"req-absent", "req-ledger"},
		}, []string{"alpha"}},
		{"governing design", store.TaskFilter{GoverningDesignIDs: []string{"design-ledger"}}, []string{"beta"}},
		{"several designs", store.TaskFilter{
			GoverningDesignIDs: []string{"design-absent", "design-ledger"},
		}, []string{"beta"}},
		{"members intersect", store.TaskFilter{
			States: []core.TaskState{core.TaskQueued}, Query: "ledger",
		}, []string{"alpha"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := make([]string, len(testCase.want))
			for i, name := range testCase.want {
				want[i] = fixture.id(name)
			}
			tasks, err := fixture.Store.ListTasksFiltered(fixture.Context, testCase.filter)
			if err != nil {
				t.Fatalf("ListTasksFiltered: %v", err)
			}
			assertTaskIDs(t, "ListTasksFiltered", taskIDs(tasks), want)
			page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{
				TaskFilter: testCase.filter,
			})
			if err != nil {
				t.Fatalf("ListTaskOperations: %v", err)
			}
			assertTaskIDs(t, "ListTaskOperations", taskIDs(page.Tasks), want)
			if page.Total != len(want) {
				t.Fatalf("ListTaskOperations total=%d want %d", page.Total, len(want))
			}
		})
	}

	// An inverted range selects nothing at all, so both stores reject it at the
	// edge rather than rendering an empty workspace as if it were the answer.
	if _, err := fixture.Store.ListTasksFiltered(fixture.Context, store.TaskFilter{
		UpdatedFrom: filterInstant(6, 1), UpdatedTo: filterInstant(2, 1),
	}); err == nil {
		t.Fatal("inverted updated range accepted")
	}
}

func taskIDs(tasks []core.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func assertTaskIDs(t *testing.T, surface string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v want %v", surface, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v want %v", surface, got, want)
		}
	}
}
