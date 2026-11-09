package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestResolveIntakePlanState covers component-plan-state invariant 1's order:
// a filed plan, then plan_state, then the legacy spec_approval, then the
// legacy level, then the workspace default, which is needed when unset.
func TestResolveIntakePlanState(t *testing.T) {
	needed, notRequired := core.PlanNeeded, core.PlanNotRequired
	on, off := true, false
	for _, test := range []struct {
		name     string
		request  PlanStateRequest
		fallback core.PlanState
		want     core.PlanState
	}{
		{"a plan awaits approval", PlanStateRequest{HasPlan: true, Level: core.L0, SpecApproval: &on}, notRequired, core.PlanAwaitingApproval},
		{"an approved plan is completed", PlanStateRequest{HasPlan: true, PlanApproved: true}, needed, core.PlanCompleted},
		{"plan_state wins", PlanStateRequest{PlanState: &notRequired, SpecApproval: &on, Level: core.L3}, needed, core.PlanNotRequired},
		{"alias true", PlanStateRequest{SpecApproval: &on, Level: core.L0}, notRequired, core.PlanNeeded},
		{"alias false", PlanStateRequest{SpecApproval: &off, Level: core.L3}, needed, core.PlanNotRequired},
		{"level L0", PlanStateRequest{Level: core.L0}, needed, core.PlanNotRequired},
		{"level L1", PlanStateRequest{Level: core.L1}, needed, core.PlanNotRequired},
		{"level L2", PlanStateRequest{Level: core.L2}, notRequired, core.PlanNeeded},
		{"level L3", PlanStateRequest{Level: core.L3}, notRequired, core.PlanNeeded},
		{"workspace default", PlanStateRequest{}, notRequired, core.PlanNotRequired},
		{"unset workspace default", PlanStateRequest{}, "", core.PlanNeeded},
	} {
		if got := ResolveIntakePlanState(test.request, test.fallback); got != test.want {
			t.Fatalf("%s: got %s, want %s", test.name, got, test.want)
		}
	}
}

// TestPrepareTaskPlanState covers the retained column bridge and the
// refusals a creating transaction applies before any write.
func TestPrepareTaskPlanState(t *testing.T) {
	for _, state := range core.PlanStates() {
		task := core.Task{PlanState: state, SpecApproval: !SpecApprovalBridge(state)}
		if err := PrepareTaskPlanState(&task); err != nil || task.SpecApproval != SpecApprovalBridge(state) {
			t.Fatalf("%s: spec_approval=%t err=%v", state, task.SpecApproval, err)
		}
	}
	if !SpecApprovalBridge(core.PlanNeeded) || !SpecApprovalBridge(core.PlanAwaitingApproval) || SpecApprovalBridge(core.PlanCompleted) || SpecApprovalBridge(core.PlanNotRequired) {
		t.Fatal("bridge routes completed or not_required to planning, or skips planning for needed or awaiting_approval")
	}
	legacy := core.Task{PolicyVersion: 1, SpecApproval: true}
	if err := PrepareTaskPlanState(&legacy); err != nil || legacy.PlanState != core.PlanNeeded || !legacy.SpecApproval {
		t.Fatalf("legacy task=%+v err=%v", legacy, err)
	}
	planned := core.Task{PlanState: core.PlanNotRequired, IntakePlan: &core.IntakePlan{Content: "plan", Approved: true}}
	if err := PrepareTaskPlanState(&planned); err != nil || planned.PlanState != core.PlanCompleted || planned.SpecApproval {
		t.Fatalf("planned task=%+v err=%v", planned, err)
	}
	for name, task := range map[string]core.Task{
		"unknown state":     {PlanState: "planned"},
		"empty plan":        {IntakePlan: &core.IntakePlan{Content: "  "}},
		"short base commit": {IntakePlan: &core.IntakePlan{Content: "plan", BaseCommit: "abc"}},
	} {
		if err := PrepareTaskPlanState(&task); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if causes := core.PlanStateCauses(); len(causes) != 8 {
		t.Fatalf("plan-state causes=%v, want the eight of component-plan-state invariant 3", causes)
	}
}
