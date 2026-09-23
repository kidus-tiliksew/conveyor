package core

import "time"

// SupersedeVerificationOrder revokes only obsolete verification claims. The
// retained cancellation event carries the old session for stale-call diagnosis.
func SupersedeVerificationOrder(order WorkOrder, head string, now time.Time) (WorkOrder, bool) {
	if order.Stage != StageVerify || order.HeadSHA == head || (order.State != WorkOrderQueued && order.State != WorkOrderClaimed) {
		return order, false
	}
	state, err := TransitionWorkOrder(order.State, WorkOrderCmdCancel)
	if err != nil {
		return order, false
	}
	order.State, order.Claimable, order.UpdatedAt = state, false, now
	order.LastAttemptID = order.AttemptID
	order.RetrySuppressed, order.RetrySuppressionReason = true, "superseded head"
	order.ClaimantID, order.SessionID, order.AttemptID, order.ClientTokenHash, order.WorkerID = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	return order, true
}
