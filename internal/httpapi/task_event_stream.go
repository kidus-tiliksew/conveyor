package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventStreamReader is the bounded chronological read a live task stream
// polls.
type taskEventStreamReader interface {
	ReadTaskEventStream(ctx context.Context, query store.TaskEventStreamQuery) (store.TaskEventStreamPage, error)
}

// errTaskEventStreamRead distinguishes a store read failure, which the
// handler reports as an error frame, from a client write failure.
var errTaskEventStreamRead = errors.New("task event stream read failed")

// taskEventStream is the connection-local delivery state of one SSE stream.
// Event IDs are identity, not order: SingleStore can make a lower-ID event
// visible after a higher-ID one. Each poll therefore rescans from the
// delivered (at, id) frontier minus the reconciliation window and emits only
// unseen IDs, so a maintained connection writes exactly one activity frame for
// every event that becomes visible while its recorded time is still inside
// the window. An event that commits later than that is not recovered on this
// connection; a reconnect replays history and the client refetches
// (component-persistence; DEC-39).
type taskEventStream struct {
	reader   taskEventStreamReader
	taskID   string
	window   time.Duration
	pageSize int
	// frontier is the maximum successfully sent (at, id) tuple.
	frontier *store.TaskEventPosition
	// sent holds the recorded time of each successfully sent event. IDs whose
	// time falls below the current rescan lower bound are evicted: every later
	// rescan starts at or above that bound, so they can never be returned
	// again and eviction cannot permit a duplicate frame.
	sent map[int64]time.Time
}

func newTaskEventStream(reader taskEventStreamReader, taskID string) *taskEventStream {
	return &taskEventStream{
		reader: reader, taskID: taskID,
		window:   store.TaskEventStreamReconciliationWindow,
		pageSize: store.TaskEventStreamMaxEvents,
		sent:     map[int64]time.Time{},
	}
}

// lowerBound is the inclusive recorded-time bound of the next rescan. Before
// the first delivery the stream traverses history from the beginning.
func (s *taskEventStream) lowerBound() time.Time {
	if s.frontier == nil {
		return time.Time{}
	}
	return s.frontier.At.Add(-s.window)
}

// poll drains one rescan. Every page restarts from this poll's lower bound
// rather than a previous poll's seek, so a delayed event behind the frontier
// is still found. A write or marshal failure stops the poll without marking
// the unsent event delivered.
func (s *taskEventStream) poll(ctx context.Context, w io.Writer) error {
	since := s.lowerBound()
	var after *store.TaskEventPosition
	for {
		page, err := s.reader.ReadTaskEventStream(ctx, store.TaskEventStreamQuery{TaskID: s.taskID, Since: since, After: after, Limit: s.pageSize})
		if err != nil {
			return fmt.Errorf("%w: %v", errTaskEventStreamRead, err)
		}
		for _, event := range page.Events {
			after = &store.TaskEventPosition{At: event.At, ID: event.ID}
			if _, seen := s.sent[event.ID]; seen {
				continue
			}
			if err := writeTaskActivityFrame(w, event); err != nil {
				return err
			}
			s.sent[event.ID] = event.At
			if s.frontier == nil || store.TaskEventAfter(event, *s.frontier) {
				s.frontier = &store.TaskEventPosition{At: event.At, ID: event.ID}
			}
		}
		if !page.More || len(page.Events) == 0 {
			break
		}
	}
	s.evict()
	return nil
}

func (s *taskEventStream) evict() {
	bound := s.lowerBound()
	if bound.IsZero() {
		return
	}
	for id, at := range s.sent {
		if at.Before(bound) {
			delete(s.sent, id)
		}
	}
}

func writeTaskActivityFrame(w io.Writer, event core.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(data)+32)
	frame = append(frame, "event: activity\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	_, err = w.Write(frame)
	return err
}
