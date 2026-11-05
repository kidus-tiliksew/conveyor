package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommitChecksClassifiesAndExcludesConveyorReview(t *testing.T) {
	var requests []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		requests = append(requests, strings.Join(args, " "))
		if strings.Contains(args[1], "/check-runs") {
			return []byte(`[{"total_count":3,"check_runs":[
				{"name":"build","status":"completed","conclusion":"success","html_url":"https://github.com/acme/app/runs/1"},
				{"name":"lint","status":"completed","conclusion":"neutral","html_url":"https://github.com/acme/app/runs/2"},
				{"name":"docs","status":"completed","conclusion":"skipped"}
			]},{"total_count":3,"check_runs":[
				{"name":"e2e","status":"completed","conclusion":"timed_out","details_url":"https://ci.example/e2e"},
				{"name":"deploy","status":"in_progress","conclusion":null}
			]}]`), nil
		}
		return []byte(`[{"state":"failure","statuses":[
			{"context":"Conveyor / Code review","state":"failure","target_url":"https://github.com/acme/app/pull/1"},
			{"context":"vercel","state":"success","target_url":"javascript:alert(1)"},
			{"context":"legacy","state":"error","target_url":"https://user:secret@ci.example/x"},
			{"context":"preview","state":"pending"}
		]}]`), nil
	}
	got, err := commitChecks(t.Context(), "acme/app", "abc123", run)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 ||
		requests[0] != "api repos/acme/app/commits/abc123/check-runs?filter=latest&per_page=100 --paginate --slurp" ||
		requests[1] != "api repos/acme/app/commits/abc123/status?per_page=100 --paginate --slurp" {
		t.Fatalf("requests = %q", requests)
	}
	want := map[string]Check{
		"build":   {Name: "build", State: CheckPassed, URL: "https://github.com/acme/app/runs/1"},
		"lint":    {Name: "lint", State: CheckPassed, URL: "https://github.com/acme/app/runs/2"},
		"docs":    {Name: "docs", State: CheckPassed},
		"e2e":     {Name: "e2e", State: CheckFailed, URL: "https://ci.example/e2e"},
		"deploy":  {Name: "deploy", State: CheckPending},
		"vercel":  {Name: "vercel", State: CheckPassed},
		"legacy":  {Name: "legacy", State: CheckFailed},
		"preview": {Name: "preview", State: CheckPending},
	}
	if len(got.Checks) != len(want) {
		t.Fatalf("checks = %+v", got.Checks)
	}
	for _, check := range got.Checks {
		if check != want[check.Name] {
			t.Errorf("check = %+v, want %+v", check, want[check.Name])
		}
	}
	if failed := got.Named(CheckFailed); len(failed) != 2 || failed[0].Name != "e2e" || failed[1].Name != "legacy" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestCheckRunStateCountsEveryOtherConclusionAsFailed(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled", "timed_out", "action_required", "stale", "", "startup_failure"} {
		if got := checkRunState("completed", conclusion); got != CheckFailed {
			t.Errorf("conclusion %q = %s", conclusion, got)
		}
	}
	for _, conclusion := range []string{"success", "neutral", "skipped"} {
		if got := checkRunState("completed", conclusion); got != CheckPassed {
			t.Errorf("conclusion %q = %s", conclusion, got)
		}
	}
	for _, status := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		if got := checkRunState(status, ""); got != CheckPending {
			t.Errorf("status %q = %s", status, got)
		}
	}
}

// A head with no check reads as none reported, and only an answer that holds
// the arrays may read that way.
func TestCommitChecksRefusesAnAnswerMissingItsArray(t *testing.T) {
	empty := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[1], "/check-runs") {
			return []byte(`[{"total_count":0,"check_runs":[]}]`), nil
		}
		return []byte(`[{"state":"pending","statuses":[]}]`), nil
	}
	got, err := commitChecks(t.Context(), "acme/app", "abc", empty)
	if err != nil || len(got.Checks) != 0 {
		t.Fatalf("none reported = %+v, %v", got, err)
	}
	for name, answer := range map[string]string{
		"check runs": `/check-runs`,
		"statuses":   `/status`,
	} {
		t.Run(name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Contains(args[1], answer) {
					return []byte(`[{"message":"partial"}]`), nil
				}
				return empty(t.Context(), args...)
			}
			if _, err := commitChecks(t.Context(), "acme/app", "abc", run); ErrorCategory(err) != ForgeResponse {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The REST runner follows every Link page and maps a refused read to the
// permission category, the case an App without Checks read reaches.
func TestCommitChecksThroughRESTRunner(t *testing.T) {
	refuse := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.URL.Query().Get("page") == "":
			if r.URL.Query().Get("filter") != "latest" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Link", "<"+server.URL+r.URL.Path+`?filter=latest&per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"one","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"check_runs":[{"name":"two","status":"queued"}]}`))
		default:
			_, _ = w.Write([]byte(`{"statuses":[]}`))
		}
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, "token", "workspace app")
	got, err := commitChecks(t.Context(), "acme/app", "abc", run)
	if err != nil || len(got.Checks) != 2 || got.Checks[1].State != CheckPending {
		t.Fatalf("paged read = %+v, %v", got, err)
	}
	refuse = true
	_, err = commitChecks(t.Context(), "acme/app", "abc", run)
	var forge *Error
	if !errors.As(err, &forge) || forge.Category != ForgePermission {
		t.Fatalf("refused read = %v", err)
	}
}

func TestCommitChecksRefusesAnEmptyCommit(t *testing.T) {
	if _, err := commitChecks(t.Context(), "acme/app", " ", nil); ErrorCategory(err) != ForgeRequest {
		t.Fatalf("err = %v", err)
	}
}
