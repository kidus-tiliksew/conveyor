package postgres

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestRiskTokenHistoryUpgradeIntegration proves the operator's decision of
// review round 1 on task 261114-9d3b4c across a real upgrade to migration 167
// (req-risk-based-review AC-12.1). Claims made, released, and retried before
// the upgrade lose their ledger rows when the isolated schema returns to its
// shape before migration 167: the claim ledger table is dropped and version
// 167 leaves the migration ledger. The production migrator then applies the
// real migration file, whose backfill copies only the identities that order
// rows still hold.
func TestRiskTokenHistoryUpgradeIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	system := store.WithActor(t.Context(), store.SystemActor())
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "risk_token_upgrade_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(system, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = Migrate(system, pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace, ctx := activityValidationWorkspace(t, st, "risk-token-upgrade")
	storetest.RunRiskTokenHistoryUpgrade(t, st, ctx, workspace, func(t *testing.T) {
		t.Helper()
		for _, statement := range []string{
			`DROP TABLE work_order_claim_identities`,
			`DELETE FROM conveyor_schema_migrations WHERE version >= 167`,
		} {
			if _, err := pool.Exec(system, statement); err != nil {
				t.Fatalf("restore the schema before migration 167: %v", err)
			}
		}
		if err := Migrate(system, pool); err != nil {
			t.Fatalf("upgrade through migration 167: %v", err)
		}
		var name string
		if err := pool.QueryRow(system, `SELECT name FROM conveyor_schema_migrations WHERE version=167`).Scan(&name); err != nil || name != "167_risk_orders.sql" {
			t.Fatalf("migration 167 ledger name=%q err=%v", name, err)
		}
		var backfilled int
		if err := pool.QueryRow(system, `SELECT count(*) FROM work_order_claim_identities WHERE workspace_id=$1`, workspace).Scan(&backfilled); err != nil {
			t.Fatal(err)
		}
		// Only the retried claim's current attempt still holds its identity.
		if backfilled != 1 {
			t.Fatalf("backfilled claim identities=%d, want 1", backfilled)
		}
	})
}
