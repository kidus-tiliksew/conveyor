package store

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Task event windows let investigation reads traverse a long task history in
// bounded immutable steps instead of materializing the whole ledger.
// component-mcp-investigation-reads; req-task-centric-operations-view REQ-2.
const (
	// TaskEventWindowMaxEvents bounds the events one window returns. Backends
	// select at most one more candidate as lookahead.
	TaskEventWindowMaxEvents = 1000
	// TaskEventWindowMaxBytes bounds the source bytes one window returns.
	TaskEventWindowMaxBytes = 1 << 20
	// taskEventRowOverhead charges fixed per-row framing so many tiny events
	// still consume budget.
	taskEventRowOverhead = 64
)

// ErrTaskEventHistoryChanged reports that the captured traversal can no longer
// be reproduced: an event at or below the captured ID ceiling committed after
// capture. Task events are append-only, so a changed count is proof.
var ErrTaskEventHistoryChanged = errors.New("event history changed: restart read")

// TaskEventPosition is the (recorded time, event ID) seek tuple.
type TaskEventPosition struct {
	At time.Time
	ID int64
}

// TaskEventBoundary is captured once per traversal from one consistent read
// view. Count matches the query's event-kind filter at or below MaxID.
type TaskEventBoundary struct {
	MaxID int64
	Count int
}

// TaskEventWindowMaxKinds bounds a kind-set filter.
const TaskEventWindowMaxKinds = 64

// TaskEventWindowQuery selects one window. A nil Boundary captures a new
// traversal; After requires a Boundary and seeks strictly past that tuple.
//
// Descending selects the newest matching events first and Before, which also
// requires a Boundary, seeks strictly before its tuple; the returned Events
// stay in ascending order. Kinds matches any listed kind and is exclusive with
// Kind. A positive PayloadLimit returns a nil payload for every row whose
// stored payload exceeds it, charges that row without its payload, and records
// the stored length in OmittedPayloadBytes (task 261015-3721c6;
// component-mcp-investigation-reads "Descending, kind-set, and
// payload-limited windows").
type TaskEventWindowQuery struct {
	TaskID       string
	Kind         string
	Kinds        []string
	Boundary     *TaskEventBoundary
	After        *TaskEventPosition
	Before       *TaskEventPosition
	Descending   bool
	Limit        int
	MaxBytes     int
	PayloadLimit int
}

// TaskEventWindow holds events ordered by ascending (At, ID). More reports that
// captured matching events follow the last returned event, or for a descending
// window precede the first returned event. OmittedPayloadBytes maps an event ID
// whose payload a PayloadLimit omitted to its stored payload length.
type TaskEventWindow struct {
	Boundary            TaskEventBoundary
	Events              []core.Event
	More                bool
	OmittedPayloadBytes map[int64]int
}

// MatchesKind reports whether kind passes the query's kind filter.
func (q TaskEventWindowQuery) MatchesKind(kind string) bool {
	if len(q.Kinds) > 0 {
		for _, candidate := range q.Kinds {
			if candidate == kind {
				return true
			}
		}
		return false
	}
	return q.Kind == "" || kind == q.Kind
}

// TaskEventTooLargeError names an event whose source bytes alone exceed the
// window budget. The traversal does not advance past it.
type TaskEventTooLargeError struct {
	EventID      int64
	Bytes, Limit int
}

func (e *TaskEventTooLargeError) Error() string {
	return fmt.Sprintf("event %d exceeds the %d-byte event window budget (%d bytes): narrow event_kind", e.EventID, e.Limit, e.Bytes)
}

func ValidateTaskEventWindowQuery(q TaskEventWindowQuery) error {
	if q.TaskID == "" || q.Limit < 1 || q.Limit > TaskEventWindowMaxEvents || q.MaxBytes < 1 || q.MaxBytes > TaskEventWindowMaxBytes {
		return fmt.Errorf("invalid task event window")
	}
	if q.After != nil && q.Boundary == nil || q.Boundary != nil && (q.Boundary.MaxID < 0 || q.Boundary.Count < 0) {
		return fmt.Errorf("invalid task event window")
	}
	if q.Before != nil && (q.Boundary == nil || !q.Descending) || q.After != nil && (q.Before != nil || q.Descending) {
		return fmt.Errorf("invalid task event window")
	}
	if q.Kind != "" && len(q.Kinds) > 0 || len(q.Kinds) > TaskEventWindowMaxKinds || q.PayloadLimit < 0 {
		return fmt.Errorf("invalid task event window")
	}
	for _, kind := range q.Kinds {
		if kind == "" {
			return fmt.Errorf("invalid task event window")
		}
	}
	return nil
}

// TaskEventSourceBytes is the source size charged against a window budget.
// Durable backends compute the same sum over stored column text.
func TaskEventSourceBytes(e core.Event) int {
	return len(e.Payload) + len(e.Kind) + len(e.ActorID) + len(string(e.ActorRole)) + len(e.JobID) + len(e.TaskID) + taskEventRowOverhead
}

// TaskEventChargedBytes is the source size a window charges for e, without the
// payload when the query's PayloadLimit omits it.
func TaskEventChargedBytes(q TaskEventWindowQuery, e core.Event) int {
	size := TaskEventSourceBytes(e)
	if q.PayloadLimit > 0 && len(e.Payload) > q.PayloadLimit {
		size -= len(e.Payload)
	}
	return size
}

// TaskEventAfter reports whether e sorts strictly after p.
func TaskEventAfter(e core.Event, p TaskEventPosition) bool {
	return e.At.After(p.At) || e.At.Equal(p.At) && e.ID > p.ID
}

// BudgetTaskEventWindow applies the byte budget, payload limit, and lookahead
// to candidates in selection order (ascending, or descending for a Descending
// query), which may hold at most Limit+1 events. The returned events are
// ascending.
func BudgetTaskEventWindow(q TaskEventWindowQuery, boundary TaskEventBoundary, candidates []core.Event) (TaskEventWindow, error) {
	window := TaskEventWindow{Boundary: boundary, Events: []core.Event{}}
	used := 0
	for _, event := range candidates {
		if len(window.Events) == q.Limit {
			window.More = true
			break
		}
		size := TaskEventChargedBytes(q, event)
		if used+size > q.MaxBytes {
			if len(window.Events) == 0 {
				return TaskEventWindow{}, &TaskEventTooLargeError{EventID: event.ID, Bytes: size, Limit: q.MaxBytes}
			}
			window.More = true
			break
		}
		used += size
		if q.PayloadLimit > 0 && len(event.Payload) > q.PayloadLimit {
			if window.OmittedPayloadBytes == nil {
				window.OmittedPayloadBytes = map[int64]int{}
			}
			window.OmittedPayloadBytes[event.ID] = len(event.Payload)
			event.Payload = nil
		}
		window.Events = append(window.Events, event)
	}
	if q.Descending {
		ReverseTaskEvents(window.Events)
	}
	return window, nil
}

// ReverseTaskEvents reverses events in place.
func ReverseTaskEvents(events []core.Event) {
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
}

// taskEventHeap keeps the first Limit+1 candidates in selection order with the
// last of them at the root, so a full ledger scan retains at most Limit+1
// events.
type taskEventHeap struct {
	events []core.Event
	first  func(a, b core.Event) bool
}

func (h taskEventHeap) Len() int           { return len(h.events) }
func (h taskEventHeap) Less(i, j int) bool { return h.first(h.events[j], h.events[i]) }
func (h taskEventHeap) Swap(i, j int)      { h.events[i], h.events[j] = h.events[j], h.events[i] }
func (h *taskEventHeap) Push(x any)        { h.events = append(h.events, x.(core.Event)) }
func (h *taskEventHeap) Pop() any {
	last := h.events[len(h.events)-1]
	h.events = h.events[:len(h.events)-1]
	return last
}

func (m *memory) ReadTaskEventWindow(ctx context.Context, q TaskEventWindowQuery) (TaskEventWindow, error) {
	if err := ValidateTaskEventWindowQuery(q); err != nil {
		return TaskEventWindow{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[q.TaskID]
	if workspace, scoped := WorkspaceFromContext(ctx); !ok || scoped && task.Workspace != workspace {
		return TaskEventWindow{}, ErrNotFound
	}
	ledger := m.events[q.TaskID]
	matches := func(e core.Event) bool { return q.MatchesKind(e.Kind) }
	// first reports whether a sorts before b in selection order: ascending,
	// or newest first for a descending window.
	first := func(a, b core.Event) bool {
		if q.Descending {
			return TaskEventAfter(a, TaskEventPosition{At: b.At, ID: b.ID})
		}
		return TaskEventAfter(b, TaskEventPosition{At: a.At, ID: a.ID})
	}
	var boundary TaskEventBoundary
	if q.Boundary == nil {
		for _, event := range ledger {
			boundary.MaxID = max(boundary.MaxID, event.ID)
		}
	} else {
		boundary = *q.Boundary
	}
	count := 0
	candidates := taskEventHeap{events: make([]core.Event, 0, min(len(ledger), q.Limit+1)), first: first}
	for _, event := range ledger {
		if event.ID > boundary.MaxID || !matches(event) {
			continue
		}
		count++
		if q.After != nil && !TaskEventAfter(event, *q.After) {
			continue
		}
		if q.Before != nil && !TaskEventAfter(core.Event{At: q.Before.At, ID: q.Before.ID}, TaskEventPosition{At: event.At, ID: event.ID}) {
			continue
		}
		if len(candidates.events) <= q.Limit {
			heap.Push(&candidates, event)
		} else if first(event, candidates.events[0]) {
			candidates.events[0] = event
			heap.Fix(&candidates, 0)
		}
	}
	if q.Boundary == nil {
		boundary.Count = count
	} else if count != boundary.Count {
		return TaskEventWindow{}, ErrTaskEventHistoryChanged
	}
	sort.Slice(candidates.events, func(i, j int) bool { return first(candidates.events[i], candidates.events[j]) })
	return BudgetTaskEventWindow(q, boundary, candidates.events)
}
