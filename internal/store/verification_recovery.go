package store

import (
	"context"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// req-verification-kits AC-3.2 through AC-3.4 and AC-7.3; feature-verification-kit-execution VK-4.1.

// VerificationRecoveryDisposition is part of the existing operator recovery
// request. It is never admitted through agent-facing verification tools.
type VerificationRecoveryDisposition struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
	Disposition  string   `json:"disposition"`
	Reason       string   `json:"reason"`
}

type verificationRecoveryKey struct{}

func WithVerificationRecovery(ctx context.Context, disposition *VerificationRecoveryDisposition) context.Context {
	return context.WithValue(ctx, verificationRecoveryKey{}, disposition)
}

func VerificationRecoveryFromContext(ctx context.Context) *VerificationRecoveryDisposition {
	v, _ := ctx.Value(verificationRecoveryKey{}).(*VerificationRecoveryDisposition)
	return v
}

func AuthorizeVerificationRecovery(ctx context.Context, membership MembershipStore) error {
	if VerificationRecoveryFromContext(ctx) == nil {
		return nil
	}
	actor := ActorFromContext(ctx)
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || actor.Role != core.ActorUser || !strings.HasPrefix(actor.ID, "user:") {
		return ErrVerificationAccess
	}
	allowed, err := membership.AuthorizeWorkspace(ctx, strings.TrimPrefix(actor.ID, "user:"), ws, core.CapabilityOperateGates)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrVerificationAccess
	}
	return nil
}

// PrepareVerificationRecovery stages immutable operator disposition separately
// from later reconciliation observations. Request replay must match every field.
func PrepareVerificationRecovery(ctx context.Context, task core.Task, order core.WorkOrder, rows []VerificationRow, requestID string, now time.Time) ([]VerificationRow, []core.Event, error) {
	d := VerificationRecoveryFromContext(ctx)
	if d == nil {
		return nil, nil, nil
	}
	actor := ActorFromContext(ctx)
	if actor.Role != core.ActorUser || order.Stage != core.StageVerify || strings.TrimSpace(d.Reason) == "" || d.OperationIDs == nil {
		return nil, nil, ErrVerificationAccess
	}
	switch d.Disposition {
	case "applied", "not_applied", "unknown":
	default:
		return nil, nil, ErrVerificationInvalid
	}
	cr, ok := verificationFind(rows, "verification_contexts", d.ContextID)
	if !ok || cr.TaskID != task.ID {
		return nil, nil, ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](cr)
	if vc.WorkOrderID != order.ID || order.HeadSHA != core.VerifyStageHead(task) {
		return nil, nil, ErrVerificationState
	}
	if err := VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order); err != nil {
		return nil, nil, err
	}
	rr, ok := verificationFind(rows, "verification_attempts", d.RunID)
	if !ok || rr.ContextID != vc.ID || rr.TaskID != task.ID {
		return nil, nil, ErrVerificationAccess
	}
	run := verificationDecode[VerificationAttempt](rr)
	if run.WorkOrderAttemptID != vc.WorkOrderAttemptID || verificationHash(verificationJSON(run.SafeInputs)) != d.InputDigest {
		return nil, nil, ErrVerificationConflict
	}
	original := core.VerificationBinding{ReviewScope: vc.ReviewScope, BaselineSHA: vc.BaselineSHA, ContextID: vc.ID, WorkOrderID: vc.WorkOrderID, WorkOrderAttemptID: vc.WorkOrderAttemptID, RunID: run.ID, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins}
	id := "recovery:" + requestID
	requestDigest := verificationHash(verificationJSON(d))
	if originalDigest, ok := ctx.Value(verificationRecoveryDigestKey{}).(string); ok {
		requestDigest = originalDigest
	}
	for _, a := range run.Recovery {
		if a.ID == id {
			if a.RequestDigest != requestDigest || a.Actor != actor.ID {
				return nil, nil, ErrVerificationConflict
			}
			return nil, nil, nil
		}
	}
	if order.State == core.WorkOrderClaimed || order.State == core.WorkOrderCompleted {
		return nil, nil, ErrVerificationState
	}
	if run.EndedAt == nil {
		run.State, run.Explanation, run.EndedAt = "cancelled", "operator recovery after claim loss", &now
	}
	var changed []VerificationRow
	seen := map[string]bool{}
	for _, opID := range d.OperationIDs {
		if seen[opID] {
			return nil, nil, ErrVerificationInvalid
		}
		seen[opID] = true
		r, ok := verificationFind(rows, "verification_operations", opID)
		if !ok || r.TaskID != task.ID || r.ContextID != vc.ID || r.RunID != run.ID {
			return nil, nil, ErrVerificationAccess
		}
		op := verificationDecode[VerificationOperation](r)
		if op.InputDigest != d.InputDigest || verificationOperationResolved(r.State) {
			return nil, nil, ErrVerificationConflict
		}
		op.Recovery = append(op.Recovery, VerificationOperationAuthorization{ID: id, RequestID: requestID, Actor: actor.ID, Reason: d.Reason, Disposition: d.Disposition, InputDigest: d.InputDigest, Original: original, At: now})
		r.Body = verificationJSON(op)
		changed = append(changed, r)
	}
	for _, r := range rows {
		if r.Table == "verification_operations" && r.RunID == run.ID && !verificationOperationResolved(r.State) && !seen[r.ID] {
			return nil, nil, ErrVerificationState
		}
	}
	run.Recovery = append(run.Recovery, VerificationReplayAuthorization{ID: id, Actor: actor.ID, Reason: d.Reason, At: now, Original: original, RequestDigest: requestDigest, Disposition: d.Disposition, InputDigest: d.InputDigest})
	rr.State, rr.Body = run.State, verificationJSON(run)
	changed = append(changed, rr)
	event := core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "verification.recovery_authorized", ActorID: actor.ID, ActorRole: actor.Role, At: now, Payload: core.JSONPayload(map[string]any{"request_id": requestID, "authorization_id": id, "original": original, "disposition": d.Disposition, "reason": d.Reason, "operation_ids": d.OperationIDs})}
	return changed, []core.Event{event}, nil
}

type verificationRecoveryDigestKey struct{}

func SanitizeVerificationRecovery(ctx context.Context, source redact.SecretSource) (context.Context, error) {
	d := VerificationRecoveryFromContext(ctx)
	if d == nil {
		return ctx, nil
	}
	if _, ok := ctx.Value(verificationRecoveryDigestKey{}).(string); !ok {
		ctx = context.WithValue(ctx, verificationRecoveryDigestKey{}, verificationHash(verificationJSON(d)))
	}
	clean := *d
	reason, _, err := redact.Text(ctx, source, d.Reason)
	if err != nil {
		return ctx, err
	}
	clean.Reason = reason
	return WithVerificationRecovery(ctx, &clean), nil
}
