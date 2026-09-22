package store

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func verificationFilterJSON(r *redact.Redactor, b []byte) ([]byte, error) {
	var value any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				lower := strings.ToLower(k)
				switch lower {
				case "authorization", "proxy-authorization", "cookie", "set-cookie", "password", "secret", "access_token", "refresh_token", "api_key", "apikey":
					x[k] = "[REDACTED:field]"
					continue
				}
				if lower == "url" {
					if raw, ok := child.(string); ok {
						if u, err := url.Parse(raw); err == nil {
							u.User = nil
							q := u.Query()
							for key := range q {
								lk := strings.ToLower(key)
								if strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") || lk == "key" || lk == "api_key" {
									q.Set(key, "[REDACTED:field]")
								}
							}
							u.RawQuery = q.Encode()
							child = u.String()
						}
					}
				}
				x[k] = walk(child)
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		default:
			return v
		}
	}
	filtered, err := json.Marshal(walk(value))
	if err != nil {
		return nil, err
	}
	clean, _, err := r.RedactJSON(filtered)
	return clean, err
}

func verificationContract(rows []VerificationRow, contextID string, subject core.VerificationSubject) (verification.Exercise, error) {
	if subject.Kind == "ordinary" {
		r, ok := verificationFind(rows, "verification_obligations", contextID+":"+subject.ObligationID)
		if !ok {
			return verification.Exercise{}, ErrVerificationAccess
		}
		v := verificationDecode[VerificationObligation](r)
		if v.Digest != subject.ContractDigest {
			return verification.Exercise{}, ErrVerificationInvalid
		}
		return v.Contract, nil
	}
	for _, r := range rows {
		if r.Table == "verification_selections" && r.ContextID == contextID {
			for _, s := range verificationDecode[VerificationSelection](r).Subjects {
				if verificationEqual(s.Subject, subject) {
					return s.Contract, nil
				}
			}
		}
	}
	return verification.Exercise{}, ErrVerificationInvalid
}
func verificationSubjectKey(s core.VerificationSubject) string {
	// Content versions intentionally do not clear unresolved external operations.
	if s.Kind == "kit" {
		return core.VerificationOperationSubject(s)
	}
	return core.VerificationOperationSubject(s)
}
func verificationWritableRun(c VerificationCommand, rows []VerificationRow) (VerificationAttempt, error) {
	r, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || r.ContextID != c.ContextID || r.TaskID != c.Access.TaskID {
		return VerificationAttempt{}, ErrVerificationAccess
	}
	v := verificationDecode[VerificationAttempt](r)
	cr, ok := verificationFind(rows, "verification_contexts", c.ContextID)
	if !ok {
		return v, ErrVerificationAccess
	}
	if _, err := verificationLiveGrant(rows, verificationDecode[VerificationContext](cr), v.GrantID, v.Subject); err != nil {
		return v, err
	}
	if v.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return VerificationAttempt{}, ErrVerificationAccess
	}
	if v.State != "running" && v.State != "pending" {
		return VerificationAttempt{}, ErrVerificationState
	}
	return v, nil
}
func verificationAttemptMutation(c VerificationCommand, rows []VerificationRow, vc VerificationContext, actor string, now time.Time, r *redact.Redactor, out *VerificationMutation) error {
	if c.Attempt == nil {
		return ErrVerificationInvalid
	}

	if c.Kind == VerificationAuthorizeRetry {
		// Retry authority is issued only by the typed work-order recovery
		// command, which binds original authority and one successor claim.
		return ErrVerificationAccess
	}
	if c.Kind == VerificationStartAttempt {
		v := *c.Attempt
		v.CoverageDigest = verificationHash(verificationJSON(c.Coverage))
		v.ID = ""
		v.ContextID = c.ContextID
		v.StartKey = c.Key
		v.WorkOrderAttemptID = c.Access.WorkOrderAttemptID
		v.CreatedBy = actor
		v.State = "running"
		v.StartedAt = time.Time{}
		v.EndedAt = nil
		v.ExitCode = nil
		v.Explanation = ""
		v.Ordinal = 0
		v.Recovery = nil
		if c.Key == "" || v.SafeInputs == nil {
			return ErrVerificationInvalid
		}
		contract, err := verificationContract(rows, c.ContextID, v.Subject)
		if err != nil {
			return err
		}
		v.EffectivePermissions = contract.Permissions // A manifest declaration is not execution authority.
		grant, err := verificationLiveGrant(rows, vc, v.GrantID, v.Subject)
		if err != nil {
			return err
		}
		if v.LocalActions == nil {
			v.LocalActions = []core.VerificationPermission{}
		}
		effective, err := verification.RequireVerificationPermissions(v.EffectiveActions, grant.Actions, v.LocalActions)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerificationAccess, err)
		}
		if err := verification.ValidateRequestedActions(contract, effective); err != nil {
			return fmt.Errorf("%w: %v", ErrVerificationAccess, err)
		}
		v.LocalActions, err = verification.NormalizeVerificationPermissions(v.LocalActions)
		if err != nil {
			return ErrVerificationInvalid
		}
		v.GrantSnapshot = &grant
		v.EffectiveActions = effective
		if len(v.EffectiveActions) == 0 {
			v.EffectiveActions = nil
		}
		b, err := verificationSanitizeJSON(r, verificationJSON(v))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return err
		}
		ordinal := 1
		var latest *VerificationAttempt
		var successorAuthorization int = -1
		for _, row := range rows {
			if row.Table == "verification_attempts" && row.TaskID == c.Access.TaskID {
				prior := verificationDecode[VerificationAttempt](row)
				if verificationSubjectKey(prior.Subject) == verificationSubjectKey(v.Subject) && (latest == nil || prior.StartedAt.After(latest.StartedAt)) {
					copy := prior
					latest = &copy
				}
			}
		}
		for _, row := range rows {
			if row.Table == "verification_attempts" && row.ContextID == c.ContextID {
				old := verificationDecode[VerificationAttempt](row)
				if old.StartKey == c.Key {
					original := old
					old.ID = ""
					old.State = "running"
					old.StartedAt = time.Time{}
					old.EndedAt = nil
					old.ExitCode = nil
					old.Explanation = ""
					old.Ordinal = 0
					old.Recovery = nil
					if !verificationEqual(old, v) {
						return ErrVerificationConflict
					}
					out.Receipt.ID = original.ID
					out.Receipt.State = original.State
					return nil
				}
				if verificationEqual(old.Subject, v.Subject) {
					if old.State == "running" || old.State == "pending" {
						return ErrVerificationState
					}

					if old.Ordinal >= ordinal {
						ordinal = old.Ordinal + 1
					}
				}
			}
		}

		if latest != nil {
			if latest.EndedAt == nil {
				return ErrVerificationState
			}
			if contract.RetryPolicy == "operator_action_required" {
				authorized := false
				for _, authorization := range latest.Recovery {
					if authorization.ID == v.ReplayAuthorizationID && strings.HasPrefix(authorization.Actor, "user:") {
						if authorization.Original.ContextID == "" {
							continue
						}
						binding, e := verificationBinding(c, rows)
						if e != nil {
							return e
						}
						if authorization.Successor == nil && authorization.Original.WorkOrderID == c.Access.WorkOrderID && sameVerificationAuthority(authorization.Original, binding) && authorization.InputDigest == verificationHash(verificationJSON(v.SafeInputs)) && (authorization.Disposition == "not_applied" || authorization.Disposition == "applied") {
							authorized = true
							for i, a := range latest.Recovery {
								if a.ID == authorization.ID {
									successorAuthorization = i
								}
							}
						}
					}
				}
				if !authorized {
					return ErrVerificationState
				}
				for _, row := range rows {
					if row.Table == "verification_attempts" && verificationDecode[VerificationAttempt](row).ReplayAuthorizationID == v.ReplayAuthorizationID {
						return ErrVerificationState
					}
				}
			}
		}
		for _, row := range rows {
			if row.Table == "verification_operations" {
				op := verificationDecode[VerificationOperation](row)
				if op.SubjectKey == verificationSubjectKey(v.Subject) && !verificationOperationResolved(row.State) {
					return ErrVerificationState
				}
			}
		}
		if contract.RetryPolicy == "safe_to_replay" && contract.SafetyBasis == "" {
			return ErrVerificationInvalid
		}
		v.ID = verificationID()
		if successorAuthorization >= 0 {
			binding, e := verificationBinding(c, rows)
			if e != nil {
				return e
			}
			binding.RunID = v.ID
			latest.Recovery[successorAuthorization].Successor = &binding
			row, ok := verificationFind(rows, "verification_attempts", latest.ID)
			if !ok {
				return ErrVerificationAccess
			}
			row.Body = verificationJSON(latest)
			out.Rows = append(out.Rows, row)
		}
		out.Receipt.LaunchAuthorized = true
		v.Ordinal = ordinal
		v.StartedAt = now
		out.Rows = append(out.Rows, verificationRow("verification_attempts", v.ID, c.Access.TaskID, c.ContextID, v.ID, c.ContextID+":"+c.Key, v.State, v))
		out.Receipt.ID = v.ID
		out.Receipt.State = v.State
		return nil
	}
	row, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || row.ContextID != c.ContextID {
		return ErrVerificationAccess
	}
	v := verificationDecode[VerificationAttempt](row)
	if v.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return ErrVerificationAccess
	}
	next := c.Attempt.State
	switch next {
	case "succeeded", "failed", "timed_out", "cancelled", "blocked", "waiting":
	default:
		return ErrVerificationInvalid
	}
	explanation, _ := r.Redact(c.Attempt.Explanation)
	if (next == "blocked" || next == "waiting") && explanation == "" {
		return ErrVerificationInvalid
	}
	if v.EndedAt != nil {
		if v.State != next || v.Explanation != explanation || !verificationEqual(v.ExitCode, c.Attempt.ExitCode) {
			return ErrVerificationConflict
		}
		out.Receipt.ID = v.ID
		out.Receipt.State = v.State
		return nil
	}
	if next == "succeeded" {
		snapshot, err := VerificationSnapshotFromRows(rows, c.ContextID)
		if err != nil {
			return err
		}
		if err := ValidateVerificationSuccess(snapshot, v.ID, c.Attempt.ExitCode); err != nil {
			return err
		}

		for _, other := range rows {
			if other.Table == "verification_operations" && verificationOperationRun(other) == v.ID && !verificationOperationResolved(other.State) {
				return ErrVerificationState
			}
		}
	}
	v.State = next
	v.Explanation = explanation
	v.ExitCode = c.Attempt.ExitCode
	v.EndedAt = &now
	row.State = next
	row.Body = verificationJSON(v)
	out.Rows = append(out.Rows, row)
	out.Receipt.ID = v.ID
	out.Receipt.State = v.State
	// A terminated process without completion acknowledgement leaves uncertain
	// operations unresolved, including a registered but unacknowledged dispatch.
	for _, other := range rows {
		if other.Table == "verification_operations" && verificationOperationRun(other) == v.ID && (other.State == "registered" || other.State == "dispatching") {
			op := verificationDecode[VerificationOperation](other)
			op.History = append(op.History, VerificationOperationObservation{State: "outcome_unknown", Actor: actor, Source: "attempt.terminate", CapturedAt: now, ContextID: c.ContextID, RunID: v.ID, WorkOrderAttemptID: c.Access.WorkOrderAttemptID})
			other.State = "outcome_unknown"
			other.Body = verificationJSON(op)
			out.Rows = append(out.Rows, other)
		}
	}
	return nil
}
func verificationOperationResolved(state string) bool {
	return state == "applied" || state == "not_applied" || state == "completed"
}

func verificationPublicationMutation(c VerificationCommand, v VerificationPublication, rows []VerificationRow, now time.Time, out *VerificationMutation) error {
	if v.PullRequestNumber <= 0 || v.HeadSHA == "" || v.BodyDigest == "" {
		return ErrVerificationInvalid
	}
	v.ID = verificationID()
	v.ContextID = c.ContextID
	v.TaskID = c.Access.TaskID
	v.State = "pending"
	v.CreatedAt = now
	v.Generation = 1
	for _, row := range rows {
		if row.Table == "verification_publications" && row.TaskID == c.Access.TaskID {
			old := verificationDecode[VerificationPublication](row)
			if old.ContextID == v.ContextID && old.HeadSHA == v.HeadSHA && old.BodyDigest == v.BodyDigest && old.PullRequestNumber == v.PullRequestNumber {
				out.Receipt.PublicationID = old.ID
				return nil
			}
			if old.Generation >= v.Generation {
				v.Generation = old.Generation + 1
			}
		}
	}
	out.Rows = append(out.Rows, verificationRow("verification_publications", v.ID, c.Access.TaskID, c.ContextID, "", string(verificationJSON([]any{v.TaskID, v.ContextID, v.HeadSHA, v.PullRequestNumber, v.BodyDigest})), "pending", v))
	out.Publication = &v
	out.Receipt.PublicationID = v.ID
	return nil
}

func verificationSortChunks(chunks []VerificationUploadChunk) {
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
}

func validateVerificationObligation(v VerificationObligation) error {
	c := v.Contract
	switch c.Kind {
	case "script", "hybrid":
		if len(c.Argv) == 0 || strings.TrimSpace(c.Argv[0]) == "" {
			return ErrVerificationInvalid
		}
	case "interactive":
		if len(c.Argv) == 0 && strings.TrimSpace(v.ObservationProcedure) == "" {
			return ErrVerificationInvalid
		}
	case "observation":
		if strings.TrimSpace(v.ObservationProcedure) == "" || len(c.EvidenceOutputs) == 0 {
			return ErrVerificationInvalid
		}
	default:
		return ErrVerificationInvalid
	}
	switch c.RetryPolicy {
	case "safe_to_replay":
		if strings.TrimSpace(c.SafetyBasis) == "" {
			return ErrVerificationInvalid
		}
	case "", "operator_action_required", "reconciliation_required":
	default:
		return ErrVerificationInvalid
	}
	seen := map[string]bool{}
	for _, id := range c.RequiredAssertions {
		if !verificationIdentifier(id) || seen[id] {
			return ErrVerificationInvalid
		}
		seen[id] = true
	}
	types := map[string]bool{"api_exchange": true, "state_observation": true, "assertion_result": true, "execution_report": true, "visual_capture": true, "operator_observation": true}
	seen = map[string]bool{}
	for _, output := range c.EvidenceOutputs {
		if !types[output.Type] || output.SchemaVersion != 1 || output.MinimumItems < 1 || seen[output.Type] {
			return ErrVerificationInvalid
		}
		seen[output.Type] = true
	}
	return nil
}
