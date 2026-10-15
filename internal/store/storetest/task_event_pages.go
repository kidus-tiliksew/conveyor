package storetest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runTaskEventWindows holds every backend to the bounded task-event traversal
// (component-mcp-investigation-reads): (at, id) order across timestamp
// ties and window boundaries, exact kind filtering, a captured ID ceiling,
// the append-only count guard, byte budgets and workspace scope.
func runTaskEventWindows(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task, other := newAggregateTask(t, x), newAggregateTask(t, x)
	base := time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)
	const kind = "test.window"
	// Reverse-time appends make ID order disagree with timestamp order, and
	// groups of three share one timestamp to force ID tie-breaks.
	for i := 0; i < 60; i++ {
		at := base.Add(time.Duration(20-i/3) * time.Second)
		eventKind := kind
		if i%4 == 3 {
			eventKind = kind + ".other"
		}
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: eventKind, At: at, Payload: core.JSONPayload(map[string]any{"n": i})}))
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: other.ID, Kind: kind, At: base}))

	ledger, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	want := func(filter string) []int64 {
		return taskEventWindowIDs(ledger, store.TaskEventWindowQuery{Kind: filter})
	}
	// traverse walks a whole captured traversal from the template query:
	// ascending windows seek After their last event, descending windows seek
	// Before their first. The returned IDs are in ascending (at, id) order.
	traverse := func(q store.TaskEventWindowQuery) (store.TaskEventBoundary, []int64, int) {
		t.Helper()
		q.TaskID = task.ID
		var (
			ids     []int64
			windows int
		)
		for {
			window, err := st.ReadTaskEventWindow(ctx, q)
			requireOK(t, err)
			windows++
			if q.Boundary != nil && window.Boundary != *q.Boundary {
				t.Fatalf("window changed captured boundary: %+v want %+v", window.Boundary, *q.Boundary)
			}
			if len(window.Events) > q.Limit {
				t.Fatalf("window returned %d events over limit %d", len(window.Events), q.Limit)
			}
			windowIDs := make([]int64, 0, len(window.Events))
			for i, event := range window.Events {
				if event.TaskID != task.ID || !q.MatchesKind(event.Kind) {
					t.Fatalf("window admitted foreign or unfiltered event %+v", event)
				}
				if previous := (store.TaskEventPosition{}); q.After != nil || i > 0 {
					if i > 0 {
						previous = taskEventPositionOf(window.Events[i-1])
					} else {
						previous = *q.After
					}
					if !store.TaskEventAfter(event, previous) {
						t.Fatalf("window is not strictly ordered after %+v: %+v", previous, event)
					}
				}
				windowIDs = append(windowIDs, event.ID)
			}
			if q.Before != nil && len(window.Events) > 0 {
				if newest := window.Events[len(window.Events)-1]; !store.TaskEventAfter(core.Event{At: q.Before.At, ID: q.Before.ID}, taskEventPositionOf(newest)) {
					t.Fatalf("descending window is not strictly before %+v: %+v", *q.Before, newest)
				}
			}
			if q.Descending {
				ids = append(windowIDs, ids...)
			} else {
				ids = append(ids, windowIDs...)
			}
			if !window.More {
				return window.Boundary, ids, windows
			}
			if len(window.Events) == 0 {
				t.Fatal("window advertised more events without progress")
			}
			captured := window.Boundary
			q.Boundary = &captured
			if q.Descending {
				oldest := taskEventPositionOf(window.Events[0])
				q.Before = &oldest
			} else {
				newest := taskEventPositionOf(window.Events[len(window.Events)-1])
				q.After = &newest
			}
		}
	}
	equal := func(name string, got, want []int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s traversal=%v want ledger order %v", name, got, want)
		}
	}

	boundary, all, windows := traverse(store.TaskEventWindowQuery{Limit: 7, MaxBytes: store.TaskEventWindowMaxBytes})
	equal("unfiltered", all, want(""))
	if boundary.Count != len(ledger) || boundary.MaxID != ledger[len(ledger)-1].ID && boundary.MaxID < ledger[len(ledger)-1].ID || windows < len(ledger)/7 {
		t.Fatalf("boundary=%+v windows=%d ledger=%d", boundary, windows, len(ledger))
	}
	filteredBoundary, filtered, _ := traverse(store.TaskEventWindowQuery{Kind: kind, Limit: 4, MaxBytes: store.TaskEventWindowMaxBytes})
	equal("exact kind", filtered, want(kind))
	if filteredBoundary.Count != 45 {
		t.Fatalf("exact filter counted %d events, want 45", filteredBoundary.Count)
	}

	// A later append, even one backdated before every captured event, has a
	// higher ID and belongs to a fresh traversal only.
	first, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: kind, Limit: 5, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: kind, At: base.Add(-time.Hour)}))
	last := first.Events[len(first.Events)-1]
	next, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: kind, Boundary: &first.Boundary, After: &store.TaskEventPosition{At: last.At, ID: last.ID}, Limit: 1000, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	if len(first.Events)+len(next.Events) != 45 || next.More {
		t.Fatalf("captured traversal admitted a later append: %d+%d more=%v", len(first.Events), len(next.Events), next.More)
	}
	fresh, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: kind, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	if fresh.Boundary.Count != 46 || fresh.Events[0].At.After(base) || !fresh.More {
		t.Fatalf("fresh traversal did not include the backdated append: %+v", fresh)
	}

	// The append-only count guard refuses a boundary whose captured set is no
	// longer reproducible; durable backends prove the delayed-commit cause.
	changed := first.Boundary
	changed.Count--
	if _, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: kind, Boundary: &changed, After: &store.TaskEventPosition{At: last.At, ID: last.ID}, Limit: 5, MaxBytes: store.TaskEventWindowMaxBytes}); !errors.Is(err, store.ErrTaskEventHistoryChanged) {
		t.Fatalf("changed history err=%v", err)
	}

	// Byte budgets end a window at its last included event; an event that can
	// never fit is an explicit error that does not advance the traversal.
	large := newAggregateTask(t, x)
	blob := strings.Repeat("b", 3000)
	for i := 0; i < 5; i++ {
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: large.ID, Kind: kind, At: base.Add(time.Duration(i) * time.Second), Payload: core.JSONPayload(map[string]any{"blob": blob})}))
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: large.ID, Kind: kind, At: base.Add(10 * time.Second), Payload: core.JSONPayload(map[string]any{"blob": strings.Repeat("h", 20000)})}))
	budget := 10000
	window, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: large.ID, Kind: kind, Limit: 1000, MaxBytes: budget})
	requireOK(t, err)
	if len(window.Events) != 3 || !window.More {
		t.Fatalf("byte budget window=%d more=%v", len(window.Events), window.More)
	}
	tail := window.Events[2]
	window, err = st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: large.ID, Kind: kind, Boundary: &window.Boundary, After: &store.TaskEventPosition{At: tail.At, ID: tail.ID}, Limit: 1000, MaxBytes: budget})
	requireOK(t, err)
	if len(window.Events) != 2 || !window.More {
		t.Fatalf("window before oversized event=%d more=%v", len(window.Events), window.More)
	}
	tail = window.Events[1]
	for range 2 {
		var tooLarge *store.TaskEventTooLargeError
		_, err = st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: large.ID, Kind: kind, Boundary: &window.Boundary, After: &store.TaskEventPosition{At: tail.At, ID: tail.ID}, Limit: 1000, MaxBytes: budget})
		if !errors.As(err, &tooLarge) || tooLarge.Limit != budget || tooLarge.Bytes <= budget || tooLarge.EventID <= tail.ID {
			t.Fatalf("oversized event err=%v", err)
		}
	}

	// Every variable-width column counts toward the source budget, not only
	// the payload.
	wide := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: wide.ID, Kind: kind, ActorID: "fixture", ActorRole: core.ActorRole("system"), At: base}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: wide.ID, Kind: kind, ActorID: strings.Repeat("a", 20000), ActorRole: core.ActorRole("system"), At: base.Add(time.Second)}))
	window, err = st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: wide.ID, Kind: kind, Limit: 1000, MaxBytes: budget})
	requireOK(t, err)
	if len(window.Events) != 1 || !window.More {
		t.Fatalf("wide actor window=%d more=%v", len(window.Events), window.More)
	}
	tail = window.Events[0]
	var tooLarge *store.TaskEventTooLargeError
	if _, err = st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: wide.ID, Kind: kind, Boundary: &window.Boundary, After: &store.TaskEventPosition{At: tail.At, ID: tail.ID}, Limit: 1000, MaxBytes: budget}); !errors.As(err, &tooLarge) || tooLarge.Bytes <= 20000 {
		t.Fatalf("oversized actor err=%v", err)
	}

	// A payload limit never relaxes the budget for the remaining columns.
	if _, err = st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: wide.ID, Kind: kind, Boundary: &window.Boundary, After: &store.TaskEventPosition{At: tail.At, ID: tail.ID}, Limit: 1000, MaxBytes: budget, PayloadLimit: 1}); !errors.As(err, &tooLarge) || tooLarge.Bytes <= 20000 {
		t.Fatalf("payload-limited oversized actor err=%v", err)
	}

	runTaskEventWindowDirections(t, x, task.ID, kind, traverse, equal)

	empty, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: "test.absent", Limit: 25, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	if len(empty.Events) != 0 || empty.More || empty.Boundary.Count != 0 || empty.Boundary.MaxID == 0 {
		t.Fatalf("empty filtered window=%+v", empty)
	}
	foreign := store.WithWorkspace(ctx, x.Workspace+"-foreign")
	if _, err := st.ReadTaskEventWindow(foreign, store.TaskEventWindowQuery{TaskID: task.ID, Descending: true, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes, PayloadLimit: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign workspace descending read err=%v", err)
	}
	tooManyKinds := make([]string, store.TaskEventWindowMaxKinds+1)
	for i := range tooManyKinds {
		tooManyKinds[i] = fmt.Sprintf("test.window.set%d", i)
	}
	if _, err := st.ReadTaskEventWindow(foreign, store.TaskEventWindowQuery{TaskID: task.ID, Limit: 25, MaxBytes: store.TaskEventWindowMaxBytes}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign workspace read err=%v", err)
	}
	for _, bad := range []store.TaskEventWindowQuery{
		{TaskID: task.ID, Limit: 0, MaxBytes: 1},
		{TaskID: task.ID, Limit: store.TaskEventWindowMaxEvents + 1, MaxBytes: 1},
		{TaskID: task.ID, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes + 1},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, After: &store.TaskEventPosition{ID: 1}},
		{Limit: 1, MaxBytes: 1},
		// Task 261015-3721c6: descending, kind-set, and payload-limit fields
		// refuse every combination the window contract leaves undefined.
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Boundary: &store.TaskEventBoundary{MaxID: 1, Count: 1}, After: &store.TaskEventPosition{ID: 1}, Descending: true},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Before: &store.TaskEventPosition{ID: 1}, Descending: true},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Boundary: &store.TaskEventBoundary{MaxID: 1, Count: 1}, Before: &store.TaskEventPosition{ID: 1}},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Boundary: &store.TaskEventBoundary{MaxID: 1, Count: 1}, After: &store.TaskEventPosition{ID: 1}, Before: &store.TaskEventPosition{ID: 1}},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Kind: kind, Kinds: []string{kind}},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Kinds: tooManyKinds},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, Kinds: []string{kind, ""}},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, PayloadLimit: -1},
	} {
		if _, err := st.ReadTaskEventWindow(ctx, bad); err == nil {
			t.Fatalf("accepted invalid window query %+v", bad)
		}
	}
}

// runTaskEventWindowDirections holds every backend to the descending,
// kind-set, and payload-limited window fields (task 261015-3721c6;
// component-mcp-investigation-reads "Descending, kind-set, and payload-limited
// windows"). traverse walks a whole captured traversal of taskID from a
// template query and returns ascending IDs.
func runTaskEventWindowDirections(t *testing.T, x Fixture, taskID, kind string, traverse func(store.TaskEventWindowQuery) (store.TaskEventBoundary, []int64, int), equal func(string, []int64, []int64)) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	const maxBytes = store.TaskEventWindowMaxBytes
	read := func(q store.TaskEventWindowQuery) store.TaskEventWindow {
		t.Helper()
		window, err := st.ReadTaskEventWindow(ctx, q)
		requireOK(t, err)
		return window
	}
	ledger, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	var maxID int64
	for _, event := range ledger {
		maxID = max(maxID, event.ID)
	}

	// Descending windows page older with Before from each window's first
	// event. Over tied timestamps and IDs that disagree with time, the walk
	// visits every captured event exactly once and meets the ascending
	// traversal in the same order.
	ascBoundary, ascending, _ := traverse(store.TaskEventWindowQuery{Limit: 7, MaxBytes: maxBytes})
	equal("ascending", ascending, taskEventWindowIDs(ledger, store.TaskEventWindowQuery{}))
	for _, limit := range []int{1, 3, 7, len(ledger)} {
		descBoundary, descending, windows := traverse(store.TaskEventWindowQuery{Descending: true, Limit: limit, MaxBytes: maxBytes})
		equal(fmt.Sprintf("descending limit %d", limit), descending, ascending)
		if descBoundary != ascBoundary || descBoundary.MaxID != maxID || descBoundary.Count != len(ledger) || windows != (len(ledger)+limit-1)/limit {
			t.Fatalf("descending limit %d boundary=%+v windows=%d want %+v", limit, descBoundary, windows, ascBoundary)
		}
	}
	newest := read(store.TaskEventWindowQuery{TaskID: taskID, Descending: true, Limit: 4, MaxBytes: maxBytes})
	equal("newest descending window", taskEventIDs(newest.Events), ascending[len(ascending)-4:])
	if !newest.More || newest.Boundary != ascBoundary || newest.OmittedPayloadBytes != nil {
		t.Fatalf("newest descending window=%+v", newest)
	}

	// Before seeks strictly before a tuple, so a timestamp tie at the pivot is
	// split by ID and neither skipped nor repeated.
	pivot := -1
	for i := 4; i < len(ledger); i++ {
		if ledger[i].At.Equal(ledger[i-1].At) {
			pivot = i
			break
		}
	}
	if pivot < 0 {
		t.Fatal("fixture has no timestamp tie to seek before")
	}
	before := read(store.TaskEventWindowQuery{TaskID: taskID, Descending: true, Boundary: &ascBoundary, Before: taskEventSeek(ledger[pivot]), Limit: 3, MaxBytes: maxBytes})
	equal("before seek", taskEventIDs(before.Events), ascending[pivot-3:pivot])
	if !before.More || before.Boundary != ascBoundary {
		t.Fatalf("before seek window=%+v", before)
	}
	oldest := read(store.TaskEventWindowQuery{TaskID: taskID, Descending: true, Boundary: &ascBoundary, Before: taskEventSeek(ledger[0]), Limit: 3, MaxBytes: maxBytes})
	if len(oldest.Events) != 0 || oldest.More {
		t.Fatalf("seek before the oldest event=%+v", oldest)
	}

	// A kind set matches any listed kind in both directions, and the captured
	// boundary counts the same set.
	set := store.TaskEventWindowQuery{Kinds: []string{kind + ".other", "test.absent"}, Limit: 4, MaxBytes: maxBytes}
	setBoundary, setAscending, _ := traverse(set)
	set.Descending = true
	setDescBoundary, setDescending, _ := traverse(set)
	wantSet := taskEventWindowIDs(ledger, set)
	equal("ascending kind set", setAscending, wantSet)
	equal("descending kind set", setDescending, wantSet)
	if len(wantSet) == 0 || setBoundary != setDescBoundary || setBoundary.Count != len(wantSet) || setBoundary.MaxID != maxID {
		t.Fatalf("kind set boundary=%+v descending=%+v want count %d max %d", setBoundary, setDescBoundary, len(wantSet), maxID)
	}
	every := []string{}
	for _, event := range ledger {
		if !slices.Contains(every, event.Kind) {
			every = append(every, event.Kind)
		}
	}
	bothBoundary, both, _ := traverse(store.TaskEventWindowQuery{Kinds: every, Descending: true, Limit: 9, MaxBytes: maxBytes})
	equal("covering kind set", both, ascending)
	if bothBoundary != ascBoundary {
		t.Fatalf("covering kind set boundary=%+v want %+v", bothBoundary, ascBoundary)
	}
	many := make([]string, store.TaskEventWindowMaxKinds)
	for i := range many {
		many[i] = fmt.Sprintf("test.window.set%d", i)
	}
	many[len(many)-1] = kind
	if window := read(store.TaskEventWindowQuery{TaskID: taskID, Kinds: many, Limit: 1000, MaxBytes: maxBytes}); window.Boundary.Count != len(taskEventWindowIDs(ledger, store.TaskEventWindowQuery{Kind: kind})) || window.More {
		t.Fatalf("maximum kind set window boundary=%+v more=%v", window.Boundary, window.More)
	}

	// A lower-ID event that becomes visible under the captured ceiling, even
	// one older than every captured event, refuses the next descending window
	// for an exact kind and a kind set alike; a fresh capture includes it.
	// Durable backends prove the delayed-commit cause in their integration
	// tests.
	if x.SeedEvents == nil {
		t.Fatal("factory must provide SeedEvents for task event window conformance")
	}
	delayed := newAggregateTask(t, x)
	at := ledger[0].At
	seeded := x.SeedEvents(t, ctx, 0, []core.Event{
		{ID: 10, TaskID: delayed.ID, Kind: kind, At: at},
		{ID: 20, TaskID: delayed.ID, Kind: kind, At: at.Add(time.Second)},
		{ID: 30, TaskID: delayed.ID, Kind: kind, At: at.Add(2 * time.Second)},
	})
	for _, filter := range []store.TaskEventWindowQuery{{Kind: kind}, {Kinds: []string{kind, "test.absent"}}} {
		filter.TaskID, filter.Descending, filter.Limit, filter.MaxBytes = delayed.ID, true, 1, maxBytes
		captured := read(filter)
		if captured.Boundary.MaxID != seeded[2].ID || captured.Boundary.Count != 3 || len(captured.Events) != 1 || captured.Events[0].ID != seeded[2].ID || !captured.More {
			t.Fatalf("delayed fixture capture=%+v seeded=%+v", captured, seeded)
		}
		filter.Boundary, filter.Before = &captured.Boundary, taskEventSeek(captured.Events[0])
		if _, err := st.ReadTaskEventWindow(ctx, filter); err != nil {
			t.Fatalf("descending window before the delayed event err=%v", err)
		}
	}
	late := x.SeedEvents(t, ctx, seeded[0].ID-10, []core.Event{{ID: 5, TaskID: delayed.ID, Kind: kind, At: at.Add(-time.Hour)}})[0]
	if late.ID >= seeded[2].ID {
		t.Fatalf("seeded delayed event %d is not under the ceiling %d", late.ID, seeded[2].ID)
	}
	for _, filter := range []store.TaskEventWindowQuery{{Kind: kind}, {Kinds: []string{kind, "test.absent"}}} {
		filter.TaskID, filter.Descending, filter.Limit, filter.MaxBytes = delayed.ID, true, 1, maxBytes
		filter.Boundary = &store.TaskEventBoundary{MaxID: seeded[2].ID, Count: 3}
		filter.Before = taskEventSeek(seeded[2])
		for range 2 {
			if _, err := st.ReadTaskEventWindow(ctx, filter); !errors.Is(err, store.ErrTaskEventHistoryChanged) {
				t.Fatalf("descending delayed lower-ID event err=%v", err)
			}
		}
		filter.Boundary, filter.Before, filter.Limit = nil, nil, 1000
		if fresh := read(filter); fresh.Boundary.Count != 4 || len(fresh.Events) != 4 || fresh.Events[0].ID != late.ID || fresh.More {
			t.Fatalf("fresh descending capture=%+v", fresh)
		}
	}

	// The notify fingerprint is one descending row with no payload: the
	// whole task's boundary at the cost of a single event.
	fingerprint := store.TaskEventWindowQuery{TaskID: taskID, Descending: true, Limit: 1, MaxBytes: maxBytes, PayloadLimit: 1}
	fingerprinted := read(fingerprint)
	last := ledger[len(ledger)-1]
	if fingerprinted.Boundary != (store.TaskEventBoundary{MaxID: maxID, Count: len(ledger)}) || len(fingerprinted.Events) != 1 || fingerprinted.Events[0].ID != last.ID || fingerprinted.Events[0].Payload != nil || !fingerprinted.More {
		t.Fatalf("fingerprint=%+v want newest event %d boundary max=%d count=%d", fingerprinted, last.ID, maxID, len(ledger))
	}
	if len(fingerprinted.OmittedPayloadBytes) != 1 || fingerprinted.OmittedPayloadBytes[last.ID] != len(last.Payload) {
		t.Fatalf("fingerprint omitted=%v want %d:%d", fingerprinted.OmittedPayloadBytes, last.ID, len(last.Payload))
	}
	// Every task creation records task.created, so no public writer leaves a
	// task without events. PostgreSQL and SingleStore seed an empty ledger
	// directly in TestTaskEventWindowFetchBoundsIntegration and read the zero
	// boundary there.

	// A later append, even one backdated below every captured event, stays
	// outside a captured descending traversal.
	captured := read(store.TaskEventWindowQuery{TaskID: taskID, Descending: true, Limit: 5, MaxBytes: maxBytes})
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, At: ledger[0].At.Add(-time.Hour), Payload: core.JSONPayload(map[string]any{"late": true})}))
	capturedBoundary := captured.Boundary
	_, rest, _ := traverse(store.TaskEventWindowQuery{Descending: true, Boundary: &capturedBoundary, Before: taskEventSeek(captured.Events[0]), Limit: 9, MaxBytes: maxBytes})
	equal("captured descending traversal", append(rest, taskEventIDs(captured.Events)...), ascending)

	// A payload limit omits over-limit payloads, charges those rows without
	// them, and reports their stored length, so an event whose payload alone
	// exceeds the byte budget is still returned.
	omit := newAggregateTask(t, x)
	base := ledger[0].At
	for i, size := range []int{10, 20000, 10} {
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: omit.ID, Kind: kind, At: base.Add(time.Duration(i) * time.Second), Payload: core.JSONPayload(map[string]any{"blob": strings.Repeat("p", size)})}))
	}
	created, err := st.ListEvents(ctx, omit.ID)
	requireOK(t, err)
	omitLedger := []core.Event{}
	for _, event := range created {
		if event.Kind == kind {
			omitLedger = append(omitLedger, event)
		}
	}
	if len(omitLedger) != 3 {
		t.Fatalf("payload fixture ledger=%+v", omitLedger)
	}
	const budget = 10000
	if unlimited := read(store.TaskEventWindowQuery{TaskID: omit.ID, Kind: kind, Limit: 1000, MaxBytes: budget}); len(unlimited.Events) != 1 || !unlimited.More || unlimited.OmittedPayloadBytes != nil {
		t.Fatalf("unlimited payload window=%d more=%v omitted=%v", len(unlimited.Events), unlimited.More, unlimited.OmittedPayloadBytes)
	}
	limited := store.TaskEventWindowQuery{TaskID: omit.ID, Kind: kind, Limit: 1000, PayloadLimit: 1000}
	charged := 0
	for _, event := range omitLedger {
		charged += store.TaskEventChargedBytes(limited, event)
	}
	if charged >= budget || store.TaskEventChargedBytes(limited, omitLedger[1]) != store.TaskEventSourceBytes(omitLedger[1])-len(omitLedger[1].Payload) {
		t.Fatalf("charged bytes=%d for ledger %+v", charged, omitLedger)
	}
	wantOmitted := map[int64]int{omitLedger[1].ID: len(omitLedger[1].Payload)}
	limited.MaxBytes = charged
	window := read(limited)
	equal("payload-limited window", taskEventIDs(window.Events), taskEventIDs(omitLedger))
	if window.More || fmt.Sprint(window.OmittedPayloadBytes) != fmt.Sprint(wantOmitted) || window.Events[1].Payload != nil ||
		string(window.Events[0].Payload) != string(omitLedger[0].Payload) || string(window.Events[2].Payload) != string(omitLedger[2].Payload) {
		t.Fatalf("payload-limited window=%+v omitted=%v want %v", window, window.OmittedPayloadBytes, wantOmitted)
	}
	limited.MaxBytes = charged - 1
	window = read(limited)
	equal("payload-limited window one byte short", taskEventIDs(window.Events), taskEventIDs(omitLedger[:2]))
	if !window.More || fmt.Sprint(window.OmittedPayloadBytes) != fmt.Sprint(wantOmitted) {
		t.Fatalf("payload-limited window one byte short more=%v omitted=%v", window.More, window.OmittedPayloadBytes)
	}
	limited.MaxBytes, limited.Limit, limited.Descending = budget, 2, true
	window = read(limited)
	equal("descending payload-limited window", taskEventIDs(window.Events), taskEventIDs(omitLedger[1:]))
	if !window.More || fmt.Sprint(window.OmittedPayloadBytes) != fmt.Sprint(wantOmitted) || window.Events[0].Payload != nil {
		t.Fatalf("descending payload-limited window=%+v", window)
	}
}

// taskEventWindowIDs lists ledger events matching q's kind filter in ledger
// (ascending tuple) order.
func taskEventWindowIDs(ledger []core.Event, q store.TaskEventWindowQuery) []int64 {
	ids := []int64{}
	for _, event := range ledger {
		if q.MatchesKind(event.Kind) {
			ids = append(ids, event.ID)
		}
	}
	return ids
}

func taskEventIDs(events []core.Event) []int64 {
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}

func taskEventPositionOf(e core.Event) store.TaskEventPosition {
	return store.TaskEventPosition{At: e.At, ID: e.ID}
}

func taskEventSeek(e core.Event) *store.TaskEventPosition {
	position := taskEventPositionOf(e)
	return &position
}
