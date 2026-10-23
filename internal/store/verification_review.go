package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// req-verification-kits REQ-7; component-verification-service (sealed review acceptance); independent adequacy judgment remains with the reviewer (DEC-53).

type VerificationReviewState struct {
	Rows  []VerificationRow
	Order core.WorkOrder
}

type VerificationReviewReader interface {
	ReadVerificationReview(context.Context, string, string) (VerificationReviewState, error)
}

// ReviewVerificationContextID names the sealed context a review round reviews:
// among the task's completed verify orders at the verify head that carry a
// verification_context_id, the one updated last, ties broken on the larger
// order ID. Only a successful seal sets the field on a verify order, so the
// result is the newest context sealed succeeded at the head. It is empty when
// verify_stage is off or no such order exists. PostgreSQL and SingleStore read
// the same order with ORDER BY updated_at DESC, id DESC
// (component-verification-service; req-verification-kits AC-4.2, AC-8.3).
func ReviewVerificationContextID(task core.Task, orders []core.WorkOrder) string {
	if !task.SetupContract.VerifyStage {
		return ""
	}
	head := core.VerifyStageHead(task)
	if head == "" {
		return ""
	}
	var selected *core.WorkOrder
	for i := range orders {
		order := &orders[i]
		if order.TaskID != task.ID || order.Stage != core.StageVerify || order.State != core.WorkOrderCompleted || order.HeadSHA != head || order.VerificationContextID == "" {
			continue
		}
		if selected == nil || order.UpdatedAt.After(selected.UpdatedAt) || (order.UpdatedAt.Equal(selected.UpdatedAt) && order.ID > selected.ID) {
			selected = order
		}
	}
	if selected == nil {
		return ""
	}
	return selected.VerificationContextID
}

// Sealed review assessment refusal codes (component-verification-service
// "Correctable refusals"). Each unwraps to the sentinel its case returned
// before the family existed, so the acceptance predicate is unchanged.
const (
	VerificationCodeReviewContextCount        = "verification_review_context_count"
	VerificationCodeReviewContextNotCurrent   = "verification_review_context_not_current"
	VerificationCodeReviewRunUnknown          = "verification_review_run_unknown"
	VerificationCodeReviewRunMissing          = "verification_review_run_missing"
	VerificationCodeReviewEvidenceUnknown     = "verification_review_evidence_unknown"
	VerificationCodeReviewMappingUnpinned     = "verification_review_mapping_unpinned"
	VerificationCodeReviewMappingDuplicate    = "verification_review_mapping_duplicate"
	VerificationCodeReviewCriterionUnknown    = "verification_review_criterion_unknown"
	VerificationCodeReviewCriterionUnexpected = "verification_review_criterion_unexpected"
	VerificationCodeReviewPinUnmapped         = "verification_review_pin_unmapped"
)

// Fixed remedies of the sealed review assessment family. They carry no record data.
const (
	verificationRemedyReviewList     = "Send verification_assessment with context_ids, run_ids, evidence_ids and mappings, using [] for an empty list, and a non-blank assessment on every mapping."
	verificationRemedyReviewContext  = "Cite exactly the context named by the review order's verification_context_id in get_work_order, read with get_verification_context."
	verificationRemedyReviewRuns     = "Copy run_ids from get_verification_context: list the latest attempt of every subject once, or send [] when no attempt ran."
	verificationRemedyReviewEvidence = "List in evidence_ids only accepted evidence of the listed runs, each once, and cite in a mapping only evidence listed in evidence_ids."
	verificationRemedyReviewMapping  = "Map each governing pin of the context at least once: a requirement mapping names an acceptance criterion of the served version, and a design mapping sends an empty acceptance_criterion_id."
)

func reviewRefusal(class error, code, field, remedy, rule string, args ...any) error {
	return verificationRefusal(class, code, field, remedy, rule, args...)
}

// ValidateSealedVerificationReview is shared by MCP/in-process preflight and
// every acceptance transaction. Only transaction-bound rows are authoritative.
// Callers run it after the review claim is authenticated; a refusal then names
// its field, rule and remedy, interpolating only field indices, counts and the
// sealed context's own pins. A context absent from the task's records and a
// stale review order keep the generic refusal (component-verification-service).
func ValidateSealedVerificationReview(task core.Task, decision *core.ReviewDecision, state VerificationReviewState) error {
	if !task.SetupContract.VerifyStage {
		return nil
	}
	assessment := decision.VerificationAssessment
	if assessment == nil {
		return reviewRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "verification_assessment", verificationRemedyReviewList, "verification_assessment is required while the task's verify_stage is on.")
	}
	for _, list := range []struct {
		name  string
		isNil bool
	}{{"context_ids", assessment.ContextIDs == nil}, {"run_ids", assessment.RunIDs == nil}, {"evidence_ids", assessment.EvidenceIDs == nil}, {"mappings", assessment.Mappings == nil}} {
		if list.isNil {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "verification_assessment."+list.name, verificationRemedyReviewList, "verification_assessment.%s is required; send [] for an empty list.", list.name)
		}
	}
	if len(assessment.ContextIDs) != 1 {
		return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewContextCount, "verification_assessment.context_ids", verificationRemedyReviewContext, "context_ids must name exactly one sealed context; it names %d.", len(assessment.ContextIDs))
	}
	row, ok := verificationFind(state.Rows, "verification_contexts", assessment.ContextIDs[0])
	if !ok || row.TaskID != task.ID {
		// Unknown and foreign contexts stay indistinguishable (AC-7.2).
		return ErrVerificationState
	}
	notCurrent := func() error {
		return reviewRefusal(ErrVerificationState, VerificationCodeReviewContextNotCurrent, "verification_assessment.context_ids[0]", verificationRemedyReviewContext, "The cited context is not the task's newest context sealed succeeded at the verify head.")
	}
	if row.State != "sealed" {
		return notCurrent()
	}
	vc := verificationDecode[VerificationContext](row)
	if vc.SealedAt == nil || vc.Result == nil || vc.Result.Submission.Outcome != "succeeded" || vc.Result.Binding.ContextID != vc.ID || vc.Result.Binding.ReviewScope != vc.ReviewScope || vc.Result.Binding.BaselineSHA != vc.BaselineSHA || !verificationEqual(vc.Result.Binding.Revisions, vc.Revisions) || !verificationEqual(vc.Result.Binding.GoverningPins, vc.GoverningPins) {
		return notCurrent()
	}
	for _, other := range state.Rows {
		if other.Table == "verification_contexts" && other.TaskID == task.ID && verificationDecode[VerificationContext](other).CreatedAt.After(vc.CreatedAt) {
			return notCurrent()
		}
	}
	head := core.VerifyStageHead(task)
	if head == "" || decision.ReviewedCommitSHA != head || (decision.HeadSHA != "" && decision.HeadSHA != head) {
		return ErrVerificationState
	}
	bound := false
	for _, r := range vc.Revisions {
		if r.Repository == task.Repo && r.SHA == head {
			bound = true
		}
	}
	if decision.ReviewScope != state.Order.ReviewScope || decision.BaselineSHA != state.Order.BaselineSHA || state.Order.TaskID != task.ID || state.Order.Stage != core.StageReview || state.Order.HeadSHA != head {
		return ErrVerificationState
	}
	if vc.ReviewScope != state.Order.ReviewScope || vc.BaselineSHA != state.Order.BaselineSHA || !bound {
		return notCurrent()
	}
	if err := VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, state.Order); err != nil {
		return err
	}
	// Exact scope is the frozen context revision set; an assessment cannot
	// substitute another context, and the current reviewed head must match it.
	evidence := map[string]core.VerificationEvidence{}
	runs := map[string]bool{}
	for _, r := range state.Rows {
		if r.ContextID != vc.ID {
			continue
		}
		if r.Table == "verification_attempts" {
			runs[r.ID] = true
		}
		if r.Table == "verification_evidence" && r.State == "evidence" {
			evidence[r.ID] = verificationDecode[VerificationEvidenceRecord](r).Envelope
		}
	}
	seen := map[string]bool{}
	for i, id := range assessment.RunIDs {
		if !runs[id] || seen[id] {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewRunUnknown, fmt.Sprintf("verification_assessment.run_ids[%d]", i), verificationRemedyReviewRuns, "run_ids[%d] is not an attempt of the cited context, or repeats an earlier entry.", i)
		}
		seen[id] = true
	}
	declaredRuns := seen
	snapshot, err := VerificationSnapshotFromRows(state.Rows, vc.ID)
	if err != nil {
		return err
	}
	latest := map[core.VerificationSubject]VerificationAttempt{}
	for _, attempt := range snapshot.Attempts {
		old, ok := latest[attempt.Subject]
		if !ok || attempt.Ordinal > old.Ordinal {
			latest[attempt.Subject] = attempt
		}
	}
	missing := 0
	for _, attempt := range latest {
		if !declaredRuns[attempt.ID] {
			missing++
		}
	}
	if missing > 0 {
		return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewRunMissing, "verification_assessment.run_ids", verificationRemedyReviewRuns, "run_ids omits the latest attempt of %d of the cited context's %d attempted subjects.", missing, len(latest))
	}
	seen = map[string]bool{}
	for i, id := range assessment.EvidenceIDs {
		if e, ok := evidence[id]; !ok || seen[id] || !declaredRuns[e.RunID] {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewEvidenceUnknown, fmt.Sprintf("verification_assessment.evidence_ids[%d]", i), verificationRemedyReviewEvidence, "evidence_ids[%d] is not accepted evidence of a listed run, or repeats an earlier entry.", i)
		}
		seen[id] = true
	}
	covered := map[string]bool{}
	mapped := map[string]bool{}
	for i, mapping := range assessment.Mappings {
		field := fmt.Sprintf("verification_assessment.mappings[%d]", i)
		if mapping.EvidenceIDs == nil {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, field+".evidence_ids", verificationRemedyReviewList, "mappings[%d].evidence_ids is required; send [] for an empty list.", i)
		}
		if strings.TrimSpace(mapping.Assessment) == "" {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, field+".assessment", verificationRemedyReviewList, "mappings[%d].assessment must be non-blank.", i)
		}
		pin := core.VerificationPin{}
		for _, p := range vc.GoverningPins {
			if p.DocumentID == mapping.DocumentID && p.Version == mapping.Version {
				pin = p
			}
		}
		if pin.DocumentID == "" {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewMappingUnpinned, field, verificationRemedyReviewMapping, "mappings[%d] names a document and version that is not a governing pin of the cited context.", i)
		}
		key := pin.Kind + ":" + pin.DocumentID
		mappingKey := key + ":" + mapping.AcceptanceCriterionID
		if mapped[mappingKey] {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewMappingDuplicate, field, verificationRemedyReviewMapping, "mappings[%d] repeats the pin %s %s v%d and criterion of an earlier mapping.", i, pin.Kind, pin.DocumentID, pin.Version)
		}
		mapped[mappingKey] = true
		covered[key] = true
		if pin.Kind == "requirement" {
			found := false
			for _, r := range state.Order.ServedRequirementSnapshot {
				if r.ID == pin.DocumentID && r.Version == pin.Version {
					for _, statement := range r.Statements {
						for _, ac := range statement.AcceptanceCriteria {
							if ac.ID == mapping.AcceptanceCriterionID {
								found = true
							}
						}
					}
				}
			}
			if !found {
				return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewCriterionUnknown, field+".acceptance_criterion_id", verificationRemedyReviewMapping, "mappings[%d].acceptance_criterion_id is not an acceptance criterion of the served requirement %s v%d.", i, pin.DocumentID, pin.Version)
			}
		} else if mapping.AcceptanceCriterionID != "" {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewCriterionUnexpected, field+".acceptance_criterion_id", verificationRemedyReviewMapping, "mappings[%d] maps the %s pin %s v%d, which takes an empty acceptance_criterion_id.", i, pin.Kind, pin.DocumentID, pin.Version)
		}
		for j, id := range mapping.EvidenceIDs {
			if _, ok := evidence[id]; !ok || !seen[id] {
				return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewEvidenceUnknown, fmt.Sprintf("%s.evidence_ids[%d]", field, j), verificationRemedyReviewEvidence, "mappings[%d].evidence_ids[%d] is not listed in evidence_ids.", i, j)
			}
		}
	}
	for _, pin := range vc.GoverningPins {
		if !covered[pin.Kind+":"+pin.DocumentID] {
			return reviewRefusal(ErrVerificationInvalid, VerificationCodeReviewPinUnmapped, "verification_assessment.mappings", verificationRemedyReviewMapping, "The governing %s pin %s v%d of the cited context has no mapping.", pin.Kind, pin.DocumentID, pin.Version)
		}
	}
	return nil
}

func (m *memory) verificationReviewStateLocked(task core.Task, orderID string) VerificationReviewState {
	state := VerificationReviewState{Order: m.workOrders[orderID]}
	for key, row := range m.verificationRows {
		if strings.HasPrefix(key, task.Workspace+"\x00") && row.TaskID == task.ID {
			state.Rows = append(state.Rows, row)
		}
	}
	return state
}

func (m *memory) ReadVerificationReview(ctx context.Context, taskID, orderID string) (VerificationReviewState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[taskID]
	ws, _ := WorkspaceFromContext(ctx)
	if !ok || task.Workspace != ws {
		return VerificationReviewState{}, ErrVerificationAccess
	}
	return m.verificationReviewStateLocked(task, orderID), nil
}

func (m *memory) verifyReviewReadyLocked(task core.Task) bool {
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID == task.ID {
			orders = append(orders, order)
		}
	}
	return core.VerifyReviewReady(task, orders)
}
