package store

import (
	"context"
	"encoding/json"
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

func TestMemoryLineageProjectsAndRebuildsFromEvents(t *testing.T) {
	const workspace = "lineage-memory"
	st := NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main"}}})
	ctx := WithWorkspace(t.Context(), workspace)
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
	assertMemoryLineage(t, st, ctx, 2)

	if _, err := st.RemoveTaskDependency(ctx, DependencyRemovalRequest{
		TaskID: child.ID, DependsOnTaskID: dependency.ID, Reason: "No longer blocks", RequestID: "remove-1",
	}); err != nil {
		t.Fatal(err)
	}
	// The operational edge is gone, but the immutable historical lineage stays.
	assertMemoryLineage(t, st, ctx, 2)
	if count, err := st.RebuildLineage(ctx); err != nil || count != 2 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	assertMemoryLineage(t, st, ctx, 2)
}

func assertMemoryLineage(t *testing.T, st Store, ctx context.Context, want int) {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil || len(links) != want {
		t.Fatalf("lineage links=%+v err=%v, want %d", links, err, want)
	}
	for _, link := range links {
		if err := link.Validate(); err != nil {
			t.Fatalf("invalid projected link %+v: %v", link, err)
		}
	}
}
