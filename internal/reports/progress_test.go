package reports

import (
	"errors"
	"reflect"
	"strings"
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
	if plan.Final != nil || len(plan.Reads) != 3 || plan.Ref != "aaaaaaa" || plan.Prefix != "aaaaaaa" || !store.FeedbackLineInputDigest(plan.Input) ||
		!reflect.DeepEqual(plan.Keep, []string{"acme/a", "acme/b", "acme/c"}) {
		t.Fatalf("no answers: %+v", plan)
	}
	input := plan.Input
	progress := &store.FeedbackLineProgress{Input: input, Candidates: map[string]string{"acme/a": store.FeedbackLineAbsent}}
	if plan := PlanResolve(request, progress); plan.Final != nil || len(plan.Reads) != 2 || plan.Reads[0].Name != "b" {
		t.Fatalf("one absence: %+v", plan)
	}
	progress.Candidates["acme/b"] = shaA
	if plan := PlanResolve(request, progress); plan.Final != nil || len(plan.Reads) != 1 {
		t.Fatalf("one match, one candidate left: %+v", plan)
	}
	progress.Candidates["acme/c"] = store.FeedbackLineAbsent
	if plan := PlanResolve(request, progress); plan.Final == nil || !reflect.DeepEqual(*plan.Final, store.FeedbackLineStep{Repository: "b", Slug: "acme/b", Commit: shaA}) {
		t.Fatalf("unique match: %+v", plan)
	}
	two := &store.FeedbackLineProgress{Input: input, Candidates: map[string]string{"acme/a": shaA, "acme/c": shaA}}
	if plan := PlanResolve(request, two); plan.Final == nil || plan.Final.Unresolved != store.FeedbackReasonAmbiguous {
		t.Fatalf("two matches before every answer: %+v", plan)
	}
	none := &store.FeedbackLineProgress{Input: input, Candidates: map[string]string{"acme/a": store.FeedbackLineAbsent, "acme/b": store.FeedbackLineAbsent, "acme/c": store.FeedbackLineAbsent}}
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
	// A configured name that now points at another GitHub repository has no
	// answer for that repository, so it is read again; the old answer stays a
	// fact about the old repository.
	repointed := feedback.ResolveRequest{Version: request.Version, Repositories: []feedback.Repository{{Name: "a", Slug: "Acme/A2"}, {Name: "b", Slug: "acme/b"}, {Name: "c", Slug: "acme/c"}}}
	if plan := PlanResolve(repointed, none); plan.Final != nil || len(plan.Reads) != 1 || plan.Reads[0].Slug != "Acme/A2" || !reflect.DeepEqual(plan.Keep, []string{"acme/a2", "acme/b", "acme/c"}) {
		t.Fatalf("a repointed name: %+v, want one read of the new repository", plan)
	}
	stored := &store.FeedbackLineProgress{Unresolved: store.FeedbackReasonNoMatch}
	if plan := PlanResolve(request, stored); plan.Final == nil || plan.Final.Unresolved != store.FeedbackReasonNoMatch || len(plan.Reads) != 0 {
		t.Fatalf("stored reason: %+v", plan)
	}
}

// TestPlanResolveInput covers review round 3 finding 2: stored answers are
// used only with the resolution input they were read with. After the
// version rule changes, the plan reads every candidate again and never
// combines an old answer with a new one, and stored answers with no input are
// not used. A stored resolution or reason is final and is used whatever the
// input is now (component-production-feedback invariant 7;
// component-feedback-reports invariant 12; approved plan version 3).
func TestPlanResolveInput(t *testing.T) {
	repos := []feedback.Repository{{Name: "a", Slug: "acme/a"}, {Name: "b", Slug: "acme/b"}}
	tagRule := func(text string) *store.EnvironmentVersionRule {
		return &store.EnvironmentVersionRule{Kind: store.EnvironmentTagTemplate, Text: text}
	}
	old := feedback.ResolveRequest{Version: store.FeedbackVersion{Version: "1.2"}, Rule: tagRule("v{version}"), Repositories: repos}
	oldPlan := PlanResolve(old, nil)
	if oldPlan.Ref != "tags/v1.2" || oldPlan.Prefix != "" || len(oldPlan.Reads) != 2 {
		t.Fatalf("old rule: %+v", oldPlan)
	}
	// acme/a held the old tag; acme/b was not answered yet.
	progress := &store.FeedbackLineProgress{Input: oldPlan.Input, Candidates: map[string]string{"acme/a": shaA}}
	if plan := PlanResolve(old, progress); plan.Final != nil || len(plan.Reads) != 1 || plan.Reads[0].Name != "b" {
		t.Fatalf("old rule with one answer: %+v", plan)
	}
	for _, tc := range []struct {
		name string
		rule *store.EnvironmentVersionRule
	}{
		{"another tag", tagRule("release-{version}")},
		{"a commit pattern", &store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "{commit}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := old
			request.Rule = tc.rule
			request.Version.Version = "bbbbbbb"
			if tc.rule.Kind == store.EnvironmentTagTemplate {
				request.Version.Version = "1.2"
			}
			plan := PlanResolve(request, progress)
			if plan.Input == oldPlan.Input {
				t.Fatalf("the input did not change: %+v", plan)
			}
			// The old match is not used: both candidates are read again.
			if plan.Final != nil || len(plan.Reads) != 2 {
				t.Fatalf("a changed rule: %+v, want both candidates read again", plan)
			}
			// One new answer alone decides nothing with the old match: acme/b
			// matches under the new input, and acme/a is still unanswered.
			mixed := &store.FeedbackLineProgress{Input: plan.Input, Candidates: map[string]string{"acme/b": shaB}}
			if next := PlanResolve(request, mixed); next.Final != nil || len(next.Reads) != 1 || next.Reads[0].Name != "a" {
				t.Fatalf("one new answer: %+v, want acme/a read again", next)
			}
			mixed.Candidates["acme/a"] = store.FeedbackLineAbsent
			if next := PlanResolve(request, mixed); next.Final == nil || !reflect.DeepEqual(*next.Final, store.FeedbackLineStep{Repository: "b", Slug: "acme/b", Commit: shaB}) {
				t.Fatalf("both new answers: %+v, want acme/b", next)
			}
		})
	}
	legacy := &store.FeedbackLineProgress{Candidates: map[string]string{"acme/a": shaA, "acme/b": store.FeedbackLineAbsent}}
	if plan := PlanResolve(old, legacy); plan.Final != nil || len(plan.Reads) != 2 {
		t.Fatalf("answers with no input: %+v, want both read again", plan)
	}
	changed := old
	changed.Rule = tagRule("release-{version}")
	for _, final := range []*store.FeedbackLineProgress{
		{Repository: "a", Slug: "acme/a", Commit: shaA},
		{Unresolved: store.FeedbackReasonNoVersionRule},
	} {
		plan := PlanResolve(changed, final)
		want := store.FeedbackLineStep{Repository: final.Repository, Slug: final.Slug, Commit: final.Commit, Unresolved: final.Unresolved}
		if plan.Final == nil || !reflect.DeepEqual(*plan.Final, want) || len(plan.Reads) != 0 {
			t.Fatalf("a final result after the rule changed: %+v, want %+v", plan, want)
		}
	}
	// Prune names the stored answers the plan does not use.
	if _, stale := oldPlan.Prune(progress); stale {
		t.Fatal("answers for the plan's input and candidates were pruned")
	}
	if prune, stale := PlanResolve(changed, progress).Prune(progress); !stale || prune.Candidate != "" || prune.Input == oldPlan.Input {
		t.Fatalf("answers for another input: prune=%+v stale=%v", prune, stale)
	}
	removed := old
	removed.Repositories = repos[1:]
	if prune, stale := PlanResolve(removed, progress).Prune(progress); !stale || !reflect.DeepEqual(prune.Keep, []string{"acme/b"}) {
		t.Fatalf("an answer of a removed repository: prune=%+v stale=%v", prune, stale)
	}
	if _, stale := PlanResolve(old, nil).Prune(&store.FeedbackLineProgress{Repository: "a", Slug: "acme/a", Commit: shaA}); stale {
		t.Fatal("a final result was pruned")
	}
	step := oldPlan.Step(repos[1], store.FeedbackLineAbsent)
	if step.Candidate != "acme/b" || step.Input != oldPlan.Input || !reflect.DeepEqual(step.Keep, oldPlan.Keep) {
		t.Fatalf("step=%+v", step)
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
	// A malformed commit with no error proves nothing.
	for _, commit := range []string{"", "abc", strings.ToUpper(shaA), `"` + shaA + `"`} {
		if answer, ok := CandidateAnswer("aaaa", commit, nil); ok {
			t.Fatalf("malformed commit %q gave the definitive answer %q", commit, answer)
		}
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
