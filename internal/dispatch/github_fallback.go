package dispatch

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The timed GitHub fallback keeps GitHub observation for a workspace without
// webhook delivery (component-runtime, Timed GitHub fallback;
// req-quiet-idle-runtime AC-3.5; DEC-99(5)). Each workspace has one
// goroutine and one timer. The timer is armed only while a task waits on
// GitHub or the workspace's monitor is enabled, and fires at most once a
// minute. For a workspace that has accepted a webhook delivery the timer is
// armed only for the silence fallback (github_fallback_silence.go;
// req-quiet-idle-runtime AC-3.8).

// GitHubFallbackInterval is the shortest time between two fires.
const GitHubFallbackInterval = time.Minute

// GitHubRateLimitPause is the pause after a rate-limited GitHub answer that
// names no reset time in Retry-After or X-RateLimit-Reset. A named reset time
// is honored as GitHub supplied it, however far away.
const GitHubRateLimitPause = 5 * time.Minute

// FallbackActor is the system actor the fallback reads and applies under.
const FallbackActor = "github-fallback"

// GitHubFallback runs the timed GitHub fallback.
type GitHubFallback struct {
	Dispatcher *Dispatcher
	// Hub delivers the task and workspace notices that arm the timer.
	Hub *changes.Hub
	// Workspaces lists the persisted workspaces.
	Workspaces func(context.Context) ([]string, error)
	// Webhooks reports whether a workspace has accepted a delivery.
	Webhooks func(ctx context.Context, workspace string) (core.GitHubWebhookStatus, error)
	// Monitor reports whether the workspace's monitor is enabled and its
	// poll interval.
	Monitor func(ctx context.Context, workspace string) (bool, time.Duration, error)
	// PollMonitor polls every monitored repository of the workspace once.
	PollMonitor func(ctx context.Context, workspace string) error
	// Now and Timer drive time; nil uses the wall clock. Tests inject both.
	Now   func() time.Time
	Timer func(time.Duration) (<-chan time.Time, func())
	// Logf writes error-level lines; nil uses log.Printf.
	Logf func(string, ...any)
	// Stepped, when set, is called after the goroutine of a workspace
	// handles an event (start, task, settings, fire, or delivered), with
	// whether its timer is armed and when it is due. Tests wait on it
	// instead of sleeping.
	Stepped func(workspace, event string, armed bool, due time.Time)

	mu      sync.Mutex
	workers map[string]*fallbackWorker
	ctx     context.Context
	running sync.WaitGroup
}

type fallbackWorker struct {
	delivered chan struct{}
	// silent mirrors fallbackState.silent for SilenceActive.
	silent atomic.Bool
}

// fallbackState is owned by one workspace goroutine.
type fallbackState struct {
	webhook     bool
	waiting     bool
	monitoring  bool
	interval    time.Duration
	lastRead    time.Time
	lastMonitor time.Time
	lastFire    time.Time
	armed       bool
	due         time.Time
	timer       <-chan time.Time
	stop        func()
	// pausedUntil is GitHub's reset time after a rate-limited read. The
	// timer is never armed before it, and the pause is held only here, in
	// memory, per workspace.
	pausedUntil time.Time

	// Silence fallback state (github_fallback_silence.go). lastDelivery is
	// the workspace's last accepted delivery; silent is set while timed
	// checks run for a webhook workspace; silenceDue is when the earliest
	// watched task that began waiting after lastDelivery reaches
	// GitHubSilenceWindow, and silenceSince is when that task began
	// waiting; since memoizes each watched task's stored waiting time.
	lastDelivery time.Time
	silent       bool
	silenceDue   time.Time
	silenceSince time.Time
	since        map[string]waitingSince
}

// Start runs one goroutine per workspace and adds a goroutine for each
// workspace a later workspace notice reveals. The daemon calls it after the
// start-up GitHub reads.
func (f *GitHubFallback) Start(ctx context.Context, workspaces []string) {
	f.mu.Lock()
	f.ctx = ctx
	if f.workers == nil {
		f.workers = map[string]*fallbackWorker{}
	}
	f.mu.Unlock()
	// Subscribe before the first list, so a workspace created in between
	// raises a notice the watcher reads.
	created := f.Hub.SubscribeAll(changes.Workspace)
	for _, workspace := range workspaces {
		f.ensure(workspace)
	}
	f.running.Add(1)
	go func() {
		defer f.running.Done()
		defer created.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-created.C():
			}
			if f.Workspaces == nil {
				continue
			}
			ids, err := f.Workspaces(ctx)
			if err != nil {
				if ctx.Err() == nil {
					f.logf(`level=error msg="timed GitHub check failed" workspace=* step=list_workspaces err=%v`, err)
				}
				continue
			}
			for _, id := range ids {
				f.ensure(id)
			}
		}
	}()
}

// Wait returns once every goroutine has ended with the service context.
func (f *GitHubFallback) Wait() { f.running.Wait() }

// WebhookDelivered stops the workspace's timer: the workspace has accepted
// a webhook delivery now, which also ends a running silence fallback.
func (f *GitHubFallback) WebhookDelivered(workspace string) {
	f.mu.Lock()
	worker := f.workers[workspace]
	f.mu.Unlock()
	if worker == nil {
		return
	}
	select {
	case worker.delivered <- struct{}{}:
	default:
	}
}

func (f *GitHubFallback) ensure(workspace string) {
	f.mu.Lock()
	if _, ok := f.workers[workspace]; ok || f.ctx == nil {
		f.mu.Unlock()
		return
	}
	worker := &fallbackWorker{delivered: make(chan struct{}, 1)}
	f.workers[workspace] = worker
	ctx := f.ctx
	f.mu.Unlock()
	// Subscribe before the first evaluation, so a notice raised while it
	// reads is delivered afterwards and nothing is lost.
	tasks := f.Hub.Subscribe(workspace, changes.Task)
	settings := f.Hub.Subscribe(workspace, changes.Workspace)
	f.running.Add(1)
	go func() {
		defer f.running.Done()
		defer tasks.Close()
		defer settings.Close()
		f.run(store.WithWorkspace(store.WithActor(ctx, store.SystemActor(FallbackActor)), workspace), workspace, worker, tasks, settings)
	}()
}

func (f *GitHubFallback) run(ctx context.Context, workspace string, worker *fallbackWorker, tasks, settings *changes.Subscription) {
	st := &fallbackState{since: map[string]waitingSince{}}
	defer st.disarm()
	f.begin(ctx, workspace, st)
	worker.silent.Store(st.silent)
	f.stepped(workspace, "start", st)
	for {
		var event string
		select {
		case <-ctx.Done():
			return
		case <-worker.delivered:
			event = "delivered"
			f.deliveryAccepted(workspace, st, f.now())
		case notice := <-tasks.C():
			event = "task"
			f.onTask(ctx, workspace, st, notice)
		case <-settings.C():
			event = "settings"
			f.onSettings(ctx, workspace, st)
		case <-st.timer:
			event = "fire"
			f.fire(ctx, workspace, st)
		}
		worker.silent.Store(st.silent)
		f.stepped(workspace, event, st)
	}
}

// begin polls the monitor once at start, then arms the timer for a
// workspace without webhook delivery that has a waiting task or an enabled
// monitor, and for a workspace with webhook delivery computes the silence
// from stored times. The start-up GitHub reads have just read every waiting
// task, so the first fire comes one minute later.
func (f *GitHubFallback) begin(ctx context.Context, workspace string, st *fallbackState) {
	now := f.now()
	if status, ok := f.webhookStatus(ctx, workspace); ok && status.HasWebhookDelivery() {
		st.webhook, st.lastDelivery = true, *status.LastDeliveryAt
	}
	enabled, interval, err := f.monitor(ctx, workspace)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor_settings err=%v`, workspace, err)
	}
	if enabled {
		f.pollMonitor(ctx, workspace, st, now)
		st.lastMonitor = now
	}
	st.monitoring, st.interval = enabled || err != nil, interval
	if st.webhook {
		st.lastRead, st.lastFire = now, now
		f.evaluateSilence(ctx, workspace, st, now, true)
		f.schedule(st, now)
		return
	}
	waiting, err := f.anyWaiting(ctx)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=waiting_tasks err=%v`, workspace, err)
		waiting = true
	}
	st.waiting = waiting
	st.lastRead, st.lastFire = now, now
	f.schedule(st, now)
}

// onTask arms the timer, or brings the next merge-readiness read forward,
// when the noticed task waits on GitHub. A notice without a task ID reads
// the whole union.
func (f *GitHubFallback) onTask(ctx context.Context, workspace string, st *fallbackState, notice changes.Notice) {
	if st.webhook && !st.silent {
		f.onSilenceTask(ctx, workspace, st, notice)
		return
	}
	if st.waiting {
		return
	}
	var waits bool
	var err error
	if notice.TaskID == "" {
		waits, err = f.anyWaiting(ctx)
	} else {
		waits, err = f.taskWaits(ctx, notice.TaskID)
	}
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=waiting_tasks err=%v`, workspace, err)
		waits = true
	}
	if !waits {
		return
	}
	now := f.now()
	if !st.armed {
		st.lastRead = now
	}
	st.waiting = true
	f.schedule(st, now)
}

// onSettings re-reads the monitor setting and the union after a workspace
// notice, raised on creation and on every configuration update.
func (f *GitHubFallback) onSettings(ctx context.Context, workspace string, st *fallbackState) {
	if st.webhook && !st.silent {
		return
	}
	now := f.now()
	wasArmed := st.armed
	enabled, interval, err := f.monitor(ctx, workspace)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor_settings err=%v`, workspace, err)
	} else {
		st.monitoring, st.interval = enabled, interval
	}
	if !st.waiting {
		waiting, waitErr := f.anyWaiting(ctx)
		if waitErr != nil {
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=waiting_tasks err=%v`, workspace, waitErr)
			waiting = true
		}
		if waiting && !wasArmed {
			st.lastRead = now
		}
		st.waiting = waiting
	}
	f.schedule(st, now)
}

// fire runs the due merge-readiness read and monitor poll, then re-arms for
// the next due one or stops when no task waits and the monitor is off. A
// failure keeps the timer armed. For a webhook workspace it first decides
// whether the silence fallback runs.
func (f *GitHubFallback) fire(ctx context.Context, workspace string, st *fallbackState) {
	now := f.now()
	st.armed, st.timer, st.lastFire = false, nil, now
	if st.stop != nil {
		st.stop()
		st.stop = nil
	}
	if status, ok := f.webhookStatus(ctx, workspace); ok && status.HasWebhookDelivery() && (!st.webhook || status.LastDeliveryAt.After(st.lastDelivery)) {
		// A delivery accepted here or on a peer daemon ends the fallback
		// and a running silence fallback.
		f.deliveryAccepted(workspace, st, *status.LastDeliveryAt)
	}
	if st.webhook && !st.silent {
		if !f.evaluateSilence(ctx, workspace, st, now, true) {
			f.schedule(st, now)
			return
		}
		// The silence fallback started: read GitHub in this fire.
		st.lastRead = now.Add(-GitHubFallbackInterval)
	}
	if st.silent && !f.anyWatched(ctx, workspace) {
		st.endSilence()
		f.schedule(st, now)
		return
	}
	if st.waiting && !now.Before(st.lastRead.Add(GitHubFallbackInterval)) && !st.paused(now) {
		waiting, err := f.readWaiting(ctx, workspace)
		st.lastRead = now
		if f.rateLimited(workspace, st, now, err) {
			// The fire's remaining GitHub reads would fail too; the tasks
			// still wait, and the next read follows GitHub's reset.
			waiting, err = true, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=merge_readiness err=%v`, workspace, err)
			waiting = true
		}
		st.waiting = waiting
		if st.silent && !waiting {
			st.endSilence()
			f.schedule(st, now)
			return
		}
	}
	enabled, interval, err := f.monitor(ctx, workspace)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor_settings err=%v`, workspace, err)
	} else {
		st.monitoring, st.interval = enabled, interval
	}
	if st.monitoring && err == nil && !now.Before(st.lastMonitor.Add(monitorInterval(st.interval))) && !st.paused(now) {
		f.pollMonitor(ctx, workspace, st, now)
		st.lastMonitor = now
	}
	f.schedule(st, now)
}

// rateLimited reports whether err is a rate-limited GitHub answer. It then
// pauses the workspace's timed reads until GitHub's reset time, or for
// GitHubRateLimitPause when the answer named none, and writes one error line
// naming the workspace and that reset time. No fire is armed before it, and
// arm keeps the one-minute spacing after the previous fire.
func (f *GitHubFallback) rateLimited(workspace string, st *fallbackState, now time.Time, err error) bool {
	retryAt, limited := github.RateLimitRetryAt(err)
	if !limited {
		return false
	}
	if retryAt.IsZero() {
		retryAt = now.Add(GitHubRateLimitPause)
	}
	st.pausedUntil = retryAt
	f.logf(`level=error msg="timed GitHub checks paused: GitHub rate limit exhausted" workspace=%s reset_at=%s err=%v`, workspace, retryAt.UTC().Format(time.RFC3339), err)
	return true
}

// paused reports whether a rate-limit pause holds at now.
func (st *fallbackState) paused(now time.Time) bool {
	return now.Before(st.pausedUntil)
}

// schedule arms the timer for the earliest due read or poll, never earlier
// than one minute after the previous fire, or stops it when neither
// condition holds.
func (f *GitHubFallback) schedule(st *fallbackState, now time.Time) {
	if st.webhook && !st.silent {
		if st.silenceDue.IsZero() {
			st.disarm()
			return
		}
		f.arm(st, now, st.silenceDue)
		return
	}
	if !st.waiting && !st.monitoring {
		st.disarm()
		return
	}
	var due time.Time
	if st.waiting {
		due = st.lastRead.Add(GitHubFallbackInterval)
	}
	if st.monitoring {
		next := st.lastMonitor.Add(monitorInterval(st.interval))
		if due.IsZero() || next.Before(due) {
			due = next
		}
	}
	f.arm(st, now, due)
}

// arm sets the timer for due, never earlier than one minute after the
// previous fire or before a rate-limit pause ends, and keeps a timer already
// armed for the same time.
func (f *GitHubFallback) arm(st *fallbackState, now, due time.Time) {
	if earliest := st.lastFire.Add(GitHubFallbackInterval); !st.lastFire.IsZero() && due.Before(earliest) {
		due = earliest
	}
	if due.Before(st.pausedUntil) {
		due = st.pausedUntil
	}
	if st.armed && st.due.Equal(due) {
		return
	}
	st.disarm()
	wait := due.Sub(now)
	if wait < 0 {
		wait = 0
	}
	st.timer, st.stop = f.timer(wait)
	st.armed, st.due = true, due
}

func (st *fallbackState) disarm() {
	if st.stop != nil {
		st.stop()
	}
	st.armed, st.timer, st.stop, st.due = false, nil, nil, time.Time{}
}

func monitorInterval(interval time.Duration) time.Duration {
	return max(interval, GitHubFallbackInterval)
}

// readWaiting runs the start-up GitHub reads for the workspace's waiting
// tasks and reports whether any task waited. With no waiting task it makes
// no GitHub call. The merge-readiness sweep and the pull-request read each
// stop at the first rate-limited answer and return it, and a rate-limited
// sweep skips the pull-request read.
func (f *GitHubFallback) readWaiting(ctx context.Context, workspace string) (bool, error) {
	waiting, err := f.anyWaiting(ctx)
	if err != nil || !waiting {
		return waiting, err
	}
	d := f.Dispatcher
	// The merge-readiness sweep stops at its first rate-limited read and
	// returns it, so no later task or pull-request read follows it.
	if _, err := d.reconcileMergeReadiness(ctx, true); err != nil {
		return true, err
	}
	reader := &StartupChecks{Dispatcher: d, Logf: f.Logf}
	_, err = reader.readPullRequests(ctx)
	return true, err
}

// anyWaiting reports whether a task waits on GitHub: the set the start-up
// pull_requests pass and each fire read (waitsOnGitHub).
func (f *GitHubFallback) anyWaiting(ctx context.Context) (bool, error) {
	candidates, err := startupPullRequestCandidates(ctx, f.Dispatcher.Store)
	return len(candidates) > 0, err
}

// taskWaits reports whether one task waits on GitHub, with the predicate
// anyWaiting and the per-fire read use: one GetTask, and one batched
// pull-request event read when the task fields alone do not decide.
func (f *GitHubFallback) taskWaits(ctx context.Context, taskID string) (bool, error) {
	task, err := f.Dispatcher.Store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if waitsOnGitHub(task, false) {
		return true, nil
	}
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
		return false, nil
	}
	recorded, err := f.Dispatcher.Store.ListMonitorPullRequestEventsForTasks(ctx, []string{taskID})
	return waitsOnGitHub(task, len(recorded[taskID]) > 0), err
}

// webhookStatus reads the workspace's webhook status; a failed read writes
// one error line and reports false.
func (f *GitHubFallback) webhookStatus(ctx context.Context, workspace string) (core.GitHubWebhookStatus, bool) {
	if f.Webhooks == nil {
		return core.GitHubWebhookStatus{}, false
	}
	status, err := f.Webhooks(ctx, workspace)
	if err != nil {
		if ctx.Err() == nil {
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=webhook_status err=%v`, workspace, err)
		}
		return core.GitHubWebhookStatus{}, false
	}
	return status, true
}

func (f *GitHubFallback) monitor(ctx context.Context, workspace string) (bool, time.Duration, error) {
	if f.Monitor == nil {
		return false, 0, nil
	}
	return f.Monitor(ctx, workspace)
}

func (f *GitHubFallback) pollMonitor(ctx context.Context, workspace string, st *fallbackState, now time.Time) {
	if f.PollMonitor == nil {
		return
	}
	err := f.PollMonitor(ctx, workspace)
	if err == nil || ctx.Err() != nil || f.rateLimited(workspace, st, now, err) {
		return
	}
	f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor err=%v`, workspace, err)
}

func (f *GitHubFallback) stepped(workspace, event string, st *fallbackState) {
	if f.Stepped != nil {
		f.Stepped(workspace, event, st.armed, st.due)
	}
}

func (f *GitHubFallback) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *GitHubFallback) timer(d time.Duration) (<-chan time.Time, func()) {
	if f.Timer != nil {
		return f.Timer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (f *GitHubFallback) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
