package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runEventRecency pins chronological event recency and cursor semantics when
// event IDs are not monotonic in insertion order. SingleStore's per-aggregator
// AUTO_INCREMENT ranges produce such IDs in production; the controlled-ID
// fixture reproduces them on every backend (component-persistence; DEC-39).
func runEventRecency(t *testing.T, x Fixture) {
	if x.SeedEvents == nil {
		t.Fatal("factory must provide SeedEvents for event recency conformance")
	}
	t.Run("merge gate uses the newest (at,id) state change", func(t *testing.T) { runMergeGateRecency(t, x) })
	t.Run("ListEventsAfter resolves owned anchors chronologically", func(t *testing.T) { runEventAnchorCursor(t, x) })
	t.Run("stream windows page by (at,id) from an inclusive bound", func(t *testing.T) { runEventStreamWindows(t, x) })
	t.Run("activity markers and context filters use chronological recency", func(t *testing.T) { runChronologicalProjections(t, x) })
	t.Run("intervention windows compare time, not ID", func(t *testing.T) { runInterventionWindowRecency(t, x) })
	t.Run("numeric snapshot ceilings expose late lower-ID commits", func(t *testing.T) { runSnapshotCeilingResiduals(t, x) })
}

// seedTime is a microsecond-precise time after every ordinary fixture event,
// so seeded events sort last unless a case backdates them.
func seedTime() time.Time {
	return time.Now().UTC().Add(time.Hour).Truncate(time.Second)
}

func stateChanged(taskID string, command core.TaskCommand, at time.Time, rank int64) core.Event {
	return core.Event{ID: rank, TaskID: taskID, Kind: "task.state_changed", At: at, Payload: core.JSONPayload(map[string]any{"command": command})}
}

func transitionToMergeGate(t *testing.T, ctx context.Context, st store.Store, id string) {
	t.Helper()
	for _, command := range []taskops.Command{
		{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true},
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true},
	} {
		if _, err := taskops.New(st).Perform(ctx, id, command); err != nil {
			t.Fatalf("%s on %s: %v", command.Kind, id, err)
		}
	}
}

func runMergeGateRecency(t *testing.T, x Fixture) {
	st := x.Backend
	ctx := store.WithActor(x.Context, store.Actor{ID: "operator-recency", Role: core.ActorUser})
	requestChanges := func(id string) error {
		_, err := taskops.New(st).RequestChanges(ctx, taskops.RequestChanges{TaskID: id, Feedback: "chronological recency", MaxBounces: 5})
		return err
	}
	atGate := func(id string) (core.Task, []core.Event) {
		t.Helper()
		task, err := st.GetTask(ctx, id)
		requireOK(t, err)
		events, err := st.ListEvents(ctx, id)
		requireOK(t, err)
		return task, events
	}

	// The incident shape: an older state change holds a higher ID than the
	// newer gate.merge. ID-descending recency would select the older one.
	task := newAggregateTask(t, x)
	transitionToMergeGate(t, ctx, st, task.ID)
	at := seedTime()
	seeded := x.SeedEvents(t, ctx, 0, []core.Event{
		stateChanged(task.ID, core.TaskOrderClaim, at, 20),
		stateChanged(task.ID, core.TaskGateMerge, at.Add(2*time.Second), 10),
	})
	if seeded[1].ID >= seeded[0].ID {
		t.Fatalf("fixture did not produce decreasing IDs: %+v", seeded)
	}
	current, events := atGate(task.ID)
	if !store.AtMergeGate(current, events) {
		t.Fatalf("view does not report the merge gate for the newest (at,id) gate.merge: state=%s recovery=%s ids=%v", current.State, current.RecoveryStage, eventIDs(events))
	}
	reversed := make([]core.Event, len(events))
	for i := range events {
		reversed[len(events)-1-i] = events[i]
	}
	if !store.AtMergeGate(current, reversed) {
		t.Fatal("merge-gate recency depends on slice order")
	}
	if err := requestChanges(task.ID); err != nil {
		t.Fatalf("RequestChangesCommand refused a task whose newest gate.merge has a lower ID: %v", err)
	}
	after, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if after.State == core.TaskAwaiting && after.RecoveryStage == core.StageImplement && store.AtMergeGate(after, mustEvents(t, ctx, st, task.ID)) {
		t.Fatalf("request changes left the task at the merge gate: %+v", after)
	}

	// Equal timestamps are ordered by ID: a later-ID non-gate change at the
	// same instant is newer, and both the view and the command refuse.
	tie := newAggregateTask(t, x)
	transitionToMergeGate(t, ctx, st, tie.ID)
	tied := x.SeedEvents(t, ctx, 0, []core.Event{
		stateChanged(tie.ID, core.TaskGateMerge, at, 10),
		stateChanged(tie.ID, core.TaskOrderClaim, at, 20),
	})
	current, events = atGate(tie.ID)
	if store.AtMergeGate(current, events) {
		t.Fatalf("view reports the merge gate behind a same-time higher-ID state change: %+v", tied)
	}
	if err := requestChanges(tie.ID); err == nil || !strings.Contains(err.Error(), "not at the merge gate") {
		t.Fatalf("RequestChangesCommand accepted a superseded gate at an equal timestamp: %v", err)
	}

	// A backdated insert does not displace the chronologically newest gate,
	// even when it carries the highest ID.
	backdated := newAggregateTask(t, x)
	transitionToMergeGate(t, ctx, st, backdated.ID)
	x.SeedEvents(t, ctx, 0, []core.Event{stateChanged(backdated.ID, core.TaskOrderClaim, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 99)})
	current, events = atGate(backdated.ID)
	if !store.AtMergeGate(current, events) {
		t.Fatal("a backdated high-ID state change displaced the merge gate")
	}
	if err := requestChanges(backdated.ID); err != nil {
		t.Fatalf("RequestChangesCommand refused behind a backdated high-ID change: %v", err)
	}
}

func mustEvents(t *testing.T, ctx context.Context, st store.Store, id string) []core.Event {
	t.Helper()
	events, err := st.ListEvents(ctx, id)
	requireOK(t, err)
	return events
}

// seedDecreasing seeds four events whose IDs fall as time advances, with one
// timestamp tie: chronological order is A(30), B(10), C(20), D(5).
func seedDecreasing(t *testing.T, x Fixture, taskID string) []core.Event {
	at := seedTime()
	return x.SeedEvents(t, x.Context, 0, []core.Event{
		{ID: 30, TaskID: taskID, Kind: "fixture.a", At: at},
		{ID: 10, TaskID: taskID, Kind: "fixture.b", At: at.Add(time.Second)},
		{ID: 20, TaskID: taskID, Kind: "fixture.c", At: at.Add(time.Second)},
		{ID: 5, TaskID: taskID, Kind: "fixture.d", At: at.Add(2 * time.Second)},
	})
}

func eventIDs(events []core.Event) []int64 {
	ids := make([]int64, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

func requireChronological(t *testing.T, name string, events []core.Event) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		if !store.TaskEventBefore(events[i-1], events[i]) {
			t.Fatalf("%s is not ordered by (at,id): %v", name, eventIDs(events))
		}
	}
}

func requireIDs(t *testing.T, name string, got []core.Event, want ...int64) {
	t.Helper()
	ids := eventIDs(got)
	if len(ids) != len(want) {
		t.Fatalf("%s ids=%v want=%v", name, ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("%s ids=%v want=%v", name, ids, want)
		}
	}
}

func runEventAnchorCursor(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task, other := newAggregateTask(t, x), newAggregateTask(t, x)
	seeded := seedDecreasing(t, x, task.ID)
	a, b, c, d := seeded[0], seeded[1], seeded[2], seeded[3]

	all, err := st.ListEventsAfter(ctx, task.ID, 0)
	requireOK(t, err)
	requireChronological(t, "ListEventsAfter(0)", all)
	if len(all) < 4 {
		t.Fatalf("zero anchor returned %v", eventIDs(all))
	}
	requireIDs(t, "zero-anchor tail", all[len(all)-4:], a.ID, b.ID, c.ID, d.ID)
	ledger, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	requireIDs(t, "zero anchor matches ListEvents", all, eventIDs(ledger)...)

	for _, step := range []struct {
		anchor core.Event
		want   []int64
	}{{a, []int64{b.ID, c.ID, d.ID}}, {b, []int64{c.ID, d.ID}}, {c, []int64{d.ID}}, {d, nil}} {
		got, err := st.ListEventsAfter(ctx, task.ID, step.anchor.ID)
		requireOK(t, err)
		requireIDs(t, "after "+step.anchor.Kind, got, step.want...)
	}
	// The first ordinary event anchors a traversal through decreasing IDs.
	got, err := st.ListEventsAfter(ctx, task.ID, all[0].ID)
	requireOK(t, err)
	requireIDs(t, "after first event", got, eventIDs(all[1:])...)

	foreignEvents := mustEvents(t, ctx, st, other.ID)
	for name, anchor := range map[string]int64{"missing": d.ID + 7919, "other task": foreignEvents[0].ID} {
		if _, err := st.ListEventsAfter(ctx, task.ID, anchor); !errors.Is(err, store.ErrEventAnchorNotFound) || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s anchor err=%v, want ErrEventAnchorNotFound", name, err)
		}
	}
	foreign := store.WithWorkspace(ctx, "other-"+core.NewTaskID())
	if _, err := st.ListEventsAfter(foreign, task.ID, a.ID); !errors.Is(err, store.ErrEventAnchorNotFound) {
		t.Fatalf("foreign-workspace anchor err=%v", err)
	}
	empty, err := st.ListEventsAfter(foreign, task.ID, 0)
	requireOK(t, err)
	if len(empty) != 0 {
		t.Fatalf("foreign workspace read %v", eventIDs(empty))
	}
}

func drainStream(t *testing.T, st store.Store, ctx context.Context, q store.TaskEventStreamQuery) []core.Event {
	t.Helper()
	var out []core.Event
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("stream window did not terminate")
		}
		page, err := st.ReadTaskEventStream(ctx, q)
		requireOK(t, err)
		if len(page.Events) > q.Limit {
			t.Fatalf("page exceeded its limit: %d > %d", len(page.Events), q.Limit)
		}
		out = append(out, page.Events...)
		if !page.More {
			return out
		}
		if len(page.Events) == 0 {
			t.Fatal("empty page reported more events")
		}
		last := page.Events[len(page.Events)-1]
		q.After = &store.TaskEventPosition{At: last.At, ID: last.ID}
	}
}

func runEventStreamWindows(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	seeded := seedDecreasing(t, x, task.ID)
	a, b, c, d := seeded[0], seeded[1], seeded[2], seeded[3]
	all, err := st.ListEventsAfter(ctx, task.ID, 0)
	requireOK(t, err)

	for _, limit := range []int{1, 2, 3, store.TaskEventStreamMaxEvents} {
		got := drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Limit: limit})
		requireIDs(t, "drained stream", got, eventIDs(all)...)
	}
	page, err := st.ReadTaskEventStream(ctx, store.TaskEventStreamQuery{TaskID: task.ID, Limit: len(all)})
	requireOK(t, err)
	if page.More || len(page.Events) != len(all) {
		t.Fatalf("exact page more=%v len=%d", page.More, len(page.Events))
	}
	page, err = st.ReadTaskEventStream(ctx, store.TaskEventStreamQuery{TaskID: task.ID, Limit: len(all) - 1})
	requireOK(t, err)
	if !page.More {
		t.Fatal("short page did not report lookahead")
	}

	// Since is inclusive on recorded time and ignores numeric ID.
	requireIDs(t, "since B", drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Since: b.At, Limit: 2}), b.ID, c.ID, d.ID)
	requireIDs(t, "since B after B", drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Since: b.At, After: &store.TaskEventPosition{At: b.At, ID: b.ID}, Limit: 2}), c.ID, d.ID)
	requireIDs(t, "since D", drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Since: d.At, Limit: 2}), d.ID)
	requireIDs(t, "since after D", drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Since: d.At.Add(time.Second), Limit: 2}))

	// A lower-ID event that becomes visible later at a tied earlier time is
	// returned by an overlapping rescan in its chronological position.
	base := a.ID - 30
	late := x.SeedEvents(t, ctx, base, []core.Event{{ID: 1, TaskID: task.ID, Kind: "fixture.late", At: b.At}})[0]
	requireIDs(t, "rescan with delayed event", drainStream(t, st, ctx, store.TaskEventStreamQuery{TaskID: task.ID, Since: d.At.Add(-store.TaskEventStreamReconciliationWindow), Limit: 2}), a.ID, late.ID, b.ID, c.ID, d.ID)

	for _, q := range []store.TaskEventStreamQuery{{TaskID: task.ID}, {TaskID: task.ID, Limit: store.TaskEventStreamMaxEvents + 1}, {Limit: 1}} {
		if _, err := st.ReadTaskEventStream(ctx, q); err == nil {
			t.Fatalf("accepted invalid stream query %+v", q)
		}
	}
	if _, err := st.ReadTaskEventStream(store.WithWorkspace(ctx, "other-"+core.NewTaskID()), store.TaskEventStreamQuery{TaskID: task.ID, Limit: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign-workspace stream err=%v", err)
	}
}

func runChronologicalProjections(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	seeded := seedDecreasing(t, x, task.ID)
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	requireOK(t, err)
	if len(markers) != 1 || markers[0].LastEventID != seeded[3].ID || !markers[0].LastEventAt.Equal(seeded[3].At) {
		t.Fatalf("activity marker=%+v, want newest (at,id) event %d at %s", markers, seeded[3].ID, seeded[3].At)
	}

	// Context membership folds attach/remove events chronologically: an older
	// higher-ID removal does not cancel a newer lower-ID attachment.
	attached := newAggregateTask(t, x)
	design := "design-" + core.NewTaskID()
	at := seedTime()
	x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 20, TaskID: attached.ID, Kind: store.TaskContextDesignRemoved, At: at, Payload: core.JSONPayload(map[string]any{"id": design})},
		{ID: 10, TaskID: attached.ID, Kind: store.TaskContextDesignAdded, At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"id": design, "version": 1})},
	})
	removed := newAggregateTask(t, x)
	x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 20, TaskID: removed.ID, Kind: store.TaskContextDesignAdded, At: at, Payload: core.JSONPayload(map[string]any{"id": design, "version": 1})},
		{ID: 10, TaskID: removed.ID, Kind: store.TaskContextDesignRemoved, At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"id": design})},
	})
	tasks, err := st.ListTasksFiltered(ctx, store.TaskFilter{GoverningDesignIDs: []string{design}})
	requireOK(t, err)
	if len(tasks) != 1 || tasks[0].ID != attached.ID {
		ids := []string{}
		for _, task := range tasks {
			ids = append(ids, task.ID)
		}
		t.Fatalf("governing-design filter=%v, want only %s", ids, attached.ID)
	}
}

func runInterventionWindowRecency(t *testing.T, x Fixture) {
	st := x.Backend
	ctx := store.WithActor(x.Context, store.Actor{ID: "operator-window", Role: core.ActorHuman})
	task := newAggregateTask(t, x)
	at := seedTime()
	requireOK(t, st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, ActorID: "operator-window", ActorRole: core.ActorHuman, Action: core.InterventionRedirect, ReasonCode: "fixture", At: at}))
	x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 50, TaskID: task.ID, Kind: "fixture.window", At: at.Add(-time.Second)},
		{ID: 40, TaskID: task.ID, Kind: "fixture.window", At: at},
		{ID: 10, TaskID: task.ID, Kind: "fixture.window", At: at.Add(time.Second)},
	})
	count, err := st.CountEventsSinceHumanIntervention(ctx, task.ID, "fixture.window")
	requireOK(t, err)
	if count != 1 {
		t.Fatalf("events since the human intervention=%d, want only the strictly later lower-ID event", count)
	}
}

// runSnapshotCeilingResiduals pins the documented behavior of the two numeric
// snapshot ceilings the existing schema and API retain. A task-event window
// refuses a traversal when a lower-ID event commits under its captured ceiling;
// a document page includes such an event and the change is visible in Total.
func runSnapshotCeilingResiduals(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	at := seedTime()
	seeded := x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 20, TaskID: task.ID, Kind: "fixture.window", At: at},
		{ID: 30, TaskID: task.ID, Kind: "fixture.window", At: at.Add(time.Second)},
	})
	base := seeded[0].ID - 20
	window, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: "fixture.window", Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	if window.Boundary.MaxID != seeded[1].ID || window.Boundary.Count != 2 || !window.More {
		t.Fatalf("captured boundary=%+v more=%v", window.Boundary, window.More)
	}
	x.SeedEvents(t, ctx, base, []core.Event{{ID: 25, TaskID: task.ID, Kind: "fixture.window", At: at.Add(2 * time.Second)}})
	last := window.Events[len(window.Events)-1]
	if _, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: "fixture.window", Boundary: &window.Boundary, After: &store.TaskEventPosition{At: last.At, ID: last.ID}, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes}); !errors.Is(err, store.ErrTaskEventHistoryChanged) {
		t.Fatalf("late lower-ID commit under the ceiling err=%v, want ErrTaskEventHistoryChanged", err)
	}

	document := "design-" + core.NewTaskID()
	docEvent := func(rank int64, at time.Time) core.Event {
		return core.Event{ID: rank, TaskID: task.ID, Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": document})}
	}
	pinned := x.SeedEvents(t, ctx, base, []core.Event{docEvent(40, at), docEvent(60, at.Add(time.Second))})
	page, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, document, store.DocumentEventQuery{Limit: 1})
	requireOK(t, err)
	if page.SnapshotID != pinned[1].ID || page.Total != 2 {
		t.Fatalf("captured document page=%+v", page)
	}
	x.SeedEvents(t, ctx, base, []core.Event{docEvent(50, at.Add(2*time.Second)), docEvent(70, at.Add(3*time.Second))})
	next, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, document, store.DocumentEventQuery{Limit: 1, Offset: 1, SnapshotID: page.SnapshotID})
	requireOK(t, err)
	if next.Total != 3 || next.SnapshotID != page.SnapshotID {
		t.Fatalf("document snapshot residual changed: total=%d snapshot=%d, want the lower-ID late commit visible as Total 3", next.Total, next.SnapshotID)
	}
}
