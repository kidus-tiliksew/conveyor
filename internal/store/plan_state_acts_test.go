package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func planActOrder(id string, stage core.Stage, state core.WorkOrderState) core.WorkOrder {
	return core.WorkOrder{ID: id, TaskID: "t", Stage: stage, State: state}
}

// TestPlanPlanStateChangeEffectsTable covers every row of
// component-plan-state invariant 4's effects table and each refusal, and
// checks plan_acts reports exactly what the change accepts
// (component-plan-state invariants 4 and 6, Verification).
func TestPlanPlanStateChangeEffectsTable(t *testing.T) {
	type want struct {
		refused   bool
		command   core.TaskCommand
		next      core.Stage
		cancelled []string
	}
	cases := []struct {
		name   string
		task   core.Task
		orders []core.WorkOrder
		to     core.PlanState
		want   want
	}{
		{"triage unfinished to needed", core.Task{State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNotRequired}, nil, core.PlanNeeded, want{}},
		{"triage unfinished to not_required", core.Task{State: core.TaskRunning, NextStage: core.StageTriage, PlanState: core.PlanAwaitingApproval}, nil, core.PlanNotRequired, want{}},
		{"parked to needed", core.Task{State: core.TaskParked, NextStage: core.StageImplement, PlanState: core.PlanNotRequired}, nil, core.PlanNeeded, want{}},
		{"plan order queued to not_required", core.Task{State: core.TaskQueued, NextStage: core.StageSpec, PlanState: core.PlanNeeded},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderQueued), planActOrder("spec-0", core.StageSpec, core.WorkOrderCancelled)}, core.PlanNotRequired,
			want{command: core.TaskPlanReroute, next: core.StageImplement, cancelled: []string{"spec-1"}}},
		{"plan order claimed to not_required", core.Task{State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderClaimed)}, core.PlanNotRequired,
			want{command: core.TaskPlanReroute, next: core.StageImplement, cancelled: []string{"spec-1"}}},
		{"plan order queued to needed names the current state", core.Task{State: core.TaskQueued, NextStage: core.StageSpec, PlanState: core.PlanNeeded},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderQueued)}, core.PlanNeeded, want{refused: true}},
		{"plan approval gate to needed", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, RecoveryStage: core.StageImplement, PlanState: core.PlanAwaitingApproval},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderCompleted)}, core.PlanNeeded,
			want{command: core.TaskPlanReroute, next: core.StageSpec}},
		{"plan approval gate to not_required", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, RecoveryStage: core.StageImplement, PlanState: core.PlanAwaitingApproval},
			nil, core.PlanNotRequired, want{command: core.TaskPlanReroute, next: core.StageImplement}},
		{"implement order queued to needed", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanNotRequired},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderQueued)}, core.PlanNeeded,
			want{command: core.TaskPlanReroute, next: core.StageSpec, cancelled: []string{"impl-1"}}},
		{"implement order queued to not_required", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanCompleted},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderQueued)}, core.PlanNotRequired, want{}},
		{"verify next to needed", core.Task{State: core.TaskQueued, NextStage: core.StageVerify, PlanState: core.PlanNotRequired}, nil, core.PlanNeeded, want{refused: true}},
		{"review next to needed", core.Task{State: core.TaskQueued, NextStage: core.StageReview, PlanState: core.PlanCompleted}, nil, core.PlanNeeded, want{refused: true}},
		{"review next to not_required", core.Task{State: core.TaskQueued, NextStage: core.StageReview, PlanState: core.PlanCompleted}, nil, core.PlanNotRequired, want{}},
		{"claimed implement order", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanNotRequired},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderClaimed)}, core.PlanNeeded, want{refused: true}},
		{"claimed implement order to not_required", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanCompleted},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderClaimed)}, core.PlanNotRequired, want{refused: true}},
		{"submitted review verdict", core.Task{State: core.TaskRunning, NextStage: core.StageReview, PlanState: core.PlanCompleted},
			[]core.WorkOrder{planActOrder("review-1", core.StageReview, core.WorkOrderSubmitted)}, core.PlanNotRequired, want{refused: true}},
		{"terminal", core.Task{State: core.TaskClosed, PlanState: core.PlanNeeded}, nil, core.PlanNotRequired, want{refused: true}},
		{"merged", core.Task{State: core.TaskMerged, PlanState: core.PlanNeeded}, nil, core.PlanNotRequired, want{refused: true}},
		{"approved", core.Task{State: core.TaskApproved, PlanState: core.PlanCompleted}, nil, core.PlanNotRequired, want{refused: true}},
		{"other gate without next stage", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, PlanState: core.PlanNotRequired}, nil, core.PlanNeeded, want{refused: true}},
		{"merge gate", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, PlanState: core.PlanCompleted}, nil, core.PlanNotRequired, want{refused: true}},
		{"current state", core.Task{State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNeeded}, nil, core.PlanNeeded, want{refused: true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.task.ID = "t"
			effects, err := PlanPlanStateChange(test.task, test.orders, test.to)
			if test.want.refused {
				if !errors.Is(err, ErrSetupChangeConflict) {
					t.Fatalf("err = %v, want a 409 refusal", err)
				}
			} else {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				cancelled := test.want.cancelled
				if cancelled == nil {
					cancelled = []string{}
				}
				if effects.Command != test.want.command || effects.NextStage != test.want.next || !reflect.DeepEqual(effects.CancelWorkOrderIDs, cancelled) ||
					effects.From != test.task.PlanState || effects.To != test.to || effects.Cause != core.PlanCauseOperator {
					t.Fatalf("effects = %+v, want %+v", effects, test.want)
				}
			}
			acts := PlanActsFor(test.task, test.orders)
			offered := acts.SetNeeded
			if test.to == core.PlanNotRequired {
				offered = acts.SetNotRequired
			}
			if offered != !test.want.refused {
				t.Fatalf("plan_acts = %+v, change accepted = %t", acts, !test.want.refused)
			}
		})
	}
	if _, err := PlanPlanStateChange(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNeeded}, nil, core.PlanAwaitingApproval); err == nil || errors.Is(err, ErrSetupChangeConflict) {
		t.Fatalf("awaiting_approval target err = %v, want a 400 refusal", err)
	}
}

// TestPlanAttachPlanPredicate covers component-plan-state invariant 5's
// predicate and effects, and checks plan_acts.attach matches it.
func TestPlanAttachPlanPredicate(t *testing.T) {
	cases := []struct {
		name      string
		task      core.Task
		orders    []core.WorkOrder
		refused   bool
		command   core.TaskCommand
		cancelled []string
	}{
		{"before triage", core.Task{State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNeeded}, nil, false, "", []string{}},
		{"parked before triage", core.Task{State: core.TaskParked, NextStage: core.StageTriage, PlanState: core.PlanNotRequired}, nil, false, "", []string{}},
		{"plan order queued", core.Task{State: core.TaskQueued, NextStage: core.StageSpec, PlanState: core.PlanNeeded},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderQueued)}, false, core.TaskGateSpec, []string{"spec-1"}},
		{"plan order claimed", core.Task{State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded},
			[]core.WorkOrder{planActOrder("spec-1", core.StageSpec, core.WorkOrderClaimed)}, false, core.TaskGateSpec, []string{"spec-1"}},
		{"implement order queued", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanNotRequired},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderQueued), planActOrder("impl-0", core.StageImplement, core.WorkOrderCompleted)}, false, core.TaskGateSpec, []string{"impl-1"}},
		{"implement order claimed", core.Task{State: core.TaskRunning, NextStage: core.StageImplement, PlanState: core.PlanNotRequired},
			[]core.WorkOrder{planActOrder("impl-1", core.StageImplement, core.WorkOrderClaimed)}, true, "", nil},
		{"awaiting approval", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, PlanState: core.PlanAwaitingApproval}, nil, true, "", nil},
		{"completed", core.Task{State: core.TaskQueued, NextStage: core.StageImplement, PlanState: core.PlanCompleted}, nil, true, "", nil},
		{"review next", core.Task{State: core.TaskQueued, NextStage: core.StageReview, PlanState: core.PlanNotRequired}, nil, true, "", nil},
		{"verify next", core.Task{State: core.TaskQueued, NextStage: core.StageVerify, PlanState: core.PlanNotRequired}, nil, true, "", nil},
		{"terminal", core.Task{State: core.TaskClosed, NextStage: core.StageImplement, PlanState: core.PlanNotRequired}, nil, true, "", nil},
		{"parked after triage", core.Task{State: core.TaskParked, NextStage: core.StageImplement, PlanState: core.PlanNotRequired}, nil, true, "", nil},
		{"job failure gate", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, NextStage: core.StageImplement, PlanState: core.PlanNotRequired}, nil, true, "", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.task.ID = "t"
			effects, err := PlanAttachPlan(test.task, test.orders)
			if test.refused != (err != nil) || (err != nil && !errors.Is(err, ErrSetupChangeConflict)) {
				t.Fatalf("err = %v, refused want %t", err, test.refused)
			}
			if !test.refused {
				if effects.Command != test.command || !reflect.DeepEqual(effects.CancelWorkOrderIDs, test.cancelled) || effects.To != core.PlanAwaitingApproval || effects.Cause != core.PlanCausePlanAttached {
					t.Fatalf("effects = %+v", effects)
				}
				if test.command == core.TaskGateSpec && effects.RecoveryStage != core.StageImplement {
					t.Fatalf("gate.spec recovery stage = %q", effects.RecoveryStage)
				}
			}
			if acts := PlanActsFor(test.task, test.orders); acts.Attach != !test.refused {
				t.Fatalf("plan_acts.attach = %t, want %t", acts.Attach, !test.refused)
			}
		})
	}
}

// TestTaskPolicyChangePlanStateTravelsAlone covers component-task-lifecycle
// invariant 44's plan_state body rule.
func TestTaskPolicyChangePlanStateTravelsAlone(t *testing.T) {
	needed, awaiting, on := core.PlanNeeded, core.PlanAwaitingApproval, true
	if err := (TaskPolicyChange{PlanState: &needed}).Validate(); err != nil {
		t.Fatalf("plan_state alone: %v", err)
	}
	for name, change := range map[string]TaskPolicyChange{
		"awaiting_approval":    {PlanState: &awaiting},
		"with verify_stage":    {PlanState: &needed, VerifyStage: &on},
		"with distinct models": {PlanState: &needed, ReviewDistinctModels: &on},
		"with verify timeout":  {PlanState: &needed, StageTimeouts: map[string]string{"verify": "1h"}},
	} {
		if err := change.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// TestResolveTriageProceedReplacesTheStaleStep checks that a triage proceed
// command takes the step of the plan state the backend read, and that every
// other command passes unchanged (component-plan-state invariant 5).
func TestResolveTriageProceedReplacesTheStaleStep(t *testing.T) {
	stale := taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageImplement, TriageProceed: true}
	got, err := ResolveTriageProceed(stale, core.PlanAwaitingApproval)
	if err != nil || got.Kind != core.TaskGateSpec || got.NextStage != "" || got.RecoveryStage != core.StageImplement || !got.ProjectStages {
		t.Fatalf("awaiting_approval: %+v err=%v", got, err)
	}
	if got, err = ResolveTriageProceed(stale, core.PlanNeeded); err != nil || got.Kind != core.TaskStageAdvance || got.NextStage != core.StageSpec {
		t.Fatalf("needed: %+v err=%v", got, err)
	}
	if _, err = ResolveTriageProceed(stale, ""); err == nil {
		t.Fatal("an empty plan state selected a proceed step")
	}
	other := taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview}
	if got, err = ResolveTriageProceed(other, core.PlanNeeded); err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("a non-proceed command changed: %+v err=%v", got, err)
	}
}
