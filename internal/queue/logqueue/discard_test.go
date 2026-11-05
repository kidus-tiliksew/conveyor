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

// shortenDiscardRetry shortens both retry drivers, because either the poll loop
// or the alarm can be the rescue whose handler call fails.
func shortenDiscardRetry(t *testing.T) {
	t.Helper()
	previousLoad, previousAlarm := loadRetryFirst, alarmRetryMin
	loadRetryFirst, alarmRetryMin = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { loadRetryFirst, alarmRetryMin = previousLoad, previousAlarm })
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
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,claimed,discarded" {
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
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,claimed,discarded" || job.LastError != "run failed" {
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
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,claimed,claimed,discarded" {
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
	clock := newDiscardClock(time.Now().UTC())
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "first", Now: clock.Now})
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
	clock.Advance(loadRetryMax)
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

// A discard append that fails after the handler succeeded is retried without
// running the handler again, so a state the owner has since changed is not
// overwritten. The retry still renews the claim, and one discard is recorded.
func TestFailedDiscardAppendRetriesWithoutRunningTheHandlerAgain(t *testing.T) {
	log := &failingDiscardAppend{NotifyingLog: notifyingLog()}
	log.failures.Store(1)
	stream := stuckExhaustedJob(t, log, "k")
	var calls atomic.Int32
	clock := newDiscardClock(time.Now().UTC())
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "replica", Now: clock.Now})
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
	clock.Advance(loadRetryMax)
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,claimed,discarded" || calls.Load() != 1 {
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

// discardClock is a clock a test moves by hand, so a claim can reach exactly the
// point a scenario needs.
type discardClock struct {
	mu  sync.Mutex
	now time.Time
}

func newDiscardClock(now time.Time) *discardClock { return &discardClock{now: now} }

func (c *discardClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *discardClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// finalizationEntity stands in for the task or row a discard handler finalizes.
// It records how many handlers ran at once and each effect that landed.
type finalizationEntity struct {
	inside, overlaps, effects atomic.Int32
}

func (e *finalizationEntity) enter() func() {
	if e.inside.Add(1) > 1 {
		e.overlaps.Add(1)
	}
	return func() { e.inside.Add(-1) }
}

// A retry that starts just before its claim expires renews the claim first, so
// crossing the old claim's threshold while the handler is blocked lets no other
// replica in, whether its view predates the renewal or not. This is the
// reviewer's two-replica interleaving.
func TestDiscardRetryNearExpiryKeepsOtherReplicasOut(t *testing.T) {
	const threshold = time.Hour
	for _, tc := range []struct {
		name string
		// freshView has B read the log after A renews; otherwise before.
		freshView bool
	}{{"BViewPredatesTheRenewal", false}, {"BViewFollowsTheRenewal", true}} {
		t.Run(tc.name, func(t *testing.T) {
			log := notifyingLog()
			stream := stuckExhaustedJob(t, log, "k")
			ctx := context.Background()
			start := time.Now().UTC()
			clockA, clockB := newDiscardClock(start), newDiscardClock(start)
			var entity finalizationEntity
			var callsA, callsB atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			a := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "a", Now: clockA.Now})
			a.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
				Discarded: func(handlerCtx context.Context, _ queue.Job, _ string) error {
					defer entity.enter()()
					if callsA.Add(1) == 1 {
						return errors.New("store unavailable")
					}
					close(entered)
					<-release
					if err := handlerCtx.Err(); err != nil {
						return err
					}
					entity.effects.Add(1)
					return nil
				}})
			b := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "b", Now: clockB.Now})
			b.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
				Discarded: func(context.Context, queue.Job, string) error {
					defer entity.enter()()
					callsB.Add(1)
					entity.effects.Add(1)
					return nil
				}})
			wsA, wsB := a.spaces["ws"], b.spaces["ws"]
			if err := a.catchUp(ctx, wsA); err != nil {
				t.Fatal(err)
			}
			if err := a.rescue(ctx, wsA); err == nil {
				t.Fatal("the first handler call did not leave a retry")
			}
			if !tc.freshView {
				if err := b.catchUp(ctx, wsB); err != nil {
					t.Fatal(err)
				}
			}
			// A's retry starts one second before the claim it holds expires.
			clockA.Advance(threshold - time.Second)
			done := make(chan error, 1)
			go func() { done <- a.rescue(ctx, wsA) }()
			<-entered
			if tc.freshView {
				if err := b.catchUp(ctx, wsB); err != nil {
					t.Fatal(err)
				}
			}
			// B stands past the threshold of A's old claim.
			clockB.Advance(threshold + time.Second)
			if err := b.rescue(ctx, wsB); err != nil {
				t.Fatalf("B rescue err=%v", err)
			}
			if callsB.Load() != 0 {
				t.Fatal("B ran the handler while A held a renewed claim")
			}
			if view := b.Snapshot("ws"); len(view) != 1 || view[0].ClaimedBy != "a" {
				t.Fatalf("B's view after losing=%+v", view)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if entity.overlaps.Load() != 0 || entity.effects.Load() != 1 {
				t.Fatalf("overlaps=%d effects=%d", entity.overlaps.Load(), entity.effects.Load())
			}
			if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,claimed,claimed,discarded" {
				t.Fatalf("kinds=%s", kinds)
			}
		})
	}
}

// When a claim expires during a blocked handler, the handler's context has
// already ended, so its late write has no effect. Another replica then takes the
// job over and finishes it, and the first replica's retry loses its claim append
// and runs nothing, even after a newer generation has started.
func TestClaimExpiryDuringTheHandlerStopsItsWrites(t *testing.T) {
	const threshold = 100 * time.Millisecond
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	ctx := context.Background()
	var entity finalizationEntity
	var callsA atomic.Int32
	clockA := newDiscardClock(time.Now().UTC())
	a := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "a", Now: clockA.Now})
	a.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(handlerCtx context.Context, _ queue.Job, _ string) error {
			defer entity.enter()()
			callsA.Add(1)
			deadline, ok := handlerCtx.Deadline()
			if !ok || time.Until(deadline) > threshold/2 {
				t.Errorf("handler context deadline=%v ok=%t, want within half the threshold", deadline, ok)
			}
			<-handlerCtx.Done()
			if handlerCtx.Err() == nil {
				entity.effects.Add(1)
			}
			return handlerCtx.Err()
		}})
	wsA := a.spaces["ws"]
	if err := a.catchUp(ctx, wsA); err != nil {
		t.Fatal(err)
	}
	if err := a.rescue(ctx, wsA); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("A rescue err=%v", err)
	}
	if callsA.Load() != 1 || entity.effects.Load() != 0 {
		t.Fatalf("calls=%d effects=%d", callsA.Load(), entity.effects.Load())
	}
	// B takes over once A's claim is stale, and finishes.
	b := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "b", Now: func() time.Time { return time.Now().UTC().Add(2 * threshold) }})
	b.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error {
			defer entity.enter()()
			entity.effects.Add(1)
			return nil
		}})
	wsB := b.spaces["ws"]
	if err := b.catchUp(ctx, wsB); err != nil {
		t.Fatal(err)
	}
	if err := b.rescue(ctx, wsB); err != nil {
		t.Fatal(err)
	}
	if job := jobState(t, log, stream); job.State != StateDiscarded {
		t.Fatalf("job after B=%+v", job)
	}
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("newer generation inserted=%t err=%v", inserted, err)
	}
	head := jobState(t, log, stream).Head
	clockA.Advance(loadRetryMax)
	if err := a.rescue(ctx, wsA); err != nil {
		t.Fatal(err)
	}
	job := jobState(t, log, stream)
	if callsA.Load() != 1 || job.Generation != 2 || job.State != StateAvailable || job.Head != head {
		t.Fatalf("A's stale retry calls=%d job=%+v", callsA.Load(), job)
	}
	if entity.overlaps.Load() != 0 || entity.effects.Load() != 1 || len(wsA.discards) != 0 {
		t.Fatalf("overlaps=%d effects=%d retries=%+v", entity.overlaps.Load(), entity.effects.Load(), wsA.discards)
	}
}

// A handler that wrote before its context ended, and then lost its discard
// append to the context, is not run again. Another replica's takeover finishes
// the job, and the first replica's retry has no effect.
func TestLossDetectedAfterSideEffectsIsNotRepeated(t *testing.T) {
	const threshold = 100 * time.Millisecond
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	ctx := context.Background()
	var callsA, callsB atomic.Int32
	clockA := newDiscardClock(time.Now().UTC())
	a := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "a", Now: clockA.Now})
	a.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(handlerCtx context.Context, _ queue.Job, _ string) error {
			callsA.Add(1)
			<-handlerCtx.Done() // The write landed; the budget ended before the append.
			return nil
		}})
	wsA := a.spaces["ws"]
	if err := a.catchUp(ctx, wsA); err != nil {
		t.Fatal(err)
	}
	// memlog ignores contexts, so fail the append as a durable log does.
	failing := &failingDiscardAppend{NotifyingLog: log}
	failing.failures.Store(1)
	a.log = failing
	if err := a.rescue(ctx, wsA); err == nil {
		t.Fatal("the failed append left no retry")
	}
	if pending := wsA.discards[stream]; !pending.handled {
		t.Fatalf("retry=%+v, want the handler recorded as done", pending)
	}
	b := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold, WorkerID: "b", Now: func() time.Time { return time.Now().UTC().Add(2 * threshold) }})
	b.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { callsB.Add(1); return nil }})
	wsB := b.spaces["ws"]
	if err := b.catchUp(ctx, wsB); err != nil {
		t.Fatal(err)
	}
	if err := b.rescue(ctx, wsB); err != nil {
		t.Fatal(err)
	}
	clockA.Advance(loadRetryMax)
	if err := a.rescue(ctx, wsA); err != nil {
		t.Fatal(err)
	}
	discards := strings.Count(streamKinds(t, log, stream), "discarded")
	if callsA.Load() != 1 || callsB.Load() != 1 || discards != 1 || len(wsA.discards) != 0 {
		t.Fatalf("calls a=%d b=%d discards=%d retries=%+v", callsA.Load(), callsB.Load(), discards, wsA.discards)
	}
}

// While a handler runs, an enqueue arriving mid-call is suppressed, so no newer
// generation can start under it. After the discard, the next enqueue starts one
// and nothing of the old attempt touches it.
func TestNewerGenerationCannotStartMidHandler(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	ctx := context.Background()
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: "replica"})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error {
			inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC())
			if err != nil || inserted {
				t.Errorf("enqueue mid-handler inserted=%t err=%v", inserted, err)
			}
			return nil
		}})
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	job := jobState(t, log, stream)
	if job.State != StateDiscarded || job.Generation != 1 {
		t.Fatalf("job=%+v", job)
	}
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("enqueue after the discard inserted=%t err=%v", inserted, err)
	}
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if job = jobState(t, log, stream); job.Generation != 2 || job.State != StateAvailable {
		t.Fatalf("newer generation=%+v", job)
	}
}

// A handler's last attempt whose claim another replica has already taken over
// loses its claim append and runs no discard handler, even when the takeover
// finished and a newer generation is waiting on the stream. That generation then
// runs normally.
func TestLastAttemptAfterATakeoverRunsNoDiscardHandler(t *testing.T) {
	log := notifyingLog()
	ctx := context.Background()
	if _, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "k")
	var discards, runs atomic.Int32
	handling, release := make(chan struct{}), make(chan struct{})
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, WorkerID: "slow"})
	rt.Register(queue.Registration{Kind: "demo",
		Handle: func(context.Context, queue.Job) error {
			if runs.Add(1) > 1 {
				return nil
			}
			close(handling)
			<-release
			return errors.New("run failed")
		},
		Discarded: func(context.Context, queue.Job, string) error { discards.Add(1); return nil }})
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopRuntimes(t, rt)
	<-handling
	job := jobState(t, log, stream)
	appendKind(t, log, "ws", stream, job.Head, KindDiscarded, outcomePayload{Attempt: 1, Error: "other replica"})
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, time.Now().UTC()); err != nil || !inserted {
		t.Fatalf("newer generation inserted=%t err=%v", inserted, err)
	}
	close(release)
	waitUntil(t, "the newer generation to complete", func() bool { return jobState(t, log, stream).State == StateCompleted })
	if discards.Load() != 0 {
		t.Fatalf("discard handler ran %d times after a takeover", discards.Load())
	}
	if kinds := streamKinds(t, log, stream); kinds != "enqueued,claimed,discarded,enqueued,claimed,completed" {
		t.Fatalf("kinds=%s", kinds)
	}
}

// A wake before a retry is due appends nothing, so a busy workspace does not
// grow the log while a handler keeps failing. Both callers still see an error,
// so each keeps its retry armed.
func TestDiscardRetryWaitsForItsBackoff(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	ctx := context.Background()
	var calls atomic.Int32
	clock := newDiscardClock(time.Now().UTC())
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Hour, WorkerID: "replica", Now: clock.Now})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error { calls.Add(1); return errors.New("store unavailable") }})
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := rt.rescue(ctx, ws); err == nil {
		t.Fatal("rescue reported no failure")
	}
	head := jobState(t, log, stream).Head
	if err := rt.rescue(ctx, ws); !errors.Is(err, errDiscardRetryWaiting) {
		t.Fatalf("early wake err=%v", err)
	}
	if got := jobState(t, log, stream).Head; got != head || calls.Load() != 1 {
		t.Fatalf("an early wake appended: head %d->%d calls=%d", head, got, calls.Load())
	}
	clock.Advance(loadRetryFirst)
	if err := rt.rescue(ctx, ws); errors.Is(err, errDiscardRetryWaiting) || err == nil {
		t.Fatalf("due retry err=%v", err)
	}
	if got := jobState(t, log, stream).Head; got != head+1 || calls.Load() != 2 {
		t.Fatalf("due retry: head %d->%d calls=%d", head, got, calls.Load())
	}
	// The delay doubles while the failure lasts, up to the bound.
	clock.Advance(loadRetryFirst)
	if err := rt.rescue(ctx, ws); !errors.Is(err, errDiscardRetryWaiting) {
		t.Fatalf("second wait err=%v", err)
	}
}

// The protected time stays within half the rescue threshold and the cap.
func TestDiscardBudget(t *testing.T) {
	for _, tc := range []struct {
		threshold, want time.Duration
	}{
		{0, discardHandlerTimeout},
		{time.Hour, discardHandlerTimeout},
		{time.Minute, discardHandlerTimeout},
		{20 * time.Second, 10 * time.Second},
	} {
		rt := NewRuntime(notifyingLog(), Options{RescueStuckAfter: tc.threshold})
		if got := rt.discardBudget(); got != tc.want {
			t.Errorf("threshold %s: budget=%s, want %s", tc.threshold, got, tc.want)
		}
	}
}

// The poll loop and the alarm both rescue. A snapshot taken before the other's
// failed attempt must not conflict with this process's own claim and read as
// losing the job, which would drop the retry until the claim went stale.
func TestAStaleSnapshotInThisProcessKeepsItsRetry(t *testing.T) {
	log := notifyingLog()
	stream := stuckExhaustedJob(t, log, "k")
	ctx := context.Background()
	var calls atomic.Int32
	clock := newDiscardClock(time.Now().UTC())
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Hour, WorkerID: "replica", Now: clock.Now})
	reg := queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { return nil },
		Discarded: func(context.Context, queue.Job, string) error {
			if calls.Add(1) == 1 {
				return errors.New("store unavailable")
			}
			return nil
		}}
	rt.Register(reg)
	ws := rt.spaces["ws"]
	if err := rt.catchUp(ctx, ws); err != nil {
		t.Fatal(err)
	}
	snapshot := rt.Snapshot("ws")[0]
	if err := rt.rescue(ctx, ws); err == nil {
		t.Fatal("rescue reported no failure")
	}
	head := jobState(t, log, stream).Head
	if err := rt.rescueWithDiscardHandler(ws, reg, snapshot, "stale", false); err != nil {
		t.Fatalf("stale snapshot err=%v", err)
	}
	if got := jobState(t, log, stream).Head; got != head || calls.Load() != 1 {
		t.Fatalf("stale snapshot acted: head %d->%d calls=%d", head, got, calls.Load())
	}
	if _, retrying := ws.discards[stream]; !retrying {
		t.Fatal("a stale snapshot dropped this process's retry")
	}
	clock.Advance(loadRetryFirst)
	if err := rt.rescue(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if job := jobState(t, log, stream); job.State != StateDiscarded || calls.Load() != 2 {
		t.Fatalf("job=%+v calls=%d", job, calls.Load())
	}
}
