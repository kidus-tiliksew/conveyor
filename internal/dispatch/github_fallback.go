package dispatch

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The timed GitHub fallback keeps GitHub observation for a workspace without
// webhook delivery (component-runtime, Timed GitHub fallback;
// req-quiet-idle-runtime AC-3.5; DEC-99(5)). Each workspace has one
// goroutine and one timer. The timer is armed only while a task waits on
// GitHub or the workspace's monitor is enabled, fires at most once a
// minute, and is never armed for a workspace that has accepted a webhook
// delivery.

// GitHubFallbackInterval is the shortest time between two fires.
const GitHubFallbackInterval = time.Minute

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
	// handles its start, a notice, or a fire, with whether its timer is
	// armed and when it is due. Tests wait on it instead of sleeping.
	Stepped func(workspace string, armed bool, due time.Time)

	mu      sync.Mutex
	workers map[string]*fallbackWorker
	ctx     context.Context
	running sync.WaitGroup
}

type fallbackWorker struct {
	delivered chan struct{}
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
// a webhook delivery and makes no timed GitHub call from now on.
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
	st := &fallbackState{}
	defer st.disarm()
	f.begin(ctx, workspace, st)
	f.stepped(workspace, st)
	for {
		select {
		case <-ctx.Done():
			return
		case <-worker.delivered:
			st.webhook = true
			st.disarm()
		case notice := <-tasks.C():
			f.onTask(ctx, workspace, st, notice)
		case <-settings.C():
			f.onSettings(ctx, workspace, st)
		case <-st.timer:
			f.fire(ctx, workspace, st)
		}
		f.stepped(workspace, st)
	}
}

// begin polls the monitor once at start, then arms the timer for a
// workspace without webhook delivery that has a waiting task or an enabled
// monitor. The start-up GitHub reads have just read every waiting task, so
// the first fire comes one minute later.
func (f *GitHubFallback) begin(ctx context.Context, workspace string, st *fallbackState) {
	now := f.now()
	st.webhook = f.hasWebhook(ctx, workspace)
	enabled, interval, err := f.monitor(ctx, workspace)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor_settings err=%v`, workspace, err)
	}
	if enabled {
		f.pollMonitor(ctx, workspace)
		st.lastMonitor = now
	}
	if st.webhook {
		return
	}
	st.monitoring, st.interval = enabled || err != nil, interval
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
	if st.webhook || st.waiting {
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
	if st.webhook {
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
// failure keeps the timer armed.
func (f *GitHubFallback) fire(ctx context.Context, workspace string, st *fallbackState) {
	now := f.now()
	st.armed, st.timer, st.lastFire = false, nil, now
	if st.stop != nil {
		st.stop()
		st.stop = nil
	}
	if f.hasWebhook(ctx, workspace) {
		// A delivery accepted here or on a peer daemon ends the fallback.
		st.webhook = true
		return
	}
	if st.waiting && !now.Before(st.lastRead.Add(GitHubFallbackInterval)) {
		waiting, err := f.readWaiting(ctx, workspace)
		st.lastRead = now
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=merge_readiness err=%v`, workspace, err)
			waiting = true
		}
		st.waiting = waiting
	}
	enabled, interval, err := f.monitor(ctx, workspace)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor_settings err=%v`, workspace, err)
	} else {
		st.monitoring, st.interval = enabled, interval
	}
	if st.monitoring && err == nil && !now.Before(st.lastMonitor.Add(monitorInterval(st.interval))) {
		f.pollMonitor(ctx, workspace)
		st.lastMonitor = now
	}
	f.schedule(st, now)
}

// schedule arms the timer for the earliest due read or poll, never earlier
// than one minute after the previous fire, or stops it when neither
// condition holds.
func (f *GitHubFallback) schedule(st *fallbackState, now time.Time) {
	if st.webhook || (!st.waiting && !st.monitoring) {
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
	if earliest := st.lastFire.Add(GitHubFallbackInterval); !st.lastFire.IsZero() && due.Before(earliest) {
		due = earliest
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
// tasks, unchanged, and reports whether any task waited. With no waiting
// task it makes no GitHub call.
func (f *GitHubFallback) readWaiting(ctx context.Context, workspace string) (bool, error) {
	waiting, err := f.anyWaiting(ctx)
	if err != nil || !waiting {
		return waiting, err
	}
	d := f.Dispatcher
	if _, err := d.ReconcileMergeReadiness(ctx); err != nil {
		return true, err
	}
	reader := &StartupChecks{Dispatcher: d, Logf: f.Logf}
	_, err = reader.readPullRequests(ctx)
	return true, err
}

// anyWaiting reports whether a task waits on GitHub: the union the start-up
// pull_requests pass reads.
func (f *GitHubFallback) anyWaiting(ctx context.Context) (bool, error) {
	candidates, err := startupPullRequestCandidates(ctx, f.Dispatcher.Store)
	return len(candidates) > 0, err
}

// taskWaits reports whether one task is in that union.
func (f *GitHubFallback) taskWaits(ctx context.Context, taskID string) (bool, error) {
	task, err := f.Dispatcher.Store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if slices.Contains(mergeReadinessTaskStates, task.State) {
		return true, nil
	}
	if task.State == core.TaskMerged || task.State == core.TaskClosed {
		return false, nil
	}
	recorded, err := f.Dispatcher.Store.ListMonitorPullRequestEventsForTasks(ctx, []string{taskID})
	return len(recorded[taskID]) > 0, err
}

func (f *GitHubFallback) hasWebhook(ctx context.Context, workspace string) bool {
	if f.Webhooks == nil {
		return false
	}
	status, err := f.Webhooks(ctx, workspace)
	if err != nil {
		if ctx.Err() == nil {
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=webhook_status err=%v`, workspace, err)
		}
		return false
	}
	return status.HasWebhookDelivery()
}

func (f *GitHubFallback) monitor(ctx context.Context, workspace string) (bool, time.Duration, error) {
	if f.Monitor == nil {
		return false, 0, nil
	}
	return f.Monitor(ctx, workspace)
}

func (f *GitHubFallback) pollMonitor(ctx context.Context, workspace string) {
	if f.PollMonitor == nil {
		return
	}
	if err := f.PollMonitor(ctx, workspace); err != nil && ctx.Err() == nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=monitor err=%v`, workspace, err)
	}
}

func (f *GitHubFallback) stepped(workspace string, st *fallbackState) {
	if f.Stepped != nil {
		f.Stepped(workspace, st.armed, st.due)
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
