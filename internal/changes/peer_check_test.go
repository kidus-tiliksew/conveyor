package changes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePeerReader returns queued results, one per read, and counts reads.
type fakePeerReader struct {
	mu      sync.Mutex
	results []fakePeerResult
	reads   int
}

type fakePeerResult struct {
	marks map[string]PeerMark
	err   error
}

func (f *fakePeerReader) push(marks map[string]PeerMark, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, fakePeerResult{marks: marks, err: err})
}

func (f *fakePeerReader) PeerMarks(context.Context) (map[string]PeerMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if len(f.results) == 0 {
		return nil, errors.New("no result queued")
	}
	next := f.results[0]
	f.results = f.results[1:]
	return next.marks, next.err
}

func (f *fakePeerReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// peerRecorder subscribes to every job and task notice of the given
// workspaces and drains what a check published.
type peerRecorder struct {
	subs map[string]*Subscription
}

func newPeerRecorder(hub *Hub, workspaces ...string) *peerRecorder {
	r := &peerRecorder{subs: map[string]*Subscription{}}
	for _, ws := range workspaces {
		r.subs[ws+":job"] = hub.Subscribe(ws, Job)
		r.subs[ws+":task"] = hub.SubscribeTask(ws, "some-task")
	}
	return r
}

// drain returns the sorted keys of subscriptions holding a notice.
func (r *peerRecorder) drain(t *testing.T) []string {
	t.Helper()
	var got []string
	for key, sub := range r.subs {
		if n, ok := received(t, sub); ok {
			if strings.HasSuffix(key, ":task") && n.TaskID != "" {
				t.Fatalf("%s: peer task notice names task %q", key, n.TaskID)
			}
			got = append(got, key)
		}
	}
	return sortedStrings(got)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func requireKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(sortedStrings(want)) {
		t.Fatalf("%s: notices=%v, want %v", what, got, sortedStrings(want))
	}
}

// TestPeerCheckPublishesWhatChanged: each check compares every workspace's
// marks with the previous successful check, never with a clock, and
// publishes a job notice for a moved log head and one workspace-wide task
// notice for changed task events (feature-quiet-idle-runtime, Peers).
func TestPeerCheckPublishesWhatChanged(t *testing.T) {
	hub := NewHub()
	reader := &fakePeerReader{}
	check := &PeerCheck{Reader: reader, Hub: hub, Interval: time.Minute}
	rec := newPeerRecorder(hub, "a", "b", "c")
	ctx := context.Background()

	// The first check has no previous marks: every workspace it reads gets
	// both notices, so a reader that subscribed before it re-reads once.
	reader.push(map[string]PeerMark{"a": {Log: 3, Events: 5, LastEvent: 9}, "b": {Log: 1}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "first check", rec.drain(t), "a:job", "a:task", "b:job", "b:task")

	// Nothing changed: nothing published.
	reader.push(map[string]PeerMark{"a": {Log: 3, Events: 5, LastEvent: 9}, "b": {Log: 1}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "idle check", rec.drain(t))

	// a's log moved; b gained an event; c appeared with a log only.
	reader.push(map[string]PeerMark{"a": {Log: 4, Events: 5, LastEvent: 9}, "b": {Log: 1, Events: 1, LastEvent: 10}, "c": {Log: 1}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "changed check", rec.drain(t), "a:job", "b:task", "c:job", "c:task")

	// A deletion paired with an insert keeps the count and moves the
	// highest ID; that is still a change.
	reader.push(map[string]PeerMark{"a": {Log: 4, Events: 5, LastEvent: 12}, "b": {Log: 1, Events: 1, LastEvent: 10}, "c": {Log: 1}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "same count, new highest ID", rec.drain(t), "a:task")
	if reader.count() != 4 {
		t.Fatalf("reads=%d, want one per check", reader.count())
	}
}

// TestPeerCheckFailureKeepsMarksAndRetries: a failed read is logged at error
// level, publishes nothing, and keeps the previous marks, so the next check
// finds a change committed before the failure (feature-quiet-idle-runtime,
// Peers).
func TestPeerCheckFailureKeepsMarksAndRetries(t *testing.T) {
	hub := NewHub()
	reader := &fakePeerReader{}
	var logs []string
	var outcomes []string
	check := &PeerCheck{Reader: reader, Hub: hub, Interval: 30 * time.Second,
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		Checked: func(published int, err error) {
			outcomes = append(outcomes, fmt.Sprintf("%d/%v", published, err != nil))
		},
	}
	rec := newPeerRecorder(hub, "a")
	ctx := context.Background()
	reader.push(map[string]PeerMark{"a": {Log: 1, Events: 1, LastEvent: 1}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	rec.drain(t)

	// Another daemon commits a job and a task event; the next read fails.
	failure := errors.New("connection reset")
	reader.push(nil, failure)
	if err := check.Check(ctx); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	requireKeys(t, "failed check", rec.drain(t))
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "level=error ") || !strings.Contains(logs[0], "connection reset") {
		t.Fatalf("logs=%q, want one error-level line naming the failure", logs)
	}

	// The retry compares with the marks before the failure and finds both.
	reader.push(map[string]PeerMark{"a": {Log: 2, Events: 2, LastEvent: 2}}, nil)
	if err := check.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "retry", rec.drain(t), "a:job", "a:task")
	if fmt.Sprint(outcomes) != "[2/false 0/true 2/false]" {
		t.Fatalf("checked outcomes=%v", outcomes)
	}

	// A failure on the very first check keeps no marks: the next success
	// treats every workspace as new.
	fresh := &PeerCheck{Reader: reader, Hub: hub, Interval: time.Second}
	reader.push(nil, failure)
	_ = fresh.Check(ctx)
	reader.push(map[string]PeerMark{"a": {Log: 2, Events: 2, LastEvent: 2}}, nil)
	if err := fresh.Check(ctx); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "first success after a failed first check", rec.drain(t), "a:job", "a:task")
}

// TestPeerCheckRunReadsOncePerTick: Run waits for its first tick (the daemon
// runs the first check itself), reads exactly once per tick of the injected
// ticker, and stops its ticker when ctx ends.
func TestPeerCheckRunReadsOncePerTick(t *testing.T) {
	hub := NewHub()
	reader := &fakePeerReader{}
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	checked := make(chan int, 4)
	var interval time.Duration
	check := &PeerCheck{Reader: reader, Hub: hub, Interval: 30 * time.Second,
		Ticker: func(d time.Duration) (<-chan time.Time, func()) {
			interval = d
			return ticks, func() { close(stopped) }
		},
		Checked: func(published int, err error) { checked <- published },
	}
	for range 3 {
		reader.push(map[string]PeerMark{"a": {Log: 1}}, nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		check.Run(ctx)
		close(done)
	}()
	for i := range 3 {
		ticks <- time.Time{}
		<-checked
		if got := reader.count(); got != i+1 {
			t.Fatalf("after tick %d reads=%d", i+1, got)
		}
	}
	if interval != 30*time.Second {
		t.Fatalf("ticker interval=%s", interval)
	}
	cancel()
	<-done
	select {
	case <-stopped:
	default:
		t.Fatal("Run did not stop its ticker")
	}
	if reader.count() != 3 {
		t.Fatalf("reads=%d after three ticks", reader.count())
	}
}

type fakePeerBackend struct {
	notifies bool
	calls    int
}

func (b *fakePeerBackend) EnablePeers(context.Context) bool {
	b.calls++
	return b.notifies
}

type fakePeerReaderBackend struct {
	fakePeerBackend
	fakePeerReader
}

// TestSelectPeerCheck: runtime.peers off runs nothing and enables nothing;
// a notifying backend runs no check; a non-notifying backend with PeerMarks
// gets one check at the interval; one without it gets an error line
// (component-runtime, Peers).
func TestSelectPeerCheck(t *testing.T) {
	hub := NewHub()
	for _, tc := range []struct {
		name    string
		peers   bool
		backend PeerBackend
		check   bool
		calls   int
		logged  string
	}{
		{name: "off", peers: false, backend: &fakePeerReaderBackend{}, logged: "runtime peers: off"},
		{name: "notifies", peers: true, backend: &fakePeerReaderBackend{fakePeerBackend: fakePeerBackend{notifies: true}}, calls: 1, logged: "runtime peers: PostgreSQL change notification"},
		{name: "peer check", peers: true, backend: &fakePeerReaderBackend{}, check: true, calls: 1, logged: "runtime peers: one peer check every 30s for the whole daemon"},
		{name: "neither", peers: true, backend: &fakePeerBackend{}, calls: 1, logged: `level=error msg="runtime peers: the backend neither notifies peers nor offers a peer check; no peer check runs"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			got := SelectPeerCheck(context.Background(), tc.peers, tc.backend, hub, 30*time.Second, func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})
			if (got != nil) != tc.check {
				t.Fatalf("check=%v, want %t", got, tc.check)
			}
			if got != nil && (got.Interval != 30*time.Second || got.Hub != hub || got.Reader == nil) {
				t.Fatalf("check=%+v", got)
			}
			var calls int
			switch b := tc.backend.(type) {
			case *fakePeerReaderBackend:
				calls = b.calls
			case *fakePeerBackend:
				calls = b.calls
			}
			if calls != tc.calls {
				t.Fatalf("EnablePeers calls=%d, want %d", calls, tc.calls)
			}
			if len(logs) != 1 || logs[0] != tc.logged {
				t.Fatalf("logs=%q, want %q", logs, tc.logged)
			}
		})
	}
}
