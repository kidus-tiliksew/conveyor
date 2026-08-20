package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryCheckpointDecisionRequestConformance(t *testing.T) {
	storetest.RunCheckpointDecisionRequestConformance(t, store.NewMemory(), store.WithWorkspace(t.Context(), "checkpoint-decision-memory"))
}
