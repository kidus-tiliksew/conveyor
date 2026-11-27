package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const lineChangeCommit = "fedcba9876543210fedcba9876543210fedcba98"

// blameAnswer renders one GraphQL answer with a file and blame ranges. Each
// range is startingLine, endingLine, commit, and its pull requests.
func blameAnswer(ranges ...string) string {
	return `{"data":{"repository":{"object":{"__typename":"Commit","file":{"path":"cmd/main.go"},"blame":{"ranges":[` + strings.Join(ranges, ",") + `]}}}}}`
}

func blameRange(start, end int, commit string, pulls ...string) string {
	return fmt.Sprintf(`{"startingLine":%d,"endingLine":%d,"commit":{"oid":%q,"associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}`, start, end, commit, strings.Join(pulls, ","))
}

func pullNode(number int, merged bool, base string) string {
	return fmt.Sprintf(`{"number":%d,"merged":%t,"baseRepository":{"nameWithOwner":%q}}`, number, merged, base)
}

// TestReadLineChange covers the one line read of component-feedback-reports
// invariant 12 (feature-production-feedback-listening FR-2): one GraphQL
// request with the workspace token and the parameters as variables, the
// inclusive range that holds the line, only merged pull requests of the same
// repository, a missing file as its own answer, and every other incomplete
// answer as a failure of the existing categories, GraphQL errors included.
func TestReadLineChange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     int
		status   int
		body     string
		headers  map[string]string
		want     LineChange
		category ForgeErrorCategory
		notFound bool
	}{
		{name: "the range that starts at the line", line: 10, body: blameAnswer(blameRange(1, 9, feedbackSHA), blameRange(10, 20, strings.ToUpper(lineChangeCommit), pullNode(7, true, "acme/app"), pullNode(3, true, "Acme/App")), blameRange(21, 30, feedbackSHA)),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{3, 7}}},
		{name: "the range that ends at the line", line: 20, body: blameAnswer(blameRange(1, 9, feedbackSHA), blameRange(10, 20, lineChangeCommit)),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{}}},
		{name: "open pull requests and other repositories are left out", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit, pullNode(4, false, "acme/app"), pullNode(5, true, "acme/fork"), pullNode(6, true, "acme/app"), pullNode(6, true, "acme/app"))),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{6}}},
		{name: "a missing file", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":null,"blame":null}}}}`, want: LineChange{FileFound: false}},
		{name: "a missing file with a blame error", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":null,"blame":null}}},"errors":[{"type":"NOT_FOUND","path":["repository","object","blame"],"message":"cmd/main.go not found"}]}`, want: LineChange{FileFound: false}},
		{name: "a missing file with another error", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":null,"blame":null}}},"errors":[{"type":"SERVICE_UNAVAILABLE","path":["repository","object","file"],"message":"timeout"}]}`, category: ForgeResponse},
		{name: "GraphQL errors on HTTP 200 with data", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":{"path":"cmd/main.go"},"blame":null}}},"errors":[{"type":"SERVICE_UNAVAILABLE","message":"timeout"}]}`, category: ForgeResponse},
		{name: "GraphQL rate limit", line: 5, body: `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, category: ForgeRateLimited},
		{name: "a repository the token cannot read", line: 5, body: `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve"}]}`, category: ForgePermission},
		{name: "a forbidden answer", line: 5, body: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`, category: ForgePermission},
		{name: "an absent commit", line: 5, body: `{"data":{"repository":{"object":null}}}`, notFound: true},
		{name: "an object that is not a commit", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Tree"}}}}`, category: ForgeResponse},
		{name: "no file member", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit"}}}}`, category: ForgeResponse},
		{name: "empty blame", line: 5, body: blameAnswer(), category: ForgeResponse},
		{name: "no range holds the line", line: 50, body: blameAnswer(blameRange(1, 9, lineChangeCommit)), category: ForgeResponse},
		{name: "a truncated pull request list", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":{"path":"cmd/main.go"},"blame":{"ranges":[{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":true},"nodes":[]}}}]}}}}}`, category: ForgeResponse},
		{name: "no pull request list", line: 5, body: `{"data":{"repository":{"object":{"__typename":"Commit","file":{"path":"cmd/main.go"},"blame":{"ranges":[{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `"}}]}}}}}`, category: ForgeResponse},
		{name: "a malformed commit ID", line: 5, body: blameAnswer(blameRange(1, 9, "abc")), category: ForgeResponse},
		{name: "a malformed answer", line: 5, body: `not json`, category: ForgeResponse},
		{name: "no data and no errors", line: 5, body: `{}`, category: ForgeResponse},
		{name: "server failure", line: 5, status: 502, body: `{"message":"bad gateway"}`, category: ForgeStatus},
		{name: "permission", line: 5, status: 403, body: `{"message":"Resource not accessible by integration"}`, category: ForgePermission},
		{name: "HTTP rate limit keeps the retry time", line: 5, status: 429, body: `{"message":"rate limit"}`, headers: map[string]string{"Retry-After": "90"}, category: ForgeRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.EscapedPath() != "/graphql" || r.Header.Get("Authorization") != "Bearer workspace-token" {
					t.Errorf("request %s %s", r.Method, r.URL.EscapedPath())
				}
				raw, _ := io.ReadAll(r.Body)
				var request struct {
					Query     string            `json:"query"`
					Variables map[string]string `json:"variables"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Errorf("request body %s: %v", raw, err)
				}
				want := map[string]string{"owner": "acme", "name": "app", "oid": feedbackSHA, "path": "cmd/main.go"}
				if request.Query != lineChangeQuery || len(request.Variables) != len(want) {
					t.Errorf("query=%q variables=%v", request.Query, request.Variables)
				}
				for key, value := range want {
					if request.Variables[key] != value {
						t.Errorf("variable %s=%q, want %q", key, request.Variables[key], value)
					}
				}
				for name, value := range tc.headers {
					w.Header().Set(name, value)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			before := time.Now()
			got, err := readLineChange(t.Context(), "acme/app", feedbackSHA, "cmd/main.go", tc.line, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want exactly one", requests.Load())
			}
			switch {
			case tc.notFound:
				if !errors.Is(err, ErrCommitNotFound) {
					t.Fatalf("err %v, want commit not found", err)
				}
			case tc.category != "":
				if ErrorCategory(err) != tc.category || errors.Is(err, ErrCommitNotFound) {
					t.Fatalf("got %+v err %v category %q, want %q", got, err, ErrorCategory(err), tc.category)
				}
				if strings.Contains(err.Error(), "cmd/main.go") {
					t.Fatalf("failure %q holds the file path", err)
				}
				if tc.headers["Retry-After"] != "" {
					retryAt, limited := RateLimitRetryAt(err)
					if !limited || retryAt.Before(before.Add(89*time.Second)) {
						t.Fatalf("retry time %v limited=%v, want about 90 seconds from now", retryAt, limited)
					}
				}
			default:
				if err != nil || got.FileFound != tc.want.FileFound || got.Commit != tc.want.Commit || !slices.Equal(got.PullRequests, tc.want.PullRequests) {
					t.Fatalf("got %+v err %v, want %+v", got, err, tc.want)
				}
			}
		})
	}
}

// TestReadLineChangeRefusesBadParameters proves that a line read with a
// malformed parameter sends no request.
func TestReadLineChangeRefusesBadParameters(t *testing.T) {
	for _, tc := range []struct {
		name, repo, commit, path string
		line                     int
	}{
		{"no owner", "app", feedbackSHA, "a.go", 1},
		{"a short commit", "acme/app", "abc1234", "a.go", 1},
		{"no path", "acme/app", feedbackSHA, "", 1},
		{"line 0", "acme/app", feedbackSHA, "a.go", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			_, err := readLineChange(t.Context(), tc.repo, tc.commit, tc.path, tc.line, func(_ context.Context, _ ...string) ([]byte, error) {
				called = true
				return nil, nil
			})
			if called || ErrorCategory(err) != ForgeRequest {
				t.Fatalf("called=%v err=%v, want a request failure before any call", called, err)
			}
		})
	}
}
