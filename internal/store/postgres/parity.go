package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/projection"
	"github.com/kidus-tiliksew/conveyor/internal/projection/catalog"
)

// Parity: compare the log-derived catalog with the legacy projection rows.
//
// For every entity a genesis snapshot would cover, the checker hashes the
// live rows exactly as the import does and compares the hash with the last
// snapshot in the catalog. Three outcomes:
//
//   - match: rows unchanged since the snapshot. Any events since are
//     kinds that did not touch the projection.
//   - drift: rows changed since the snapshot. The events since are the
//     kinds a real projector has to fold; this is the work list.
//   - missing: the entity has rows but no stream in the log at all, which
//     means the import has not run for it, or a write path bypasses the
//     mirror.
//
// Orphans are streams in the catalog whose rows are gone, which the legacy
// store does by deleting; the log keeps them, as it should.
//
// Log-core migration plan, phase 2, task 2.2.

// ParityDrift is one entity whose rows moved on from its snapshot.
type ParityDrift struct {
	Stream       eventlog.StreamID `json:"stream"`
	SnapshotHash string            `json:"snapshot_hash"`
	LiveHash     string            `json:"live_hash"`
	KindsSince   []string          `json:"kinds_since"`
}

// ParityFamilyReport summarizes one entity family.
type ParityFamilyReport struct {
	Family  string `json:"family"`
	Match   int    `json:"match"`
	Drift   int    `json:"drift"`
	Missing int    `json:"missing"`
	Orphans int    `json:"orphans"`
	// UnfoldedKinds counts, per kind, how many drifted entities saw that kind
	// since their snapshot. Sorted output comes from KindsByWeight.
	UnfoldedKinds map[string]int `json:"unfolded_kinds,omitempty"`
	Drifts        []ParityDrift  `json:"drifts,omitempty"`
}

// ParityReport is the result of LogParity for one workspace.
type ParityReport struct {
	Workspace    string               `json:"workspace"`
	Position     eventlog.Position    `json:"position"`
	Streams      int                  `json:"streams"`
	Families     []ParityFamilyReport `json:"families"`
	ReplayMillis int64                `json:"replay_millis"`
	Duration     time.Duration        `json:"duration"`
}

// Clean reports whether every entity matched.
func (r ParityReport) Clean() bool {
	for _, f := range r.Families {
		if f.Drift > 0 || f.Missing > 0 {
			return false
		}
	}
	return true
}

// KindsByWeight lists a family's unfolded kinds, most frequent first.
func (f ParityFamilyReport) KindsByWeight() []string {
	kinds := make([]string, 0, len(f.UnfoldedKinds))
	for kind := range f.UnfoldedKinds {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if f.UnfoldedKinds[kinds[i]] != f.UnfoldedKinds[kinds[j]] {
			return f.UnfoldedKinds[kinds[i]] > f.UnfoldedKinds[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	return kinds
}

// ParityOptions controls one check.
type ParityOptions struct {
	// MaxDrifts caps the per-family drift detail; zero keeps every drift.
	MaxDrifts int
}

// LogParity replays the workspace's log into a catalog and compares it with
// the legacy rows. It never writes.
func (s *Store) LogParity(ctx context.Context, workspace string, opts ParityOptions) (ParityReport, error) {
	started := time.Now()
	cat := catalog.New()
	runner := projection.NewRunner(s.logDriver(), workspace, cat)
	runner.SnapshotEvery = 0
	if _, err := runner.CatchUp(ctx); err != nil {
		return ParityReport{}, err
	}
	report := ParityReport{Workspace: workspace, Position: runner.Position(), Streams: cat.Len(), ReplayMillis: time.Since(started).Milliseconds()}

	families := genesisFamilies
	if workspace == eventlog.DeploymentWorkspace {
		families = genesisDeploymentFamilies
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ParityReport{}, err
	}
	defer tx.Rollback(ctx)
	for _, family := range families {
		fr := ParityFamilyReport{Family: family.family, UnfoldedKinds: map[string]int{}}
		ids, err := s.genesisEntityIDs(ctx, workspace, family)
		if err != nil {
			return ParityReport{}, err
		}
		live := make(map[eventlog.StreamID]bool, len(ids))
		for _, id := range ids {
			stream := family.stream(id)
			live[stream] = true
			snapshot, err := s.readGenesisSnapshot(ctx, tx, workspace, family, id)
			if err != nil {
				return ParityReport{}, fmt.Errorf("%s %s: %w", family.table, id, err)
			}
			if snapshot == nil {
				continue
			}
			entity := cat.Entity(stream)
			if entity == nil || entity.Snapshot == nil {
				fr.Missing++
				continue
			}
			if entity.Snapshot.ContentHash == snapshot.ContentHash {
				fr.Match++
				continue
			}
			fr.Drift++
			drift := ParityDrift{Stream: stream, SnapshotHash: entity.Snapshot.ContentHash, LiveHash: snapshot.ContentHash}
			seen := map[string]bool{}
			for _, ref := range entity.Since {
				if !seen[ref.Kind] {
					seen[ref.Kind] = true
					drift.KindsSince = append(drift.KindsSince, ref.Kind)
					fr.UnfoldedKinds[ref.Kind]++
				}
			}
			if opts.MaxDrifts == 0 || len(fr.Drifts) < opts.MaxDrifts {
				fr.Drifts = append(fr.Drifts, drift)
			}
		}
		streamType := family.stream("x").Type()
		for _, stream := range cat.Streams(streamType) {
			if !live[stream] {
				fr.Orphans++
			}
		}
		if len(fr.UnfoldedKinds) == 0 {
			fr.UnfoldedKinds = nil
		}
		report.Families = append(report.Families, fr)
	}
	report.Duration = time.Since(started)
	return report, nil
}
