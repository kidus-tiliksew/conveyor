package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The workspace change stream (component-http-api, "Workspace change
// stream"; feature-quiet-idle-runtime, "Pushed task streams and
// `await_review`"). The unbuffered flush channel is the barrier: while the
// handler waits in Flush, later commits coalesce in its subscription.

// storeCallCounter counts every task and event read the handler could make.
type storeCallCounter struct {
	store.Store
	mu    sync.Mutex
	calls int
}

func (s *storeCallCounter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *storeCallCounter) bump() {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
}

func (s *storeCallCounter) GetTask(ctx context.Context, id string) (core.Task, error) {
	s.bump()
	return s.Store.GetTask(ctx, id)
}

func (s *storeCallCounter) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	s.bump()
	return s.Store.ListEvents(ctx, taskID)
}

func (s *storeCallCounter) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	s.bump()
	return s.Store.ListCompactEvents(ctx, taskID)
}

func (s *storeCallCounter) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	s.bump()
	return s.Store.ReadTaskEventWindow(ctx, q)
}

func (s *storeCallCounter) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	s.bump()
	return s.Store.ReadTaskEventStream(ctx, q)
}

// countingFlusher counts Flush calls as they start, before the embedded
// recorder blocks handing the chunk to the test, so a test can tell the
// handler is held in Flush.
type countingFlusher struct {
	*sseRecorder
	started atomic.Int64
}

func (r *countingFlusher) Flush() {
	r.started.Add(1)
	r.sseRecorder.Flush()
}

// waitFlushes waits until n flushes have started.
func (r *countingFlusher) waitFlushes(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.started.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("handler started %d flushes, want %d", r.started.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

type workspaceConnection struct {
	*notifyConnection
	flusher *countingFlusher
}

func openWorkspaceChanges(h *historyHarness, workspace, token string) *workspaceConnection {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+workspace+"/changes", nil).WithContext(ctx)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	connection := &notifyConnection{recorder: newSSERecorder(), cancel: cancel, done: make(chan struct{})}
	flusher := &countingFlusher{sseRecorder: connection.recorder}
	go func() {
		h.handler.ServeHTTP(flusher, request)
		close(connection.done)
		close(connection.recorder.flushes)
	}()
	return &workspaceConnection{notifyConnection: connection, flusher: flusher}
}

func TestWorkspaceChangesStream(t *testing.T) {
	base := store.NewMemory()
	ctx := store.WithActor(context.Background(), store.SystemActor())
	demo := store.WithWorkspace(ctx, historyWorkspace)
	other := store.WithWorkspace(ctx, "other")
	for _, task := range []struct {
		ctx context.Context
		id  string
	}{{demo, "ws-a"}, {demo, "ws-b"}, {demo, "ws-c"}, {other, "ws-other"}} {
		workspace, _ := store.WorkspaceFromContext(task.ctx)
		if err := base.CreateTask(task.ctx, core.Task{ID: task.id, Workspace: workspace, State: core.TaskRunning, CreatedAt: historyEpoch}); err != nil {
			t.Fatal(err)
		}
	}
	counter := &storeCallCounter{Store: base}
	h := newHistoryHarness(t, counter)
	h.srv.Changes = memoryHub(t, base)
	keepalive := make(chan time.Time)
	h.srv.StreamKeepalive = func() (<-chan time.Time, func()) { return keepalive, func() {} }
	h.handler = h.srv.Handler()
	commit := func(ctx context.Context, taskID string) {
		t.Helper()
		if err := base.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "dispatch.reconciled"}); err != nil {
			t.Fatal(err)
		}
	}

	connection := openWorkspaceChanges(h, historyWorkspace, historyTokenA)
	if frame := <-connection.recorder.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	if connection.recorder.header.Get("Content-Type") != "text/event-stream" || connection.recorder.header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers=%v", connection.recorder.header)
	}
	commit(demo, "ws-a")
	// Once ws-a's frame is written the handler has taken that notice and is
	// held in Flush; commits on two more tasks coalesce into one pending
	// notice that names no task.
	connection.flusher.waitFlushes(t, 2)
	commit(demo, "ws-b")
	commit(demo, "ws-c")
	if frame := <-connection.recorder.flushes; frame != "event: activity\ndata: {\"task_id\":\"ws-a\"}\n\n" {
		t.Fatalf("task frame=%q", frame)
	}
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("coalesced frame=%q", frame)
	}
	// Another workspace's commit writes nothing: the next flush is the
	// keep-alive.
	commit(other, "ws-other")
	keepalive <- time.Time{}
	if frame := <-connection.recorder.flushes; frame != keepaliveComment {
		t.Fatalf("frame after another workspace's commit=%q", frame)
	}
	commit(demo, "ws-c")
	if frame := <-connection.recorder.flushes; frame != "event: activity\ndata: {\"task_id\":\"ws-c\"}\n\n" {
		t.Fatalf("second task frame=%q", frame)
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("workspace stream made %d store reads", got)
	}
	connection.cancel()
	<-connection.done

	unauthenticated := openWorkspaceChanges(h, historyWorkspace, "")
	<-unauthenticated.done
	if unauthenticated.recorder.code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.recorder.code)
	}
	unknown := openWorkspaceChanges(h, "not-a-member", historyTokenA)
	<-unknown.done
	if unknown.recorder.code != http.StatusNotFound {
		t.Fatalf("unknown workspace status=%d", unknown.recorder.code)
	}
}

// hubSubscriptions counts the hub's open subscriptions. The test reads it
// only while the handler is held in a flush or has returned.
func hubSubscriptions(hub *changes.Hub) int {
	return reflect.ValueOf(hub).Elem().FieldByName("subs").Len()
}

// failingFlusher fails every write once fail is set.
type failingFlusher struct {
	*countingFlusher
	fail atomic.Bool
}

func (r *failingFlusher) Write(p []byte) (int, error) {
	if r.fail.Load() {
		return 0, errors.New("client went away")
	}
	return r.countingFlusher.Write(p)
}

// The workspace stream also writes one activity frame with {} for each
// `workspace` notice of its workspace, such as a new feedback report or a
// document proposal, and still reads nothing from the store
// (component-http-api invariant 30; feature-production-feedback-listening
// FR-5).
func TestFeedbackWorkspaceStreamWritesWorkspaceNotices(t *testing.T) {
	base := store.NewMemory()
	ctx := store.WithActor(context.Background(), store.SystemActor())
	demo := store.WithWorkspace(ctx, historyWorkspace)
	if err := base.CreateTask(demo, core.Task{ID: "ws-a", Workspace: historyWorkspace, State: core.TaskRunning, CreatedAt: historyEpoch}); err != nil {
		t.Fatal(err)
	}
	counter := &storeCallCounter{Store: base}
	h := newHistoryHarness(t, counter)
	hub := memoryHub(t, base)
	h.srv.Changes = hub
	keepalive := make(chan time.Time)
	h.srv.StreamKeepalive = func() (<-chan time.Time, func()) { return keepalive, func() {} }
	h.handler = h.srv.Handler()
	workspaceNotice := func(workspace string) {
		hub.Publish(changes.Notice{Workspace: workspace, Kind: changes.Workspace})
	}
	idle := hubSubscriptions(hub)

	connection := openWorkspaceChanges(h, historyWorkspace, historyTokenA)
	// The handler is held in the ready flush: it has subscribed, so a
	// notice now is written after ready.
	connection.flusher.waitFlushes(t, 1)
	if got := hubSubscriptions(hub); got != idle+2 {
		t.Fatalf("an open stream holds %d subscriptions, want %d", got-idle, 2)
	}
	workspaceNotice(historyWorkspace)
	if frame := <-connection.recorder.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("workspace notice frame=%q, want %q", frame, activityFrame)
	}
	// A task notice keeps its payload.
	if err := base.AppendEvent(demo, core.Event{TaskID: "ws-a", Kind: "dispatch.reconciled"}); err != nil {
		t.Fatal(err)
	}
	if frame := <-connection.recorder.flushes; frame != "event: activity\ndata: {\"task_id\":\"ws-a\"}\n\n" {
		t.Fatalf("task frame=%q", frame)
	}
	// A burst of workspace notices while the handler is held in a flush
	// coalesces into one frame.
	workspaceNotice(historyWorkspace)
	connection.flusher.waitFlushes(t, 4)
	for range 5 {
		workspaceNotice(historyWorkspace)
	}
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("frame before the burst=%q", frame)
	}
	if frame := <-connection.recorder.flushes; frame != activityFrame {
		t.Fatalf("burst frame=%q", frame)
	}
	// Another workspace's notice writes nothing: the next flush is the
	// keep-alive.
	workspaceNotice("other")
	keepalive <- time.Time{}
	if frame := <-connection.recorder.flushes; frame != keepaliveComment {
		t.Fatalf("frame after another workspace's notice=%q", frame)
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("workspace stream made %d store reads", got)
	}
	// A client that goes away ends both subscriptions.
	connection.cancel()
	<-connection.done
	if got := hubSubscriptions(hub); got != idle {
		t.Fatalf("after the client went away %d subscriptions stay open", got-idle)
	}

	// A write that fails ends the stream and both subscriptions.
	request := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+historyWorkspace+"/changes", nil)
	request.Header.Set("Authorization", "Bearer "+historyTokenA)
	failing := &failingFlusher{countingFlusher: &countingFlusher{sseRecorder: newSSERecorder()}}
	done := make(chan struct{})
	go func() {
		h.handler.ServeHTTP(failing, request)
		close(done)
		close(failing.flushes)
	}()
	if frame := <-failing.flushes; frame != readyFrame {
		t.Fatalf("first frame=%q", frame)
	}
	failing.fail.Store(true)
	workspaceNotice(historyWorkspace)
	<-done
	if got := hubSubscriptions(hub); got != idle {
		t.Fatalf("after a failed write %d subscriptions stay open", got-idle)
	}

	// A wake that closes after a normal return ends both subscriptions.
	wake := h.srv.openWorkspaceWake(historyWorkspace)
	if got := hubSubscriptions(hub); got != idle+2 {
		t.Fatalf("a wake holds %d subscriptions", got-idle)
	}
	wake.close()
	if got := hubSubscriptions(hub); got != idle {
		t.Fatalf("after close %d subscriptions stay open", got-idle)
	}
}
