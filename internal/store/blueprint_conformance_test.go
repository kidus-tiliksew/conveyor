package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryBlueprintConformance(t *testing.T) {
	storetest.RunBlueprintConformance(t, func(t *testing.T, repos []config.Repo) storetest.BlueprintFixture {
		t.Helper()
		workspace := "memory-blueprint"
		cfg := &config.Config{Workspace: workspace, Repos: repos}
		return storetest.BlueprintFixture{
			Store: store.NewMemoryWithConfig(cfg), Context: store.WithWorkspace(t.Context(), workspace), Workspace: workspace,
		}
	})
}
