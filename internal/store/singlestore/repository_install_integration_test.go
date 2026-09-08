package singlestore

import (
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
)

func TestRepositoryInstallMigrationBackfillAndRetryIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	// Recreate the pre-migration shape only in this package's isolated test DB.
	for _, query := range []string{
		`DELETE FROM conveyor_singlestore_migrations WHERE version=2`,
		`ALTER TABLE repos DROP COLUMN install_conveyor`,
		`INSERT INTO repos(workspace_id,name,url,github_slug,default_base) VALUES('legacy-install','repo','https://example.test/repo','','main')`,
	} {
		if _, err := st.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := st.db.QueryRowContext(ctx, `SELECT install_conveyor FROM repos WHERE workspace_id='legacy-install' AND name='repo'`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("legacy row enabled=%v error=%v", enabled, err)
	}
	// Model a crash after DDL and a later write, before recording migration 2.
	for _, query := range []string{
		`UPDATE repos SET install_conveyor=true WHERE workspace_id='legacy-install' AND name='repo'`,
		`DELETE FROM conveyor_singlestore_migrations WHERE version=2`,
	} {
		if _, err := st.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT install_conveyor FROM repos WHERE workspace_id='legacy-install' AND name='repo'`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("retry lost explicit on: enabled=%v error=%v", enabled, err)
	}
}

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
