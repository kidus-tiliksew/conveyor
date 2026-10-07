package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Event recency and cursor reads follow chronological (at, id) order, never
// numeric ID order alone. SingleStore AUTO_INCREMENT hands out IDs from
// per-aggregator ranges, so a later insert can receive a lower ID; event ID is
// immutable identity, not insertion order (component-persistence; DEC-39).
const (
	// TaskEventStreamMaxEvents bounds one stream-window page. Backends select
	// at most one more row as lookahead.
	TaskEventStreamMaxEvents = 1000
	// TaskEventStreamReconciliationWindow is how far behind its delivered
	// frontier a live stream rescans on every poll. It recovers an event that
	// becomes visible after a chronologically later one was delivered, provided
	// its recorded time is within this window of the frontier.
	TaskEventStreamReconciliationWindow = 30 * time.Second
)

// ErrEventAnchorNotFound reports a nonzero ListEventsAfter anchor that is not
// an event of the requested task in the caller's workspace. It wraps
// ErrNotFound. Readers never fall back to numeric-ID comparison or a replay.
var ErrEventAnchorNotFound = fmt.Errorf("event anchor not found: %w", ErrNotFound)

// TaskEventStreamQuery selects one bounded page of a task's ledger in
// ascending (at, id) order. Since is an inclusive lower bound on recorded time;
// the zero value starts at the beginning. After, when set, seeks strictly past
// that tuple within the bound.
type TaskEventStreamQuery struct {
	TaskID string
	Since  time.Time
	After  *TaskEventPosition
	Limit  int
}

// TaskEventStreamPage holds at most Limit events. More reports that at least
// one further matching event follows the last returned event.
type TaskEventStreamPage struct {
	Events []core.Event
	More   bool
}

func ValidateTaskEventStreamQuery(q TaskEventStreamQuery) error {
	if q.TaskID == "" || q.Limit < 1 || q.Limit > TaskEventStreamMaxEvents {
		return fmt.Errorf("invalid task event stream window")
	}
	return nil
}

// taskEventStreamMatches reports whether e falls inside the inclusive time
// bound and strictly past the optional seek tuple.
func taskEventStreamMatches(e core.Event, q TaskEventStreamQuery) bool {
	if !q.Since.IsZero() && e.At.Before(q.Since) {
		return false
	}
	return q.After == nil || TaskEventAfter(e, *q.After)
}

// TaskEventBefore reports whether a sorts strictly before b in (at, id) order.
func TaskEventBefore(a, b core.Event) bool {
	return TaskEventAfter(b, TaskEventPosition{At: a.At, ID: a.ID})
}

// SortTaskEventsChronologically orders events by ascending (at, id).
func SortTaskEventsChronologically(events []core.Event) {
	sort.SliceStable(events, func(i, j int) bool { return TaskEventBefore(events[i], events[j]) })
}

// ChronologicalTaskEvents returns an (at, id)-ordered copy.
func ChronologicalTaskEvents(events []core.Event) []core.Event {
	out := append([]core.Event(nil), events...)
	SortTaskEventsChronologically(out)
	return out
}

// LatestTaskEvent returns the chronologically newest event of kind, or of any
// kind when kind is empty: the maximum (at, id) tuple, independent of slice
// order.
func LatestTaskEvent(events []core.Event, kind string) (core.Event, bool) {
	var latest core.Event
	found := false
	for _, event := range events {
		if kind != "" && event.Kind != kind {
			continue
		}
		if !found || TaskEventBefore(latest, event) {
			latest, found = event, true
		}
	}
	return latest, found
}

// PageTaskEventStream applies a stream query to an in-memory ledger.
func PageTaskEventStream(ledger []core.Event, q TaskEventStreamQuery) TaskEventStreamPage {
	matched := make([]core.Event, 0, min(len(ledger), q.Limit+1))
	for _, event := range ledger {
		if taskEventStreamMatches(event, q) {
			matched = append(matched, event)
		}
	}
	SortTaskEventsChronologically(matched)
	page := TaskEventStreamPage{Events: matched}
	if len(matched) > q.Limit {
		page.Events, page.More = matched[:q.Limit], true
	}
	return page
}

// EventsAfterAnchor returns ledger events strictly after the anchor event's
// (at, id) tuple in ascending order. A zero anchor returns the whole ledger.
func EventsAfterAnchor(ledger []core.Event, anchorID int64) ([]core.Event, error) {
	out := append([]core.Event(nil), ledger...)
	SortTaskEventsChronologically(out)
	if anchorID == 0 {
		return out, nil
	}
	for i, event := range out {
		if event.ID == anchorID {
			return append([]core.Event{}, out[i+1:]...), nil
		}
	}
	return nil, ErrEventAnchorNotFound
}

// visibleTaskLedgerLocked refuses a task owned by another workspace. Legacy
// NewMemory fixtures may hold tasks without a workspace; those stay visible.
func (m *memory) visibleTaskLedgerLocked(ctx context.Context, taskID string) ([]core.Event, bool) {
	task, ok := m.tasks[taskID]
	if workspace, scoped := WorkspaceFromContext(ctx); !ok || scoped && task.Workspace != "" && task.Workspace != workspace {
		return nil, false
	}
	return m.events[taskID], true
}

func (m *memory) ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ledger, ok := m.visibleTaskLedgerLocked(ctx, taskID)
	if !ok {
		if afterID != 0 {
			return nil, ErrEventAnchorNotFound
		}
		return []core.Event{}, nil
	}
	return EventsAfterAnchor(ledger, afterID)
}

func (m *memory) ReadTaskEventStream(ctx context.Context, q TaskEventStreamQuery) (TaskEventStreamPage, error) {
	if err := ValidateTaskEventStreamQuery(q); err != nil {
		return TaskEventStreamPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ledger, ok := m.visibleTaskLedgerLocked(ctx, q.TaskID)
	if !ok {
		return TaskEventStreamPage{}, ErrNotFound
	}
	return PageTaskEventStream(ledger, q), nil
}

// TaskStateChangedCommand decodes the command of a task.state_changed event.
func TaskStateChangedCommand(event core.Event) (core.TaskCommand, bool) {
	var payload struct {
		Command core.TaskCommand `json:"command"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return "", false
	}
	return payload.Command, true
}
