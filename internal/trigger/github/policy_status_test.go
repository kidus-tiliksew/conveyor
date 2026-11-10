package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// policyStatusRunner answers the pull request view and the head's statuses,
// and records every call.
func policyStatusRunner(statuses string, postErr error) (ghRunner, *[][]string) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "pr view"):
			return []byte(`{"number":7,"url":"https://github.com/acme/api/pull/7","headRefOid":"abc123"}`), nil
		case strings.Contains(joined, "/statuses?per_page=100"):
			return []byte(statuses), nil
		case strings.Contains(joined, "--method POST"):
			if postErr != nil {
				return nil, postErr
			}
			return []byte(`{"id":99}`), nil
		}
		return nil, fmt.Errorf("unexpected call: %v", args)
	}
	return run, &calls
}

func posts(calls [][]string) []string {
	var out []string
	for _, call := range calls {
		if joined := strings.Join(call, " "); strings.Contains(joined, "--method POST") {
			out = append(out, joined)
		}
	}
	return out
}

func TestPublishPolicyStatusPostsPendingAndSuccessOnTheHead(t *testing.T) {
	for _, state := range []string{PolicyStatusPending, PolicyStatusSuccess} {
		t.Run(state, func(t *testing.T) {
			run, calls := policyStatusRunner(`[]`, nil)
			result, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "conveyor/task-1", HeadSHA: "head-1", State: state}, run)
			if err != nil {
				t.Fatal(err)
			}
			written := posts(*calls)
			if !result.Written || len(written) != 1 {
				t.Fatalf("result=%+v posts=%v", result, written)
			}
			post := written[0]
			for _, want := range []string{"repos/acme/api/statuses/head-1", "state=" + state, "context=conveyor/review-policy", "description=" + PolicyStatusDescription(state), "target_url=https://github.com/acme/api/pull/7"} {
				if !strings.Contains(post, want) {
					t.Fatalf("post %q lacks %q", post, want)
				}
			}
			for _, call := range *calls {
				if strings.Contains(strings.Join(call, " "), "comments") {
					t.Fatalf("policy status wrote a comment: %v", call)
				}
			}
		})
	}
}

func TestPublishPolicyStatusRepeatMakesNoSecondWrite(t *testing.T) {
	latest := fmt.Sprintf(`[{"id":12,"state":"success","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"},
		{"id":3,"state":"failure","context":"Conveyor / Code review"}]`, PolicyStatusDescription(PolicyStatusSuccess))
	run, calls := policyStatusRunner(latest, nil)
	result, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Written || len(posts(*calls)) != 0 {
		t.Fatalf("an identical latest status was written again: %+v %v", result, *calls)
	}
}

// An older entry that matches does not stand in for a newer different one:
// GitHub keeps every status in a context's history, and only the latest one
// counts.
func TestPublishPolicyStatusComparesOnlyTheLatestStatusOfTheContext(t *testing.T) {
	history := fmt.Sprintf(`[{"id":20,"state":"pending","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"},
		{"id":10,"state":"success","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"}]`,
		PolicyStatusDescription(PolicyStatusPending), PolicyStatusDescription(PolicyStatusSuccess))
	run, calls := policyStatusRunner(history, nil)
	result, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Written || len(posts(*calls)) != 1 {
		t.Fatalf("an obsolete matching entry suppressed the write: %+v %v", result, *calls)
	}
	// The same history out of order still reads the highest ID as latest.
	reordered := fmt.Sprintf(`[{"id":10,"state":"success","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"},
		{"id":20,"state":"pending","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"}]`,
		PolicyStatusDescription(PolicyStatusSuccess), PolicyStatusDescription(PolicyStatusPending))
	run, calls = policyStatusRunner(reordered, nil)
	if result, err = publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run); err != nil || !result.Written {
		t.Fatalf("reordered history: result=%+v err=%v calls=%v", result, err, *calls)
	}
}

func TestPublishPolicyStatusRefusesBadInputAndReportsForgeFailure(t *testing.T) {
	run, calls := policyStatusRunner(`[]`, nil)
	for _, publication := range []PolicyStatusPublication{
		{Repo: "acme/api", HeadSHA: "head-1", State: "failure"},
		{Repo: "acme/api", HeadSHA: "head-1", State: ""},
		{Repo: "", HeadSHA: "head-1", State: PolicyStatusPending},
		{Repo: "acme/api", HeadSHA: " ", State: PolicyStatusPending},
	} {
		if _, err := publishPolicyStatus(t.Context(), publication, run); err == nil {
			t.Fatalf("publication %+v was accepted", publication)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("refused input reached the forge: %v", *calls)
	}
	run, _ = policyStatusRunner(`[]`, errors.New("HTTP 502"))
	if _, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusPending}, run); err == nil {
		t.Fatal("a failed post reported success")
	}
	run, _ = policyStatusRunner(`null`, nil)
	if _, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusPending}, run); err == nil {
		t.Fatal("an unreadable status list was treated as empty")
	}
}

// The check-status read ignores both statuses Conveyor publishes and keeps
// every other status (component-submission-merge invariant 59).
func TestCommitChecksIgnoresConveyorReviewPolicyStatus(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[1], "/check-runs") {
			return []byte(`[{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}]`), nil
		}
		return []byte(`[{"state":"pending","statuses":[
			{"context":"conveyor/review-policy","state":"pending"},
			{"context":"Conveyor / Code review","state":"failure"},
			{"context":"ci/unit","state":"success"}
		]}]`), nil
	}
	got, err := commitChecks(t.Context(), "acme/app", "abc123", run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("checks = %+v", got.Checks)
	}
	for _, check := range got.Checks {
		if check.Name == PolicyStatusContext || check.Name == ReviewStatusContext {
			t.Fatalf("Conveyor's own status counted as a check: %+v", got.Checks)
		}
	}
	if pending := got.Named(CheckPending); len(pending) != 0 {
		t.Fatalf("the pending policy status held the merge: %+v", pending)
	}
}
