package singlestore

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestPlanStateUpgradeFillsEachLegacyCaseIntegration proves that migrating a
// database whose ledger stops at version 34 runs migration 0035's hook, which
// adds the plan-state columns behind information_schema checks, fills each
// existing task's plan state once by component-plan-state invariant 8's
// order, gives existing plan versions the plan_order origin and no base
// commit, and writes no event; that an interrupted startup filling again
// changes nothing already filled; and that a row an older writer inserts
// without a plan state reads as needed.
func TestPlanStateUpgradeFillsEachLegacyCaseIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	// Model a database from before migration 0035: the columns are absent and
	// the ledger stops at version 34.
	for _, statement := range []string{
		`ALTER TABLE tasks DROP COLUMN plan_state`,
		`ALTER TABLE task_specs DROP COLUMN origin`,
		`ALTER TABLE task_specs DROP COLUMN base_commit`,
		`DELETE FROM conveyor_singlestore_migrations WHERE version >= 35`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := st.migrate(context.Background()); err != nil {
			t.Errorf("restore migration 0035: %v", err)
		}
	})
	workspace := "plan-state-upgrade"
	if _, err := st.db.ExecContext(ctx, `INSERT INTO workspaces(id, name) VALUES(?, ?)`, workspace, workspace); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	approved, unapproved := true, false
	cases := []struct {
		id            string
		state         core.TaskState
		gateKind      string
		policyVersion int
		specApproval  bool
		level         core.EscalationLevel
		plan          *bool
		want          core.PlanState
	}{
		{"gate", core.TaskAwaiting, string(core.GateKindPlanApproval), 1, true, "", &unapproved, core.PlanAwaitingApproval},
		{"gate-approved", core.TaskAwaiting, string(core.GateKindPlanApproval), 1, true, "", &approved, core.PlanAwaitingApproval},
		{"approved", core.TaskRunning, "", 1, true, "", &approved, core.PlanCompleted},
		{"approved-v0", core.TaskRunning, "", 0, false, core.L1, &approved, core.PlanCompleted},
		{"unapproved", core.TaskRunning, "", 1, true, "", &unapproved, core.PlanNeeded},
		{"spec-on", core.TaskQueued, "", 1, true, core.L0, nil, core.PlanNeeded},
		{"spec-off", core.TaskQueued, "", 1, false, core.L3, nil, core.PlanNotRequired},
		{"l2", core.TaskQueued, "", 0, false, core.L2, nil, core.PlanNeeded},
		{"l3", core.TaskQueued, "", 0, false, core.L3, nil, core.PlanNeeded},
		{"l1", core.TaskQueued, "", 0, true, core.L1, nil, core.PlanNotRequired},
		{"l0", core.TaskQueued, "", 0, true, core.L0, nil, core.PlanNotRequired},
		{"merge-gate", core.TaskAwaiting, string(core.GateKindMergeApproval), 1, false, "", nil, core.PlanNotRequired},
	}
	for _, legacy := range cases {
		var gateKind any
		if legacy.gateKind != "" {
			gateKind = legacy.gateKind
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,gate_kind)
			VALUES(?,?,'test',?,'','',?,'conveyor','main',?,?,?,'',?,true,?,?)`,
			legacy.id, workspace, legacy.id, string(legacy.level), "conveyor/task-"+legacy.id, string(legacy.state), at, legacy.specApproval, legacy.policyVersion, gateKind); err != nil {
			t.Fatalf("%s: %v", legacy.id, err)
		}
		if legacy.plan != nil {
			var approvedAt any
			if *legacy.plan {
				approvedAt = at
			}
			if _, err := st.db.ExecContext(ctx, `INSERT INTO task_specs
				(workspace_id,task_id,version,content,acceptance_count,acceptance,decomposition,approved,approved_at,created_at,agent,model)
				VALUES(?,?,1,'plan',0,'[]','[]',?,?,?,'','')`, workspace, legacy.id, *legacy.plan, approvedAt, at); err != nil {
				t.Fatalf("%s: %v", legacy.id, err)
			}
		}
	}
	countEvents := func() int {
		t.Helper()
		var count int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=?`, workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countEvents()
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var recorded int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?`, planStateMigrationVersion).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("migration 0035 ledger rows=%d err=%v", recorded, err)
	}
	stateOf := func(id string) core.PlanState {
		t.Helper()
		var state *string
		if err := st.db.QueryRowContext(ctx, `SELECT plan_state FROM tasks WHERE workspace_id=? AND id=?`, workspace, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == nil {
			t.Fatalf("%s: plan_state is NULL after the fill", id)
		}
		return core.PlanState(*state)
	}
	for _, legacy := range cases {
		if got := stateOf(legacy.id); got != legacy.want {
			t.Fatalf("%s: plan_state=%q, want %q", legacy.id, got, legacy.want)
		}
	}
	var origins, commits int
	if err := st.db.QueryRowContext(ctx, `SELECT SUM(origin <> 'plan_order'), SUM(base_commit IS NOT NULL) FROM task_specs WHERE workspace_id=?`, workspace).Scan(&origins, &commits); err != nil {
		t.Fatal(err)
	}
	if origins != 0 || commits != 0 {
		t.Fatalf("existing plan versions: %d without plan_order origin, %d with a base commit", origins, commits)
	}
	if after := countEvents(); after != before {
		t.Fatalf("upgrade wrote %d events", after-before)
	}

	// An interrupted startup runs the hook again: the columns exist, and a row
	// already filled keeps its state.
	if _, err := st.db.ExecContext(ctx, `UPDATE tasks SET plan_state='completed' WHERE workspace_id=? AND id='spec-on'`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM conveyor_singlestore_migrations WHERE version=?`, planStateMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stateOf("spec-on"); got != core.PlanCompleted {
		t.Fatalf("a rerun refilled a filled row: %q", got)
	}
	if after := countEvents(); after != before {
		t.Fatalf("rerun wrote %d events", after-before)
	}
	// A writer from before migration 0035 inserts a task without a plan state
	// after the fill; reads serve it as needed instead of failing.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version)
		VALUES('late-writer',?,'test','late','','','','conveyor','main','conveyor/task-late-writer','queued',?,'',false,true,1)`, workspace, at); err != nil {
		t.Fatal(err)
	}
	late, err := st.GetTask(store.WithWorkspace(ctx, workspace), "late-writer")
	if err != nil || late.PlanState != core.PlanNeeded {
		t.Fatalf("late writer row read=%q err=%v", late.PlanState, err)
	}
	listed, err := st.ListTasks(store.WithWorkspace(ctx, workspace))
	if err != nil || len(listed) != len(cases)+1 {
		t.Fatalf("listed %d tasks err=%v", len(listed), err)
	}
	got, err := st.GetTask(store.WithWorkspace(ctx, workspace), "approved")
	if err != nil || got.PlanState != core.PlanCompleted {
		t.Fatalf("store read=%q err=%v", got.PlanState, err)
	}
	plan, found, err := st.GetSpecVersion(store.WithWorkspace(ctx, workspace), "approved", 1)
	if err != nil || !found || plan.Origin != core.PlanOriginPlanOrder || plan.BaseCommit != nil {
		t.Fatalf("store plan read=%+v found=%t err=%v", plan, found, err)
	}
}
