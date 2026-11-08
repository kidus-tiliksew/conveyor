package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// TestKitRunnerPlainScriptAssertionOutcomes runs real children: a plain
// script that declares a required assertion it cannot send ends waiting with
// an explanation naming the assertion and the new-attempt remedy, and the
// same script with no required assertions succeeds on exit zero
// (req-verification-kits AC-3.2; component-verification-runner).
func TestKitRunnerPlainScriptAssertionOutcomes(t *testing.T) {
	report := verification.EvidenceOutput{Type: "execution_report", SchemaVersion: 1, MinimumItems: 1}
	for _, tc := range []struct {
		name       string
		assertions []verification.Assertion
		outputs    []verification.EvidenceOutput
		want       string
		explain    string
	}{
		{name: "declared assertion waits", assertions: []verification.Assertion{{ID: "touched-package-tests-pass"}}, outputs: []verification.EvidenceOutput{report},
			want:    "waiting",
			explain: "required assertion touched-package-tests-pass has no passing result. This attempt has ended and takes no more evidence. Have the exercise send it through the relay, or register a contract that doesn't require it, then start a new attempt."},
		{name: "no required assertions succeeds", assertions: []verification.Assertion{}, outputs: []verification.EvidenceOutput{report}, want: "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", "exit 0"}, TimeoutSeconds: 5, RequiredAssertions: tc.assertions, EvidenceOutputs: tc.outputs, Operations: []verification.Operation{}}
			f := newKitExecutionFixture(t, e)
			subject := f.snapshot.Attempts[0].Subject
			environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
			err := f.v.launch(t.Context(), e, f.v.root, t.TempDir(), "run", "grant", subject, environment, nil)
			if f.outcome != tc.want {
				t.Fatalf("outcome %q want %q: %v", f.outcome, tc.want, err)
			}
			if tc.want == "succeeded" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.HasSuffix(err.Error(), "waiting "+tc.explain) {
				t.Fatalf("waiting explanation %v, want %q", err, tc.explain)
			}
		})
	}
}

func TestWaitingExplanation(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("a%d", i+1)
		}
		return out
	}
	const ended = ". This attempt has ended and takes no more evidence. "
	for _, tc := range []struct {
		name string
		gaps store.VerificationSuccessGaps
		want string
	}{
		{"no gaps keeps the fixed sentence", store.VerificationSuccessGaps{}, waitingExplanationFallback},
		{"unnamed reason keeps the fixed sentence", store.VerificationSuccessGaps{MissingAssertions: []string{"a"}, Unnamed: true}, waitingExplanationFallback},
		{"two assertions", store.VerificationSuccessGaps{MissingAssertions: []string{"a", "b"}},
			"required assertions a, b have no passing result" + ended + "Have the exercise send them through the relay, or register a contract that doesn't require them, then start a new attempt."},
		{"short output", store.VerificationSuccessGaps{ShortOutputs: []store.VerificationOutputGap{{Type: "assertion_result", Have: 0, Minimum: 1}}},
			"evidence output below its minimum_items: assertion_result has 0 of 1" + ended + "Have the exercise send it through the relay, or register a contract that doesn't require it, then start a new attempt."},
		{"operator observation", store.VerificationSuccessGaps{MissingOperatorObservation: true},
			"the operator observation is missing" + ended + "Have the operator record it, or register a contract that doesn't require it, then start a new attempt."},
		{"every kind", store.VerificationSuccessGaps{MissingAssertions: []string{"a"}, ShortOutputs: []store.VerificationOutputGap{{Type: "api_exchange", Have: 1, Minimum: 2}}, MissingOperatorObservation: true},
			"required assertion a has no passing result; evidence output below its minimum_items: api_exchange has 1 of 2; the operator observation is missing" + ended + "Have the exercise send the missing evidence through the relay and the operator record the observation, or register a contract that doesn't require them, then start a new attempt."},
		{"ten IDs then a count", store.VerificationSuccessGaps{MissingAssertions: ids(12), ShortOutputs: []store.VerificationOutputGap{{Type: "api_exchange", Have: 0, Minimum: 1}}},
			"required assertions a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, and 2 more have no passing result; 1 evidence output is below its minimum_items" + ended + "Have the exercise send them through the relay, or register a contract that doesn't require them, then start a new attempt."},
		{"budget shared across kinds", store.VerificationSuccessGaps{MissingAssertions: ids(9), ShortOutputs: []store.VerificationOutputGap{{Type: "api_exchange", Have: 0, Minimum: 1}, {Type: "state_observation", Have: 0, Minimum: 1}}},
			"required assertions a1, a2, a3, a4, a5, a6, a7, a8, a9 have no passing result; evidence output below its minimum_items: api_exchange has 0 of 1, and 1 more" + ended + "Have the exercise send them through the relay, or register a contract that doesn't require them, then start a new attempt."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingExplanation(tc.gaps); got != tc.want {
				t.Fatalf("explanation\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
