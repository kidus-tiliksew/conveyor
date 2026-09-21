package store

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// VerifyVerificationClaimLoss is only for the internal reconciler. Callers must
// hold the task/order transaction lock; ordinary claim checks remain unchanged
// (feature-verification-kit-execution VK-4, req-verification-kits AC-3.3).
func VerifyVerificationClaimLoss(ctx context.Context, a VerificationAccess, task core.Task, o core.WorkOrder, now time.Time) error {
	ws, ok := WorkspaceFromContext(ctx)
	actor := ActorFromContext(ctx)
	if !ok || actor.Role != core.ActorSystem || actor.ID != "verification-reconciler" || a.UserID != "" || a.TaskID == "" || task.ID != a.TaskID || task.Workspace != ws || o.ID != a.WorkOrderID || o.TaskID != a.TaskID || o.Stage != core.StageVerify || a.WorkOrderAttemptID == "" {
		return ErrVerificationAccess
	}
	if o.AttemptID == a.WorkOrderAttemptID && o.State == core.WorkOrderClaimed && o.LeaseExpiresAt.After(now) && (o.ExecutionDeadline.IsZero() || o.ExecutionDeadline.After(now)) {
		return ErrVerificationState
	}
	return nil
}

func prepareVerificationClaimLoss(ctx context.Context, c VerificationCommand, rows []VerificationRow, now time.Time) (VerificationMutation, error) {
	var out VerificationMutation
	actor := ActorFromContext(ctx)
	if actor.Role != core.ActorSystem || actor.ID != "verification-reconciler" {
		return out, ErrVerificationAccess
	}
	contextRow, ok := verificationFind(rows, "verification_contexts", c.ContextID)
	if !ok || contextRow.TaskID != c.Access.TaskID {
		return out, ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](contextRow)
	row, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || row.TaskID != c.Access.TaskID || row.ContextID != c.ContextID || vc.WorkOrderID != c.Access.WorkOrderID || vc.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return out, ErrVerificationAccess
	}
	v := verificationDecode[VerificationAttempt](row)
	if v.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return out, ErrVerificationAccess
	}
	out.Receipt = VerificationReceipt{ID: v.ID, State: v.State}
	// A terminal report that won the transaction race is immutable.
	if v.EndedAt != nil {
		return out, nil
	}
	if vc.SealedAt != nil || (v.State != "running" && v.State != "pending") {
		return out, ErrVerificationState
	}
	v.State, v.Explanation, v.EndedAt = "cancelled", "claim_lost", &now
	row.State, row.Body = v.State, verificationJSON(v)
	out.Rows = append(out.Rows, row)
	out.Receipt.State = v.State
	for _, other := range rows {
		if other.Table == "verification_operations" && other.TaskID == c.Access.TaskID && other.ContextID == c.ContextID && other.RunID == v.ID && (other.State == "registered" || other.State == "dispatching") {
			op := verificationDecode[VerificationOperation](other)
			op.History = append(op.History, VerificationOperationObservation{State: "outcome_unknown", Actor: actor.ID, Source: "claim_lost", CapturedAt: now})
			other.State, other.Body = "outcome_unknown", verificationJSON(op)
			out.Rows = append(out.Rows, other)
		}
	}
	out.Event = core.Event{TaskID: c.Access.TaskID, Kind: "verification.attempt.terminate", ActorID: actor.ID, ActorRole: actor.Role, Payload: verificationJSON(out.Receipt)}
	return out, nil
}

// VerificationReconciliationCommands builds candidates only. ApplyVerification
// rechecks the authoritative order under its lock before every cancellation.
func VerificationReconciliationCommands(ctx context.Context, rows []VerificationRow) ([]VerificationCommand, error) {
	actor := ActorFromContext(ctx)
	if actor.Role != core.ActorSystem || actor.ID != "verification-reconciler" {
		return nil, ErrVerificationAccess
	}
	var result []VerificationCommand
	for _, row := range rows {
		if row.Table != "verification_attempts" || (row.State != "running" && row.State != "pending") {
			continue
		}
		cr, ok := verificationFind(rows, "verification_contexts", row.ContextID)
		if !ok {
			return nil, ErrVerificationInvalid
		}
		vc := verificationDecode[VerificationContext](cr)
		result = append(result, VerificationCommand{Kind: VerificationReconcileClaimLoss, ContextID: vc.ID, RunID: row.ID, Access: VerificationAccess{TaskID: vc.TaskID, WorkOrderID: vc.WorkOrderID, WorkOrderAttemptID: vc.WorkOrderAttemptID}})
	}
	return result, nil
}
