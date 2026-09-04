package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
)

// Runtime is queue.Runtime on the event log.
//
// One goroutine per workspace tails the log and folds job streams into an
// in-memory index; one goroutine per (workspace, kind) drains that index one
// job at a time, which is the same concurrency River gave each queue. A
// claim is an expected-version append, so any number of replicas may run
// the same runtime against the same log. The periodic order clock runs
// in-process on a ticker and writes nothing to the log: a tick is not a
// fact, and the tick handler is idempotent under concurrent replicas.
type Runtime struct {
	log     eventlog.Store
	opts    Options
	mu      sync.Mutex
	regs    map[string]queueargs.Registration
	spaces  map[string]*workspace
	started bool
	// runCtx is cancelled by StopAndCancel; loopCtx by Stop.
	loopCtx    context.Context
	cancelLoop context.CancelFunc
	runCtx     context.Context
	cancelRun  context.CancelFunc
	wg         sync.WaitGroup
	clockKind  string
}

// Options configure the runtime.
type Options struct {
	Workspaces []string
	// RescueStuckAfter is how long a claimed job may run before the poll
	// loop treats its worker as dead and reschedules or discards it.
	RescueStuckAfter time.Duration
	// PollInterval is how often each workspace tails the log. Default 1s.
	PollInterval time.Duration
	// ClockInterval is the order clock's period. Default 5s; zero disables.
	ClockInterval time.Duration
	// WorkerID names this process in claims.
	WorkerID string
	// Now is the clock; tests replace it.
	Now func() time.Time
	// Logf receives operational messages; nil discards them.
	Logf func(string, ...any)
}

var _ queueargs.Runtime = (*Runtime)(nil)

type workspace struct {
	id       string
	position eventlog.Position
	jobs     map[eventlog.StreamID]*Job
	// running marks kinds whose worker holds a job, so the index and the
	// worker agree on "one at a time" even before the claim event is read
	// back from the log.
	running map[string]bool
	wake    chan struct{}
	stopped bool
}

// Default retry backoff when a registration sets none: doubling from one
// second, capped at an hour.
func defaultRetryDelay(attempt int) time.Duration {
	delay := time.Second
	for i := 1; i < attempt && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func NewRuntime(log eventlog.Store, opts Options) *Runtime {
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.ClockInterval == 0 {
		opts.ClockInterval = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.WorkerID == "" {
		opts.WorkerID = "conveyord"
	}
	rt := &Runtime{log: log, opts: opts, regs: map[string]queueargs.Registration{}, spaces: map[string]*workspace{}}
	rt.clockKind = queueargs.OrderClockArgs{}.Kind()
	for _, id := range opts.Workspaces {
		rt.spaces[id] = newWorkspace(id)
	}
	return rt
}

func newWorkspace(id string) *workspace {
	return &workspace{id: id, jobs: map[eventlog.StreamID]*Job{}, running: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func (rt *Runtime) Register(registration queueargs.Registration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.regs[registration.Kind] = registration
}

func (rt *Runtime) registration(kind string) (queueargs.Registration, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	reg, ok := rt.regs[kind]
	return reg, ok
}

// EnsureWorkspace adds a workspace; if the runtime is running, its loops
// start immediately.
func (rt *Runtime) EnsureWorkspace(id string) error {
	rt.mu.Lock()
	if _, ok := rt.spaces[id]; ok {
		rt.mu.Unlock()
		return nil
	}
	ws := newWorkspace(id)
	rt.spaces[id] = ws
	started := rt.started
	rt.mu.Unlock()
	if started {
		rt.startWorkspace(ws)
	}
	return nil
}

func (rt *Runtime) Start(ctx context.Context) error {
	rt.mu.Lock()
	if rt.started {
		rt.mu.Unlock()
		return errors.New("logqueue: runtime already started")
	}
	rt.started = true
	rt.loopCtx, rt.cancelLoop = context.WithCancel(context.Background())
	rt.runCtx, rt.cancelRun = context.WithCancel(context.Background())
	spaces := make([]*workspace, 0, len(rt.spaces))
	for _, ws := range rt.spaces {
		spaces = append(spaces, ws)
	}
	rt.mu.Unlock()
	for _, ws := range spaces {
		rt.startWorkspace(ws)
	}
	return nil
}

func (rt *Runtime) startWorkspace(ws *workspace) {
	kinds := rt.workerKinds()
	rt.wg.Add(1 + len(kinds))
	go rt.pollLoop(ws)
	for _, kind := range kinds {
		go rt.workerLoop(ws, kind)
	}
	if rt.opts.ClockInterval > 0 {
		if _, ok := rt.registration(rt.clockKind); ok {
			rt.wg.Add(1)
			go rt.clockLoop(ws)
		}
	}
}

// workerKinds are the registered kinds that run as queued jobs; the clock
// runs on its own loop.
func (rt *Runtime) workerKinds() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	kinds := make([]string, 0, len(rt.regs))
	for kind := range rt.regs {
		if kind != rt.clockKind {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

// Stop drains: loops end, running handlers finish.
func (rt *Runtime) Stop(ctx context.Context) error {
	rt.mu.Lock()
	if !rt.started {
		rt.mu.Unlock()
		return nil
	}
	cancel := rt.cancelLoop
	rt.mu.Unlock()
	cancel()
	return rt.wait(ctx)
}

// StopAndCancel interrupts running handlers, then waits.
func (rt *Runtime) StopAndCancel(ctx context.Context) error {
	rt.mu.Lock()
	if !rt.started {
		rt.mu.Unlock()
		return nil
	}
	cancelLoop, cancelRun := rt.cancelLoop, rt.cancelRun
	rt.mu.Unlock()
	cancelLoop()
	cancelRun()
	return rt.wait(ctx)
}

func (rt *Runtime) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		rt.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pollLoop tails the workspace's log into the job index and rescues
// stuck jobs.
func (rt *Runtime) pollLoop(ws *workspace) {
	defer rt.wg.Done()
	ticker := time.NewTicker(rt.opts.PollInterval)
	defer ticker.Stop()
	for {
		if err := rt.catchUp(rt.loopCtx, ws); err != nil && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: workspace %s: tail: %v", ws.id, err)
		}
		if err := rt.rescue(rt.loopCtx, ws); err != nil && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: workspace %s: rescue: %v", ws.id, err)
		}
		select {
		case <-rt.loopCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// catchUp applies every job event after the workspace's position.
func (rt *Runtime) catchUp(ctx context.Context, ws *workspace) error {
	for {
		rt.mu.Lock()
		after := ws.position
		rt.mu.Unlock()
		events, err := rt.log.Tail(ctx, ws.id, after, 1000)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		rt.mu.Lock()
		for _, event := range events {
			ws.position = event.Position
			if event.Stream.Type() != StreamType {
				continue
			}
			job, ok := ws.jobs[event.Stream]
			if !ok {
				job = &Job{Workspace: ws.id, Stream: event.Stream}
				job.Kind, job.Key, _ = strings.Cut(event.Stream.EntityID(), ":")
				ws.jobs[event.Stream] = job
			}
			folded, err := Fold(ws.id, event.Stream, []eventlog.Event{event})
			if err != nil {
				rt.mu.Unlock()
				return err
			}
			applyEvent(job, event, folded)
		}
		rt.mu.Unlock()
		rt.wakeWorkers(ws)
		if len(events) < 1000 {
			return nil
		}
	}
}

// applyEvent folds one event into an already-folded job. Fold is written
// over a whole stream, so this re-derives the single-event effect on top
// of the job's current state.
func applyEvent(job *Job, event eventlog.Event, single Job) {
	job.Head = event.Version
	switch event.Kind {
	case KindEnqueued:
		job.Kind, job.Key, job.Args = single.Kind, single.Key, single.Args
		job.MaxAttempts = single.MaxAttempts
		job.Attempt = 0
		job.State = single.State
		job.ScheduledAt = single.ScheduledAt
		job.ClaimedAt, job.ClaimedBy, job.LastError = time.Time{}, "", ""
		job.EnqueuedAt = event.Position
		job.Generation++
	case KindClaimed:
		job.State = StateRunning
		job.Attempt = single.Attempt
		job.ClaimedAt, job.ClaimedBy = single.ClaimedAt, single.ClaimedBy
	case KindCompleted:
		job.State = StateCompleted
		job.ClaimedBy = ""
	case KindFailed, KindRescued:
		job.LastError = single.LastError
		job.State = StateScheduled
		job.ScheduledAt = single.ScheduledAt
		job.ClaimedBy = ""
	case KindSnoozed:
		if job.Attempt > 0 {
			job.Attempt--
		}
		job.State = StateScheduled
		job.ScheduledAt = single.ScheduledAt
		job.ClaimedBy = ""
	case KindDiscarded:
		if single.LastError != "" {
			job.LastError = single.LastError
		}
		job.State = StateDiscarded
		job.ClaimedBy = ""
	}
}

func (rt *Runtime) wakeWorkers(ws *workspace) {
	select {
	case ws.wake <- struct{}{}:
	default:
	}
}

// rescue reschedules or discards jobs whose claim is older than the
// threshold: their worker is presumed dead. The attempt counts, as River's
// rescuer counts it.
func (rt *Runtime) rescue(ctx context.Context, ws *workspace) error {
	if rt.opts.RescueStuckAfter <= 0 {
		return nil
	}
	now := rt.opts.Now().UTC()
	rt.mu.Lock()
	var stuck []Job
	for _, job := range ws.jobs {
		if job.State == StateRunning && !job.ClaimedAt.IsZero() && now.Sub(job.ClaimedAt) > rt.opts.RescueStuckAfter && !ws.running[job.Kind+"|"+string(job.Stream)] {
			stuck = append(stuck, *job)
		}
	}
	rt.mu.Unlock()
	for _, job := range stuck {
		payload := outcomePayload{Attempt: job.Attempt, Error: "rescued: claim exceeded " + rt.opts.RescueStuckAfter.String()}
		kind := KindRescued
		if job.Attempt >= job.MaxAttempts {
			kind = KindDiscarded
		} else {
			payload.NextAt = now.Add(rt.retryDelay(job.Kind, job.Attempt))
		}
		if err := rt.appendOutcome(ctx, job, kind, payload, now); err != nil && !eventlog.IsVersionConflict(err) {
			return err
		}
		rt.opts.Logf("logqueue: rescued %s attempt %d/%d as %s", job.ID(), job.Attempt, job.MaxAttempts, kind)
	}
	return nil
}

func (rt *Runtime) retryDelay(kind string, attempt int) time.Duration {
	if reg, ok := rt.registration(kind); ok && reg.RetryDelay != nil {
		return reg.RetryDelay(attempt)
	}
	return defaultRetryDelay(attempt)
}

func (rt *Runtime) appendOutcome(ctx context.Context, job Job, kind string, payload outcomePayload, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = rt.log.Append(ctx, job.Workspace, job.Stream, job.Head, []eventlog.NewEvent{{
		Kind: kind, ActorID: rt.opts.WorkerID, ActorRole: "system", Payload: encoded, At: now,
	}})
	return err
}

// workerLoop drains one kind in one workspace, one job at a time.
func (rt *Runtime) workerLoop(ws *workspace, kind string) {
	defer rt.wg.Done()
	for {
		job, ok := rt.next(ws, kind)
		if !ok {
			select {
			case <-rt.loopCtx.Done():
				return
			case <-ws.wake:
			case <-time.After(rt.opts.PollInterval):
			}
			continue
		}
		rt.run(ws, kind, job)
		select {
		case <-rt.loopCtx.Done():
			return
		default:
		}
	}
}

// next picks the earliest claimable job of kind, marking the kind busy.
func (rt *Runtime) next(ws *workspace, kind string) (Job, bool) {
	now := rt.opts.Now().UTC()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if ws.running[kind] {
		return Job{}, false
	}
	var best *Job
	for _, job := range ws.jobs {
		if job.Kind != kind || !job.Claimable(now) {
			continue
		}
		if best == nil || job.EnqueuedAt < best.EnqueuedAt {
			best = job
		}
	}
	if best == nil {
		return Job{}, false
	}
	ws.running[kind] = true
	ws.running[kind+"|"+string(best.Stream)] = true
	return *best, true
}

func (rt *Runtime) release(ws *workspace, kind string, stream eventlog.StreamID) {
	rt.mu.Lock()
	delete(ws.running, kind)
	delete(ws.running, kind+"|"+string(stream))
	rt.mu.Unlock()
}

// run claims a job, works it, and records the outcome. A lost claim race
// is silent: the other replica owns the job now.
func (rt *Runtime) run(ws *workspace, kind string, job Job) {
	defer rt.release(ws, kind, job.Stream)
	reg, ok := rt.registration(kind)
	if !ok {
		return
	}
	now := rt.opts.Now().UTC()
	attempt := job.Attempt + 1
	claim, err := json.Marshal(claimedPayload{Attempt: attempt, Worker: rt.opts.WorkerID, ClaimedAt: now})
	if err != nil {
		rt.opts.Logf("logqueue: encode claim: %v", err)
		return
	}
	head, err := rt.log.Append(rt.loopCtx, job.Workspace, job.Stream, job.Head, []eventlog.NewEvent{{
		Kind: KindClaimed, ActorID: rt.opts.WorkerID, ActorRole: "system", Payload: claim, At: now,
	}})
	if err != nil {
		if !eventlog.IsVersionConflict(err) && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: claim %s: %v", job.ID(), err)
		}
		rt.refresh(ws, job.Stream)
		return
	}
	rt.mu.Lock()
	if current, ok := ws.jobs[job.Stream]; ok {
		current.State, current.Attempt, current.ClaimedAt, current.ClaimedBy, current.Head = StateRunning, attempt, now, rt.opts.WorkerID, head
	}
	rt.mu.Unlock()
	job.Head = head
	job.Attempt = attempt

	handlerErr := reg.Handle(rt.runCtx, queueargs.Job{
		ID: job.ID(), Kind: kind, Attempt: attempt, MaxAttempts: job.MaxAttempts, Args: job.Args,
	})
	finished := rt.opts.Now().UTC()
	var snooze *queueargs.SnoozeError
	outcome := outcomePayload{Attempt: attempt}
	var outcomeKind string
	switch {
	case handlerErr == nil:
		outcomeKind = KindCompleted
	case errors.As(handlerErr, &snooze):
		outcomeKind = KindSnoozed
		outcome.Until = finished.Add(snooze.Duration)
	case attempt >= job.MaxAttempts:
		outcomeKind = KindDiscarded
		outcome.Error = handlerErr.Error()
	default:
		outcomeKind = KindFailed
		outcome.Error = handlerErr.Error()
		outcome.NextAt = finished.Add(rt.retryDelay(kind, attempt))
	}
	// The outcome is recorded even during cancellation, so a snooze from a
	// shutdown-interrupted handler lands before the process exits. The index
	// learns the outcome from the tail, never from a local apply, so every
	// event folds exactly once; until then the job reads as running, which
	// keeps the worker from re-claiming it.
	if err := rt.appendOutcome(context.Background(), job, outcomeKind, outcome, finished); err != nil {
		rt.opts.Logf("logqueue: record %s for %s: %v", outcomeKind, job.ID(), err)
	}
}

// refresh replaces a job's index entry with a full fold of its stream,
// used after a lost claim race so a partial history never stands in for
// the truth.
func (rt *Runtime) refresh(ws *workspace, stream eventlog.StreamID) {
	job, err := Load(context.Background(), rt.log, ws.id, stream)
	if err != nil {
		rt.opts.Logf("logqueue: refresh %s: %v", stream, err)
		return
	}
	rt.mu.Lock()
	if job.Head > 0 {
		ws.jobs[stream] = &job
	} else {
		delete(ws.jobs, stream)
	}
	rt.mu.Unlock()
}

// clockLoop invokes the order clock handler on a ticker, once at start.
func (rt *Runtime) clockLoop(ws *workspace) {
	defer rt.wg.Done()
	reg, ok := rt.registration(rt.clockKind)
	if !ok {
		return
	}
	args, _ := json.Marshal(queueargs.OrderClockArgs{WorkspaceID: ws.id})
	tick := 0
	fire := func() {
		tick++
		job := queueargs.Job{ID: fmt.Sprintf("clock/%s@%d", ws.id, tick), Kind: rt.clockKind, Attempt: 1, MaxAttempts: 1, Args: args}
		if err := reg.Handle(rt.runCtx, job); err != nil && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: order clock %s: %v", ws.id, err)
		}
	}
	fire()
	ticker := time.NewTicker(rt.opts.ClockInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rt.loopCtx.Done():
			return
		case <-ticker.C:
			fire()
		}
	}
}

// Snapshot returns the runtime's view of a workspace's jobs, for tests and
// the shadow comparison.
func (rt *Runtime) Snapshot(workspaceID string) []Job {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ws, ok := rt.spaces[workspaceID]
	if !ok {
		return nil
	}
	out := make([]Job, 0, len(ws.jobs))
	for _, job := range ws.jobs {
		out = append(out, *job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out
}
