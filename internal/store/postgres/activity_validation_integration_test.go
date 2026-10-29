package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

const activityValidationSentinel = "ACTIVITY-VALIDATION-SENTINEL"

func openActivityValidationStore(t *testing.T) (*Store, *queryRecorder) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	return newStore(pool), recorder
}

func activityValidationWorkspace(t *testing.T, st *Store, prefix string) (string, context.Context) {
	t.Helper()
	workspace := prefix + "-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return workspace, ctx
}

// seedActivityValidationScale grows every history the validation read must
// not transfer: task bodies, event payloads, work-order snapshots, plan and
// document content, and dependency chains, each carrying the sentinel.
func seedActivityValidationScale(t *testing.T, st *Store, ctx context.Context, workspace string, tasks, eventsPerTask int) {
	t.Helper()
	now := time.Now().UTC()
	padding := strings.Repeat(activityValidationSentinel+" ", 64)
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	previous := ""
	for i := range tasks {
		id := fmt.Sprintf("scale-%04d-%s", i, core.NewTaskID())
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Scale " + id, Body: padding, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}
		var dependencies []string
		if previous != "" {
			dependencies = []string{previous}
		}
		if err := st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		previous = id
		for n := range 2 {
			orderID := fmt.Sprintf("%s-implement-%d", id, n+1)
			if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
				t.Fatal(err)
			}
			if err := storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: id, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: padding, Acceptance: core.JSONPayload([]string{padding}), Decomposition: core.JSONPayload([]any{})}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.pool.Exec(ctx, `UPDATE work_orders SET governance_snapshot=jsonb_build_object('pad',$2::text), served_requirement_snapshot=jsonb_build_array($2::text) WHERE workspace_id=$1`, workspace, padding); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO events (task_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
		SELECT t.id, 'task.updated', 'system', 'system', jsonb_build_object('pad', $2::text), now() - interval '1 minute', t.workspace_id
		FROM tasks t CROSS JOIN generate_series(1, $3) WHERE t.workspace_id=$1`, workspace, padding, eventsPerTask); err != nil {
		t.Fatal(err)
	}
	for i := range max(1, tasks/10) {
		requirementID := fmt.Sprintf("req-scale-%d", i)
		if _, _, err := st.CreateRequirement(human, core.Requirement{ID: requirementID, Title: "Scale " + requirementID}, core.RequirementVersion{Content: "# Scale " + requirementID + "\n\n" + padding, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Scale."}}}); err != nil {
			t.Fatal(err)
		}
		designID := fmt.Sprintf("component-scale-%d", i)
		if _, _, err := st.CreateSystemDesign(human, core.SystemDesign{ID: designID, Title: "Scale " + designID, Category: "Component design"}, core.SystemDesignVersion{Content: "# Scale\n\n" + padding + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/" + designID + "/**\n```\n", Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
}

// activityValidationTransfer runs the validation statement and returns the
// rows and raw bytes PostgreSQL transferred.
func activityValidationTransfer(t *testing.T, st *Store, ctx context.Context, now time.Time) (int, int, []byte) {
	t.Helper()
	schema, err := st.activityValidationSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.pool.Query(ctx, activityValidationSQL(schema), workspace(ctx), now)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count, size := 0, 0
	var raw []byte
	for rows.Next() {
		count++
		for _, value := range rows.RawValues() {
			size += len(value)
			raw = append(raw, value...)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return count, size, raw
}

func TestActivityValidationTransfersFixedSizeIntegration(t *testing.T) {
	st, recorder := openActivityValidationStore(t)
	smallWorkspace, small := activityValidationWorkspace(t, st, "activity-small")
	largeWorkspace, large := activityValidationWorkspace(t, st, "activity-large")
	unrelatedWorkspace, unrelated := activityValidationWorkspace(t, st, "activity-unrelated")
	seedActivityValidationScale(t, st, small, smallWorkspace, 10, 2)
	seedActivityValidationScale(t, st, large, largeWorkspace, 200, 20)
	seedActivityValidationScale(t, st, unrelated, unrelatedWorkspace, 50, 5)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := st.ReadActivityValidation(small, now); err != nil {
		t.Fatal(err)
	}
	// Column discovery is cached: every later read is one statement.
	type observation struct {
		queries, rows, bytes int
		elapsed              time.Duration
	}
	observe := func(ctx context.Context) observation {
		t.Helper()
		recorder.reset()
		started := time.Now()
		if _, err := st.ReadActivityValidation(ctx, now); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(started)
		queries := len(recorder.snapshot())
		count, size, raw := activityValidationTransfer(t, st, ctx, now)
		if bytes.Contains(raw, []byte(activityValidationSentinel)) {
			t.Fatalf("validation transferred a body, payload, content, or snapshot: %q", raw)
		}
		return observation{queries: queries, rows: count, bytes: size, elapsed: elapsed}
	}
	smallRead, largeRead := observe(small), observe(large)
	t.Logf("activity validation: small %+v; large %+v", smallRead, largeRead)
	// Two fixed rows plus one per table, each at most a 4-byte ordinal, a
	// 32-byte name, an 8-byte count, and a 40-character decimal.
	wantRows := 2 + len(store.ActivityValidationTables)
	if smallRead.queries != 1 || largeRead.queries != 1 || smallRead.rows != wantRows || largeRead.rows != wantRows {
		t.Fatalf("validation reads: small %+v large %+v, want 1 query and %d rows", smallRead, largeRead, wantRows)
	}
	if largeRead.bytes > wantRows*(4+32+8+40) {
		t.Fatalf("large validation transferred %d bytes", largeRead.bytes)
	}
	if largeRead.elapsed > 2*time.Second {
		t.Fatalf("large validation read took %s", largeRead.elapsed)
	}
}

func TestActivityValidationSeesRowOnlyWritesAndLateCommitsIntegration(t *testing.T) {
	st, _ := openActivityValidationStore(t)
	workspaceID, ctx := activityValidationWorkspace(t, st, "activity-race")
	now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspaceID, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, Title: "Race", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	orderID := taskID + "-implement-1"
	if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	read := func() store.ActivityValidation {
		t.Helper()
		validation, err := st.ReadActivityValidation(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return validation
	}

	// RenewWorkerClaimCommand commits the row before it appends its event; a
	// reader between the two sees the row change alone.
	before := read()
	if _, err := st.pool.Exec(ctx, `UPDATE work_orders SET queue_deadline=queue_deadline + interval '1 minute' WHERE workspace_id=$1 AND id=$2`, workspaceID, orderID); err != nil {
		t.Fatal(err)
	}
	rowOnly := read()
	if rowOnly.EventCount != before.EventCount || rowOnly.Signature == before.Signature {
		t.Fatalf("row-only write: before %+v after %+v", before, rowOnly)
	}

	// A lower event ID committed after a higher one raises the count while
	// the maximum ID stays put.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var lowID int64
	if err = tx.QueryRow(ctx, `INSERT INTO events (task_id, kind, actor_id, actor_role, payload_json, at, workspace_id) VALUES ($1, 'task.updated', 'system', 'system', '{}', now(), $2) RETURNING id`, taskID, workspaceID).Scan(&lowID); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.updated", At: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	higher := read()
	if higher.EventMaxID <= lowID || higher.EventCount != rowOnly.EventCount+1 {
		t.Fatalf("higher commit: %+v, low id %d", higher, lowID)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	late := read()
	if late.EventMaxID != higher.EventMaxID || late.EventCount != higher.EventCount+1 || late == higher {
		t.Fatalf("late lower-ID commit: before %+v after %+v", higher, late)
	}
}
