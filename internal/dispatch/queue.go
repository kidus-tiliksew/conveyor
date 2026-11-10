package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
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

// Work is the order clock handler the deadline alarm runs at each fire. It
// runs seven passes for the workspace and logs each error and each non-zero
// count without skipping the later passes; every pass keeps its per-task lock
// and its idempotent request IDs (component-durable-queue, The deadline
// alarm; req-quiet-idle-runtime AC-2.4).
func (w *orderClockWorker) Work(ctx context.Context, job queue.Job) error {
	args, err := queue.DecodeArgs[queue.OrderClockArgs](job)
	if err != nil {
		return err
	}
	ctx = store.WithActor(ctx, store.SystemActor(fmt.Sprintf("queue:%s", job.ID)))
	ctx = store.WithWorkspace(ctx, args.WorkspaceID)
	st := w.dispatcher.Store
	var errs []error
	now := time.Now().UTC()
	if _, err = taskops.New(st).TickOrderClock(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("order clock: %w", err))
	}
	// Bounded automatic recovery AR-5 (feature-bounded-automatic-recovery).
	if reenqueued, reenqueueErr := store.ReconcileFailedPublications(ctx, st, now); reenqueueErr != nil {
		log.Printf("automatic publication recovery in workspace %s: %v", args.WorkspaceID, reenqueueErr)
		errs = append(errs, fmt.Errorf("automatic publication recovery: %w", reenqueueErr))
	} else if reenqueued != 0 {
		log.Printf("automatically re-enqueued %d failed publication(s) in workspace %s", reenqueued, args.WorkspaceID)
	}
	if recover := w.dispatcher.AutomaticReviewRecovery; recover != nil {
		if reviewActs, reviewErr := recover(ctx, now); reviewErr != nil {
			log.Printf("automatic review recovery in workspace %s: %v", args.WorkspaceID, reviewErr)
			errs = append(errs, fmt.Errorf("automatic review recovery: %w", reviewErr))
		} else if reviewActs != 0 {
			log.Printf("automatically recovered %d review round(s) in workspace %s", reviewActs, args.WorkspaceID)
		}
	}
	// A due check-status deadline reads that head once
	// (component-submission-merge invariant 61).
	if deadlineErr := w.dispatcher.ApplyDueCheckStatusDeadlines(ctx); deadlineErr != nil {
		log.Printf("check-status deadlines in workspace %s: %v", args.WorkspaceID, deadlineErr)
		errs = append(errs, fmt.Errorf("check-status deadlines: %w", deadlineErr))
	}
	// A failed verification delivery whose next_attempt_at passed is enqueued
	// again (component-verification-publication).
	if deliveries, ok := st.(store.VerificationDeliveryStore); ok {
		deliveryCtx := store.WithActor(ctx, store.SystemActor("verification-publication"))
		if deliveryErr := deliveries.ReconcileVerificationDeliveries(deliveryCtx); deliveryErr != nil {
			log.Printf("verification delivery recovery in workspace %s: %v", args.WorkspaceID, deliveryErr)
			errs = append(errs, fmt.Errorf("verification delivery recovery: %w", deliveryErr))
		}
	}
	if err := reconcileVerificationHousekeeping(ctx, st, args.WorkspaceID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileVerificationHousekeeping runs the order clock handler's verification
// passes, which the removed minute reconcile tick ran: claim-loss
// reconciliation, then expiry of at most VerificationChunkExpiryLimit staging
// upload chunks under VerificationChunkExpiryBudget. A claim error does not
// skip expiry. A full page publishes a deadline notice, so the next fire
// drains the next page (component-durable-queue, The deadline alarm;
// component-verification-evidence).
func reconcileVerificationHousekeeping(ctx context.Context, st store.Store, workspace string) error {
	verification, ok := st.(store.VerificationStore)
	if !ok {
		return nil
	}
	ctx = store.WithActor(ctx, store.SystemActor("verification-reconciler"))
	var errs []error
	if reconciled, err := verification.ReconcileVerificationClaims(ctx); err != nil {
		log.Printf("reconcile verification claims in workspace %s: %v", workspace, err)
		errs = append(errs, fmt.Errorf("verification claim reconciliation: %w", err))
	} else if reconciled != 0 {
		log.Printf("reconciled %d lost verification claim(s) in workspace %s", reconciled, workspace)
	}
	expiryCtx, cancel := context.WithTimeout(ctx, store.VerificationChunkExpiryBudget)
	defer cancel()
	expired, err := verification.ExpireVerificationChunks(expiryCtx, store.VerificationChunkExpiryLimit)
	if err != nil {
		log.Printf("expire verification upload chunks in workspace %s: %v", workspace, err)
		errs = append(errs, fmt.Errorf("verification chunk expiry: %w", err))
	}
	if expired != 0 {
		log.Printf("expired %d verification upload chunk(s) in workspace %s", expired, workspace)
	}
	if expired >= store.VerificationChunkExpiryLimit {
		if notifier, ok := st.(interface{ Changes() *changes.Hub }); ok {
			notifier.Changes().Publish(changes.Notice{Workspace: workspace, Kind: changes.Deadline, At: time.Now().UTC()})
		}
	}
	return errors.Join(errs...)
}

// StoreDeadlines binds the store's NextDeadline to each workspace as the
// deadline alarm's source, under the system actor deadline-alarm
// (component-runtime, Daemon startup order).
func StoreDeadlines(st store.DeadlineReader) logqueue.DeadlineSource {
	return func(ctx context.Context, workspace string, after time.Time) (time.Time, bool, error) {
		ctx = store.WithWorkspace(store.WithActor(ctx, store.SystemActor("deadline-alarm")), workspace)
		return st.NextDeadline(ctx, after)
	}
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
	publications := &verificationPublicationWorker{dispatcher: d}
	// Rescue runs a kind's Discarded handler after it discards a job whose
	// handler never recorded an outcome (component-durable-queue, Handlers).
	dispatchWorker := &dispatchTaskWorker{dispatcher: d, shutdown: shutdown}
	reviewWorker := &reviewPublicationWorker{dispatcher: d}
	issueWorker := &githubIssuePublicationWorker{dispatcher: d}
	policyWorker := &reviewPolicyStatusWorker{dispatcher: d}
	return []queue.Registration{
		{Kind: queue.VerificationPublicationArgs{}.Kind(), Handle: publications.Work},
		{Kind: "verification_publication", Handle: publications.Legacy},
		{
			Kind:      queue.DispatchTaskArgs{}.Kind(),
			Handle:    dispatchWorker.Work,
			Discarded: dispatchWorker.Discarded,
			// Bounded T12/T13 backoff between failed dispatch attempts
			// (component-durable-queue).
			RetryDelay: queue.DispatchTaskRetryDelay,
		},
		{Kind: queue.ReviewPublicationArgs{}.Kind(), Handle: reviewWorker.Work, Discarded: reviewWorker.Discarded},
		{Kind: queue.GitHubIssuePublicationArgs{}.Kind(), Handle: issueWorker.Work, Discarded: issueWorker.Discarded},
		{Kind: queue.ReviewPolicyStatusArgs{}.Kind(), Handle: policyWorker.Work, Discarded: policyWorker.Discarded},
		{Kind: queue.PullRequestCloseArgs{}.Kind(), Handle: (&pullRequestCloseWorker{dispatcher: d}).Work, RetryDelay: queue.DispatchTaskRetryDelay},
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
