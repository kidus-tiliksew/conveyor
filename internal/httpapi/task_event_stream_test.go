package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// visibleEvents is a deterministic commit barrier: an event is committed when
// a test appends it, in any ID order, with no wall-clock sleeps.
type visibleEvents struct {
	events  []core.Event
	err     error
	queries []store.TaskEventStreamQuery
}

func (v *visibleEvents) ReadTaskEventStream(_ context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	v.queries = append(v.queries, q)
	if v.err != nil {
		return store.TaskEventStreamPage{}, v.err
	}
	if err := store.ValidateTaskEventStreamQuery(q); err != nil {
		return store.TaskEventStreamPage{}, err
	}
	return store.PageTaskEventStream(v.events, q), nil
}

func (v *visibleEvents) commit(events ...core.Event) { v.events = append(v.events, events...) }

// frameWriter records complete activity frames and can fail on a chosen
// write, modelling a disconnected client.
type frameWriter struct {
	frames []int64
	failAt int
	writes int
}

func (w *frameWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes == w.failAt {
		return 0, errors.New("client disconnected")
	}
	text := string(p)
	if !strings.HasPrefix(text, "event: activity\ndata: ") || !strings.HasSuffix(text, "\n\n") {
		return 0, errors.New("malformed frame")
	}
	var event core.Event
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(text, "event: activity\ndata: "), "\n\n")), &event); err != nil {
		return 0, err
	}
	w.frames = append(w.frames, event.ID)
	return len(p), nil
}

var streamEpoch = time.Date(2026, 10, 6, 11, 56, 12, 598000000, time.UTC)

func streamEvent(id int64, offset time.Duration) core.Event {
	return core.Event{ID: id, TaskID: "stream-task", Kind: "task.state_changed", At: streamEpoch.Add(offset), Payload: json.RawMessage(`{}`)}
}

func newTestStream(reader taskEventStreamReader, pageSize int) *taskEventStream {
	s := newTaskEventStream(reader, "stream-task")
	s.pageSize = pageSize
	return s
}

func pollFrames(t *testing.T, s *taskEventStream) []int64 {
	t.Helper()
	w := &frameWriter{}
	if err := s.poll(context.Background(), w); err != nil {
		t.Fatalf("poll: %v", err)
	}
	return w.frames
}

func requireFrames(t *testing.T, name string, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s frames=%v want=%v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s frames=%v want=%v", name, got, want)
		}
	}
}

// The incident IDs: consecutive inserts alternate between SingleStore
// aggregator ranges near 2^51 and 2^50.
const (
	highRange = int64(2251799813695928)
	lowRange  = int64(1125899906853648)
)

func TestTaskEventStreamDeliversNonMonotonicIDsExactlyOnce(t *testing.T) {
	visible := &visibleEvents{}
	visible.commit(
		streamEvent(highRange, 0),
		streamEvent(lowRange, 13*time.Minute),
		streamEvent(lowRange+5, 13*time.Minute+4*time.Second),
	)
	stream := newTestStream(visible, 2)
	requireFrames(t, "history traversal across pages", pollFrames(t, stream), highRange, lowRange, lowRange+5)
	if visible.queries[0].Since != (time.Time{}) || visible.queries[0].After != nil {
		t.Fatalf("initial traversal did not start at the beginning: %+v", visible.queries[0])
	}
	requireFrames(t, "overlapping idle poll", pollFrames(t, stream))

	// A numeric ID cursor would have stopped here: every later low-range event
	// has an ID below the first high-range event.
	visible.commit(streamEvent(lowRange+54, 21*time.Minute))
	requireFrames(t, "later low-range event", pollFrames(t, stream), lowRange+54)
	requireFrames(t, "repeat", pollFrames(t, stream))
}

func TestTaskEventStreamReconcilesDelayedEventsInsideWindow(t *testing.T) {
	visible := &visibleEvents{}
	for i := int64(0); i < 5; i++ {
		visible.commit(streamEvent(1000+i, time.Duration(i)*time.Second))
	}
	stream := newTestStream(visible, 2)
	requireFrames(t, "initial", pollFrames(t, stream), 1000, 1001, 1002, 1003, 1004)
	frontier := *stream.frontier

	// A lower-ID event becomes visible after the frontier, recorded 10s
	// behind it; another ties the frontier's time with a lower ID.
	visible.commit(streamEvent(7, 4*time.Second-10*time.Second), streamEvent(8, 4*time.Second))
	before := len(visible.queries)
	requireFrames(t, "delayed lower-ID events", pollFrames(t, stream), 7, 8)
	if *stream.frontier != frontier {
		t.Fatalf("delivering earlier events moved the frontier from %+v to %+v", frontier, *stream.frontier)
	}
	rescan := visible.queries[before:]
	if len(rescan) != 4 {
		t.Fatalf("rescan of 7 events at page size 2 used %d pages", len(rescan))
	}
	for i, q := range rescan {
		if !q.Since.Equal(frontier.At.Add(-store.TaskEventStreamReconciliationWindow)) {
			t.Fatalf("rescan page did not restart at the reconciliation bound: %+v", q)
		}
		if (i == 0) != (q.After == nil) {
			t.Fatalf("rescan page %d seek=%+v: only later pages of one poll seek", i, q.After)
		}
	}
	requireFrames(t, "overlap after reconciliation", pollFrames(t, stream))

	// High-to-low IDs at advancing times are delivered in (at,id) order.
	visible.commit(streamEvent(900, 7*time.Second), streamEvent(800, 6*time.Second), streamEvent(850, 6*time.Second))
	requireFrames(t, "decreasing IDs", pollFrames(t, stream), 800, 850, 900)
	if stream.frontier.ID != 900 {
		t.Fatalf("frontier=%+v want event 900", *stream.frontier)
	}
}

func TestTaskEventStreamMissesCommitsOlderThanWindow(t *testing.T) {
	visible := &visibleEvents{}
	visible.commit(streamEvent(10, time.Minute))
	stream := newTestStream(visible, 1000)
	requireFrames(t, "initial", pollFrames(t, stream), 10)
	// Documented limitation: an event whose recorded time is already outside
	// the window when it becomes visible is not recovered on this connection.
	visible.commit(streamEvent(5, time.Minute-store.TaskEventStreamReconciliationWindow-time.Microsecond))
	requireFrames(t, "outside-window late commit", pollFrames(t, stream))
	// At the inclusive bound it is still recovered.
	visible.commit(streamEvent(6, time.Minute-store.TaskEventStreamReconciliationWindow))
	requireFrames(t, "boundary late commit", pollFrames(t, stream), 6)

	// A reconnect replays history from the beginning; the client refetches.
	replay := newTestStream(visible, 1000)
	requireFrames(t, "reconnect replay", pollFrames(t, replay), 5, 6, 10)
}

func TestTaskEventStreamFailedWriteDoesNotMarkDelivery(t *testing.T) {
	visible := &visibleEvents{}
	visible.commit(streamEvent(30, 0), streamEvent(20, time.Second), streamEvent(10, 2*time.Second))
	stream := newTestStream(visible, 2)
	failing := &frameWriter{failAt: 2}
	if err := stream.poll(context.Background(), failing); err == nil || errors.Is(err, errTaskEventStreamRead) {
		t.Fatalf("write failure err=%v", err)
	}
	requireFrames(t, "before failure", failing.frames, 30)
	if _, sent := stream.sent[20]; sent || stream.frontier.ID != 30 {
		t.Fatalf("failed frame marked delivered: sent=%v frontier=%+v", stream.sent, *stream.frontier)
	}
	requireFrames(t, "resume after failure", pollFrames(t, stream), 20, 10)

	visible.commit(core.Event{ID: 5, TaskID: "stream-task", Kind: "bad", At: streamEpoch.Add(3 * time.Second), Payload: json.RawMessage(`{`)})
	if err := stream.poll(context.Background(), &frameWriter{}); err == nil {
		t.Fatal("marshal failure was not reported")
	}
	if _, sent := stream.sent[5]; sent || stream.frontier.ID != 10 {
		t.Fatal("unmarshalable event marked delivered")
	}
}

func TestTaskEventStreamReadFailureIsDistinct(t *testing.T) {
	stream := newTestStream(&visibleEvents{err: errors.New("database unavailable")}, 2)
	if err := stream.poll(context.Background(), &frameWriter{}); !errors.Is(err, errTaskEventStreamRead) {
		t.Fatalf("read failure err=%v", err)
	}
}

func TestTaskEventStreamEvictsOnlyIDsBelowTheRescanBound(t *testing.T) {
	visible := &visibleEvents{}
	stream := newTestStream(visible, 1000)
	var delivered []int64
	for i := int64(0); i < 120; i++ {
		// IDs fall as time advances, one event per second.
		visible.commit(streamEvent(10_000-i, time.Duration(i)*time.Second))
		delivered = append(delivered, pollFrames(t, stream)...)
		bound := stream.lowerBound()
		for id, at := range stream.sent {
			if at.Before(bound) {
				t.Fatalf("sent set retained %d below bound %s", id, bound)
			}
		}
		if len(stream.sent) > int(store.TaskEventStreamReconciliationWindow/time.Second)+1 {
			t.Fatalf("sent set grew to %d entries", len(stream.sent))
		}
	}
	if len(delivered) != 120 {
		t.Fatalf("delivered %d frames, want 120 without duplicates", len(delivered))
	}
	seen := map[int64]bool{}
	for _, id := range delivered {
		if seen[id] {
			t.Fatalf("duplicate frame for %d", id)
		}
		seen[id] = true
	}
	// Every evicted ID sorts below the rescan bound, so a rescan cannot
	// return it and no duplicate is possible.
	requireFrames(t, "rescan after eviction", pollFrames(t, stream))
}

func TestTaskEventStreamFrameShape(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTaskActivityFrame(&buf, streamEvent(42, 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "event: activity\ndata: {\"id\":42,") || !strings.HasSuffix(buf.String(), "}\n\n") {
		t.Fatalf("frame=%q", buf.String())
	}
}
