package main

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestAssignmentResolveTaskWiresRecordedOwnerLookup(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	resolver := assignmentResolveTask(st, "app", "org/app")
	source := monitor.GitHubSource{ResolveTask: resolver}
	if source.ResolveTask == nil {
		t.Fatal("daemon GitHubSource ResolveTask is nil")
	}
	merged := core.Task{ID: "A", Workspace: "demo", Repo: "app", Branch: "feature/x", State: core.TaskMerged, CreatedAt: time.Now()}
	open := core.Task{ID: "B", Workspace: "demo", Repo: "app", Branch: "feature/x", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: "A", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 4, "repository": "org/app"})}); err != nil {
		t.Fatal(err)
	}
	id, ok, err := source.ResolveTask(ctx, "feature/x", 4)
	if err != nil || !ok || id != "A" {
		t.Fatalf("recorded owner A lost: id=%s ok=%t err=%v", id, ok, err)
	}
}
