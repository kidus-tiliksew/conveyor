package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	controlstore "github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPolicyProjectionMigrationIsReplaySafeAndStartsFrozenTaskIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "policy-migration-" + core.NewTaskID()
	ctx := controlstore.WithWorkspace(context.Background(), workspace)
	bootstrap := &config.Config{
		Workspace: workspace, MaxBounces: 9, WorkOrderQueueTimeoutText: "24h",
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec": {Execution: config.ExecutionMCP, TimeoutText: "1h"}, "implement": {Execution: config.ExecutionMCP, TimeoutText: "9h"}, "review": {Execution: config.ExecutionMCP, TimeoutText: "3h"},
		}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
		Repos:  []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
	}
	if _, err = st.BootstrapWorkspaceConfig(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	legacyYAML := "workspace: " + workspace + `
max_bounces: 3
work_order_queue_timeout: 24h
execution_settings:
    spec:
        timeout: 25m
    implementation:
        timeout: 2h
    review:
        timeout: 35m
review:
    seats:
        - model: first
        - model: second
repos:
    - name: repo
      url: https://example.test/repo
      base: main
`
	if _, err = st.pool.Exec(ctx, `UPDATE workspaces SET config_yaml=$1 WHERE id=$2`, legacyYAML, workspace); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Source: "migration-test", Title: "Frozen policy migration", Repo: "repo", BaseBranch: "main", Branch: "conveyor/policy-migration-" + core.NewTaskID(), State: core.TaskQueued, NextStage: core.StageImplement, PolicyVersion: 1, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	legacyContract := `{"name":"legacy","execution_settings":{"spec":{"timeout":"25m"},"implementation":{"timeout":"2h"},"review":{"timeout":"35m"}},"review":{"seats":[{"model":"first"},{"model":"second"}]},"refresh_review":"delta"}`
	if _, err = st.pool.Exec(ctx, `UPDATE tasks SET setup_name='legacy', setup_contract=$1::jsonb WHERE id=$2`, legacyContract, task.ID); err != nil {
		t.Fatal(err)
	}
	var eventsBefore string
	if err = st.pool.QueryRow(ctx, `SELECT coalesce(string_agg(row_to_json(event_row)::text, E'\n' ORDER BY id), '') FROM events event_row WHERE task_id=$1`, task.ID).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	projection, err := migrationFiles.ReadFile("migrations/099_policy_contract_projection.sql")
	if err != nil {
		t.Fatal(err)
	}
	retirement, err := migrationFiles.ReadFile("migrations/100_retire_server_execution_configuration.sql")
	if err != nil {
		t.Fatal(err)
	}
	apply := func() string {
		t.Helper()
		if _, execErr := st.pool.Exec(ctx, string(projection)); execErr != nil {
			t.Fatal(execErr)
		}
		if _, execErr := st.pool.Exec(ctx, string(retirement)); execErr != nil {
			t.Fatal(execErr)
		}
		var document string
		if scanErr := st.pool.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspace).Scan(&document); scanErr != nil {
			t.Fatal(scanErr)
		}
		return document
	}
	first, second := apply(), apply()
	if first != second {
		t.Fatalf("migration replay changed canonical policy YAML\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	var eventsAfter string
	if err = st.pool.QueryRow(ctx, `SELECT coalesce(string_agg(row_to_json(event_row)::text, E'\n' ORDER BY id), '') FROM events event_row WHERE task_id=$1`, task.ID).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatal("policy migration changed append-only event rows")
	}
	persisted, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.SetupName != "" || persisted.SetupContract.MaxBounces != 3 || persisted.SetupContract.ExecutionSettings.Implementation.TimeoutText != "2h" || persisted.SetupContract.ExecutionSettings.Review.TimeoutText != "35m" || len(persisted.SetupContract.Review.Seats) != 2 {
		t.Fatalf("projected task policy=%+v", persisted.SetupContract)
	}
	if err = dispatch.New(st, bootstrap, nil).DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].ExecutionTimeoutText != "2h" || orders[0].RequiredModel != "" || orders[0].RequiredHarness != "" {
		t.Fatalf("post-migration in-flight order=%+v err=%v", orders, err)
	}
}
