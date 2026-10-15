package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// Notify mode (task 261015-3721c6; component-http-api "Live task event
// stream"): the stream polls a fingerprint, never an event page.

// fingerprintLedger answers the notify fingerprint read over events committed
// in any ID order and records every query.
type fingerprintLedger struct {
	events  []core.Event
	err     error
	queries []store.TaskEventWindowQuery
}

func (f *fingerprintLedger) ReadTaskEventWindow(_ context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return store.TaskEventWindow{}, f.err
	}
	window := store.TaskEventWindow{Events: []core.Event{}}
	var newest *core.Event
	for i := range f.events {
		window.Boundary.MaxID = max(window.Boundary.MaxID, f.events[i].ID)
		window.Boundary.Count++
		if newest == nil || store.TaskEventAfter(f.events[i], store.TaskEventPosition{At: newest.At, ID: newest.ID}) {
			newest = &f.events[i]
		}
	}
	if newest != nil {
		event := *newest
		event.Payload = nil
		window.Events = append(window.Events, event)
	}
	return window, nil
}

func (f *fingerprintLedger) commit(events ...core.Event) { f.events = append(f.events, events...) }

// notifyWriter records notify frames and can fail on a chosen write.
type notifyWriter struct {
	frames []string
	failAt int
	writes int
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes == w.failAt {
		return 0, errors.New("client disconnected")
	}
	w.frames = append(w.frames, string(p))
	return len(p), nil
}

const (
	readyFrame    = "event: ready\ndata: {}\n\n"
	activityFrame = "event: activity\ndata: {}\n\n"
	errorFrame    = "event: error\ndata: {}\n\n"
)

func notifyPoll(t *testing.T, s *taskEventNotifyStream) []string {
	t.Helper()
	w := &notifyWriter{}
	if err := s.poll(context.Background(), w); err != nil {
		t.Fatalf("poll: %v", err)
	}
	return w.frames
}

func TestTaskEventNotifyStreamReadyAfterOneFingerprint(t *testing.T) {
	ledger := &fingerprintLedger{}
	ledger.commit(streamEvent(3, 0), streamEvent(1, time.Second), streamEvent(2, 2*time.Second))
	stream := newTaskEventNotifyStream(ledger, "stream-task")
	w := &notifyWriter{}
	if err := stream.start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.frames, []string{readyFrame}) || len(ledger.queries) != 1 {
		t.Fatalf("start frames=%q queries=%d", w.frames, len(ledger.queries))
	}
	q := ledger.queries[0]
	if q.TaskID != "stream-task" || !q.Descending || q.Limit != 1 || q.PayloadLimit != 1 || q.MaxBytes != store.TaskEventWindowMaxBytes || q.Boundary != nil || q.Kind != "" || q.Kinds != nil {
		t.Fatalf("fingerprint query=%+v", q)
	}
	if *stream.written != (store.TaskEventBoundary{MaxID: 3, Count: 3}) {
		t.Fatalf("fingerprint=%+v", *stream.written)
	}
	if frames := notifyPoll(t, stream); len(frames) != 0 {
		t.Fatalf("idle poll frames=%q", frames)
	}
}

func TestTaskEventNotifyStreamOneFramePerChangedPoll(t *testing.T) {
	ledger := &fingerprintLedger{}
	stream := newTaskEventNotifyStream(ledger, "stream-task")
	// An empty ledger has the fingerprint (0, 0).
	if err := stream.start(context.Background(), &notifyWriter{}); err != nil || *stream.written != (store.TaskEventBoundary{}) {
		t.Fatalf("empty start err=%v fingerprint=%+v", err, stream.written)
	}
	if frames := notifyPoll(t, stream); len(frames) != 0 {
		t.Fatalf("empty idle poll frames=%q", frames)
	}
	for i := int64(0); i < 40; i++ {
		ledger.commit(streamEvent(highRange+i, time.Duration(i)*time.Millisecond))
	}
	if frames := notifyPoll(t, stream); !slices.Equal(frames, []string{activityFrame}) {
		t.Fatalf("40 commits between polls frames=%q", frames)
	}
	if frames := notifyPoll(t, stream); len(frames) != 0 {
		t.Fatalf("no commit frames=%q", frames)
	}
	// A delayed lower-ID event with an earlier recorded time leaves the
	// maximum ID unchanged but raises the count.
	ledger.commit(streamEvent(lowRange, -time.Hour))
	if frames := notifyPoll(t, stream); !slices.Equal(frames, []string{activityFrame}) {
		t.Fatalf("delayed lower-ID commit frames=%q", frames)
	}
	if *stream.written != (store.TaskEventBoundary{MaxID: highRange + 39, Count: 41}) {
		t.Fatalf("fingerprint=%+v", *stream.written)
	}
	for _, q := range ledger.queries {
		if q.Limit != 1 || q.PayloadLimit != 1 || !q.Descending {
			t.Fatalf("notify read an event page: %+v", q)
		}
	}
}

func TestTaskEventNotifyStreamRecordsOnlyAfterWriteAndReportsReadFailure(t *testing.T) {
	ledger := &fingerprintLedger{}
	stream := newTaskEventNotifyStream(ledger, "stream-task")
	if err := stream.start(context.Background(), &notifyWriter{failAt: 1}); err == nil || errors.Is(err, errTaskEventStreamRead) || stream.written != nil {
		t.Fatalf("failed ready write err=%v written=%v", err, stream.written)
	}
	if err := stream.start(context.Background(), &notifyWriter{}); err != nil {
		t.Fatal(err)
	}
	ledger.commit(streamEvent(5, 0))
	if err := stream.poll(context.Background(), &notifyWriter{failAt: 1}); err == nil || errors.Is(err, errTaskEventStreamRead) {
		t.Fatalf("failed activity write err=%v", err)
	}
	if *stream.written != (store.TaskEventBoundary{}) {
		t.Fatalf("failed write recorded %+v", *stream.written)
	}
	if frames := notifyPoll(t, stream); !slices.Equal(frames, []string{activityFrame}) {
		t.Fatalf("retry after failed write frames=%q", frames)
	}
	ledger.err = errors.New("database unavailable")
	if err := stream.poll(context.Background(), &notifyWriter{}); !errors.Is(err, errTaskEventStreamRead) {
		t.Fatalf("read failure err=%v", err)
	}
	if err := newTaskEventNotifyStream(ledger, "stream-task").start(context.Background(), &notifyWriter{}); !errors.Is(err, errTaskEventStreamRead) {
		t.Fatalf("start read failure err=%v", err)
	}
}

// windowCountingStore counts the event reads a stream makes.
type windowCountingStore struct {
	store.Store
	mu            sync.Mutex
	windowQueries []store.TaskEventWindowQuery
	pageReads     int
	failWindow    bool
}

func (s *windowCountingStore) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	s.mu.Lock()
	s.windowQueries = append(s.windowQueries, q)
	fail := s.failWindow
	s.mu.Unlock()
	if fail {
		return store.TaskEventWindow{}, errors.New("database unavailable")
	}
	return s.Store.ReadTaskEventWindow(ctx, q)
}

func (s *windowCountingStore) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	s.mu.Lock()
	s.pageReads++
	s.mu.Unlock()
	return s.Store.ReadTaskEventStream(ctx, q)
}

func (s *windowCountingStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	s.mu.Lock()
	s.pageReads++
	s.mu.Unlock()
	return s.Store.ListEvents(ctx, taskID)
}

func (s *windowCountingStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.windowQueries), s.pageReads
}

// sseRecorder hands the bytes written since the previous flush to the test
// on every flush, a deterministic barrier with no sleeps.
type sseRecorder struct {
	header  http.Header
	mu      sync.Mutex
	pending bytes.Buffer
	code    int
	flushes chan string
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}, flushes: make(chan string)}
}

func (r *sseRecorder) Header() http.Header { return r.header }
func (r *sseRecorder) WriteHeader(code int) {
	r.code = code
}
func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending.Write(p)
}
func (r *sseRecorder) Flush() {
	r.mu.Lock()
	chunk := r.pending.String()
	r.pending.Reset()
	r.mu.Unlock()
	r.flushes <- chunk
}

type notifyConnection struct {
	recorder *sseRecorder
	cancel   context.CancelFunc
	done     chan struct{}
}

func openNotifyStream(h *historyHarness, taskID, query string) *notifyConnection {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/events/stream"+query, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+historyTokenA)
	connection := &notifyConnection{recorder: newSSERecorder(), cancel: cancel, done: make(chan struct{})}
	go func() {
		h.handler.ServeHTTP(connection.recorder, request)
		close(connection.done)
	}()
	return connection
}

func (c *notifyConnection) close() {
	c.cancel()
	<-c.done
}

func TestTaskEventNotifyStreamHTTP(t *testing.T) {
	base := store.NewMemory()
	events := seedHistoryFixture(t, base, "notify-task", 100_000, 99_400, 0)
	counting := &windowCountingStore{Store: base}
	h := newHistoryHarness(t, counting)
	ticks := make(chan time.Time)
	h.srv.taskEventStreamTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)

	if response := h.get(historyTokenA, "/v1/tasks/notify-task/events/stream?mode=replay"); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: %d", response.Code)
	}
	connection := openNotifyStream(h, "notify-task", "?mode=notify&workspace_id="+historyWorkspace)
	if frame := <-connection.recorder.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	if connection.recorder.header.Get("Content-Type") != "text/event-stream" || connection.recorder.header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers=%v", connection.recorder.header)
	}
	// One fingerprint read, no event page and no replay over 100,000 events.
	if windows, pages := counting.counts(); windows != 1 || pages != 0 {
		t.Fatalf("ready over %d events used %d window reads and %d page reads", len(events), windows, pages)
	}
	counting.mu.Lock()
	q := counting.windowQueries[0]
	counting.mu.Unlock()
	if q.Limit != 1 || q.PayloadLimit != 1 || !q.Descending {
		t.Fatalf("fingerprint query=%+v", q)
	}
	ticks <- time.Time{}
	if frame := <-connection.recorder.flushes; frame != "" {
		t.Fatalf("idle poll frame=%q", frame)
	}
	for i := 0; i < 25; i++ {
		if err := base.AppendEvent(ctx, core.Event{TaskID: "notify-task", Kind: "dispatch.reconciled"}); err != nil {
			t.Fatal(err)
		}
	}
	ticks <- time.Time{}
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("25 commits frame=%q", frame)
	}
	connection.close()

	// A reconnect reads the fingerprint again and sends ready; commits during
	// the gap are covered by the refresh ready triggers.
	if err := base.AppendEvent(ctx, core.Event{TaskID: "notify-task", Kind: "merge.requested"}); err != nil {
		t.Fatal(err)
	}
	reconnect := openNotifyStream(h, "notify-task", "?mode=notify&workspace_id="+historyWorkspace)
	if frame := <-reconnect.recorder.flushes; frame != readyFrame {
		t.Fatalf("reconnect frame=%q", frame)
	}
	ticks <- time.Time{}
	if frame := <-reconnect.recorder.flushes; frame != "" {
		t.Fatalf("reconnect idle frame=%q", frame)
	}
	// A read failure writes an error frame and ends the stream.
	counting.mu.Lock()
	counting.failWindow = true
	counting.mu.Unlock()
	ticks <- time.Time{}
	if frame := <-reconnect.recorder.flushes; frame != errorFrame {
		t.Fatalf("read failure frame=%q", frame)
	}
	<-reconnect.done
	reconnect.cancel()
	if _, pages := counting.counts(); pages != 0 {
		t.Fatalf("notify mode read %d event pages", pages)
	}
}

func TestTaskEventStreamLegacyModeUnchanged(t *testing.T) {
	base := store.NewMemory()
	seedHistoryFixture(t, base, "legacy-task", 8, 2, 0)
	counting := &windowCountingStore{Store: base}
	h := newHistoryHarness(t, counting)
	h.srv.taskEventStreamTicks = func() (<-chan time.Time, func()) { t.Fatal("legacy mode used the notify ticker"); return nil, nil }
	connection := openNotifyStream(h, "legacy-task", "?workspace_id="+historyWorkspace)
	frame := <-connection.recorder.flushes
	if strings.Count(frame, "event: activity\ndata: {\"id\":") != 8 || strings.Contains(frame, "event: ready") {
		t.Fatalf("legacy replay=%q", frame)
	}
	connection.close()
	if windows, pages := counting.counts(); windows != 0 || pages == 0 {
		t.Fatalf("legacy reads windows=%d pages=%d", windows, pages)
	}
}
