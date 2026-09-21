package store

import (
	"encoding/json"
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
		return "kit:" + s.KitID + ":" + s.ExerciseID
	}
	return "ordinary:" + s.ObligationID
}
func verificationWritableRun(c VerificationCommand, rows []VerificationRow) (VerificationAttempt, error) {
	r, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || r.ContextID != c.ContextID || r.TaskID != c.Access.TaskID {
		return VerificationAttempt{}, ErrVerificationAccess
	}
	v := verificationDecode[VerificationAttempt](r)
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
		if c.Access.UserID == "" || !c.Authority.OperateGates || c.Attempt.Explanation == "" {
			return ErrVerificationAccess
		}
		row, ok := verificationFind(rows, "verification_attempts", c.RunID)
		if !ok || row.TaskID != c.Access.TaskID {
			return ErrVerificationAccess
		}
		old := verificationDecode[VerificationAttempt](row)
		if old.EndedAt == nil {
			return ErrVerificationState
		}
		reason, _ := r.Redact(c.Attempt.Explanation)
		for _, authorization := range old.Recovery {
			if authorization.ContextID == c.ContextID && authorization.Reason == reason && authorization.Actor == actor {
				out.Receipt.ID = authorization.ID
				return nil
			}
		}
		authorization := VerificationReplayAuthorization{ID: verificationID(), ContextID: c.ContextID, Actor: actor, Reason: reason, At: now}
		old.Recovery = append(old.Recovery, authorization)
		row.Body = verificationJSON(old)
		out.Rows = append(out.Rows, row)
		out.Receipt.ID = authorization.ID
		return nil
	}
	if c.Kind == VerificationStartAttempt {
		v := *c.Attempt
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
		b, err := verificationSanitizeJSON(r, verificationJSON(v))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return err
		}
		ordinal := 1
		var latest *VerificationAttempt
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
					if authorization.ID == v.ReplayAuthorizationID && authorization.ContextID == c.ContextID && strings.HasPrefix(authorization.Actor, "user:") {
						authorized = true
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
		for _, other := range rows {
			if other.Table == "verification_operations" && other.RunID == v.ID && !verificationOperationResolved(other.State) {
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
		if other.Table == "verification_operations" && other.RunID == v.ID && (other.State == "registered" || other.State == "dispatching") {
			op := verificationDecode[VerificationOperation](other)
			op.History = append(op.History, VerificationOperationObservation{State: "outcome_unknown", Actor: actor, Source: "attempt.terminate", CapturedAt: now})
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
func verificationOperationMutation(c VerificationCommand, rows []VerificationRow, actor string, now time.Time, r *redact.Redactor, out *VerificationMutation) error {
	runRow, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || runRow.ContextID != c.ContextID || runRow.TaskID != c.Access.TaskID {
		return ErrVerificationAccess
	}
	attempt := verificationDecode[VerificationAttempt](runRow)
	if c.Kind == VerificationPrepareOperation {
		if _, err := verificationWritableRun(c, rows); err != nil {
			return err
		}
		if c.Operation == nil || c.Key == "" {
			return ErrVerificationInvalid
		}
		v := *c.Operation
		v.ID = ""
		v.ContextID = c.ContextID
		v.RunID = c.RunID
		v.Key = c.Key
		v.SubjectKey = verificationSubjectKey(attempt.Subject)
		v.CreatedBy = actor
		v.CreatedAt = time.Time{}
		v.History = nil
		contract, err := verificationContract(rows, c.ContextID, attempt.Subject)
		if err != nil {
			return err
		}
		v.RetryPolicy = contract.RetryPolicy
		declared := false
		for _, op := range contract.Operations {
			if op.ID == v.StepID && op.TargetBinding == v.Target {
				declared = true
			}
		}
		if !declared || !verificationIdentifier(v.StepID) || !verificationIdentifier(v.Target) || len(v.InputDigest) != 64 {
			return ErrVerificationInvalid
		}
		for _, row := range rows {
			if row.Table == "verification_operations" {
				old := verificationDecode[VerificationOperation](row)
				if old.Key == c.Key {
					if row.TaskID != c.Access.TaskID {
						return ErrVerificationAccess
					}
					original := old
					old.ID = ""
					old.CreatedAt = time.Time{}
					old.History = nil
					// Successor attempts may inspect the original key but cannot replace its
					// provenance. Only the immutable logical input fields govern this replay.
					if old.SubjectKey != v.SubjectKey || old.StepID != v.StepID || old.Target != v.Target || old.InputDigest != v.InputDigest || old.RetryPolicy != v.RetryPolicy {
						return ErrVerificationConflict
					}
					out.Receipt.ID = original.ID
					return nil
				}
				if old.SubjectKey == v.SubjectKey && old.StepID == v.StepID && old.Target == v.Target && !verificationOperationResolved(row.State) {
					return ErrVerificationState
				}
			}
		}
		v.ID = verificationID()
		v.CreatedAt = now
		v.History = []VerificationOperationObservation{{State: "registered", Actor: actor, Source: "operation.prepare", CapturedAt: now}}
		out.Rows = append(out.Rows, verificationRow("verification_operations", v.ID, c.Access.TaskID, c.ContextID, c.RunID, c.Key, "registered", v))
		out.Receipt.ID = v.ID
		return nil
	}
	if c.Operation == nil || c.Observation == nil {
		return ErrVerificationInvalid
	}
	row, ok := verificationFind(rows, "verification_operations", c.Operation.ID)
	if !ok || row.TaskID != c.Access.TaskID {
		return ErrVerificationAccess
	}
	v := verificationDecode[VerificationOperation](row)
	if v.SubjectKey != verificationSubjectKey(attempt.Subject) {
		return ErrVerificationAccess
	}
	obs := *c.Observation
	obs.Actor = actor
	if obs.Source == "" || obs.CapturedAt.IsZero() || obs.CapturedAt.After(now.Add(time.Minute)) {
		return ErrVerificationInvalid
	}
	obs.ProviderReference, _ = r.Redact(obs.ProviderReference)
	obs.Source, _ = r.Redact(obs.Source)
	if len(v.History) > 0 && verificationEqual(v.History[len(v.History)-1], obs) {
		out.Receipt.ID = v.ID
		return nil
	}
	allowed := false
	switch obs.State {
	case "dispatching":
		if c.Access.UserID != "" {
			return ErrVerificationAccess
		}
		if _, err := verificationWritableRun(c, rows); err != nil {
			return err
		}
		allowed = row.State == "registered" || row.State == "not_applied"
	case "completed":
		if c.Access.UserID != "" {
			return ErrVerificationAccess
		}
		allowed = row.State == "dispatching"
	case "outcome_unknown":
		allowed = row.State == "registered" || row.State == "dispatching"
	case "applied", "not_applied", "unknown":
		allowed = row.State == "dispatching" || row.State == "outcome_unknown" || row.State == "unknown"
	}
	if !allowed {
		return ErrVerificationState
	}
	v.History = append(v.History, obs)
	row.State = obs.State
	row.Body = verificationJSON(v)
	out.Rows = append(out.Rows, row)
	out.Receipt.ID = v.ID
	return nil
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
