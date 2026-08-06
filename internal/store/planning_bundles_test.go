package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestPlanningBundleApprovalCreatesOneAtomicDependencyOrderedTaskSet(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-bundle", Title: "Bundle delivery"}, core.RequirementVersion{Content: "Bundle delivery", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver a task bundle."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	pending, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "Bundle delivery v2", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver a dependency-ordered task bundle."}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-bundle", Goal: core.PlanningGoalBundle})
	if err != nil {
		t.Fatal(err)
	}
	bundle := core.PlanningBundle{ID: "bundle-1", SessionID: session.ID, Title: "Delivery", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: pending.Version}}, Tasks: []core.PlanningBundleTask{
		{MemberID: "one", Title: "One", Body: "First task", Repo: "conveyor", Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
		{MemberID: "two", Title: "Two", Body: "Second task", Repo: "conveyor", DependsOn: []string{"one"}, Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
		{MemberID: "three", Title: "Three", Body: "Third task", Repo: "conveyor", DependsOn: []string{"two"}, Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}}},
	}}
	created, err := st.CreatePlanningBundle(ctx, bundle)
	if err != nil || created.Status != core.PlanningBundlePending || created.Documents[0].Status != "pending" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	approved, err := st.ApprovePlanningBundle(ctx, created.ID)
	if err != nil || approved.Status != core.PlanningBundleApproved {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	blocking, err := st.ListBlockingTaskIDs(ctx, approved.Tasks[2].CreatedTaskID)
	if err != nil || len(blocking) != 1 || blocking[0] != approved.Tasks[1].CreatedTaskID {
		t.Fatalf("blocking=%v err=%v", blocking, err)
	}
	attached, err := TaskContextForTask(ctx, st, approved.Tasks[0].CreatedTaskID)
	if err != nil || len(attached.Requirements) != 1 || attached.Requirements[0].ID != requirement.ID {
		t.Fatalf("context=%+v err=%v", attached, err)
	}
	version, err := st.GetRequirementVersion(ctx, requirement.ID, pending.Version)
	if err != nil || version.Confirmed {
		t.Fatalf("pending version=%+v err=%v", version, err)
	}
	repeated, err := st.ApprovePlanningBundle(ctx, created.ID)
	if err != nil || repeated.Tasks[0].CreatedTaskID != approved.Tasks[0].CreatedTaskID {
		t.Fatalf("repeat=%+v err=%v", repeated, err)
	}
	tasks, _ = st.ListTasks(ctx)
	if len(tasks) != 3 {
		t.Fatalf("repeat created duplicates: %d", len(tasks))
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposes, creates int
	for _, link := range links {
		if link.SrcType == core.LineagePlanningBundle && link.SrcID == created.ID {
			if link.Kind == "proposes" {
				proposes++
			}
			if link.Kind == "creates" {
				creates++
			}
		}
	}
	if proposes != 1 || creates != 3 {
		t.Fatalf("bundle lineage proposes=%d creates=%d links=%+v", proposes, creates, links)
	}
}

func TestPlanningBundleRejectsCyclesAndRejectDecisionCreatesNothing(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-reject", Title: "Reject"}, core.RequirementVersion{Content: "Reject", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Reject task sets."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	pending, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "Reject v2", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Reject task sets atomically."}}})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-reject", Goal: core.PlanningGoalBundle})
	base := core.PlanningBundle{ID: "bundle-reject", SessionID: session.ID, Title: "Reject", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: pending.Version}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", Title: "One", Body: "Body", Repo: "conveyor"}}}
	cycle := base
	cycle.ID = "bundle-cycle"
	cycle.Tasks = []core.PlanningBundleTask{{MemberID: "one", Title: "One", Body: "Body", Repo: "conveyor", DependsOn: []string{"two"}}, {MemberID: "two", Title: "Two", Body: "Body", Repo: "conveyor", DependsOn: []string{"one"}}}
	if _, err = st.CreatePlanningBundle(ctx, cycle); err == nil {
		t.Fatal("cycle unexpectedly finalized")
	}
	created, err := st.CreatePlanningBundle(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := st.RejectPlanningBundle(ctx, created.ID)
	if err != nil || rejected.Status != core.PlanningBundleRejected {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
	if _, err = st.ApprovePlanningBundle(ctx, created.ID); err == nil {
		t.Fatal("rejected bundle approved")
	}
	tasks, _ := st.ListTasks(ctx)
	if len(tasks) != 0 {
		t.Fatalf("rejection created tasks: %+v", tasks)
	}
}
