package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// PeerTailInterval is how often a daemon that runs beside other daemons on a
// backend without change notification tails each workspace's log
// (DEC-99(3); req-quiet-idle-runtime AC-1.3, AC-2.5).
const PeerTailInterval = 30 * time.Second

// Runtime is queue.Runtime on the event log.
//
// One goroutine per workspace catches up on the log, folding job streams
// into an in-memory index, once at start and then only when a job change
// notice or a peer tail tick wakes it; one goroutine per (workspace, kind)
// drains that index one job at a time, so a workspace never runs two jobs of
// one kind at once (DEC-99(1); component-durable-queue). A
// claim is an expected-version append, so any number of replicas may run
// the same runtime against the same log. One deadline alarm per workspace
// runs the order clock at the earliest recorded deadline and writes nothing
// to the log: a tick is not a fact, and the tick handler is idempotent under
// concurrent replicas.
type Runtime struct {
	log     eventlog.Store
	opts    Options
	hub     *changes.Hub
	mu      sync.Mutex
	regs    map[string]queue.Registration
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
	// RescueStuckAfter is how long a claimed job may run before a catch-up
	// treats its worker as dead and reschedules or discards it.
	RescueStuckAfter time.Duration
	// Changes is the hub whose job notices wake each workspace's catch-up.
	// Nil selects the hub of a log that exposes Changes(), such as a
	// backend's Log(), and otherwise a private hub.
	Changes *changes.Hub
	// PeerTail, when positive, also catches up each workspace on that
	// period: PeerTailInterval for peers on a backend without change
	// notification. Zero runs no peer tail.
	PeerTail time.Duration
	// Deadlines is the deadline alarm's source of stored deadlines; nil
	// means the alarm serves only the index and notices.
	Deadlines DeadlineSource
	// WorkerID names this process in claims.
	WorkerID string
	// Now is the clock; tests replace it.
	Now func() time.Time
	// Logf receives operational messages; nil discards them.
	Logf func(string, ...any)
}

var _ queue.Runtime = (*Runtime)(nil)

type workspace struct {
	id       string
	position eventlog.Position
	jobs     map[eventlog.StreamID]*Job
	// running marks kinds whose worker holds a job, so the index and the
	// worker agree on "one at a time" even before the claim event is read
	// back from the log.
	running map[string]bool
	// wakes holds one channel per worker kind, so every worker learns of
	// every catch-up; a shared channel would let one kind take another's.
	wakes map[string]chan struct{}
	// fold asks the poll loop to catch up after this process recorded an
	// outcome, so its own completions and discards, which raise no notice,
	// reach the index.
	fold chan struct{}
	// alarmIndex tells the deadline alarm the index changed; alarmReread
	// asks it to read the stored deadline again after a local job outcome.
	alarmIndex  chan struct{}
	alarmReread chan struct{}
	alarmTarget time.Time
	stopped     bool
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
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.WorkerID == "" {
		opts.WorkerID = "conveyord"
	}
	hub := opts.Changes
	if hub == nil {
		if source, ok := log.(interface{ Changes() *changes.Hub }); ok {
			hub = source.Changes()
		}
	}
	if hub == nil {
		hub = changes.NewHub()
	}
	rt := &Runtime{log: log, opts: opts, hub: hub, regs: map[string]queue.Registration{}, spaces: map[string]*workspace{}}
	rt.clockKind = queue.OrderClockArgs{}.Kind()
	for _, id := range opts.Workspaces {
		rt.spaces[id] = newWorkspace(id)
	}
	return rt
}

func newWorkspace(id string) *workspace {
	return &workspace{id: id, jobs: map[eventlog.StreamID]*Job{}, running: map[string]bool{}, wakes: map[string]chan struct{}{}, fold: make(chan struct{}, 1),
		alarmIndex: make(chan struct{}, 1), alarmReread: make(chan struct{}, 1)}
}

func (rt *Runtime) Register(registration queue.Registration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.regs[registration.Kind] = registration
}

func (rt *Runtime) registration(kind string) (queue.Registration, bool) {
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
	rt.wg.Add(2 + len(kinds))
	go rt.pollLoop(ws)
	go rt.alarmLoop(ws)
	for _, kind := range kinds {
		go rt.workerLoop(ws, kind)
	}
}

// workerKinds are the registered kinds that run as queued jobs; the order
// clock runs on the deadline alarm.
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

// pollLoop catches up once and then waits: it catches up again only on a job notice for the workspace (or for
// every workspace, after a peer listener reconnects) and on a peer tail
// tick. While idle it sends no statement (req-quiet-idle-runtime AC-1.1,
// AC-2.1). Rescue runs after every catch-up.
func (rt *Runtime) pollLoop(ws *workspace) {
	defer rt.wg.Done()
	notices := rt.hub.Subscribe(ws.id, changes.Job)
	defer notices.Close()
	var peerTick <-chan time.Time
	if rt.opts.PeerTail > 0 {
		ticker := time.NewTicker(rt.opts.PeerTail)
		defer ticker.Stop()
		peerTick = ticker.C
	}
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
		case <-notices.C():
		case <-ws.fold:
		case <-peerTick:
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
				fresh := NewJob(ws.id, event.Stream)
				job = &fresh
				ws.jobs[event.Stream] = job
			}
			if err := job.Apply(event); err != nil {
				rt.mu.Unlock()
				return err
			}
		}
		rt.mu.Unlock()
		rt.wakeWorkers(ws)
		signal(ws.alarmIndex)
		if len(events) < 1000 {
			return nil
		}
	}
}

func (rt *Runtime) wakeWorkers(ws *workspace) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, wake := range ws.wakes {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// wakeChannel is the worker kind's wake channel in the workspace.
func (rt *Runtime) wakeChannel(ws *workspace, kind string) chan struct{} {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	wake, ok := ws.wakes[kind]
	if !ok {
		wake = make(chan struct{}, 1)
		ws.wakes[kind] = wake
	}
	return wake
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

// workerLoop drains one kind in one workspace, one job at a time. With
// nothing claimable it waits for a wake: a catch-up's, or the deadline
// alarm's when a scheduled job falls due. It sets no timer of its own.
func (rt *Runtime) workerLoop(ws *workspace, kind string) {
	defer rt.wg.Done()
	wake := rt.wakeChannel(ws, kind)
	for {
		job, ok := rt.next(ws, kind)
		if !ok {
			select {
			case <-rt.loopCtx.Done():
				return
			case <-wake:
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

	handlerErr := reg.Handle(rt.runCtx, queue.Job{
		WorkspaceID: ws.id, ID: job.ID(), Kind: kind, Attempt: attempt, MaxAttempts: job.MaxAttempts, Args: job.Args,
	})
	finished := rt.opts.Now().UTC()
	var snooze *queue.SnoozeError
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
	signal(ws.fold)
	signal(ws.alarmReread)
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

// Snapshot returns the runtime's view of a workspace's jobs, for tests.
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
