package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// successFixture builds one ordinary attempt "run" for contract e with the
// given evidence. Evidence IDs are their types unless set.
func successFixture(e verification.Exercise, evidence ...core.VerificationEvidence) VerificationSnapshot {
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: e.ID, ContractDigest: "digest"}
	snapshot := VerificationSnapshot{
		Obligations: []VerificationObligation{{ID: e.ID, Digest: "digest", Contract: e}},
		Attempts:    []VerificationAttempt{{ID: "run", Subject: subject, State: "running"}},
	}
	for _, item := range evidence {
		item.RunID, item.Subject = "run", subject
		if item.ID == "" {
			item.ID = item.Type
		}
		snapshot.Evidence = append(snapshot.Evidence, VerificationEvidenceRecord{Envelope: item})
	}
	return snapshot
}

func successExecutionReport(exit int) core.VerificationEvidence {
	no := false
	return core.VerificationEvidence{Type: "execution_report", Payload: core.JSONPayload(core.ExecutionReportPayload{ExitCode: &exit, TimedOut: &no, Cancelled: &no})}
}

func successAssertion(id, outcome string) core.VerificationEvidence {
	return core.VerificationEvidence{ID: "assertion-" + id, Type: "assertion_result", Payload: core.JSONPayload(core.AssertionResultPayload{AssertionID: id, Outcome: outcome, Supporting: []core.VerificationReference{{EvidenceID: "execution_report"}}})}
}

func successExercise(kind string, assertions []string, outputs ...verification.EvidenceOutput) verification.Exercise {
	e := verification.Exercise{ID: "check", Kind: kind, RequiredAssertions: []verification.Assertion{}, EvidenceOutputs: outputs, Operations: []verification.Operation{}}
	for _, id := range assertions {
		e.RequiredAssertions = append(e.RequiredAssertions, verification.Assertion{ID: id})
	}
	return e
}

// TestVerificationSuccessGapsAgreeWithEvaluator pins each gap kind the helper
// names, and that the helper reports no gaps exactly when
// ValidateVerificationSuccess passes (req-verification-kits AC-3.2). Removing a
// gap kind from the helper turns that case's gap into Unnamed or drops it, and
// this test fails.
func TestVerificationSuccessGapsAgreeWithEvaluator(t *testing.T) {
	zero, seven := 0, 7
	report := verification.EvidenceOutput{Type: "execution_report", SchemaVersion: 1, MinimumItems: 1}
	assertionOutput := verification.EvidenceOutput{Type: "assertion_result", SchemaVersion: 1, MinimumItems: 1}
	observationOutput := verification.EvidenceOutput{Type: "operator_observation", SchemaVersion: 1, MinimumItems: 1}
	observation := core.VerificationEvidence{Type: "operator_observation"}
	for _, tc := range []struct {
		name     string
		snapshot VerificationSnapshot
		exit     *int
		want     VerificationSuccessGaps
		wantErr  error
	}{
		{name: "plain script passes", snapshot: successFixture(successExercise("script", nil, report), successExecutionReport(0)), exit: &zero},
		{name: "asserted script passes", snapshot: successFixture(successExercise("script", []string{"tests-pass"}, report, assertionOutput), successExecutionReport(0), successAssertion("tests-pass", "pass")), exit: &zero},
		{name: "hybrid passes", snapshot: successFixture(successExercise("hybrid", nil, report, observationOutput), successExecutionReport(0), observation), exit: &zero},
		{name: "missing assertion", snapshot: successFixture(successExercise("script", []string{"tests-pass"}, report), successExecutionReport(0)), exit: &zero,
			want: VerificationSuccessGaps{MissingAssertions: []string{"tests-pass"}}, wantErr: ErrVerificationState},
		{name: "failed assertion has no passing result", snapshot: successFixture(successExercise("script", []string{"tests-pass"}, report), successExecutionReport(0), successAssertion("tests-pass", "fail")), exit: &zero,
			want: VerificationSuccessGaps{MissingAssertions: []string{"tests-pass"}}, wantErr: ErrVerificationState},
		{name: "short output", snapshot: successFixture(successExercise("script", nil, report, assertionOutput), successExecutionReport(0)), exit: &zero,
			want: VerificationSuccessGaps{ShortOutputs: []VerificationOutputGap{{Type: "assertion_result", Have: 0, Minimum: 1}}}, wantErr: ErrVerificationState},
		{name: "missing operator observation", snapshot: successFixture(successExercise("interactive", nil)), exit: &zero,
			want: VerificationSuccessGaps{MissingOperatorObservation: true}, wantErr: ErrVerificationState},
		{name: "every kind in declared order", snapshot: successFixture(successExercise("hybrid", []string{"b", "a"}, report, assertionOutput), successExecutionReport(0)), exit: &zero,
			want: VerificationSuccessGaps{MissingAssertions: []string{"b", "a"}, ShortOutputs: []VerificationOutputGap{{Type: "assertion_result", Have: 0, Minimum: 1}}, MissingOperatorObservation: true}, wantErr: ErrVerificationState},
		{name: "nonzero exit is unnamed", snapshot: successFixture(successExercise("script", nil, report), successExecutionReport(7)), exit: &seven,
			want: VerificationSuccessGaps{Unnamed: true}, wantErr: ErrVerificationState},
		{name: "missing execution report is unnamed beside a named gap", snapshot: successFixture(successExercise("script", []string{"tests-pass"})), exit: &zero,
			want: VerificationSuccessGaps{MissingAssertions: []string{"tests-pass"}, Unnamed: true}, wantErr: ErrVerificationState},
		{name: "unresolved reference is unnamed", snapshot: func() VerificationSnapshot {
			s := successFixture(successExercise("script", []string{"tests-pass"}, report), successExecutionReport(0))
			bad := successAssertion("tests-pass", "pass")
			bad.Payload = core.JSONPayload(core.AssertionResultPayload{AssertionID: "tests-pass", Outcome: "pass", Supporting: []core.VerificationReference{{EvidenceID: "elsewhere"}}})
			bad.RunID, bad.Subject = "run", s.Attempts[0].Subject
			s.Evidence = append(s.Evidence, VerificationEvidenceRecord{Envelope: bad})
			return s
		}(), exit: &zero, want: VerificationSuccessGaps{Unnamed: true}, wantErr: ErrVerificationInvalid},
		{name: "unknown run is unnamed", snapshot: func() VerificationSnapshot {
			s := successFixture(successExercise("script", nil, report), successExecutionReport(0))
			s.Attempts[0].ID = "other"
			return s
		}(), exit: &zero, want: VerificationSuccessGaps{Unnamed: true}, wantErr: ErrVerificationAccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVerificationSuccess(tc.snapshot, "run", tc.exit)
			gaps := VerificationSuccessGapsFor(tc.snapshot, "run", tc.exit)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("evaluator verdict %v, want %v", err, tc.wantErr)
			}
			if (err == nil) != gaps.Empty() {
				t.Fatalf("helper disagrees with evaluator: verdict %v, gaps %+v", err, gaps)
			}
			if !reflect.DeepEqual(gaps, tc.want) {
				t.Fatalf("gaps %+v, want %+v", gaps, tc.want)
			}
		})
	}
}
