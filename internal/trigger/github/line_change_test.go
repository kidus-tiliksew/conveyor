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
	return `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":{"blame":{"ranges":[` + strings.Join(ranges, ",") + `]}}}}}`
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
		retry    time.Duration
	}{
		// Stored as the answer.
		{name: "L01 data complete: the range that starts at the line, merged pull requests of the repository", line: 10, body: blameAnswer(blameRange(1, 9, feedbackSHA), blameRange(10, 20, strings.ToUpper(lineChangeCommit), pullNode(7, true, "acme/app"), pullNode(3, true, "Acme/App")), blameRange(21, 30, feedbackSHA)),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{3, 7}}},
		{name: "L02 data complete: the range that ends at the line, no pull request", line: 20, body: blameAnswer(blameRange(1, 9, feedbackSHA), blameRange(10, 20, lineChangeCommit)),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{}}},
		{name: "L03 data complete: open, closed, and other-repository pull requests are left out", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit, pullNode(4, false, "acme/app"), pullNode(5, true, "acme/fork"), pullNode(6, true, "acme/app"), pullNode(6, true, "acme/app"))),
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{6}}},
		// Stored as file_not_found.
		{name: "L04 file null with a missing-path error at the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","path":["repository","blamed","blame"],"message":"x"}]}`, want: LineChange{FileFound: false}},
		{name: "L05 file null with missing-path errors at the file and the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","path":["repository","commit","file"],"message":"x"},{"type":"NOT_FOUND","path":["repository","blamed","blame"],"message":"x"}]}`, want: LineChange{FileFound: false}},
		// Unanswered: a forge failure of its category.
		{name: "L06 file null with no error", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L07 file null with RATE_LIMITED at the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"RATE_LIMITED","path":["repository","blamed","blame"],"message":"x"}]}`, category: ForgeRateLimited},
		{name: "L08 file null with FORBIDDEN at the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"FORBIDDEN","path":["repository","blamed","blame"],"message":"x"}]}`, category: ForgePermission},
		{name: "L09 file null with SERVICE_UNAVAILABLE at the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"SERVICE_UNAVAILABLE","path":["repository","blamed","blame"],"message":"x"}]}`, category: ForgeResponse},
		{name: "L10 file null with SERVICE_UNAVAILABLE at the file", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"SERVICE_UNAVAILABLE","path":["repository","commit","file"],"message":"x"}]}`, category: ForgeResponse},
		{name: "L11 file null with a missing-path error and a rate limit", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","path":["repository","blamed","blame"],"message":"x"},{"type":"RATE_LIMITED","message":"x"}]}`, category: ForgeRateLimited},
		{name: "L12 file null with a NOT_FOUND error that names no path", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","message":"x"}]}`, category: ForgePermission},
		{name: "L13 partial data: the file and an error at the blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":null}},"errors":[{"type":"SERVICE_UNAVAILABLE","path":["repository","blamed","blame"],"message":"x"}]}`, category: ForgeResponse},
		{name: "L14 GraphQL rate limit on HTTP 200", line: 5, body: `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, category: ForgeRateLimited},
		{name: "L15 repository inaccessible", line: 5, body: `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve"}]}`, category: ForgePermission},
		{name: "L16 forbidden with no data", line: 5, body: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`, category: ForgePermission},
		{name: "L17 commit absent", line: 5, body: `{"data":{"repository":{"commit":null,"blamed":null}}}`, notFound: true},
		{name: "L18 object that is not a commit", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Tree"},"blamed":{}}}}`, category: ForgeResponse},
		{name: "L19 no file member", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit"},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L20 empty blame", line: 5, body: blameAnswer(), category: ForgeResponse},
		{name: "L21 no range holds the line", line: 50, body: blameAnswer(blameRange(1, 9, lineChangeCommit)), category: ForgeResponse},
		{name: "L22 connection with a next page", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":true},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L23 connection without pageInfo", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"nodes":[]}}}`), category: ForgeResponse},
		{name: "L24 connection with a null hasNextPage", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":null},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L25 connection without nodes", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false}}}}`), category: ForgeResponse},
		{name: "L26 connection with null nodes", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":null}}}`), category: ForgeResponse},
		{name: "L27 a null node or a node without merged", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[null]}}}`), category: ForgeResponse},
		{name: "L27 a node without merged", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":3,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L28 no pull request connection", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `"}}`), category: ForgeResponse},
		{name: "L29 no blame and no error", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":{}}}}`, category: ForgeResponse},
		{name: "L30 malformed commit ID", line: 5, body: blameAnswer(blameRange(1, 9, "abc")), category: ForgeResponse},
		{name: "L31 malformed JSON", line: 5, body: `not json`, category: ForgeResponse},
		{name: "L32 no data and no errors", line: 5, body: `{}`, category: ForgeResponse},
		{name: "L33 HTTP server failure", line: 5, status: 502, body: `{"message":"bad gateway"}`, category: ForgeStatus},
		{name: "L34 HTTP permission", line: 5, status: 403, body: `{"message":"Resource not accessible by integration"}`, category: ForgePermission},
		{name: "L35 HTTP 429 keeps the retry time", line: 5, status: 429, body: `{"message":"rate limit"}`, headers: map[string]string{"Retry-After": "90"}, category: ForgeRateLimited, retry: 90 * time.Second},
		{name: "L36 HTTP 403 with no remaining requests keeps the retry time", line: 5, status: 403, body: `{"message":"API rate limit exceeded"}`, headers: map[string]string{"X-RateLimit-Remaining": "0", "Retry-After": "120"}, category: ForgeRateLimited, retry: 120 * time.Second},
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
				retryAt, limited := RateLimitRetryAt(err)
				if limited != (tc.category == ForgeRateLimited) {
					t.Fatalf("rate limited=%v, want %v", limited, tc.category == ForgeRateLimited)
				}
				switch {
				case tc.retry > 0 && (retryAt.Before(before.Add(tc.retry-time.Second)) || retryAt.After(time.Now().Add(tc.retry+time.Second))):
					t.Fatalf("retry time %v, want about %v from now", retryAt, tc.retry)
				case tc.retry == 0 && !retryAt.IsZero():
					t.Fatalf("retry time %v, want none named", retryAt)
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
