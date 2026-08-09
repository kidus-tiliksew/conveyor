package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryPlanRevisionConformance(t *testing.T) {
	storetest.RunPlanRevisionConformance(t, store.NewMemory(), store.WithWorkspace(t.Context(), "test"))
}
