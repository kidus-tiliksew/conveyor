package dispatch

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

// Historical oversize rows must remain readable, but normal store writes now
// reject them. This read fixture supplies legacy bytes to the provider-budget
// tests without weakening ordinary artifact validation.
type historicalArtifactStore struct {
	store.Store
	oversized map[string][]byte
}

func newHistoricalArtifactStore() *historicalArtifactStore {
	return &historicalArtifactStore{Store: store.NewMemory(), oversized: map[string][]byte{}}
}
func (s *historicalArtifactStore) CreateArtifact(ctx context.Context, a core.Artifact, b []byte) (core.Artifact, error) {
	if len(b) <= core.MaxArtifactBytes {
		return s.Store.CreateArtifact(ctx, a, b)
	}
	stored, err := s.Store.CreateArtifact(ctx, a, b[:core.MaxArtifactBytes])
	if err != nil {
		return stored, err
	}
	s.oversized[stored.ID] = b
	stored.SizeBytes = int64(len(b))
	return stored, nil
}
func (s *historicalArtifactStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	a, b, err := s.Store.GetArtifact(ctx, id)
	if legacy, ok := s.oversized[id]; ok {
		b = legacy
		a.SizeBytes = int64(len(b))
	}
	return a, b, err
}
func (s *historicalArtifactStore) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	a, err := s.Store.ListArtifacts(ctx)
	s.project(a)
	return a, err
}
func (s *historicalArtifactStore) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	a, err := s.Store.ListArtifactsForLineage(ctx, nodes)
	s.project(a)
	return a, err
}
func (s *historicalArtifactStore) project(artifacts []core.Artifact) {
	for i, a := range artifacts {
		if b, ok := s.oversized[a.ID]; ok {
			artifacts[i].SizeBytes = int64(len(b))
		}
	}
}
func paddedArtifactPNG(seed, size int) []byte {
	b := make([]byte, size)
	copy(b, testimage.PNG(fmt.Sprint(seed)))
	return b
}
