package store

import (
	"errors"
	"fmt"
	"net/http"
)

// VerificationRefusal names the field, rule, and remedy of a refused
// verification call so a verifier can correct it (component-verification-service
// "Correctable refusals"). It is returned only after scope authentication; an
// unknown or foreign scope keeps the generic ErrVerificationAccess. Rule text
// interpolates only values from the caller's own authenticated context.
type VerificationRefusal struct {
	Code   string
	Field  string
	Rule   string
	Remedy string
	// Class is the sentinel the refusal unwraps to: ErrVerificationInvalid,
	// ErrVerificationState, ErrVerificationConflict, or ErrVerificationAccess.
	Class error
}

// VerificationRemedyError is the original name of the outcome remedies.
type VerificationRemedyError = VerificationRefusal

// verificationRuleLimit bounds a rule sentence (component-verification-service).
const verificationRuleLimit = 1024

func (e *VerificationRefusal) Error() string {
	s := e.Code
	if e.Field != "" {
		s += " at " + e.Field
	}
	if e.Rule != "" {
		s += ": " + e.Rule
	}
	if e.Remedy != "" {
		if e.Rule == "" {
			s += ":"
		}
		s += " " + e.Remedy
	}
	return s
}

func (e *VerificationRefusal) Unwrap() error {
	if e.Class == nil {
		return ErrVerificationInvalid
	}
	return e.Class
}

// HTTPStatus maps the refusal class to its REST status. A grant refusal is
// computed after scope authentication, so it answers 409 rather than 404.
func (e *VerificationRefusal) HTTPStatus() int {
	if errors.Is(e.Unwrap(), ErrVerificationInvalid) {
		return http.StatusBadRequest
	}
	return http.StatusConflict
}

func verificationRefusal(class error, code, field, remedy, rule string, args ...any) error {
	if len(args) > 0 {
		rule = fmt.Sprintf(rule, args...)
	}
	if len(rule) > verificationRuleLimit {
		rule = rule[:verificationRuleLimit-3] + "..."
	}
	return &VerificationRefusal{Code: code, Field: field, Rule: rule, Remedy: remedy, Class: class}
}

// Stable refusal codes (component-verification-service "Correctable refusals").
const (
	VerificationCodeFieldRequired              = "verification_field_required"
	VerificationCodeDispositionInvalid         = "verification_coverage_disposition_invalid"
	VerificationCodeSourceDuplicate            = "verification_coverage_source_duplicate"
	VerificationCodeSubjectUnknown             = "verification_coverage_subject_unknown"
	VerificationCodeObligationIDInvalid        = "verification_obligation_id_invalid"
	VerificationCodeObligationsMismatch        = "verification_coverage_obligations_mismatch"
	VerificationCodeSubjectUnmapped            = "verification_coverage_subject_unmapped"
	VerificationCodePinUnmapped                = "verification_coverage_pin_unmapped"
	VerificationCodeCoverageChanged            = "verification_coverage_changed"
	VerificationCodeCitationUnresolved         = "verification_citation_unresolved"
	VerificationCodePlanMissing                = "verification_coverage_plan_missing"
	VerificationCodeFindingInvalid             = "verification_finding_invalid"
	VerificationCodeSubjectUnregistered        = "verification_subject_unregistered"
	VerificationCodeGrantMissing               = "verification_grant_missing"
	VerificationCodeGrantRevoked               = "verification_grant_revoked"
	VerificationCodeGrantExpired               = "verification_grant_expired"
	VerificationCodeGrantActionsUncovered      = "verification_grant_actions_uncovered"
	VerificationCodeDiscoveryNotValid          = "verification_discovery_not_valid"
	VerificationCodeSelectionInvalid           = "verification_selection_invalid"
	VerificationCodeAttemptRunning             = "verification_attempt_running"
	VerificationCodeSubjectUnstarted           = "verification_subject_unstarted"
	VerificationCodeSubjectNotSucceeded        = "verification_subject_not_succeeded"
	VerificationCodeRetryAuthorizationRequired = "verification_retry_authorization_required"
	VerificationCodeOperationUnresolved        = "verification_operation_unresolved"
	VerificationCodeFeedbackTooLong            = "verification_feedback_too_long"
	VerificationCodeFeedbackUngrounded         = "verification_feedback_ungrounded"
	VerificationCodeCheckpointUngrounded       = "verification_checkpoint_ungrounded"
	VerificationCodeSubmissionConflict         = "verification_submission_conflict"
	VerificationCodeRequestShape               = "verification_request_shape"
	VerificationCodeEvidenceBindingMismatch    = "verification_evidence_binding_mismatch"
	VerificationCodeEvidenceRunUnknown         = "verification_evidence_run_unknown"
)

// Fixed remedies. They carry no record data.
const (
	verificationRemedyList      = "Send every required list, using [] for an empty list, and a non-blank text for every required text."
	verificationRemedyCoverage  = "Rebuild coverage from get_verification_context: map every selected kit subject and registered obligation, cite every governing pin, and keep registered sources."
	verificationRemedyCitation  = "Cite approved_plan at the approved version with a section ID its content holds (such as a done-criterion label), a statement or acceptance criterion ID of a served requirement version, or a section heading of a served design version."
	verificationRemedySubject   = "Copy the subject, including its digest, from get_verification_context."
	verificationRemedyAttempt   = "Finish or report the latest attempt with report_verification_outcome before starting another attempt or sealing."
	verificationRemedyOutcome   = VerificationOutcomeMapping
	verificationRemedyFeedback  = "Submit feedback only with a failed latest attempt, a declaration defect in the context, or at least one finding that cites the approved plan or a served requirement and appears in coverage.sources."
	verificationRemedyConflict  = "Resubmit the original request unchanged, or read the sealed context with get_verification_context."
	verificationRemedyAction    = "Request only actions the grant and the local kit_permissions both cover, or ask an operator to grant the missing action."
	verificationRemedyOperation = "Reconcile each unresolved operation with reconcile_verification_operation, or submit operator_action_required."
	verificationRemedyRetry     = "The latest attempt's retry policy is operator_action_required: submit operator_action_required. The operator issues a one-use authorization with conveyor order recover <order-id> --disposition applied|not_applied (or the order's disposition card), and the next verifier passes --replay-authorization recovery:<request_id>."
	verificationRemedyShape     = "Compare the call with the tool's input schema from tools/list: send every required property, use the listed JSON types and values, and omit properties the schema does not define."
	verificationRemedyBinding   = "Copy each binding field exactly: workspace_id, task_id, work_order_id, work_order_attempt_id, context_id, revisions, and governing_pins from get_verification_context, and run_id and subject from the attempt that start_verification_attempt returned. Omit submitted_by; the service derives it from the authenticated caller."
	verificationRemedyRun       = "Send the run_id that start_verification_attempt returned for this context, not the work-order attempt ID."
)

// Evidence binding refusals (component-verification-service invariant 29).
// They keep the access class, so REST answers 409, and their rule names only
// the request field, never a submitted or expected value. They are returned
// only after the store authenticated the claim's own context.

// verificationEvidenceBindingRefusal names the first envelope field of item
// index that differs from its context or attempt.
func verificationEvidenceBindingRefusal(index int, field string) error {
	path := fmt.Sprintf("evidence[%d].%s", index, field)
	return verificationRefusal(ErrVerificationAccess, VerificationCodeEvidenceBindingMismatch, path, verificationRemedyBinding, "%s must repeat the value the verification context or attempt holds; a different value is refused so a later run cannot relabel earlier provenance.", path)
}

// VerificationSubmittedByRefusal names an evidence item whose supplied
// submitted_by differs from the authenticated caller (invariant 27). The
// caller returns it only after the request's context is proven to belong to
// the claim's order and attempt; otherwise the refusal stays generic.
func VerificationSubmittedByRefusal(index int) error {
	path := fmt.Sprintf("evidence[%d].submitted_by", index)
	return verificationRefusal(ErrVerificationAccess, VerificationCodeEvidenceBindingMismatch, path, verificationRemedyBinding, "%s, when supplied, must name the authenticated caller.", path)
}

// verificationEvidenceRunUnknownRefusal answers an evidence write whose run_id
// names no attempt of the authenticated context. An absent run and a run of
// another context get this identical answer.
func verificationEvidenceRunUnknownRefusal() error {
	return verificationRefusal(ErrVerificationAccess, VerificationCodeEvidenceRunUnknown, "run_id", verificationRemedyRun, "run_id must name an attempt of the verification context the request names.")
}

// VerificationRequestShapeRefusal is the request-shape refusal of an
// authenticated caller: field is the JSON path and rule a server sentence
// naming the violated keyword. Both are bounded to 1024 bytes
// (component-verification-service "Correctable refusals").
func VerificationRequestShapeRefusal(field, rule string) error {
	if len(field) > verificationRuleLimit {
		field = field[:verificationRuleLimit-3] + "..."
	}
	return verificationRefusal(ErrVerificationInvalid, VerificationCodeRequestShape, field, verificationRemedyShape, rule)
}

// VerificationMissingGrantRemedy is the two-phase missing-grant phrase that
// component-verification-runner prints (cmd/conveyor/kit_verify.go#kitMissingGrantRemedy)
// and that a verification_grant_missing refusal carries.
func VerificationMissingGrantRemedy(orderID, contextID string) string {
	return fmt.Sprintf("while this claim is live, an operator runs `conveyor verification permissions inspect %[1]s` and grants the missing subject against context %[2]s; if the grant wait ends first, submit_verification with outcome operator_action_required seals context %[2]s and releases the order, and the operator then recovers verify order %[1]s, waits for the successor claim to prepare a new context, runs `conveyor verification permissions inspect %[1]s` again and grants against that new context; a grant on the sealed context does not carry over", orderID, contextID)
}
