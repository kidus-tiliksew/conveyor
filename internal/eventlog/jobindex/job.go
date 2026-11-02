// Package jobindex holds the job fold and the job index the event-log
// drivers keep beside the log (component-durable-queue, The job index;
// req-quiet-idle-runtime AC-5.1, AC-5.2).
//
// Every job is one stream, job/<kind>:<key>. Job.Apply is the one
// transition table: logqueue's Load and its runtime's tail fold with it,
// and the drivers fold with it when they write a stream's index row in the
// same transaction as the append. It lives here, below the drivers, so a
// driver folds without importing the queue; logqueue re-exports it.
package jobindex

import (
	"encoding/json"
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

// EnqueuedPayload is the payload of job.enqueued.
type EnqueuedPayload struct {
	Kind        string          `json:"kind"`
	Key         string          `json:"key"`
	Args        json.RawMessage `json:"args"`
	MaxAttempts int             `json:"max_attempts"`
	// ScheduledAt delays the first run; zero means now.
	ScheduledAt time.Time `json:"scheduled_at,omitempty"`
}

// ClaimedPayload is the payload of job.claimed.
type ClaimedPayload struct {
	Attempt   int       `json:"attempt"`
	Worker    string    `json:"worker"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// OutcomePayload is the payload of job.completed, job.failed, job.snoozed,
// job.rescued, and job.discarded.
type OutcomePayload struct {
	Attempt int       `json:"attempt"`
	Error   string    `json:"error,omitempty"`
	NextAt  time.Time `json:"next_at,omitempty"`
	Until   time.Time `json:"until,omitempty"`
}

// Fold derives a job from its stream's events, in version order.
func Fold(workspace string, stream eventlog.StreamID, events []eventlog.Event) (Job, error) {
	job := NewJob(workspace, stream)
	for _, event := range events {
		if err := job.Apply(event); err != nil {
			return job, err
		}
	}
	return job, nil
}

// NewJob is the empty job for a stream, before any event.
func NewJob(workspace string, stream eventlog.StreamID) Job {
	job := Job{Workspace: workspace, Stream: stream}
	job.Kind, job.Key, _ = strings.Cut(stream.EntityID(), ":")
	return job
}

// Apply folds one more event of the job's stream into it. Fold and the
// runtime's tail both go through here, so there is one transition table.
func (j *Job) Apply(event eventlog.Event) error {
	j.Head = event.Version
	switch event.Kind {
	case KindEnqueued:
		var p EnqueuedPayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return fmt.Errorf("jobindex: %s@%d: %w", j.Stream, event.Version, err)
		}
		j.Kind, j.Key, j.Args = p.Kind, p.Key, p.Args
		j.MaxAttempts = p.MaxAttempts
		j.Attempt = 0
		j.State = StateAvailable
		j.ScheduledAt = p.ScheduledAt
		if !p.ScheduledAt.IsZero() && p.ScheduledAt.Truncate(time.Microsecond).After(event.At) {
			j.State = StateScheduled
		}
		j.ClaimedAt, j.ClaimedBy, j.LastError = time.Time{}, "", ""
		j.EnqueuedAt = event.Position
		j.Generation++
	case KindClaimed:
		var p ClaimedPayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return fmt.Errorf("jobindex: %s@%d: %w", j.Stream, event.Version, err)
		}
		j.State = StateRunning
		j.Attempt = p.Attempt
		j.ClaimedAt, j.ClaimedBy = p.ClaimedAt, p.Worker
	case KindCompleted:
		j.State = StateCompleted
		j.ClaimedBy = ""
	case KindFailed, KindRescued:
		var p OutcomePayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return fmt.Errorf("jobindex: %s@%d: %w", j.Stream, event.Version, err)
		}
		j.LastError = p.Error
		j.State = StateScheduled
		j.ScheduledAt = p.NextAt
		j.ClaimedBy = ""
	case KindSnoozed:
		var p OutcomePayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return fmt.Errorf("jobindex: %s@%d: %w", j.Stream, event.Version, err)
		}
		// A snooze is not an execution: hand the attempt back.
		if j.Attempt > 0 {
			j.Attempt--
		}
		j.State = StateScheduled
		j.ScheduledAt = p.Until
		j.ClaimedBy = ""
	case KindDiscarded:
		var p OutcomePayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return fmt.Errorf("jobindex: %s@%d: %w", j.Stream, event.Version, err)
		}
		if p.Error != "" {
			j.LastError = p.Error
		}
		j.State = StateDiscarded
		j.ClaimedBy = ""
	}
	return nil
}
