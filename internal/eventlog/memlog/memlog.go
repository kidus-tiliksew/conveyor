// Package memlog is the in-memory eventlog driver: the reference
// implementation the conformance suite is written against, and the test
// double for everything above the store.
package memlog

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

type workspaceLog struct {
	streams   map[eventlog.StreamID][]eventlog.Event
	ordered   []eventlog.Event // by position
	position  eventlog.Position
	snapshots map[string]eventlog.Snapshot
}

// Store is a process-local eventlog.Store. It is safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	workspaces map[string]*workspaceLog
	now        func() time.Time
}

var _ eventlog.Store = (*Store)(nil)

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
		w = &workspaceLog{streams: map[eventlog.StreamID][]eventlog.Event{}, snapshots: map[string]eventlog.Snapshot{}}
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
	for _, incoming := range events {
		incoming = eventlog.Normalize(incoming, now)
		head++
		w.position++
		stored := eventlog.Event{
			Workspace: workspace, Stream: stream, Version: head, Position: w.position,
			Kind: incoming.Kind, ActorID: incoming.ActorID, ActorRole: incoming.ActorRole,
			Payload: append([]byte(nil), incoming.Payload...), At: incoming.At,
		}
		w.streams[stream] = append(w.streams[stream], stored)
		w.ordered = append(w.ordered, stored)
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

func (s *Store) PutSnapshot(_ context.Context, workspace string, snapshot eventlog.Snapshot) error {
	if workspace == "" {
		return eventlog.ErrEmptyWorkspace
	}
	if snapshot.Key == "" {
		return eventlog.ErrSnapshotMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.At.IsZero() {
		snapshot.At = s.now().UTC()
	}
	snapshot.Blob = append([]byte(nil), snapshot.Blob...)
	s.workspace(workspace).snapshots[snapshot.Key] = snapshot
	return nil
}

func (s *Store) GetSnapshot(_ context.Context, workspace, key string) (eventlog.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspace]
	if !ok {
		return eventlog.Snapshot{}, eventlog.ErrSnapshotMissing
	}
	snapshot, ok := w.snapshots[key]
	if !ok {
		return eventlog.Snapshot{}, eventlog.ErrSnapshotMissing
	}
	snapshot.Blob = append([]byte(nil), snapshot.Blob...)
	return snapshot, nil
}

func copyEvents(in []eventlog.Event) []eventlog.Event {
	out := make([]eventlog.Event, len(in))
	for i, e := range in {
		e.Payload = append([]byte(nil), e.Payload...)
		out[i] = e
	}
	return out
}
