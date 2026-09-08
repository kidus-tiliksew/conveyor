package postgres

import (
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
)

func TestRepositoryInstallColumnAndBackfillIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 121)
	ctx := store.WithWorkspace(t.Context(), "install-backfill")
	legacy := "workspace: install-backfill\nrepos:\n  - name: repo\n    url: https://example.test/repo\n    base: main\n"
	if _, err := st.pool.Exec(ctx, `INSERT INTO workspaces(id,name,config_yaml) VALUES($1,$1,$2)`, "install-backfill", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repos(workspace_id,name,url,default_base) VALUES($1,'repo','https://example.test/repo','main')`, "install-backfill"); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(ctx, st.pool, 122); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := st.pool.QueryRow(ctx, `SELECT install_conveyor FROM repos WHERE workspace_id=$1 AND name='repo'`, "install-backfill").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("migration enabled existing repository")
	}
	doc, err := st.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Document.Repos[0].InstallEnabled() {
		t.Fatal("legacy YAML read enabled existing repository")
	}
	for _, choice := range []bool{true, false} {
		cfg := &config.Config{Workspace: "install-backfill", Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main", InstallConveyor: config.InstallSwitch(choice)}}}
		receipt, err := st.UpdateWorkspaceConfig(ctx, doc.Version, cfg)
		if err != nil {
			t.Fatal(err)
		}
		doc = receipt.VersionedDocument
		if err = st.pool.QueryRow(ctx, `SELECT install_conveyor FROM repos WHERE workspace_id=$1 AND name='repo'`, "install-backfill").Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != choice {
			t.Fatal("repository column did not round trip")
		}
	}
}
