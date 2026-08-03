package storetest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type LineageFixture struct {
	Store     store.Store
	Context   context.Context
	Workspace string
}

type LineageFactory func(*testing.T, []config.Repo) LineageFixture

// RunLineageConformance drives ordinary store writers and asserts the common
// event projection/rebuild contract for memory and Postgres.
func RunLineageConformance(t *testing.T, factory LineageFactory) {
	t.Helper()
	fixture := factory(t, []config.Repo{{Name: "conveyor", Base: "main"}})
	st, ctx := fixture.Store, fixture.Context
	now := time.Now().UTC()
	parent := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	dependency := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []*core.Task{&parent, &dependency} {
		task.BaseBranch = "main"
		task.Branch = "conveyor/task-" + task.ID
		task.Title = task.ID
	}
	for _, task := range []core.Task{parent, dependency} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	child := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning,
		ParentTaskID: parent.ID, OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: now}
	child.BaseBranch, child.Branch, child.Title = "main", "conveyor/task-"+child.ID, child.ID
	if err := st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	before := lineageSnapshot(t, st, ctx)
	if !hasLineageKind(before, "depends_on") {
		t.Fatalf("real dependency writer omitted lineage: %v", before)
	}
	if !hasLineageKind(before, "materializes") {
		t.Fatalf("real task writer omitted lineage: %v", before)
	}
	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "conformance", RequestID: core.NewTaskID()})
	if err != nil {
		t.Fatal(err)
	}
	after := lineageSnapshot(t, st, ctx)
	if result.Projected != len(after) {
		t.Fatalf("result=%+v links=%v", result, after)
	}
	if len(before) != len(after) {
		t.Fatalf("before=%v after=%v", before, after)
	}
	for key, eventID := range before {
		if after[key] != eventID {
			t.Fatalf("edge %s provenance before=%d after=%d", key, eventID, after[key])
		}
	}
}

func lineageSnapshot(t *testing.T, st store.Store, ctx context.Context) map[string]int64 {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Kind < links[j].Kind })
	out := map[string]int64{}
	for _, link := range links {
		if !link.SrcType.Valid() || !link.DstType.Valid() || link.CreatedByEventID == 0 {
			t.Fatalf("invalid lineage link: %+v", link)
		}
		out[link.Kind+"\x00"+link.SrcID+"\x00"+link.DstID] = link.CreatedByEventID
	}
	return out
}

func hasLineageKind(snapshot map[string]int64, kind string) bool {
	for key := range snapshot {
		if len(key) > len(kind) && key[:len(kind)+1] == kind+"\x00" {
			return true
		}
	}
	return false
}
