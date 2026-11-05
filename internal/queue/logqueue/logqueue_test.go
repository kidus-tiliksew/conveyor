package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
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

// TestEnqueueIsAvailableWhateverTheClockResolution: an enqueue is immediate
// however the engine rounds the event time. Linux clocks carry
// nanoseconds and Postgres stores microseconds; a schedule time that
// out-resolves the stored event time must not read as "later".
func TestEnqueueIsAvailableWhateverTheClockResolution(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	now := time.Date(2026, 9, 4, 18, 39, 4, 24384203, time.UTC)
	if _, err := Enqueue(ctx, log, "ws", "demo", "k1", demoArgs{Key: "k1"}, 3, now); err != nil {
		t.Fatal(err)
	}
	job, err := Load(ctx, log, "ws", StreamFor("demo", "k1"))
	if err != nil || job.State != StateAvailable || !job.ScheduledAt.IsZero() || !job.Claimable(now) {
		t.Fatalf("job=%+v err=%v, want available now", job, err)
	}
	// A row written with a nanosecond schedule time equal to the event
	// time folds the same way once the event time has been rounded.
	stream := StreamFor("demo", "k2")
	appendKind(t, log, "ws", stream, 0, KindEnqueued, enqueuedPayload{Kind: "demo", Key: "k2", Args: []byte(`{}`), MaxAttempts: 3, ScheduledAt: now})
	job, err = Load(ctx, log, "ws", stream)
	if err != nil || job.State != StateAvailable {
		t.Fatalf("nanosecond schedule job=%+v err=%v, want available", job, err)
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

// notifyingLog is a memlog that raises job notices, as every backend's Log()
// does; the runtime wakes only on those notices.
func notifyingLog() *NotifyingLog { return Notify(memlog.New(), changes.NewHub()) }

// runtimeFixture builds a runtime on a notifying log and a handler that
// records what it saw.
type runtimeFixture struct {
	log     *NotifyingLog
	rt      *Runtime
	mu      sync.Mutex
	calls   []queue.Job
	outcome func(job queue.Job) error
}

func newRuntimeFixture(t *testing.T, opts Options) *runtimeFixture {
	t.Helper()
	f := &runtimeFixture{log: notifyingLog()}
	if len(opts.Workspaces) == 0 {
		opts.Workspaces = []string{"ws"}
	}
	f.rt = NewRuntime(f.log, opts)
	f.rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
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
	f.outcome = func(queue.Job) error {
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
	f.outcome = func(job queue.Job) error {
		// First execution snoozes, which hands the attempt back; the next
		// two executions fail, and the second of those exhausts max 2.
		if atomic.AddInt32(&seen, 1) == 1 {
			return queue.Snooze(20 * time.Millisecond)
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
	log := notifyingLog()
	var calls int32
	handler := func(_ context.Context, job queue.Job) error {
		atomic.AddInt32(&calls, 1)
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	var runtimes []*Runtime
	for i := 0; i < 3; i++ {
		rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, WorkerID: fmt.Sprintf("replica-%d", i)})
		rt.Register(queue.Registration{Kind: "demo", Handle: handler})
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
	log := notifyingLog()
	now := time.Now().UTC()
	// A claim left behind by a dead worker, older than the threshold.
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "k", demoArgs{}, 3, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "k")
	appendKind(t, log, "ws", stream, 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "dead", ClaimedAt: now.Add(-time.Hour)})
	f := &runtimeFixture{log: log}
	f.rt = NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute})
	f.rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
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

// TestRescueDiscardRunsTheDiscardHandlerOnce: a stuck claim on its last
// attempt is discarded by rescue, and the registration's discard handler runs
// once with the job and the rescue reason, across two replicas; the job
// handler never runs (req-task-lifecycle-and-queue AC-6.2).
func TestRescueDiscardRunsTheDiscardHandlerOnce(t *testing.T) {
	log := notifyingLog()
	now := time.Now().UTC()
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "k", demoArgs{Key: "k"}, 1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "k")
	appendKind(t, log, "ws", stream, 1, KindClaimed, claimedPayload{Attempt: 1, Worker: "dead", ClaimedAt: now.Add(-time.Hour)})
	var mu sync.Mutex
	var discarded []queue.Job
	var reasons []string
	var handled int32
	var runtimes []*Runtime
	for i := 0; i < 2; i++ {
		rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: time.Minute, WorkerID: fmt.Sprintf("replica-%d", i)})
		rt.Register(queue.Registration{Kind: "demo",
			Handle: func(context.Context, queue.Job) error { atomic.AddInt32(&handled, 1); return nil },
			Discarded: func(_ context.Context, job queue.Job, reason string) error {
				mu.Lock()
				defer mu.Unlock()
				discarded = append(discarded, job)
				reasons = append(reasons, reason)
				return nil
			}})
		runtimes = append(runtimes, rt)
	}
	for _, rt := range runtimes {
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, rt := range runtimes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = rt.StopAndCancel(ctx)
			cancel()
		}
	})
	waitUntil(t, "the discard handler", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(discarded) > 0
	})
	// Let both replicas finish their catch-ups and rescues.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(discarded) != 1 {
		t.Fatalf("discard handler ran %d times, want once", len(discarded))
	}
	job := discarded[0]
	if job.ID != "job/demo:k@1" || job.WorkspaceID != "ws" || job.Kind != "demo" || job.Attempt != 1 || job.MaxAttempts != 1 || !strings.Contains(string(job.Args), `"k"`) {
		t.Fatalf("discarded job=%+v", job)
	}
	if !strings.Contains(reasons[0], "rescued: claim exceeded") {
		t.Fatalf("reason=%q", reasons[0])
	}
	if atomic.LoadInt32(&handled) != 0 {
		t.Fatalf("job handler ran %d times for a discarded job", handled)
	}
	if state, err := Load(context.Background(), log, "ws", stream); err != nil || state.State != StateDiscarded {
		t.Fatalf("job=%+v err=%v", state, err)
	}
}

func TestRuntimeStopAndCancelInterruptsHandlerAndRecordsSnooze(t *testing.T) {
	f := newRuntimeFixture(t, Options{})
	started := make(chan struct{})
	f.outcome = func(job queue.Job) error {
		close(started)
		return queue.Snooze(time.Second)
	}
	// Block the handler until cancellation by wrapping it.
	f.rt.Register(queue.Registration{Kind: "demo", Handle: func(ctx context.Context, job queue.Job) error {
		close(started)
		<-ctx.Done()
		return queue.Snooze(time.Second)
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
	log := notifyingLog()
	var ticks int32
	// One stored deadline just after start drives the second fire; like a
	// store, the source returns it until a fire passes it.
	stored := time.Now().UTC().Add(20 * time.Millisecond)
	deadlines := func(_ context.Context, workspace string, after time.Time) (time.Time, bool, error) {
		if after.Before(stored) {
			return stored, true, nil
		}
		return time.Time{}, false, nil
	}
	rt := NewRuntime(log, Options{Deadlines: deadlines})
	rt.Register(queue.Registration{Kind: queue.OrderClockArgs{}.Kind(), Handle: func(_ context.Context, job queue.Job) error {
		args, err := queue.DecodeArgs[queue.OrderClockArgs](job)
		if err != nil || args.WorkspaceID != "late" || job.WorkspaceID != "late" {
			return fmt.Errorf("clock args=%+v err=%v", args, err)
		}
		atomic.AddInt32(&ticks, 1)
		return nil
	}})
	var ran int32
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error { atomic.AddInt32(&ran, 1); return nil }})
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

// TestRuntimeHandlersSeeTrustedPartition: jobs enqueued in a starting
// workspace and in one added to a running runtime run with the trusted
// partition, and a job a handler enqueues wakes the runtime on its notice
// (component-durable-queue).
func TestRuntimeHandlersSeeTrustedPartition(t *testing.T) {
	log := notifyingLog()
	var completed atomic.Int32
	rt := NewRuntime(log, Options{Workspaces: []string{"alpha"}})
	rt.Register(queue.Registration{Kind: "publication", Handle: func(ctx context.Context, job queue.Job) error {
		if job.WorkspaceID != "alpha" && job.WorkspaceID != "beta" {
			t.Errorf("untrusted partition: %q", job.WorkspaceID)
		}
		if completed.Add(1) == 1 {
			// A second job enqueued by a handler wakes the runtime on its notice.
			if _, err := Enqueue(ctx, log, job.WorkspaceID, "publication", "second", map[string]string{"workspace_id": "forged"}, 5, time.Now()); err != nil {
				t.Errorf("enqueue second: %v", err)
			}
		}
		return nil
	}})
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(context.Background())
	if err := rt.EnsureWorkspace("beta"); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"alpha", "beta"} {
		if _, err := Enqueue(t.Context(), log, ws, "publication", "same-pr", map[string]string{"workspace_id": "forged"}, 5, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(3 * time.Second)
	for completed.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("completed=%d, want the two enqueued jobs and the handler's job", completed.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// countingLog counts Tail calls, the statement a catch-up sends.
type countingLog struct {
	*NotifyingLog
	tails atomic.Int32
}

func (l *countingLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	l.tails.Add(1)
	return l.NotifyingLog.Tail(ctx, workspace, after, limit)
}

// TestRuntimeStartsJobWithinOneSecondAndStaysQuietWhileIdle: with no poll
// interval a job committed after start runs on its notice, well inside the
// one-second bound, and an idle runtime sends no Tail (req-quiet-idle-runtime
// AC-1.1, AC-2.1).
func TestRuntimeStartsJobWithinOneSecondAndStaysQuietWhileIdle(t *testing.T) {
	log := &countingLog{NotifyingLog: notifyingLog()}
	started := make(chan time.Time, 1)
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error {
		started <- time.Now()
		return nil
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(context.Background())
	settle := func() int32 {
		// Wait until the tail count stops moving, observed twice in a row.
		last := log.tails.Load()
		for stable := 0; stable < 2; {
			time.Sleep(50 * time.Millisecond)
			if now := log.tails.Load(); now == last {
				stable++
			} else {
				last, stable = now, 0
			}
		}
		return last
	}
	idle := settle()
	if idle == 0 {
		t.Fatal("no catch-up at start")
	}
	time.Sleep(300 * time.Millisecond)
	if got := log.tails.Load(); got != idle {
		t.Fatalf("idle runtime tailed %d more times", got-idle)
	}
	committed := time.Now()
	if _, err := Enqueue(context.Background(), log, "ws", "demo", "k", demoArgs{}, 3, committed.UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-started:
		if latency := at.Sub(committed); latency > 600*time.Millisecond {
			t.Fatalf("job started %s after commit; the bound is one second", latency)
		}
	case <-time.After(time.Second):
		t.Fatal("job did not start within one second of its commit")
	}
}

// TestRuntimeCatchesUpUnnotifiedAppendOnPeerNotice: an append that raised
// no notice in this process, as another daemon's commit on SingleStore, is
// found by the catch-up that the daemon-wide peer check's job notice wakes.
// The runtime keeps no peer timer of its own, so without that notice it
// never tails (feature-quiet-idle-runtime, Peers; req-quiet-idle-runtime
// AC-1.3, AC-2.5).
func TestRuntimeCatchesUpUnnotifiedAppendOnPeerNotice(t *testing.T) {
	raw := memlog.New()
	hub := changes.NewHub()
	counted := &tailCountingLog{Store: Notify(raw, hub)}
	ran := make(chan struct{}, 1)
	rt := NewRuntime(counted, Options{Workspaces: []string{"ws"}, Changes: hub})
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error {
		ran <- struct{}{}
		return nil
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for counted.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the runtime made no first catch-up")
		}
		time.Sleep(time.Millisecond)
	}
	tails := counted.count()
	if _, err := Enqueue(context.Background(), raw, "ws", "demo", "peer", demoArgs{}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
		t.Fatal("an unnotified append ran before any notice")
	case <-time.After(100 * time.Millisecond):
	}
	if got := counted.count(); got != tails {
		t.Fatalf("the runtime tailed %d times without a notice", got-tails)
	}
	// The peer check publishes one job notice for the workspace.
	hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Job})
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer notice did not catch up the unnotified append")
	}
}

// tailCountingLog counts Tail calls.
type tailCountingLog struct {
	eventlog.Store
	mu    sync.Mutex
	tails int
}

func (l *tailCountingLog) Tail(ctx context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	l.mu.Lock()
	l.tails++
	l.mu.Unlock()
	return l.Store.Tail(ctx, workspace, after, limit)
}

func (l *tailCountingLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tails
}

// TestNotifyRaisesJobNoticesAfterAppend: enqueue, failure, snooze, and
// rescue appends raise one job notice; a claim, completion, discard, other
// stream, suppressed enqueue, or version conflict raises none; and a batch
// in the context holds the notice until it commits.
func TestNotifyRaisesJobNoticesAfterAppend(t *testing.T) {
	hub := changes.NewHub()
	log := Notify(memlog.New(), hub)
	notices := hub.Subscribe("ws", changes.Job)
	defer notices.Close()
	got := func() bool {
		select {
		case <-notices.C():
			return true
		default:
			return false
		}
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{}, 3, now); err != nil || !inserted || !got() {
		t.Fatalf("enqueue inserted=%t err=%v, want a notice", inserted, err)
	}
	if inserted, err := Enqueue(ctx, log, "ws", "demo", "k", demoArgs{}, 3, now); err != nil || inserted || got() {
		t.Fatalf("suppressed enqueue inserted=%t err=%v raised a notice", inserted, err)
	}
	stream := StreamFor("demo", "k")
	for i, step := range []struct {
		kind  string
		raise bool
	}{{KindClaimed, false}, {KindFailed, true}, {KindClaimed, false}, {KindSnoozed, true}, {KindClaimed, false}, {KindRescued, true}, {KindClaimed, false}, {KindCompleted, false}} {
		appendKind(t, log, "ws", stream, eventlog.Version(i+1), step.kind, outcomePayload{Attempt: 1})
		if got() != step.raise {
			t.Fatalf("%s raised=%t, want %t", step.kind, !step.raise, step.raise)
		}
	}
	if _, err := log.Append(ctx, "ws", stream, 1, []eventlog.NewEvent{{Kind: KindEnqueued}}); !eventlog.IsVersionConflict(err) || got() {
		t.Fatalf("conflicting append err=%v raised a notice", err)
	}
	appendKind(t, log, "ws", "task/t1", 0, KindEnqueued, struct{}{})
	if got() {
		t.Fatal("a non-job stream raised a job notice")
	}
	batch := hub.Begin()
	if _, err := Enqueue(changes.WithBatch(ctx, batch), log, "ws", "demo", "batched", demoArgs{}, 3, now); err != nil {
		t.Fatal(err)
	}
	if got() {
		t.Fatal("a batched notice published before commit")
	}
	batch.Commit()
	if !got() {
		t.Fatal("committed batch did not publish the job notice")
	}
	if log.Changes() != hub {
		t.Fatal("Changes() does not expose the hub")
	}
	if rt := NewRuntime(log, Options{}); rt.hub != hub {
		t.Fatal("runtime did not take the log's hub")
	}
}

// TestEnqueueAndRuntimeActorsAreExplicit proves queue envelopes name their
// system actor explicitly: enqueue records the canonical queue subsystem
// built by core.SystemActor, and the runtime records its claim and outcome
// under its worker identity with the system role (component-persistence,
// Actor context). The claim is observed through a handler channel.
func TestEnqueueAndRuntimeActorsAreExplicit(t *testing.T) {
	f := newRuntimeFixture(t, Options{WorkerID: "replica-a"})
	claimed := make(chan queue.Job, 1)
	release := make(chan struct{})
	f.outcome = func(job queue.Job) error {
		claimed <- job
		<-release
		return nil
	}
	if _, err := Enqueue(context.Background(), f.log, "ws", "demo", "actor", demoArgs{Key: "actor"}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stream := StreamFor("demo", "actor")
	actorOf := func(kind string) (string, string, bool) {
		events, err := f.log.Read(context.Background(), "ws", stream, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind == kind {
				return event.ActorID, event.ActorRole, true
			}
		}
		return "", "", false
	}
	if id, role, ok := actorOf(KindEnqueued); !ok || id != core.SystemActor("queue").ID || role != string(core.ActorSystem) {
		t.Fatalf("enqueue actor=%q/%q ok=%t", id, role, ok)
	}
	f.start(t)
	<-claimed
	if id, role, ok := actorOf(KindClaimed); !ok || id != "replica-a" || role != string(core.ActorSystem) {
		t.Fatalf("claim actor=%q/%q ok=%t", id, role, ok)
	}
	close(release)
	f.waitFor(t, "completion", func() bool { return f.state(t, "actor").State == StateCompleted })
	if id, role, ok := actorOf(KindCompleted); !ok || id != "replica-a" || role != string(core.ActorSystem) {
		t.Fatalf("outcome actor=%q/%q ok=%t", id, role, ok)
	}
}
