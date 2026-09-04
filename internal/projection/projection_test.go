package projection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/projection"
	"github.com/kidus-tiliksew/conveyor/internal/projection/catalog"
)

// counter is a minimal projector: it counts events per kind.
type counter struct {
	version int
	Counts  map[string]int
	applied []eventlog.Position
}

func newCounter(version int) *counter { return &counter{version: version, Counts: map[string]int{}} }

func (c *counter) Name() string { return "counter" }
func (c *counter) Version() int { return c.version }
func (c *counter) Reset()       { c.Counts = map[string]int{}; c.applied = nil }
func (c *counter) Apply(e eventlog.Event) error {
	c.Counts[e.Kind]++
	c.applied = append(c.applied, e.Position)
	return nil
}
func (c *counter) MarshalState() ([]byte, error) { return json.Marshal(c.Counts) }
func (c *counter) UnmarshalState(b []byte) error { return json.Unmarshal(b, &c.Counts) }

func seed(t *testing.T, store eventlog.Store, workspace string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		stream := eventlog.TaskStream(fmt.Sprintf("t%d", i%3))
		if _, err := store.Append(ctx, workspace, stream, eventlog.ExpectAny, []eventlog.NewEvent{{Kind: fmt.Sprintf("k%d", i%2)}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerReplaysAndSnapshotsAtCadence(t *testing.T) {
	ctx := context.Background()
	store := memlog.New()
	seed(t, store, "ws", 25)
	c := newCounter(1)
	r := projection.NewRunner(store, "ws", c)
	r.SnapshotEvery = 10
	r.PageSize = 7
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	applied, err := r.CatchUp(ctx)
	if err != nil || applied != 25 {
		t.Fatalf("applied=%d err=%v", applied, err)
	}
	if c.Counts["k0"] != 13 || c.Counts["k1"] != 12 {
		t.Fatalf("counts=%v", c.Counts)
	}
	for i := 1; i < len(c.applied); i++ {
		if c.applied[i] <= c.applied[i-1] {
			t.Fatalf("positions applied out of order: %v", c.applied)
		}
	}
	if r.Position() != 25 {
		t.Fatalf("position=%d", r.Position())
	}
	snapshot, err := store.GetSnapshot(ctx, "ws", "projector/counter")
	if err != nil {
		t.Fatalf("no cadence snapshot: %v", err)
	}
	if snapshot.Position != 20 {
		t.Fatalf("cadence snapshot at %d, want 20", snapshot.Position)
	}

	// A fresh runner resumes from the snapshot and applies only the rest.
	c2 := newCounter(1)
	r2 := projection.NewRunner(store, "ws", c2)
	if err := r2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if r2.Position() != 20 {
		t.Fatalf("loaded position=%d", r2.Position())
	}
	applied, err = r2.CatchUp(ctx)
	if err != nil || applied != 5 {
		t.Fatalf("resume applied=%d err=%v", applied, err)
	}
	if c2.Counts["k0"] != 13 || c2.Counts["k1"] != 12 {
		t.Fatalf("resumed counts=%v", c2.Counts)
	}
	if len(c2.applied) != 5 {
		t.Fatalf("resumed runner applied %d events, want 5", len(c2.applied))
	}
	// Nothing new: CatchUp is a no-op.
	applied, _ = r2.CatchUp(ctx)
	if applied != 0 {
		t.Fatalf("idle catch-up applied %d", applied)
	}
}

func TestRunnerRebuildsOnVersionBump(t *testing.T) {
	ctx := context.Background()
	store := memlog.New()
	seed(t, store, "ws", 12)
	r := projection.NewRunner(store, "ws", newCounter(1))
	r.SnapshotEvery = 0
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCounter(2)
	r2 := projection.NewRunner(store, "ws", c)
	if err := r2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if r2.Position() != 0 {
		t.Fatalf("version bump did not reset: position=%d", r2.Position())
	}
	applied, err := r2.CatchUp(ctx)
	if err != nil || applied != 12 {
		t.Fatalf("rebuild applied=%d err=%v", applied, err)
	}
}

func TestRunnerResetsWhenProjectorsDisagreeOnPosition(t *testing.T) {
	ctx := context.Background()
	store := memlog.New()
	seed(t, store, "ws", 6)
	a, b := newCounter(1), &named{counter: newCounter(1), name: "other"}
	r := projection.NewRunner(store, "ws", a, b)
	r.SnapshotEvery = 0
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	// Corrupt one snapshot's position: the runner must not trust a pair of
	// read models that reflect different positions.
	if err := store.PutSnapshot(ctx, "ws", eventlog.Snapshot{Key: "projector/other", Blob: []byte(`{"version":1,"position":2,"state":{}}`)}); err != nil {
		t.Fatal(err)
	}
	r2 := projection.NewRunner(store, "ws", newCounter(1), &named{counter: newCounter(1), name: "other"})
	if err := r2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if r2.Position() != 0 {
		t.Fatalf("mismatched snapshots accepted: position=%d", r2.Position())
	}
}

type named struct {
	*counter
	name string
}

func (n *named) Name() string { return n.name }

func TestCatalogTracksSnapshotsAndEventsSince(t *testing.T) {
	c := catalog.New()
	stream := eventlog.TaskStream("t1")
	events := []eventlog.Event{
		{Stream: stream, Version: 1, Position: 1, Kind: "task.created", LegacyID: 10},
		{Stream: stream, Version: 2, Position: 2, Kind: "task.classified", LegacyID: 11},
		{Stream: stream, Version: 3, Position: 3, Kind: eventlog.SnapshotImportedKind, Payload: []byte(`{"family":"task","table":"tasks","id":"t1","row":{"id":"t1","state":"queued"},"content_hash":"sha256:abc"}`)},
	}
	for _, e := range events {
		if err := c.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	entity := c.Entity(stream)
	if entity == nil || entity.Snapshot == nil || entity.Snapshot.ContentHash != "sha256:abc" || entity.Snapshot.Version != 3 || len(entity.Since) != 0 {
		t.Fatalf("entity after snapshot=%+v", entity)
	}
	if len(c.Stale()) != 0 {
		t.Fatalf("stale after snapshot=%d", len(c.Stale()))
	}
	if err := c.Apply(eventlog.Event{Stream: stream, Version: 4, Position: 4, Kind: "task.state_changed", LegacyID: 12}); err != nil {
		t.Fatal(err)
	}
	stale := c.Stale()
	if len(stale) != 1 || stale[0].Since[0].Kind != "task.state_changed" || stale[0].Head != 4 {
		t.Fatalf("stale=%+v", stale)
	}
	if err := c.Apply(eventlog.Event{Stream: eventlog.RequirementStream("r1"), Version: 1, Position: 5, Kind: "requirement.created"}); err != nil {
		t.Fatal(err)
	}
	if got := c.Streams("requirement"); len(got) != 1 || got[0] != eventlog.RequirementStream("r1") {
		t.Fatalf("streams(requirement)=%v", got)
	}
	if len(c.Stale()) != 2 {
		t.Fatalf("stale=%d, want the unsnapshotted requirement too", len(c.Stale()))
	}
	data, err := c.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	restored := catalog.New()
	if err := restored.UnmarshalState(data); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 2 || restored.Entity(stream).Snapshot.Row["state"] != "queued" {
		t.Fatalf("restored catalog lost state")
	}
}
