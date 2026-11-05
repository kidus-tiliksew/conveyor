package logqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// stuckExhaustedJob leaves a job claimed by a dead worker on its last attempt.
func stuckExhaustedJob(t *testing.T, log eventlog.Store, key string) eventlog.StreamID {
	t.Helper()
	claimedAt := time.Now().UTC().Add(-time.Hour)
	if _, err := Enqueue(context.Background(), log, "ws", "demo", key, demoArgs{Key: key}, 1, claimedAt); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", key)
	appendKind(t, log, "ws", stream, 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "dead", ClaimedAt: claimedAt})
	return stream
}

func streamKinds(t *testing.T, log eventlog.Store, stream eventlog.StreamID) string {
	t.Helper()
	events, err := log.Read(context.Background(), "ws", stream, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, strings.TrimPrefix(event.Kind, "job."))
	}
	return strings.Join(kinds, ",")
}

func shortenDiscardRetry(t *testing.T) {
	t.Helper()
	previous := loadRetryFirst
	loadRetryFirst = 20 * time.Millisecond
	t.Cleanup(func() { loadRetryFirst = previous })
}

func stopRuntimes(t *testing.T, runtimes ...*Runtime) {
	t.Helper()
	t.Cleanup(func() {
		for _, rt := range runtimes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = rt.StopAndCancel(ctx)
			cancel()
		}
	})
}

func jobState(t *testing.T, log eventlog.Store, stream eventlog.StreamID) Job {
	t.Helper()
	job, err := Load(context.Background(), log, "ws", stream)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A rescue whose discard handler fails on its first store operation keeps the
// job active and retries with no notice and no restart. The discard is
// recorded only after the handler succeeds, and the idle loop then stops
// (req-task-lifecycle-and-queue AC-6.2; req-quiet-idle-runtime AC-1.3).
func TestRescueRetriesAFailedDiscardHandlerWithoutANotice(t *testing.T) {
	shortenDiscardRetry(t)
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	notices := log.Changes().Subscribe("ws", changes.Job)
	defer notices.Close()
	var calls atomic.Int32
	release := make(chan struct{})
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "replica"})
	rt.Register(queue.Registration{Kind: "demo",
		Handle: func(context.Context, queue.Job) error { t.Error("job handler ran"); return nil },
		Discarded: func(_ context.Context, job queue.Job, reason string) error {
			if job.Attempt != 1 || job.MaxAttempts != 1 || !strings.Contains(reason, "rescued: claim exceeded") {
				t.Errorf("job=%+v reason=%q", job, reason)
			}
			if calls.Add(1) == 1 {
				defer close(release)
				return errors.New("store unavailable")
			}
			return nil
		}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopRuntimes(t, rt)
	<-release
	waitUntil(t, "the retried discard", func() bool { return jobState(t, log, stream).State == StateDiscarded })
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,discarded" {
		t.Fatalf("kinds=%s", kinds)
	}
	if calls.Load() != 2 {
		t.Fatalf("discard handler calls=%d, want 2", calls.Load())
	}
	select {
	case n := <-notices.C():
		t.Fatalf("recovery depended on notice %+v", n)
	default:
	}
	time.Sleep(10 * loadRetryFirst)
	if calls.Load() != 2 {
		t.Fatalf("discard handler ran again after success: %d", calls.Load())
	}
}

// While a discard handler keeps failing the job stays active, so an enqueue
// cannot start a newer generation under it.
func TestFailedDiscardKeepsTheJobActive(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "replica"})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { return errors.New("store unavailable") }})
	ctx := context.Background()
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := rt.rescue(ctx, ws); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("rescue err=%v", err)
	}
	if job := jobState(t, log, stream); job.State != StateRunning || !job.Active() {
		t.Fatalf("job=%+v", job)
	}
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || inserted {
		t.Fatalf("enqueue under a failed discard inserted=%t err=%v", inserted, err)
	}
}

// A handler's failed last attempt confirms its stop through the discard handler
// and records the discard only after that succeeds, with no second run of the
// job handler and no further notice.
func TestLastAttemptRetriesAFailedDiscardHandler(t *testing.T) {
	shortenDiscardRetry(t)
	log := notifyingLog()
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "k")
	var handled, calls atomic.Int32
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, WorkerID: "replica"})
	rt.Register(queue.Registration{Kind: "demo",
		Handle: func(context.Context, queue.Job) error { handled.Add(1); return errors.New("run failed") },
		Discarded: func(_ context.Context, _ queue.Job, reason string) error {
			if reason != "run failed" {
				t.Errorf("reason=%q", reason)
			}
			if calls.Add(1) == 1 {
				return errors.New("store unavailable")
			}
			return nil
		}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopRuntimes(t, rt)
	waitUntil(t, "the retried discard", func() bool { return jobState(t, log, stream).State == StateDiscarded })
	job := jobState(t, log, stream)
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,discarded" || job.LastError != "run failed" {
		t.Fatalf("kinds=%s job=%+v", kinds, job)
	}
	if handled.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("job handler calls=%d discard handler calls=%d", handled.Load(), calls.Load())
	}
}

// Across replicas one owner runs the handler: every failure and retry stays
// with the replica whose claim append won, and one discard is recorded.
func TestDiscardRetryStaysWithOneReplica(t *testing.T) {
	shortenDiscardRetry(t)
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	var mu sync.Mutex
	var workers []string
	var runtimes []*Runtime
	for i := range 3 {
		worker := fmt.Sprintf("replica-%d", i)
		rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: worker})
		rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
			Discarded: func(context.Context, queue.Job, string) error {
				mu.Lock()
				defer mu.Unlock()
				workers = append(workers, worker)
				if len(workers) < 3 {
					return errors.New("store unavailable")
				}
				return nil
			}})
		runtimes = append(runtimes, rt)
	}
	for _, rt := range runtimes {
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	stopRuntimes(t, runtimes...)
	waitUntil(t, "the discard", func() bool { return jobState(t, log, stream).State == StateDiscarded })
	time.Sleep(10 * loadRetryFirst)
	mu.Lock()
	defer mu.Unlock()
	if len(workers) != 3 || workers[0] != workers[1] || workers[1] != workers[2] {
		t.Fatalf("handler ran on %v, want three calls on one replica", workers)
	}
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,discarded" {
		t.Fatalf("kinds=%s", kinds)
	}
}

// A replica whose view is stale loses its claim append and runs no handler
// against a newer generation.
func TestStaleRescueRunsNoDiscardHandler(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	var calls atomic.Int32
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "stale"})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { calls.Add(1); return nil }})
	ctx := context.Background()
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	appendKind(t, log, "ws", stream, 2, KindDiscarded, outcomePayload{Attempt: 1, Error: "other replica"})
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("newer generation inserted=%t err=%v", inserted, err)
	}
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	job := jobState(t, log, stream)
	if calls.Load() != 0 || job.State != StateAvailable || job.Generation != 2 || job.Attempt != 0 {
		t.Fatalf("calls=%d job=%+v", calls.Load(), job)
	}
	view := rt.Snapshot("ws")
	if len(view) != 1 || view[0].State != StateAvailable || view[0].Generation != 2 {
		t.Fatalf("view=%+v", view)
	}
}

// A retry finds that the stream moved on while it waited and leaves the newer
// generation alone.
func TestDiscardRetryLeavesANewerGenerationAlone(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	var calls atomic.Int32
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "first"})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { calls.Add(1); return errors.New("store unavailable") }})
	ctx := context.Background()
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := rt.rescue(ctx, ws); err == nil {
		t.Fatal("rescue reported no failure")
	}
	head := jobState(t, log, stream).Head
	appendKind(t, log, "ws", stream, head, KindDiscarded, outcomePayload{Attempt: 2, Error: "other replica"})
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("newer generation inserted=%t err=%v", inserted, err)
	}
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	job := jobState(t, log, stream)
	if calls.Load() != 1 || job.State != StateAvailable || job.Generation != 2 {
		t.Fatalf("calls=%d job=%+v", calls.Load(), job)
	}
	if len(ws.discards) != 0 {
		t.Fatalf("retry kept: %+v", ws.discards)
	}
}

// failingDiscardAppend fails appends of job.discarded a set number of times.
type failingDiscardAppend struct {
	*NotifyingLog
	failures atomic.Int32
}

func (l *failingDiscardAppend) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	if len(events) == 1 && events[0].Kind == KindDiscarded && l.failures.Add(-1) >= 0 {
		return 0, errors.New("log unavailable")
	}
	return l.NotifyingLog.Append(ctx, workspace, stream, expected, events)
}

// A discard append that fails after the handler succeeded is retried. The
// handler runs again and finds nothing to do, and one discard is recorded.
func TestFailedDiscardAppendIsRetried(t *testing.T) {
	log := &failingDiscardAppend{NotifyingLog: notifyingLog()}
	log.failures.Store(1)
	stream := stuckExhaustedJob(t, log, "k")
	var calls atomic.Int32
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "replica"})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { calls.Add(1); return nil }})
	ctx := context.Background()
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := rt.rescue(ctx, ws); err == nil || !strings.Contains(err.Error(), "log unavailable") {
		t.Fatalf("rescue err=%v", err)
	}
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,discarded" || calls.Load() != 2 {
		t.Fatalf("kinds=%s calls=%d", kinds, calls.Load())
	}
}

// The intent survives the process: after a restart, or on another replica, the
// running job is found again once the failed owner's claim goes stale.
func TestFailedDiscardIsRecoveredAfterItsClaimGoesStale(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	first := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "first"})
	first.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { return errors.New("store unavailable") }})
	ctx := context.Background()
	if err := first.catchUp(ctx, first.spaces["ws"]); err != nil {
		t.Fatal(err)
	}
	if err := first.rescue(ctx, first.spaces["ws"]); err == nil {
		t.Fatal("rescue reported no failure")
	}
	var calls atomic.Int32
	build := func(now time.Time) *Runtime {
		rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "second", Now: func() time.Time { return now }})
		rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
			Discarded: func(context.Context, queue.Job, string) error { calls.Add(1); return nil }})
		if err := rt.catchUp(ctx, rt.spaces["ws"]); err != nil {
			t.Fatal(err)
		}
		return rt
	}
	fresh := build(time.Now().UTC())
	if err := fresh.rescue(ctx, fresh.spaces["ws"]); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("a replica took over a live claim")
	}
	later := build(time.Now().UTC().Add(2 * time.Minute))
	if err := later.rescue(ctx, later.spaces["ws"]); err != nil {
		t.Fatal(err)
	}
	if kinds := streamKinds(t, log, stream); calls.Load() != 1 || kinds != "enqueued,claimed,claimed,claimed,discarded" {
		t.Fatalf("calls=%d kinds=%s", calls.Load(), kinds)
	}
	if job := jobState(t, log, stream); job.State != StateDiscarded {
		t.Fatalf("job=%+v", job)
	}
}
