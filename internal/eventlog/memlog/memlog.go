// Package memlog is the in-memory eventlog driver: the reference
// implementation the conformance suite is written against, and the test
// double for everything above the store.
package memlog

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

type workspaceLog struct {
	streams  map[eventlog.StreamID][]eventlog.Event
	ordered  []eventlog.Event // by position
	position eventlog.Position
	// index is the job index: one row per job stream, written with the
	// append under the same mutex (component-durable-queue, The job index).
	index map[eventlog.StreamID]jobindex.Row
}

// Store is a process-local eventlog.Store. It is safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	workspaces map[string]*workspaceLog
	now        func() time.Time
}

var (
	_ eventlog.Store  = (*Store)(nil)
	_ jobindex.Reader = (*Store)(nil)
)

func New() *Store {
	return &Store{workspaces: map[string]*workspaceLog{}, now: time.Now}
}

// WithClock replaces the timestamp source; tests use it for determinism.
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

func (s *Store) workspace(id string) *workspaceLog {
	w, ok := s.workspaces[id]
	if !ok {
		w = &workspaceLog{streams: map[eventlog.StreamID][]eventlog.Event{}, index: map[eventlog.StreamID]jobindex.Row{}}
		s.workspaces[id] = w
	}
	return w
}

func (s *Store) Append(_ context.Context, workspace string, stream eventlog.StreamID, expected eventlog.Version, events []eventlog.NewEvent) (eventlog.Version, error) {
	if err := eventlog.ValidateAppend(workspace, stream, events); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workspace(workspace)
	head := eventlog.Version(len(w.streams[stream]))
	if expected != eventlog.ExpectAny && expected != head {
		return head, &eventlog.VersionConflictError{Workspace: workspace, Stream: stream, Expected: expected, Actual: head}
	}
	now := s.now().UTC()
	observed := head
	position := w.position
	stored := make([]eventlog.Event, 0, len(events))
	for _, incoming := range events {
		incoming = eventlog.Normalize(incoming, now)
		head++
		position++
		stored = append(stored, eventlog.Event{
			Workspace: workspace, Stream: stream, Version: head, Position: position,
			Kind: incoming.Kind, ActorID: incoming.ActorID, ActorRole: incoming.ActorRole,
			Payload: append([]byte(nil), incoming.Payload...), At: incoming.At,
		})
	}
	// The index row is computed before anything is stored, so a fold
	// error leaves the log and the index as they were.
	var row jobindex.Row
	if jobindex.IsJobStream(stream) {
		var prev *jobindex.Row
		if current, ok := w.index[stream]; ok {
			prev = &current
		}
		var err error
		row, err = jobindex.Next(workspace, stream, prev, observed, stored, func() ([]eventlog.Event, error) {
			return append(append([]eventlog.Event(nil), w.streams[stream]...), stored...), nil
		})
		if err != nil {
			return observed, fmt.Errorf("memlog: job index: %w", err)
		}
	}
	w.streams[stream] = append(w.streams[stream], stored...)
	w.ordered = append(w.ordered, stored...)
	w.position = position
	if jobindex.IsJobStream(stream) {
		w.index[stream] = row
	}
	return head, nil
}

func (s *Store) Read(_ context.Context, workspace string, stream eventlog.StreamID, after eventlog.Version, limit int) ([]eventlog.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return nil, nil
	}
	all := w.streams[stream]
	if int(after) >= len(all) {
		return nil, nil
	}
	rest := all[after:]
	if limit > 0 && len(rest) > limit {
		rest = rest[:limit]
	}
	return copyEvents(rest), nil
}

func (s *Store) Head(_ context.Context, workspace string, stream eventlog.StreamID) (eventlog.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return 0, nil
	}
	return eventlog.Version(len(w.streams[stream])), nil
}

func (s *Store) Tail(_ context.Context, workspace string, after eventlog.Position, limit int) ([]eventlog.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return nil, nil
	}
	start := sort.Search(len(w.ordered), func(i int) bool { return w.ordered[i].Position > after })
	rest := w.ordered[start:]
	if limit > 0 && len(rest) > limit {
		rest = rest[:limit]
	}
	return copyEvents(rest), nil
}

func copyEvents(in []eventlog.Event) []eventlog.Event {
	out := make([]eventlog.Event, len(in))
	for i, e := range in {
		e.Payload = append([]byte(nil), e.Payload...)
		out[i] = e
	}
	return out
}

// UnfinishedJobs implements jobindex.Reader.
func (s *Store) UnfinishedJobs(_ context.Context, workspace string) (eventlog.Position, []jobindex.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return 0, nil, nil
	}
	var rows []jobindex.Row
	for _, row := range w.index {
		if jobindex.Unfinished(row.State) {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Stream < rows[j].Stream })
	return w.position, rows, nil
}

// JobIndexRow implements jobindex.Reader.
func (s *Store) JobIndexRow(_ context.Context, workspace string, stream eventlog.StreamID) (jobindex.Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return jobindex.Row{}, false, nil
	}
	row, ok := w.index[stream]
	return row, ok, nil
}

// RepairJobIndex implements jobindex.Reader. memlog writes every row with
// its append, so only DropJobIndexRow can leave one to repair.
func (s *Store) RepairJobIndex(_ context.Context, workspace string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return 0, nil
	}
	repaired := 0
	for stream, events := range w.streams {
		if !jobindex.IsJobStream(stream) || len(events) == 0 {
			continue
		}
		if row, ok := w.index[stream]; ok && row.Version == eventlog.Version(len(events)) {
			continue
		}
		job, err := jobindex.Fold(workspace, stream, events)
		if err != nil {
			return repaired, fmt.Errorf("memlog: repair job index %s: %w", stream, err)
		}
		w.index[stream] = jobindex.RowOf(job)
		repaired++
	}
	return repaired, nil
}

// DropJobIndexRow deletes one stream's index row, standing in for an
// append that an older binary made without writing the index. Tests use it.
func (s *Store) DropJobIndexRow(workspace string, stream eventlog.StreamID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workspaces[workspace]; ok {
		delete(w.index, stream)
	}
}
