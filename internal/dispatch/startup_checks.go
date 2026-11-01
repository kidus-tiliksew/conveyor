package dispatch

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Start-up checks replace the minute reconcile tick: each repair pass runs
// once per workspace at daemon start, every repair is recorded as one
// drift.repaired event and one error-level log line, and nothing repeats the
// passes on a timer (component-runtime, Start-up checks and background loops;
// req-quiet-idle-runtime REQ-4, AC-4.1 to AC-4.3, AC-3.4; DEC-99(6)).

// StartupMergeReadinessPass names the start-up GitHub read.
const StartupMergeReadinessPass = "merge_readiness"

const (
	startupRetryInitial = 5 * time.Second
	startupRetryMaximum = time.Minute
)

// StartupCheckStore is the store surface the drift-repair passes use.
type StartupCheckStore interface {
	store.Store
	ReconcileQueuedTasks(context.Context) (int, error)
	ReconcileBlueprintClosures(context.Context) (int, error)
}

// StartupChecks runs the daemon's start-up checks.
type StartupChecks struct {
	Dispatcher *Dispatcher
	// Store runs the drift-repair passes and records their events.
	Store StartupCheckStore
	// Logf writes the error-level lines; nil uses log.Printf.
	Logf func(string, ...any)
	// RetryInitial and RetryMaximum bound the delay before a failed pass
	// runs again. Zero values use five seconds and one minute.
	RetryInitial, RetryMaximum time.Duration
	// Sleep waits before a retry and returns ctx's error when it ends
	// first; nil waits on a timer.
	Sleep func(context.Context, time.Duration) error

	retries sync.WaitGroup
}

type startupPass struct {
	name string
	run  func(context.Context) (int, error)
}

func (c *StartupChecks) driftPasses() []startupPass {
	st := c.Store
	return []startupPass{
		{store.DriftPassGitHubLifecycleIntents, c.Dispatcher.ReconcileGitHubLifecycles},
		{store.DriftPassGitHubIssuePublications, st.ReconcileGitHubLifecycles},
		{store.DriftPassQueuedTasks, st.ReconcileQueuedTasks},
		{store.DriftPassBlueprintClosures, st.ReconcileBlueprintClosures},
		{store.DriftPassReviewPublications, st.ReconcileReviewPublications},
		{store.DriftPassVerificationDeliveries, func(ctx context.Context) (int, error) {
			deliveries, ok := st.(store.VerificationDeliveryStore)
			if !ok {
				return 0, nil
			}
			return 0, deliveries.ReconcileVerificationDeliveries(store.WithActor(ctx, store.SystemActor("verification-publication")))
		}},
	}
}

// RunDrift runs every drift-repair pass once for each workspace, in order.
// The daemon calls it before the queue runtime starts, so no alarm fire or
// worker on this daemon repairs drift before a pass can report it. A failed
// pass never skips a later pass or workspace; it is retried in the
// background until it completes or ctx, the service context, ends.
func (c *StartupChecks) RunDrift(ctx context.Context, workspaces []string) {
	for _, workspace := range workspaces {
		for _, pass := range c.driftPasses() {
			c.run(ctx, workspace, pass)
		}
	}
}

// RunGitHub reads GitHub once for every task that waits on it in each
// workspace: ReconcileMergeReadiness reads each task in a state
// mergeReadinessTaskStates selects (req-quiet-idle-runtime AC-3.4). The
// daemon calls it once the server is wired and before it serves requests.
func (c *StartupChecks) RunGitHub(ctx context.Context, workspaces []string) {
	for _, workspace := range workspaces {
		c.run(ctx, workspace, startupPass{StartupMergeReadinessPass, c.Dispatcher.ReconcileMergeReadiness})
	}
}

// Wait returns once every background retry has ended.
func (c *StartupChecks) Wait() { c.retries.Wait() }

func (c *StartupChecks) run(ctx context.Context, workspace string, pass startupPass) {
	delay := c.retryInitial()
	if c.attempt(ctx, workspace, pass, 1, delay) == nil || ctx.Err() != nil {
		return
	}
	c.retries.Add(1)
	go func() {
		defer c.retries.Done()
		for attempt := 2; ; attempt++ {
			if err := c.sleep(ctx, delay); err != nil {
				return
			}
			delay = min(2*delay, c.retryMaximum())
			if c.attempt(ctx, workspace, pass, attempt, delay) == nil || ctx.Err() != nil {
				return
			}
		}
	}()
}

// attempt runs one pass for one workspace and records each repair it
// reported, including repairs reported before a failure.
func (c *StartupChecks) attempt(ctx context.Context, workspace string, pass startupPass, attempt int, retryIn time.Duration) error {
	var mu sync.Mutex
	var repairs []store.DriftRepair
	passCtx := store.WithWorkspace(store.WithActor(ctx, store.SystemActor("startup-check")), workspace)
	recordCtx := passCtx
	passCtx = store.WithDriftRecorder(passCtx, func(repair store.DriftRepair) {
		mu.Lock()
		repairs = append(repairs, repair)
		mu.Unlock()
	})
	_, err := pass.run(passCtx)
	mu.Lock()
	reported := repairs
	mu.Unlock()
	for _, repair := range reported {
		c.record(recordCtx, workspace, repair)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return err
		}
		c.logf(`level=error msg="start-up check failed" workspace=%s pass=%s attempt=%d retry_in=%s err=%v`, workspace, pass.name, attempt, retryIn, err)
	}
	return err
}

// record appends one drift.repaired event and writes one error-level line.
// A failed append writes a second error line, so the repair is never left
// unreported (req-quiet-idle-runtime AC-4.1, AC-4.2).
func (c *StartupChecks) record(ctx context.Context, workspace string, repair store.DriftRepair) {
	c.logf(`level=error msg="start-up check repaired drift" workspace=%s task=%s pass=%s mismatch=%q repair=%q`, workspace, repair.TaskID, repair.Pass, repair.Mismatch, repair.Repair)
	event := core.Event{TaskID: repair.TaskID, Kind: store.DriftRepairedEvent, Payload: store.DriftRepairedPayload(repair)}
	events := store.Store(c.Store)
	if c.Store == nil {
		events = c.Dispatcher.Store
	}
	if err := events.AppendEvent(ctx, event); err != nil {
		c.logf(`level=error msg="start-up check could not record drift.repaired" workspace=%s task=%s pass=%s err=%v`, workspace, repair.TaskID, repair.Pass, err)
	}
}

func (c *StartupChecks) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (c *StartupChecks) retryInitial() time.Duration {
	if c.RetryInitial > 0 {
		return c.RetryInitial
	}
	return startupRetryInitial
}

func (c *StartupChecks) retryMaximum() time.Duration {
	if c.RetryMaximum > 0 {
		return c.RetryMaximum
	}
	return startupRetryMaximum
}

func (c *StartupChecks) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
