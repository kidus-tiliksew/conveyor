package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type demoArgs struct {
	Key string `json:"key"`
}

func (demoArgs) Kind() string { return "demo" }

func TestEnqueueIsUniqueWhileActiveAndReopensAfterCompletion(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	now := time.Now().UTC()
	inserted, err := Enqueue(ctx, log, "ws", "demo", "k1", demoArgs{Key: "k1"}, 3, now)
	if err != nil || !inserted {
		t.Fatalf("first enqueue inserted=%t err=%v", inserted, err)
	}
	inserted, err = Enqueue(ctx, log, "ws", "demo", "k1", demoArgs{Key: "k1"}, 3, now)
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue inserted=%t err=%v", inserted, err)
	}
	job, err := Load(ctx, log, "ws", StreamFor("demo", "k1"))
	if err != nil || job.State != StateAvailable || job.Generation != 1 || job.MaxAttempts != 3 || job.Kind != "demo" || job.Key != "k1" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	// Complete it by hand, then a new enqueue opens generation 2.
	appendKind(t, log, "ws", job.Stream, job.Head, KindClaimed, claimedPayload{Attempt: 1, Worker: "w", ClaimedAt: now})
	appendKind(t, log, "ws", job.Stream, job.Head+1, KindCompleted, outcomePayload{Attempt: 1})
	inserted, err = Enqueue(ctx, log, "ws", "demo", "k1", demoArgs{Key: "k1"}, 3, now)
	if err != nil || !inserted {
		t.Fatalf("re-enqueue after completion inserted=%t err=%v", inserted, err)
	}
	job, _ = Load(ctx, log, "ws", job.Stream)
	if job.Generation != 2 || job.State != StateAvailable || job.Attempt != 0 {
		t.Fatalf("generation 2 job=%+v", job)
	}
	if _, err := Enqueue(ctx, log, "ws", "demo", "k1", demoArgs{}, 0, now); err == nil {
		t.Fatal("zero max attempts accepted")
	}
}

func TestFoldTracksAttemptsSnoozeRescueAndDiscard(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	stream := StreamFor("demo", "k")
	events := []eventlog.Event{
		mk(stream, 1, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "k", Args: []byte(`{"key":"k"}`), MaxAttempts: 2, ScheduledAt: now}, now),
		mk(stream, 2, KindClaimed, claimedPayload{Attempt: 1, Worker: "a", ClaimedAt: now}, now),
		mk(stream, 3, KindSnoozed, outcomePayload{Attempt: 1, Until: now.Add(time.Minute)}, now),
	}
	job, err := Fold("ws", stream, events)
	if err != nil || job.State != StateScheduled || job.Attempt != 0 || !job.ScheduledAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("after snooze job=%+v err=%v", job, err)
	}
	if job.Claimable(now) || !job.Claimable(now.Add(time.Minute)) {
		t.Fatal("snoozed job claimable at the wrong time")
	}
	events = append(events,
		mk(stream, 4, KindClaimed, claimedPayload{Attempt: 1, Worker: "a", ClaimedAt: now.Add(time.Minute)}, now),
		mk(stream, 5, KindFailed, outcomePayload{Attempt: 1, Error: "boom", NextAt: now.Add(2 * time.Minute)}, now),
	)
	job, _ = Fold("ws", stream, events)
	if job.State != StateScheduled || job.Attempt != 1 || job.LastError != "boom" || job.Active() == false {
		t.Fatalf("after failure job=%+v", job)
	}
	events = append(events,
		mk(stream, 6, KindClaimed, claimedPayload{Attempt: 2, Worker: "b", ClaimedAt: now.Add(2 * time.Minute)}, now),
		mk(stream, 7, KindDiscarded, outcomePayload{Attempt: 2, Error: "boom again"}, now),
	)
	job, _ = Fold("ws", stream, events)
	if job.State != StateDiscarded || job.Attempt != 2 || job.Active() || job.Claimable(now.Add(time.Hour)) || job.LastError != "boom again" {
		t.Fatalf("after discard job=%+v", job)
	}
	if job.ID() != string(stream)+"@1" || job.Head != 7 {
		t.Fatalf("id=%s head=%d", job.ID(), job.Head)
	}
}

func mk(stream eventlog.StreamID, version eventlog.Version, kind string, payload any, at time.Time) eventlog.Event {
	encoded, _ := jsonMarshal(payload)
	return eventlog.Event{Workspace: "ws", Stream: stream, Version: version, Position: eventlog.Position(version), Kind: kind, Payload: encoded, At: at}
}

func appendKind(t *testing.T, log eventlog.Store, workspace string, stream eventlog.StreamID, expected eventlog.Version, kind string, payload any) {
	t.Helper()
	encoded, _ := jsonMarshal(payload)
	if _, err := log.Append(context.Background(), workspace, stream, expected, []eventlog.NewEvent{{Kind: kind, Payload: encoded}}); err != nil {
		t.Fatal(err)
	}
}

// runtimeFixture builds a runtime with fast polling and a handler that
// records what it saw.
type runtimeFixture struct {
	log     *memlog.Store
	rt      *Runtime
	mu      sync.Mutex
	calls   []queueargs.Job
	outcome func(job queueargs.Job) error
}

func newRuntimeFixture(t *testing.T, opts Options) *runtimeFixture {
	t.Helper()
	f := &runtimeFixture{log: memlog.New()}
	opts.PollInterval = 20 * time.Millisecond
	if opts.ClockInterval == 0 {
		opts.ClockInterval = -1
	}
	if len(opts.Workspaces) == 0 {
		opts.Workspaces = []string{"ws"}
	}
	f.rt = NewRuntime(f.log, opts)
	f.rt.Register(queueargs.Registration{Kind: "demo", Handle: func(_ context.Context, job queueargs.Job) error {
		f.mu.Lock()
		f.calls = append(f.calls, job)
		outcome := f.outcome
		f.mu.Unlock()
		if outcome != nil {
			return outcome(job)
		}
		return nil
	}, RetryDelay: func(attempt int) time.Duration { return 30 * time.Millisecond }})
	return f
}

func (f *runtimeFixture) start(t *testing.T) {
	t.Helper()
	if err := f.rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.rt.StopAndCancel(ctx)
	})
}

func (f *runtimeFixture) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *runtimeFixture) state(t *testing.T, key string) Job {
	t.Helper()
	job, err := Load(context.Background(), f.log, "ws", StreamFor("demo", key))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *runtimeFixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestRuntimeRunsJobsOneAtATimeInEnqueueOrder(t *testing.T) {
	f := newRuntimeFixture(t, Options{})
	var concurrent, peak int32
	f.outcome = func(queueargs.Job) error {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return nil
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if _, err := Enqueue(context.Background(), f.log, "ws", "demo", fmt.Sprintf("k%d", i), demoArgs{Key: fmt.Sprintf("k%d", i)}, 3, now); err != nil {
			t.Fatal(err)
		}
	}
	f.start(t)
	f.waitFor(t, "five completions", func() bool {
		for i := 0; i < 5; i++ {
			if f.state(t, fmt.Sprintf("k%d", i)).State != StateCompleted {
				return false
			}
		}
		return true
	})
	if atomic.LoadInt32(&peak) != 1 {
		t.Fatalf("peak concurrency=%d, want 1", peak)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, call := range f.calls {
		if want := fmt.Sprintf("job/demo:k%d@1", i); call.ID != want || call.Attempt != 1 || call.MaxAttempts != 3 {
			t.Fatalf("call %d=%+v want id %s", i, call, want)
		}
	}
}

func TestRuntimeRetriesThenDiscardsAndSnoozeDoesNotCount(t *testing.T) {
	f := newRuntimeFixture(t, Options{})
	var seen int32
	f.outcome = func(job queueargs.Job) error {
		// First execution snoozes, which hands the attempt back; the next
		// two executions fail, and the second of those exhausts max 2.
		if atomic.AddInt32(&seen, 1) == 1 {
			return queueargs.Snooze(20 * time.Millisecond)
		}
		return errors.New("boom")
	}
	if _, err := Enqueue(context.Background(), f.log, "ws", "demo", "k", demoArgs{Key: "k"}, 2, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.waitFor(t, "discard", func() bool { return f.state(t, "k").State == StateDiscarded })
	job := f.state(t, "k")
	if job.Attempt != 2 || job.LastError != "boom" {
		t.Fatalf("discarded job=%+v", job)
	}
	// Snooze (attempt 1), retry attempt 1 (fails, scheduled), attempt 2 (discard).
	if got := f.callCount(); got != 3 {
		t.Fatalf("handler calls=%d want 3", got)
	}
	f.mu.Lock()
	attempts := []int{f.calls[0].Attempt, f.calls[1].Attempt, f.calls[2].Attempt}
	f.mu.Unlock()
	if attempts[0] != 1 || attempts[1] != 1 || attempts[2] != 2 {
		t.Fatalf("attempts=%v want [1 1 2]", attempts)
	}
	events, _ := f.log.Read(context.Background(), "ws", StreamFor("demo", "k"), 0, 0)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := fmt.Sprint([]string{KindEnqueued, KindClaimed, KindSnoozed, KindClaimed, KindFailed, KindClaimed, KindDiscarded})
	if fmt.Sprint(kinds) != want {
		t.Fatalf("kinds=%v", kinds)
	}
}

func TestRuntimeReplicasClaimEachJobOnce(t *testing.T) {
	log := memlog.New()
	var calls int32
	handler := func(_ context.Context, job queueargs.Job) error {
		atomic.AddInt32(&calls, 1)
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	var runtimes []*Runtime
	for i := 0; i < 3; i++ {
		rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, PollInterval: 10 * time.Millisecond, ClockInterval: -1, WorkerID: fmt.Sprintf("replica-%d", i)})
		rt.Register(queueargs.Registration{Kind: "demo", Handle: handler})
		runtimes = append(runtimes, rt)
	}
	const jobs = 12
	now := time.Now().UTC()
	for i := 0; i < jobs; i++ {
		if _, err := Enqueue(context.Background(), log, "ws", "demo", fmt.Sprintf("k%d", i), demoArgs{}, 3, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, rt := range runtimes {
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, rt := range runtimes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = rt.Stop(ctx)
			cancel()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := 0
		for i := 0; i < jobs; i++ {
			job, _ := Load(context.Background(), log, "ws", StreamFor("demo", fmt.Sprintf("k%d", i)))
			if job.State == StateCompleted {
				done++
			}
		}
		if done == jobs {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != jobs {
		t.Fatalf("handler ran %d times for %d jobs", got, jobs)
	}
	for i := 0; i < jobs; i++ {
		events, _ := log.Read(context.Background(), "ws", StreamFor("demo", fmt.Sprintf("k%d", i)), 0, 0)
		claims := 0
		for _, e := range events {
			if e.Kind == KindClaimed {
				claims++
			}
		}
		if claims != 1 {
			t.Fatalf("job k%d claimed %d times", i, claims)
		}
	}
}

func TestRuntimeRescuesStuckClaims(t *testing.T) {
	log := memlog.New()
	now := time.Now().UTC()
	// A claim left behind by a dead worker, older than the threshold.
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "k", demoArgs{}, 3, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "k")
	appendKind(t, log, "ws", stream, 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "dead", ClaimedAt: now.Add(-time.Hour)})
	f := &runtimeFixture{log: log}
	f.rt = NewRuntime(log, Options{Workspaces: []string{"ws"}, PollInterval: 10 * time.Millisecond, ClockInterval: -1, RescueStuckAfter: time.Minute})
	f.rt.Register(queueargs.Registration{Kind: "demo", Handle: func(_ context.Context, job queueargs.Job) error {
		f.mu.Lock()
		f.calls = append(f.calls, job)
		f.mu.Unlock()
		return nil
	}, RetryDelay: func(int) time.Duration { return 0 }})
	f.start(t)
	f.waitFor(t, "rescue and completion", func() bool { return f.state(t, "k").State == StateCompleted })
	events, _ := log.Read(context.Background(), "ws", stream, 0, 0)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{KindEnqueued, KindClaimed, KindRescued, KindClaimed, KindCompleted}) {
		t.Fatalf("kinds=%v", kinds)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0].Attempt != 2 {
		t.Fatalf("rescued run calls=%+v want one call at attempt 2", f.calls)
	}
}

func TestRuntimeStopAndCancelInterruptsHandlerAndRecordsSnooze(t *testing.T) {
	f := newRuntimeFixture(t, Options{})
	started := make(chan struct{})
	f.outcome = func(job queueargs.Job) error {
		close(started)
		return queueargs.Snooze(time.Second)
	}
	// Block the handler until cancellation by wrapping it.
	f.rt.Register(queueargs.Registration{Kind: "demo", Handle: func(ctx context.Context, job queueargs.Job) error {
		close(started)
		<-ctx.Done()
		return queueargs.Snooze(time.Second)
	}})
	if _, err := Enqueue(context.Background(), f.log, "ws", "demo", "k", demoArgs{}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rt.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
	job := f.state(t, "k")
	if job.State != StateScheduled || job.Attempt != 0 {
		t.Fatalf("after cancel job=%+v want scheduled with the attempt handed back", job)
	}
}

func TestRuntimeEnsureWorkspaceAfterStartAndClock(t *testing.T) {
	log := memlog.New()
	var ticks int32
	rt := NewRuntime(log, Options{PollInterval: 10 * time.Millisecond, ClockInterval: 15 * time.Millisecond})
	rt.Register(queueargs.Registration{Kind: queueargs.OrderClockArgs{}.Kind(), Handle: func(_ context.Context, job queueargs.Job) error {
		args, err := queueargs.DecodeArgs[queueargs.OrderClockArgs](job)
		if err != nil || args.WorkspaceID != "late" {
			return fmt.Errorf("clock args=%+v err=%v", args, err)
		}
		atomic.AddInt32(&ticks, 1)
		return nil
	}})
	var ran int32
	rt.Register(queueargs.Registration{Kind: "demo", Handle: func(context.Context, queueargs.Job) error { atomic.AddInt32(&ran, 1); return nil }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
	}()
	if _, err := Enqueue(context.Background(), log, "late", "demo", "k", demoArgs{}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureWorkspace("late"); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureWorkspace("late"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (atomic.LoadInt32(&ran) == 0 || atomic.LoadInt32(&ticks) < 2) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&ran) != 1 || atomic.LoadInt32(&ticks) < 2 {
		t.Fatalf("ran=%d ticks=%d", ran, ticks)
	}
	if snap := rt.Snapshot("late"); len(snap) != 1 || snap[0].State != StateCompleted {
		t.Fatalf("snapshot=%+v", snap)
	}
}

func TestDefaultRetryDelayDoublesAndCaps(t *testing.T) {
	if defaultRetryDelay(1) != time.Second || defaultRetryDelay(2) != 2*time.Second || defaultRetryDelay(4) != 8*time.Second {
		t.Fatal("unexpected early delays")
	}
	if defaultRetryDelay(40) != time.Hour {
		t.Fatalf("cap=%s", defaultRetryDelay(40))
	}
}
