package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresConformanceIntegration(t *testing.T) {
	integrationDatabaseURL(t)
	storetest.RunAll(t, storetest.Factory{
		ProductionCapable: true,
		Capabilities:      storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			t.Helper()
			// The guarded integration helper isolates deployment-level identity
			// as well as workspace data with a new schema for each fixture.
			st := newIdentityIntegrationStore(t, 0)
			workspace := "conformance-" + core.NewTaskID()
			ctx := store.WithWorkspace(t.Context(), workspace)
			cfg := &config.Config{Workspace: workspace, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg, SeedLegacy: postgresConformanceLegacySeed(st, ctx, workspace)}
		},
	})
}
