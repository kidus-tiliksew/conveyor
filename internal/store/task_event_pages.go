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

// TaskEventWindowQuery selects one window. A nil Boundary captures a new
// traversal; After requires a Boundary and seeks strictly past that tuple.
type TaskEventWindowQuery struct {
	TaskID   string
	Kind     string
	Boundary *TaskEventBoundary
	After    *TaskEventPosition
	Limit    int
	MaxBytes int
}

// TaskEventWindow holds events ordered by ascending (At, ID). More reports that
// captured matching events follow the last returned event.
type TaskEventWindow struct {
	Boundary TaskEventBoundary
	Events   []core.Event
	More     bool
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
	return nil
}

// TaskEventSourceBytes is the source size charged against a window budget.
// Durable backends compute the same sum over stored column text.
func TaskEventSourceBytes(e core.Event) int {
	return len(e.Payload) + len(e.Kind) + len(e.ActorID) + len(string(e.ActorRole)) + len(e.JobID) + len(e.TaskID) + taskEventRowOverhead
}

// TaskEventAfter reports whether e sorts strictly after p.
func TaskEventAfter(e core.Event, p TaskEventPosition) bool {
	return e.At.After(p.At) || e.At.Equal(p.At) && e.ID > p.ID
}

// BudgetTaskEventWindow applies the byte budget and lookahead to ordered
// candidates, which may hold at most Limit+1 events.
func BudgetTaskEventWindow(q TaskEventWindowQuery, boundary TaskEventBoundary, candidates []core.Event) (TaskEventWindow, error) {
	window := TaskEventWindow{Boundary: boundary, Events: []core.Event{}}
	used := 0
	for _, event := range candidates {
		if len(window.Events) == q.Limit {
			window.More = true
			break
		}
		size := TaskEventSourceBytes(event)
		if used+size > q.MaxBytes {
			if len(window.Events) == 0 {
				return TaskEventWindow{}, &TaskEventTooLargeError{EventID: event.ID, Bytes: size, Limit: q.MaxBytes}
			}
			window.More = true
			break
		}
		used += size
		window.Events = append(window.Events, event)
	}
	return window, nil
}

// taskEventHeap keeps the smallest (At, ID) candidates with the largest at the
// root, so a full ledger scan retains at most Limit+1 events.
type taskEventHeap []core.Event

func (h taskEventHeap) Len() int { return len(h) }
func (h taskEventHeap) Less(i, j int) bool {
	return TaskEventAfter(h[i], TaskEventPosition{At: h[j].At, ID: h[j].ID})
}
func (h taskEventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *taskEventHeap) Push(x any)   { *h = append(*h, x.(core.Event)) }
func (h *taskEventHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
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
	matches := func(e core.Event) bool { return q.Kind == "" || e.Kind == q.Kind }
	var boundary TaskEventBoundary
	if q.Boundary == nil {
		for _, event := range ledger {
			boundary.MaxID = max(boundary.MaxID, event.ID)
		}
	} else {
		boundary = *q.Boundary
	}
	count := 0
	candidates := make(taskEventHeap, 0, min(len(ledger), q.Limit+1))
	for _, event := range ledger {
		if event.ID > boundary.MaxID || !matches(event) {
			continue
		}
		count++
		if q.After != nil && !TaskEventAfter(event, *q.After) {
			continue
		}
		if len(candidates) <= q.Limit {
			heap.Push(&candidates, event)
		} else if TaskEventAfter(candidates[0], TaskEventPosition{At: event.At, ID: event.ID}) {
			candidates[0] = event
			heap.Fix(&candidates, 0)
		}
	}
	if q.Boundary == nil {
		boundary.Count = count
	} else if count != boundary.Count {
		return TaskEventWindow{}, ErrTaskEventHistoryChanged
	}
	sort.Slice(candidates, func(i, j int) bool {
		return TaskEventAfter(candidates[j], TaskEventPosition{At: candidates[i].At, ID: candidates[i].ID})
	})
	return BudgetTaskEventWindow(q, boundary, candidates)
}
