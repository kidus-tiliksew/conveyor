package core

import "time"

// NormalizeLegacyStaleWorkOrder returns a stored order that still holds the
// retired stale value as the queued order the clock persists: same queue
// position, checkpoint, dependencies, pins, and attempt history, and no
// recovery counted (component-work-orders invariant 5). It does not touch an
// order in any other state.
//
// A stale order came only from the retired queue clock, so a scheduled
// automatic recovery or an automatic-recovery exhaustion on it is an age-only
// signal and is cleared. When the order also carries genuine attempt evidence,
// a lease that expired or attempt retries that ran out, those signals belong to
// that failure and stay, so the shared automatic recovery decision still
// handles it (DEC-117(1), (3)).
func NormalizeLegacyStaleWorkOrder(order WorkOrder, now time.Time) WorkOrder {
	if order.State != WorkOrderStale {
		return order
	}
	order.State = WorkOrderQueued
	if !genuineAttemptFailure(order) {
		order.NextAutomaticRecoveryAt = time.Time{}
		if order.RetrySuppressionReason == AutomaticRecoveryExhaustedReason {
			order.RetrySuppressed = false
			order.RetrySuppressionReason = ""
		}
	}
	order.Claimable = order.ClaimableAt(now) && order.QueueBlockedAt.IsZero()
	return order
}

// genuineAttemptFailure reports attempt evidence that automatic recovery acts
// on: an expired lease or exhausted attempt retries (component-work-orders
// invariant 46).
func genuineAttemptFailure(order WorkOrder) bool {
	return order.LastAttemptOutcome == WorkOrderOutcomeExpired ||
		order.RetrySuppressionReason == AttemptRetriesExhaustedReason
}
