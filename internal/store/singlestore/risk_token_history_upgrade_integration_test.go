package singlestore

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestRiskTokenHistoryUpgradeIntegration proves the operator's decision of
// review round 1 on task 261114-9d3b4c across a real upgrade to migration 0040
// (req-risk-based-review AC-12.1). Claims made, released, and retried before
// the upgrade lose their ledger rows when the database returns to its shape
// before migration 0040: the claim ledger table is dropped and version 40
// leaves the migration ledger. The production migrator then applies the real
// migration file, whose backfill copies only the identities that order rows
// still hold.
func TestRiskTokenHistoryUpgradeIntegration(t *testing.T) {
	st := integrationStore(t)
	system := store.WithActor(t.Context(), store.SystemActor())
	workspace := "risk-token-upgrade"
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.CreateWorkspace(ctx, workspace, "Risk token upgrade", cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.migrate(context.Background()); err != nil {
			t.Errorf("restore migration 0040: %v", err)
		}
	})
	storetest.RunRiskTokenHistoryUpgrade(t, st, ctx, workspace, func(t *testing.T) {
		t.Helper()
		for _, statement := range []string{
			`DROP TABLE work_order_claim_identities`,
			`DELETE FROM conveyor_singlestore_migrations WHERE version >= 40`,
		} {
			if _, err := st.db.ExecContext(system, statement); err != nil {
				t.Fatalf("restore the schema before migration 0040: %v", err)
			}
		}
		if err := st.migrate(system); err != nil {
			t.Fatalf("upgrade through migration 0040: %v", err)
		}
		var name string
		if err := st.db.QueryRowContext(system, `SELECT name FROM conveyor_singlestore_migrations WHERE version=40`).Scan(&name); err != nil || name != "0040_risk_orders.sql" {
			t.Fatalf("migration 0040 ledger name=%q err=%v", name, err)
		}
		var backfilled int
		if err := st.db.QueryRowContext(system, `SELECT COUNT(*) FROM work_order_claim_identities WHERE workspace_id=?`, workspace).Scan(&backfilled); err != nil {
			t.Fatal(err)
		}
		// Only the retried claim's current attempt still holds its identity.
		if backfilled != 1 {
			t.Fatalf("backfilled claim identities=%d, want 1", backfilled)
		}
	})
}
