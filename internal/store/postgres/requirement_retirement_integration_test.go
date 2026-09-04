package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMigration094RetiresSupersededRequirementVersionsIdempotentlyIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "requirement_retirement_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateControlPlaneToVersion(t.Context(), pool, 92); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "requirement-retirement-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if _, err = pool.Exec(ctx, `INSERT INTO requirements
		(workspace_id,id,slug,title,current_version,statement_high_water_mark,created_at,updated_at)
		VALUES ($1,'req-260811-0ee057','zombie-fixture','Zombie fixture',NULL,1,$2,$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO requirement_versions
		(workspace_id,requirement_id,version,content,statements_json,origin,confirmed,confirmed_by,confirmed_at,created_at)
		VALUES
		($1,'req-260811-0ee057',2,'known zombie','[{"id":"REQ-1","statement":"Keep history."}]','operator',false,'',NULL,$2),
		($1,'req-260811-0ee057',4,'confirmed successor','[{"id":"REQ-1","statement":"Keep history current."}]','operator',true,'operator',$2,$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE requirements SET current_version=4 WHERE workspace_id=$1 AND id='req-260811-0ee057'`, workspace); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlaneToVersion(t.Context(), pool, 0); err != nil {
		t.Fatal(err)
	}
	var retired, confirmed bool
	var retiredBy, content string
	var retiredByVersion int
	var retiredAt time.Time
	if err = pool.QueryRow(ctx, `SELECT retired,confirmed,retired_by,retired_at,retired_by_version,content
		FROM requirement_versions WHERE workspace_id=$1 AND requirement_id='req-260811-0ee057' AND version=2`, workspace).
		Scan(&retired, &confirmed, &retiredBy, &retiredAt, &retiredByVersion, &content); err != nil {
		t.Fatal(err)
	}
	if !retired || confirmed || retiredBy != "migration-094" || retiredAt.IsZero() || retiredByVersion != 4 || content != "known zombie" {
		t.Fatalf("repaired version retired=%v confirmed=%v retired_by=%q retired_at=%s retired_by_version=%d content=%q", retired, confirmed, retiredBy, retiredAt, retiredByVersion, content)
	}
	if err = pool.QueryRow(ctx, `SELECT retired FROM requirement_versions
		WHERE workspace_id=$1 AND requirement_id='req-260811-0ee057' AND version=4`, workspace).Scan(&retired); err != nil || retired {
		t.Fatalf("current version retired=%v err=%v", retired, err)
	}
	items, err := st.ListPendingProposals(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("pending proposals after repair=%+v err=%v", items, err)
	}
	var eventsBefore int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1
		AND kind='requirement.version_retired' AND payload_json->>'requirement_id'='req-260811-0ee057'`, workspace).Scan(&eventsBefore); err != nil || eventsBefore != 1 {
		t.Fatalf("retirement events=%d err=%v", eventsBefore, err)
	}
	if err = migrateControlPlaneToVersion(t.Context(), pool, 0); err != nil {
		t.Fatal(err)
	}
	var eventsAfter int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1
		AND kind='requirement.version_retired' AND payload_json->>'requirement_id'='req-260811-0ee057'`, workspace).Scan(&eventsAfter); err != nil || eventsAfter != eventsBefore {
		t.Fatalf("retirement events after rerun=%d err=%v, want %d", eventsAfter, err, eventsBefore)
	}
}
