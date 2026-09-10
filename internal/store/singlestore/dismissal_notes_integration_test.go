package singlestore

import "testing"

func TestDismissalNoteMigrationRetriesPartialDDLIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	// This fixture owns the isolated test database. Model a crash after the
	// first column committed but before the second and the ledger were written.
	if _, err := st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=4"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "ALTER TABLE system_design_versions DROP COLUMN dismissal_note"); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requirement_versions", "system_design_versions"} {
		var count int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name='dismissal_note' AND is_nullable='YES'`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s nullable columns=%d err=%v", table, count, err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=4").Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger rows=%d err=%v", count, err)
	}
}
