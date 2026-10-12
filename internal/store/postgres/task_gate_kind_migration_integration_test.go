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

func TestTaskGateKindMigrationBackfillsLatestCommandAndReplaysCleanIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "gate_kind_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if err = migrateControlPlaneToVersion(ctx, pool, 143); err != nil {
		t.Fatalf("migrate to 143: %v", err)
	}
	workspace := "gate-kind-" + core.NewTaskID()
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insertTask := func(id string, state core.TaskState) {
		t.Helper()
		if _, err = pool.Exec(ctx, `INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval)
			VALUES ($1,$2,'test','gate kind','','','L2','conveyor','main',$3,$4,$5,'',true,true)`,
			id, workspace, "conveyor/task-"+id, state, now); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent := func(taskID string, at time.Time, command core.TaskCommand) {
		t.Helper()
		if _, err = pool.Exec(ctx, `INSERT INTO events (workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,'task.state_changed','migration','system',$3,$4)`,
			workspace, taskID, core.JSONPayload(map[string]any{"command": command}), at); err != nil {
			t.Fatal(err)
		}
	}
	mapped, olderWins, unmapped, silent, running := core.NewTaskID(), core.NewTaskID(), core.NewTaskID(), core.NewTaskID(), core.NewTaskID()
	planRevision, mergeGate, bounce, tie := core.NewTaskID(), core.NewTaskID(), core.NewTaskID(), core.NewTaskID()
	insertTask(mapped, core.TaskAwaiting)
	insertTask(olderWins, core.TaskAwaiting)
	insertTask(unmapped, core.TaskAwaiting)
	insertTask(silent, core.TaskAwaiting)
	insertTask(running, core.TaskRunning)
	insertTask(planRevision, core.TaskAwaiting)
	insertTask(mergeGate, core.TaskAwaiting)
	insertTask(bounce, core.TaskAwaiting)
	insertTask(tie, core.TaskAwaiting)
	insertEvent(mapped, now.Add(-time.Minute), core.TaskCancel)
	insertEvent(mapped, now, core.TaskGateSpec)
	insertEvent(olderWins, now, core.TaskJobFail)
	insertEvent(olderWins, now.Add(-time.Minute), core.TaskGateMerge)
	insertEvent(unmapped, now, core.TaskCancel)
	insertEvent(running, now, core.TaskGateSpec)
	insertEvent(planRevision, now, core.TaskGatePlanRevision)
	insertEvent(mergeGate, now, core.TaskGateMerge)
	insertEvent(bounce, now, core.TaskStageBounceLimit)
	insertEvent(tie, now, core.TaskJobFail)
	insertEvent(tie, now, core.TaskGateMerge)
	var tieLower, tieHigher int64
	var tieLowerCommand, tieHigherCommand string
	if err = pool.QueryRow(ctx, `SELECT id, payload_json->>'command' FROM events WHERE task_id=$1 ORDER BY id LIMIT 1`, tie).Scan(&tieLower, &tieLowerCommand); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT id, payload_json->>'command' FROM events WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, tie).Scan(&tieHigher, &tieHigherCommand); err != nil {
		t.Fatal(err)
	}
	if tieHigher <= tieLower || tieLowerCommand != string(core.TaskJobFail) || tieHigherCommand != string(core.TaskGateMerge) {
		t.Fatalf("equal-at fixture ids=%d/%d commands=%s/%s", tieLower, tieHigher, tieLowerCommand, tieHigherCommand)
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 145 {
		t.Fatalf("schema version=%d, want 145", version)
	}
	kind := func(id string) *string {
		t.Helper()
		var value *string
		if err = pool.QueryRow(ctx, `SELECT gate_kind FROM tasks WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := kind(mapped); got == nil || *got != string(core.GateKindPlanApproval) {
		t.Fatalf("mapped gate_kind=%v", got)
	}
	if got := kind(olderWins); got == nil || *got != string(core.GateKindJobFailure) {
		t.Fatalf("latest-command gate_kind=%v", got)
	}
	if got := kind(planRevision); got == nil || *got != string(core.GateKindPlanRevision) {
		t.Fatalf("plan revision gate_kind=%v", got)
	}
	if got := kind(mergeGate); got == nil || *got != string(core.GateKindMergeApproval) {
		t.Fatalf("merge gate_kind=%v", got)
	}
	if got := kind(bounce); got == nil || *got != string(core.GateKindReviewBounceLimit) {
		t.Fatalf("bounce gate_kind=%v", got)
	}
	if got := kind(tie); got == nil || *got != string(core.GateKindMergeApproval) {
		t.Fatalf("equal-at higher-id gate_kind=%v, want merge_approval", got)
	}
	if kind(unmapped) != nil || kind(silent) != nil || kind(running) != nil {
		t.Fatalf("unmapped=%v silent=%v running=%v", kind(unmapped), kind(silent), kind(running))
	}
	var eventsBefore int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1`, workspace).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE tasks SET gate_kind=NULL WHERE id=$1`, mapped); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("migrations/144_task_gate_kind.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("replay 144: %v", err)
	}
	if got := kind(mapped); got == nil || *got != string(core.GateKindPlanApproval) {
		t.Fatalf("replay changed mapped gate_kind=%v", got)
	}
	if kind(unmapped) != nil {
		t.Fatalf("replay filled unmapped gate_kind=%v", kind(unmapped))
	}
	var eventsAfter int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1`, workspace).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("replay wrote events: before=%d after=%d", eventsBefore, eventsAfter)
	}
}
