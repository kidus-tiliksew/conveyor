// Package projection folds an event log into in-process read models.
//
// A Projector is a pure fold: Apply one event, marshal or restore its state.
// A Runner owns one workspace's projectors, replays the log into them from
// the last snapshot position, and writes snapshots at a fixed cadence so a
// restart resumes from a position rather than from the first event. Bumping
// a projector's Version discards its snapshot and rebuilds from zero, which
// is how a read model changes shape without a schema migration.
package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Projector is one read model's fold.
type Projector interface {
	// Name identifies the projector; it is part of the snapshot key.
	Name() string
	// Version changes whenever the fold or the state shape changes. A
	// mismatch with the stored snapshot forces a rebuild from position zero.
	Version() int
	// Apply folds one event into the state. Events arrive in position order.
	Apply(event eventlog.Event) error
	// MarshalState serialises the state for a snapshot.
	MarshalState() ([]byte, error)
	// UnmarshalState restores the state from a snapshot.
	UnmarshalState(data []byte) error
	// Reset returns the state to empty.
	Reset()
}

// Runner replays one workspace's log into a set of projectors.
type Runner struct {
	store      eventlog.Store
	workspace  string
	projectors []Projector
	position   eventlog.Position
	unsaved    int
	// SnapshotEvery is the number of applied events between snapshots. Zero
	// disables automatic snapshots; Snapshot can still be called directly.
	SnapshotEvery int
	// PageSize is how many events one Tail call fetches.
	PageSize int
}

const (
	defaultSnapshotEvery = 5000
	defaultPageSize      = 1000
)

func NewRunner(store eventlog.Store, workspace string, projectors ...Projector) *Runner {
	return &Runner{store: store, workspace: workspace, projectors: projectors, SnapshotEvery: defaultSnapshotEvery, PageSize: defaultPageSize}
}

// Position is the log position the projectors reflect.
func (r *Runner) Position() eventlog.Position { return r.position }

// Workspace is the workspace this runner folds.
func (r *Runner) Workspace() string { return r.workspace }

type snapshotState struct {
	Version  int               `json:"version"`
	Position eventlog.Position `json:"position"`
	State    json.RawMessage   `json:"state"`
}

func snapshotKey(p Projector) string {
	return fmt.Sprintf("projector/%s", p.Name())
}

// Load restores every projector from its snapshot. If any projector has no
// usable snapshot (missing, or written by a different Version), every
// projector is reset and the runner starts from position zero, so all read
// models always reflect the same position.
func (r *Runner) Load(ctx context.Context) error {
	var loaded []snapshotState
	usable := true
	for _, p := range r.projectors {
		snapshot, err := r.store.GetSnapshot(ctx, r.workspace, snapshotKey(p))
		if errors.Is(err, eventlog.ErrSnapshotMissing) {
			usable = false
			break
		}
		if err != nil {
			return fmt.Errorf("projection: load %s: %w", p.Name(), err)
		}
		var state snapshotState
		if err := json.Unmarshal(snapshot.Blob, &state); err != nil || state.Version != p.Version() {
			usable = false
			break
		}
		loaded = append(loaded, state)
	}
	if usable {
		for i := 1; i < len(loaded); i++ {
			if loaded[i].Position != loaded[0].Position {
				usable = false
				break
			}
		}
	}
	if !usable {
		r.reset()
		return nil
	}
	for i, p := range r.projectors {
		if err := p.UnmarshalState(loaded[i].State); err != nil {
			r.reset()
			return nil
		}
	}
	if len(loaded) > 0 {
		r.position = loaded[0].Position
	}
	r.unsaved = 0
	return nil
}

func (r *Runner) reset() {
	for _, p := range r.projectors {
		p.Reset()
	}
	r.position = 0
	r.unsaved = 0
}

// CatchUp applies every event after the current position and returns how
// many it applied. Snapshots are written along the way at SnapshotEvery.
func (r *Runner) CatchUp(ctx context.Context) (int, error) {
	pageSize := r.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	applied := 0
	for {
		events, err := r.store.Tail(ctx, r.workspace, r.position, pageSize)
		if err != nil {
			return applied, fmt.Errorf("projection: tail after %d: %w", r.position, err)
		}
		if len(events) == 0 {
			return applied, nil
		}
		for _, event := range events {
			if event.Position <= r.position {
				continue
			}
			for _, p := range r.projectors {
				if err := p.Apply(event); err != nil {
					return applied, fmt.Errorf("projection: %s apply %s@%d: %w", p.Name(), event.Stream, event.Version, err)
				}
			}
			r.position = event.Position
			r.unsaved++
			applied++
			if r.SnapshotEvery > 0 && r.unsaved >= r.SnapshotEvery {
				if err := r.Snapshot(ctx); err != nil {
					return applied, err
				}
			}
		}
		if len(events) < pageSize {
			return applied, nil
		}
	}
}

// Snapshot persists every projector at the current position.
func (r *Runner) Snapshot(ctx context.Context) error {
	for _, p := range r.projectors {
		state, err := p.MarshalState()
		if err != nil {
			return fmt.Errorf("projection: marshal %s: %w", p.Name(), err)
		}
		blob, err := json.Marshal(snapshotState{Version: p.Version(), Position: r.position, State: state})
		if err != nil {
			return err
		}
		if err := r.store.PutSnapshot(ctx, r.workspace, eventlog.Snapshot{Key: snapshotKey(p), Position: r.position, Blob: blob}); err != nil {
			return fmt.Errorf("projection: snapshot %s: %w", p.Name(), err)
		}
	}
	r.unsaved = 0
	return nil
}
