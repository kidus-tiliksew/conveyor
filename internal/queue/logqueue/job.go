// Package logqueue is the durable queue on the event log.
//
// Every job is one stream, job/<kind>:<key>, so uniqueness per key is the
// stream itself rather than a lookup. Enqueue appends job.enqueued unless
// the stream's fold says a job is still active; a claim is an append of
// job.claimed that names the version the worker observed, so two replicas
// racing for the same job have exactly one win. Retry, snooze, rescue, and
// exhaustion are events on the same stream. Nothing here is engine-specific:
// the package needs only eventlog.Store.
//
// Log-core migration plan, phase 3, task 3.2.
package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Event kinds on a job stream.
const (
	KindEnqueued  = "job.enqueued"
	KindClaimed   = "job.claimed"
	KindCompleted = "job.completed"
	KindFailed    = "job.failed"
	KindSnoozed   = "job.snoozed"
	KindRescued   = "job.rescued"
	KindDiscarded = "job.discarded"
)

// StreamType is the stream type every job lives under.
const StreamType = "job"

// StreamFor is the stream that owns the job for one kind and unique key.
func StreamFor(kind, key string) eventlog.StreamID {
	return eventlog.StreamID(StreamType + "/" + kind + ":" + key)
}

// State is where a job stands after folding its stream.
type State string

const (
	// StateNone means the stream has no job yet.
	StateNone State = ""
	// StateAvailable means the job may be claimed now.
	StateAvailable State = "available"
	// StateScheduled means the job may be claimed once ScheduledAt passes:
	// a retry, a snooze, or a rescue.
	StateScheduled State = "scheduled"
	// StateRunning means a worker holds the job.
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateDiscarded State = "discarded"
)

// Job is the folded state of one job stream.
type Job struct {
	Workspace string
	Stream    eventlog.StreamID
	Kind      string
	Key       string
	Args      json.RawMessage
	State     State
	// Attempt is the number of executions counted so far. A claim runs
	// Attempt+1; a snooze hands the count back.
	Attempt     int
	MaxAttempts int
	// ScheduledAt is when a scheduled job becomes claimable.
	ScheduledAt time.Time
	ClaimedAt   time.Time
	ClaimedBy   string
	LastError   string
	// Head is the stream version the fold reflects; a claim names it.
	Head eventlog.Version
	// EnqueuedAt orders claimable jobs: earliest enqueue first.
	EnqueuedAt eventlog.Position
	// Generation counts enqueues on the stream; the handler's job id
	// carries it so a completion for an earlier generation is recognisable.
	Generation int
}

// Active reports whether a new enqueue must be suppressed.
func (j Job) Active() bool {
	switch j.State {
	case StateAvailable, StateScheduled, StateRunning:
		return true
	}
	return false
}

// Claimable reports whether a worker may claim the job at now.
func (j Job) Claimable(now time.Time) bool {
	switch j.State {
	case StateAvailable:
		return true
	case StateScheduled:
		return !now.Before(j.ScheduledAt)
	}
	return false
}

// ID is the identity handed to handlers: stream and generation.
func (j Job) ID() string {
	return fmt.Sprintf("%s@%d", j.Stream, j.Generation)
}

type enqueuedPayload struct {
	Kind        string          `json:"kind"`
	Key         string          `json:"key"`
	Args        json.RawMessage `json:"args"`
	MaxAttempts int             `json:"max_attempts"`
	ScheduledAt time.Time       `json:"scheduled_at"`
}

type claimedPayload struct {
	Attempt   int       `json:"attempt"`
	Worker    string    `json:"worker"`
	ClaimedAt time.Time `json:"claimed_at"`
}

type outcomePayload struct {
	Attempt int       `json:"attempt"`
	Error   string    `json:"error,omitempty"`
	NextAt  time.Time `json:"next_at,omitempty"`
	Until   time.Time `json:"until,omitempty"`
}

// Fold derives a job from its stream's events, in version order.
func Fold(workspace string, stream eventlog.StreamID, events []eventlog.Event) (Job, error) {
	job := Job{Workspace: workspace, Stream: stream}
	kind, key, _ := strings.Cut(stream.EntityID(), ":")
	job.Kind, job.Key = kind, key
	for _, event := range events {
		job.Head = event.Version
		switch event.Kind {
		case KindEnqueued:
			var p enqueuedPayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			job.Kind, job.Key, job.Args = p.Kind, p.Key, p.Args
			job.MaxAttempts = p.MaxAttempts
			job.Attempt = 0
			job.State = StateAvailable
			job.ScheduledAt = p.ScheduledAt
			if !p.ScheduledAt.IsZero() && p.ScheduledAt.After(event.At) {
				job.State = StateScheduled
			}
			job.ClaimedAt, job.ClaimedBy, job.LastError = time.Time{}, "", ""
			job.EnqueuedAt = event.Position
			job.Generation++
		case KindClaimed:
			var p claimedPayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			job.State = StateRunning
			job.Attempt = p.Attempt
			job.ClaimedAt, job.ClaimedBy = p.ClaimedAt, p.Worker
		case KindCompleted:
			job.State = StateCompleted
			job.ClaimedBy = ""
		case KindFailed:
			var p outcomePayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			job.LastError = p.Error
			job.State = StateScheduled
			job.ScheduledAt = p.NextAt
			job.ClaimedBy = ""
		case KindSnoozed:
			var p outcomePayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			// A snooze is not an execution: hand the attempt back.
			if job.Attempt > 0 {
				job.Attempt--
			}
			job.State = StateScheduled
			job.ScheduledAt = p.Until
			job.ClaimedBy = ""
		case KindRescued:
			var p outcomePayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			job.LastError = p.Error
			job.State = StateScheduled
			job.ScheduledAt = p.NextAt
			job.ClaimedBy = ""
		case KindDiscarded:
			var p outcomePayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return job, fmt.Errorf("logqueue: %s@%d: %w", stream, event.Version, err)
			}
			if p.Error != "" {
				job.LastError = p.Error
			}
			job.State = StateDiscarded
			job.ClaimedBy = ""
		}
	}
	return job, nil
}

// Load reads and folds one job stream.
func Load(ctx context.Context, log eventlog.Log, workspace string, stream eventlog.StreamID) (Job, error) {
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
func Enqueue(ctx context.Context, log eventlog.Log, workspace, kind, key string, args any, maxAttempts int, now time.Time) (bool, error) {
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
	payload, err := json.Marshal(enqueuedPayload{Kind: kind, Key: key, Args: encoded, MaxAttempts: maxAttempts, ScheduledAt: now.UTC()})
	if err != nil {
		return false, err
	}
	_, err = log.Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{
		Kind: KindEnqueued, ActorID: "queue", ActorRole: "system", Payload: payload, At: now,
	}})
	if eventlog.IsVersionConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
