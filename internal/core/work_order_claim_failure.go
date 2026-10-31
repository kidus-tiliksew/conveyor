package core

import "time"

// WorkOrderClaimFailureClassBackendOperation is the only recorded error
// class: a claim that failed with store.ErrBackendOperation
// (component-work-orders, Claim admission).
const WorkOrderClaimFailureClassBackendOperation = "backend_operation"

// WorkOrderClaimFailureCountCap bounds the recorded consecutive count.
const WorkOrderClaimFailureCountCap = 1_000_000

// WorkOrderClaimFailure is the bounded record of an order's consecutive
// claims that failed in the backend. It carries no driver text, SQL,
// session, or claimant, and a successful claim of the order deletes it
// (component-work-orders, Claim admission; component-persistence).
type WorkOrderClaimFailure struct {
	WorkOrderID         string    `json:"work_order_id"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	FirstFailedAt       time.Time `json:"first_failed_at"`
	LastFailedAt        time.Time `json:"last_failed_at"`
	ErrorClass          string    `json:"error_class"`
}

// NextWorkOrderClaimFailureCount is the count after one more failure,
// saturating at WorkOrderClaimFailureCountCap.
func NextWorkOrderClaimFailureCount(current int) int {
	if current < 0 {
		current = 0
	}
	if current >= WorkOrderClaimFailureCountCap {
		return WorkOrderClaimFailureCountCap
	}
	return current + 1
}
