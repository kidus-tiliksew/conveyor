package singlestore

import "testing"

func TestOpenTaskBranchUniqueDropsKeyIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='tasks' AND index_name='tasks_branch_key'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unique key remains count=%d err=%v", count, err)
	}
	if _, err := st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=8"); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='tasks' AND index_name='tasks_branch_key'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry unique key count=%d err=%v", count, err)
	}
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=8").Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger rows=%d err=%v", count, err)
	}
}
