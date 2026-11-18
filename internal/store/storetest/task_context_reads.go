package storetest

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runTaskContextReads proves the single-task context read states only the
// attachment kinds its fold uses, so its rows do not grow with a task's
// unrelated history (component-task-lifecycle invariant 53).
func runTaskContextReads(t *testing.T, x Fixture) {
	ctx := x.Context
	repo := x.Config.Repos[0]
	id := core.NewTaskID()
	requireOK(t, x.Backend.CreateTask(ctx, core.Task{
		ID: id, Workspace: x.Workspace, Source: "context-reads", Title: "Context reads",
		Repo: repo.Name, BaseBranch: repo.Base, Branch: "conveyor/task-" + id,
		State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC(),
	}))
	events := []core.Event{
		{ID: 1, TaskID: id, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "req-context-reads"}), At: time.Now().UTC()},
	}
	for i := range 50 {
		events = append(events, core.Event{ID: int64(i + 2), TaskID: id, Kind: "job.started", Payload: core.JSONPayload(map[string]any{"n": i}), At: time.Now().UTC().Add(time.Duration(i+1) * time.Second)})
	}
	events = append(events, core.Event{ID: 52, TaskID: id, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": "component-context-reads", "version": 3}), At: time.Now().UTC().Add(time.Minute)})
	x.SeedEvents(t, ctx, 0, events)

	narrow, err := x.Backend.ListTaskContextEvents(ctx, id)
	requireOK(t, err)
	if len(narrow) != 2 {
		t.Fatalf("context read returned %d events, want the 2 attachment events", len(narrow))
	}
	for _, event := range narrow {
		if !store.IsTaskContextEventKind(event.Kind) {
			t.Fatalf("context read returned the unrelated kind %q", event.Kind)
		}
	}
	ledger, err := x.Backend.ListCompactEvents(ctx, id)
	requireOK(t, err)
	want := make([]core.Event, 0, len(ledger))
	for _, event := range ledger {
		if store.IsTaskContextEventKind(event.Kind) {
			want = append(want, event)
		}
	}
	if len(want) != len(narrow) {
		t.Fatalf("context read returned %d events, the compact ledger holds %d attachment events", len(narrow), len(want))
	}
	for i := range want {
		if narrow[i].ID != want[i].ID || narrow[i].Kind != want[i].Kind || string(narrow[i].Payload) != string(want[i].Payload) {
			t.Fatalf("context read event %d = %+v, want %+v", i, narrow[i], want[i])
		}
	}
}
