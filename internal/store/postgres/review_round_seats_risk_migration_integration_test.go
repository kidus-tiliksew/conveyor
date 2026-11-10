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

// TestReviewRoundSeatsRiskSourceMigrationIntegration upgrades a ledger from
// version 162 through migration 163 (req-risk-based-review AC-7.1;
// feature-risk-based-review RR-5): before it the column refuses the source
// risk, after it a round records risk, an unknown source is still refused, a
// recorded source survives, and the migration replays cleanly.
func TestReviewRoundSeatsRiskSourceMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "review_seats_risk_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	workspace := "review-seats-risk-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if err = migrateControlPlaneToVersion(ctx, pool, 162); err != nil {
		t.Fatalf("migrate to 162: %v", err)
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
		VALUES ($1,$2,'test','risk seats','','','L2','conveyor','main',$3,'running',$4,'',true,true)`, taskID, workspace, "conveyor/task-"+taskID, now); err != nil {
		t.Fatal(err)
	}
	insertRecord := `INSERT INTO review_round_seats (workspace_id,task_id,review_round,seat_count,seat_count_source,minimum_seat_count,implementation_work_order_id,created_at) VALUES ($1,$2,$3,$4,$5,$6,'',$7)`
	if _, err = pool.Exec(ctx, insertRecord, workspace, taskID, 1, 1, "risk", 1, now); err == nil {
		t.Fatal("version 162 accepted the risk source")
	}
	if _, err = pool.Exec(ctx, insertRecord, workspace, taskID, 1, 1, "minimum", 1, now); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, insertRecord, workspace, taskID, 2, 2, "risk", 1, now); err != nil {
		t.Fatalf("risk source after migration 163: %v", err)
	}
	if _, err = pool.Exec(ctx, insertRecord, workspace, taskID, 3, 2, "workspace", 1, now); err == nil {
		t.Fatal("an unknown source was accepted after migration 163")
	}
	st := newStore(pool)
	records, err := st.ListReviewRoundSeats(ctx, taskID)
	if err != nil || len(records) != 2 || records[0].SeatCountSource != store.ReviewSeatSourceMinimum || records[1].SeatCountSource != store.ReviewSeatSourceRisk {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	// Reopening replays the ledger without touching data.
	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if again, err := st.ListReviewRoundSeats(ctx, taskID); err != nil || len(again) != 2 {
		t.Fatalf("replayed records=%+v err=%v", again, err)
	}
}
