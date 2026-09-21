package core

import (
	"fmt"
	"net/url"
	"strings"
)

// req-verification-kits REQ-3 and REQ-7; feature-verification-kit-execution VK-4.1 (DEC-43).

// VerificationOperationTransition is the closed VK-4.1 operation machine.
// Receipts do not issue dispatch permission; only a newly committed transition
// from registered or not_applied can authorize a provider call.
func VerificationOperationTransition(from, to string) error {
	allowed := map[string][]string{
		"registered":      {"dispatching", "outcome_unknown"},
		"dispatching":     {"completed", "outcome_unknown", "applied", "not_applied", "unknown"},
		"outcome_unknown": {"applied", "not_applied", "unknown"},
		"unknown":         {"applied", "not_applied", "unknown"},
		"not_applied":     {"dispatching"},
	}
	for _, next := range allowed[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("verification operation transition %s -> %s refused", from, to)
}

// VerificationOperationSubject deliberately excludes revision and digest:
// changing the source cannot hide an unresolved external action (VK-4.1).
func VerificationOperationSubject(s VerificationSubject) string {
	if s.Kind == "kit" {
		return "kit:" + s.KitID + ":" + s.ExerciseID
	}
	return "ordinary:" + s.ObligationID
}

// SanitizeVerificationProviderReference removes URL credentials and query or
// fragment data before the configured secret redactor handles the remainder.
func SanitizeVerificationProviderReference(reference string) string {
	reference = strings.TrimSpace(reference)
	if u, err := url.Parse(reference); err == nil && u.Scheme != "" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	return reference
}

// VerificationBinding retains immutable source authority independently from
// the execution claim which may be replaced after interruption (VK-2, VK-4.1).
type VerificationBinding struct {
	ReviewScope        string                 `json:"review_scope"`
	BaselineSHA        string                 `json:"baseline_sha"`
	ContextID          string                 `json:"context_id"`
	WorkOrderID        string                 `json:"work_order_id"`
	WorkOrderAttemptID string                 `json:"work_order_attempt_id"`
	RunID              string                 `json:"run_id"`
	Revisions          []VerificationRevision `json:"revisions"`
	GoverningPins      []VerificationPin      `json:"governing_pins"`
}

type VerificationAssessment struct {
	ContextIDs  []string                        `json:"context_ids"`
	RunIDs      []string                        `json:"run_ids"`
	EvidenceIDs []string                        `json:"evidence_ids"`
	Mappings    []VerificationAssessmentMapping `json:"mappings"`
	Actor       string                          `json:"actor,omitempty"`
}

type VerificationAssessmentMapping struct {
	DocumentID            string   `json:"document_id"`
	Version               int      `json:"version"`
	AcceptanceCriterionID string   `json:"acceptance_criterion_id"`
	EvidenceIDs           []string `json:"evidence_ids"`
	Assessment            string   `json:"assessment"`
}
