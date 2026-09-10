package singlestore

import (
	"bytes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"testing"
)

func TestWorkspaceGitHubAppEncryptedLedgerIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "app-ledger-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{Workspace: workspace}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunGitHubAppConformance(t, storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg})
	owner, err := st.VerifyPersonalAccessToken(ctx, "conformance-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT kind,actor_id,payload_json FROM events WHERE workspace_id=? AND kind LIKE 'workspace.github_app_%' ORDER BY id`, workspace)
	if err != nil {
		t.Fatal(err)
	}
	var events []core.Event
	for rows.Next() {
		var e core.Event
		if err := rows.Scan(&e.Kind, &e.ActorID, &e.Payload); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	storetest.AssertGitHubAppEvents(t, events, workspace, store.UserActorID(owner.ID))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "cipher-fixture", ClientID: "client"}, PrivateKey: "database-encrypted-app-private-material"}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, workspace, app); err != nil {
		t.Fatal(err)
	}
	var nonce, ciphertext []byte
	if err = st.db.QueryRowContext(ctx, `SELECT private_key_nonce,private_key_ciphertext FROM workspace_github_apps WHERE workspace_id=?`, workspace).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || bytes.Contains(ciphertext, []byte(app.PrivateKey)) {
		t.Fatal("app row is not encrypted")
	}
	// Replaying ciphertext in another workspace must fail authentication.
	other := workspace + "-other"
	if _, err = st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, other), &config.Config{Workspace: other}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, other, app); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, `UPDATE workspace_github_apps SET private_key_nonce=?,private_key_ciphertext=? WHERE workspace_id=?`, nonce, ciphertext, other); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, other); err == nil {
		t.Fatal("cross-workspace ciphertext replay accepted")
	}
}
func TestWorkspaceGitHubAppMigrationGuard(t *testing.T) {
	content, err := migrationFiles.ReadFile("migrations/0003_workspace_github_apps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("CREATE ROWSTORE TABLE IF NOT EXISTS workspace_github_apps")) {
		t.Fatal("restart-safe app migration absent")
	}
	files, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, file := range files {
		if file.version == 3 {
			found = true
			if file.name != "0003_workspace_github_apps.sql" {
				t.Fatal("app migration version reused")
			}
		}
	}
	if !found {
		t.Fatal("app migration missing")
	}

}
