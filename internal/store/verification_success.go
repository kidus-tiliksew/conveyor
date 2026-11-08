package store

import (
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// VerificationOutputGap is one declared evidence output whose accepted items
// for the attempt are below its minimum_items.
type VerificationOutputGap struct {
	Type    string
	Have    int
	Minimum int
}

// VerificationSuccessGaps reports why the success evaluator refuses an
// attempt, in the terms a verifier can act on. MissingAssertions lists the
// required assertion IDs without a passing result, in declared order.
// ShortOutputs lists declared outputs below their minimum_items.
// MissingOperatorObservation reports an interactive or hybrid contract with no
// operator observation. Unnamed reports that the evaluator also refuses for a
// reason outside those three kinds, such as a nonzero exit, a missing
// execution report, an unresolved operation, or an invalid reference.
type VerificationSuccessGaps struct {
	MissingAssertions          []string
	ShortOutputs               []VerificationOutputGap
	MissingOperatorObservation bool
	Unnamed                    bool
}

// Empty reports whether the evaluator found nothing to refuse.
func (g VerificationSuccessGaps) Empty() bool {
	return len(g.MissingAssertions) == 0 && len(g.ShortOutputs) == 0 && !g.MissingOperatorObservation && !g.Unnamed
}

// VerificationSuccessGapsFor names what keeps ValidateVerificationSuccess from
// passing for runID. Both read the same walk, so the helper reports no gaps
// exactly when the evaluator passes, and it never changes the evaluator's
// verdict (req-verification-kits AC-3.2; component-verification-service).
func VerificationSuccessGapsFor(snapshot VerificationSnapshot, runID string, exit *int) VerificationSuccessGaps {
	var gaps VerificationSuccessGaps
	walkVerificationSuccess(snapshot, runID, exit, func(f verificationSuccessFailure) {
		switch f.kind {
		case verificationGapAssertion:
			gaps.MissingAssertions = append(gaps.MissingAssertions, f.assertionID)
		case verificationGapOutput:
			gaps.ShortOutputs = append(gaps.ShortOutputs, f.output)
		case verificationGapOperatorObservation:
			gaps.MissingOperatorObservation = true
		default:
			gaps.Unnamed = true
		}
	})
	return gaps
}

func ValidateVerificationSuccess(snapshot VerificationSnapshot, runID string, exit *int) error {
	var first error
	walkVerificationSuccess(snapshot, runID, exit, func(f verificationSuccessFailure) {
		if first == nil {
			first = f.err
		}
	})
	return first
}

type verificationGapKind int

const (
	verificationGapUnnamed verificationGapKind = iota
	verificationGapAssertion
	verificationGapOutput
	verificationGapOperatorObservation
)

type verificationSuccessFailure struct {
	kind        verificationGapKind
	err         error
	assertionID string
	output      VerificationOutputGap
}

// walkVerificationSuccess applies the success rule (component-verification-service
// invariant 21) in a fixed order and calls fail for every refusal it finds.
// The first call carries the evaluator's verdict. The walk stops where no later
// check can be read, and otherwise continues so the helper can name every gap.
func walkVerificationSuccess(snapshot VerificationSnapshot, runID string, exit *int, fail func(verificationSuccessFailure)) {
	unnamed := func(err error) { fail(verificationSuccessFailure{kind: verificationGapUnnamed, err: err}) }
	var attempt *VerificationAttempt
	for i := range snapshot.Attempts {
		if snapshot.Attempts[i].ID == runID {
			attempt = &snapshot.Attempts[i]
		}
	}
	if attempt == nil {
		unnamed(ErrVerificationAccess)
		return
	}
	var contract *verification.Exercise
	for _, s := range snapshot.Selections {
		for _, subject := range s.Subjects {
			if subject.Subject == attempt.Subject {
				v := subject.Contract
				contract = &v
			}
		}
	}
	for _, o := range snapshot.Obligations {
		if attempt.Subject.Kind == "ordinary" && o.ID == attempt.Subject.ObligationID && o.Digest == attempt.Subject.ContractDigest {
			v := o.Contract
			contract = &v
		}
	}
	if contract == nil {
		unnamed(ErrVerificationState)
		return
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && (exit == nil || *exit != 0) {
		unnamed(ErrVerificationState)
	}
	counts := map[string]int{}
	assertions := map[string]core.AssertionResultPayload{}
	evidence := map[string]core.VerificationEvidence{}
	execution, interaction := false, false
	for _, record := range snapshot.Evidence {
		e := record.Envelope
		if e.RunID != runID || e.Subject != attempt.Subject {
			continue
		}
		evidence[e.ID] = e
		counts[e.Type]++
		switch e.Type {
		case "assertion_result":
			var p core.AssertionResultPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				unnamed(ErrVerificationInvalid)
				return
			}
			if _, ok := assertions[p.AssertionID]; ok {
				unnamed(ErrVerificationInvalid)
				return
			}
			assertions[p.AssertionID] = p
		case "execution_report":
			var p core.ExecutionReportPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.ExitCode != nil && *p.ExitCode == 0 && p.TimedOut != nil && !*p.TimedOut && p.Cancelled != nil && !*p.Cancelled {
				execution = true
			}
		case "operator_observation":
			interaction = true
		}
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && !execution {
		unnamed(ErrVerificationState)
	}
	if (contract.Kind == "interactive" || contract.Kind == "hybrid") && !interaction {
		fail(verificationSuccessFailure{kind: verificationGapOperatorObservation, err: ErrVerificationState})
	}
	if contract.Kind == "observation" && len(contract.EvidenceOutputs) == 0 {
		unnamed(ErrVerificationState)
	}
	for _, output := range contract.EvidenceOutputs {
		if counts[output.Type] < output.MinimumItems {
			fail(verificationSuccessFailure{kind: verificationGapOutput, err: ErrVerificationState, output: VerificationOutputGap{Type: output.Type, Have: counts[output.Type], Minimum: output.MinimumItems}})
		}
	}
	for _, id := range contract.AssertionIDs() {
		p, ok := assertions[id]
		if !ok || p.Outcome != "pass" || len(p.Supporting) == 0 {
			fail(verificationSuccessFailure{kind: verificationGapAssertion, err: ErrVerificationState, assertionID: id})
			continue
		}
		for _, ref := range p.Supporting {
			if ref.EvidenceID != "" {
				if _, ok := evidence[ref.EvidenceID]; !ok {
					unnamed(ErrVerificationInvalid)
					break
				}
			} else {
				found := false
				for _, e := range evidence {
					for _, a := range e.Artifacts {
						if a.ArtifactID == ref.ArtifactID && a.SHA256 == ref.SHA256 {
							found = true
						}
					}
				}
				if !found {
					unnamed(ErrVerificationInvalid)
					break
				}
			}
		}
	}
	completed := map[string]bool{}
	for _, op := range snapshot.Operations {
		bound := op.RunID == runID
		for _, successor := range op.Successors {
			bound = bound || successor.RunID == runID
		}
		if !bound {
			continue
		}
		if len(op.History) == 0 {
			unnamed(ErrVerificationState)
			continue
		}
		state := op.History[len(op.History)-1].State
		if state != "applied" && state != "completed" {
			unnamed(ErrVerificationState)
			continue
		}
		completed[op.StepID+":"+op.Target] = true
	}
	for _, op := range contract.Operations {
		if !completed[op.ID+":"+op.TargetBinding] {
			unnamed(ErrVerificationState)
		}
	}
}
