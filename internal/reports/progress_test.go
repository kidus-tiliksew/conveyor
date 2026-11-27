package reports

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// TestPlanResolve covers the next step of resolving from stored answers: the
// reads still needed, an early ambiguous result, the unique match, no match,
// the results that need no read, and stored results reused as they are
// (approved plan version 3; component-production-feedback invariant 7).
func TestPlanResolve(t *testing.T) {
	repos := []feedback.Repository{{Name: "a", Slug: "acme/a"}, {Name: "b", Slug: "acme/b"}, {Name: "c", Slug: "acme/c"}, {Name: "docs"}}
	request := feedback.ResolveRequest{Version: store.FeedbackVersion{Commit: "AAAAAAA"}, Repositories: repos}
	plan := PlanResolve(request, nil)
	if plan.Final != nil || len(plan.Reads) != 3 || plan.Ref != "aaaaaaa" || plan.Prefix != "aaaaaaa" {
		t.Fatalf("no answers: %+v", plan)
	}
	progress := &store.FeedbackLineProgress{Candidates: map[string]string{"a": store.FeedbackLineAbsent}}
	if plan := PlanResolve(request, progress); plan.Final != nil || len(plan.Reads) != 2 || plan.Reads[0].Name != "b" {
		t.Fatalf("one absence: %+v", plan)
	}
	progress.Candidates["b"] = shaA
	if plan := PlanResolve(request, progress); plan.Final != nil || len(plan.Reads) != 1 {
		t.Fatalf("one match, one candidate left: %+v", plan)
	}
	progress.Candidates["c"] = store.FeedbackLineAbsent
	if plan := PlanResolve(request, progress); plan.Final == nil || *plan.Final != (store.FeedbackLineStep{Repository: "b", Commit: shaA}) {
		t.Fatalf("unique match: %+v", plan)
	}
	two := &store.FeedbackLineProgress{Candidates: map[string]string{"a": shaA, "c": shaA}}
	if plan := PlanResolve(request, two); plan.Final == nil || plan.Final.Unresolved != store.FeedbackReasonAmbiguous {
		t.Fatalf("two matches before every answer: %+v", plan)
	}
	none := &store.FeedbackLineProgress{Candidates: map[string]string{"a": store.FeedbackLineAbsent, "b": store.FeedbackLineAbsent, "c": store.FeedbackLineAbsent}}
	if plan := PlanResolve(request, none); plan.Final == nil || plan.Final.Unresolved != store.FeedbackReasonNoMatch {
		t.Fatalf("no match: %+v", plan)
	}
	for _, tc := range []struct {
		name    string
		request feedback.ResolveRequest
		reason  string
	}{
		{"named repository without a slug", feedback.ResolveRequest{Version: store.FeedbackVersion{Repository: "docs", Commit: "aaaaaaa"}, Repositories: repos}, store.FeedbackReasonNoRepository},
		{"version without a rule", feedback.ResolveRequest{Version: store.FeedbackVersion{Version: "1.2"}, Repositories: repos}, store.FeedbackReasonNoVersionRule},
		{"rule mismatch", feedback.ResolveRequest{Version: store.FeedbackVersion{Version: "1.2"}, Rule: &store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "build-{commit}"}, Repositories: repos}, store.FeedbackReasonRuleMismatch},
	} {
		if plan := PlanResolve(tc.request, nil); plan.Final == nil || plan.Final.Unresolved != tc.reason || len(plan.Reads) != 0 {
			t.Fatalf("%s: %+v, want %s with no read", tc.name, plan, tc.reason)
		}
	}
	stored := &store.FeedbackLineProgress{Unresolved: store.FeedbackReasonNoMatch}
	if plan := PlanResolve(request, stored); plan.Final == nil || plan.Final.Unresolved != store.FeedbackReasonNoMatch || len(plan.Reads) != 0 {
		t.Fatalf("stored reason: %+v", plan)
	}
}

// TestCandidateAnswer covers which commit-read outcomes are definitive.
func TestCandidateAnswer(t *testing.T) {
	if answer, ok := CandidateAnswer("aaaa", shaA, nil); !ok || answer != shaA {
		t.Fatalf("match: %q %v", answer, ok)
	}
	if answer, ok := CandidateAnswer("bbbb", shaA, nil); !ok || answer != store.FeedbackLineAbsent {
		t.Fatalf("another commit: %q %v", answer, ok)
	}
	if answer, ok := CandidateAnswer("aaaa", "", github.CommitNotFoundError("aaaa")); !ok || answer != store.FeedbackLineAbsent {
		t.Fatalf("not found: %q %v", answer, ok)
	}
	for _, err := range []error{github.StatusError(502, errors.New("x")), github.RateLimitedError(time.Time{}, errors.New("x")), github.PermissionError(errors.New("x"))} {
		if _, ok := CandidateAnswer("aaaa", "", err); ok {
			t.Fatalf("%v was definitive", err)
		}
	}
}

// TestTimingResultRoundTrip proves that a stored timing result rebuilds the
// same outcome from the deploy records.
func TestTimingResultRoundTrip(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
	f.task("t-new", shaC, shaB)
	outcome := Timing(f.input(base.Add(time.Hour + 3*time.Minute)))
	rebuilt, err := TimingFromResult(TimingResultOf(outcome), f.deploys)
	if err != nil || !reflect.DeepEqual(rebuilt.Links, outcome.Links) || !rebuilt.Ended {
		t.Fatalf("rebuilt=%+v err=%v, want %+v", rebuilt, err, outcome)
	}
	if _, err := TimingFromResult(store.FeedbackTimingResult{Links: []store.FeedbackTimingLink{{TaskID: "t", DeployID: "gone"}}}, f.deploys); err == nil {
		t.Fatal("a missing deploy was accepted")
	}
}
