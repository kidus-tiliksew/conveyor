package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Pushed task streams (component-http-api, "When a task stream reads";
// feature-quiet-idle-runtime, "Pushed task streams and `await_review`";
// req-quiet-idle-runtime AC-1.2, AC-2.2). Every case runs over the volatile
// backend's hub with no read ticker: the only wakes are the commits the test
// makes, the keep-alive and peer channels it drives, and the test hook. The
// unbuffered flush channel of sseRecorder is the barrier; nothing sleeps.

const keepaliveComment = ": keepalive\n\n"

// pushStreamStore counts the event reads a stream makes and runs afterRead
// after each one returns from the backend, before the stream sees it, so a
// test can commit inside the window between a read and the next wait.
type pushStreamStore struct {
	store.Store
	mu        sync.Mutex
	reads     int
	afterRead func(read int)
	// gate, when set, blocks each read after the backend returned until the
	// test sends on it; started reports the read's ordinal first.
	gate    chan struct{}
	started chan int
}

func (s *pushStreamStore) observe() {
	s.mu.Lock()
	s.reads++
	read, hook, gate, started := s.reads, s.afterRead, s.gate, s.started
	s.mu.Unlock()
	if started != nil {
		started <- read
	}
	if gate != nil {
		<-gate
	}
	if hook != nil {
		hook(read)
	}
}

func (s *pushStreamStore) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	window, err := s.Store.ReadTaskEventWindow(ctx, q)
	s.observe()
	return window, err
}

func (s *pushStreamStore) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	page, err := s.Store.ReadTaskEventStream(ctx, q)
	s.observe()
	return page, err
}

func (s *pushStreamStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *pushStreamStore) setGate(gate chan struct{}, started chan int) {
	s.mu.Lock()
	s.gate, s.started = gate, started
	s.mu.Unlock()
}

func memoryHub(t *testing.T, st store.Store) *changes.Hub {
	t.Helper()
	source, ok := st.(interface{ Changes() *changes.Hub })
	if !ok {
		t.Fatal("memory store exposes no change hub")
	}
	return source.Changes()
}

type pushHarness struct {
	*historyHarness
	base      store.Store
	reads     *pushStreamStore
	keepalive chan time.Time
	peer      chan time.Time
	ctx       context.Context
}

func newPushHarness(t *testing.T, taskID string) *pushHarness {
	t.Helper()
	base := store.NewMemory()
	seedHistoryFixture(t, base, taskID, 8, 2, 0)
	reads := &pushStreamStore{Store: base}
	h := newHistoryHarness(t, reads)
	if h.srv.Changes != nil {
		t.Fatal("a wrapper without Changes() must not resolve a hub")
	}
	h.srv.Changes = memoryHub(t, base)
	keepalive := make(chan time.Time)
	h.srv.StreamKeepalive = func() (<-chan time.Time, func()) { return keepalive, func() {} }
	h.srv.taskEventStreamTicks = func() (<-chan time.Time, func()) { return nil, func() {} }
	h.handler = h.srv.Handler()
	return &pushHarness{
		historyHarness: h, base: base, reads: reads, keepalive: keepalive,
		ctx: store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace),
	}
}

func (h *pushHarness) commit(t *testing.T, taskID, kind string) {
	t.Helper()
	if err := h.base.AppendEvent(h.ctx, core.Event{TaskID: taskID, Kind: kind}); err != nil {
		t.Fatal(err)
	}
}

func streamQuery(notify bool) string {
	if notify {
		return "?mode=notify&workspace_id=" + historyWorkspace
	}
	return "?workspace_id=" + historyWorkspace
}

// wantActivity checks one wake's flush: the notify mode writes one data-less
// activity frame, the legacy mode one frame per new event.
func wantActivity(t *testing.T, notify bool, frame string, events int) {
	t.Helper()
	if notify {
		if frame != activityFrame {
			t.Fatalf("notify wake frame=%q", frame)
		}
		return
	}
	if got := strings.Count(frame, "event: activity\ndata: {\"id\":"); got != events {
		t.Fatalf("legacy wake wrote %d frames, want %d: %q", got, events, frame)
	}
}

func TestTaskEventStreamsReadOnlyOnNotice(t *testing.T) {
	for _, notify := range []bool{true, false} {
		name := "legacy"
		if notify {
			name = "notify"
		}
		t.Run(name, func(t *testing.T) {
			h := newPushHarness(t, "push-task")
			seedHistoryFixture(t, h.base, "push-sibling", 8, 2, 0)
			connection := openNotifyStream(h.historyHarness, "push-task", streamQuery(notify))
			defer connection.close()
			first := <-connection.recorder.flushes
			if notify && first != readyFrame {
				t.Fatalf("first frame=%q", first)
			}
			if !notify && strings.Count(first, "event: activity") != 8 {
				t.Fatalf("legacy replay=%q", first)
			}
			afterFirst := h.reads.readCount()

			// Keep-alives write a comment and read nothing.
			for range 3 {
				h.keepalive <- time.Time{}
				if frame := <-connection.recorder.flushes; frame != keepaliveComment {
					t.Fatalf("keep-alive frame=%q", frame)
				}
			}
			// A commit on another task of the workspace does not wake the
			// stream: the next flush is the keep-alive, not a read.
			h.commit(t, "push-sibling", "dispatch.reconciled")
			h.keepalive <- time.Time{}
			if frame := <-connection.recorder.flushes; frame != keepaliveComment {
				t.Fatalf("frame after another task's commit=%q", frame)
			}
			if got := h.reads.readCount(); got != afterFirst {
				t.Fatalf("idle stream made %d reads after its first read", got-afterFirst)
			}

			// A commit on the task wakes one read.
			h.commit(t, "push-task", "merge.requested")
			wantActivity(t, notify, <-connection.recorder.flushes, 1)
			if got := h.reads.readCount(); got != afterFirst+1 {
				t.Fatalf("one commit made %d reads", got-afterFirst)
			}
		})
	}
}

// A commit that lands after a read's snapshot, before the stream waits, is
// the lost-wake window: the subscription opened before the first read keeps
// its notice, so the first wait returns at once.
func TestTaskEventStreamsCommitBetweenFirstReadAndWait(t *testing.T) {
	for _, notify := range []bool{true, false} {
		name := "legacy"
		if notify {
			name = "notify"
		}
		t.Run(name, func(t *testing.T) {
			h := newPushHarness(t, "window-task")
			h.reads.afterRead = func(read int) {
				if read == 1 {
					h.commit(t, "window-task", "merge.requested")
				}
			}
			connection := openNotifyStream(h.historyHarness, "window-task", streamQuery(notify))
			defer connection.close()
			first := <-connection.recorder.flushes
			if notify && first != readyFrame {
				t.Fatalf("first frame=%q", first)
			}
			if !notify && strings.Count(first, "event: activity") != 8 {
				t.Fatalf("legacy first read saw the injected commit: %q", first)
			}
			wantActivity(t, notify, <-connection.recorder.flushes, 1)
		})
	}
}

// A commit during a read stays buffered and causes one more read; several
// commits during one read coalesce into one notice and one read that covers
// them all.
func TestTaskEventStreamsCommitDuringReadAndCoalescedNotices(t *testing.T) {
	for _, notify := range []bool{true, false} {
		name := "legacy"
		if notify {
			name = "notify"
		}
		t.Run(name, func(t *testing.T) {
			h := newPushHarness(t, "busy-task")
			connection := openNotifyStream(h.historyHarness, "busy-task", streamQuery(notify))
			defer connection.close()
			<-connection.recorder.flushes
			gate, started := make(chan struct{}), make(chan int)
			h.reads.setGate(gate, started)

			h.commit(t, "busy-task", "merge.requested")
			<-started // the woken read has its snapshot and is held
			for range 4 {
				h.commit(t, "busy-task", "dispatch.reconciled")
			}
			gate <- struct{}{}
			wantActivity(t, notify, <-connection.recorder.flushes, 1)
			// The four commits made during the held read wake exactly one
			// more read, which delivers all of them.
			<-started
			gate <- struct{}{}
			wantActivity(t, notify, <-connection.recorder.flushes, 4)
			h.reads.setGate(nil, nil)
			h.keepalive <- time.Time{}
			if frame := <-connection.recorder.flushes; frame != keepaliveComment {
				t.Fatalf("frame after the coalesced read=%q", frame)
			}
			if got := h.reads.readCount(); got != 3 {
				t.Fatalf("reads=%d, want first read plus two woken reads", got)
			}
		})
	}
}

func TestTaskEventNotifyStreamTestHookIsANoticeSource(t *testing.T) {
	h := newPushHarness(t, "peer-task")
	ticks := make(chan time.Time)
	h.srv.taskEventStreamTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	h.srv.PeerTail = time.Hour
	h.handler = h.srv.Handler()
	connection := openNotifyStream(h.historyHarness, "peer-task", streamQuery(true))
	defer connection.close()
	if frame := <-connection.recorder.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	// taskEventStreamTicks stays a notice source for the notify mode: a
	// tick reads the fingerprint, which has not changed.
	ticks <- time.Time{}
	if frame := <-connection.recorder.flushes; frame != "" {
		t.Fatalf("unchanged tick frame=%q", frame)
	}
	h.commit(t, "peer-task", "merge.requested")
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("commit frame=%q", frame)
	}
	if got := h.reads.readCount(); got != 3 {
		t.Fatalf("reads=%d", got)
	}
}

func TestTaskEventStreamPeerTailWakes(t *testing.T) {
	h := newPushHarness(t, "tail-task")
	h.srv.Changes = nil // a peer on a backend without notification hears no commit
	h.srv.PeerTail = time.Millisecond
	h.handler = h.srv.Handler()
	connection := openNotifyStream(h.historyHarness, "tail-task", streamQuery(true))
	defer connection.close()
	if frame := <-connection.recorder.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	h.commit(t, "tail-task", "merge.requested")
	for {
		frame := <-connection.recorder.flushes
		if frame == activityFrame {
			return
		}
		if frame != "" {
			t.Fatalf("peer-tail frame=%q", frame)
		}
	}
}
