package store

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

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
