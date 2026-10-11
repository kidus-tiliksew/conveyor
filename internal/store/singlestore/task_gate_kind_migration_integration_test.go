package singlestore

import (
	"os"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskGateKindCrossWorkspaceBackfillIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	insertTask := func(workspace, id string, state core.TaskState) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO workspaces(id, name) VALUES(?, ?) ON DUPLICATE KEY UPDATE name=name`, workspace, workspace); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval)
			VALUES(?,?,'test','gate kind','','','L2','conveyor','main',?,?,?,'',true,true)`,
			id, workspace, "conveyor/task-"+workspace+"-"+id, state, at); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent := func(workspace, taskID string, id int64, at time.Time, command core.TaskCommand) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO events
			(id,workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES(?,?,?,'task.state_changed','migration','system',?,?)`,
			id, workspace, taskID, `{"command":"`+string(command)+`"}`, at); err != nil {
			t.Fatal(err)
		}
	}
	const shared = "shared-task"
	insertTask("gate-a", shared, core.TaskAwaiting)
	insertTask("gate-b", shared, core.TaskAwaiting)
	insertEvent("gate-a", shared, 10, at.Add(time.Minute), core.TaskGateSpec)
	insertEvent("gate-b", shared, 99, at, core.TaskJobFail)
	insertTask("gate-a", "tie", core.TaskAwaiting)
	insertEvent("gate-a", "tie", 1, at, core.TaskJobFail)
	insertEvent("gate-a", "tie", 2, at, core.TaskGateMerge)
	for i, gate := range []struct {
		id      string
		command core.TaskCommand
	}{
		{"spec", core.TaskGateSpec},
		{"revision", core.TaskGatePlanRevision},
		{"merge", core.TaskGateMerge},
		{"bounce", core.TaskStageBounceLimit},
		{"fail", core.TaskJobFail},
	} {
		insertTask("gate-a", gate.id, core.TaskAwaiting)
		insertEvent("gate-a", gate.id, int64(21+i), at, gate.command)
	}

	raw, err := migrationFiles.ReadFile("migrations/0019_task_gate_kind.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	kind := func(workspace, id string) *string {
		t.Helper()
		var value *string
		if err = st.db.QueryRowContext(ctx, `SELECT gate_kind FROM tasks WHERE workspace_id=? AND id=?`, workspace, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	want := func(workspace, id, expected string) {
		t.Helper()
		got := kind(workspace, id)
		if got == nil || *got != expected {
			t.Fatalf("%s/%s gate_kind=%v, want %s", workspace, id, got, expected)
		}
	}
	want("gate-a", shared, string(core.GateKindPlanApproval))
	want("gate-b", shared, string(core.GateKindJobFailure))
	want("gate-a", "tie", string(core.GateKindMergeApproval))
	want("gate-a", "spec", string(core.GateKindPlanApproval))
	want("gate-a", "revision", string(core.GateKindPlanRevision))
	want("gate-a", "merge", string(core.GateKindMergeApproval))
	want("gate-a", "bounce", string(core.GateKindReviewBounceLimit))
	want("gate-a", "fail", string(core.GateKindJobFailure))
	if _, err = st.db.ExecContext(ctx, `UPDATE tasks SET gate_kind=NULL WHERE workspace_id='gate-a' AND id=?`, shared); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.ExecContext(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	want("gate-a", shared, string(core.GateKindPlanApproval))
	want("gate-b", shared, string(core.GateKindJobFailure))
}
