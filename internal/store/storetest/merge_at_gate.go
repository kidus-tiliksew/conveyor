package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runCheckStatusDeadline is the shared check-status storage contract
// (req-review-gates-evidence AC-1.5; component-submission-merge, Check status):
// the deadline and its event commit together, a later read of the same head
// neither moves it nor appends, clearing keeps the head, and a new head starts
// fresh. Every task read returns the columns.
func runCheckStatusDeadline(t *testing.T, x Fixture) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	event := func(id, head, reason string) core.Event {
		return core.Event{TaskID: id, Kind: store.EventCheckPending, Payload: core.JSONPayload(map[string]any{"head_sha": head, "reason": reason})}
	}
	count := func(t *testing.T, id string) int {
		t.Helper()
		events, err := st.ListCheckStatusEvents(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return len(events[id])
	}
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	id := createRunningTask(t, st, ctx, workspace)

	fresh, err := st.GetTask(ctx, id)
	if err != nil || fresh.CheckStatusDeadline != nil || fresh.CheckStatusDeadlineHead != "" {
		t.Fatalf("new task deadline=%v head=%q err=%v", fresh.CheckStatusDeadline, fresh.CheckStatusDeadlineHead, err)
	}
	recorded, err := st.RecordCheckStatusDeadline(ctx, id, "head-a", base, event(id, "head-a", "pending"))
	if err != nil || !recorded {
		t.Fatalf("first record=%t err=%v", recorded, err)
	}
	read := func(t *testing.T, wantHead string, want *time.Time) {
		t.Helper()
		got, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.CheckStatusDeadlineHead != wantHead {
			t.Fatalf("head = %q, want %q", got.CheckStatusDeadlineHead, wantHead)
		}
		if (want == nil) != (got.CheckStatusDeadline == nil) || (want != nil && !got.CheckStatusDeadline.Equal(*want)) {
			t.Fatalf("deadline = %v, want %v", got.CheckStatusDeadline, want)
		}
		listed, err := st.ListTasksFiltered(ctx, store.TaskFilter{States: []core.TaskState{got.State}})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range listed {
			if candidate.ID == id && candidate.CheckStatusDeadlineHead != wantHead {
				t.Fatalf("listed head = %q, want %q", candidate.CheckStatusDeadlineHead, wantHead)
			}
		}
	}
	read(t, "head-a", &base)
	if count(t, id) != 1 {
		t.Fatalf("events = %d, want 1", count(t, id))
	}

	later := base.Add(time.Hour)
	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-a", later, event(id, "head-a", "pending"))
	if err != nil || recorded {
		t.Fatalf("second record=%t err=%v, want no change", recorded, err)
	}
	read(t, "head-a", &base)
	if count(t, id) != 1 {
		t.Fatalf("a later read appended: %d", count(t, id))
	}

	cleared, err := st.ClearCheckStatusDeadline(ctx, id)
	if err != nil || !cleared {
		t.Fatalf("clear=%t err=%v", cleared, err)
	}
	read(t, "head-a", nil)
	if cleared, err = st.ClearCheckStatusDeadline(ctx, id); err != nil || cleared {
		t.Fatalf("second clear=%t err=%v", cleared, err)
	}

	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-a", later, event(id, "head-a", "pending"))
	if err != nil || !recorded {
		t.Fatalf("pending again record=%t err=%v", recorded, err)
	}
	read(t, "head-a", &later)

	newHead := later.Add(time.Hour)
	recorded, err = st.RecordCheckStatusDeadline(ctx, id, "head-b", newHead, event(id, "head-b", "read_failed"))
	if err != nil || !recorded {
		t.Fatalf("new head record=%t err=%v", recorded, err)
	}
	read(t, "head-b", &newHead)
	if count(t, id) != 3 {
		t.Fatalf("events = %d, want 3", count(t, id))
	}

	if _, err = st.RecordCheckStatusDeadline(ctx, id, " ", later, event(id, "", "")); !errors.Is(err, store.ErrCheckStatusHeadRequired) {
		t.Fatalf("blank head err = %v", err)
	}
	if _, err = st.RecordCheckStatusDeadline(ctx, "missing-task", "head", later, event("missing-task", "head", "")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task record err = %v", err)
	}
	if _, err = st.ClearCheckStatusDeadline(ctx, "missing-task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task clear err = %v", err)
	}

	// The retry enqueues one dispatch_task job, only for an approved gate-off
	// task, and a job that is still active holds the stream.
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, id); err != nil || inserted {
		t.Fatalf("running task enqueue inserted=%t err=%v", inserted, err)
	}
	if _, err = st.EnqueueCheckStatusRetry(ctx, "missing-task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task enqueue err = %v", err)
	}
	approved := createTask(t, st, ctx, workspace, core.TaskApproved, "")
	stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), approved)
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, approved); err != nil || !inserted {
		t.Fatalf("approved task enqueue inserted=%t err=%v", inserted, err)
	}
	if inserted, err := st.EnqueueCheckStatusRetry(ctx, approved); err != nil || inserted {
		t.Fatalf("second enqueue inserted=%t err=%v", inserted, err)
	}
	job, err := logqueue.Load(ctx, st.Log(), workspace, stream)
	if err != nil || job.Generation != 1 || job.State != logqueue.StateAvailable || job.MaxAttempts != queue.DispatchTaskMaxAttempts {
		t.Fatalf("job=%+v err=%v", job, err)
	}

	// The batch read returns only the check-status kinds in (at, id) order and
	// stays inside the workspace.
	for _, kind := range []string{store.EventCheckHold, store.EventCheckHoldCleared, "merge.requested", store.EventCheckOverride} {
		if err = st.AppendEvent(ctx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"head_sha": "head-b"})}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListCheckStatusEvents(ctx, []string{id, "missing-task"})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, recordedEvent := range events[id] {
		kinds = append(kinds, recordedEvent.Kind)
	}
	want := []string{store.EventCheckPending, store.EventCheckPending, store.EventCheckPending, store.EventCheckHold, store.EventCheckHoldCleared}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if _, listedMissing := events["missing-task"]; listedMissing {
		t.Fatal("a missing task was listed")
	}
	empty, err := st.ListCheckStatusEvents(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty read = %+v, %v", empty, err)
	}
	foreign, err := st.ListCheckStatusEvents(store.WithWorkspace(ctx, workspace+"-foreign"), []string{id})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign workspace read = %+v, %v", foreign, err)
	}
}
