package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

const activityValidationSentinel = "SINGLESTORE-ACTIVITY-VALIDATION-SENTINEL"

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
	var ids []string
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
		ids = append(ids, id)
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
	if _, err := sharedDatabase.admin.ExecContext(ctx, "UPDATE `"+sharedDatabase.name+"`.work_orders SET served_requirement_snapshot=?, governance_snapshot=? WHERE workspace_id=?", core.JSONPayload([]string{padding}), core.JSONPayload(map[string]string{"pad": padding}), workspace); err != nil {
		t.Fatal(err)
	}
	payload := string(core.JSONPayload(map[string]string{"pad": padding}))
	for start := 0; start < len(ids); start += 50 {
		var values []string
		var args []any
		for _, id := range ids[start:min(start+50, len(ids))] {
			for range eventsPerTask {
				values = append(values, "(?,?,'task.updated','system','system',?,?)")
				args = append(args, workspace, id, payload, now.Add(-time.Minute))
			}
		}
		if len(values) == 0 {
			continue
		}
		if _, err := sharedDatabase.admin.ExecContext(ctx, "INSERT INTO `"+sharedDatabase.name+"`.events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at) VALUES "+strings.Join(values, ","), args...); err != nil {
			t.Fatal(err)
		}
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

func TestActivityValidationTransfersFixedSizeIntegration(t *testing.T) {
	st := integrationStore(t)
	smallWorkspace, small := activityValidationWorkspace(t, st, "activity-small")
	largeWorkspace, large := activityValidationWorkspace(t, st, "activity-large")
	unrelatedWorkspace, unrelated := activityValidationWorkspace(t, st, "activity-unrelated")
	seedActivityValidationScale(t, st, small, smallWorkspace, 5, 2)
	seedActivityValidationScale(t, st, large, largeWorkspace, 60, 20)
	seedActivityValidationScale(t, st, unrelated, unrelatedWorkspace, 20, 5)

	recorder := &selectedBytesRecorder{needles: [][]byte{[]byte(activityValidationSentinel)}}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}
	now := time.Now().UTC().Truncate(time.Microsecond)
	// Column discovery is cached: every later read is one statement.
	if _, err = recorded.ReadActivityValidation(small, now); err != nil {
		t.Fatal(err)
	}
	type observation struct {
		queries, rows, bytes int
		elapsed              time.Duration
	}
	observe := func(ctx context.Context) observation {
		t.Helper()
		recorder.start()
		started := time.Now()
		if _, err := recorded.ReadActivityValidation(ctx, now); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(started)
		rows, size := recorder.totals()
		queries, hits := recorder.stop()
		if len(hits) != 0 {
			t.Fatalf("validation transferred a body, payload, content, or snapshot: %v", hits)
		}
		return observation{queries: len(queries), rows: rows, bytes: size, elapsed: elapsed}
	}
	smallRead, largeRead := observe(small), observe(large)
	t.Logf("activity validation: small %+v; large %+v", smallRead, largeRead)
	// Two fixed rows plus one per table, each at most a 32-byte name and a
	// 40-character decimal besides its integers.
	wantRows := 2 + len(store.ActivityValidationTables)
	if smallRead.queries != 1 || largeRead.queries != 1 || smallRead.rows != wantRows || largeRead.rows != wantRows {
		t.Fatalf("validation reads: small %+v large %+v, want 1 query and %d rows", smallRead, largeRead, wantRows)
	}
	if largeRead.bytes > wantRows*(32+40) {
		t.Fatalf("large validation transferred %d bytes", largeRead.bytes)
	}
	if largeRead.elapsed > 5*time.Second {
		t.Fatalf("large validation read took %s", largeRead.elapsed)
	}
}

func TestActivityValidationSeesRowOnlyWritesAndLateCommitsIntegration(t *testing.T) {
	st := integrationStore(t)
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

	// A row committed without its event changes the signature alone.
	before := read()
	if _, err := sharedDatabase.admin.ExecContext(ctx, "UPDATE `"+sharedDatabase.name+"`.work_orders SET queue_deadline=DATE_ADD(queue_deadline, INTERVAL 1 MINUTE) WHERE workspace_id=? AND id=?", workspaceID, orderID); err != nil {
		t.Fatal(err)
	}
	rowOnly := read()
	if rowOnly.EventCount != before.EventCount || rowOnly.Signature == before.Signature {
		t.Fatalf("row-only write: before %+v after %+v", before, rowOnly)
	}

	// An event that commits after a later one raises the count whatever its ID.
	tx, err := sharedDatabase.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "INSERT INTO `"+sharedDatabase.name+"`.events (workspace_id, task_id, kind, actor_id, actor_role, payload_json, at) VALUES (?,?,'task.updated','system','system','{}',?)", workspaceID, taskID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.updated", At: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	visible := read()
	if visible.EventCount != rowOnly.EventCount+1 {
		t.Fatalf("committed event: %+v after %+v", visible, rowOnly)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	late := read()
	if late.EventCount != visible.EventCount+1 || late == visible {
		t.Fatalf("late commit: before %+v after %+v", visible, late)
	}
}
