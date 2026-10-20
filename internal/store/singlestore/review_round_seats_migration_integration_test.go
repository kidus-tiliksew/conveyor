package singlestore

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestReviewRoundSeatsMigrationIntegration replays migration 0024
// (feature-review-seat-minimum RS-2) over populated rows from the previous
// ledger version and from a partial DDL state where only one nullable column
// and one table exist. The columns arrive NULL with no backfill, the store
// validates records because SingleStore enforces no CHECK, the keys refuse
// duplicates, and the read projections return seeded counts.
func TestReviewRoundSeatsMigrationIntegration(t *testing.T) {
	st := integrationStore(t)
	base := store.WithActor(t.Context(), store.SystemActor())
	const ws = "review-seats"
	ctx := store.WithWorkspace(base, ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	implement := core.WorkOrder{ID: task.ID + "-implement-1", JobID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, CreatedAt: now}
	if err = st.CreateJob(ctx, core.Job{ID: implement.JobID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.CreateWorkOrder(ctx, st, implement); err != nil {
		t.Fatal(err)
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(base, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	columnExists := func(table, column string) bool {
		t.Helper()
		exists, err := st.columnExists(base, table, column)
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}
	tableExists := func(table string) bool {
		t.Helper()
		var count int
		if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}
	assertMigrated := func(label string) {
		t.Helper()
		var ledger int
		if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=24`).Scan(&ledger); err != nil || ledger != 1 {
			t.Fatalf("%s: ledger rows for 0024=%d err=%v", label, ledger, err)
		}
		for _, column := range [][2]string{{"work_orders", "declared_review_seats"}, {"task_specs", "review_seats"}} {
			if !columnExists(column[0], column[1]) {
				t.Fatalf("%s: %s.%s missing", label, column[0], column[1])
			}
		}
		for _, table := range []string{"review_round_seats", "review_seat_additions"} {
			if !tableExists(table) {
				t.Fatalf("%s: table %s missing", label, table)
			}
		}
		order, err := st.GetWorkOrder(ctx, implement.ID)
		if err != nil || order.DeclaredReviewSeats != nil {
			t.Fatalf("%s: implement order=%+v err=%v", label, order, err)
		}
		approved, ok, err := st.GetApprovedSpecVersion(ctx, task.ID)
		if err != nil || !ok || approved.ReviewSeats != nil {
			t.Fatalf("%s: plan=%+v ok=%t err=%v", label, approved, ok, err)
		}
		records, err := st.ListReviewRoundSeats(ctx, task.ID)
		if err != nil || len(records) != 0 {
			t.Fatalf("%s: records=%+v err=%v", label, records, err)
		}
	}
	assertMigrated("fresh")

	// The previous ledger version: neither table nor column exists.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=24`)
	exec(`DROP TABLE IF EXISTS review_round_seats`)
	exec(`DROP TABLE IF EXISTS review_seat_additions`)
	exec(`ALTER TABLE work_orders DROP COLUMN declared_review_seats`)
	exec(`ALTER TABLE task_specs DROP COLUMN review_seats`)
	if err = st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated("upgrade")

	// A restart after partial DDL: only one column and one table exist.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=24`)
	exec(`DROP TABLE IF EXISTS review_seat_additions`)
	exec(`ALTER TABLE task_specs DROP COLUMN review_seats`)
	if !columnExists("work_orders", "declared_review_seats") || columnExists("task_specs", "review_seats") || !tableExists("review_round_seats") || tableExists("review_seat_additions") {
		t.Fatal("partial DDL fixture is not partial")
	}
	if err = st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated("partial restart")
	if err = st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated("replay")

	// SingleStore enforces no CHECK, so the store validates before insert.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := store.ReviewRoundSeats{TaskID: task.ID, ReviewRound: 1, SeatCount: 0, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1, CreatedAt: now}
	if err = insertReviewRoundSeatsTx(ctx, tx, invalid); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
		t.Fatalf("invalid record error=%v", err)
	}
	invalid.SeatCount, invalid.SeatCountSource = 2, "workspace"
	if err = insertReviewRoundSeatsTx(ctx, tx, invalid); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
		t.Fatalf("unknown source error=%v", err)
	}
	valid := store.ReviewRoundSeats{TaskID: task.ID, ReviewRound: 1, SeatCount: 3, SeatCountSource: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2, ImplementationWorkOrderID: implement.ID, CreatedAt: now}
	if err = insertReviewRoundSeatsTx(ctx, tx, valid); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(base, `INSERT INTO review_round_seats(workspace_id,task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at) VALUES(?,?,1,2,'minimum',2,'',?)`, ws, task.ID, now); err == nil {
		t.Fatal("duplicate round record was accepted")
	}
	addition := `INSERT INTO review_seat_additions(workspace_id,request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at) VALUES(?,?,?,1,3,4,'reason','operator',?)`
	exec(addition, ws, "add-1", task.ID, now)
	if _, err = st.db.ExecContext(base, addition, ws, "add-1", task.ID, now); err == nil {
		t.Fatal("duplicate addition request was accepted")
	}

	// Read projections return seeded counts; RS-4 and RS-3 own the writers.
	exec(`UPDATE work_orders SET declared_review_seats=3 WHERE workspace_id=? AND id=?`, ws, implement.ID)
	exec(`UPDATE task_specs SET review_seats=4 WHERE workspace_id=? AND task_id=?`, ws, task.ID)
	order, err := st.GetWorkOrder(ctx, implement.ID)
	if err != nil || order.DeclaredReviewSeats == nil || *order.DeclaredReviewSeats != 3 {
		t.Fatalf("declared projection=%v err=%v", order.DeclaredReviewSeats, err)
	}
	listed, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(listed) != 1 || listed[0].DeclaredReviewSeats == nil || *listed[0].DeclaredReviewSeats != 3 {
		t.Fatalf("listed declared projection=%+v err=%v", listed, err)
	}
	approved, ok, err := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || !ok || approved.ReviewSeats == nil || *approved.ReviewSeats != 4 {
		t.Fatalf("planned projection=%+v ok=%t err=%v", approved, ok, err)
	}
	records, err := st.ListReviewRoundSeats(ctx, task.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v, want %+v", records, err, valid)
	}
	got := records[0]
	if !got.CreatedAt.Equal(valid.CreatedAt) {
		t.Fatalf("record created_at=%s, want %s", got.CreatedAt, valid.CreatedAt)
	}
	got.CreatedAt = valid.CreatedAt
	if got != valid {
		t.Fatalf("record=%+v, want %+v", got, valid)
	}
	if foreign, err := st.ListReviewRoundSeats(store.WithWorkspace(base, ws+"-foreign"), task.ID); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign workspace records=%+v err=%v", foreign, err)
	}
}
