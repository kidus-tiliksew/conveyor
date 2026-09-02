package store_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// The in-memory half of the requirement and planning-session conformance suite
// . The Postgres half runs the identical suite in
// internal/store/postgres, which is what proves the two agree.
func TestMemoryRequirementConformance(t *testing.T) {
	factory := func(t *testing.T, repos []config.Repo) storetest.RequirementFixture {
		t.Helper()
		// A fresh store and workspace per subtest, mirroring the fresh Postgres
		// workspace the integration harness provisions.
		workspace := "memory-requirements-" + core.NewTaskID()
		cfg := &config.Config{Workspace: workspace, Repos: repos}
		return storetest.RequirementFixture{
			Store:     store.NewMemoryWithConfig(cfg),
			Context:   store.WithWorkspace(t.Context(), workspace),
			Workspace: workspace,
		}
	}
	storetest.RunRequirementConformance(t, factory)
	storetest.RunVersionDismissalConformance(t, factory)
}
