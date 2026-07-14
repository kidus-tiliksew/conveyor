package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestMigrationVersion(t *testing.T) {
	t.Parallel()
	version, err := migrationVersion("migrations/001_phase2.sql")
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("version = %d", version)
	}
	version, err = migrationVersion("migrations/002_credential_leases.sql")
	if err != nil || version != 2 {
		t.Fatalf("second migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/003_config_capacity_ownership.sql")
	if err != nil || version != 3 {
		t.Fatalf("third migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/004_activity_indexes.sql")
	if err != nil || version != 4 {
		t.Fatalf("fourth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/005_credential_lease_task.sql")
	if err != nil || version != 5 {
		t.Fatalf("fifth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/006_phase3_pipeline.sql")
	if err != nil || version != 6 {
		t.Fatalf("sixth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/007_durable_pipeline_transition.sql")
	if err != nil || version != 7 {
		t.Fatalf("seventh migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/008_dynamic_workspace_config.sql")
	if err != nil || version != 8 {
		t.Fatalf("eighth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/009_phase47_mcp.sql")
	if err != nil || version != 9 {
		t.Fatalf("ninth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/010_mcp_task_intake.sql")
	if err != nil || version != 10 {
		t.Fatalf("tenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/011_remove_job_budgets.sql")
	if err != nil || version != 11 {
		t.Fatalf("eleventh migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/012_review_publications.sql")
	if err != nil || version != 12 {
		t.Fatalf("twelfth migration version = %d, err=%v", version, err)
	}
	for _, name := range []string{"migration.sql", "zero_phase.sql", "000_phase.sql"} {
		if _, err := migrationVersion(name); err == nil {
			t.Errorf("migrationVersion(%q) succeeded", name)
		}
	}
}

func TestConfigDiffIgnoresRuntimeParsedTimeout(t *testing.T) {
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Model: "operator", Execution: config.ExecutionMCP, TimeoutText: "2h", Timeout: 2 * time.Hour},
	}}}
	document := cfg.WorkspaceDocument()
	if sections := configDiff(document, document); len(sections) != 0 {
		t.Fatalf("unchanged config diff = %v", sections)
	}
}
