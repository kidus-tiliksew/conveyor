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

// TestPlanStateUpgradeFillsEachLegacyCaseIntegration proves migration 161 and
// its hook fill each existing task's plan state once by
// component-plan-state invariant 8's order, give existing plan versions the
// plan_order origin and no base commit, and write no event.
func TestPlanStateUpgradeFillsEachLegacyCaseIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "plan_state_upgrade_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
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
	if err = migrateControlPlaneToVersion(ctx, pool, planStateMigrationVersion-1); err != nil {
		t.Fatalf("migrate to %d: %v", planStateMigrationVersion-1, err)
	}
	workspace := "plan-state-upgrade-" + core.NewTaskID()
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	type legacyTask struct {
		name          string
		state         core.TaskState
		gateKind      string
		policyVersion int
		specApproval  bool
		level         core.EscalationLevel
		approvedPlan  *bool
		want          core.PlanState
	}
	approved, unapproved := true, false
	cases := []legacyTask{
		{"at the plan approval gate", core.TaskAwaiting, string(core.GateKindPlanApproval), 1, true, "", &unapproved, core.PlanAwaitingApproval},
		{"at the plan approval gate with an earlier approved plan", core.TaskAwaiting, string(core.GateKindPlanApproval), 1, true, "", &approved, core.PlanAwaitingApproval},
		{"approved plan version", core.TaskRunning, "", 1, true, "", &approved, core.PlanCompleted},
		{"approved plan version at policy version 0", core.TaskRunning, "", 0, false, core.L1, &approved, core.PlanCompleted},
		{"unapproved plan with spec_approval on", core.TaskRunning, "", 1, true, "", &unapproved, core.PlanNeeded},
		{"spec_approval true at policy version 1", core.TaskQueued, "", 1, true, core.L0, nil, core.PlanNeeded},
		{"spec_approval false at policy version 1", core.TaskQueued, "", 1, false, core.L3, nil, core.PlanNotRequired},
		{"L2 at policy version 0", core.TaskQueued, "", 0, false, core.L2, nil, core.PlanNeeded},
		{"L3 at policy version 0", core.TaskQueued, "", 0, false, core.L3, nil, core.PlanNeeded},
		{"L1 at policy version 0", core.TaskQueued, "", 0, true, core.L1, nil, core.PlanNotRequired},
		{"L0 at policy version 0", core.TaskQueued, "", 0, true, core.L0, nil, core.PlanNotRequired},
		{"merge gate", core.TaskAwaiting, string(core.GateKindMergeApproval), 1, false, "", nil, core.PlanNotRequired},
	}
	ids := make([]string, len(cases))
	for i, legacy := range cases {
		ids[i] = core.NewTaskID()
		var gateKind any
		if legacy.gateKind != "" {
			gateKind = legacy.gateKind
		}
		if _, err = pool.Exec(ctx, `INSERT INTO tasks
			(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval,policy_version,gate_kind)
			VALUES ($1,$2,'test',$3,'','',$4,'conveyor','main',$5,$6,$7,'',$8,true,$9,$10)`,
			ids[i], workspace, legacy.name, string(legacy.level), "conveyor/task-"+ids[i], string(legacy.state), now, legacy.specApproval, legacy.policyVersion, gateKind); err != nil {
			t.Fatalf("%s: %v", legacy.name, err)
		}
		if legacy.approvedPlan != nil {
			if _, err = pool.Exec(ctx, `INSERT INTO task_specs (task_id,version,content,acceptance_count,acceptance,decomposition,approved,approved_at,created_at,agent,model)
				VALUES ($1,1,'plan',0,'[]','[]',true,$2,$2,'','')`, ids[i], now); err != nil {
				t.Fatalf("%s: %v", legacy.name, err)
			}
			if !*legacy.approvedPlan {
				if _, err = pool.Exec(ctx, `UPDATE task_specs SET approved=false, approved_at=NULL WHERE task_id=$1`, ids[i]); err != nil {
					t.Fatalf("%s: %v", legacy.name, err)
				}
			}
		}
	}
	countEvents := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1`, workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countEvents()

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for i, legacy := range cases {
		var state string
		if err = pool.QueryRow(ctx, `SELECT plan_state FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace, ids[i]).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if core.PlanState(state) != legacy.want {
			t.Fatalf("%s: plan_state=%q, want %q", legacy.name, state, legacy.want)
		}
	}
	var origins, commits int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE s.origin <> 'plan_order'), count(*) FILTER (WHERE s.base_commit IS NOT NULL)
		FROM task_specs s JOIN tasks t ON t.id=s.task_id WHERE t.workspace_id=$1`, workspace).Scan(&origins, &commits); err != nil {
		t.Fatal(err)
	}
	if origins != 0 || commits != 0 {
		t.Fatalf("existing plan versions: %d without plan_order origin, %d with a base commit", origins, commits)
	}
	if after := countEvents(); after != before {
		t.Fatalf("upgrade wrote %d events", after-before)
	}
	// The column refuses a value outside the four states.
	if _, err = pool.Exec(ctx, `UPDATE tasks SET plan_state='planned' WHERE workspace_id=$1 AND id=$2`, workspace, ids[0]); err == nil {
		t.Fatal("tasks.plan_state accepted an unknown value")
	}
	if _, err = pool.Exec(ctx, `UPDATE task_specs SET base_commit='ABC' WHERE task_id=$1`, ids[0]); err == nil {
		t.Fatal("task_specs.base_commit accepted a value that is not a full commit ID")
	}
	if _, err = pool.Exec(ctx, `UPDATE task_specs SET origin='agent' WHERE task_id=$1`, ids[0]); err == nil {
		t.Fatal("task_specs.origin accepted an unknown origin")
	}
	// Reads through the store serve the filled state.
	st := newStore(pool)
	got, err := st.GetTask(store.WithWorkspace(ctx, workspace), ids[2])
	if err != nil || got.PlanState != core.PlanCompleted {
		t.Fatalf("store read=%+v err=%v", got.PlanState, err)
	}
	plan, found, err := st.GetSpecVersion(store.WithWorkspace(ctx, workspace), ids[2], 1)
	if err != nil || !found || plan.Origin != core.PlanOriginPlanOrder || plan.BaseCommit != nil {
		t.Fatalf("store plan read=%+v found=%t err=%v", plan, found, err)
	}
}
