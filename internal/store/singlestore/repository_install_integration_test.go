package singlestore

import (
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
)

func TestRepositoryInstallColumnIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(t.Context(), "install-columns")
	cfg := &config.Config{Workspace: "install-columns", Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []bool{true, false} {
		doc, err := st.WorkspaceConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Repos[0].InstallConveyor = config.InstallSwitch(choice)
		if _, err = st.UpdateWorkspaceConfig(ctx, doc.Version, cfg); err != nil {
			t.Fatal(err)
		}
		var enabled bool
		if err = st.db.QueryRowContext(ctx, `SELECT install_conveyor FROM repos WHERE workspace_id=? AND name='repo'`, "install-columns").Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != choice {
			t.Fatal("repository column did not round trip")
		}
	}
}
