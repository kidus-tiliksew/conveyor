package store

import "github.com/kidus-tiliksew/conveyor/internal/core"

// VerifyClaimRepinsAuthority reports whether a claim of a verify order
// replaces its served-requirement and governance snapshots with the task's
// current authority, given the task's verification rows. A verify order
// serves current authority until a verification record binds its pins: a
// sealed context of the order, or an unresolved operation or unconsumed
// operator recovery authorization of one of its contexts. Each of those
// compares later work with the pins it was recorded against, so the claim
// keeps them (component-work-orders, Claim admission;
// component-verification-service; req-verification-kits AC-2.1). Review
// seats never re-pin: their verdict binds to the sealed context's pins.
func VerifyClaimRepinsAuthority(order core.WorkOrder, rows []VerificationRow) bool {
	if order.Stage != core.StageVerify || order.VerificationContextID != "" || (order.Checkpoint != nil && order.Checkpoint.Verification != nil) {
		return false
	}
	contexts := map[string]bool{}
	for _, r := range rows {
		if r.Table != "verification_contexts" || r.TaskID != order.TaskID {
			continue
		}
		vc := verificationDecode[VerificationContext](r)
		if vc.WorkOrderID != order.ID {
			continue
		}
		if vc.SealedAt != nil {
			return false
		}
		contexts[r.ID] = true
	}
	for _, r := range rows {
		if !contexts[r.ContextID] {
			continue
		}
		switch r.Table {
		case "verification_operations":
			if !verificationOperationResolved(r.State) {
				return false
			}
		case "verification_attempts":
			for _, authorization := range verificationDecode[VerificationAttempt](r).Recovery {
				if authorization.Successor == nil {
					return false
				}
			}
		}
	}
	return true
}
