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
	"strconv"
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

// manyPulls renders count merged pull requests of acme/app.
func manyPulls(count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = pullNode(i+1, true, "acme/app")
	}
	return out
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
		// resetIn sets X-RateLimit-Reset to that long after the request, and
		// answerRetry is the retry time a complete answer carries.
		resetIn     time.Duration
		answerRetry time.Duration
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
		// Retry times (review round 7): GitHub answers a GraphQL rate limit with
		// HTTP 200, and its headers state when it allows the next request. The
		// later of Retry-After and X-RateLimit-Reset is kept.
		{name: "L82 HTTP 200 RATE_LIMITED keeps a reset ten minutes away", line: 5, body: `{"data":{"repository":{"commit":null,"blamed":null}},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, headers: map[string]string{"X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "L83 HTTP 200 RATE_LIMITED keeps the reset when it is later than Retry-After", line: 5, body: `{"data":{"repository":{"commit":null,"blamed":null}},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, headers: map[string]string{"Retry-After": "90"}, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "L84 HTTP 200 RATE_LIMITED keeps Retry-After when it is later than the reset", line: 5, body: `{"data":{"repository":{"commit":null,"blamed":null}},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, headers: map[string]string{"Retry-After": "900"}, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 15 * time.Minute},
		{name: "L85 HTTP 200 RATE_LIMITED beside partial data keeps the reset", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":null}},"errors":[{"type":"RATE_LIMITED","path":["repository","blamed","blame"],"message":"x"}]}`, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "L86 HTTP 429 keeps the later reset", line: 5, status: 429, body: `{"message":"rate limit"}`, headers: map[string]string{"Retry-After": "60"}, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "L87 HTTP 403 with no request left keeps the reset", line: 5, status: 403, body: `{"message":"API rate limit exceeded"}`, headers: map[string]string{"X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute, category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "L88 HTTP 403 secondary limit keeps Retry-After", line: 5, status: 403, body: `{"message":"You have exceeded a secondary rate limit"}`, headers: map[string]string{"Retry-After": "300"}, category: ForgeRateLimited, retry: 5 * time.Minute},
		{name: "L89 HTTP 502 holds no retry time", line: 5, status: 502, body: `{"message":"bad gateway"}`, headers: map[string]string{"Retry-After": "300"}, category: ForgeStatus},
		{name: "L90 a complete answer with no request left carries the reset", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit)), headers: map[string]string{"X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute,
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{}}, answerRetry: 10 * time.Minute},
		{name: "L91 a complete answer with requests left carries no retry time", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit)), headers: map[string]string{"X-RateLimit-Remaining": "4999"}, resetIn: 10 * time.Minute,
			want: LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{}}},
		{name: "L37 the data member is not an object", line: 5, body: `{"data":"x"}`, category: ForgeResponse},
		{name: "L38 the repository member is not an object", line: 5, body: `{"data":{"repository":[]}}`, category: ForgeResponse},
		{name: "L39 no commit member", line: 5, body: `{"data":{"repository":{"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L40 the commit member is not an object", line: 5, body: `{"data":{"repository":{"commit":"x","blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L41 no __typename", line: 5, body: `{"data":{"repository":{"commit":{"file":{"path":"cmd/main.go"}},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L42 a null __typename", line: 5, body: `{"data":{"repository":{"commit":{"__typename":null,"file":{"path":"cmd/main.go"}},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L43 a __typename that is not a string", line: 5, body: `{"data":{"repository":{"commit":{"__typename":7,"file":{"path":"cmd/main.go"}},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L44 a file that is not an object", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":"cmd/main.go"},"blamed":{"blame":{"ranges":[]}}}}}`, category: ForgeResponse},
		{name: "L45 a file object without its path", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{}},"blamed":{"blame":{"ranges":[` + blameRange(1, 9, lineChangeCommit, pullNode(7, true, "acme/app")) + `]}}}}}`, category: ForgeResponse},
		{name: "L46 a file with a null path", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":null}},"blamed":{"blame":{"ranges":[` + blameRange(1, 9, lineChangeCommit) + `]}}}}}`, category: ForgeResponse},
		{name: "L47 a file with an empty path", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":""}},"blamed":{"blame":{"ranges":[` + blameRange(1, 9, lineChangeCommit) + `]}}}}}`, category: ForgeResponse},
		{name: "L48 a file with another path", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"other.go"}},"blamed":{"blame":{"ranges":[` + blameRange(1, 9, lineChangeCommit) + `]}}}}}`, category: ForgeResponse},
		{name: "L49 a file path that is not a string", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":7}},"blamed":{"blame":{"ranges":[` + blameRange(1, 9, lineChangeCommit) + `]}}}}}`, category: ForgeResponse},
		{name: "L50 no blamed member", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}}}}}`, category: ForgeResponse},
		{name: "L51 a null blamed member with no error", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":null}}}`, category: ForgeResponse},
		{name: "L52 a blamed member that is not an object", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":[]}}}`, category: ForgeResponse},
		{name: "L53 a null blame", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":{"blame":null}}}}`, category: ForgeResponse},
		{name: "L54 a blame without ranges", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":{"blame":{}}}}}`, category: ForgeResponse},
		{name: "L55 a blame with null ranges", line: 5, body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":{"path":"cmd/main.go"}},"blamed":{"blame":{"ranges":null}}}}}`, category: ForgeResponse},
		{name: "L56 a null range", line: 5, body: blameAnswer(`null`, blameRange(1, 9, lineChangeCommit)), category: ForgeResponse},
		{name: "L57 a range without its starting line", line: 5, body: blameAnswer(`{"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L58 a range without its ending line", line: 5, body: blameAnswer(`{"startingLine":1,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L59 a range that ends before it starts", line: 5, body: blameAnswer(blameRange(9, 1, lineChangeCommit)), category: ForgeResponse},
		{name: "L60 a range that starts at line 0", line: 5, body: blameAnswer(blameRange(0, 9, lineChangeCommit)), category: ForgeResponse},
		{name: "L61 a range line that is not a number", line: 5, body: blameAnswer(`{"startingLine":"1","endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L62 the range that holds the line without its commit", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9}`), category: ForgeResponse},
		{name: "L63 another range without its commit", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit), `{"startingLine":10,"endingLine":20,"commit":null}`), category: ForgeResponse},
		{name: "L64 a commit without its ID", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L65 a commit with a null ID", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":null,"associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L66 two ranges hold the line", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit), blameRange(5, 12, feedbackSHA)), category: ForgeResponse},
		{name: "L67 a pull request without its number", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"merged":true,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L68 a pull request with number 0", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":0,"merged":true,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L69 a pull request with a null number", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":null,"merged":true,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L70 a pull request number that is not a number", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":"7","merged":true,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L71 a pull request with a null merged state", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":null,"baseRepository":{"nameWithOwner":"acme/app"}}]}}}`), category: ForgeResponse},
		{name: "L72 a merged pull request without its base repository", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true}]}}}`), category: ForgeResponse},
		{name: "L73 a merged pull request with a null base repository", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":null}]}}}`), category: ForgeResponse},
		{name: "L74 a merged pull request with an empty base repository", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":{}}]}}}`), category: ForgeResponse},
		{name: "L75 a base repository with a null name", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":{"nameWithOwner":null}}]}}}`), category: ForgeResponse},
		{name: "L76 a base repository with an empty name", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":{"nameWithOwner":""}}]}}}`), category: ForgeResponse},
		{name: "L77 a base repository name that is not a slug", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":{"nameWithOwner":"acme"}}]}}}`), category: ForgeResponse},
		{name: "L78 a base repository name with quote characters", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":true,"baseRepository":{"nameWithOwner":"\"acme/app\""}}]}}}`), category: ForgeResponse},
		{name: "L79 an open pull request with an empty base repository", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"merged":false,"baseRepository":{}}]}}}`), category: ForgeResponse},
		{name: "L80 a hasNextPage that is not a boolean", line: 5, body: blameAnswer(`{"startingLine":1,"endingLine":9,"commit":{"oid":"` + lineChangeCommit + `","associatedPullRequests":{"pageInfo":{"hasNextPage":"false"},"nodes":[]}}}`), category: ForgeResponse},
		{name: "L81 more nodes than one page holds", line: 5, body: blameAnswer(blameRange(1, 9, lineChangeCommit, manyPulls(LineChangePullRequests+1)...)), category: ForgeResponse},
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
				if tc.resetIn != 0 {
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(tc.resetIn).Unix(), 10))
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
				switch {
				case tc.answerRetry > 0 && (got.RetryAt.Before(before.Add(tc.answerRetry-time.Second)) || got.RetryAt.After(time.Now().Add(tc.answerRetry+time.Second))):
					t.Fatalf("answer retry time %v, want about %v from now", got.RetryAt, tc.answerRetry)
				case tc.answerRetry == 0 && !got.RetryAt.IsZero():
					t.Fatalf("answer retry time %v, want none", got.RetryAt)
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

// TestReadLineChangeTimeout proves that a line read that times out is a
// request failure with no retry time: nothing is stored, and the worker asks
// again on its next attempt (review round 7).
func TestReadLineChangeTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := readLineChange(ctx, "acme/app", feedbackSHA, "cmd/main.go", 5, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
	if ErrorCategory(err) != ForgeRequest {
		t.Fatalf("err %v category %q, want a request failure", err, ErrorCategory(err))
	}
	if _, limited := RateLimitRetryAt(err); limited {
		t.Fatal("a timeout carried a rate limit")
	}
}

// TestGraphQLWithoutTheRetryEnvelope proves that every other GraphQL caller
// gets the body alone, as before: without the line read's flag, an HTTP 200
// answer with rate-limit headers is returned unchanged.
func TestGraphQLWithoutTheRetryEnvelope(t *testing.T) {
	body := `{"data":{"viewer":{"login":"x"}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App")
	raw, err := run(t.Context(), "api", "graphql", "-f", "query={viewer{login}}")
	if err != nil || string(raw) != body {
		t.Fatalf("raw %s err %v, want the body unchanged", raw, err)
	}
	enveloped, err := run(t.Context(), "api", "graphql", graphqlRetryEnvelope, "-f", "query={viewer{login}}")
	var envelope graphqlEnvelope
	if err != nil || json.Unmarshal(enveloped, &envelope) != nil || string(envelope.Body) != body || !envelope.RemainingZero || envelope.RetryAt.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("enveloped %s err %v, want the body with the reset", enveloped, err)
	}
}
