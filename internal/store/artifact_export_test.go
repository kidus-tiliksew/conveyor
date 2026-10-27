package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// SeedEventsForTest appends test-only events with controlled IDs, mapping
// each relative rank onto a base far above the allocator. The allocator is
// not advanced, so later ordinary appends receive lower IDs, as SingleStore's
// per-aggregator ranges can produce.
func SeedEventsForTest(t *testing.T, st Backend, ctx context.Context, base int64, events []core.Event) []core.Event {
	t.Helper()
	m := st.(*volatileMemory).memory
	m.mu.Lock()
	defer m.mu.Unlock()
	if base == 0 {
		base = m.nextEventID + 1<<40
		for _, items := range m.events {
			for _, e := range items {
				base = max(base, e.ID+1<<40)
			}
		}
	}
	out := make([]core.Event, 0, len(events))
	for _, e := range events {
		if _, ok := m.tasks[e.TaskID]; (!ok && e.TaskID != "") || e.ID <= 0 || e.At.IsZero() {
			t.Fatalf("seed event needs an existing or empty task, a positive rank and a time: %+v", e)
		}
		e.ID = base + e.ID
		if e.Payload == nil {
			e.Payload = core.JSONPayload(struct{}{})
		}
		if e.ActorRole == "" {
			e.ActorID, e.ActorRole = "system", core.ActorSystem
		}
		m.events[e.TaskID] = append(m.events[e.TaskID], e)
		out = append(out, e)
	}
	return out
}

// SeedArtifactMetadataForTest models a historical row, bypassing intake only
// inside test binaries. Ownership links remain unchanged.
func SeedArtifactMetadataForTest(t *testing.T, st Backend, ctx context.Context, a core.Artifact, content []byte) {
	t.Helper()
	m := st.(*volatileMemory).memory
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, _ := WorkspaceFromContext(ctx)
	key := memoryArtifactKey{workspace: ws, id: a.ID}
	existing, ok := m.artifacts[key]
	if !ok {
		t.Fatal("seed requires an existing artifact")
	}
	existing.meta = a
	existing.content = append([]byte(nil), content...)
	for i := range existing.links {
		existing.links[i].ContentType = a.ContentType
		existing.links[i].SizeBytes = a.SizeBytes
	}
	m.artifacts[key] = existing
}

// SeedLineageLinkForTest inserts one retained link row as historical
// migrations left it; no public writer accepts a link (component-lineage).
func SeedLineageLinkForTest(t *testing.T, st Backend, link core.LineageLink) {
	t.Helper()
	m := st.(*volatileMemory).memory
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lineage.put(link)
}

// SeedStoredTaskHoldForTest sets one task's stored historical hold to true,
// as a row written before the hold retirement would read; no public writer
// can (DEC-74(3); component-persistence).
func SeedStoredTaskHoldForTest(t *testing.T, st Backend, taskID string) {
	t.Helper()
	m := st.(*volatileMemory).memory
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		t.Fatal("seed requires an existing task")
	}
	task.Hold = true
	m.tasks[taskID] = task
}

// SeedSetupContractForTest replaces one task's setup contract with the
// decoding of raw, as the durable backends read a row an earlier encoding
// wrote; no public writer accepts a raw contract.
func SeedSetupContractForTest(t *testing.T, st Backend, taskID, raw string) {
	t.Helper()
	var setup config.ExecutionSetup
	if err := json.Unmarshal([]byte(raw), &setup); err != nil {
		t.Fatal(err)
	}
	m := st.(*volatileMemory).memory
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		t.Fatal("seed requires an existing task")
	}
	task.SetupContract = setup
	m.tasks[taskID] = task
}
