package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemorySystemDesignProposalConformance(t *testing.T) {
	storetest.RunSystemDesignProposalConformance(t, store.NewMemory(), store.WithWorkspace(t.Context(), "test"))
}
