package core

import (
	"errors"
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
		{"T8", TaskRunning, TaskTriageRouteHuman, TaskAwaiting},
		{"T9", TaskRunning, TaskTriagePark, TaskParked},
		{"T10", TaskRunning, TaskGateSpec, TaskAwaiting},
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
