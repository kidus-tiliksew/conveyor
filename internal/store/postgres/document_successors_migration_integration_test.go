package postgres

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestDocumentSuccessorMigrationsAdvanceCanonicalVersion118Integration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 118)
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 120); err != nil {
		t.Fatalf("advance canonical version-118 history: %v", err)
	}
	assertDocumentSuccessorMigrationHead(t, st)
}

func TestDocumentSuccessorMigrationsPreserveHistoricalArchivalVersion118Integration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 117)
	workspace := "archive-v118-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		ALTER TABLE requirements
			ADD COLUMN archived_at timestamptz,
			ADD COLUMN archived_by text NOT NULL DEFAULT '',
			ADD COLUMN superseding_document_ids text[] NOT NULL DEFAULT '{}';
		ALTER TABLE system_designs
			ADD COLUMN archived_at timestamptz,
			ADD COLUMN archived_by text NOT NULL DEFAULT '',
			ADD COLUMN superseding_document_ids text[] NOT NULL DEFAULT '{}'`); err != nil {
		t.Fatalf("seed historical archival columns: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO requirements(workspace_id,id,slug,title,archived_at,superseding_document_ids)
		VALUES($1,'req-legacy-archive','req-legacy-archive','Legacy requirement',now(),ARRAY['req-current','design-current'])`, workspace); err != nil {
		t.Fatalf("seed historical archived requirement: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO system_designs(workspace_id,id,slug,title,category,created_at,updated_at,archived_at,superseding_document_ids)
		VALUES($1,'design-legacy-archive','design-legacy-archive','Legacy design','Architecture',now(),now(),now(),ARRAY['design-current','req-current'])`, workspace); err != nil {
		t.Fatalf("seed historical archived design: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO conveyor_schema_migrations(version,name,checksum) VALUES(118,$1,$2)`, historicalArchivalMigrationName, historicalArchivalMigrationChecksum); err != nil {
		t.Fatalf("seed historical archival version-118 ledger: %v", err)
	}

	if err := migrateControlPlaneToVersion(ctx, st.pool, 120); err != nil {
		t.Fatalf("advance historical archival version-118 history: %v", err)
	}
	assertDocumentSuccessorMigrationHead(t, st)

	var requirementSuccessors, designSuccessors []string
	if err := st.pool.QueryRow(ctx, `SELECT superseded_by FROM requirements WHERE workspace_id=$1 AND id='req-legacy-archive'`, workspace).Scan(&requirementSuccessors); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT superseded_by FROM system_designs WHERE workspace_id=$1 AND id='design-legacy-archive'`, workspace).Scan(&designSuccessors); err != nil {
		t.Fatal(err)
	}
	if got := len(requirementSuccessors); got != 2 || requirementSuccessors[0] != "req-current" || requirementSuccessors[1] != "design-current" {
		t.Fatalf("requirement successors=%v", requirementSuccessors)
	}
	if got := len(designSuccessors); got != 2 || designSuccessors[0] != "design-current" || designSuccessors[1] != "req-current" {
		t.Fatalf("design successors=%v", designSuccessors)
	}
	var appliedName, appliedChecksum string
	if err := st.pool.QueryRow(ctx, `SELECT name,checksum FROM conveyor_schema_migrations WHERE version=118`).Scan(&appliedName, &appliedChecksum); err != nil {
		t.Fatal(err)
	}
	if appliedName != historicalArchivalMigrationName || appliedChecksum != historicalArchivalMigrationChecksum {
		t.Fatalf("historical ledger row was rewritten: name=%q checksum=%q", appliedName, appliedChecksum)
	}
}

func assertDocumentSuccessorMigrationHead(t *testing.T, st *Store) {
	t.Helper()
	var version, dependencyTables, canonicalColumns, predecessorColumns int
	if err := st.pool.QueryRow(t.Context(), `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='task_dependency_additions'`).Scan(&dependencyTables); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('requirements','system_designs') AND column_name='superseded_by'`).Scan(&canonicalColumns); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('requirements','system_designs') AND column_name='superseding_document_ids'`).Scan(&predecessorColumns); err != nil {
		t.Fatal(err)
	}
	if version != 120 || dependencyTables != 1 || canonicalColumns != 2 || predecessorColumns != 0 {
		t.Fatalf("migration head version=%d dependency_tables=%d canonical_columns=%d predecessor_columns=%d", version, dependencyTables, canonicalColumns, predecessorColumns)
	}
}
