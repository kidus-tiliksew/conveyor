package store

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// req-verification-kits REQ-3/REQ-7; feature-verification-kit-execution VK-4.1 (DEC-43).

func verificationBinding(c VerificationCommand, rows []VerificationRow) (core.VerificationBinding, error) {
	r, ok := verificationFind(rows, "verification_contexts", c.ContextID)
	if !ok || r.TaskID != c.Access.TaskID {
		return core.VerificationBinding{}, ErrVerificationAccess
	}
	v := verificationDecode[VerificationContext](r)
	if v.WorkOrderID != c.Access.WorkOrderID || v.WorkOrderAttemptID != c.Access.WorkOrderAttemptID || v.SealedAt != nil {
		return core.VerificationBinding{}, ErrVerificationAccess
	}
	return core.VerificationBinding{ReviewScope: v.ReviewScope, BaselineSHA: v.BaselineSHA, ContextID: v.ID, WorkOrderID: v.WorkOrderID, WorkOrderAttemptID: v.WorkOrderAttemptID, RunID: c.RunID, Revisions: v.Revisions, GoverningPins: v.GoverningPins}, nil
}

func sameVerificationAuthority(a, b core.VerificationBinding) bool {
	return a.ReviewScope == b.ReviewScope && a.BaselineSHA == b.BaselineSHA && verificationEqual(a.Revisions, b.Revisions) && verificationEqual(a.GoverningPins, b.GoverningPins)
}

func verificationOperationMutation(c VerificationCommand, rows []VerificationRow, actor string, now time.Time, redactor *redact.Redactor, out *VerificationMutation) error {
	if c.Operation == nil {
		return ErrVerificationInvalid
	}
	binding, err := verificationBinding(c, rows)
	if err != nil {
		return err
	}
	if c.Kind == VerificationReconcileOperation {
		return reconcileVerificationOperation(c, rows, binding, actor, now, redactor, out)
	}
	runRow, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || runRow.ContextID != c.ContextID || runRow.TaskID != c.Access.TaskID {
		return ErrVerificationAccess
	}
	attempt := verificationDecode[VerificationAttempt](runRow)
	if attempt.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return ErrVerificationAccess
	}
	if c.Kind == VerificationPrepareOperation {
		if _, err := verificationWritableRun(c, rows); err != nil {
			return err
		}
		if c.Key == "" {
			return ErrVerificationInvalid
		}
		v := *c.Operation
		v.ID, v.ContextID, v.RunID, v.Key = "", c.ContextID, c.RunID, c.Key
		v.SubjectKey, v.CreatedBy = verificationSubjectKey(attempt.Subject), actor
		v.CreatedAt, v.History = time.Time{}, nil
		v.Original, v.Successors, v.Recovery = binding, nil, nil
		contract, err := verificationContract(rows, c.ContextID, attempt.Subject)
		if err != nil {
			return err
		}
		v.RetryPolicy = contract.RetryPolicy
		if (v.IdempotencyScope == "") != v.IdempotencyValidUntil.IsZero() {
			return ErrVerificationInvalid
		}
		v.IdempotencyScope, _ = redactor.Redact(v.IdempotencyScope)
		if v.InputDigest != verificationHash(verificationJSON(attempt.SafeInputs)) {
			return ErrVerificationConflict
		}
		declared := false
		for _, op := range contract.Operations {
			if op.ID == v.StepID && op.TargetBinding == v.Target {
				declared = true
			}
		}
		digest, err := hex.DecodeString(v.InputDigest)
		if !declared || !verificationIdentifier(v.StepID) || !verificationIdentifier(v.Target) || err != nil || len(digest) != 32 || strings.ToLower(v.InputDigest) != v.InputDigest {
			return ErrVerificationInvalid
		}
		for _, row := range rows {
			if row.Table != "verification_operations" {
				continue
			}
			old := verificationDecode[VerificationOperation](row)
			if old.Key == c.Key {
				if row.TaskID != c.Access.TaskID {
					return ErrVerificationAccess
				}
				if old.SubjectKey != v.SubjectKey || old.StepID != v.StepID || old.Target != v.Target || old.InputDigest != v.InputDigest || old.RetryPolicy != v.RetryPolicy || old.IdempotencyScope != v.IdempotencyScope || !old.IdempotencyValidUntil.Equal(v.IdempotencyValidUntil) {
					return ErrVerificationConflict
				}
				out.Receipt = VerificationReceipt{ID: old.ID, State: row.State}
				if old.ContextID == c.ContextID && old.RunID == c.RunID {
					return nil
				}
				if old.Original.ContextID == "" || !sameVerificationAuthority(old.Original, binding) {
					return ErrVerificationConflict
				}
				if !verificationOperationResolved(row.State) {
					return ErrVerificationState
				}
				for _, b := range old.Successors {
					if verificationEqual(b, binding) {
						return nil
					}
				}
				if old.RetryPolicy == "operator_action_required" && row.State == "not_applied" {
					if err := consumeOperationAuthorization(&old, c.ReplayAuthorizationID, binding); err != nil {
						return err
					}
				}
				old.Successors = append(old.Successors, binding)
				row.Body = verificationJSON(old)
				out.Rows = append(out.Rows, row)
				return nil
			}
			if old.SubjectKey == v.SubjectKey && old.StepID == v.StepID && old.Target == v.Target && (!verificationOperationResolved(row.State) || (row.TaskID == c.Access.TaskID && sameVerificationAuthority(old.Original, binding) && old.InputDigest == v.InputDigest)) {
				if old.InputDigest != v.InputDigest {
					return ErrVerificationConflict
				}
				return ErrVerificationState
			}
		}
		v.ID, v.CreatedAt = verificationID(), now
		v.History = []VerificationOperationObservation{{State: "registered", Actor: actor, Source: "operation.prepare", CapturedAt: now, ContextID: c.ContextID, RunID: c.RunID, WorkOrderAttemptID: c.Access.WorkOrderAttemptID}}
		out.Rows = append(out.Rows, verificationRow("verification_operations", v.ID, c.Access.TaskID, c.ContextID, c.RunID, c.Key, "registered", v))
		out.Receipt = VerificationReceipt{ID: v.ID, State: "registered"}
		return nil
	}
	if c.Observation == nil {
		return ErrVerificationInvalid
	}
	row, ok := verificationFind(rows, "verification_operations", c.Operation.ID)
	if !ok || row.TaskID != c.Access.TaskID {
		return ErrVerificationAccess
	}
	v := verificationDecode[VerificationOperation](row)
	bound := row.ContextID == c.ContextID && row.RunID == c.RunID
	if n := len(v.Successors); n > 0 {
		bound = verificationEqual(v.Successors[n-1], binding)
	}
	if !bound || v.SubjectKey != verificationSubjectKey(attempt.Subject) {
		return ErrVerificationAccess
	}
	obs := *c.Observation
	if obs.State != "dispatching" && obs.State != "completed" {
		return ErrVerificationInvalid
	}
	if obs.State == "dispatching" || obs.State == "completed" {
		if c.Access.UserID != "" {
			return ErrVerificationAccess
		}
		if _, err := verificationWritableRun(c, rows); err != nil {
			return err
		}
	}
	if obs.State == "dispatching" && !v.IdempotencyValidUntil.IsZero() && !v.IdempotencyValidUntil.After(now) {
		if row.State != "not_applied" || len(v.History) == 0 || v.History[len(v.History)-1].CapturedAt.Before(v.IdempotencyValidUntil) {
			return ErrVerificationState
		}
	}
	// The ordinary relay cannot replace the authenticated recovery boundary.
	if v.RetryPolicy == "operator_action_required" && obs.State == "dispatching" && row.State == "not_applied" {
		authorized := false
		for _, a := range v.Recovery {
			if a.Successor != nil && verificationEqual(*a.Successor, binding) {
				authorized = true
			}
		}
		if !authorized {
			return ErrVerificationState
		}
	}
	return appendVerificationOperationObservation(c, row, v, obs, actor, now, redactor, out)
}

func consumeOperationAuthorization(op *VerificationOperation, id string, binding core.VerificationBinding) error {
	for i := range op.Recovery {
		a := &op.Recovery[i]
		if a.ID == id && id != "" && a.Disposition == "not_applied" && a.InputDigest == op.InputDigest && sameVerificationAuthority(a.Original, binding) && a.Successor == nil {
			copy := binding
			a.Successor = &copy
			return nil
		}
	}
	return ErrVerificationState
}

func reconcileVerificationOperation(c VerificationCommand, rows []VerificationRow, binding core.VerificationBinding, actor string, now time.Time, redactor *redact.Redactor, out *VerificationMutation) error {
	if c.Observation == nil {
		return ErrVerificationInvalid
	}
	switch c.Observation.State {
	case "applied", "not_applied", "unknown":
	default:
		return ErrVerificationInvalid
	}
	row, ok := verificationFind(rows, "verification_operations", c.Operation.ID)
	if !ok || row.TaskID != c.Access.TaskID {
		return ErrVerificationAccess
	}
	op := verificationDecode[VerificationOperation](row)
	if op.Original.ContextID == "" || !sameVerificationAuthority(op.Original, binding) {
		return ErrVerificationConflict
	}
	if op.RetryPolicy == "operator_action_required" {
		authorized := false
		for _, a := range op.Recovery {
			if a.ID == c.ReplayAuthorizationID && a.ID != "" && a.InputDigest == op.InputDigest && a.Disposition == c.Observation.State {
				authorized = true
			}
		}
		if !authorized {
			return ErrVerificationState
		}
	}
	return appendVerificationOperationObservation(c, row, op, *c.Observation, actor, now, redactor, out)
}

func appendVerificationOperationObservation(c VerificationCommand, row VerificationRow, op VerificationOperation, obs VerificationOperationObservation, actor string, now time.Time, redactor *redact.Redactor, out *VerificationMutation) error {
	obs.Actor = actor
	obs.ContextID, obs.WorkOrderAttemptID, obs.RunID = c.ContextID, c.Access.WorkOrderAttemptID, c.RunID
	if obs.Source == "" || obs.CapturedAt.IsZero() || obs.CapturedAt.After(now.Add(time.Minute)) {
		return ErrVerificationInvalid
	}
	obs.ProviderReference, _ = redactor.Redact(core.SanitizeVerificationProviderReference(obs.ProviderReference))
	obs.Source, _ = redactor.Redact(obs.Source)
	out.Receipt = VerificationReceipt{ID: op.ID, State: row.State}
	if len(op.History) > 0 && verificationEqual(op.History[len(op.History)-1], obs) {
		return nil
	}
	if core.VerificationOperationTransition(row.State, obs.State) != nil {
		return ErrVerificationState
	}
	op.History = append(op.History, obs)
	row.State, row.Body = obs.State, verificationJSON(op)
	out.Rows = append(out.Rows, row)
	out.Receipt.State, out.Receipt.DispatchAuthorized = obs.State, obs.State == "dispatching"
	return nil
}

func verificationOperationRun(row VerificationRow) string {
	op := verificationDecode[VerificationOperation](row)
	if len(op.Successors) > 0 {
		return op.Successors[len(op.Successors)-1].RunID
	}
	return op.RunID
}
