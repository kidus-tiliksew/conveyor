package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryForgeAuthorIdentityConformance(t *testing.T) {
	storetest.RunForgeAuthorIdentityConformance(t, store.NewMemory(), store.WithWorkspace(t.Context(), "forge-author-memory"))
}
