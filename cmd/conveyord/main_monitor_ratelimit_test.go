package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// monitorClock drives the fallback's timer; the test never waits real
// minutes.
type monitorClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*monitorTimer
}

type monitorTimer struct {
	at            time.Time
	c             chan time.Time
	stopped, done bool
}

func (c *monitorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *monitorClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &monitorTimer{at: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.c, func() {
		c.mu.Lock()
		t.stopped = true
		c.mu.Unlock()
	}
}

// Advance moves time and fires each live timer that falls due, reporting
// how many fired.
func (c *monitorClock) Advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := 0
	for _, t := range c.timers {
		if !t.stopped && !t.done && !t.at.After(c.now) {
			t.done = true
			t.c <- c.now
			fired++
		}
	}
	return fired
}

type monitorStep struct {
	event string
	armed bool
	due   time.Time
}

// TestTimedMonitorPollPausesAtRateLimit drives the timed GitHub fallback
// through the production monitorPoller adapter for a workspace without
// webhook delivery that monitors three repositories. A rate-limited GitHub
// answer ends the monitor sweep after that one read, with no retry, keeps
// its category and reset time, and writes one error line naming the
// workspace and reset; no read happens before the reset, and every
// repository is polled again at the reset (component-runtime, Timed GitHub
// fallback; req-quiet-idle-runtime AC-3.5).
func TestTimedMonitorPollPausesAtRateLimit(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{
		{Name: "api", GitHub: "acme/api", Base: "main"},
		{Name: "web", GitHub: "acme/web", Base: "main"},
		{Name: "docs", GitHub: "acme/docs", Base: "main"},
	}}
	cfg.Monitor.Enabled = true
	cfg.Monitor.Repositories = []string{"api", "web", "docs"}
	cfg.Monitor.PollInterval = time.Minute
	cfg.Monitor.StartupWindow = time.Hour
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()

	var limited atomic.Pointer[time.Time]
	var mu sync.Mutex
	var reads []string
	var sleeps atomic.Int32
	service := &monitor.Service{Store: st, WorkspaceID: "demo", Enabled: true, Repositories: map[string]struct{}{"api": {}, "web": {}, "docs": {}}, Intake: func(context.Context, monitor.TaskRequest) (monitor.IntakeResult, error) {
		t.Error("monitor filed a task")
		return monitor.IntakeResult{}, nil
	}}
	polls := &monitorPoller{Store: st, Dispatcher: d, Service: service,
		Config: func(context.Context) (*config.Config, error) { return cfg, nil },
		Runner: func(_ context.Context, _, slug string) (func(context.Context, ...string) ([]byte, error), error) {
			return func(_ context.Context, args ...string) ([]byte, error) {
				mu.Lock()
				reads = append(reads, slug)
				mu.Unlock()
				if reset := limited.Load(); reset != nil {
					return nil, githubtrigger.RateLimitedError(*reset, errors.New("GitHub REST status 403: API rate limit exceeded for installation ID 12."))
				}
				return quietMonitorRead(args...), nil
			}, nil
		},
		Sleep: func(context.Context, time.Duration) error {
			sleeps.Add(1)
			return nil
		},
	}
	readCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(reads)
	}

	start := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	clock := &monitorClock{now: start}
	steps := make(chan monitorStep, 64)
	var logMu sync.Mutex
	var lines []string
	fallback := newGitHubFallback(st, d, func(context.Context) ([]string, error) { return []string{"demo"}, nil }, polls, func(format string, args ...any) {
		logMu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	fallback.Now, fallback.Timer = clock.Now, clock.Timer
	fallback.Stepped = func(_, event string, armed bool, due time.Time) { steps <- monitorStep{event, armed, due} }
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		fallback.Wait()
	})
	await := func(event string) monitorStep {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case s := <-steps:
				if s.event == event {
					return s
				}
			case <-deadline:
				t.Fatalf("fallback did not handle a %s event", event)
				return monitorStep{}
			}
		}
	}
	advance := func(d time.Duration) (monitorStep, bool) {
		t.Helper()
		if clock.Advance(d) == 0 {
			return monitorStep{}, false
		}
		return await("fire"), true
	}
	errorLines := func() []string {
		logMu.Lock()
		defer logMu.Unlock()
		var out []string
		for _, line := range lines {
			if strings.Contains(line, "level=error") {
				out = append(out, line)
			}
		}
		return out
	}

	fallback.Start(runCtx, []string{"demo"})
	if s := await("start"); !s.armed || readCount() != 6 {
		t.Fatalf("start step=%+v reads=%d, want armed after one poll of each repository: a head read and a window listing", s, readCount())
	}

	reset := start.Add(21 * time.Minute)
	limited.Store(&reset)
	s, fired := advance(time.Minute)
	if !fired || !s.armed || !s.due.Equal(reset) {
		t.Fatalf("rate-limited fire=%+v fired=%t, want armed for the reset %s", s, fired, reset)
	}
	if got := readCount(); got != 7 || sleeps.Load() != 0 {
		t.Fatalf("reads=%d sleeps=%d, want one read and no retry after the rate limit", got, sleeps.Load())
	}
	if got := errorLines(); len(got) != 1 || !strings.Contains(got[0], `msg="timed GitHub checks paused: GitHub rate limit exhausted" workspace=demo reset_at=`+reset.Format(time.RFC3339)) {
		t.Fatalf("error lines=%v, want one pause line naming the workspace and reset", got)
	}
	if _, fired := advance(19 * time.Minute); fired || readCount() != 7 {
		t.Fatalf("fired=%t reads=%d before GitHub's reset", fired, readCount())
	}
	limited.Store(nil)
	if s, fired := advance(time.Minute); !fired || !s.armed || readCount() != 10 {
		t.Fatalf("fire at the reset=%+v fired=%t reads=%d, want every repository polled again from its tip", s, fired, readCount())
	}
	if got := errorLines(); len(got) != 1 {
		t.Fatalf("error lines after the reset=%v", got)
	}
}

// TestPushedRefPollKeepsRetriesOnRateLimit: a push delivery's poll keeps
// the bounded retries, so the delivery-driven path is not narrowed, and
// its final error keeps the rate-limit category and reset time.
func TestPushedRefPollKeepsRetriesOnRateLimit(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", GitHub: "acme/api", Base: "main"}}}
	cfg.Monitor.Enabled = true
	cfg.Monitor.Repositories = []string{"api"}
	cfg.Monitor.PollInterval = time.Minute
	cfg.Monitor.StartupWindow = time.Hour
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	var reads atomic.Int32
	service := &monitor.Service{Store: st, WorkspaceID: "demo", Enabled: true, Repositories: map[string]struct{}{"api": {}}}
	polls := &monitorPoller{Store: st, Dispatcher: dispatch.New(st, cfg, nil), Service: service,
		Config: func(context.Context) (*config.Config, error) { return cfg, nil },
		Runner: func(context.Context, string, string) (func(context.Context, ...string) ([]byte, error), error) {
			return func(context.Context, ...string) ([]byte, error) {
				reads.Add(1)
				return nil, githubtrigger.RateLimitedError(reset, errors.New("GitHub REST status 403: API rate limit exceeded"))
			}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	err := polls.PushedRef(ctx, "acme/api", "refs/heads/main")
	retryAt, isLimited := githubtrigger.RateLimitRetryAt(err)
	if reads.Load() != 3 || !isLimited || !retryAt.Equal(reset) {
		t.Fatalf("reads=%d limited=%t retry at %s err=%v, want three attempts and a typed rate limit", reads.Load(), isLimited, retryAt, err)
	}
}
