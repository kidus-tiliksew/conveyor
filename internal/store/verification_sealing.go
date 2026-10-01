package store

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// req-verification-kits REQ-4/REQ-5; feature-verification-kit-execution VK-5.1 and VK-7.

type VerificationCoverage struct {
	Sources       []VerificationCoverageSource `json:"sources"`
	ObligationIDs []string                     `json:"obligation_ids"`
	Justification string                       `json:"justification"`
}

type VerificationSubmission struct {
	Outcome        string               `json:"outcome"`
	Coverage       VerificationCoverage `json:"coverage"`
	Feedback       string               `json:"feedback,omitempty"`
	RequiredAction string               `json:"required_action,omitempty"`
}

type VerificationResult struct {
	Submission  VerificationSubmission
	Binding     core.VerificationBinding
	Disposition string
	Actor       string
	SealedAt    time.Time
	Checkpoint  *VerificationCheckpoint `json:"Checkpoint,omitempty"`
}

// feature-verification-kit-execution VK-13.2/VK-13.3: server-computed grounds
// for an operator checkpoint. Only admission_refused carries an unverified cause.
const (
	VerificationGroundAttemptBlocked      = "attempt_blocked"
	VerificationGroundAttemptWaiting      = "attempt_waiting"
	VerificationGroundAttemptTimedOut     = "attempt_timed_out"
	VerificationGroundAttemptCancelled    = "attempt_cancelled"
	VerificationGroundMissingGrant        = "missing_grant"
	VerificationGroundAdmissionRefused    = "admission_refused"
	VerificationGroundOperationUnresolved = "operation_unresolved"
)

type VerificationCheckpointGround struct {
	Kind           string                    `json:"kind"`
	Subject        core.VerificationSubject  `json:"subject"`
	AttemptID      string                    `json:"attempt_id,omitempty"`
	Ordinal        int                       `json:"ordinal,omitempty"`
	Explanation    string                    `json:"explanation,omitempty"`
	EvidenceIDs    []string                  `json:"evidence_ids,omitempty"`
	Permissions    []verification.Permission `json:"permissions,omitempty"`
	OperationIDs   []string                  `json:"operation_ids,omitempty"`
	ServerVerified bool                      `json:"server_verified"`
}

type VerificationCheckpointSubject struct {
	Subject   core.VerificationSubject `json:"subject"`
	AttemptID string                   `json:"attempt_id,omitempty"`
	State     string                   `json:"state"`
}

type VerificationCheckpoint struct {
	Reason          string                          `json:"reason"`
	RequiredAction  string                          `json:"required_action"`
	Grounds         []VerificationCheckpointGround  `json:"grounds"`
	Subjects        []VerificationCheckpointSubject `json:"subjects"`
	MissingSubjects []core.VerificationSubject      `json:"missing_subjects"`
	OperationIDs    []string                        `json:"operation_ids"`
	EvidenceIDs     []string                        `json:"evidence_ids"`
	SessionID       string                          `json:"session_id"`
	Summary         string                          `json:"summary"`
}

// ValidateVerificationSeal reuses the attempt evaluator inside the backend's
// lifecycle transaction; no caller-supplied success callback is authoritative.
func ValidateVerificationSeal(c VerificationCommand, rows []VerificationRow, now time.Time, actor string) (*VerificationResult, error) {
	if c.Submission == nil {
		return nil, ErrVerificationInvalid
	}
	if err := ValidateVerificationOutcome(*c.Submission); err != nil {
		return nil, err
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
	checkpoint := s.Outcome == "operator_action_required"
	record := &VerificationCheckpoint{Reason: s.Feedback, RequiredAction: s.RequiredAction, Grounds: []VerificationCheckpointGround{}, Subjects: []VerificationCheckpointSubject{}, MissingSubjects: []core.VerificationSubject{}, OperationIDs: []string{}, EvidenceIDs: []string{}, SessionID: c.Access.Claim.SessionID}
	failed, blocked := false, false
	for _, subject := range subjects {
		var latest *VerificationAttempt
		for i := range snapshot.Attempts {
			a := &snapshot.Attempts[i]
			if a.Subject == subject && (latest == nil || a.Ordinal > latest.Ordinal) {
				latest = a
			}
		}
		if latest == nil && checkpoint {
			// VK-13.2: an unstarted subject is recorded as missing evidence, never
			// as an executed attempt. A missing grant is verified from records.
			ground := VerificationCheckpointGround{Kind: VerificationGroundMissingGrant, Subject: subject, ServerVerified: true}
			if verificationSubjectGranted(snapshot, vc, subject) {
				ground.Kind, ground.ServerVerified, ground.Explanation = VerificationGroundAdmissionRefused, false, "verifier-reported admission refusal before start"
			}
			if contract, err := verificationContract(rows, vc.ID, subject); err == nil {
				ground.Permissions = contract.Permissions
			}
			record.Grounds = append(record.Grounds, ground)
			record.MissingSubjects = append(record.MissingSubjects, subject)
			blocked = true
			continue
		}
		if latest == nil || latest.EndedAt == nil {
			return nil, ErrVerificationState
		}
		record.Subjects = append(record.Subjects, VerificationCheckpointSubject{Subject: subject, AttemptID: latest.ID, State: latest.State})
		switch latest.State {
		case "succeeded":
			if err := ValidateVerificationSuccess(snapshot, latest.ID, latest.ExitCode); err != nil {
				return nil, err
			}
		case "failed":
			failed = true
		case "blocked", "waiting", "cancelled", "timed_out":
			blocked = true
			record.Grounds = append(record.Grounds, VerificationCheckpointGround{Kind: "attempt_" + latest.State, Subject: subject, AttemptID: latest.ID, Ordinal: latest.Ordinal, Explanation: latest.Explanation, EvidenceIDs: verificationAttemptEvidence(snapshot, latest.ID), ServerVerified: true})
		default:
			return nil, ErrVerificationState
		}
	}
	unresolved := false
	for _, row := range rows {
		if row.Table == "verification_operations" && row.TaskID == c.Access.TaskID && !verificationOperationResolved(row.State) {
			unresolved = true
			op := verificationDecode[VerificationOperation](row)
			record.OperationIDs = append(record.OperationIDs, row.ID)
			record.Grounds = append(record.Grounds, VerificationCheckpointGround{Kind: VerificationGroundOperationUnresolved, Subject: verificationOperationSubject(snapshot, op), AttemptID: op.RunID, Explanation: row.State, OperationIDs: []string{row.ID}, ServerVerified: true})
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
		if !blocked && !unresolved {
			return nil, ErrVerificationState
		}
	default:
		return nil, ErrVerificationInvalid
	}
	disposition := "selected_kits"
	if len(selection.Subjects) == 0 {
		disposition = "no_selected_kits"
	}
	result := &VerificationResult{Submission: s, Binding: binding, Disposition: disposition, Actor: actor, SealedAt: now}
	if checkpoint {
		for _, e := range snapshot.Evidence {
			record.EvidenceIDs = append(record.EvidenceIDs, e.Envelope.ID)
		}
		record.Summary = verificationCheckpointSummary(record.Grounds)
		result.Checkpoint = record
	}
	return result, nil
}

// ValidateVerificationOutcome publishes the VK-13.1 stage vocabulary. It runs
// after scope authentication and before any staged write.
func ValidateVerificationOutcome(s VerificationSubmission) error {
	switch s.Outcome {
	case "succeeded", "feedback":
		return nil
	case "operator_action_required":
		if strings.TrimSpace(s.Feedback) == "" || strings.TrimSpace(s.RequiredAction) == "" || len(s.Feedback) > VerificationCheckpointTextLimit || len(s.RequiredAction) > VerificationCheckpointTextLimit {
			return ErrVerificationCheckpointIncomplete
		}
		return nil
	}
	return ErrVerificationOutcomeUnsupported
}

func verificationSubjectGranted(snapshot VerificationSnapshot, vc VerificationContext, subject core.VerificationSubject) bool {
	revoked := map[string]bool{}
	for _, r := range snapshot.PermissionRevocations {
		revoked[r.GrantID] = true
	}
	for _, g := range snapshot.PermissionGrants {
		if !revoked[g.ID] && g.ContextID == vc.ID && g.WorkOrderAttemptID == vc.WorkOrderAttemptID && verificationEqual(g.Subject, subject) && verificationEqual(g.Revisions, vc.Revisions) {
			return true
		}
	}
	return false
}

func verificationAttemptEvidence(snapshot VerificationSnapshot, runID string) []string {
	ids := []string{}
	for _, e := range snapshot.Evidence {
		if e.Envelope.RunID == runID {
			ids = append(ids, e.Envelope.ID)
		}
	}
	return ids
}

func verificationOperationSubject(snapshot VerificationSnapshot, op VerificationOperation) core.VerificationSubject {
	for _, a := range snapshot.Attempts {
		if a.ID == op.RunID {
			return a.Subject
		}
	}
	return core.VerificationSubject{}
}

func verificationCheckpointSummary(grounds []VerificationCheckpointGround) string {
	parts := []string{}
	for _, g := range grounds {
		name := g.Subject.ObligationID
		if g.Subject.Kind == "kit" {
			name = g.Subject.KitID + "/" + g.Subject.ExerciseID
		}
		part := g.Kind + " " + g.Subject.Kind + ":" + name
		if g.AttemptID != "" {
			part += " (" + g.AttemptID + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// VerificationSealedCommand supplies the sanitized immutable submission to the
// lifecycle adapter, so feedback cannot bypass the evidence redaction boundary.
func VerificationSealedCommand(c VerificationCommand, mutation VerificationMutation) VerificationCommand {
	for _, row := range mutation.Rows {
		if row.Table == "verification_contexts" && row.ID == c.ContextID {
			vc := verificationDecode[VerificationContext](row)
			if vc.Result != nil {
				c.Submission = &vc.Result.Submission
				c.SealedCheckpoint = vc.Result.Checkpoint
			}
		}
	}
	return c
}
