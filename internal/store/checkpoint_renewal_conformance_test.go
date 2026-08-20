package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryCheckpointRenewalConformance(t *testing.T) {
	storetest.RunCheckpointRenewalConformance(t, store.NewMemory(), store.WithWorkspace(t.Context(), "checkpoint-renewal"))
}
