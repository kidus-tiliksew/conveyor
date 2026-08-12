package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresTaskContextQueueRepinConformanceIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "postgres-repin-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour},
		}},
	}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunTaskContextQueueRepinConformance(t, st, ctx, cfg)
}
