package core

import (
	"testing"
	"time"
)

// The recover route and the next action share one queued eligibility: each
// queued flag alone or combined admits the order, a queued order without any
// is refused with the route's text, stale and timed-out orders are admitted,
// and other states keep their own refusals (component-work-orders, Recovery;
// req-operator-next-action AC-1.4).
func TestRecoverStateRefusalSharesTheRouteEligibility(t *testing.T) {
	retry := time.Date(2026, 10, 17, 10, 30, 0, 0, time.UTC)
	routeText := "work order o is not released, expired, or retry-suppressed"
	cases := []struct {
		name        string
		order       WorkOrder
		recoverable bool
		state       string
		nextAction  string
	}{
		{name: "queued never claimed", order: WorkOrder{State: WorkOrderQueued}, state: routeText, nextAction: routeText},
		{name: "queued after operator recovery", order: WorkOrder{State: WorkOrderQueued, OperatorDirection: "Continue.", RedispatchCount: 1, LastFailureMessage: WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: &WorkOrderCheckpoint{DecisionRequest: "Confirm.", Class: WorkOrderCheckpointClassAuthorityConflict}}, state: routeText, nextAction: routeText},
		{name: "queued released", order: WorkOrder{State: WorkOrderQueued, LastAttemptOutcome: WorkOrderOutcomeReleased}, recoverable: true},
		{name: "queued expired", order: WorkOrder{State: WorkOrderQueued, LastAttemptOutcome: WorkOrderOutcomeExpired}, recoverable: true},
		{name: "queued retry-suppressed", order: WorkOrder{State: WorkOrderQueued, RetrySuppressed: true}, recoverable: true},
		{name: "queued retry scheduled", order: WorkOrder{State: WorkOrderQueued, NextRetryAt: retry}, recoverable: true},
		{name: "queued all flags", order: WorkOrder{State: WorkOrderQueued, LastAttemptOutcome: WorkOrderOutcomeReleased, RetrySuppressed: true, NextRetryAt: retry}, recoverable: true},
		{name: "stale", order: WorkOrder{State: WorkOrderStale}},
		{name: "timed out", order: WorkOrder{State: WorkOrderTimedOut}},
		{name: "claimed", order: WorkOrder{State: WorkOrderClaimed, LastAttemptOutcome: WorkOrderOutcomeReleased}, state: routeText, nextAction: "work order o cannot be recovered while it is claimed"},
		{name: "submitted", order: WorkOrder{State: WorkOrderSubmitted}, state: routeText, nextAction: "work order o cannot be recovered from submitted"},
		{name: "stamped refusal", order: WorkOrder{State: WorkOrderQueued, LastAttemptOutcome: WorkOrderOutcomeReleased, RecoveryRefusal: "frozen policy refuses"}, recoverable: true, nextAction: "frozen policy refuses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.order.ID = "o"
			if got := RecoverableQueuedWorkOrder(tc.order); got != tc.recoverable {
				t.Fatalf("RecoverableQueuedWorkOrder = %v, want %v", got, tc.recoverable)
			}
			if got := WorkOrderRecoverStateRefusal(tc.order); got != tc.state {
				t.Fatalf("WorkOrderRecoverStateRefusal = %q, want %q", got, tc.state)
			}
			if got := NextActionRecoverRefusal(tc.order); got != tc.nextAction {
				t.Fatalf("NextActionRecoverRefusal = %q, want %q", got, tc.nextAction)
			}
		})
	}
}

// The predicate states exactly the route's 409 conditions for every task
// state (req-operator-next-action AC-1.4; component-next-action, Invariants 7).
func TestNextActionRedispatchRefusalStatesTheRouteConditions(t *testing.T) {
	held := []WorkOrder{{ID: "o", Stage: StageImplement, State: WorkOrderStale}}
	cases := []struct {
		name   string
		task   Task
		orders []WorkOrder
		want   string
	}{
		{"running", Task{ID: "t", State: TaskRunning, NextStage: StageImplement}, nil, TaskRedispatchActiveRefusal},
		{"queued and held", Task{ID: "t", State: TaskQueued, NextStage: StageImplement}, held, TaskHeldByWorkOrderRefusal + ": task t waits on work order o"},
		{"queued with another stage next", Task{ID: "t", State: TaskQueued, NextStage: StageReview}, held, ""},
		{"queued with no orders", Task{ID: "t", State: TaskQueued, NextStage: StageImplement}, nil, ""},
		{"queued with no next stage", Task{ID: "t", State: TaskQueued}, held, ""},
		{"parked at its recovery stage", Task{ID: "t", State: TaskParked, RecoveryStage: StageImplement}, held, ""},
		{"parked at its next stage", Task{ID: "t", State: TaskParked, NextStage: StageImplement}, nil, ""},
		{"parked with no stage", Task{ID: "t", State: TaskParked}, nil, TaskRedispatchNoStageRefusal},
		{"awaiting with a stage", Task{ID: "t", State: TaskAwaiting, NextStage: StageImplement}, nil, ""},
		{"awaiting with only a recovery stage", Task{ID: "t", State: TaskAwaiting, RecoveryStage: StageImplement}, nil, TaskRedispatchNoStageRefusal},
		{"approved with a stage", Task{ID: "t", State: TaskApproved, NextStage: StageReview}, nil, ""},
		{"approved with no stage", Task{ID: "t", State: TaskApproved}, nil, TaskRedispatchNoStageRefusal},
		{"claiming with a stage", Task{ID: "t", State: TaskClaiming, NextStage: StageImplement}, nil, ""},
		{"claiming with no stage", Task{ID: "t", State: TaskClaiming}, nil, TaskRedispatchNoStageRefusal},
		{"merged with no stage", Task{ID: "t", State: TaskMerged}, nil, TaskRedispatchNoStageRefusal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextActionRedispatchRefusal(tc.task, tc.orders); got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
	for _, state := range []TaskState{TaskMerged, TaskClosed} {
		got := NextActionRedispatchRefusal(Task{ID: "t", State: state, NextStage: StageImplement}, nil)
		_, err := TransitionTask(state, TaskRecover)
		if err == nil || got != err.Error() {
			t.Fatalf("%s refusal = %q, want the transition error %v", state, got, err)
		}
	}
}

// An unpersisted claim expiry holds the stage as the stored claim does, and
// the caller's projected orders stay unchanged.
func TestNextActionRedispatchRefusalReadsStoredClaims(t *testing.T) {
	orders := []WorkOrder{
		{ID: "claim", Stage: StageImplement, State: WorkOrderQueued, ClaimExpiryUnpersisted: true},
		{ID: "seat", Stage: StageReview, ReviewRound: 1, State: WorkOrderQueued},
	}
	task := Task{ID: "t", State: TaskQueued, NextStage: StageImplement}
	want := TaskHeldByWorkOrderRefusal + ": task t waits on work order claim"
	if got := NextActionRedispatchRefusal(task, orders); got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
	if orders[0].State != WorkOrderQueued {
		t.Fatalf("projected order mutated: %s", orders[0].State)
	}
	orders[0].ClaimExpiryUnpersisted = false
	if got := NextActionRedispatchRefusal(task, orders); got != "" {
		t.Fatalf("persisted expiry refusal = %q", got)
	}
}
