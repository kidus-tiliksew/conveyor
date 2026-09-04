package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The queue port.
//
// Everything above the queue talks to these types. The queue owns the
// durable job streams, the worker loop, retries, stuck-job rescue, and the
// periodic clock; the dispatcher owns what a job means. The implementation
// is internal/queue/logqueue, on the event log.

// Job is one unit of work handed to a handler.
type Job struct {
	// ID is the driver's identity for the row, opaque to handlers.
	ID string
	// Kind matches the registration the driver routed the job to.
	Kind string
	// Attempt is 1 on the first execution; MaxAttempts bounds retries the
	// way the enqueuer configured them.
	Attempt     int
	MaxAttempts int
	// Args is the JSON the enqueuer stored; DecodeArgs recovers the type.
	Args json.RawMessage
}

// DecodeArgs recovers the typed arguments a job was enqueued with.
func DecodeArgs[T any](job Job) (T, error) {
	var args T
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return args, fmt.Errorf("queue: decode %s args: %w", job.Kind, err)
	}
	return args, nil
}

// Handler works one job. Returning nil completes it; returning a SnoozeError
// reschedules the same row after its duration without counting a failure;
// any other error records a failed attempt and the driver retries per the
// registration's delay until MaxAttempts.
type Handler func(ctx context.Context, job Job) error

// SnoozeError asks the driver to run the same job again later. It is an
// error only so a handler can express it through its single return value.
type SnoozeError struct{ Duration time.Duration }

func (e *SnoozeError) Error() string {
	return fmt.Sprintf("queue: snooze %s", e.Duration)
}

// Snooze builds a SnoozeError.
func Snooze(d time.Duration) error { return &SnoozeError{Duration: d} }

// Registration binds a kind to its handler and retry policy.
type Registration struct {
	Kind   string
	Handle Handler
	// RetryDelay, when set, decides how long after a failed attempt the next
	// one runs. Nil uses the driver's default backoff.
	RetryDelay func(attempt int) time.Duration
}

// Runtime is the worker side of a driver. Register every kind before Start.
type Runtime interface {
	Register(registration Registration)
	// EnsureWorkspace creates the workspace's queues and periodic clock,
	// idempotently. Startup calls it for every known workspace; a workspace
	// created later is added through the same call.
	EnsureWorkspace(workspace string) error
	Start(ctx context.Context) error
	// Stop drains: running jobs finish, no new ones start.
	Stop(ctx context.Context) error
	// StopAndCancel interrupts running jobs. The dispatcher's shutdown
	// marker distinguishes this from a stage deadline.
	StopAndCancel(ctx context.Context) error
}
