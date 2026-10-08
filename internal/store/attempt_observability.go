package store

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Attempt-ending capture refusals (req-260820-221be8 AC-2.1, AC-2.3; DEC-26;
// component-work-orders). Identity mismatches use
// ErrWorkOrderClaimUnauthorized so a caller that only knows an ID learns
// nothing about another claimant's attempt.
var (
	// ErrAttemptCaptureUnverified reports that the persisted order row does
	// not identify how the named attempt ended: it is still the active claim,
	// it ended through unmediated lease expiry or an execution-clock timeout,
	// or a later lifecycle write no longer attributes the row's ending to it.
	// Conveyor never invents a reason, so no capture is recorded.
	ErrAttemptCaptureUnverified = errors.New("attempt ending unverified")
	// ErrAttemptCaptureReasonConflict reports a declared termination reason
	// that differs from the attempt's persisted ending or from the reason an
	// earlier capture of the same attempt already recorded.
	ErrAttemptCaptureReasonConflict = errors.New("attempt capture termination reason conflicts with the persisted ending")
)

// AttemptCaptureClaimEventMatches authenticates a late attempt-ending capture
// against the immutable work_order.claimed event of that exact attempt, for
// any stage. The current order row is deliberately not consulted for
// identity: a successor may already hold its claim fields. Worker-plane
// claims carry WorkerID == ClaimantID == worker ID; run-plane claims carry an
// empty WorkerID and core.TaskRunClaimantID(user). An empty claimant never
// matches, so an anonymous claim cannot be captured by an empty identity.
func AttemptCaptureClaimEventMatches(event core.Event, order core.WorkOrder, claim core.WorkOrderClaimIdentity, capture core.WorkOrderAttemptCapture) bool {
	if event.Kind != "work_order.claimed" || !AttemptCaptureIdentityComplete(claim, capture) {
		return false
	}
	var claimed core.WorkOrder
	return json.Unmarshal(event.Payload, &claimed) == nil && claimed.ID == order.ID &&
		claimed.AttemptID == capture.AttemptID && claimed.SessionID == capture.SessionID &&
		claimed.ClaimantID == claim.ClaimantID && claimed.WorkerID == claim.WorkerID
}

// AttemptCaptureIdentityComplete rejects identities that cannot name one
// immutable claim. A non-empty claim session must be the captured session.
// Backends that express the claim-event predicate natively in SQL apply it
// before querying.
func AttemptCaptureIdentityComplete(claim core.WorkOrderClaimIdentity, capture core.WorkOrderAttemptCapture) bool {
	return capture.AttemptID != "" && capture.SessionID != "" && claim.ClaimantID != "" &&
		(claim.SessionID == "" || claim.SessionID == capture.SessionID)
}

// NormalizeAttemptCapture trims the identity and declared reason. Content is
// left untouched; the worker service bounds and redacts it.
func NormalizeAttemptCapture(capture core.WorkOrderAttemptCapture) core.WorkOrderAttemptCapture {
	capture.SessionID = strings.TrimSpace(capture.SessionID)
	capture.AttemptID = strings.TrimSpace(capture.AttemptID)
	capture.TerminationReason = strings.TrimSpace(capture.TerminationReason)
	return capture
}

// AttemptCaptureReason derives the authoritative termination reason for an
// authenticated capture. When the attempt already has a capture, its recorded
// reason is final: a retry can never rewrite it, and a later lifecycle write
// to the row (a successor's release) cannot make the duplicate fail. Without
// a prior capture the reason comes only from the attempt's own persisted
// ending on the locked order row.
func AttemptCaptureReason(order core.WorkOrder, capture core.WorkOrderAttemptCapture, existingReason string, existing bool) (string, error) {
	reason := existingReason
	if !existing {
		derived, ok := order.AttemptEndingReason(capture.AttemptID)
		if !ok {
			return "", ErrAttemptCaptureUnverified
		}
		reason = derived
	}
	if declared := strings.TrimSpace(capture.TerminationReason); declared != "" && declared != reason {
		return "", ErrAttemptCaptureReasonConflict
	}
	return reason, nil
}
