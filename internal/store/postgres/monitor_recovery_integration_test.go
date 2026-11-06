package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMonitorRecoveryOutcomeAfterMigrationIntegration proves that a store
// migrated through migration 160 accepts the recovery_observed outcome and keeps
// refusing any outcome outside the list (component-monitor-drift invariant 17;
// component-persistence invariant 35).
func TestMonitorRecoveryOutcomeAfterMigrationIntegration(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "recovery-outcome-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	var migration string
	if err = st.pool.QueryRow(ctx, `SELECT name FROM conveyor_schema_migrations WHERE version=160`).Scan(&migration); err != nil {
		t.Fatalf("read migration 160 from the ledger: %v", err)
	}
	if !strings.Contains(migration, "monitor_recovery_outcome") {
		t.Fatalf("migration 160 is %q", migration)
	}
	var constraint string
	if err = st.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='monitor_observations_task_outcome_check' AND conrelid='monitor_observations'::regclass`).Scan(&constraint); err != nil {
		t.Fatalf("read the outcome constraint: %v", err)
	}
	for _, want := range []string{"created", "reused", "recovery_observed"} {
		if !strings.Contains(constraint, want) {
			t.Fatalf("the outcome constraint %q does not allow %s", constraint, want)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{
		ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Source: "monitor:post_merge_failure",
		IntakeKey: "monitor:post_merge_failure:repo:commit:abc:attempt:1", State: core.TaskQueued,
		NextStage: core.StageTriage, CreatedAt: now,
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	observe := func(occurrence string) string {
		t.Helper()
		observation := monitor.Observation{
			WorkspaceID: workspace, Repository: "repo", Kind: monitor.PostMergeFailure,
			OccurrenceID: occurrence, SourceURL: "https://example.test/commit/" + occurrence,
			CommitSHA: occurrence, ObservedAt: now,
		}
		if _, fresh, observeErr := st.Observe(ctx, observation); observeErr != nil || !fresh {
			t.Fatalf("observe %s: fresh=%t err=%v", occurrence, fresh, observeErr)
		}
		return observation.Identity()
	}

	identity := observe("recovery:commit:outcome")
	linked, err := st.LinkTask(ctx, identity, task.ID, monitor.OutcomeRecoveryObserved)
	if err != nil {
		t.Fatalf("link with %s: %v", monitor.OutcomeRecoveryObserved, err)
	}
	if linked.TaskID != task.ID || linked.TaskOutcome != monitor.OutcomeRecoveryObserved || linked.State != "task_linked" {
		t.Fatalf("linked = %+v", linked)
	}

	refused := observe("recovery:commit:refused")
	if _, err = st.LinkTask(ctx, refused, task.ID, "unlisted_outcome"); err == nil {
		t.Fatal("an unlisted outcome was accepted")
	}
	row, err := st.getObservation(ctx, refused)
	if err != nil {
		t.Fatal(err)
	}
	if row.TaskID != "" || row.TaskOutcome != "" {
		t.Fatalf("a refused link wrote %+v", row)
	}

	annotated := observe("recovery:commit:annotated")
	payload := map[string]any{"identity": annotated}
	record, err := st.LinkRecoveryTask(ctx, annotated, task.ID, payload, payload)
	if err != nil {
		t.Fatalf("recovery link: %v", err)
	}
	if record.TaskID != task.ID || record.TaskOutcome != monitor.OutcomeRecoveryObserved {
		t.Fatalf("record = %+v", record)
	}
}
