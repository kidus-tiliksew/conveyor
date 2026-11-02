package dispatch

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
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// fakeClock drives the fallback's timers; no test waits real minutes.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	c       chan time.Time
	stopped bool
	fired   bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.c, func() {
		c.mu.Lock()
		t.stopped = true
		c.mu.Unlock()
	}
}

// Advance moves time and fires every live timer that falls due, reporting
// how many fired.
func (c *fakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			t.c <- c.now
			fired++
		}
	}
	return fired
}

type fallbackStep struct {
	event string
	armed bool
	due   time.Time
}

// failingTasksStore fails task listing while fail is set.
type failingTasksStore struct {
	store.Store
	fail atomic.Bool
}

func (s *failingTasksStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	if s.fail.Load() {
		return nil, errors.New("database unavailable")
	}
	return s.Store.ListTasksFiltered(ctx, filter)
}

type fallbackFixture struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	st       *failingTasksStore
	hub      *changes.Hub
	clock    *fakeClock
	fallback *GitHubFallback
	steps    chan fallbackStep
	reads    atomic.Int32
	polls    atomic.Int32
	webhook  atomic.Bool
	// delivery, when set, is the stored last accepted delivery time the
	// Webhooks read reports; it takes precedence over webhook.
	delivery atomic.Pointer[time.Time]
	monitor  atomic.Bool
	interval atomic.Int64
	// onMonitor runs inside the Monitor settings read, which a fire makes
	// after its merge-readiness read.
	onMonitor atomic.Pointer[func()]
	logMu     sync.Mutex
	lines     []string
	start     time.Time
}

func newFallbackFixture(t *testing.T) *fallbackFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test"))
	memory := store.NewMemory()
	hub := memory.(interface{ Changes() *changes.Hub }).Changes()
	f := &fallbackFixture{t: t, ctx: ctx, cancel: cancel, st: &failingTasksStore{Store: memory}, hub: hub, steps: make(chan fallbackStep, 64)}
	f.start = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f.clock = &fakeClock{now: f.start}
	d := New(f.st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	forbidMergeCalls(t, d)
	d.ViewPullRequest = func(_ context.Context, _, branch string) (githubtrigger.PullRequest, error) {
		f.reads.Add(1)
		return githubtrigger.PullRequest{}, fmt.Errorf("%w for branch %s", githubtrigger.ErrPullRequestNotFound, branch)
	}
	f.fallback = &GitHubFallback{
		Dispatcher: d, Hub: hub,
		Workspaces: func(context.Context) ([]string, error) { return []string{"test"}, nil },
		Webhooks: func(context.Context, string) (core.GitHubWebhookStatus, error) {
			if at := f.delivery.Load(); at != nil {
				delivered := *at
				return core.GitHubWebhookStatus{LastDeliveryAt: &delivered}, nil
			}
			if f.webhook.Load() {
				at := f.clock.Now()
				return core.GitHubWebhookStatus{LastDeliveryAt: &at}, nil
			}
			return core.GitHubWebhookStatus{}, nil
		},
		Monitor: func(context.Context, string) (bool, time.Duration, error) {
			if hook := f.onMonitor.Swap(nil); hook != nil {
				(*hook)()
			}
			return f.monitor.Load(), time.Duration(f.interval.Load()), nil
		},
		PollMonitor: func(context.Context, string) error {
			f.polls.Add(1)
			return nil
		},
		Now: f.clock.Now, Timer: f.clock.Timer,
		Logf: func(format string, args ...any) {
			f.logMu.Lock()
			f.lines = append(f.lines, fmt.Sprintf(format, args...))
			f.logMu.Unlock()
		},
		Stepped: func(_, event string, armed bool, due time.Time) { f.steps <- fallbackStep{event, armed, due} },
	}
	t.Cleanup(func() {
		cancel()
		f.fallback.Wait()
	})
	return f
}

func (f *fallbackFixture) run() fallbackStep {
	f.t.Helper()
	f.fallback.Start(f.ctx, []string{"test"})
	return f.await("start")
}

// await waits for the workspace goroutine to finish handling an event of
// the kind. One write can raise several task notices, so earlier steps of
// other kinds are skipped; none of them calls GitHub.
func (f *fallbackFixture) await(event string) fallbackStep {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-f.steps:
			if s.event == event {
				return s
			}
		case <-deadline:
			f.t.Fatalf("fallback did not handle a %s event", event)
			return fallbackStep{}
		}
	}
}

// awaitTask waits for a handled task notice that leaves the timer armed or
// stopped as wanted. A write such as a close raises several notices, and
// one handled after a fire may report an earlier state, so other task
// steps are skipped; the call fails if none matches within five seconds.
func (f *fallbackFixture) awaitTask(armed bool) fallbackStep {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-f.steps:
			if s.event == "task" && s.armed == armed {
				return s
			}
			if s.event == "fire" {
				f.t.Fatalf("unexpected fire %+v while waiting for a task notice", s)
			}
		case <-deadline:
			f.t.Fatalf("no task notice left the timer armed=%t", armed)
			return fallbackStep{}
		}
	}
}

// quiet asserts the goroutine handles no fire.
func (f *fallbackFixture) quiet() {
	f.t.Helper()
	for {
		select {
		case s := <-f.steps:
			if s.event == "fire" {
				f.t.Fatalf("unexpected fire %+v", s)
			}
		case <-time.After(20 * time.Millisecond):
			return
		}
	}
}

// advance moves the clock; when a timer falls due it waits for the fire.
func (f *fallbackFixture) advance(d time.Duration) (fallbackStep, bool) {
	f.t.Helper()
	if f.clock.Advance(d) == 0 {
		return fallbackStep{}, false
	}
	return f.await("fire"), true
}

func (f *fallbackFixture) createTask(id string, state core.TaskState) {
	f.t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: state, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		f.t.Fatal(err)
	}
	// Creation records a task event in production; raise its notice.
	if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: id, Kind: "task.note", Payload: core.JSONPayload(map[string]any{"note": "created"})}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fallbackFixture) closeTask(id string) {
	f.t.Helper()
	if _, err := taskops.New(f.st.Store).Perform(f.ctx, id, taskops.Command{Kind: core.TaskCancel}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fallbackFixture) errorLines(substr string) []string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	var out []string
	for _, line := range f.lines {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// TestGitHubFallbackWebhookWorkspaceMakesNoTimedCall: a workspace with
// webhook delivery arms no timer, so a waiting task and an enabled monitor
// cause no GitHub call across ten minutes beyond the one start-up monitor
// poll (req-quiet-idle-runtime AC-3.5; component-runtime, Timed GitHub
// fallback).
func TestGitHubFallbackWebhookWorkspaceMakesNoTimedCall(t *testing.T) {
	f := newFallbackFixture(t)
	f.webhook.Store(true)
	f.monitor.Store(true)
	f.createTask("waiting", core.TaskQueued)
	if s := f.run(); s.armed {
		t.Fatalf("webhook workspace armed: %+v", s)
	}
	f.createTask("another", core.TaskRunning)
	if s := f.awaitTask(false); s.armed {
		t.Fatalf("task notice armed a webhook workspace: %+v", s)
	}
	for range 10 {
		if _, fired := f.advance(time.Minute); fired {
			t.Fatal("a timer fired for a webhook workspace")
		}
	}
	if f.reads.Load() != 0 || f.polls.Load() != 1 {
		t.Fatalf("reads=%d polls=%d, want no timed call and one start-up poll", f.reads.Load(), f.polls.Load())
	}
}

// TestGitHubFallbackRunsOnlyWhileATaskWaits: without webhook delivery, an
// idle workspace arms nothing; the task notice that makes a task wait arms
// the timer and the first read comes one minute later; reads repeat at most
// once a minute; the timer stops once no task waits and restarts on a later
// notice.
func TestGitHubFallbackRunsOnlyWhileATaskWaits(t *testing.T) {
	f := newFallbackFixture(t)
	if s := f.run(); s.armed {
		t.Fatalf("idle workspace armed: %+v", s)
	}
	if _, fired := f.advance(10 * time.Minute); fired || f.reads.Load() != 0 {
		t.Fatalf("idle workspace fired or read: reads=%d", f.reads.Load())
	}
	notice := f.clock.Now()
	f.createTask("waiting", core.TaskQueued)
	s := f.awaitTask(true)
	if !s.armed || !s.due.Equal(notice.Add(time.Minute)) {
		t.Fatalf("notice step=%+v, want armed for %s", s, notice.Add(time.Minute))
	}
	if _, fired := f.advance(59 * time.Second); fired {
		t.Fatal("fired before one minute")
	}
	if s, fired := f.advance(time.Second); !fired || !s.armed || f.reads.Load() != 1 {
		t.Fatalf("first fire=%+v fired=%t reads=%d", s, fired, f.reads.Load())
	}
	if _, fired := f.advance(30 * time.Second); fired {
		t.Fatal("fired twice within a minute")
	}
	if _, fired := f.advance(30 * time.Second); !fired || f.reads.Load() != 2 {
		t.Fatalf("second fire reads=%d", f.reads.Load())
	}
	f.closeTask("waiting")
	if s := f.awaitTask(true); !s.armed {
		t.Fatalf("closing notice while armed=%+v", s)
	}
	if s, fired := f.advance(time.Minute); !fired || s.armed || f.reads.Load() != 2 {
		t.Fatalf("fire after the task closed=%+v fired=%t reads=%d, want stopped without a read", s, fired, f.reads.Load())
	}
	if _, fired := f.advance(10 * time.Minute); fired {
		t.Fatal("stopped timer fired")
	}
	f.createTask("again", core.TaskRunning)
	if s := f.awaitTask(true); !s.armed {
		t.Fatalf("restart step=%+v", s)
	}
	if _, fired := f.advance(time.Minute); !fired || f.reads.Load() != 3 {
		t.Fatalf("restarted timer reads=%d", f.reads.Load())
	}
}

// TestGitHubFallbackArmsForTaskWaitingAtStart: a task already waiting when
// the daemon starts arms the timer; the first timed read follows the
// start-up read by one minute.
func TestGitHubFallbackArmsForTaskWaitingAtStart(t *testing.T) {
	f := newFallbackFixture(t)
	f.createTask("parked-with-pr", core.TaskParked)
	openPullRequest(t, f.st, f.ctx, "parked-with-pr", 51, "parked-head")
	s := f.run()
	if !s.armed || !s.due.Equal(f.start.Add(time.Minute)) {
		t.Fatalf("start step=%+v", s)
	}
	if _, fired := f.advance(time.Minute); !fired || f.reads.Load() != 1 {
		t.Fatalf("reads=%d", f.reads.Load())
	}
}

// TestGitHubFallbackNoticeDuringFireRearms: a task that starts waiting while
// a fire runs, after the fire found no waiting task, re-arms the stopped
// timer from its buffered notice instead of being lost.
func TestGitHubFallbackNoticeDuringFireRearms(t *testing.T) {
	f := newFallbackFixture(t)
	f.createTask("first", core.TaskQueued)
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	f.closeTask("first")
	f.awaitTask(true)
	hook := func() { f.createTask("during-fire", core.TaskQueued) }
	f.onMonitor.Store(&hook)
	if s, fired := f.advance(time.Minute); !fired || s.armed {
		t.Fatalf("fire step=%+v fired=%t, want stopped", s, fired)
	}
	if s := f.awaitTask(true); !s.armed {
		t.Fatalf("buffered notice step=%+v, want re-armed", s)
	}
	if _, fired := f.advance(time.Minute); !fired || f.reads.Load() != 1 {
		t.Fatalf("re-armed timer reads=%d", f.reads.Load())
	}
}

// TestGitHubFallbackFailedReadKeepsTimer: a failed merge-readiness read
// writes one error line and keeps the timer armed; the next minute reads
// again.
func TestGitHubFallbackFailedReadKeepsTimer(t *testing.T) {
	f := newFallbackFixture(t)
	f.createTask("waiting", core.TaskQueued)
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	f.st.fail.Store(true)
	if s, fired := f.advance(time.Minute); !fired || !s.armed {
		t.Fatalf("failed fire=%+v fired=%t, want still armed", s, fired)
	}
	if lines := f.errorLines(`level=error msg="timed GitHub check failed" workspace=test step=merge_readiness`); len(lines) != 1 || !strings.Contains(lines[0], "database unavailable") {
		t.Fatalf("lines=%v", f.lines)
	}
	f.st.fail.Store(false)
	if _, fired := f.advance(time.Minute); !fired || f.reads.Load() != 1 {
		t.Fatalf("retry reads=%d", f.reads.Load())
	}
}

// TestGitHubFallbackMonitorCadence: an enabled monitor polls once at start
// and then at its interval, never more than once a minute; a configuration
// notice that enables the monitor arms a stopped timer.
func TestGitHubFallbackMonitorCadence(t *testing.T) {
	f := newFallbackFixture(t)
	f.monitor.Store(true)
	f.interval.Store(int64(5 * time.Minute))
	s := f.run()
	if !s.armed || !s.due.Equal(f.start.Add(5*time.Minute)) || f.polls.Load() != 1 {
		t.Fatalf("start step=%+v polls=%d", s, f.polls.Load())
	}
	if _, fired := f.advance(4 * time.Minute); fired {
		t.Fatal("polled before the interval")
	}
	if _, fired := f.advance(time.Minute); !fired || f.polls.Load() != 2 || f.reads.Load() != 0 {
		t.Fatalf("interval poll polls=%d reads=%d", f.polls.Load(), f.reads.Load())
	}
	f.interval.Store(int64(10 * time.Second))
	f.hub.Publish(changes.Notice{Workspace: "test", Kind: changes.Workspace})
	if s := f.await("settings"); !s.armed || !s.due.Equal(f.start.Add(6*time.Minute)) {
		t.Fatalf("short interval step=%+v, want one minute after the last poll", s)
	}
	if _, fired := f.advance(time.Minute); !fired || f.polls.Load() != 3 {
		t.Fatalf("clamped poll polls=%d", f.polls.Load())
	}
	f.monitor.Store(false)
	f.hub.Publish(changes.Notice{Workspace: "test", Kind: changes.Workspace})
	if s := f.await("settings"); s.armed {
		t.Fatalf("disabled monitor step=%+v", s)
	}
	f.monitor.Store(true)
	f.hub.Publish(changes.Notice{Workspace: "test", Kind: changes.Workspace})
	if s := f.await("settings"); !s.armed {
		t.Fatalf("enabling configuration notice step=%+v", s)
	}
	if _, fired := f.advance(time.Minute); !fired || f.polls.Load() != 4 {
		t.Fatalf("re-enabled poll polls=%d", f.polls.Load())
	}
}

// TestGitHubFallbackStopsOnWebhookDelivery: a delivery accepted on a peer
// stops the timer at the next fire with no GitHub call, and a local
// delivery stops it at once.
func TestGitHubFallbackStopsOnWebhookDelivery(t *testing.T) {
	f := newFallbackFixture(t)
	f.createTask("waiting", core.TaskQueued)
	if s := f.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	f.webhook.Store(true)
	if s, fired := f.advance(time.Minute); !fired || s.armed || f.reads.Load() != 0 {
		t.Fatalf("peer delivery fire=%+v reads=%d", s, f.reads.Load())
	}
	f.createTask("later", core.TaskQueued)
	if s := f.awaitTask(false); s.armed {
		t.Fatalf("notice after delivery armed: %+v", s)
	}

	g := newFallbackFixture(t)
	g.createTask("waiting", core.TaskQueued)
	if s := g.run(); !s.armed {
		t.Fatalf("start step=%+v", s)
	}
	g.fallback.WebhookDelivered("test")
	if s := g.await("delivered"); s.armed {
		t.Fatalf("local delivery step=%+v", s)
	}
	if _, fired := g.advance(time.Minute); fired || g.reads.Load() != 0 {
		t.Fatalf("timer fired after a local delivery: reads=%d", g.reads.Load())
	}
	g.quiet()
}
