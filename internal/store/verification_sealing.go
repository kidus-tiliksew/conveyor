package store

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// req-verification-kits REQ-4/REQ-5; feature-verification-kit-execution VK-5.1 and VK-7.

type VerificationCoverage struct {
	Sources       []VerificationCoverageSource `json:"sources"`
	ObligationIDs []string                     `json:"obligation_ids"`
	Justification string                       `json:"justification"`
}

type VerificationSubmission struct {
	Outcome  string               `json:"outcome"`
	Coverage VerificationCoverage `json:"coverage"`
	Feedback string               `json:"feedback,omitempty"`
}

type VerificationResult struct {
	Submission  VerificationSubmission
	Binding     core.VerificationBinding
	Disposition string
	Actor       string
	SealedAt    time.Time
}

// ValidateVerificationSeal reuses the attempt evaluator inside the backend's
// lifecycle transaction; no caller-supplied success callback is authoritative.
func ValidateVerificationSeal(c VerificationCommand, rows []VerificationRow, now time.Time, actor string) (*VerificationResult, error) {
	if c.Submission == nil {
		return nil, ErrVerificationInvalid
	}
	s := *c.Submission
	if s.Coverage.ObligationIDs == nil || strings.TrimSpace(s.Coverage.Justification) == "" {
		return nil, ErrVerificationInvalid
	}
	snapshot, err := VerificationSnapshotFromRows(rows, c.ContextID)
	if err != nil {
		return nil, err
	}
	binding, err := verificationBinding(c, rows)
	if err != nil {
		return nil, err
	}
	if err := ValidateVerificationCoverage(s.Coverage, snapshot); err != nil {
		return nil, err
	}
	vc := snapshot.Contexts[0]
	if len(snapshot.Attempts) > 0 && (vc.Coverage == nil || !verificationEqual(*vc.Coverage, s.Coverage)) {
		return nil, ErrVerificationState
	}
	if len(vc.Discovery) != len(vc.Revisions) || len(snapshot.Selections) != 1 {
		return nil, ErrVerificationState
	}
	seen := map[string]bool{}
	for _, raw := range vc.Discovery {
		var d struct {
			Repository string `json:"repository"`
			SHA        string `json:"revision"`
			State      string `json:"state"`
		}
		if json.Unmarshal(raw, &d) != nil || seen[d.Repository] || (d.State != "no_manifest" && d.State != "present") {
			return nil, ErrVerificationState
		}
		found := false
		for _, revision := range vc.Revisions {
			if revision.Repository == d.Repository && revision.SHA == d.SHA {
				found = true
			}
		}
		if !found {
			return nil, ErrVerificationState
		}
		seen[d.Repository] = true
	}
	selection := snapshot.Selections[0]
	if len(selection.Receipt.Diagnostics) != 0 {
		return nil, ErrVerificationState
	}
	for _, kit := range selection.Receipt.Kits {
		if kit.Eligibility == "invalid" {
			return nil, ErrVerificationState
		}
	}
	if len(s.Coverage.ObligationIDs) != len(snapshot.Obligations) {
		return nil, ErrVerificationState
	}
	seen = map[string]bool{}
	for _, id := range s.Coverage.ObligationIDs {
		if id == "" || seen[id] {
			return nil, ErrVerificationInvalid
		}
		seen[id] = true
		found := false
		for _, o := range snapshot.Obligations {
			if o.ID == id {
				found = true
			}
		}
		if !found {
			return nil, ErrVerificationState
		}
	}
	subjects := []core.VerificationSubject{}
	for _, subject := range selection.Subjects {
		subjects = append(subjects, subject.Subject)
	}
	for _, o := range snapshot.Obligations {
		subjects = append(subjects, core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest})
	}
	failed, blocked := false, false
	for _, subject := range subjects {
		var latest *VerificationAttempt
		for i := range snapshot.Attempts {
			a := &snapshot.Attempts[i]
			if a.Subject == subject && (latest == nil || a.Ordinal > latest.Ordinal) {
				latest = a
			}
		}
		if latest == nil || latest.EndedAt == nil {
			return nil, ErrVerificationState
		}
		switch latest.State {
		case "succeeded":
			if err := ValidateVerificationSuccess(snapshot, latest.ID, latest.ExitCode); err != nil {
				return nil, err
			}
		case "failed":
			failed = true
		case "blocked", "waiting", "cancelled", "timed_out":
			blocked = true
		default:
			return nil, ErrVerificationState
		}
	}
	unresolved := false
	for _, row := range rows {
		if row.Table == "verification_operations" && row.TaskID == c.Access.TaskID && !verificationOperationResolved(row.State) {
			unresolved = true
		}
	}
	switch s.Outcome {
	case "succeeded":
		if failed || blocked || unresolved {
			return nil, ErrVerificationState
		}
	case "feedback":
		if !failed || unresolved || strings.TrimSpace(s.Feedback) == "" {
			return nil, ErrVerificationState
		}
	case "operator_action_required":
		if (!blocked && !unresolved) || strings.TrimSpace(s.Feedback) == "" {
			return nil, ErrVerificationState
		}
	default:
		return nil, ErrVerificationInvalid
	}
	disposition := "selected_kits"
	if len(selection.Subjects) == 0 {
		disposition = "no_selected_kits"
	}
	return &VerificationResult{Submission: s, Binding: binding, Disposition: disposition, Actor: actor, SealedAt: now}, nil
}

// VerificationSealedCommand supplies the sanitized immutable submission to the
// lifecycle adapter, so feedback cannot bypass the evidence redaction boundary.
func VerificationSealedCommand(c VerificationCommand, mutation VerificationMutation) VerificationCommand {
	for _, row := range mutation.Rows {
		if row.Table == "verification_contexts" && row.ID == c.ContextID {
			vc := verificationDecode[VerificationContext](row)
			if vc.Result != nil {
				c.Submission = &vc.Result.Submission
			}
		}
	}
	return c
}
