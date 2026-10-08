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
	Store          store.Store
	Context        context.Context
	Workspace      string
	Repo           string
	Suffix         string
	AssigneeUserID string
	Assign         func(*testing.T, string, string)
	// ForeignContext is bound to a second workspace of the same store. The
	// Updated cases seed activity there to prove it never counts at home.
	ForeignContext   context.Context
	ForeignWorkspace string
}

func (f TaskFilterFixture) id(name string) string { return name + "-" + f.Suffix }

// filterInstant keeps the fixture's timestamps far from any wall clock, so a
// bound never accidentally matches because a test ran at the wrong moment.
func filterInstant(month, day int) time.Time {
	return time.Date(2026, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

// SeedTaskFilterFixture writes three tasks whose text, creation time, later
// activity, and attached context differ along exactly one axis at a time.
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
		createdAt := map[string]time.Time{
			"alpha": filterInstant(3, 1),
			"beta":  filterInstant(6, 1),
			"gamma": filterInstant(1, 1),
		}[seed.name]
		if err := fixture.Store.CreateTask(fixture.Context, core.Task{
			ID: id, Workspace: fixture.Workspace, Title: seed.title, Source: seed.source,
			Repo: fixture.Repo, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: seed.state, NextStage: core.StageImplement, CreatedAt: createdAt,
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
	// Later activity deliberately falls outside the creation windows below. It
	// must not change whether any task matches a Created filter.
	for _, activity := range []struct {
		task string
		at   time.Time
	}{
		{"alpha", filterInstant(7, 1)},
		{"beta", filterInstant(7, 2)},
	} {
		if err := fixture.Store.AppendEvent(fixture.Context, core.Event{
			TaskID: fixture.id(activity.task), Kind: "task.state_changed", At: activity.at,
			Payload: core.JSONPayload(map[string]any{"state": "running"}),
		}); err != nil {
			t.Fatalf("seed %s activity: %v", activity.task, err)
		}
	}
	if fixture.Assign != nil {
		fixture.Assign(t, fixture.id("alpha"), fixture.AssigneeUserID)
		fixture.Assign(t, fixture.id("beta"), fixture.AssigneeUserID)
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
		}, []string{"beta", "alpha", "gamma"}},
		{"repository", store.TaskFilter{Repositories: []string{fixture.Repo}}, []string{"beta", "alpha", "gamma"}},
		{"assignee", store.TaskFilter{Assignee: fixture.AssigneeUserID}, []string{"beta", "alpha"}},
		{"unassigned", store.TaskFilter{Assignee: "unassigned"}, []string{"gamma"}},
		{"unknown assignee", store.TaskFilter{Assignee: "usr-unknown"}, nil},
		{"unknown repository", store.TaskFilter{Repositories: []string{"absent"}}, nil},
		{"several repositories", store.TaskFilter{
			Repositories: []string{"absent", fixture.Repo},
		}, []string{"beta", "alpha", "gamma"}},
		{"free text on title", store.TaskFilter{Query: "ledger"}, []string{"beta", "alpha"}},
		{"free text ignores case", store.TaskFilter{Query: "LeDgEr"}, []string{"beta", "alpha"}},
		{"free text on source", store.TaskFilter{Query: "github"}, []string{"beta"}},
		{"free text on id", store.TaskFilter{Query: fixture.id("gamma")}, []string{"gamma"}},
		{"free text on branch", store.TaskFilter{Query: "conveyor/task-" + fixture.id("alpha")}, []string{"alpha"}},
		// A needle is a literal, so SQL wildcards an operator types are searched
		// for rather than expanded into "everything".
		{"free text treats wildcards literally", store.TaskFilter{Query: "%"}, nil},
		{"free text underscore is literal", store.TaskFilter{Query: "_"}, nil},
		{"created from is inclusive", store.TaskFilter{CreatedFrom: filterInstant(6, 1)}, []string{"beta"}},
		{"created to is exclusive", store.TaskFilter{CreatedTo: filterInstant(6, 1)}, []string{"alpha", "gamma"}},
		{"created range ignores later events", store.TaskFilter{
			CreatedFrom: filterInstant(2, 1), CreatedTo: filterInstant(5, 1),
		}, []string{"alpha"}},
		{"created range", store.TaskFilter{
			CreatedFrom: filterInstant(1, 1), CreatedTo: filterInstant(2, 1),
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

	// The same predicate must govern both the page and its total. Two assigned
	// rows make an accidentally post-filtered page or unfiltered count visible.
	for offset, want := range []string{"beta", "alpha"} {
		page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{
			TaskFilter: store.TaskFilter{Assignee: fixture.AssigneeUserID}, Limit: 1, Offset: offset,
		})
		if err != nil {
			t.Fatalf("assigned page %d: %v", offset, err)
		}
		assertTaskIDs(t, "assigned page", taskIDs(page.Tasks), []string{fixture.id(want)})
		if page.Total != 2 {
			t.Fatalf("assigned page %d total=%d want 2", offset, page.Total)
		}
	}

	// An inverted range selects nothing at all, so both stores reject it at the
	// edge rather than rendering an empty workspace as if it were the answer.
	if _, err := fixture.Store.ListTasksFiltered(fixture.Context, store.TaskFilter{
		CreatedFrom: filterInstant(6, 1), CreatedTo: filterInstant(2, 1),
	}); err == nil {
		t.Fatal("inverted created range accepted")
	}
}

// updatedInstant places the Updated fixture a year before the shared fixture
// and far from the wall clock. Assignment and other writes stamp events at the
// current time, so no seeded shared-fixture task can fall inside these windows.
func updatedInstant(month, day int) time.Time {
	return time.Date(2025, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

// RunTaskFilterUpdatedConformance asserts the Updated bounds (AC-2.4) on every
// backend after RunTaskFilterConformance has used the shared fixture. Updated
// is a task's last activity: the time of its newest event by (at, id), or its
// creation time before any event (component-persistence). Its own tasks carry
// the title token "Sundial" so lower-bound-only cases can exclude the shared
// fixture, whose assignment events land at the current time.
func RunTaskFilterUpdatedConformance(t *testing.T, fixture TaskFilterFixture) {
	t.Helper()
	create := func(ctx context.Context, workspace, name string, createdAt time.Time) string {
		t.Helper()
		id := fixture.id(name)
		if err := fixture.Store.CreateTask(ctx, core.Task{
			ID: id, Workspace: workspace, Title: "Sundial " + name, Source: "operator",
			Repo: fixture.Repo, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		return id
	}
	appendAt := func(ctx context.Context, taskID, kind string, at time.Time, payload map[string]any) {
		t.Helper()
		if err := fixture.Store.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, At: at, Payload: core.JSONPayload(payload)}); err != nil {
			t.Fatalf("seed %s %s: %v", taskID, kind, err)
		}
	}
	// old: created first, attached to a requirement, then active much later.
	old := create(fixture.Context, fixture.Workspace, "old", updatedInstant(1, 1))
	appendAt(fixture.Context, old, store.TaskContextRequirementAdded, updatedInstant(1, 2), map[string]any{"id": "req-sundial", "version": 1})
	appendAt(fixture.Context, old, "task.state_changed", updatedInstant(6, 1), map[string]any{"state": "running"})
	// late: its newest event is appended first. An older event appended
	// afterwards carries a higher ID, and a second event ties the newest time;
	// neither may move the Updated instant.
	late := create(fixture.Context, fixture.Workspace, "late", updatedInstant(2, 1))
	appendAt(fixture.Context, late, "task.state_changed", updatedInstant(5, 1), map[string]any{"state": "running"})
	appendAt(fixture.Context, late, "task.state_changed", updatedInstant(4, 1), map[string]any{"state": "queued"})
	appendAt(fixture.Context, late, "task.hold.set", updatedInstant(5, 1), map[string]any{"hold": true})
	// quiet: no event at all, so Updated falls back to creation.
	create(fixture.Context, fixture.Workspace, "quiet", updatedInstant(3, 1))

	for _, testCase := range []struct {
		name   string
		filter store.TaskFilter
		want   []string
	}{
		{"updated from inclusive", store.TaskFilter{Query: "sundial", UpdatedFrom: updatedInstant(5, 1)}, []string{"late", "old"}},
		{"updated to exclusive", store.TaskFilter{Query: "sundial", UpdatedTo: updatedInstant(5, 1)}, []string{"quiet"}},
		// old was created in January, outside this window; its June activity
		// is what selects it.
		{"updated range uses later activity", store.TaskFilter{
			UpdatedFrom: updatedInstant(5, 15), UpdatedTo: updatedInstant(6, 15),
		}, []string{"old"}},
		{"updated falls back to creation without events", store.TaskFilter{
			UpdatedFrom: updatedInstant(2, 15), UpdatedTo: updatedInstant(3, 15),
		}, []string{"quiet"}},
		// late's April event was appended after its May event; the newest
		// tuple is still May, so April selects nothing and May selects late.
		{"updated chooses newest event time", store.TaskFilter{
			UpdatedFrom: updatedInstant(4, 15), UpdatedTo: updatedInstant(5, 15),
		}, []string{"late"}},
		{"updated ignores an older event appended later", store.TaskFilter{
			UpdatedFrom: updatedInstant(3, 15), UpdatedTo: updatedInstant(4, 15),
		}, nil},
		{"updated intersects created and context filters (created)", store.TaskFilter{
			Query: "sundial", UpdatedFrom: updatedInstant(4, 15), CreatedFrom: updatedInstant(1, 15),
		}, []string{"late"}},
		{"updated intersects created and context filters (context)", store.TaskFilter{
			UpdatedFrom: updatedInstant(4, 15), UpdatedTo: updatedInstant(12, 1), ServesRequirementIDs: []string{"req-sundial"},
		}, []string{"old"}},
		{"updated intersects created and context filters (both families)", store.TaskFilter{
			UpdatedFrom: updatedInstant(4, 15), UpdatedTo: updatedInstant(12, 1),
			CreatedFrom: updatedInstant(1, 15), CreatedTo: updatedInstant(2, 15),
		}, []string{"late"}},
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
			page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{TaskFilter: testCase.filter})
			if err != nil {
				t.Fatalf("ListTaskOperations: %v", err)
			}
			assertTaskIDs(t, "ListTaskOperations", taskIDs(page.Tasks), want)
			if page.Total != len(want) {
				t.Fatalf("ListTaskOperations total=%d want %d", page.Total, len(want))
			}
		})
	}

	t.Run("updated pagination and total share predicate", func(t *testing.T) {
		filter := store.TaskFilter{Query: "sundial", UpdatedFrom: updatedInstant(4, 15)}
		for offset, name := range []string{"late", "old"} {
			page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{TaskFilter: filter, Limit: 1, Offset: offset})
			if err != nil {
				t.Fatalf("updated page %d: %v", offset, err)
			}
			assertTaskIDs(t, "updated page", taskIDs(page.Tasks), []string{fixture.id(name)})
			if page.Total != 2 {
				t.Fatalf("updated page %d total=%d want 2", offset, page.Total)
			}
		}
		page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{TaskFilter: filter, Limit: 1, Offset: 2})
		if err != nil {
			t.Fatalf("updated page past end: %v", err)
		}
		if len(page.Tasks) != 0 || page.Total != 2 {
			t.Fatalf("updated page past end = %v total=%d, want none of 2", taskIDs(page.Tasks), page.Total)
		}
	})

	t.Run("equal and inverted updated bounds rejected", func(t *testing.T) {
		for name, filter := range map[string]store.TaskFilter{
			"equal":    {UpdatedFrom: updatedInstant(5, 1), UpdatedTo: updatedInstant(5, 1)},
			"inverted": {UpdatedFrom: updatedInstant(6, 1), UpdatedTo: updatedInstant(2, 1)},
		} {
			if _, err := fixture.Store.ListTasksFiltered(fixture.Context, filter); err == nil {
				t.Fatalf("%s updated range accepted by ListTasksFiltered", name)
			}
			if _, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{TaskFilter: filter}); err == nil {
				t.Fatalf("%s updated range accepted by ListTaskOperations", name)
			}
		}
	})

	t.Run("updated ignores foreign workspace events", func(t *testing.T) {
		if fixture.ForeignContext == nil {
			t.Fatal("fixture has no foreign workspace")
		}
		foreign := create(fixture.ForeignContext, fixture.ForeignWorkspace, "foreign", updatedInstant(1, 1))
		appendAt(fixture.ForeignContext, foreign, "task.state_changed", updatedInstant(5, 20), map[string]any{"state": "running"})
		window := store.TaskFilter{UpdatedFrom: updatedInstant(5, 15), UpdatedTo: updatedInstant(5, 25)}
		// The foreign event selects its own task in its own workspace ...
		away, err := fixture.Store.ListTaskOperations(fixture.ForeignContext, store.TaskOperationsQuery{TaskFilter: window})
		if err != nil {
			t.Fatalf("foreign ListTaskOperations: %v", err)
		}
		assertTaskIDs(t, "foreign ListTaskOperations", taskIDs(away.Tasks), []string{foreign})
		// ... and contributes nothing at home, where no task was active then.
		home, err := fixture.Store.ListTasksFiltered(fixture.Context, window)
		if err != nil {
			t.Fatalf("home ListTasksFiltered: %v", err)
		}
		assertTaskIDs(t, "home ListTasksFiltered", taskIDs(home), nil)
		page, err := fixture.Store.ListTaskOperations(fixture.Context, store.TaskOperationsQuery{TaskFilter: window})
		if err != nil {
			t.Fatalf("home ListTaskOperations: %v", err)
		}
		if len(page.Tasks) != 0 || page.Total != 0 {
			t.Fatalf("home ListTaskOperations = %v total=%d, want none", taskIDs(page.Tasks), page.Total)
		}
	})
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
