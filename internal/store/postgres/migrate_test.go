package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
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
	version, err = migrationVersion("migrations/013_work_order_clocks.sql")
	if err != nil || version != 13 {
		t.Fatalf("thirteenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/014_multi_workspace.sql")
	if err != nil || version != 14 {
		t.Fatalf("fourteenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/015_phase51_worker.sql")
	if err != nil || version != 15 {
		t.Fatalf("fifteenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/016_review_panel.sql")
	if err != nil || version != 16 {
		t.Fatalf("sixteenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/017_review_harness_snapshot.sql")
	if err != nil || version != 17 {
		t.Fatalf("seventeenth migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/023_worker_recovery.sql")
	if err != nil || version != 23 {
		t.Fatalf("worker recovery migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/024_review_round_retry.sql")
	if err != nil || version != 24 {
		t.Fatalf("review round retry migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/025_interrupted_review_recovery.sql")
	if err != nil || version != 25 {
		t.Fatalf("interrupted review recovery migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/026_remove_inprocess_cost.sql")
	if err != nil || version != 26 {
		t.Fatalf("in-process cost removal migration version = %d, err=%v", version, err)
	}
	version, err = migrationVersion("migrations/033_child_failure_details.sql")
	if err != nil || version != 33 {
		t.Fatalf("child failure detail version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/036_forge_error_categories.sql")
	if err != nil || version != 36 {
		t.Fatalf("forge error categories version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/037_cancel_intervention_action.sql")
	if err != nil || version != 37 {
		t.Fatalf("cancel intervention action version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/038_rate_limit_telemetry.sql")
	if err != nil || version != 38 {
		t.Fatalf("rate limit telemetry version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/040_monitor_and_drift.sql")
	if err != nil || version != 40 {
		t.Fatalf("monitor and drift version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/041_blueprint_dependencies.sql")
	if err != nil || version != 41 {
		t.Fatalf("blueprint dependency version=%d err=%v", version, err)
	}
	version, err = migrationVersion("migrations/042_workspace_blueprint_parent_fk.sql")
	if err != nil || version != 42 {
		t.Fatalf("workspace blueprint parent version=%d err=%v", version, err)
	}
	for _, name := range []string{"migration.sql", "zero_phase.sql", "000_phase.sql"} {
		if _, err := migrationVersion(name); err == nil {
			t.Errorf("migrationVersion(%q) succeeded", name)
		}
	}
}

func TestCanonicalStateMigrationRendersFromCoreStateSets(t *testing.T) {
	raw, err := migrationFiles.ReadFile("migrations/035_canonical_lifecycle_states.sql")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderMigration(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, "state IN ("+quotedTaskStates()+")") {
		t.Fatalf("task constraint is not core-derived: %s", text)
	}
	if !strings.Contains(text, "state IN ("+quotedWorkOrderStates()+")") {
		t.Fatalf("work-order constraint is not core-derived: %s", text)
	}
	if migrationChecksum(raw) == migrationChecksum(rendered) {
		t.Fatal("raw template and rendered migration unexpectedly share a checksum")
	}
}

func TestInterventionActionMigrationIsImmutableAndMatchesCoreActionSet(t *testing.T) {
	raw, err := migrationFiles.ReadFile("migrations/037_cancel_intervention_action.sql")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderMigration(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	values := make([]string, 0, len(core.InterventionActions()))
	for _, action := range core.InterventionActions() {
		values = append(values, "'"+string(action)+"'")
	}
	if !strings.Contains(text, "action IN ("+strings.Join(values, ", ")+")") {
		t.Fatalf("static intervention constraint does not match the core action set: %s", text)
	}
	if migrationChecksum(raw) != migrationChecksum(rendered) {
		t.Fatal("immutable intervention migration changed while rendering")
	}
}

func TestJobCostPersistenceKeepsMissingDistinctFromReportedZero(t *testing.T) {
	inProcess := jobInsertParams(core.Job{ID: "in-process", Runner: "in-process"})
	if inProcess.CostUsd.Valid {
		t.Fatalf("in-process cost unexpectedly valid: %+v", inProcess.CostUsd)
	}
	reportedZero := 0.0
	worker := jobInsertParams(core.Job{ID: "worker", Runner: "external", CostUSD: &reportedZero})
	if !worker.CostUsd.Valid || worker.CostUsd.Float64 != 0 {
		t.Fatalf("worker-reported zero was not preserved: %+v", worker.CostUsd)
	}
	roundTrip := jobFromDB(db.Job{ID: "worker", Runner: "external", CostUsd: worker.CostUsd})
	if roundTrip.CostUSD == nil || *roundTrip.CostUSD != 0 {
		t.Fatalf("worker-reported zero round trip = %+v", roundTrip.CostUSD)
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

func TestConfigDiffReportsReviewPanelChanges(t *testing.T) {
	before := config.WorkspaceDocument{Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt"}}}}
	after := before
	after.Review.Seats = []config.ReviewSeat{{Model: "gpt"}, {Model: "claude", Harness: "claude"}}
	sections := configDiff(before, after)
	if len(sections) != 1 || sections[0] != "review" {
		t.Fatalf("review config diff=%v", sections)
	}
}
