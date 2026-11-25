package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const feedbackSHA = "0123456789abcdef0123456789abcdef01234567"

// TestReadCommit covers the commit or tag read of
// feature-production-feedback-live FB-3: one escaped ref, the full commit
// ID, not found as ErrCommitNotFound, and each other failure categorized.
func TestReadCommit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		path     string
		status   int
		body     string
		want     string
		notFound bool
		category ForgeErrorCategory
		// repoStatus answers the repository read that a 404 makes; 0 is 200.
		repoStatus int
	}{
		{name: "commit", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", body: `{"sha":"` + strings.ToUpper(feedbackSHA) + `"}`, want: feedbackSHA},
		{name: "tag with a slash", ref: "tags/api/v1.2", path: "/repos/acme/app/commits/tags%2Fapi%2Fv1.2", body: `{"sha":"` + feedbackSHA + `"}`, want: feedbackSHA},
		{name: "not found in a readable repository", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 404, body: `{"message":"Not Found"}`, notFound: true},
		{name: "not found in a repository the token cannot read", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 404, body: `{"message":"Not Found"}`, repoStatus: 404, category: ForgePermission},
		{name: "not found and the repository read fails", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 404, body: `{"message":"Not Found"}`, repoStatus: 502, category: ForgeStatus},
		{name: "no commit for the SHA", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 422, body: `{"message":"No commit found for SHA: abc1234"}`, notFound: true},
		{name: "malformed", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", body: `{"sha":"abc"}`, category: ForgeResponse},
		{name: "server failure", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 502, body: `{"message":"bad gateway"}`, category: ForgeStatus},
		{name: "permission", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 403, body: `{"message":"Resource not accessible"}`, category: ForgePermission},
		{name: "rate limited", ref: "abc1234", path: "/repos/acme/app/commits/abc1234", status: 429, body: `{"message":"rate limit"}`, category: ForgeRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() == "/repos/acme/app" && tc.status == 404 {
					if tc.repoStatus != 0 {
						w.WriteHeader(tc.repoStatus)
					}
					fmt.Fprint(w, `{"full_name":"acme/app"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer workspace-token" || r.URL.EscapedPath() != tc.path {
					t.Errorf("request %s", r.URL.EscapedPath())
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			got, err := readCommit(t.Context(), "acme/app", tc.ref, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
			switch {
			case tc.want != "":
				if err != nil || got != tc.want {
					t.Fatalf("got %q err %v", got, err)
				}
			case tc.notFound:
				if !errors.Is(err, ErrCommitNotFound) {
					t.Fatalf("err %v, want not found", err)
				}
			default:
				if ErrorCategory(err) != tc.category || errors.Is(err, ErrCommitNotFound) {
					t.Fatalf("err %v category %q, want %q", err, ErrorCategory(err), tc.category)
				}
			}
		})
	}
}

// TestCompareArrived covers the opt-in arrived-commit read: one bounded
// page, every commit decoded, and Complete only when the page lists every
// arrived commit. CompareAncestry keeps its one-commit request.
func TestCompareArrived(t *testing.T) {
	commits := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"sha":"%040x"}`, i+1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for _, tc := range []struct {
		name     string
		body     string
		count    int
		complete bool
		cause    string
	}{
		{name: "complete", body: `{"status":"ahead","ahead_by":2,"behind_by":0,"commits":` + commits(2) + `}`, count: 2, complete: true},
		{name: "partial", body: `{"status":"ahead","ahead_by":250,"behind_by":0,"commits":` + commits(100) + `}`, count: 100},
		{name: "diverged", body: `{"status":"diverged","ahead_by":1,"behind_by":1,"commits":` + commits(1) + `}`, count: 1},
		{name: "malformed commit", body: `{"status":"ahead","ahead_by":1,"behind_by":0,"commits":[{"sha":"abc"}]}`, cause: "malformed commit"},
		{name: "too many commits", body: `{"status":"ahead","ahead_by":101,"behind_by":0,"commits":` + commits(101) + `}`, cause: "more commits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/app/compare/base-sha...head-sha" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
					t.Errorf("request %s", r.URL)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			got, err := compareArrived(t.Context(), "acme/app", "base-sha", "head-sha", 500, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
			if tc.cause != "" {
				if err == nil || !strings.Contains(err.Error(), tc.cause) || ErrorCategory(err) != ForgeResponse {
					t.Fatalf("err %v, want %q", err, tc.cause)
				}
				return
			}
			if err != nil || len(got.Commits) != tc.count || got.Complete != tc.complete || got.Commits[0] != fmt.Sprintf("%040x", 1) {
				t.Fatalf("got %+v err %v", got, err)
			}
		})
	}
}
