package store

import (
	"context"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// req-verification-kits REQ-7; feature-verification-kit-execution VK-7; independent judgment remains with the reviewer (DEC-29).

type VerificationReviewState struct {
	Rows  []VerificationRow
	Order core.WorkOrder
}

type VerificationReviewReader interface {
	ReadVerificationReview(context.Context, string, string) (VerificationReviewState, error)
}

// ValidateSealedVerificationReview is shared by MCP/in-process preflight and
// every acceptance transaction. Only transaction-bound rows are authoritative.
func ValidateSealedVerificationReview(task core.Task, decision *core.ReviewDecision, state VerificationReviewState) error {
	if !task.SetupContract.VerifyStage {
		return nil
	}
	assessment := decision.VerificationAssessment
	if assessment == nil || len(assessment.ContextIDs) != 1 || assessment.RunIDs == nil || assessment.EvidenceIDs == nil || assessment.Mappings == nil {
		return ErrVerificationInvalid
	}
	row, ok := verificationFind(state.Rows, "verification_contexts", assessment.ContextIDs[0])
	if !ok || row.TaskID != task.ID || row.State != "sealed" {
		return ErrVerificationState
	}
	vc := verificationDecode[VerificationContext](row)
	if vc.SealedAt == nil || vc.Result == nil || vc.Result.Submission.Outcome != "succeeded" || vc.Result.Binding.ContextID != vc.ID || vc.Result.Binding.ReviewScope != vc.ReviewScope || vc.Result.Binding.BaselineSHA != vc.BaselineSHA || !verificationEqual(vc.Result.Binding.Revisions, vc.Revisions) || !verificationEqual(vc.Result.Binding.GoverningPins, vc.GoverningPins) {
		return ErrVerificationState
	}
	for _, other := range state.Rows {
		if other.Table == "verification_contexts" && other.TaskID == task.ID && verificationDecode[VerificationContext](other).CreatedAt.After(vc.CreatedAt) {
			return ErrVerificationState
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
	if vc.ReviewScope != state.Order.ReviewScope || vc.BaselineSHA != state.Order.BaselineSHA || decision.ReviewScope != state.Order.ReviewScope || decision.BaselineSHA != state.Order.BaselineSHA || !bound || state.Order.TaskID != task.ID || state.Order.Stage != core.StageReview || state.Order.HeadSHA != head {
		return ErrVerificationState
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
	for _, id := range assessment.RunIDs {
		if !runs[id] || seen[id] {
			return ErrVerificationInvalid
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
	for _, attempt := range latest {
		if !declaredRuns[attempt.ID] {
			return ErrVerificationInvalid
		}
	}
	seen = map[string]bool{}
	for _, id := range assessment.EvidenceIDs {
		if e, ok := evidence[id]; !ok || seen[id] || !declaredRuns[e.RunID] {
			return ErrVerificationInvalid
		}
		seen[id] = true
	}
	covered := map[string]bool{}
	mapped := map[string]bool{}
	for _, mapping := range assessment.Mappings {
		if strings.TrimSpace(mapping.Assessment) == "" || mapping.EvidenceIDs == nil {
			return ErrVerificationInvalid
		}
		pin := core.VerificationPin{}
		for _, p := range vc.GoverningPins {
			if p.DocumentID == mapping.DocumentID && p.Version == mapping.Version {
				pin = p
			}
		}
		if pin.DocumentID == "" {
			return ErrVerificationInvalid
		}
		key := pin.Kind + ":" + pin.DocumentID
		mappingKey := key + ":" + mapping.AcceptanceCriterionID
		if mapped[mappingKey] {
			return ErrVerificationInvalid
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
				return ErrVerificationInvalid
			}
		} else if mapping.AcceptanceCriterionID != "" {
			return ErrVerificationInvalid
		}
		for _, id := range mapping.EvidenceIDs {
			if _, ok := evidence[id]; !ok || !seen[id] {
				return ErrVerificationInvalid
			}
		}
	}
	for _, pin := range vc.GoverningPins {
		if !covered[pin.Kind+":"+pin.DocumentID] {
			return ErrVerificationInvalid
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
