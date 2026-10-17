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
