package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestLineageProjectorCoversDeliveryChain(t *testing.T) {
	events := []core.Event{
		{ID: 1, Kind: "planning_session.finalized", Payload: core.JSONPayload(map[string]any{"session_id": "plan-1", "produced_requirement_id": "req-1"})},
		{ID: 2, TaskID: "blueprint-1", Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": "req-1"})},
		{ID: 3, TaskID: "blueprint-1", Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})},
		{ID: 4, TaskID: "task-1", Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": "implement-1"})},
		{ID: 5, TaskID: "task-1", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "number": 7, "base_sha": "base", "head_sha": "abc"})},
		{ID: 6, TaskID: "task-1", Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "evidence_ids": []string{"artifact-1"}})},
		{ID: 7, TaskID: "task-1", Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "base_sha": "base", "head_sha": "abc"})},
	}
	wantKinds := map[string]bool{"produced_requirement": false, "serves": false, "versions": false, "dispatches": false, "submitted_as": false, "submitted_range": false, "produced_verdict": false, "supports": false, "merged_range": false}
	for index := range events {
		events[index].At = time.Now().UTC()
		// Round-trip through JSON because production events arrive as raw JSON;
		// this also exercises []any decoding for evidence IDs.
		raw, _ := json.Marshal(events[index])
		var event core.Event
		_ = json.Unmarshal(raw, &event)
		for _, link := range lineageLinksForEvent("demo", event) {
			wantKinds[link.Kind] = true
		}
	}
	for kind, found := range wantKinds {
		if !found {
			t.Errorf("projector did not produce %q edge", kind)
		}
	}
}

func TestLineageProjectorDoesNotInventCommitRanges(t *testing.T) {
	t.Parallel()
	for _, event := range []core.Event{
		{ID: 1, TaskID: "task-1", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "number": 7, "head_sha": "head-only"})},
		{ID: 2, TaskID: "task-1", Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "base_sha": "base-only"})},
	} {
		event.At = time.Now().UTC()
		for _, link := range lineageLinksForEvent("demo", event) {
			if link.DstType == core.LineageCommitRange {
				t.Fatalf("partial event projected commit range: %+v", link)
			}
		}
	}
}

func TestLineageProjectorSuppressesRemovedDependency(t *testing.T) {
	t.Parallel()
	want := core.LineageLink{Workspace: "demo", SrcType: core.LineageTask, SrcID: "task-1", DstType: core.LineageTask, DstID: "task-2", Kind: "depends_on", CreatedByEventID: 2}
	for _, payload := range []map[string]any{
		{"task_id": "task-1", "depends_on_task_id": "task-2"},
		{"depends_on_task_id": "task-2"},
	} {
		projection := ProjectLineageEvent("demo", core.Event{ID: 2, TaskID: "task-1", Kind: "task.dependency_removed", Payload: core.JSONPayload(payload)})
		if len(projection.Links) != 0 || projection.Unsupported != 0 || len(projection.Suppresses) != 1 {
			t.Fatalf("removal projection=%+v", projection)
		}
		if got := projection.Suppresses[0]; lineageLinkKey(got) != lineageLinkKey(want) {
			t.Fatalf("suppressed=%+v want key of %+v", got, want)
		}
	}
	malformed := ProjectLineageEvent("demo", core.Event{ID: 3, TaskID: "task-1", Kind: "task.dependency_removed", Payload: core.JSONPayload(map[string]any{"task_id": "task-1"})})
	if len(malformed.Suppresses) != 0 || malformed.Unsupported != 1 {
		t.Fatalf("removal without a dependency suppressed=%+v", malformed)
	}
}

func TestRequirementConfirmationWithoutPredecessorDoesNotSupersedeDraft(t *testing.T) {
	t.Parallel()
	event := core.Event{ID: 1, Kind: "requirement.version_confirmed", Payload: core.JSONPayload(map[string]any{
		"requirement_id": "req-1", "version": 3, "supersedes_version": 0,
	})}
	links := lineageLinksForEvent("demo", event)
	for _, link := range links {
		if link.Kind == "supersedes" {
			t.Fatalf("first confirmed version superseded an unconfirmed draft: %+v", links)
		}
	}
	// The confirmed version still joins its requirement (component-lineage).
	if len(links) != 1 || links[0].Kind != "versions" || links[0].SrcID != "req-1" || links[0].DstID != "req-1:v3" {
		t.Fatalf("confirmed version links=%+v, want requirement -versions-> req-1:v3 only", links)
	}
}

func TestMemoryLineageProjectsAndRebuildsFromEvents(t *testing.T) {
	const workspace = "lineage-memory"
	st := NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main"}}})
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), workspace)
	now := time.Now().UTC()
	parent := core.Task{ID: "blueprint", Workspace: workspace, Repo: "app", State: core.TaskRunning, CreatedAt: now}
	dependency := core.Task{ID: "dependency", Workspace: workspace, Repo: "app", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []core.Task{parent, dependency} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	child := core.Task{ID: "child", Workspace: workspace, Repo: "app", State: core.TaskRunning,
		ParentTaskID: parent.ID, OriginSpecVersion: 2, OriginSubID: "SUB-1", CreatedAt: now}
	if err := st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	// Each task.created also projects blueprint -plans-> task (component-lineage).
	assertMemoryLineage(t, st, ctx, 5)

	if _, err := st.RemoveTaskDependency(ctx, DependencyRemovalRequest{
		TaskID: child.ID, DependsOnTaskID: dependency.ID, Reason: "No longer blocks", RequestID: "remove-1",
	}); err != nil {
		t.Fatal(err)
	}
	// Removal suppresses the depends_on edge; the materializes edge stays
	// (component-lineage).
	assertMemoryLineage(t, st, ctx, 4)
	memoryStore := st.(*memory)
	memoryStore.lineage[lineageLinkKey(core.LineageLink{Workspace: workspace, SrcType: core.LineageTask, SrcID: child.ID, DstType: core.LineageTask, DstID: "legacy", Kind: "legacy_event_note"})] = core.LineageLink{
		Workspace: workspace, SrcType: core.LineageTask, SrcID: child.ID, DstType: core.LineageTask, DstID: "legacy", Kind: "legacy_event_note", CreatedByEventID: 1,
	}
	memoryStore.lineage[lineageLinkKey(core.LineageLink{Workspace: workspace, SrcType: core.LineageRequirement, SrcID: "legacy", DstType: core.LineageTask, DstID: child.ID, Kind: "historical_feature_assignment"})] = core.LineageLink{
		Workspace: workspace, SrcType: core.LineageRequirement, SrcID: "legacy", DstType: core.LineageTask, DstID: child.ID, Kind: "historical_feature_assignment",
	}
	if result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "test", RequestID: "memory-1"}); err != nil || result.Projected != 4 {
		t.Fatalf("rebuild result=%+v err=%v", result, err)
	} else if result.Existing != 2 {
		t.Fatalf("rebuild omitted retained legacy links: %+v", result)
	}
	assertMemoryLineage(t, st, ctx, 6)
}

func TestMemoryLineageRebuildPreservesUnregenerableProjectorLinks(t *testing.T) {
	const workspace = "lineage-preservation"
	st := NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main"}}}).(*memory)
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), workspace)
	task := core.Task{ID: "historical-task", Workspace: workspace, Repo: "app", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 53; number++ {
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": number})}); err != nil {
			t.Fatal(err)
		}
		events := st.events[task.ID]
		event := events[len(events)-1]
		link := core.LineageLink{Workspace: workspace, SrcType: core.LineageTask, SrcID: task.ID,
			DstType: core.LineagePullRequest, DstID: strconv.Itoa(number), Kind: "submitted_as",
			CreatedByEventID: event.ID, CreatedAt: event.At}
		st.lineage[lineageLinkKey(link)] = link
	}
	request := core.LineageRebuildRequest{Reason: "live-shaped repair", RequestID: "preserve-53"}
	result, err := st.RebuildLineage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// The task's own blueprint -plans-> task edge is the one projected row.
	if result.PreservedUnregenerable != 53 || result.Unsupported != 53 || result.Projected != 1 {
		t.Fatalf("rebuild result=%+v", result)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil || len(links) != 54 {
		t.Fatalf("links=%d err=%v", len(links), err)
	}
	if repeated, err := st.RebuildLineage(ctx, request); err != nil || repeated != result {
		t.Fatalf("idempotent result=%+v err=%v want=%+v", repeated, err, result)
	}
	if _, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "different", RequestID: request.RequestID}); !errors.Is(err, ErrLineageRebuildConflict) {
		t.Fatalf("request id conflict error=%v", err)
	}
}

// TestMemoryLineageRebuildDropsStaleRemovedDependencyRow models a depends_on
// row an earlier binary left in place after task.dependency_removed, before
// the projector suppressed it. Rebuild neither regenerates nor preserves the
// row (component-lineage).
func TestMemoryLineageRebuildDropsStaleRemovedDependencyRow(t *testing.T) {
	const workspace = "lineage-stale-dependency"
	st := NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main"}}}).(*memory)
	ctx := WithWorkspace(WithActor(t.Context(), Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	for _, id := range []string{"dependent", "dependency"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "app", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddTaskDependency(ctx, DependencyAdditionRequest{TaskID: "dependent", DependsOnTaskID: "dependency", Reason: "order", RequestID: "add"}); err != nil {
		t.Fatal(err)
	}
	var stale core.LineageLink
	for _, link := range st.lineage {
		if link.Kind == "depends_on" {
			stale = link
		}
	}
	if stale.CreatedByEventID == 0 {
		t.Fatal("dependency addition projected no edge")
	}
	if _, err := st.RemoveTaskDependency(ctx, DependencyRemovalRequest{TaskID: "dependent", DependsOnTaskID: "dependency", Reason: "unordered", RequestID: "remove"}); err != nil {
		t.Fatal(err)
	}
	st.lineage[lineageLinkKey(stale)] = stale
	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "drop stale dependency", RequestID: "stale-dependency"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the two tasks' blueprint -plans-> task edges are projected.
	if result.PreservedUnregenerable != 0 || result.Ambiguous != 0 || result.Projected != 2 {
		t.Fatalf("rebuild result=%+v, want the stale row dropped", result)
	}
	if _, exists := st.lineage[lineageLinkKey(stale)]; exists {
		t.Fatalf("rebuild kept stale dependency row %+v", stale)
	}
}

func TestMemoryLineageRebuildClassifiesRegeneratedLegacyKeyOnlyAsProjected(t *testing.T) {
	const workspace = "lineage-overlap"
	st := NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main"}}}).(*memory)
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), workspace)
	task := core.Task{ID: "overlap-task", Workspace: workspace, Repo: "app", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": "overlap-order"})}); err != nil {
		t.Fatal(err)
	}
	key := lineageLinkKey(core.LineageLink{Workspace: workspace, SrcType: core.LineageTask, SrcID: task.ID,
		DstType: core.LineageWorkOrder, DstID: "overlap-order", Kind: "dispatches"})
	legacy := st.lineage[key]
	legacy.CreatedByEventID = 0
	legacy.LegacyCreatedByEvent = "legacy.dispatch"
	st.lineage[key] = legacy

	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "overlap classification", RequestID: core.NewTaskID()})
	if err != nil {
		t.Fatal(err)
	}
	// The regenerated dispatches key and the task's plans edge are projected.
	if result.Projected != 2 || result.Existing != 0 || result.PreservedUnregenerable != 0 {
		t.Fatalf("overlapping legacy key was double-classified: %+v", result)
	}
}

func assertMemoryLineage(t *testing.T, st Store, ctx context.Context, want int) {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil || len(links) != want {
		t.Fatalf("lineage links=%+v err=%v, want %d", links, err, want)
	}
	for _, link := range links {
		if !projectorOwnsLineageKind(link.Kind) {
			continue
		}
		if err := link.Validate(); err != nil {
			t.Fatalf("invalid projected link %+v: %v", link, err)
		}
	}
}

// TestLineageProjectorConnectsRequirementVersionsPlansAndSuccessors covers
// the projector cases that join requirements to their versions, a served
// version to its task, a task's plan side to its delivery side, and a retired
// task to its start-over successor (component-lineage; req-260802-72fc68
// AC-1.1, AC-5.1, AC-5.2).
func TestLineageProjectorConnectsRequirementVersionsPlansAndSuccessors(t *testing.T) {
	t.Parallel()
	keys := func(links []core.LineageLink) []string {
		out := make([]string, 0, len(links))
		for _, link := range links {
			out = append(out, string(link.SrcType)+":"+link.SrcID+" -"+link.Kind+"-> "+string(link.DstType)+":"+link.DstID)
		}
		sort.Strings(out)
		return out
	}
	project := func(kind, taskID string, payload map[string]any) LineageEventProjection {
		return ProjectLineageEvent("demo", core.Event{ID: 7, TaskID: taskID, Kind: kind, Payload: core.JSONPayload(payload)})
	}
	for _, test := range []struct {
		name        string
		kind        string
		payload     map[string]any
		links       []string
		suppresses  []string
		unsupported int
	}{
		{name: "first confirmed version joins its requirement", kind: "requirement.version_confirmed", payload: map[string]any{"requirement_id": "req-a", "version": 1},
			links: []string{"requirement:req-a -versions-> requirement_version:req-a:v1"}},
		{name: "later version joins and supersedes", kind: "requirement.version_confirmed", payload: map[string]any{"requirement_id": "req-a", "version": 3, "supersedes_version": 2},
			links: []string{"requirement:req-a -versions-> requirement_version:req-a:v3", "requirement_version:req-a:v3 -supersedes-> requirement_version:req-a:v2"}},
		{name: "versioned attachment serves from both nodes", kind: TaskContextRequirementAdded, payload: map[string]any{"id": "req-a", "version": 2},
			links: []string{"requirement:req-a -serves-> task:task-1", "requirement_version:req-a:v2 -serves-> task:task-1"}},
		{name: "historical attachment stays unversioned", kind: TaskContextRequirementAdded, payload: map[string]any{"id": "req-a"},
			links: []string{"requirement:req-a -serves-> task:task-1"}},
		{name: "unconfirmed attachment projects nothing", kind: TaskContextRequirementAdded, payload: map[string]any{"id": "req-a", "pending_version": 4, "unconfirmed": true}},
		{name: "activation serves from both nodes", kind: TaskContextRequirementActive, payload: map[string]any{"id": "req-a", "version": 4},
			links: []string{"requirement:req-a -serves-> task:task-1", "requirement_version:req-a:v4 -serves-> task:task-1"}},
		{name: "removal suppresses every recorded version", kind: TaskContextRequirementRemoved, payload: map[string]any{"id": "req-a", "versions": []int{2, 4}},
			suppresses: []string{"requirement:req-a -serves-> task:task-1", "requirement_version:req-a:v2 -serves-> task:task-1", "requirement_version:req-a:v4 -serves-> task:task-1"}},
		{name: "historical removal suppresses the requirement edge", kind: TaskContextRequirementRemoved, payload: map[string]any{"id": "req-a"},
			suppresses: []string{"requirement:req-a -serves-> task:task-1"}},
		{name: "task creation joins its blueprint", kind: "task.created", payload: map[string]any{"id": "task-1"},
			links: []string{"blueprint:task-1 -plans-> task:task-1"}},
		{name: "child creation joins its blueprint and materializes", kind: "task.created", payload: map[string]any{"parent_task_id": "parent", "origin_spec_version": 2},
			links: []string{"blueprint:task-1 -plans-> task:task-1", "blueprint_version:parent:v2 -materializes-> task:task-1"}},
		{name: "start-over links the successor", kind: "task.started_over", payload: map[string]any{"successor_id": "task-2", "reason": "retry"},
			links: []string{"task:task-1 -succeeded_by-> task:task-2"}},
		{name: "start-over without a successor is unsupported", kind: "task.started_over", payload: map[string]any{"reason": "retry"}, unsupported: 1},
	} {
		projection := project(test.kind, "task-1", test.payload)
		if got := keys(projection.Links); !reflect.DeepEqual(got, append([]string{}, test.links...)) {
			t.Errorf("%s: links=%v want %v", test.name, got, test.links)
		}
		if got := keys(projection.Suppresses); !reflect.DeepEqual(got, append([]string{}, test.suppresses...)) {
			t.Errorf("%s: suppresses=%v want %v", test.name, got, test.suppresses)
		}
		if projection.Unsupported != test.unsupported {
			t.Errorf("%s: unsupported=%d want %d", test.name, projection.Unsupported, test.unsupported)
		}
	}
	if projection := ProjectLineageEvent("demo", core.Event{ID: 8, Kind: "task.created", Payload: core.JSONPayload(map[string]any{})}); len(projection.Links) != 0 {
		t.Fatalf("task.created without a task projected %+v", projection.Links)
	}
	for _, kind := range []string{"plans", "succeeded_by"} {
		if !projectorOwnsLineageKind(kind) {
			t.Fatalf("projector does not own %s", kind)
		}
	}
}

// TestLineageReplaySuppressesHistoricalVersionlessRemoval proves that replay
// ends every version edge an attachment projected when its historical removal
// records no versions, that a removal recording its versions adds no duplicate
// suppression, and that the attachment fold the removal writers use records
// the same versions (component-lineage; req-260802-72fc68 AC-1.3).
func TestLineageReplaySuppressesHistoricalVersionlessRemoval(t *testing.T) {
	t.Parallel()
	events := []core.Event{
		{ID: 1, TaskID: "task-1", Kind: TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-a", "version": 1})},
		{ID: 2, TaskID: "task-1", Kind: TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-b", "pending_version": 3, "unconfirmed": true})},
		{ID: 3, TaskID: "task-1", Kind: TaskContextRequirementActive, Payload: core.JSONPayload(map[string]any{"id": "req-a", "version": 2})},
		{ID: 4, TaskID: "task-2", Kind: TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-a", "version": 2})},
	}
	if got := ActiveTaskContextRequirementVersions(events); !reflect.DeepEqual(got, map[string][]int{"req-a": {1, 2}}) {
		t.Fatalf("folded versions=%v", got)
	}
	replay := NewLineageReplay()
	for _, event := range events {
		replay.Project("demo", event)
	}
	suppressed := func(projection LineageEventProjection) []string {
		out := []string{}
		for _, link := range projection.Suppresses {
			out = append(out, link.SrcID+"->"+link.DstID)
		}
		sort.Strings(out)
		return out
	}
	historical := replay.Project("demo", core.Event{ID: 5, TaskID: "task-1", Kind: TaskContextRequirementRemoved, Payload: core.JSONPayload(map[string]any{"id": "req-a"})})
	if got := suppressed(historical); !reflect.DeepEqual(got, []string{"req-a->task-1", "req-a:v1->task-1", "req-a:v2->task-1"}) {
		t.Fatalf("historical removal suppressed %v", got)
	}
	readd := core.Event{ID: 6, TaskID: "task-1", Kind: TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-a", "version": 3})}
	replay.Project("demo", readd)
	recorded := replay.Project("demo", core.Event{ID: 7, TaskID: "task-1", Kind: TaskContextRequirementRemoved, Payload: core.JSONPayload(RequirementRemovalPayload("req-a", []int{3}))})
	if got := suppressed(recorded); !reflect.DeepEqual(got, []string{"req-a->task-1", "req-a:v3->task-1"}) {
		t.Fatalf("recorded removal suppressed %v", got)
	}
	other := replay.Project("demo", core.Event{ID: 8, TaskID: "task-2", Kind: TaskContextRequirementRemoved, Payload: core.JSONPayload(map[string]any{"id": "req-a"})})
	if got := suppressed(other); !reflect.DeepEqual(got, []string{"req-a->task-2", "req-a:v2->task-2"}) {
		t.Fatalf("another task's removal suppressed %v", got)
	}
}
