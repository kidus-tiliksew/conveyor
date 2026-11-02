package logqueue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// The deadline alarm (component-durable-queue, The deadline alarm;
// req-quiet-idle-runtime AC-2.4). Each test waits on fires, which the clock
// handler reports on a channel, never on a fixed sleep, and keeps its bound
// well inside the five-second budget.

// alarmBudget is the AC-2.4 bound: a due deadline applies within five
// seconds.
const alarmBudget = 5 * time.Second

// alarmFire is one fire as the order clock handler saw it. start is the
// clock reading the alarm took when the fire began, which AlarmSpacing
// governs (alarm.go, the fire closure in alarmLoop). entered is the handler's
// own reading on entry, which lags start by the fire's start-to-handler
// latency.
type alarmFire struct {
	start   time.Time
	entered time.Time
}

// fireClock is an Options.Now that remembers the last reading each goroutine
// took. A fire reads the clock once at its start and then calls the order
// clock handler on the same goroutine with no reading between: runClock only
// looks up the registration and encodes the job. The last reading on the
// handler's goroutine is therefore its fire's start. Worker goroutines read
// the clock concurrently, so readings are keyed by goroutine.
type fireClock struct {
	now  func() time.Time
	mu   sync.Mutex
	last map[uint64]time.Time
}

func newFireClock(now func() time.Time) *fireClock {
	if now == nil {
		now = time.Now
	}
	return &fireClock{now: now, last: map[uint64]time.Time{}}
}

func (c *fireClock) Now() time.Time {
	at := c.now()
	id := goroutineID()
	c.mu.Lock()
	c.last[id] = at
	c.mu.Unlock()
	return at
}

// fireStart returns and forgets the start of the fire that runs the calling
// order clock handler. It returns zero when the goroutine took no reading.
func (c *fireClock) fireStart() time.Time {
	id := goroutineID()
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.last[id]
	if !ok {
		return time.Time{}
	}
	delete(c.last, id)
	return at.UTC()
}

// goroutineID parses the calling goroutine's ID from its stack header,
// "goroutine N [running]:".
func goroutineID() uint64 {
	var buf [64]byte
	header := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	if end := bytes.IndexByte(header, ' '); end > 0 {
		header = header[:end]
	}
	id, err := strconv.ParseUint(string(header), 10, 64)
	if err != nil {
		panic("goroutine ID: " + err.Error())
	}
	return id
}

// manualClock moves only when the test sets it.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// spacingError checks the alarm's spacing rule where the alarm defines it:
// two fires start at least AlarmSpacing apart. A positive limit also bounds
// the gap from above. Handler-entry times are not compared. Their gap is
// AlarmSpacing plus the second fire's start-to-handler latency minus the
// first's, so it drops below AlarmSpacing whenever the first fire's latency
// is the larger.
func spacingError(first, second alarmFire, limit time.Duration) error {
	if first.start.IsZero() || second.start.IsZero() {
		return fmt.Errorf("fire start not recorded: first=%s second=%s", first.start, second.start)
	}
	gap := second.start.Sub(first.start)
	if gap < AlarmSpacing {
		return fmt.Errorf("fires started %s apart, want at least %s", gap, AlarmSpacing)
	}
	if limit > 0 && gap > limit {
		return fmt.Errorf("fires started %s apart, want at most %s", gap, limit)
	}
	return nil
}

// alarmFixture is a runtime whose order clock handler reports each fire.
type alarmFixture struct {
	log   *NotifyingLog
	hub   *changes.Hub
	rt    *Runtime
	clock *fireClock
	fires chan alarmFire
	mu    sync.Mutex
	// hold, when set, runs once in the next fire's handler before the
	// handler's entry reading.
	hold func()
	// block, when set, holds the next fire until it is closed.
	block chan struct{}
	reads atomic.Int32
}

// storedDeadline is a deadline source that behaves like a store: it
// returns the earliest of its deadlines after the bound.
type storedDeadline struct {
	mu    sync.Mutex
	times []time.Time
	err   error
}

func (s *storedDeadline) set(times ...time.Time) {
	s.mu.Lock()
	s.times = times
	s.mu.Unlock()
}

func (s *storedDeadline) source(f *alarmFixture) DeadlineSource {
	return func(_ context.Context, _ string, after time.Time) (time.Time, bool, error) {
		f.reads.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err != nil {
			err := s.err
			s.err = nil
			return time.Time{}, false, err
		}
		var earliest time.Time
		for _, at := range s.times {
			if at.After(after) && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
		return earliest, !earliest.IsZero(), nil
	}
}

func newAlarmFixture(t *testing.T, deadlines *storedDeadline, opts Options) *alarmFixture {
	t.Helper()
	f := &alarmFixture{fires: make(chan alarmFire, 64)}
	f.hub = changes.NewHub()
	f.log = Notify(notifyingLog().Store, f.hub)
	f.clock = newFireClock(opts.Now)
	opts.Now = f.clock.Now
	opts.Workspaces = []string{"ws"}
	if deadlines != nil {
		opts.Deadlines = deadlines.source(f)
	}
	f.rt = NewRuntime(f.log, opts)
	f.rt.Register(queue.Registration{Kind: queue.OrderClockArgs{}.Kind(), Handle: func(context.Context, queue.Job) error {
		start := f.clock.fireStart()
		f.mu.Lock()
		hold, block := f.hold, f.block
		f.hold, f.block = nil, nil
		f.mu.Unlock()
		if hold != nil {
			hold()
		}
		f.fires <- alarmFire{start: start, entered: f.clock.Now().UTC()}
		if block != nil {
			<-block
		}
		return nil
	}})
	return f
}

func (f *alarmFixture) start(t *testing.T) {
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

// fire waits for the next fire within within.
func (f *alarmFixture) fire(t *testing.T, what string, within time.Duration) alarmFire {
	t.Helper()
	select {
	case fired := <-f.fires:
		return fired
	case <-time.After(within):
		t.Fatalf("no fire for %s within %s", what, within)
	}
	return alarmFire{}
}

// idle waits until the alarm arms no timer, then proves no fire follows.
func (f *alarmFixture) idle(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(alarmBudget)
	for {
		if _, armed := f.rt.AlarmTarget("ws"); !armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the alarm stayed armed", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case fired := <-f.fires:
		t.Fatalf("%s: unexpected fire at %s", what, fired.entered)
	case <-time.After(200 * time.Millisecond):
	}
}

// within asserts a fire applied a deadline inside the budget, and inside 60%
// of it on this machine so a slower CI runner still passes.
func within(t *testing.T, what string, deadline, fired time.Time) {
	t.Helper()
	late := fired.Sub(deadline)
	if late < 0 {
		t.Fatalf("%s fired %s before its deadline", what, -late)
	}
	if late > alarmBudget*6/10 {
		t.Fatalf("%s fired %s after its deadline; budget %s", what, late, alarmBudget)
	}
}

func TestAlarmFiresAtStartAndAtStoredDeadline(t *testing.T) {
	deadlines := &storedDeadline{}
	f := newAlarmFixture(t, deadlines, Options{})
	due := time.Now().UTC().Add(1500 * time.Millisecond)
	deadlines.set(due)
	f.start(t)
	f.fire(t, "start", alarmBudget)
	within(t, "stored deadline", due, f.fire(t, "stored deadline", alarmBudget+time.Second).entered)
	// The fire at the deadline reads the next target: none.
	f.idle(t, "after the only deadline")
}

func TestAlarmWithNoDeadlineSendsNoStatement(t *testing.T) {
	deadlines := &storedDeadline{}
	f := newAlarmFixture(t, deadlines, Options{})
	f.start(t)
	f.fire(t, "start", alarmBudget)
	f.idle(t, "no deadline")
	reads := f.reads.Load()
	if reads != 1 {
		t.Fatalf("deadline reads=%d, want the one after the start fire", reads)
	}
	// Nothing reads the source while idle.
	select {
	case fired := <-f.fires:
		t.Fatalf("idle alarm fired at %s", fired.entered)
	case <-time.After(300 * time.Millisecond):
	}
	if got := f.reads.Load(); got != reads {
		t.Fatalf("idle alarm read deadlines %d times", got-reads)
	}
}

func TestAlarmEarlierNoticeMovesTargetForward(t *testing.T) {
	deadlines := &storedDeadline{}
	f := newAlarmFixture(t, deadlines, Options{})
	far := time.Now().UTC().Add(time.Hour)
	deadlines.set(far)
	f.start(t)
	f.fire(t, "start", alarmBudget)
	waitTarget(t, f.rt, far)
	// A deadline recorded while the alarm sleeps until a later one: the
	// store holds it and the write's notice carries it.
	due := time.Now().UTC().Add(1200 * time.Millisecond)
	deadlines.set(far, due)
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline, At: due})
	within(t, "earlier deadline", due, f.fire(t, "earlier deadline", alarmBudget+time.Second).entered)
	waitTarget(t, f.rt, far)
	// A later notice moves nothing.
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline, At: far.Add(time.Hour)})
	waitTarget(t, f.rt, far)
}

func TestAlarmTimelessOrPastNoticeFiresAtOnce(t *testing.T) {
	for name, at := range map[string]time.Time{"timeless": {}, "past": time.Now().UTC().Add(-time.Hour)} {
		t.Run(name, func(t *testing.T) {
			f := newAlarmFixture(t, &storedDeadline{}, Options{})
			f.start(t)
			first := f.fire(t, "start", alarmBudget)
			f.idle(t, "before the notice")
			sent := time.Now().UTC()
			f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline, At: at})
			fired := f.fire(t, name+" notice", alarmBudget)
			within(t, name+" notice", sent, fired.entered)
			if err := spacingError(first, fired, 0); err != nil {
				t.Fatal(err)
			}
			f.idle(t, "after the notice")
		})
	}
}

// TestAlarmServesNoticeRaisedDuringFire: a deadline committed while a fire
// runs, after the fire's read, is served when the fire returns; the
// subscription holds it.
func TestAlarmServesNoticeRaisedDuringFire(t *testing.T) {
	f := newAlarmFixture(t, &storedDeadline{}, Options{})
	release := make(chan struct{})
	f.block = release
	f.start(t)
	f.fire(t, "start", alarmBudget)
	// The first fire is still running its handler.
	due := time.Now().UTC().Add(-time.Millisecond)
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline, At: due})
	close(release)
	within(t, "notice raised during a fire", due, f.fire(t, "notice raised during a fire", alarmBudget).entered)
	f.idle(t, "after serving it")
}

// TestAlarmCoalescedNoticesFireOnce: a later timed notice and a timeless one
// coalesce on the hub into one fire-now notice, so neither wake is lost and
// the pair costs one fire.
func TestAlarmCoalescedNoticesFireOnce(t *testing.T) {
	f := newAlarmFixture(t, &storedDeadline{}, Options{})
	release := make(chan struct{})
	f.block = release
	f.start(t)
	f.fire(t, "start", alarmBudget)
	sent := time.Now().UTC()
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline, At: sent.Add(time.Hour)})
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline})
	close(release)
	within(t, "coalesced notices", sent, f.fire(t, "coalesced notices", alarmBudget).entered)
	f.idle(t, "after the coalesced fire")
}

func TestAlarmRetriesAfterAnError(t *testing.T) {
	previous := alarmRetryMin
	alarmRetryMin = 50 * time.Millisecond
	t.Cleanup(func() { alarmRetryMin = previous })
	deadlines := &storedDeadline{err: errors.New("database unavailable")}
	f := newAlarmFixture(t, deadlines, Options{})
	f.start(t)
	first := f.fire(t, "start", alarmBudget)
	// The failed read schedules a retry; the spacing still applies.
	retried := f.fire(t, "retry", alarmBudget)
	if err := spacingError(first, retried, alarmBudget*6/10); err != nil {
		t.Fatalf("retry after the failed fire: %v", err)
	}
	f.idle(t, "after a successful retry")
}

// TestAlarmSpacingToleratesHandlerEntryLatency drives the interleaving that
// made the handler-entry spacing assertion flake, on a manual clock with no
// sleep. The first fire's handler takes its entry reading 5ms after the fire
// started. A timeless notice then arrives exactly AlarmSpacing after that
// start, so the second fire starts at once and its handler adds no latency.
// The fire-start check passes; the handler-entry gap is 995ms, which the
// former assertion rejected.
func TestAlarmSpacingToleratesHandlerEntryLatency(t *testing.T) {
	const latency = 5 * time.Millisecond
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	manual := &manualClock{now: t0}
	f := newAlarmFixture(t, &storedDeadline{}, Options{Now: manual.Now})
	f.hold = func() { manual.set(t0.Add(latency)) }
	f.start(t)
	first := f.fire(t, "start", alarmBudget)
	// The first fire takes no clock reading after its handler's entry reading,
	// and with no deadline, retry, or job it arms no timer. The clock moves
	// only here, so the second fire's start reads exactly this time.
	manual.set(t0.Add(AlarmSpacing))
	f.hub.Publish(changes.Notice{Workspace: "ws", Kind: changes.Deadline})
	second := f.fire(t, "notice", alarmBudget)

	want := []alarmFire{{start: t0, entered: t0.Add(latency)}, {start: t0.Add(AlarmSpacing), entered: t0.Add(AlarmSpacing)}}
	for i, got := range []alarmFire{first, second} {
		if !got.start.Equal(want[i].start) || !got.entered.Equal(want[i].entered) {
			t.Fatalf("fire %d start=%s entered=%s, want start=%s entered=%s", i+1, got.start, got.entered, want[i].start, want[i].entered)
		}
	}
	if err := spacingError(first, second, alarmBudget*6/10); err != nil {
		t.Fatalf("fire-start spacing rejected handler-entry latency: %v", err)
	}
	// The former assertion compared handler-entry times.
	if gap := second.entered.Sub(first.entered); gap >= AlarmSpacing {
		t.Fatalf("handler entries %s apart; the interleaving must put them under %s", gap, AlarmSpacing)
	}
}

func TestAlarmWakesWorkersForAJobRetryWithoutWorkerTimer(t *testing.T) {
	f := newRuntimeFixture(t, Options{})
	var attempts atomic.Int32
	started := make(chan time.Time, 4)
	f.outcome = func(queue.Job) error {
		started <- time.Now().UTC()
		if attempts.Add(1) == 1 {
			return errors.New("first attempt fails")
		}
		return nil
	}
	f.rt.Register(queue.Registration{Kind: "demo", Handle: func(_ context.Context, job queue.Job) error {
		return f.outcome(job)
	}, RetryDelay: func(int) time.Duration { return 1500 * time.Millisecond }})
	f.start(t)
	if _, err := Enqueue(context.Background(), f.log, "ws", "demo", "retry", demoArgs{Key: "retry"}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(alarmBudget):
		t.Fatal("first attempt never ran")
	}
	var nextAt time.Time
	f.waitFor(t, "the failed attempt in the log", func() bool {
		job := f.state(t, "retry")
		nextAt = job.ScheduledAt
		return job.State == StateScheduled
	})
	select {
	case at := <-started:
		within(t, "job retry", nextAt, at)
	case <-time.After(alarmBudget + 2*time.Second):
		t.Fatal("job retry never ran")
	}
}

func TestAlarmRescuesAForeignClaimAtItsRescueTime(t *testing.T) {
	const threshold = 1200 * time.Millisecond
	f := newRuntimeFixture(t, Options{RescueStuckAfter: threshold})
	ctx := context.Background()
	if _, err := Enqueue(ctx, f.log, "ws", "demo", "foreign", demoArgs{Key: "foreign"}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Another process claims the job before this runtime starts, then dies.
	job := f.state(t, "foreign")
	claimedAt := time.Now().UTC()
	payload, _ := jsonMarshal(claimedPayload{Attempt: 1, Worker: "dead-replica", ClaimedAt: claimedAt})
	if _, err := f.log.Append(ctx, "ws", job.Stream, job.Head, []eventlog.NewEvent{{Kind: KindClaimed, ActorID: "dead-replica", ActorRole: "system", Payload: payload, At: claimedAt}}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	due := claimedAt.Add(threshold)
	f.waitFor(t, "rescue", func() bool { return f.state(t, "foreign").Attempt >= 1 && f.state(t, "foreign").State != StateRunning })
	if late := time.Since(due); late > alarmBudget*6/10 {
		t.Fatalf("rescue %s after the rescue time", late)
	}
}

// waitTarget waits until the alarm sleeps until want.
func waitTarget(t *testing.T, rt *Runtime, want time.Time) {
	t.Helper()
	deadline := time.Now().Add(alarmBudget)
	for time.Now().Before(deadline) {
		if got, armed := rt.AlarmTarget("ws"); armed && got.Equal(want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, armed := rt.AlarmTarget("ws")
	t.Fatalf("alarm target=%s armed=%t, want %s", got, armed, want)
}

// flakyLog fails the next append of a rescue outcome once, as a transient
// backend error would.
type flakyLog struct {
	*NotifyingLog
	failRescue atomic.Bool
	failed     atomic.Int32
}

func (l *flakyLog) Append(ctx context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	for _, event := range events {
		if event.Kind == KindRescued && l.failRescue.CompareAndSwap(true, false) {
			l.failed.Add(1)
			return 0, errors.New("backend unavailable")
		}
	}
	return l.NotifyingLog.Append(ctx, workspace, stream, expected, events)
}

// TestAlarmRetriesAFailedRescue: a rescue whose append fails once keeps a
// retry target, so after the backend recovers the alarm rescues the stuck
// claim again and the job runs, with no notice or other wake from outside.
func TestAlarmRetriesAFailedRescue(t *testing.T) {
	previous := alarmRetryMin
	alarmRetryMin = 50 * time.Millisecond
	t.Cleanup(func() { alarmRetryMin = previous })
	const threshold = 1200 * time.Millisecond
	log := &flakyLog{NotifyingLog: notifyingLog()}
	log.failRescue.Store(true)
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, RescueStuckAfter: threshold})
	ran := make(chan struct{}, 1)
	rt.Register(queue.Registration{Kind: "demo", Handle: func(context.Context, queue.Job) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}, RetryDelay: func(int) time.Duration { return 30 * time.Millisecond }})
	ctx := context.Background()
	if _, err := Enqueue(ctx, log, "ws", "demo", "stuck", demoArgs{Key: "stuck"}, 3, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Another process claimed the job and died.
	job, err := Load(ctx, log, "ws", StreamFor("demo", "stuck"))
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := time.Now().UTC()
	payload, _ := jsonMarshal(claimedPayload{Attempt: 1, Worker: "dead-replica", ClaimedAt: claimedAt})
	if _, err := log.Append(ctx, "ws", job.Stream, job.Head, []eventlog.NewEvent{{Kind: KindClaimed, ActorID: "dead-replica", ActorRole: "system", Payload: payload, At: claimedAt}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.StopAndCancel(stopCtx)
	})
	due := claimedAt.Add(threshold)
	select {
	case <-ran:
	case <-time.After(due.Sub(time.Now()) + alarmBudget):
		t.Fatalf("stuck job never ran after the failed rescue; injected failures=%d", log.failed.Load())
	}
	if got := log.failed.Load(); got != 1 {
		t.Fatalf("injected rescue failures=%d, want 1", got)
	}
	if late := time.Since(due); late > alarmBudget*6/10 {
		t.Fatalf("job ran %s after its rescue time", late)
	}
	// Once the job has completed, the alarm is idle again.
	deadline := time.Now().Add(alarmBudget)
	for {
		if _, armed := rt.AlarmTarget("ws"); !armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alarm stayed armed after the rescued job completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAlarmRetriesAFailedOrderClock: an order clock handler error keeps a
// retry target, so a deadline the failed fire left unapplied is reached
// again without a notice.
func TestAlarmRetriesAFailedOrderClock(t *testing.T) {
	previous := alarmRetryMin
	alarmRetryMin = 50 * time.Millisecond
	t.Cleanup(func() { alarmRetryMin = previous })
	log := notifyingLog()
	clock := newFireClock(nil)
	rt := NewRuntime(log, Options{Workspaces: []string{"ws"}, Now: clock.Now})
	fires := make(chan alarmFire, 8)
	var calls atomic.Int32
	rt.Register(queue.Registration{Kind: queue.OrderClockArgs{}.Kind(), Handle: func(context.Context, queue.Job) error {
		fires <- alarmFire{start: clock.fireStart(), entered: time.Now().UTC()}
		if calls.Add(1) == 1 {
			return errors.New("database unavailable")
		}
		return nil
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.StopAndCancel(stopCtx)
	})
	first := <-fires
	select {
	case retried := <-fires:
		if err := spacingError(first, retried, alarmBudget*6/10); err != nil {
			t.Fatalf("retry after the failed fire: %v", err)
		}
	case <-time.After(alarmBudget):
		t.Fatal("failed order clock fire was never retried")
	}
	deadline := time.Now().Add(alarmBudget)
	for {
		if _, armed := rt.AlarmTarget("ws"); !armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alarm stayed armed after a successful retry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
