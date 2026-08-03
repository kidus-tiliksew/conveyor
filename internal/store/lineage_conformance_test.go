package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestMemoryLineageConformance(t *testing.T) {
	storetest.RunLineageConformance(t, func(t *testing.T, repos []config.Repo) storetest.LineageFixture {
		workspace := "lineage-" + core.NewTaskID()
		st := store.NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: repos})
		return storetest.LineageFixture{Store: st, Context: store.WithWorkspace(t.Context(), workspace), Workspace: workspace}
	})
}
