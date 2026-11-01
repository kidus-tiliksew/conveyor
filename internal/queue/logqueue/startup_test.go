package logqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// indexedLog wraps a notifying memlog and records what the runtime reads:
// the streams it reads, every tail's start position, and the index rows
// returned. Hooks let a test fail or interleave an index read.
type indexedLog struct {
	*NotifyingLog
	mem *memlog.Store

	mu            sync.Mutex
	streamsRead   map[eventlog.StreamID]int
	eventsRead    int
	tailFrom      []eventlog.Position
	tailEvents    int
	indexRows     int
	unfinishedErr error
	beforeRows    func()
	tailsBeforeOK int
	loadedOK      bool
}

func newIndexedLog() *indexedLog {
	mem := memlog.New()
	return &indexedLog{NotifyingLog: Notify(mem, changes.NewHub()), mem: mem, streamsRead: map[eventlog.StreamID]int{}}
}

func (l *indexedLog) Read(ctx context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	events, err := l.NotifyingLog.Read(ctx, workspace, stream, after, limit)
	l.mu.Lock()
	l.streamsRead[stream]++
	l.eventsRead += len(events)
	l.mu.Unlock()
	return events, err
}

func (l *indexedLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	events, err := l.NotifyingLog.Tail(ctx, workspace, after, limit)
	l.mu.Lock()
	l.tailFrom = append(l.tailFrom, after)
	l.tailEvents += len(events)
	if !l.loadedOK {
		l.tailsBeforeOK++
	}
	l.mu.Unlock()
	return events, err
}

func (l *indexedLog) UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	l.mu.Lock()
	failure, hook := l.unfinishedErr, l.beforeRows
	l.unfinishedErr, l.beforeRows = nil, nil
	l.mu.Unlock()
	if failure != nil {
		return 0, nil, failure
	}
	position, rows, err := l.NotifyingLog.UnfinishedJobs(ctx, workspace)
	if err == nil && hook != nil {
		// An append commits between the position read and the row read.
		hook()
		_, rows, err = l.NotifyingLog.UnfinishedJobs(ctx, workspace)
	}
	l.mu.Lock()
	l.indexRows += len(rows)
	l.loadedOK = err == nil
	l.mu.Unlock()
	return position, rows, err
}

func (l *indexedLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streamsRead = map[eventlog.StreamID]int{}
	l.eventsRead, l.tailEvents, l.indexRows = 0, 0, 0
	l.tailFrom = nil
}

type startFixture struct {
	log   *indexedLog
	rt    *Runtime
	mu    sync.Mutex
	calls []queue.Job
	logs  []string
}

func newStartFixture(t *testing.T, log *indexedLog, opts Options) *startFixture {
	t.Helper()
	f := &startFixture{log: log}
	opts.Workspaces = []string{"ws"}
	opts.Logf = func(format string, args ...any) {
		f.mu.Lock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
		f.mu.Unlock()
	}
	f.rt = NewRuntime(log, opts)
	f.rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
		f.mu.Lock()
		f.calls = append(f.calls, job)
		f.mu.Unlock()
		return nil
	}, RetryDelay: func(int) time.Duration { return 10 * time.Millisecond }})
	if err := f.rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.rt.StopAndCancel(ctx)
	})
	return f
}

func (f *startFixture) called(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.ID == id {
			return true
		}
	}
	return false
}

func (f *startFixture) logged(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, line := range f.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func appendJob(t *testing.T, log eventlog.Store, key string, expected eventlog.Version, kind string, payload any) {
	t.Helper()
	appendKind(t, log, "ws", StreamFor("demo", key), expected, kind, payload)
}

// finishJob appends a full generation that completes.
func finishJob(t *testing.T, log eventlog.Store, key string, head eventlog.Version, now time.Time) eventlog.Version {
	t.Helper()
	appendJob(t, log, key, head, KindEnqueued, enqueuedPayload{Kind: "demo", Key: key, Args: []byte(`{}`), MaxAttempts: 3})
	appendJob(t, log, key, head+1, KindClaimed, claimedPayload{Attempt: 1, Worker: "old", ClaimedAt: now})
	appendJob(t, log, key, head+2, KindCompleted, outcomePayload{Attempt: 1})
	return head + 3
}

// TestRestartedRuntimeRunsUnfinishedJobsFromTheIndex: a log holding
// finished jobs and one job each available, scheduled, and running under a
// dead claim. A started runtime reads only the unfinished streams, tails
// from the head rather than from zero, and runs all three
// (req-quiet-idle-runtime AC-5.1).
func TestRestartedRuntimeRunsUnfinishedJobsFromTheIndex(t *testing.T) {
	log := newIndexedLog()
	now := time.Now().UTC()
	for i := 0; i < 50; i++ {
		finishJob(t, log, fmt.Sprintf("done-%d", i), 0, now)
	}
	appendJob(t, log, "available", 0, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "available", Args: []byte(`{}`), MaxAttempts: 3})
	appendJob(t, log, "scheduled", 0, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "scheduled", Args: []byte(`{}`), MaxAttempts: 3})
	appendJob(t, log, "scheduled", 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "old", ClaimedAt: now})
	appendJob(t, log, "scheduled", 2, KindFailed, outcomePayload{Attempt: 1, Error: "boom", NextAt: now.Add(-time.Second)})
	appendJob(t, log, "running", 0, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "running", Args: []byte(`{}`), MaxAttempts: 3})
	appendJob(t, log, "running", 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "dead", ClaimedAt: now.Add(-time.Hour)})
	head, _, _ := log.mem.UnfinishedJobs(context.Background(), "ws")
	log.reset()

	f := newStartFixture(t, log, Options{RescueStuckAfter: time.Minute})
	waitUntil(t, "the three unfinished jobs to run", func() bool {
		return f.called("job/demo:available@1") && f.called("job/demo:scheduled@1") && f.called("job/demo:running@1")
	})
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.indexRows != 3 {
		t.Fatalf("index rows read=%d, want the 3 unfinished", log.indexRows)
	}
	for stream := range log.streamsRead {
		if strings.HasPrefix(string(stream), "job/demo:done-") {
			t.Fatalf("start read finished stream %s", stream)
		}
	}
	if len(log.tailFrom) == 0 || log.tailFrom[0] != head {
		t.Fatalf("first tail from %v, want the head %d", log.tailFrom, head)
	}
	if !f.logged("loaded 3 unfinished jobs") {
		t.Fatalf("no load line in %v", f.logs)
	}
}

// TestCatchUpSkipsEventsAnIndexRowAlreadyReflects: an enqueue commits
// after the start-up position is read but before the rows are, so the row
// already holds it and the tail returns it again. It folds once: the job
// runs as generation 1.
func TestCatchUpSkipsEventsAnIndexRowAlreadyReflects(t *testing.T) {
	log := newIndexedLog()
	log.beforeRows = func() {
		appendJob(t, log.mem, "late", 0, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "late", Args: []byte(`{}`), MaxAttempts: 3})
	}
	f := newStartFixture(t, log, Options{})
	waitUntil(t, "the late job to run", func() bool { return f.called("job/demo:late@1") })
	waitUntil(t, "the completion to fold", func() bool {
		snap := f.rt.Snapshot("ws")
		return len(snap) == 1 && snap[0].State == StateCompleted
	})
	if snap := f.rt.Snapshot("ws"); snap[0].Generation != 1 {
		t.Fatalf("generation=%d, want 1: the enqueue folded twice", snap[0].Generation)
	}
	if f.called("job/demo:late@2") {
		t.Fatal("the job ran as a second generation")
	}
}

// TestReenqueuedFinishedJobKeepsItsGeneration: a stream finished three
// times before start is not loaded; when it is enqueued again after start,
// the runtime loads its row and runs generation 4.
func TestReenqueuedFinishedJobKeepsItsGeneration(t *testing.T) {
	log := newIndexedLog()
	now := time.Now().UTC()
	head := eventlog.Version(0)
	for i := 0; i < 3; i++ {
		head = finishJob(t, log, "again", head, now)
	}
	f := newStartFixture(t, log, Options{})
	waitUntil(t, "the start-up load", func() bool { return f.logged("loaded 0 unfinished jobs") })
	if inserted, err := Enqueue(context.Background(), log, "ws", "demo", "again", demoArgs{Key: "again"}, 3, now); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	waitUntil(t, "generation 4 to run", func() bool { return f.called("job/demo:again@4") })
}

// TestStartRepairsAMissingIndexRow: a job appended without its index row,
// as a binary older than the index would, is rebuilt at start and runs.
func TestStartRepairsAMissingIndexRow(t *testing.T) {
	log := newIndexedLog()
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "orphan", demoArgs{Key: "orphan"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	log.mem.DropJobIndexRow("ws", StreamFor("demo", "orphan"))
	f := newStartFixture(t, log, Options{})
	waitUntil(t, "the repaired job to run", func() bool { return f.called("job/demo:orphan@1") })
	if !f.logged("rebuilt 1 job index rows") {
		t.Fatalf("no repair line in %v", f.logs)
	}
}

// TestFailedLoadIsRetriedOnTheNextWake: the first index read fails, the
// loop does not tail, and the next job notice retries the load, after
// which both jobs run.
func TestFailedLoadIsRetriedOnTheNextWake(t *testing.T) {
	log := newIndexedLog()
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "first", demoArgs{Key: "first"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	log.unfinishedErr = errors.New("database unavailable")
	f := newStartFixture(t, log, Options{})
	waitUntil(t, "the failed load to be logged", func() bool { return f.logged("database unavailable") })
	if f.called("job/demo:first@1") {
		t.Fatal("a job ran before the load succeeded")
	}
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "second", demoArgs{Key: "second"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "both jobs to run", func() bool { return f.called("job/demo:first@1") && f.called("job/demo:second@1") })
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.tailsBeforeOK != 0 {
		t.Fatalf("tailed %d times before the load succeeded", log.tailsBeforeOK)
	}
}

// TestLogWithoutAnIndexIsTailedFromZero: a log that keeps no job index
// still starts, by tailing from position zero.
func TestLogWithoutAnIndexIsTailedFromZero(t *testing.T) {
	plain := struct{ eventlog.Store }{memlog.New()}
	if _, err := Enqueue(context.Background(), plain, "ws", "demo", "plain", demoArgs{Key: "plain"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	log := Notify(plain, changes.NewHub())
	if _, _, err := log.UnfinishedJobs(context.Background(), "ws"); !errors.Is(err, jobindex.ErrUnsupported) {
		t.Fatalf("forwarding without an index err=%v", err)
	}
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}})
	ran := make(chan string, 1)
	rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
		ran <- job.ID
		return nil
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.StopAndCancel(context.Background()) })
	select {
	case id := <-ran:
		if id != "job/demo:plain@1" {
			t.Fatalf("ran %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not run")
	}
}

// TestIndexMatchesFoldAcrossReplicasAndPeerTail: two runtimes share one
// log. One learns of jobs only through its 20 ms peer tail, the other
// through notices, and a log writer appends enqueues without a notice. A
// duplicate enqueue of an active job is suppressed. After every job ran
// once, each job stream's index row equals a full fold, and a restarted
// runtime finds nothing unfinished.
func TestIndexMatchesFoldAcrossReplicasAndPeerTail(t *testing.T) {
	mem := memlog.New()
	notifying := Notify(mem, changes.NewHub())
	var mu sync.Mutex
	runs := map[string]int{}
	register := func(rt *Runtime) {
		rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
			mu.Lock()
			runs[job.ID]++
			mu.Unlock()
			return nil
		}})
	}
	start := func(log eventlog.Store, opts Options) *Runtime {
		opts.Workspaces = []string{"ws"}
		rt := NewRuntime(log, opts)
		register(rt)
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = rt.StopAndCancel(ctx)
		})
		return rt
	}
	start(notifying, Options{WorkerID: "a"})
	start(Notify(mem, changes.NewHub()), Options{WorkerID: "b", PeerTail: 20 * time.Millisecond})
	keys := make([]string, 12)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
		writer := eventlog.Store(mem) // no notice: only the peer tail sees these
		if i%2 == 0 {
			writer = notifying
		}
		if inserted, err := Enqueue(context.Background(), writer, "ws", "demo", keys[i], demoArgs{Key: keys[i]}, 3, time.Now()); err != nil || !inserted {
			t.Fatalf("enqueue %s inserted=%v err=%v", keys[i], inserted, err)
		}
	}
	if inserted, err := Enqueue(context.Background(), notifying, "ws", "demo", "k0", demoArgs{Key: "k0"}, 3, time.Now()); err != nil {
		t.Fatal(err)
	} else if inserted {
		// k0 may already have completed; then the enqueue is a new generation.
		keys = append(keys, "k0@2")
	}
	waitUntil(t, "every job to complete", func() bool {
		for _, key := range keys {
			name, _, _ := strings.Cut(key, "@")
			job, err := Load(context.Background(), mem, "ws", StreamFor("demo", name))
			if err != nil || job.State != StateCompleted {
				return false
			}
		}
		return true
	})
	mu.Lock()
	for id, n := range runs {
		if n != 1 {
			mu.Unlock()
			t.Fatalf("%s ran %d times", id, n)
		}
	}
	mu.Unlock()
	for i := range 12 {
		stream := StreamFor("demo", fmt.Sprintf("k%d", i))
		events, _ := mem.Read(context.Background(), "ws", stream, 0, 0)
		job, err := Fold("ws", stream, events)
		if err != nil {
			t.Fatal(err)
		}
		row, ok, _ := mem.JobIndexRow(context.Background(), "ws", stream)
		if want := jobindex.RowOf(job); !ok || row != want {
			t.Fatalf("%s row=%+v, full fold=%+v", stream, row, want)
		}
	}
	if _, rows, err := mem.UnfinishedJobs(context.Background(), "ws"); err != nil || len(rows) != 0 {
		t.Fatalf("unfinished after every job ran=%v err=%v", rows, err)
	}
}
