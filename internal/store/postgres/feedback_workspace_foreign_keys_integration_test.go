package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackWorkspaceTable names one feedback table that migration 181 keys to
// workspaces, an insert of one valid row, and a read of a workspace's rows.
type feedbackWorkspaceTable struct {
	name       string
	constraint string
	insert     string
	read       string
}

var feedbackWorkspaceTables = []feedbackWorkspaceTable{
	{
		name:       "feedback_comparisons",
		constraint: "feedback_comparisons_workspace_id_fkey",
		insert:     `INSERT INTO feedback_comparisons (workspace_id, repository, base_commit, head_commit, status, compared_at) VALUES ($1, $2, $3, $3, 'identical', $4)`,
		read:       `SELECT repository || ' ' || base_commit || ' ' || head_commit || ' ' || status || ' ' || compared_at::text FROM feedback_comparisons WHERE workspace_id = $1 ORDER BY repository`,
	},
	{
		name:       "feedback_admissions",
		constraint: "feedback_admissions_workspace_id_fkey",
		insert:     `INSERT INTO feedback_admissions (workspace_id, repository, base_commit, head_ref, admitted_at) VALUES ($1, $2, $3, 'main', $4)`,
		read:       `SELECT repository || ' ' || base_commit || ' ' || head_ref || ' ' || admitted_at::text FROM feedback_admissions WHERE workspace_id = $1 ORDER BY repository`,
	},
	{
		name:       "feedback_branch_heads",
		constraint: "feedback_branch_heads_workspace_id_fkey",
		insert:     `INSERT INTO feedback_branch_heads (workspace_id, repository, branch, head_commit, observed_at) VALUES ($1, $2, 'main', $3, $4)`,
		read:       `SELECT repository || ' ' || branch || ' ' || head_commit || ' ' || observed_at::text FROM feedback_branch_heads WHERE workspace_id = $1 ORDER BY repository`,
	},
}

// TestFeedbackWorkspaceForeignKeysMigrationIntegration upgrades a ledger
// from version 180 through migration 181 (component-persistence invariants
// 21 and 34 to 37): the upgrade removes only rows whose workspace does not
// exist, keeps every valid row, refuses a new row for a missing workspace in
// each table, and deletes a workspace's rows with the workspace alone.
func TestFeedbackWorkspaceForeignKeysMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered first, so the schema drop below runs
	// while the admin pool is still open.
	t.Cleanup(admin.Close)
	schema := "feedback_workspace_fk_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
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
	suffix := core.NewTaskID()
	kept, removed, orphan := "feedback-fk-kept-"+suffix, "feedback-fk-removed-"+suffix, "feedback-fk-orphan-"+suffix
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), kept)
	if err = migrateControlPlaneToVersion(ctx, pool, 180); err != nil {
		t.Fatalf("migrate to 180: %v", err)
	}
	for _, workspace := range []string{kept, removed} {
		if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a1", 20)
	// Version 180 has no key, so each table accepts the orphan row.
	for _, table := range feedbackWorkspaceTables {
		for _, workspace := range []string{kept, removed, orphan} {
			if _, err = pool.Exec(ctx, table.insert, workspace, "acme/conveyor", commit, at); err != nil {
				t.Fatalf("version 180 %s row for %s: %v", table.name, workspace, err)
			}
		}
	}
	before := map[string][]string{}
	for _, table := range feedbackWorkspaceTables {
		for _, workspace := range []string{kept, removed} {
			before[table.name+" "+workspace] = feedbackWorkspaceRows(t, ctx, pool, table, workspace)
		}
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, table := range feedbackWorkspaceTables {
		if rows := feedbackWorkspaceRows(t, ctx, pool, table, orphan); len(rows) != 0 {
			t.Fatalf("migration 181 kept the %s orphan: %v", table.name, rows)
		}
		for _, workspace := range []string{kept, removed} {
			want := before[table.name+" "+workspace]
			if got := feedbackWorkspaceRows(t, ctx, pool, table, workspace); len(want) != 1 || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s rows for %s after migration 181 = %v, want %v", table.name, workspace, got, want)
			}
		}
	}
	// Each table refuses a row for a missing workspace in its own statement,
	// so one refusal cannot hide another.
	missing := "feedback-fk-missing-" + core.NewTaskID()
	for _, table := range feedbackWorkspaceTables {
		_, err = pool.Exec(ctx, table.insert, missing, "acme/other", commit, at)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != table.constraint {
			t.Fatalf("%s row for a missing workspace err=%v, want SQLSTATE 23503 on %s", table.name, err, table.constraint)
		}
		if _, err = pool.Exec(ctx, table.insert, kept, "acme/other", commit, at); err != nil {
			t.Fatalf("%s valid row after migration 181: %v", table.name, err)
		}
	}
	// Deleting one workspace deletes its rows and no row of the other.
	if _, err = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, removed); err != nil {
		t.Fatal(err)
	}
	for _, table := range feedbackWorkspaceTables {
		if rows := feedbackWorkspaceRows(t, ctx, pool, table, removed); len(rows) != 0 {
			t.Fatalf("%s kept rows of a deleted workspace: %v", table.name, rows)
		}
		if rows := feedbackWorkspaceRows(t, ctx, pool, table, kept); len(rows) != 2 {
			t.Fatalf("%s rows of the kept workspace after a delete = %v, want 2", table.name, rows)
		}
	}
	// Reopening replays the ledger without touching data.
	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, table := range feedbackWorkspaceTables {
		if rows := feedbackWorkspaceRows(t, ctx, pool, table, kept); len(rows) != 2 {
			t.Fatalf("%s rows after a replay = %v, want 2", table.name, rows)
		}
	}
}

// feedbackWorkspaceRows reads one workspace's rows of a table as text and
// fails the test on any query, scan, or iteration error.
func feedbackWorkspaceRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table feedbackWorkspaceTable, workspace string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, table.read, workspace)
	if err != nil {
		t.Fatalf("read %s for %s: %v", table.name, workspace, err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s for %s: %v", table.name, workspace, err)
	}
	return got
}
