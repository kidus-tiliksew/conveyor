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
	state, head, fromIndex, err := enqueueHead(ctx, log, workspace, stream)
	if err != nil {
		return false, err
	}
	if jobindex.Unfinished(state) {
		return false, nil
	}
	// An enqueue is immediate; ScheduledAt is only ever set by a retry,
	// snooze, or rescue. Writing now here would make the job's state depend
	// on how the engine rounds the event time.
	payload, err := json.Marshal(enqueuedPayload{Kind: kind, Key: key, Args: encoded, MaxAttempts: maxAttempts})
	if err != nil {
		return false, err
	}
	events := []eventlog.NewEvent{{
		Kind: KindEnqueued, ActorID: core.SystemActor("queue").ID, ActorRole: string(core.ActorSystem), Payload: payload, At: now,
	}}
	_, err = log.Append(ctx, workspace, stream, head, events)
	if eventlog.IsVersionConflict(err) && fromIndex {
		// The index row is written in every append's transaction, so a
		// conflict means a concurrent writer; fold the stream once to tell a
		// lost race from a row that fell behind.
		job, loadErr := Load(ctx, log, workspace, stream)
		if loadErr != nil {
			return false, loadErr
		}
		if job.Active() {
			return false, nil
		}
		_, err = log.Append(ctx, workspace, stream, job.Head, events)
	}
	if eventlog.IsVersionConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// enqueueHead returns the stream's job state and head from its job index row,
// one primary-key read, instead of folding every event of the stream. A log
// without an index, or a stream without a row, is folded.
func enqueueHead(ctx context.Context, log eventlog.Store, workspace string, stream eventlog.StreamID) (State, eventlog.Version, bool, error) {
	if reader, ok := jobindex.ReaderOf(log); ok {
		row, found, err := reader.JobIndexRow(ctx, workspace, stream)
		if err != nil && !errors.Is(err, jobindex.ErrUnsupported) {
			return "", 0, false, err
		}
		if err == nil && found {
			return row.State, row.Version, true, nil
		}
	}
	job, err := Load(ctx, log, workspace, stream)
	if err != nil {
		return "", 0, false, err
	}
	return job.State, job.Head, false, nil
}

// StreamUnfinished reports whether the stream still holds a job that may run,
// from the job index when the log keeps one.
func StreamUnfinished(ctx context.Context, log eventlog.Store, workspace string, stream eventlog.StreamID) (bool, error) {
	state, _, _, err := enqueueHead(ctx, log, workspace, stream)
	if err != nil {
		return false, err
	}
	return jobindex.Unfinished(state), nil
}

// LoadDiscarded loads a discarded job from its current generation. A missing,
// unsupported, or inconsistent index row folds the whole stream. ok is true
// only when the job was discarded after its final attempt.
func LoadDiscarded(ctx context.Context, log eventlog.Store, workspace string, stream eventlog.StreamID) (Job, bool, error) {
	if reader, ok := jobindex.ReaderOf(log); ok {
		row, found, err := reader.JobIndexRow(ctx, workspace, stream)
		if err != nil && !errors.Is(err, jobindex.ErrUnsupported) {
			return Job{}, false, err
		}
		if err == nil && found {
			if row.State != StateDiscarded {
				return Job{}, false, nil
			}
			job, matched, err := jobindex.LoadCurrent(ctx, reader, row)
			if err != nil {
				return Job{}, false, err
			}
			if matched {
				return job, job.Attempt >= job.MaxAttempts, nil
			}
		}
	}
	job, err := Load(ctx, log, workspace, stream)
	if err != nil {
		return Job{}, false, err
	}
	return job, job.State == StateDiscarded && job.Attempt >= job.MaxAttempts, nil
}
