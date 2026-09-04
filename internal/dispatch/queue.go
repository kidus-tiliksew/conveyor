package dispatch

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// WorkspaceQueueRegistrar converges the workspaces the queue runtime serves
// with the workspaces persisted in the store.
type WorkspaceQueueRegistrar struct {
	mu    sync.Mutex
	known map[string]struct{}
	add   func(string) error
	logf  func(string, ...any)
}

func NewWorkspaceQueueRegistrar(known []string, add func(string) error, logf func(string, ...any)) *WorkspaceQueueRegistrar {
	registered := make(map[string]struct{}, len(known))
	for _, workspace := range known {
		registered[workspace] = struct{}{}
	}
	return &WorkspaceQueueRegistrar{known: registered, add: add, logf: logf}
}

func (r *WorkspaceQueueRegistrar) Ensure(workspace string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.known[workspace]; ok {
		return false, nil
	}
	if err := r.add(workspace); err != nil {
		return false, err
	}
	r.known[workspace] = struct{}{}
	r.logf("registered queue scheduling for workspace %s", workspace)
	return true, nil
}

func (r *WorkspaceQueueRegistrar) Converge(workspaces []core.Workspace) error {
	for _, workspace := range workspaces {
		if _, err := r.Ensure(workspace.ID); err != nil {
			return fmt.Errorf("register queue scheduling for workspace %s: %w", workspace.ID, err)
		}
	}
	return nil
}

type dispatchTaskWorker struct {
	dispatcher *Dispatcher
	shutdown   *ShutdownMarker
}

const (
	QueueRescueSafetyMargin = 5 * time.Minute
	shutdownRetryDelay      = time.Second
)

// ShutdownMarker distinguishes daemon interruption from a stage-owned
// deadline. The queue supplies cancellation for both cases, so the worker
// also requires this process-scoped marker before it preserves the attempt.
type ShutdownMarker struct{ stopping atomic.Bool }

func (m *ShutdownMarker) Mark() {
	if m != nil {
		m.stopping.Store(true)
	}
}

func (m *ShutdownMarker) Stopping() bool { return m != nil && m.stopping.Load() }

// MarkedRuntime marks hard shutdown before the queue cancels active work.
type MarkedRuntime struct {
	runtime  queue.Runtime
	shutdown *ShutdownMarker
}

func NewMarkedRuntime(runtime queue.Runtime, shutdown *ShutdownMarker) *MarkedRuntime {
	return &MarkedRuntime{runtime: runtime, shutdown: shutdown}
}

func (c *MarkedRuntime) Stop(ctx context.Context) error { return c.runtime.Stop(ctx) }

func (c *MarkedRuntime) StopAndCancel(ctx context.Context) error {
	c.shutdown.Mark()
	return c.runtime.StopAndCancel(ctx)
}

type orderClockWorker struct {
	dispatcher *Dispatcher
}

func (w *orderClockWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.OrderClockArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("queue:%s", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	_, err = taskops.New(w.dispatcher.Store).TickOrderClock(ctx, time.Now().UTC())
	return err
}

type reviewPublicationWorker struct {
	dispatcher *Dispatcher
}

type githubIssuePublicationWorker struct {
	dispatcher *Dispatcher
}

// Registrations binds every job kind to its handler and retry policy. The
// daemon hands them to whichever queue.Runtime is in use.
func (d *Dispatcher) Registrations(shutdown *ShutdownMarker) []queue.Registration {
	return []queue.Registration{
		{
			Kind:   queue.DispatchTaskArgs{}.Kind(),
			Handle: (&dispatchTaskWorker{dispatcher: d, shutdown: shutdown}).Work,
			// Bounded T12/T13 backoff between failed dispatch attempts
			// (design-task-lifecycle).
			RetryDelay: queue.DispatchTaskRetryDelay,
		},
		{Kind: queue.ReviewPublicationArgs{}.Kind(), Handle: (&reviewPublicationWorker{dispatcher: d}).Work},
		{Kind: queue.GitHubIssuePublicationArgs{}.Kind(), Handle: (&githubIssuePublicationWorker{dispatcher: d}).Work},
		{Kind: queue.OrderClockArgs{}.Kind(), Handle: (&orderClockWorker{dispatcher: d}).Work},
	}
}

// QueueRescueThreshold bounds crash recovery above every stage that may
// run inside conveyord. The default timeout remains part of the calculation
// when a route is absent or has not yet been normalized.
func QueueRescueThreshold(workspaceConfigs map[string]*config.Config) (time.Duration, error) {
	workspaces := make([]string, 0, len(workspaceConfigs))
	for workspace := range workspaceConfigs {
		workspaces = append(workspaces, workspace)
	}
	sort.Strings(workspaces)
	maxTimeout := time.Duration(0)
	routes := make([]routeTimeout, 0, len(workspaces)*2)
	for _, workspace := range workspaces {
		cfg := workspaceConfigs[workspace]
		if cfg == nil {
			return 0, fmt.Errorf("queue rescue threshold: workspace %s has no effective configuration", workspace)
		}
		for _, route := range inProcessRouteTimeouts(workspace, cfg) {
			routes = append(routes, route)
			if route.timeout > maxTimeout {
				maxTimeout = route.timeout
			}
		}
	}
	if maxTimeout == 0 {
		maxTimeout = config.DefaultStageTimeout
	}
	if maxTimeout > time.Duration(1<<63-1)-QueueRescueSafetyMargin {
		return 0, fmt.Errorf("queue rescue threshold overflows maximum duration for route timeout %s", maxTimeout)
	}
	threshold := maxTimeout + QueueRescueSafetyMargin
	for _, route := range routes {
		if route.timeout <= 0 || route.timeout >= threshold {
			return 0, fmt.Errorf("route %s timeout %s must be strictly below rescue threshold %s", route.name, route.timeout, threshold)
		}
	}
	return threshold, nil
}

type routeTimeout struct {
	name    string
	timeout time.Duration
}

func inProcessRouteTimeouts(workspace string, cfg *config.Config) []routeTimeout {
	routes := make([]routeTimeout, 0, 2)
	for _, stage := range []string{"triage", "spec"} {
		timeout := config.DefaultStageTimeout
		if route, ok := cfg.Routing.Stages[stage]; ok && route.Timeout > 0 {
			timeout = route.Timeout
		}
		routes = append(routes, routeTimeout{name: workspace + "/" + stage, timeout: timeout})
	}
	return routes
}

// ValidateQueueRescueThreshold prevents a queue added after startup from
// introducing an in-process route that can be rescued while still live.
func ValidateQueueRescueThreshold(workspace string, cfg *config.Config, threshold time.Duration) error {
	if cfg == nil {
		return fmt.Errorf("queue rescue threshold: workspace %s has no effective configuration", workspace)
	}
	for _, route := range inProcessRouteTimeouts(workspace, cfg) {
		if route.timeout <= 0 || route.timeout >= threshold {
			return fmt.Errorf("route %s timeout %s must be strictly below rescue threshold %s", route.name, route.timeout, threshold)
		}
	}
	return nil
}
