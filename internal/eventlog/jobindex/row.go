package jobindex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Row is one job stream's row in event_log_job_index: the projection of
// Job.Apply's fold onto the columns the runtime reads at start
// (component-durable-queue, The job index). A stream that holds events but
// no job.enqueued yet has a row with an empty state.
type Row struct {
	Workspace  string
	Stream     eventlog.StreamID
	Kind       string
	State      State
	Generation int
	// Version is the stream head the row reflects.
	Version eventlog.Version
	// NextAt is the fold's ScheduledAt; zero is stored as null.
	NextAt time.Time
	// ClaimedAt is the fold's ClaimedAt; zero is stored as null.
	ClaimedAt time.Time
}

// UnfinishedStates are the states the runtime loads at start: a job in one
// of them may still run.
var UnfinishedStates = []State{StateAvailable, StateScheduled, StateRunning}

// Unfinished reports whether a row in state may still run.
func Unfinished(state State) bool {
	switch state {
	case StateAvailable, StateScheduled, StateRunning:
		return true
	}
	return false
}

// IsJobStream reports whether the drivers keep an index row for stream.
func IsJobStream(stream eventlog.StreamID) bool {
	return stream.Type() == StreamType
}

// RowOf projects a folded job onto its index row. Times are kept to
// microseconds in UTC, the precision every durable driver stores.
func RowOf(job Job) Row {
	return Row{
		Workspace:  job.Workspace,
		Stream:     job.Stream,
		Kind:       job.Kind,
		State:      job.State,
		Generation: job.Generation,
		Version:    job.Head,
		NextAt:     storedTime(job.ScheduledAt),
		ClaimedAt:  storedTime(job.ClaimedAt),
	}
}

func storedTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Microsecond)
}

// EmptyRow is the row of a stream before any event.
func EmptyRow(workspace string, stream eventlog.StreamID) Row {
	return RowOf(NewJob(workspace, stream))
}

// seed is the partial job a row stands for. Every column of the row changes
// under Apply only as a function of the columns themselves and the event, so
// folding events onto the seed gives the same row as a full fold.
func (r Row) seed() Job {
	job := NewJob(r.Workspace, r.Stream)
	if r.Kind != "" {
		job.Kind = r.Kind
	}
	job.State = r.State
	job.Generation = r.Generation
	job.Head = r.Version
	job.ScheduledAt = r.NextAt
	job.ClaimedAt = r.ClaimedAt
	return job
}

// Advance folds events, which follow the row's version in order, onto the
// row. It equals RowOf of a full fold of the stream through those events.
func Advance(prev Row, events []eventlog.Event) (Row, error) {
	job := prev.seed()
	for _, event := range events {
		if err := job.Apply(event); err != nil {
			return prev, err
		}
	}
	return RowOf(job), nil
}

// Next computes a stream's row after an append, for a driver that holds the
// stream's head lock. prev is the stored row, if any; observed is the head
// the append saw before its events; appended are the events it wrote.
// When the stored row matches the observed head, the new events are folded
// onto it. Otherwise the row is missing or behind its stream, so readAll,
// which must see the appended events, supplies the whole stream to fold.
func Next(workspace string, stream eventlog.StreamID, prev *Row, observed eventlog.Version, appended []eventlog.Event, readAll func() ([]eventlog.Event, error)) (Row, error) {
	switch {
	case prev != nil && prev.Version == observed:
		return Advance(*prev, appended)
	case prev == nil && observed == 0:
		return Advance(EmptyRow(workspace, stream), appended)
	}
	events, err := readAll()
	if err != nil {
		return Row{}, err
	}
	job, err := Fold(workspace, stream, events)
	if err != nil {
		return Row{}, err
	}
	return RowOf(job), nil
}

// ErrUnsupported is returned by a log wrapper whose driver keeps no index.
var ErrUnsupported = errors.New("jobindex: the log keeps no job index")

// Reader is the job index a driver keeps beside its log. memlog, pglog, and
// s2log implement it, and logqueue.NotifyingLog forwards it.
type Reader interface {
	// UnfinishedJobs reads the workspace's current log position and then
	// the rows in state available, scheduled, or running, ordered by
	// stream. Every event at or below the position is reflected in the
	// rows; a row may also reflect later events.
	UnfinishedJobs(ctx context.Context, workspace string) (eventlog.Position, []Row, error)
	// JobIndexRow reads one stream's row; ok is false when it has none.
	JobIndexRow(ctx context.Context, workspace string, stream eventlog.StreamID) (Row, bool, error)
	// ReadGeneration returns the stream's events from its last job.enqueued
	// at or below version through version, ascending, or from version 1
	// when it has none: the current generation of a job whose row is at
	// version. No earlier event is returned.
	ReadGeneration(ctx context.Context, workspace string, stream eventlog.StreamID, version eventlog.Version) ([]eventlog.Event, error)
	// RepairJobIndex rebuilds, from a full fold, every row of the workspace
	// that is missing or whose version differs from its stream's head, and
	// reports how many it rebuilt. Only mismatched streams are read.
	RepairJobIndex(ctx context.Context, workspace string) (int, error)
}

// ReaderOf returns log's job index when it keeps one.
func ReaderOf(log eventlog.Store) (Reader, bool) {
	reader, ok := log.(Reader)
	return reader, ok
}

// LoadCurrent folds a job from its row and the events of its current
// generation only, read through ReadGeneration, starting from the
// generation the row implies. ok is false when the result does not match
// the row's state, generation, and version; the caller then folds the whole
// stream.
func LoadCurrent(ctx context.Context, reader Reader, row Row) (Job, bool, error) {
	job := NewJob(row.Workspace, row.Stream)
	if row.Version == 0 {
		return job, row.State == StateNone && row.Generation == 0, nil
	}
	events, err := reader.ReadGeneration(ctx, row.Workspace, row.Stream, row.Version)
	if err != nil {
		return Job{}, false, err
	}
	if len(events) > 0 && events[0].Kind == KindEnqueued {
		job.Generation = row.Generation - 1
	}
	for _, event := range events {
		if err := job.Apply(event); err != nil {
			return Job{}, false, fmt.Errorf("jobindex: %s: %w", row.Stream, err)
		}
	}
	got := RowOf(job)
	return job, got.State == row.State && got.Generation == row.Generation && got.Version == row.Version, nil
}

// CurrentGeneration returns the suffix of a stream's events, ascending and
// ending at or below version, that starts at its last job.enqueued, or every
// event through version when there is none. memlog uses it, and the SQL
// drivers select the same rows.
func CurrentGeneration(events []eventlog.Event, version eventlog.Version) []eventlog.Event {
	end := 0
	for end < len(events) && events[end].Version <= version {
		end++
	}
	events = events[:end]
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == KindEnqueued {
			return events[i:]
		}
	}
	return events
}
