package storetest

import (
	"errors"
	"fmt"
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
		ids := []int64{}
		for _, event := range ledger {
			if filter == "" || event.Kind == filter {
				ids = append(ids, event.ID)
			}
		}
		return ids
	}
	traverse := func(filter string, limit, maxBytes int) (store.TaskEventBoundary, []int64, int) {
		t.Helper()
		var (
			boundary *store.TaskEventBoundary
			after    *store.TaskEventPosition
			ids      []int64
			windows  int
		)
		for {
			window, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: filter, Boundary: boundary, After: after, Limit: limit, MaxBytes: maxBytes})
			requireOK(t, err)
			windows++
			if boundary != nil && window.Boundary != *boundary {
				t.Fatalf("window changed captured boundary: %+v want %+v", window.Boundary, *boundary)
			}
			if len(window.Events) > limit {
				t.Fatalf("window returned %d events over limit %d", len(window.Events), limit)
			}
			for i, event := range window.Events {
				if event.TaskID != task.ID || filter != "" && event.Kind != filter {
					t.Fatalf("window admitted foreign or unfiltered event %+v", event)
				}
				if previous := (store.TaskEventPosition{}); after != nil || i > 0 {
					if i > 0 {
						previous = store.TaskEventPosition{At: window.Events[i-1].At, ID: window.Events[i-1].ID}
					} else {
						previous = *after
					}
					if !store.TaskEventAfter(event, previous) {
						t.Fatalf("window is not strictly ordered after %+v: %+v", previous, event)
					}
				}
				ids = append(ids, event.ID)
			}
			if !window.More {
				return window.Boundary, ids, windows
			}
			if len(window.Events) == 0 {
				t.Fatal("window advertised more events without progress")
			}
			last := window.Events[len(window.Events)-1]
			captured := window.Boundary
			boundary, after = &captured, &store.TaskEventPosition{At: last.At, ID: last.ID}
		}
	}
	equal := func(name string, got, want []int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s traversal=%v want ledger order %v", name, got, want)
		}
	}

	boundary, all, windows := traverse("", 7, store.TaskEventWindowMaxBytes)
	equal("unfiltered", all, want(""))
	if boundary.Count != len(ledger) || boundary.MaxID != ledger[len(ledger)-1].ID && boundary.MaxID < ledger[len(ledger)-1].ID || windows < len(ledger)/7 {
		t.Fatalf("boundary=%+v windows=%d ledger=%d", boundary, windows, len(ledger))
	}
	filteredBoundary, filtered, _ := traverse(kind, 4, store.TaskEventWindowMaxBytes)
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

	empty, err := st.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{TaskID: task.ID, Kind: "test.absent", Limit: 25, MaxBytes: store.TaskEventWindowMaxBytes})
	requireOK(t, err)
	if len(empty.Events) != 0 || empty.More || empty.Boundary.Count != 0 || empty.Boundary.MaxID == 0 {
		t.Fatalf("empty filtered window=%+v", empty)
	}
	foreign := store.WithWorkspace(ctx, x.Workspace+"-foreign")
	if _, err := st.ReadTaskEventWindow(foreign, store.TaskEventWindowQuery{TaskID: task.ID, Limit: 25, MaxBytes: store.TaskEventWindowMaxBytes}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign workspace read err=%v", err)
	}
	for _, bad := range []store.TaskEventWindowQuery{
		{TaskID: task.ID, Limit: 0, MaxBytes: 1},
		{TaskID: task.ID, Limit: store.TaskEventWindowMaxEvents + 1, MaxBytes: 1},
		{TaskID: task.ID, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes + 1},
		{TaskID: task.ID, Limit: 1, MaxBytes: 1, After: &store.TaskEventPosition{ID: 1}},
		{Limit: 1, MaxBytes: 1},
	} {
		if _, err := st.ReadTaskEventWindow(ctx, bad); err == nil {
			t.Fatalf("accepted invalid window query %+v", bad)
		}
	}
}
