package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestReviewRoundSeatsMigrationIntegration upgrades a populated ledger from
// version 148 through migration 149 (feature-review-seat-minimum RS-2): the
// nullable columns arrive NULL with no backfill, the two tables enforce their
// keys and checks, the read projections return seeded counts, and the
// migration replays cleanly.
func TestReviewRoundSeatsMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "review_seats_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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
	workspace := "review-seats-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if err = migrateControlPlaneToVersion(ctx, pool, 148); err != nil {
		t.Fatalf("migrate to 148: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if _, err = pool.Exec(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval)
		VALUES ($1,$2,'test','review seats','','','L2','conveyor','main',$3,'running',$4,'',true,true)`, taskID, workspace, "conveyor/task-"+taskID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO task_specs (task_id,version,content,acceptance_count,acceptance,decomposition,approved,approved_at,created_at,agent,model)
		VALUES ($1,1,'plan',0,'[]','[]',true,$2,$2,'','')`, taskID, now); err != nil {
		t.Fatal(err)
	}
	legacy := newStore(pool)
	jobID := taskID + "-implement-1"
	if err = legacy.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobDone, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO work_orders (id,workspace_id,task_id,job_id,stage,state,queue_entered_at,queue_deadline,created_at,updated_at)
		VALUES ($1,$2,$3,$1,'implement','submitted',$4,$5,$4,$4)`, jobID, workspace, taskID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != embeddedMigrationHead(t) {
		t.Fatalf("schema version=%d, want embedded head %d", version, embeddedMigrationHead(t))
	}
	st := newStore(pool)
	assertNoBackfill := func(label string) {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, jobID)
		if err != nil || order.DeclaredReviewSeats != nil {
			t.Fatalf("%s: implement order=%+v err=%v", label, order, err)
		}
		spec, ok, err := st.GetSpecVersion(ctx, taskID, 1)
		if err != nil || !ok || spec.ReviewSeats != nil {
			t.Fatalf("%s: plan=%+v ok=%t err=%v", label, spec, ok, err)
		}
		records, err := st.ListReviewRoundSeats(ctx, taskID)
		if err != nil || len(records) != 0 {
			t.Fatalf("%s: records=%+v err=%v", label, records, err)
		}
		var additions int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM review_seat_additions`).Scan(&additions); err != nil || additions != 0 {
			t.Fatalf("%s: additions=%d err=%v", label, additions, err)
		}
	}
	assertNoBackfill("upgrade")

	refused := func(label, statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err == nil {
			t.Fatalf("%s was accepted", label)
		}
	}
	insertRecord := `INSERT INTO review_round_seats (workspace_id,task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	refused("zero seat count", insertRecord, workspace, taskID, 1, 0, "minimum", 1, "", now)
	refused("unknown source", insertRecord, workspace, taskID, 1, 2, "workspace", 1, "", now)
	refused("zero minimum", insertRecord, workspace, taskID, 1, 2, "minimum", 0, "", now)
	refused("unknown task", insertRecord, workspace, "missing-task", 1, 2, "minimum", 2, "", now)
	if _, err = pool.Exec(ctx, insertRecord, workspace, taskID, 1, 3, "declared", 2, jobID, now); err != nil {
		t.Fatal(err)
	}
	refused("duplicate round", insertRecord, workspace, taskID, 1, 2, "minimum", 2, "", now)
	insertAddition := `INSERT INTO review_seat_additions (workspace_id,request_id,task_id,review_round,prior_seat_count,seat_count,reason,actor_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,'reason','operator',$7)`
	refused("non-increasing addition", insertAddition, workspace, "add-1", taskID, 1, 3, 3, now)
	if _, err = pool.Exec(ctx, insertAddition, workspace, "add-1", taskID, 1, 3, 4, now); err != nil {
		t.Fatal(err)
	}
	refused("duplicate addition request", insertAddition, workspace, "add-1", taskID, 1, 4, 5, now)
	refused("zero declared count", `UPDATE work_orders SET declared_review_seats=0 WHERE workspace_id=$1 AND id=$2`, workspace, jobID)
	refused("zero planned count", `UPDATE task_specs SET review_seats=0 WHERE task_id=$1`, taskID)

	// Read projections return seeded counts; RS-4 and RS-3 own the writers.
	if _, err = pool.Exec(ctx, `UPDATE work_orders SET declared_review_seats=3 WHERE workspace_id=$1 AND id=$2`, workspace, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE task_specs SET review_seats=4 WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	order, err := st.GetWorkOrder(ctx, jobID)
	if err != nil || order.DeclaredReviewSeats == nil || *order.DeclaredReviewSeats != 3 {
		t.Fatalf("declared projection=%+v err=%v", order.DeclaredReviewSeats, err)
	}
	listed, err := st.ListTaskWorkOrders(ctx, taskID)
	if err != nil || len(listed) != 1 || listed[0].DeclaredReviewSeats == nil || *listed[0].DeclaredReviewSeats != 3 {
		t.Fatalf("listed declared projection=%+v err=%v", listed, err)
	}
	approved, ok, err := st.GetApprovedSpecVersion(ctx, taskID)
	if err != nil || !ok || approved.ReviewSeats == nil || *approved.ReviewSeats != 4 {
		t.Fatalf("planned projection=%+v ok=%t err=%v", approved, ok, err)
	}
	records, err := st.ListReviewRoundSeats(ctx, taskID)
	if err != nil || len(records) != 1 || records[0].SeatCount != 3 || records[0].SeatCountSource != store.ReviewSeatSourceDeclared || records[0].ImplementationWorkOrderID != jobID {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if foreign, err := st.ListReviewRoundSeats(store.WithWorkspace(ctx, workspace+"-foreign"), taskID); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign workspace records=%+v err=%v", foreign, err)
	}

	// Reopening replays the ledger without touching data.
	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if again, err := st.ListReviewRoundSeats(ctx, taskID); err != nil || len(again) != 1 {
		t.Fatalf("replayed records=%+v err=%v", again, err)
	}
}
