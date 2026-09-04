// Package riverqueue is the River driver behind the queue port. It is the
// only package outside the PostgreSQL store's River-specific file that
// imports River; the dispatcher and the daemon see queue.Runtime.
//
// Log-core migration plan, phase 3, task 3.1.
package riverqueue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
)

// Migrate applies River's bundled schema. The store calls it under its
// startup lock, after the control-plane migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create River migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate River schema: %w", err)
	}
	return nil
}

// activeStates are the states in which a duplicate insert is suppressed.
// Completed jobs are deliberately excluded so an intentional redispatch is
// not a silent no-op until job cleanup.
var activeStates = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
	rivertype.JobStateRetryable, rivertype.JobStateScheduled,
}

const publicationMaxAttempts = 5

// Inserter enqueues jobs inside a caller's transaction. It carries no
// workers; the store owns it so enqueues commit with the rows they follow.
type Inserter struct {
	client *river.Client[pgx.Tx]
}

func NewInserter(pool *pgxpool.Pool) (*Inserter, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create River insert client: %w", err)
	}
	return &Inserter{client: client}, nil
}

// InsertDispatchTx enqueues a task dispatch, unique per task while one is
// active or may retry. It reports whether a row was inserted.
func (i *Inserter) InsertDispatchTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) (bool, error) {
	result, err := i.client.InsertTx(ctx, tx, queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: taskID}, &river.InsertOpts{
		MaxAttempts: queueargs.DispatchTaskMaxAttempts,
		Queue:       queueargs.DispatchQueue(workspace),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: activeStates},
	})
	if err != nil {
		return false, err
	}
	return !result.UniqueSkippedAsDuplicate, nil
}

// InsertReviewPublicationTx enqueues a review publication, unique per review
// work order while active.
func (i *Inserter) InsertReviewPublicationTx(ctx context.Context, tx pgx.Tx, workspace, reviewWorkOrderID string) error {
	_, err := i.client.InsertTx(ctx, tx, queueargs.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: reviewWorkOrderID}, &river.InsertOpts{
		MaxAttempts: publicationMaxAttempts,
		Queue:       queueargs.ReviewPublicationQueue(workspace),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: activeStates},
	})
	return err
}

// InsertGitHubIssuePublicationTx enqueues an issue publication, unique per
// task while active.
func (i *Inserter) InsertGitHubIssuePublicationTx(ctx context.Context, tx pgx.Tx, workspace, taskID string) error {
	_, err := i.client.InsertTx(ctx, tx, queueargs.GitHubIssuePublicationArgs{WorkspaceID: workspace, TaskID: taskID}, &river.InsertOpts{
		MaxAttempts: publicationMaxAttempts,
		Queue:       queueargs.GitHubIssuePublicationQueue(workspace),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: activeStates},
	})
	return err
}

// Options configure the worker runtime.
type Options struct {
	// RescueStuckAfter bounds crash recovery above every stage that may run
	// inside the daemon; the dispatcher computes it from route timeouts.
	RescueStuckAfter time.Duration
	// Workspaces are registered before the client is built so their queues
	// and clocks exist from the first poll.
	Workspaces []string
}

// Runtime is a queue.Runtime on River.
type Runtime struct {
	client *river.Client[pgx.Tx]
	mu     sync.RWMutex
	regs   map[string]queueargs.Registration
}

var _ queueargs.Runtime = (*Runtime)(nil)

// NewRuntime builds the River client with one typed worker per kind the
// port knows. Handlers arrive through Register; a job whose kind has no
// registration fails loudly rather than silently completing.
func NewRuntime(pool *pgxpool.Pool, opts Options) (*Runtime, error) {
	rt := &Runtime{regs: map[string]queueargs.Registration{}}
	workers := river.NewWorkers()
	river.AddWorker(workers, newAdapter[queueargs.DispatchTaskArgs](rt))
	river.AddWorker(workers, newAdapter[queueargs.ReviewPublicationArgs](rt))
	river.AddWorker(workers, newAdapter[queueargs.GitHubIssuePublicationArgs](rt))
	river.AddWorker(workers, newAdapter[queueargs.OrderClockArgs](rt))
	queues := map[string]river.QueueConfig{queueargs.ControlQueue: {MaxWorkers: 1}}
	periodic := make([]*river.PeriodicJob, 0, len(opts.Workspaces))
	for _, workspace := range opts.Workspaces {
		for _, name := range workspaceQueues(workspace) {
			queues[name] = river.QueueConfig{MaxWorkers: 1}
		}
		periodic = append(periodic, orderClockPeriodicJob(workspace))
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		// Dispatcher stage contexts enforce the configured per-stage
		// wall-clock limits. River's one-minute default would cancel long
		// harness runs before those limits (DEC-1).
		JobTimeout:           -1,
		RescueStuckJobsAfter: opts.RescueStuckAfter,
		Queues:               queues,
		Workers:              workers,
		PeriodicJobs:         periodic,
	})
	if err != nil {
		return nil, err
	}
	rt.client = client
	return rt, nil
}

func workspaceQueues(workspace string) []string {
	return []string{
		queueargs.DispatchQueue(workspace),
		queueargs.ReviewPublicationQueue(workspace),
		queueargs.GitHubIssuePublicationQueue(workspace),
	}
}

func orderClockPeriodicJob(workspace string) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Second),
		func() (river.JobArgs, *river.InsertOpts) {
			return queueargs.OrderClockArgs{WorkspaceID: workspace}, &river.InsertOpts{Queue: queueargs.ControlQueue}
		},
		&river.PeriodicJobOpts{ID: queueargs.OrderClockPeriodicID(workspace), RunOnStart: true},
	)
}

func (rt *Runtime) Register(registration queueargs.Registration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.regs[registration.Kind] = registration
}

func (rt *Runtime) registration(kind string) (queueargs.Registration, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	reg, ok := rt.regs[kind]
	return reg, ok
}

// EnsureWorkspace adds the workspace's queues and periodic clock to a
// running client. Already-present queues are not an error.
func (rt *Runtime) EnsureWorkspace(workspace string) error {
	for _, name := range workspaceQueues(workspace) {
		if err := rt.client.Queues().Add(name, river.QueueConfig{MaxWorkers: 1}); err != nil && !errors.Is(err, &river.QueueAlreadyAddedError{}) {
			return err
		}
	}
	_, err := rt.client.PeriodicJobs().AddSafely(orderClockPeriodicJob(workspace))
	return err
}

func (rt *Runtime) Start(ctx context.Context) error         { return rt.client.Start(ctx) }
func (rt *Runtime) Stop(ctx context.Context) error          { return rt.client.Stop(ctx) }
func (rt *Runtime) StopAndCancel(ctx context.Context) error { return rt.client.StopAndCancel(ctx) }

// Client exposes the underlying River client for tests that observe job
// completion; production code has no use for it.
func (rt *Runtime) Client() *river.Client[pgx.Tx] { return rt.client }

// adapter is the typed River worker that forwards to a registration.
type adapter[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	rt *Runtime
}

func newAdapter[T river.JobArgs](rt *Runtime) *adapter[T] {
	return &adapter[T]{rt: rt}
}

// WorkerFor returns a River worker for one registration, for tests that
// drive a job through rivertest without a runtime.
func WorkerFor[T river.JobArgs](registration queueargs.Registration) river.Worker[T] {
	rt := &Runtime{regs: map[string]queueargs.Registration{registration.Kind: registration}}
	return newAdapter[T](rt)
}

func (a *adapter[T]) Work(ctx context.Context, job *river.Job[T]) error {
	reg, ok := a.rt.registration(job.Kind)
	if !ok {
		return fmt.Errorf("riverqueue: no handler registered for kind %q", job.Kind)
	}
	err := reg.Handle(ctx, queueargs.Job{
		ID: strconv.FormatInt(job.ID, 10), Kind: job.Kind,
		Attempt: job.Attempt, MaxAttempts: job.MaxAttempts, Args: job.EncodedArgs,
	})
	var snooze *queueargs.SnoozeError
	if errors.As(err, &snooze) {
		return river.JobSnooze(snooze.Duration)
	}
	return err
}

func (a *adapter[T]) NextRetry(job *river.Job[T]) time.Time {
	if reg, ok := a.rt.registration(job.Kind); ok && reg.RetryDelay != nil {
		return time.Now().UTC().Add(reg.RetryDelay(job.Attempt))
	}
	return time.Time{}
}
