package core

import (
	"errors"
	"strings"
	"testing"
)

func TestTaskLifecycleT1ThroughT22(t *testing.T) {
	tests := []struct {
		name    string
		from    TaskState
		command TaskCommand
		to      TaskState
	}{
		{"T1", TaskClaiming, TaskIntakeFinalize, TaskQueued},
		{"T2", TaskQueued, TaskDispatchStart, TaskRunning},
		{"T3", TaskQueued, TaskOrderClaim, TaskRunning},
		{"T4", TaskRunning, TaskStageAdvance, TaskQueued},
		{"T5", TaskRunning, TaskStageBounce, TaskQueued},
		{"T6", TaskRunning, TaskStageBounceLimit, TaskAwaiting},
		{"T7", TaskRunning, TaskJobFail, TaskAwaiting},
		{"T9", TaskRunning, TaskTriagePark, TaskParked},
		{"T10", TaskRunning, TaskGateSpec, TaskAwaiting},
		{"T10a", TaskRunning, TaskGatePlanRevision, TaskAwaiting},
		{"T11", TaskRunning, TaskGateMerge, TaskAwaiting},
		{"T12", TaskQueued, TaskDispatchFailRetry, TaskQueued},
		{"T13", TaskQueued, TaskDispatchFailFinal, TaskParked},
		{"T14", TaskAwaiting, TaskInterventionReject, TaskClosed},
		{"T15", TaskAwaiting, TaskInterventionApproveSpec, TaskQueued},
		{"T16", TaskAwaiting, TaskInterventionApproveReview, TaskApproved},
		{"T17", TaskAwaiting, TaskInterventionRedirect, TaskQueued},
		{"T18", TaskApproved, TaskMergeConfirm, TaskMerged},
		{"T19", TaskApproved, TaskRefreshReview, TaskQueued},
		{"T20", TaskApproved, TaskConflictDispatch, TaskQueued},
		{"T21", TaskParked, TaskRecover, TaskQueued},
	}
	if _, err := TransitionTask(TaskQueued, TaskGatePlanRevision); err == nil {
		t.Fatal("gate.plan_revision accepted outside running")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := TransitionTask(test.from, test.command)
			if err != nil || got != test.to {
				t.Fatalf("transition = %q, %v; want %q", got, err, test.to)
			}
		})
	}
	for _, from := range []TaskState{TaskClaiming, TaskQueued, TaskRunning, TaskAwaiting, TaskApproved, TaskParked} {
		if got, err := TransitionTask(from, TaskCancel); err != nil || got != TaskClosed {
			t.Fatalf("T22 from %q = %q, %v", from, got, err)
		}
	}
	if _, err := TransitionTask(TaskRunning, TaskCommand("triage.route_human")); err == nil {
		t.Fatal("retired triage.route_human command remains in the current lifecycle")
	}
}

func TestTransitionConflictDispatchSelectsInitialAndRecoveryEdges(t *testing.T) {
	tests := []struct {
		from    TaskState
		command TaskCommand
	}{
		{from: TaskApproved, command: TaskConflictDispatch},
		{from: TaskQueued, command: TaskRecoverRefresh},
		{from: TaskRunning, command: TaskRecoverRefresh},
	}
	for _, test := range tests {
		to, command, err := TransitionConflictDispatch(test.from)
		if err != nil || to != TaskQueued || command != test.command {
			t.Fatalf("from %s: to=%s command=%s err=%v", test.from, to, command, err)
		}
	}
	if _, _, err := TransitionConflictDispatch(TaskAwaiting); err == nil {
		t.Fatal("awaiting task accepted conflict dispatch recovery")
	}
}

func TestWorkOrderLifecycleW1ThroughW15(t *testing.T) {
	tests := []struct {
		name    string
		from    WorkOrderState
		command WorkOrderCommand
		to      WorkOrderState
	}{
		{"W1", "", WorkOrderCmdCreate, WorkOrderQueued},
		{"W2", WorkOrderQueued, WorkOrderCmdClaim, WorkOrderClaimed},
		{"W3", WorkOrderClaimed, WorkOrderCmdRenew, WorkOrderClaimed},
		{"W4", WorkOrderClaimed, WorkOrderCmdRelease, WorkOrderQueued},
		{"W4a", WorkOrderClaimed, WorkOrderCmdRequestPlanRevision, WorkOrderQueued},
		{"W5", WorkOrderClaimed, WorkOrderCmdExpire, WorkOrderQueued},
		{"W6", WorkOrderClaimed, WorkOrderCmdSubmitForReview, WorkOrderSubmitted},
		{"W7", WorkOrderClaimed, WorkOrderCmdSubmitSpec, WorkOrderCompleted},
		{"W8", WorkOrderClaimed, WorkOrderCmdSubmitReviewVerdict, WorkOrderCompleted},
		{"W9", WorkOrderSubmitted, WorkOrderCmdReviewTerminal, WorkOrderCompleted},
		{"W10", WorkOrderSubmitted, WorkOrderCmdReviewRevise, WorkOrderClaimed},
	}
	for _, from := range []WorkOrderState{WorkOrderQueued, WorkOrderClaimed} {
		tests = append(tests, struct {
			name    string
			from    WorkOrderState
			command WorkOrderCommand
			to      WorkOrderState
		}{"W11", from, WorkOrderCmdTimeout, WorkOrderTimedOut})
	}
	for _, from := range []WorkOrderState{WorkOrderQueued, WorkOrderClaimed, WorkOrderSubmitted} {
		tests = append(tests, struct {
			name    string
			from    WorkOrderState
			command WorkOrderCommand
			to      WorkOrderState
		}{"W12", from, WorkOrderCmdMarkStale, WorkOrderStale})
	}
	for _, from := range []WorkOrderState{WorkOrderTimedOut, WorkOrderStale} {
		tests = append(tests, struct {
			name    string
			from    WorkOrderState
			command WorkOrderCommand
			to      WorkOrderState
		}{"W13", from, WorkOrderCmdRecover, WorkOrderQueued})
	}
	tests = append(tests, struct {
		name    string
		from    WorkOrderState
		command WorkOrderCommand
		to      WorkOrderState
	}{"W14", WorkOrderStale, WorkOrderCmdRedispatch, WorkOrderQueued})
	for _, from := range []WorkOrderState{WorkOrderQueued, WorkOrderStale} {
		tests = append(tests, struct {
			name    string
			from    WorkOrderState
			command WorkOrderCommand
			to      WorkOrderState
		}{"W16", from, WorkOrderCmdPreempt, WorkOrderCancelled})
	}
	for _, from := range []WorkOrderState{WorkOrderQueued, WorkOrderClaimed, WorkOrderSubmitted, WorkOrderStale, WorkOrderTimedOut} {
		tests = append(tests, struct {
			name    string
			from    WorkOrderState
			command WorkOrderCommand
			to      WorkOrderState
		}{"W15", from, WorkOrderCmdCancel, WorkOrderCancelled})
	}
	for _, test := range tests {
		t.Run(test.name+"/"+string(test.from), func(t *testing.T) {
			got, err := TransitionWorkOrder(test.from, test.command)
			if err != nil || got != test.to {
				t.Fatalf("transition = %q, %v; want %q", got, err, test.to)
			}
		})
	}
}

func TestWorkOrderLifecycleRejectsRedispatchOutsideW14(t *testing.T) {
	for _, from := range []WorkOrderState{WorkOrderQueued, WorkOrderClaimed, WorkOrderSubmitted, WorkOrderTimedOut} {
		t.Run(string(from), func(t *testing.T) {
			if _, err := TransitionWorkOrder(from, WorkOrderCmdRedispatch); err == nil {
				t.Fatalf("order.redispatch unexpectedly allowed from %q", from)
			}
		})
	}
}

func TestInvalidTransitionCarriesAllowedAlternatives(t *testing.T) {
	_, err := TransitionTask(TaskMerged, TaskCancel)
	var invalid *ErrInvalidTransition
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %T %v", err, err)
	}
	if invalid.Space != TaskLifecycle || invalid.From != string(TaskMerged) || invalid.Command != string(TaskCancel) || len(invalid.Allowed) != 0 {
		t.Fatalf("invalid transition = %#v", invalid)
	}
	_, err = TransitionWorkOrder(WorkOrderSubmitted, WorkOrderCmdClaim)
	if !errors.As(err, &invalid) || len(invalid.Allowed) == 0 {
		t.Fatalf("allowed alternatives missing: %#v", invalid)
	}
}

func TestLifecycleStateDiagramIsGeneratedFromCanonicalTables(t *testing.T) {
	diagram := LifecycleStateDiagram()
	if strings.Contains(diagram, "triage.route_human") {
		t.Fatalf("diagram contains retired triage route:\n%s", diagram)
	}
	for _, edge := range []string{
		"task_claiming --> task_queued: intake.finalize",
		"task_approved --> task_merged: merge.confirm",
		"order_claimed --> order_queued: claim.expire",
		"order_queued --> order_timed_out: order.timeout",
	} {
		if !strings.Contains(diagram, edge) {
			t.Fatalf("diagram missing %q:\n%s", edge, diagram)
		}
	}
}
