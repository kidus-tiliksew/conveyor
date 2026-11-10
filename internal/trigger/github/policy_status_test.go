package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		case strings.Contains(joined, "/status?per_page=100"):
			if !strings.Contains(joined, "--paginate --slurp --max-pages 20") {
				return nil, fmt.Errorf("status read is not a bounded page walk: %v", args)
			}
			return []byte(`[{"state":"pending","statuses":` + statuses + `}]`), nil
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

// policyStatusServer serves the combined status of head-1 through the REST
// runner: the first page holds 100 unrelated contexts and names page two in
// its Link header. page decides what each later page answers. Every POST is
// recorded.
type policyStatusServer struct {
	mu    sync.Mutex
	base  string
	reads []string
	posts int
}

func (f *policyStatusServer) start(t *testing.T, page func(w http.ResponseWriter, number string)) ghRunner {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/statuses/head-1":
			f.posts++
			_, _ = w.Write([]byte(`{"id":900}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/commits/head-1/status":
			number := r.URL.Query().Get("page")
			f.reads = append(f.reads, number)
			if number == "" {
				var unrelated []string
				for i := 0; i < 100; i++ {
					unrelated = append(unrelated, fmt.Sprintf(`{"id":%d,"state":"success","context":"ci/job-%03d"}`, 1000+i, i))
				}
				w.Header().Set("Link", "<"+server.URL+r.URL.Path+`?per_page=100&page=2>; rel="next"`)
				_, _ = w.Write([]byte(`{"state":"pending","statuses":[` + strings.Join(unrelated, ",") + `]}`))
				return
			}
			page(w, number)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.base = server.URL
	rest := newRESTRunner(server.Client(), server.URL, "token", "workspace app")
	// The pull request view answers from the fixture; every API call goes
	// through the REST runner and its Link-following page walk.
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "pr" && args[1] == "view" {
			return []byte(`{"number":7,"url":"https://github.com/acme/api/pull/7","headRefOid":"head-1"}`), nil
		}
		return rest(ctx, args...)
	}
}

// An identical status whose context GitHub lists on page two of the combined
// status is found, so a repeat publication writes nothing (component-git-
// delivery invariant 37).
func TestPublishPolicyStatusFindsTheContextOnALaterPage(t *testing.T) {
	fixture := &policyStatusServer{}
	run := fixture.start(t, func(w http.ResponseWriter, number string) {
		_, _ = fmt.Fprintf(w, `{"state":"pending","statuses":[{"id":42,"state":"success","context":"conveyor/review-policy","description":%q,"target_url":"https://github.com/acme/api/pull/7"}]}`,
			PolicyStatusDescription(PolicyStatusSuccess))
	})
	result, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Written || fixture.posts != 0 {
		t.Fatalf("an identical status on page two was written again: result=%+v posts=%d", result, fixture.posts)
	}
	if strings.Join(fixture.reads, ",") != ",2" {
		t.Fatalf("pages read=%q, want the first page and page 2", fixture.reads)
	}
	// A different state on page two is still written, once.
	result, err = publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusPending}, run)
	if err != nil || !result.Written || fixture.posts != 1 {
		t.Fatalf("a changed state result=%+v posts=%d err=%v", result, fixture.posts, err)
	}
}

// A failed read of a later page fails the publication under the forge
// category of the failure; it never counts as an absent status, so nothing
// is posted.
func TestPublishPolicyStatusPropagatesALaterPageReadFailure(t *testing.T) {
	fixture := &policyStatusServer{}
	run := fixture.start(t, func(w http.ResponseWriter, number string) {
		http.Error(w, `{"message":"server error"}`, http.StatusBadGateway)
	})
	_, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run)
	if err == nil || fixture.posts != 0 {
		t.Fatalf("a failed page two read err=%v posts=%d", err, fixture.posts)
	}
	if category := ErrorCategory(err); category == "" || category == ForgeRequest {
		t.Fatalf("failure category=%q err=%v", category, err)
	}
	if strings.Join(fixture.reads, ",") != ",2" {
		t.Fatalf("pages read=%q", fixture.reads)
	}
}

// The page walk is bounded: a status list that still names a next page after
// the bound fails the publication with the forge response category, and
// nothing is posted.
func TestPublishPolicyStatusBoundsThePageWalk(t *testing.T) {
	fixture := &policyStatusServer{}
	// Every later page names another; only the bound ends the walk.
	run := fixture.start(t, func(w http.ResponseWriter, number string) {
		var page int
		_, _ = fmt.Sscanf(number, "%d", &page)
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/api/commits/head-1/status?per_page=100&page=%d>; rel="next"`, fixture.base, page+1))
		_, _ = w.Write([]byte(`{"state":"pending","statuses":[{"id":1,"state":"success","context":"other"}]}`))
	})
	_, err := publishPolicyStatus(t.Context(), PolicyStatusPublication{Repo: "acme/api", Branch: "b", HeadSHA: "head-1", State: PolicyStatusSuccess}, run)
	if ErrorCategory(err) != ForgeResponse || fixture.posts != 0 {
		t.Fatalf("an unbounded status list err=%v posts=%d", err, fixture.posts)
	}
	if len(fixture.reads) != policyStatusMaxPages {
		t.Fatalf("pages read=%d, want the bound %d", len(fixture.reads), policyStatusMaxPages)
	}
}

// The runner refuses a page bound that is not a positive integer before any
// request.
func TestRESTRunnerRefusesABadPageBound(t *testing.T) {
	run := newRESTRunner(http.DefaultClient, "http://127.0.0.1:1", "token", "workspace app")
	for _, bound := range []string{"0", "-1", "x"} {
		if _, err := run(t.Context(), "api", "repos/acme/api/commits/h/status", "--paginate", "--max-pages", bound); ErrorCategory(err) != ForgeRequest {
			t.Fatalf("bound %q err=%v", bound, err)
		}
	}
}
