package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// req-verification-kits AC-3.2 through AC-3.4 and AC-7.3; component-verification-service.

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
	if run.WorkOrderAttemptID != vc.WorkOrderAttemptID || verificationRunInputDigest(run) != d.InputDigest {
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

// verificationRunInputDigest is the safe-input digest a recovery request must
// carry for one verification attempt. PrepareVerificationRecovery checks it,
// and the next-action disposition binding states it.
func verificationRunInputDigest(run VerificationAttempt) string {
	return verificationHash(verificationJSON(run.SafeInputs))
}

// verificationDispositionBinding binds the sealed verification checkpoint of
// one released verify order to the four fields a recovery request carries:
// the checkpoint's context, the attempt that recorded its unresolved
// operations, those operations, and that attempt's safe-input digest. It
// applies the task, context, head, attempt, and operation checks of
// PrepareVerificationRecovery, so a request built from the binding, a
// disposition, and a reason passes them unchanged. A checkpoint the
// validator would reject yields a refusal naming the limitation instead of
// a partial binding (feature-operator-next-action NA-3, Disposition binding;
// component-verification-service, Operator checkpoint and recovery).
func verificationDispositionBinding(task core.Task, order core.WorkOrder, rows []VerificationRow) nextaction.DispositionInput {
	refuse := func(format string, args ...any) nextaction.DispositionInput {
		return nextaction.DispositionInput{Refusal: fmt.Sprintf(format, args...)}
	}
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil {
		return refuse("verify order %s has no sealed verification checkpoint to dispose", order.ID)
	}
	checkpoint := order.Checkpoint.Verification
	cr, ok := verificationFind(rows, "verification_contexts", checkpoint.ContextID)
	if !ok || cr.TaskID != task.ID {
		return refuse("verification checkpoint %s is not readable for task %s", checkpoint.ContextID, task.ID)
	}
	vc := verificationDecode[VerificationContext](cr)
	if order.Stage != core.StageVerify || vc.WorkOrderID != order.ID || vc.WorkOrderAttemptID == "" || vc.WorkOrderAttemptID != order.LastAttemptID {
		return refuse("verification checkpoint %s was not sealed by the released attempt of verify order %s", vc.ID, order.ID)
	}
	if order.HeadSHA != core.VerifyStageHead(task) || VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order) != nil {
		return refuse("verification checkpoint %s no longer matches the task head or authority of verify order %s", vc.ID, order.ID)
	}
	runID := ""
	var operationIDs []string
	seen := map[string]bool{}
	for _, id := range checkpoint.OperationIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		r, ok := verificationFind(rows, "verification_operations", id)
		if !ok || r.TaskID != task.ID {
			return refuse("operation %s of verification checkpoint %s is not readable", id, vc.ID)
		}
		if verificationOperationResolved(r.State) {
			continue
		}
		if r.ContextID != vc.ID {
			return refuse("unresolved operation %s belongs to another verification context than checkpoint %s; one recovery names one context", id, vc.ID)
		}
		if runID != "" && r.RunID != runID {
			return refuse("unresolved operations of verification checkpoint %s span more than one verification run; one recovery names one run", vc.ID)
		}
		runID = r.RunID
		operationIDs = append(operationIDs, id)
	}
	if len(operationIDs) == 0 {
		if binding := verificationGatedAttemptBinding(task, order, rows); binding != nil {
			return nextaction.DispositionInput{Binding: binding}
		}
		return refuse("verification checkpoint %s has no unresolved operations to dispose", vc.ID)
	}
	rr, ok := verificationFind(rows, "verification_attempts", runID)
	if !ok || rr.ContextID != vc.ID || rr.TaskID != task.ID {
		return refuse("verification run %s of checkpoint %s is not readable", runID, vc.ID)
	}
	run := verificationDecode[VerificationAttempt](rr)
	if run.WorkOrderAttemptID != vc.WorkOrderAttemptID {
		return refuse("verification run %s was not recorded by the attempt that sealed checkpoint %s", runID, vc.ID)
	}
	digest := verificationRunInputDigest(run)
	for _, id := range operationIDs {
		r, _ := verificationFind(rows, "verification_operations", id)
		if verificationDecode[VerificationOperation](r).InputDigest != digest {
			return refuse("operation %s was prepared over other inputs than verification run %s", id, runID)
		}
	}
	for _, r := range rows {
		if r.Table == "verification_operations" && r.RunID == runID && !verificationOperationResolved(r.State) && !seen[r.ID] {
			return refuse("verification run %s has unresolved operation %s outside checkpoint %s", runID, r.ID, vc.ID)
		}
	}
	sort.Strings(operationIDs)
	return nextaction.DispositionInput{Binding: &nextaction.Disposition{ContextID: vc.ID, RunID: runID, OperationIDs: operationIDs, InputDigest: digest}}
}

// verificationGatedAttemptBinding binds the attempt the start-path retry gate
// would refuse to retry without an operator authorization, for a checkpoint
// that names no unresolved operation. The gate reads the latest attempt of
// each subject among the verify order's contexts; a candidate's contract is
// operator_action_required, it has ended, it records no unresolved
// operation, and no authorization the gate would accept is still unused on
// it. One recovery names one run, so the earliest candidate by start time
// binds and the next projection moves to the next subject. The binding
// carries no operation and passes PrepareVerificationRecovery's context,
// head, authority, attempt, and digest checks unchanged
// (feature-operator-next-action NA-3, Disposition binding;
// component-verification-service, Operator checkpoint and recovery;
// req-verification-kits AC-3.4).
func verificationGatedAttemptBinding(task core.Task, order core.WorkOrder, rows []VerificationRow) *nextaction.Disposition {
	contexts := map[string]VerificationContext{}
	for _, r := range rows {
		if r.Table != "verification_contexts" || r.TaskID != task.ID {
			continue
		}
		vc := verificationDecode[VerificationContext](r)
		if vc.WorkOrderID == order.ID {
			contexts[r.ID] = vc
		}
	}
	type subjectAttempt struct {
		run       VerificationAttempt
		contextID string
	}
	latest := map[string]subjectAttempt{}
	for _, r := range rows {
		if r.Table != "verification_attempts" || r.TaskID != task.ID {
			continue
		}
		if _, ok := contexts[r.ContextID]; !ok {
			continue
		}
		run := verificationDecode[VerificationAttempt](r)
		key := verificationSubjectKey(run.Subject)
		prior, ok := latest[key]
		if !ok || run.StartedAt.After(prior.run.StartedAt) || (run.StartedAt.Equal(prior.run.StartedAt) && run.ID > prior.run.ID) {
			latest[key] = subjectAttempt{run: run, contextID: r.ContextID}
		}
	}
	var chosen *VerificationAttempt
	var chosenContext VerificationContext
	for _, candidate := range latest {
		run, vc := candidate.run, contexts[candidate.contextID]
		if run.EndedAt == nil || verificationRecoveryUnused(run, order.ID) {
			continue
		}
		if run.WorkOrderAttemptID != vc.WorkOrderAttemptID || VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order) != nil {
			continue
		}
		contract, err := verificationContract(rows, vc.ID, run.Subject)
		if err != nil || contract.RetryPolicy != "operator_action_required" {
			continue
		}
		unresolved := false
		for _, r := range rows {
			unresolved = unresolved || (r.Table == "verification_operations" && r.RunID == run.ID && !verificationOperationResolved(r.State))
		}
		if unresolved {
			continue
		}
		if chosen == nil || run.StartedAt.Before(chosen.StartedAt) || (run.StartedAt.Equal(chosen.StartedAt) && run.ID < chosen.ID) {
			copy := run
			chosen, chosenContext = &copy, vc
		}
	}
	if chosen == nil {
		return nil
	}
	return &nextaction.Disposition{ContextID: chosenContext.ID, RunID: chosen.ID, OperationIDs: []string{}, InputDigest: verificationRunInputDigest(*chosen)}
}

// verificationRecoveryUnused reports whether run still carries an operator
// authorization for orderID that the start-path retry gate would accept and
// no successor attempt has consumed.
func verificationRecoveryUnused(run VerificationAttempt, orderID string) bool {
	for _, a := range run.Recovery {
		if a.Successor == nil && strings.HasPrefix(a.Actor, "user:") && a.Original.ContextID != "" && a.Original.WorkOrderID == orderID && (a.Disposition == "applied" || a.Disposition == "not_applied") {
			return true
		}
	}
	return false
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
