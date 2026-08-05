package postgres

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestPostgresSystemDesignDriftConformanceIntegration(t *testing.T) {
	storetest.RunSystemDesignDriftConformance(t, func(t *testing.T) (store.Store, context.Context, string) {
		st, err := Open(t.Context(), integrationDatabaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		workspace := "postgres-drift-" + core.NewTaskID()
		ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
		cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
		if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		return st, ctx, workspace
	})
}
