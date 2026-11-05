package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskEventStreamReader is the bounded chronological read a live task stream
// makes on its first read and on each wake.
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
	reader taskEventStreamReader
	// redactor is built for each poll, so a frame never predates a secret
	// that was readable when its poll began.
	redactor func(context.Context) (*redact.Redactor, error)
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

func newTaskEventStream(reader taskEventStreamReader, taskID string, redactor func(context.Context) (*redact.Redactor, error)) *taskEventStream {
	return &taskEventStream{
		reader: reader, redactor: redactor, taskID: taskID,
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
	redactor, err := s.redactor(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errTaskEventStreamRead, err)
	}
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
			clean, err := redactEventForRead(event, redactor)
			if err != nil {
				return err
			}
			if err := writeTaskActivityFrame(w, clean); err != nil {
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

// redactEventForRead returns the event as a REST read serves it. Recorded
// events are never rewritten, so an event stored before its secret's shape was
// known is redacted here instead (req-security-boundaries REQ-3, REQ-5). A
// payload with nothing to remove keeps its recorded bytes.
func redactEventForRead(event core.Event, redactor *redact.Redactor) (core.Event, error) {
	event.ActorID, _ = redactor.Redact(event.ActorID)
	if len(bytes.TrimSpace(event.Payload)) == 0 {
		return event, nil
	}
	clean, stats, err := redactor.RedactJSON(event.Payload)
	if err != nil {
		return core.Event{}, fmt.Errorf("redact event payload: %w", err)
	}
	if stats.Total() > 0 {
		event.Payload = clean
	}
	return event, nil
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

// taskEventFingerprintReader is the one window read a notify stream makes
// on start and on each wake.
type taskEventFingerprintReader interface {
	ReadTaskEventWindow(ctx context.Context, query store.TaskEventWindowQuery) (store.TaskEventWindow, error)
}

// taskEventNotifyStream is the connection-local state of a mode=notify
// stream. It reads no event pages and never replays history: the
// fingerprint is the task's history boundary (maximum event ID, count at or
// below it), which any newly visible event changes whatever its ID or
// recorded time, and which never returns to an earlier value because events
// are never deleted. written is the fingerprint of the last successfully
// written frame (task 261015-3721c6; component-http-api "Live task event
// stream").
type taskEventNotifyStream struct {
	reader  taskEventFingerprintReader
	taskID  string
	written *store.TaskEventBoundary
}

func newTaskEventNotifyStream(reader taskEventFingerprintReader, taskID string) *taskEventNotifyStream {
	return &taskEventNotifyStream{reader: reader, taskID: taskID}
}

// fingerprint transfers at most one event row and no payload.
func (s *taskEventNotifyStream) fingerprint(ctx context.Context) (store.TaskEventBoundary, error) {
	window, err := s.reader.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{
		TaskID: s.taskID, Descending: true, Limit: 1, MaxBytes: store.TaskEventWindowMaxBytes, PayloadLimit: 1,
	})
	if err != nil {
		return store.TaskEventBoundary{}, fmt.Errorf("%w: %v", errTaskEventStreamRead, err)
	}
	return window.Boundary, nil
}

// start reads the first fingerprint and writes ready; the dashboard refreshes
// detail once on ready, covering every event committed before or during the
// subscription.
func (s *taskEventNotifyStream) start(ctx context.Context, w io.Writer) error {
	current, err := s.fingerprint(ctx)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return err
	}
	s.written = &current
	return nil
}

// poll writes one activity frame when the fingerprint differs from the last
// one written, however many commits happened since the previous wake, and records the new
// fingerprint only after the write succeeds.
func (s *taskEventNotifyStream) poll(ctx context.Context, w io.Writer) error {
	current, err := s.fingerprint(ctx)
	if err != nil {
		return err
	}
	if s.written != nil && *s.written == current {
		return nil
	}
	if _, err := io.WriteString(w, "event: activity\ndata: {}\n\n"); err != nil {
		return err
	}
	s.written = &current
	return nil
}

// streamKeepaliveInterval is how often an open stream writes an SSE comment
// so proxies keep the connection open. A keep-alive reads nothing.
const streamKeepaliveInterval = 25 * time.Second

const keepaliveFrame = ": keepalive\n\n"

// streamWake is what a pushed stream waits on after its first read: change
// notices from the hub, including the workspace-wide task notices the
// daemon-wide peer check republishes, and, for the notify mode in tests,
// taskEventStreamTicks. It keeps no peer timer (feature-quiet-idle-runtime,
// Peers; req-quiet-idle-runtime AC-1.3). The stream opens it before its first read, so a
// commit after that read leaves a notice buffered for the first wait
// (component-http-api, "When a task stream reads"; DEC-99(1)).
type streamWake struct {
	subscription *changes.Subscription
	ticks        <-chan time.Time
	keepalive    <-chan time.Time
	stops        []func()
}

// openTaskWake subscribes to the task's notices. ticks selects the notify
// mode's test hook as an extra notice source.
func (s *Server) openTaskWake(workspace, taskID string, ticks bool) *streamWake {
	wake := s.openWake(s.Changes.SubscribeTask(workspace, taskID))
	if ticks && s.taskEventStreamTicks != nil {
		c, stop := s.taskEventStreamTicks()
		wake.ticks = c
		wake.stops = append(wake.stops, stop)
	}
	return wake
}

// openWorkspaceWake subscribes to every task notice of the workspace.
func (s *Server) openWorkspaceWake(workspace string) *streamWake {
	return s.openWake(s.Changes.Subscribe(workspace, changes.Task))
}

func (s *Server) openWake(subscription *changes.Subscription) *streamWake {
	wake := &streamWake{subscription: subscription}
	wake.stops = append(wake.stops, subscription.Close)
	if s.StreamKeepalive != nil {
		c, stop := s.StreamKeepalive()
		wake.keepalive = c
		wake.stops = append(wake.stops, stop)
	} else {
		ticker := time.NewTicker(streamKeepaliveInterval)
		wake.keepalive = ticker.C
		wake.stops = append(wake.stops, ticker.Stop)
	}
	return wake
}

func (w *streamWake) close() {
	for _, stop := range w.stops {
		stop()
	}
}

// wait blocks until a notice or a test tick arrives, and
// returns the notice; a tick returns the zero notice, which names no task.
// Keep-alive ticks write a comment and keep waiting. It reports false when
// the request ended or a keep-alive write failed.
func (w *streamWake) wait(ctx context.Context, out io.Writer, flusher http.Flusher) (changes.Notice, bool) {
	for {
		select {
		case <-ctx.Done():
			return changes.Notice{}, false
		case notice := <-w.subscription.C():
			return notice, true
		case <-w.ticks:
			return changes.Notice{}, true
		case <-w.keepalive:
			if _, err := io.WriteString(out, keepaliveFrame); err != nil {
				return changes.Notice{}, false
			}
			flusher.Flush()
		}
	}
}

// taskStreamWorkspace is the workspace whose notices a task stream receives:
// the task's own, or the request's when the task names none.
func taskStreamWorkspace(ctx context.Context, task core.Task) string {
	if task.Workspace != "" {
		return task.Workspace
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}

// streamTaskEventNotifications serves mode=notify: ready after the first
// fingerprint, then one activity frame per changed fingerprint per wake. A
// read failure writes an error frame and ends the stream; a write failure
// ends it silently.
func (s *Server) streamTaskEventNotifications(w http.ResponseWriter, flusher http.Flusher, r *http.Request, taskID string, wake *streamWake) {
	ctx := r.Context()
	stream := newTaskEventNotifyStream(s.Store, taskID)
	fail := func(err error) {
		if errors.Is(err, errTaskEventStreamRead) {
			_, _ = w.Write([]byte("event: error\ndata: {}\n\n"))
			flusher.Flush()
		}
	}
	if err := stream.start(ctx, w); err != nil {
		fail(err)
		return
	}
	flusher.Flush()
	for {
		if _, ok := wake.wait(ctx, w, flusher); !ok {
			return
		}
		if err := stream.poll(ctx, w); err != nil {
			fail(err)
			return
		}
		flusher.Flush()
	}
}
