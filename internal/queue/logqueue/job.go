// Package logqueue is the durable queue on the event log.
//
// Every job is one stream, job/<kind>:<key>, so uniqueness per key is the
// stream itself rather than a lookup. Enqueue appends job.enqueued unless
// the stream's fold says a job is still active; a claim is an append of
// job.claimed that names the version the worker observed, so two replicas
// racing for the same job have exactly one win. Retry, snooze, rescue, and
// exhaustion are events on the same stream. Nothing here is engine-specific:
// the package needs only eventlog.Store.
package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// The job fold lives in jobindex so the log drivers can maintain the job
// index with the same transition table (component-durable-queue, Job
// streams and their event kinds). These aliases keep logqueue's names.

// Event kinds on a job stream.
const (
	KindEnqueued  = jobindex.KindEnqueued
	KindClaimed   = jobindex.KindClaimed
	KindCompleted = jobindex.KindCompleted
	KindFailed    = jobindex.KindFailed
	KindSnoozed   = jobindex.KindSnoozed
	KindRescued   = jobindex.KindRescued
	KindDiscarded = jobindex.KindDiscarded
)

// StreamType is the stream type every job lives under.
const StreamType = jobindex.StreamType

// StreamFor is the stream that owns the job for one kind and unique key.
func StreamFor(kind, key string) eventlog.StreamID { return jobindex.StreamFor(kind, key) }

// State is where a job stands after folding its stream.
type State = jobindex.State

const (
	StateNone      = jobindex.StateNone
	StateAvailable = jobindex.StateAvailable
	StateScheduled = jobindex.StateScheduled
	StateRunning   = jobindex.StateRunning
	StateCompleted = jobindex.StateCompleted
	StateDiscarded = jobindex.StateDiscarded
)

// Job is the folded state of one job stream.
type Job = jobindex.Job

type (
	enqueuedPayload = jobindex.EnqueuedPayload
	claimedPayload  = jobindex.ClaimedPayload
	outcomePayload  = jobindex.OutcomePayload
)

// Fold derives a job from its stream's events, in version order.
func Fold(workspace string, stream eventlog.StreamID, events []eventlog.Event) (Job, error) {
	return jobindex.Fold(workspace, stream, events)
}

// NewJob is the empty job for a stream, before any event.
func NewJob(workspace string, stream eventlog.StreamID) Job {
	return jobindex.NewJob(workspace, stream)
}

// Load reads and folds one job stream.
func Load(ctx context.Context, log eventlog.Store, workspace string, stream eventlog.StreamID) (Job, error) {
	events, err := log.Read(ctx, workspace, stream, 0, 0)
	if err != nil {
		return Job{}, err
	}
	return Fold(workspace, stream, events)
}

// Enqueue appends a new job unless one is still active on the stream. It
// reports whether it enqueued. A version conflict means another writer
// enqueued first, which counts as "not inserted", the same answer a unique
// index would give.
func Enqueue(ctx context.Context, log eventlog.Store, workspace, kind, key string, args any, maxAttempts int, now time.Time) (bool, error) {
	if maxAttempts <= 0 {
		return false, errors.New("logqueue: max attempts must be positive")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return false, fmt.Errorf("logqueue: encode %s args: %w", kind, err)
	}
	stream := StreamFor(kind, key)
	job, err := Load(ctx, log, workspace, stream)
	if err != nil {
		return false, err
	}
	if job.Active() {
		return false, nil
	}
	// An enqueue is immediate; ScheduledAt is only ever set by a retry,
	// snooze, or rescue. Writing now here would make the job's state depend
	// on how the engine rounds the event time.
	payload, err := json.Marshal(enqueuedPayload{Kind: kind, Key: key, Args: encoded, MaxAttempts: maxAttempts})
	if err != nil {
		return false, err
	}
	_, err = log.Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{
		Kind: KindEnqueued, ActorID: core.SystemActor("queue").ID, ActorRole: string(core.ActorSystem), Payload: payload, At: now,
	}})
	if eventlog.IsVersionConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
