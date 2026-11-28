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
		{name: "L89 HTTP 502 keeps the Retry-After it states", line: 5, status: 502, body: `{"message":"bad gateway"}`, headers: map[string]string{"Retry-After": "300"}, category: ForgeStatus, retry: 5 * time.Minute},
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
				if _, limited := RateLimitRetryAt(err); limited != (tc.category == ForgeRateLimited) {
					t.Fatalf("rate limited=%v, want %v", limited, tc.category == ForgeRateLimited)
				}
				retryAt := StatedRetryAt(err)
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
	if err != nil || json.Unmarshal(enveloped, &envelope) != nil || string(envelope.Body) != body || !envelope.RemainingZero || envelope.Reset.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("enveloped %s err %v, want the body with the reset", enveloped, err)
	}
}

// TestGraphQLWithoutTheRetryEnvelopeOnARefusedStatus proves that a refused
// status answers every other GraphQL caller as before, an error and no
// bytes, and answers the line read's flag with the error and the envelope of
// the same response.
func TestGraphQLWithoutTheRetryEnvelopeOnARefusedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"bad gateway"}`)
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App")
	raw, err := run(t.Context(), "api", "graphql", "-f", "query={viewer{login}}")
	if raw != nil || ErrorCategory(err) != ForgeStatus || !StatedRetryAt(err).IsZero() {
		t.Fatalf("raw %q err %v, want no bytes and the status failure as before", raw, err)
	}
	enveloped, err := run(t.Context(), "api", "graphql", graphqlRetryEnvelope, "-f", "query={viewer{login}}")
	var envelope graphqlEnvelope
	if ErrorCategory(err) != ForgeStatus || json.Unmarshal(enveloped, &envelope) != nil || envelope.RetryAfter.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("enveloped %s err %v, want the status failure with the envelope", enveloped, err)
	}
}

// lineRetryCell is the expected result of one line read in the retry grid:
// kind is "found", "missing", or the failure category, and retry is how long
// after the request the carried retry time is, zero for none.
type lineRetryCell struct {
	kind  string
	retry time.Duration
}

// lineRetrySignal is one column of the retry grid: the headers of the
// response, and whether its body holds a parsed RATE_LIMITED error.
type lineRetrySignal struct {
	name        string
	headers     map[string]string
	resetIn     time.Duration
	rateLimited bool
}

// TestReadLineChangeRetryGrid proves review round 8 over the whole grid of
// outcome by retry signal. The line read works out one retry time for each
// response before it uses the body: the latest of Retry-After, of
// X-RateLimit-Reset when no request is left or the response is a rate
// limit, and it attaches that time in one place to whatever it returns. So
// a found file, a missing file, and every failure carry it; a RATE_LIMITED
// error decides the outcome before any shape check; a reset beside requests
// left is not a wait; and a stored answer is still the whole answer
// (component-feedback-reports invariant 12; component-production-feedback
// invariant 28). Exactly one request is sent in every cell.
func TestReadLineChangeRetryGrid(t *testing.T) {
	const rl = `{"type":"RATE_LIMITED","message":"API rate limit exceeded"}`
	signals := []lineRetrySignal{
		{name: "S1 no signal"},
		{name: "S2 Retry-After two minutes", headers: map[string]string{"Retry-After": "120"}},
		{name: "S3 no request left, reset in ten minutes", headers: map[string]string{"X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute},
		{name: "S4 reset in ten minutes with requests left", headers: map[string]string{"X-RateLimit-Remaining": "4999"}, resetIn: 10 * time.Minute},
		{name: "S5 RATE_LIMITED error, reset in ten minutes", resetIn: 10 * time.Minute, rateLimited: true},
		{name: "S6 RATE_LIMITED error with no time", rateLimited: true},
		{name: "S7 Retry-After fifteen minutes is later than no request left", headers: map[string]string{"Retry-After": "900", "X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute},
		{name: "S8 no request left is later than Retry-After two minutes", headers: map[string]string{"Retry-After": "120", "X-RateLimit-Remaining": "0"}, resetIn: 10 * time.Minute},
		{name: "S9 RATE_LIMITED error, Retry-After fifteen minutes later than the reset", headers: map[string]string{"Retry-After": "900"}, resetIn: 10 * time.Minute, rateLimited: true},
	}
	found := LineChange{FileFound: true, Commit: lineChangeCommit, PullRequests: []int{7}}
	r := string(ForgeRateLimited)
	// graphqlRow is the expected row of a GraphQL answer that fails with
	// category c: every signal keeps c except a RATE_LIMITED error, which
	// decides the outcome before any shape check.
	graphqlRow := func(c string) [9]lineRetryCell {
		return [9]lineRetryCell{{c, 0}, {c, 2 * time.Minute}, {c, 10 * time.Minute}, {c, 0}, {r, 10 * time.Minute}, {r, 0}, {c, 15 * time.Minute}, {c, 10 * time.Minute}, {r, 15 * time.Minute}}
	}
	// refusedRow is the row of a refused HTTP status that is not a rate
	// limit: its body is not a GraphQL answer, so a RATE_LIMITED error in
	// it decides nothing, and a reset counts only when no request is left.
	refusedRow := func(c string) [9]lineRetryCell {
		return [9]lineRetryCell{{c, 0}, {c, 2 * time.Minute}, {c, 10 * time.Minute}, {c, 0}, {c, 0}, {c, 0}, {c, 15 * time.Minute}, {c, 10 * time.Minute}, {c, 15 * time.Minute}}
	}
	for _, outcome := range []struct {
		name string
		// status is the HTTP status, 200 when zero. body is the answer with
		// no rate-limit error, and limited the same answer with one.
		status        int
		body, limited string
		// hangUp closes the connection with no answer, as a transport
		// failure does, so no signal arrives.
		hangUp   bool
		notFound bool
		want     [9]lineRetryCell
	}{
		{name: "O1 a complete answer with the file found", body: blameAnswer(blameRange(1, 9, lineChangeCommit, pullNode(7, true, "acme/app"))),
			limited: strings.TrimSuffix(blameAnswer(blameRange(1, 9, lineChangeCommit, pullNode(7, true, "acme/app"))), "}") + `,"errors":[` + rl + `]}`,
			want:    [9]lineRetryCell{{"found", 0}, {"found", 2 * time.Minute}, {"found", 10 * time.Minute}, {"found", 0}, {r, 10 * time.Minute}, {r, 0}, {"found", 15 * time.Minute}, {"found", 10 * time.Minute}, {r, 15 * time.Minute}}},
		{name: "O2 the missing-file answer", body: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","path":["repository","blamed","blame"],"message":"x"}]}`,
			limited: `{"data":{"repository":{"commit":{"__typename":"Commit","file":null},"blamed":null}},"errors":[{"type":"NOT_FOUND","path":["repository","blamed","blame"],"message":"x"},` + rl + `]}`,
			want:    [9]lineRetryCell{{"missing", 0}, {"missing", 2 * time.Minute}, {"missing", 10 * time.Minute}, {"missing", 0}, {r, 10 * time.Minute}, {r, 0}, {"missing", 15 * time.Minute}, {"missing", 10 * time.Minute}, {r, 15 * time.Minute}}},
		{name: "O3 forge_status: the commit is absent", body: `{"data":{"repository":{"commit":null,"blamed":null}}}`, limited: `{"data":{"repository":{"commit":null,"blamed":null}},"errors":[` + rl + `]}`, notFound: true, want: graphqlRow(string(ForgeStatus))},
		{name: "O4 forge_permission: a FORBIDDEN error", body: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"x"}]}`, limited: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"x"},` + rl + `]}`, want: graphqlRow(string(ForgePermission))},
		{name: "O5 forge_response: partial data, a repository without its members", body: `{"data":{"repository":{}}}`, limited: `{"data":{"repository":{}},"errors":[` + rl + `]}`, want: graphqlRow(string(ForgeResponse))},
		{name: "O6 forge_response: a malformed body", body: `not json`, limited: `{"data":"x","errors":[{"type":7},` + rl + `]}`, want: graphqlRow(string(ForgeResponse))},
		{name: "O7 forge_status: HTTP 502", status: http.StatusBadGateway, body: `{"message":"bad gateway"}`, limited: `{"errors":[` + rl + `]}`, want: refusedRow(string(ForgeStatus))},
		{name: "O8 forge_permission: HTTP 401", status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`, limited: `{"errors":[` + rl + `]}`, want: refusedRow(string(ForgePermission))},
		{name: "O9 forge_rate_limited: HTTP 429", status: http.StatusTooManyRequests, body: `{"message":"rate limit"}`, limited: `{"errors":[` + rl + `]}`,
			want: [9]lineRetryCell{{r, 0}, {r, 2 * time.Minute}, {r, 10 * time.Minute}, {r, 10 * time.Minute}, {r, 10 * time.Minute}, {r, 0}, {r, 15 * time.Minute}, {r, 10 * time.Minute}, {r, 15 * time.Minute}}},
		{name: "O10 forge_request: the connection closes with no answer", hangUp: true,
			want: [9]lineRetryCell{{"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}, {"forge_request", 0}}},
	} {
		for i, signal := range signals {
			want := outcome.want[i]
			t.Run(outcome.name+"/"+signal.name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if outcome.hangUp {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					for name, value := range signal.headers {
						w.Header().Set(name, value)
					}
					if signal.resetIn != 0 {
						w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(signal.resetIn).Unix(), 10))
					}
					if outcome.status != 0 {
						w.WriteHeader(outcome.status)
					}
					if signal.rateLimited {
						fmt.Fprint(w, outcome.limited)
					} else {
						fmt.Fprint(w, outcome.body)
					}
				}))
				defer server.Close()
				before := time.Now()
				got, err := readLineChange(t.Context(), "acme/app", feedbackSHA, "cmd/main.go", 5, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
				after := time.Now()
				if requests.Load() != 1 {
					t.Fatalf("requests=%d, want exactly one", requests.Load())
				}
				var carried time.Time
				switch want.kind {
				case "found":
					if err != nil || got.FileFound != found.FileFound || got.Commit != found.Commit || !slices.Equal(got.PullRequests, found.PullRequests) {
						t.Fatalf("got %+v err %v, want the whole answer %+v", got, err, found)
					}
					carried = got.RetryAt
				case "missing":
					if err != nil || got.FileFound || got.Commit != "" || len(got.PullRequests) != 0 {
						t.Fatalf("got %+v err %v, want the missing-file answer", got, err)
					}
					carried = got.RetryAt
				default:
					if err == nil || string(ErrorCategory(err)) != want.kind || !got.RetryAt.IsZero() || got.FileFound {
						t.Fatalf("got %+v err %v category %q, want an unanswered %s", got, err, ErrorCategory(err), want.kind)
					}
					if commitAbsent := errors.Is(err, ErrCommitNotFound); commitAbsent != (outcome.notFound && want.kind == string(ForgeStatus)) {
						t.Fatalf("commit not found=%v for %s", commitAbsent, want.kind)
					}
					carried = StatedRetryAt(err)
					if limitedAt, limited := RateLimitRetryAt(err); limited != (want.kind == r) || (limited && !limitedAt.Equal(carried)) {
						t.Fatalf("rate limited=%v at %v, want %v at %v", limited, limitedAt, want.kind == r, carried)
					}
				}
				switch {
				case want.retry == 0 && !carried.IsZero():
					t.Fatalf("carried retry time %v, want none", carried)
				case want.retry > 0 && (carried.Before(before.Add(want.retry-time.Second)) || carried.After(after.Add(want.retry+time.Second))):
					t.Fatalf("carried retry time %v, want about %v after the request", carried, want.retry)
				}
			})
		}
	}
}

// lineTransportServer answers every request with status and headers and
// then a body in one of these ways: "whole" sends body; "truncated"
// declares a longer Content-Length than it sends, so the body is cut short;
// "broken" sends the headers and then an invalid chunk; "hangup" closes the
// connection before any header; "stall" sends nothing until the client
// gives up. resetIn sets X-RateLimit-Reset that long after the request.
func lineTransportServer(t *testing.T, status int, headers map[string]string, resetIn time.Duration, body, mode string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		all := map[string]string{"Content-Type": "application/json"}
		for name, value := range headers {
			all[name] = value
		}
		if resetIn != 0 {
			all["X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10)
		}
		switch mode {
		case "stall":
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		case "hangup", "broken":
			conn, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			if mode == "hangup" {
				return
			}
			fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
			for name, value := range all {
				fmt.Fprintf(buffered, "%s: %s\r\n", name, value)
			}
			fmt.Fprint(buffered, "Transfer-Encoding: chunked\r\n\r\nzz\r\n")
			buffered.Flush()
			return
		}
		for name, value := range all {
			w.Header().Set(name, value)
		}
		if mode == "truncated" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+1000))
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestReadLineChangeTransportPoints proves review round 9 through the real
// HTTP path of the line read, a local server and the REST runner, at every
// point where that path can fail after the request is sent. The status and
// the retry headers are taken as soon as the headers arrive, before the
// body is read, and kept on whatever happens next: a refused status decides
// the category even when its body cannot be read or is cut short, and a
// rate limit keeps its reset, or carries no time so that the worker's
// one-minute fallback applies (component-production-feedback invariant 28;
// component-feedback-reports invariant 12).
func TestReadLineChangeTransportPoints(t *testing.T) {
	const partial = `{"data":{"repository":`
	limited := map[string]string{"X-RateLimit-Remaining": "0"}
	for _, tc := range []struct {
		name     string
		status   int
		headers  map[string]string
		resetIn  time.Duration
		body     string
		mode     string
		category ForgeErrorCategory
		retry    time.Duration
		// cancelled cancels the context before the call; noRequest means
		// the server sees no request.
		cancelled bool
		timeout   bool
		noRequest bool
	}{
		// No response at all: no status and no signal.
		{name: "T01 the connection closes before any header", mode: "hangup", category: ForgeRequest},
		{name: "T02 a timeout before the headers", mode: "stall", timeout: true, category: ForgeRequest},
		{name: "T03 a context cancelled before the call", mode: "whole", status: http.StatusOK, cancelled: true, noRequest: true, category: ForgeRequest},
		// The headers arrive and the body cannot be read or is cut short.
		{name: "T04 HTTP 429 with a reset and a body cut short", status: http.StatusTooManyRequests, resetIn: 10 * time.Minute, body: partial, mode: "truncated", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T05 HTTP 429 with no stated time and a body cut short", status: http.StatusTooManyRequests, body: partial, mode: "truncated", category: ForgeRateLimited},
		{name: "T06 HTTP 429 with a reset and a broken body", status: http.StatusTooManyRequests, resetIn: 10 * time.Minute, mode: "broken", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T07 HTTP 429 with no stated time and a broken body", status: http.StatusTooManyRequests, mode: "broken", category: ForgeRateLimited},
		{name: "T08 HTTP 403 with no request left, a reset, and a body cut short", status: http.StatusForbidden, headers: limited, resetIn: 10 * time.Minute, body: partial, mode: "truncated", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T09 HTTP 403 with no request left, no stated time, and a body cut short", status: http.StatusForbidden, headers: limited, body: partial, mode: "truncated", category: ForgeRateLimited},
		{name: "T10 HTTP 403 with no request left, a reset, and a broken body", status: http.StatusForbidden, headers: limited, resetIn: 10 * time.Minute, mode: "broken", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T11 HTTP 403 with no request left, no stated time, and a broken body", status: http.StatusForbidden, headers: limited, mode: "broken", category: ForgeRateLimited},
		{name: "T12 HTTP 403 secondary limit with Retry-After and a body cut short", status: http.StatusForbidden, headers: map[string]string{"Retry-After": "120"}, body: partial, mode: "truncated", category: ForgeRateLimited, retry: 2 * time.Minute},
		{name: "T13 HTTP 403 without rate-limit headers and a body cut short", status: http.StatusForbidden, body: partial, mode: "truncated", category: ForgePermission},
		{name: "T14 HTTP 401 and a body cut short", status: http.StatusUnauthorized, body: partial, mode: "truncated", category: ForgePermission},
		{name: "T15 HTTP 404 and a body cut short", status: http.StatusNotFound, body: partial, mode: "truncated", category: ForgeStatus},
		{name: "T16 HTTP 502 with Retry-After and a body cut short", status: http.StatusBadGateway, headers: map[string]string{"Retry-After": "120"}, body: partial, mode: "truncated", category: ForgeStatus, retry: 2 * time.Minute},
		{name: "T17 HTTP 502 and a broken body", status: http.StatusBadGateway, mode: "broken", category: ForgeStatus},
		{name: "T18 HTTP 200 with no request left, a reset, and a body cut short", status: http.StatusOK, headers: limited, resetIn: 10 * time.Minute, body: partial, mode: "truncated", category: ForgeResponse, retry: 10 * time.Minute},
		{name: "T19 HTTP 200 with a reset, requests left, and a broken body", status: http.StatusOK, headers: map[string]string{"X-RateLimit-Remaining": "4999"}, resetIn: 10 * time.Minute, mode: "broken", category: ForgeResponse},
		// The body is read and is not JSON.
		{name: "T20 HTTP 200 with no request left, a reset, and a body that is not JSON", status: http.StatusOK, headers: limited, resetIn: 10 * time.Minute, body: `not json`, mode: "whole", category: ForgeResponse, retry: 10 * time.Minute},
		// Each status class with a readable body.
		{name: "T21 HTTP 429 with a reset and a readable body", status: http.StatusTooManyRequests, resetIn: 10 * time.Minute, body: `{"message":"rate limit"}`, mode: "whole", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T22 HTTP 403 with no request left and a readable body", status: http.StatusForbidden, headers: limited, resetIn: 10 * time.Minute, body: `{"message":"API rate limit exceeded"}`, mode: "whole", category: ForgeRateLimited, retry: 10 * time.Minute},
		{name: "T23 HTTP 403 without rate-limit headers and a readable body", status: http.StatusForbidden, body: `{"message":"Resource not accessible"}`, mode: "whole", category: ForgePermission},
		{name: "T24 HTTP 422 without rate-limit headers and a readable body", status: http.StatusUnprocessableEntity, body: `{"message":"x"}`, mode: "whole", category: ForgeStatus},
		{name: "T25 HTTP 500 with no request left, a reset, and a readable body", status: http.StatusInternalServerError, headers: limited, resetIn: 10 * time.Minute, body: `{"message":"x"}`, mode: "whole", category: ForgeStatus, retry: 10 * time.Minute},
		{name: "T26 HTTP 429 with a reset past the year 9999 and a body cut short", status: http.StatusTooManyRequests, headers: map[string]string{"X-RateLimit-Reset": "999999999999"}, body: partial, mode: "truncated", category: ForgeRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := lineTransportServer(t, tc.status, tc.headers, tc.resetIn, tc.body, tc.mode, &requests)
			ctx := t.Context()
			if tc.cancelled {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if tc.timeout {
				timed, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				ctx = timed
			}
			before := time.Now()
			got, err := readLineChange(ctx, "acme/app", feedbackSHA, "cmd/main.go", 5, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
			after := time.Now()
			want := int32(1)
			if tc.noRequest {
				want = 0
			}
			if requests.Load() != want {
				t.Fatalf("requests=%d, want %d", requests.Load(), want)
			}
			if err == nil || ErrorCategory(err) != tc.category || got.FileFound || !got.RetryAt.IsZero() {
				t.Fatalf("got %+v err %v category %q, want an unanswered %s", got, err, ErrorCategory(err), tc.category)
			}
			carried := StatedRetryAt(err)
			if limitedAt, isLimited := RateLimitRetryAt(err); isLimited != (tc.category == ForgeRateLimited) || (isLimited && !limitedAt.Equal(carried)) {
				t.Fatalf("rate limited=%v at %v, want %v at %v", isLimited, limitedAt, tc.category == ForgeRateLimited, carried)
			}
			switch {
			case tc.retry == 0 && !carried.IsZero():
				t.Fatalf("carried retry time %v, want none", carried)
			case tc.retry > 0 && (carried.Before(before.Add(tc.retry-time.Second)) || carried.After(after.Add(tc.retry+time.Second))):
				t.Fatalf("carried retry time %v, want about %v after the request", carried, tc.retry)
			}
		})
	}
}

// TestRESTCallersKeepTheirUnreadableBodyFailure proves that the change of
// round 9 stays inside the line read: through the same transport, a REST
// call and a GraphQL call without the line read's flag still answer an HTTP
// 429 whose body is cut short with the forge_response failure they answered
// before, with no retry time, and a readable 429 still keeps its reset.
func TestRESTCallersKeepTheirUnreadableBodyFailure(t *testing.T) {
	var requests atomic.Int32
	cut := lineTransportServer(t, http.StatusTooManyRequests, nil, 10*time.Minute, `{"message":`, "truncated", &requests)
	run := newRESTRunner(cut.Client(), cut.URL, "workspace-token", "workspace demo GitHub App")
	for name, args := range map[string][]string{
		"REST":    {"api", "repos/acme/app/commits/main"},
		"GraphQL": {"api", "graphql", "-f", "query={viewer{login}}"},
	} {
		raw, err := run(t.Context(), args...)
		if raw != nil || ErrorCategory(err) != ForgeResponse || !StatedRetryAt(err).IsZero() {
			t.Fatalf("%s: raw %q err %v category %q, want the forge_response failure as before", name, raw, err, ErrorCategory(err))
		}
	}
	whole := lineTransportServer(t, http.StatusTooManyRequests, nil, 10*time.Minute, `{"message":"rate limit"}`, "whole", &requests)
	_, err := newRESTRunner(whole.Client(), whole.URL, "workspace-token", "workspace demo GitHub App")(t.Context(), "api", "repos/acme/app/commits/main")
	if retryAt, limited := RateLimitRetryAt(err); !limited || retryAt.Before(time.Now().Add(9*time.Minute)) {
		t.Fatalf("err %v retry %v, want the readable rate limit with its reset as before", err, retryAt)
	}
}

// headerValueClass is one class of value of one retry header: the field
// lines to send, nil for an absent header, and the usable time it gives,
// the zero time for no time. For X-RateLimit-Remaining, noneLeft says
// whether the value states that no request is left.
type headerValueClass struct {
	name     string
	values   func(now time.Time) []string
	want     func(now time.Time) time.Time
	noneLeft bool
}

func fixedValues(values ...string) func(time.Time) []string {
	return func(time.Time) []string { return values }
}

func noTime(time.Time) time.Time { return time.Time{} }

func afterNow(d time.Duration) func(time.Time) time.Time {
	return func(now time.Time) time.Time { return now.Add(d) }
}

func atTime(at time.Time) func(time.Time) time.Time {
	return func(time.Time) time.Time { return at }
}

func unixValue(at func(time.Time) time.Time, format string) func(time.Time) []string {
	return func(now time.Time) []string { return []string{fmt.Sprintf(format, at(now).Unix())} }
}

// retryAfterClasses, resetClasses, and remainingClasses are every class of
// value review round 10 names for each retry header.
var (
	tenMinutes        = afterNow(10 * time.Minute)
	retryAfterClasses = []headerValueClass{
		{name: "RA01 absent", want: noTime},
		{name: "RA02 empty", values: fixedValues(""), want: noTime},
		{name: "RA03 valid 600 seconds", values: fixedValues("600"), want: tenMinutes},
		{name: "RA04 zero, now, no wait", values: fixedValues("0"), want: afterNow(0)},
		{name: "RA05 negative", values: fixedValues("-600"), want: noTime},
		{name: "RA06 not a number", values: fixedValues("soon"), want: noTime},
		{name: "RA07 a sign", values: fixedValues("+600"), want: noTime},
		{name: "RA08 a space inside", values: fixedValues("6 00"), want: noTime},
		{name: "RA09 spaces around, removed by HTTP", values: fixedValues("  600  "), want: tenMinutes},
		{name: "RA10 a decimal", values: fixedValues("600.5"), want: noTime},
		{name: "RA11 too large for 64 bits", values: fixedValues("99999999999999999999"), want: noTime},
		{name: "RA12 too large for a duration", values: fixedValues("9223372037"), want: noTime},
		{name: "RA13 the longest delay, far in the future", values: fixedValues("9223372036"), want: afterNow(9223372036 * time.Second)},
		{name: "RA14 a hundred years", values: fixedValues("3153600000"), want: afterNow(3153600000 * time.Second)},
		{name: "RA15 an HTTP date in the future", values: func(now time.Time) []string { return []string{now.Add(10 * time.Minute).UTC().Format(http.TimeFormat)} }, want: tenMinutes},
		{name: "RA16 an HTTP date in the past, no wait", values: func(now time.Time) []string { return []string{now.Add(-time.Hour).UTC().Format(http.TimeFormat)} }, want: afterNow(-time.Hour)},
		{name: "RA17 an RFC 850 date, in the past", values: fixedValues("Sunday, 06-Nov-94 08:49:37 GMT"), want: atTime(time.Date(1994, 11, 6, 8, 49, 37, 0, time.UTC))},
		{name: "RA18 an asctime date, in the past", values: fixedValues("Sun Nov  6 08:49:37 1994"), want: atTime(time.Date(1994, 11, 6, 8, 49, 37, 0, time.UTC))},
		{name: "RA19 a malformed date", values: fixedValues("Fri, 31 Foo 2026 10:00:00 GMT"), want: noTime},
		{name: "RA20 sent twice, the later time kept", values: fixedValues("60", "600"), want: tenMinutes},
		{name: "RA21 sent twice in one line", values: fixedValues("60, 600"), want: noTime},
		{name: "RA22 sent twice, one value unusable", values: fixedValues("soon", "600"), want: tenMinutes},
	}
	resetClasses = []headerValueClass{
		{name: "RS01 absent", want: noTime},
		{name: "RS02 empty", values: fixedValues(""), want: noTime},
		{name: "RS03 valid, in ten minutes", values: unixValue(tenMinutes, "%d"), want: tenMinutes},
		{name: "RS04 zero, the Unix epoch, no wait", values: fixedValues("0"), want: atTime(time.Unix(0, 0))},
		{name: "RS05 negative", values: fixedValues("-1"), want: noTime},
		{name: "RS06 not a number", values: fixedValues("soon"), want: noTime},
		{name: "RS07 a sign", values: unixValue(tenMinutes, "+%d"), want: noTime},
		{name: "RS08 a space inside", values: func(now time.Time) []string {
			v := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
			return []string{v[:5] + " " + v[5:]}
		}, want: noTime},
		{name: "RS09 spaces around, removed by HTTP", values: unixValue(tenMinutes, "  %d  "), want: tenMinutes},
		{name: "RS10 a decimal", values: unixValue(tenMinutes, "%d.5"), want: noTime},
		{name: "RS11 too large for a time JSON carries", values: fixedValues("999999999999"), want: noTime},
		{name: "RS12 too large for 64 bits", values: fixedValues("99999999999999999999"), want: noTime},
		{name: "RS13 the last second JSON carries, far in the future", values: fixedValues("253402300799"), want: atTime(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))},
		{name: "RS14 in the past, no wait", values: unixValue(afterNow(-time.Hour), "%d"), want: afterNow(-time.Hour)},
		{name: "RS15 far in the future, the year 2100", values: fixedValues("4102444800"), want: atTime(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))},
		{name: "RS16 sent twice, the later time kept", values: func(now time.Time) []string {
			return []string{strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)}
		}, want: tenMinutes},
		{name: "RS17 sent twice in one line", values: func(now time.Time) []string {
			return []string{fmt.Sprintf("%d, %d", now.Unix(), now.Add(10*time.Minute).Unix())}
		}, want: noTime},
		{name: "RS18 sent twice, one value unusable", values: func(now time.Time) []string {
			return []string{"999999999999", strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)}
		}, want: tenMinutes},
	}
	remainingClasses = []headerValueClass{
		{name: "RM01 absent"},
		{name: "RM02 empty", values: fixedValues("")},
		{name: "RM03 zero", values: fixedValues("0"), noneLeft: true},
		{name: "RM04 zero with a leading zero", values: fixedValues("00"), noneLeft: true},
		{name: "RM05 requests left", values: fixedValues("4999")},
		{name: "RM06 negative", values: fixedValues("-1")},
		{name: "RM07 not a number", values: fixedValues("none")},
		{name: "RM08 a sign", values: fixedValues("+0")},
		{name: "RM09 spaces around, removed by HTTP", values: fixedValues(" 0 "), noneLeft: true},
		{name: "RM10 a space inside", values: fixedValues("0 0")},
		{name: "RM11 a decimal", values: fixedValues("0.0")},
		{name: "RM12 too large for 64 bits", values: fixedValues("99999999999999999999")},
		{name: "RM13 sent twice, one value zero", values: fixedValues("5", "0"), noneLeft: true},
		{name: "RM14 sent twice in one line", values: fixedValues("5, 0")},
	}
)

// headerValueCase is one line read in the value-class test: the status,
// the body and how it is sent, the retry headers by class, and what the
// read must return.
type headerValueCase struct {
	name    string
	status  int
	mode    string
	headers map[string]headerValueClass
	// found is true when the read must return the whole answer, and false
	// when it must fail with forge_rate_limited.
	found bool
	want  func(now time.Time) time.Time
}

// TestReadLineChangeHeaderValueClasses proves review round 10 through the
// real HTTP path: each of Retry-After, X-RateLimit-Reset, and
// X-RateLimit-Remaining is read on its own into a usable time or no time,
// for every class of value; one header that cannot be used never changes
// what another one gives; and no header value turns into an error, a
// panic, or an encoding failure. Each row runs alone and, where one header
// is unusable, beside a valid other header. Rate-limit rows run with a
// readable body and with a body cut short, since only a readable 429
// carries the time the shared transport works out for other callers
// (component-production-feedback invariant 28; component-feedback-reports
// invariant 12).
func TestReadLineChangeHeaderValueClasses(t *testing.T) {
	answer := blameAnswer(blameRange(1, 9, lineChangeCommit))
	validRetryAfter := retryAfterClasses[2]
	validReset := resetClasses[2]
	noneLeft := remainingClasses[2]
	var cases []headerValueCase
	limited := func(name string, headers map[string]headerValueClass, want func(time.Time) time.Time) {
		for _, mode := range []string{"whole", "truncated"} {
			cases = append(cases, headerValueCase{name: name + " (" + mode + " body)", status: http.StatusTooManyRequests, mode: mode, headers: headers, want: want})
		}
	}
	for _, class := range retryAfterClasses {
		limited("alone/HTTP 429/"+class.name, map[string]headerValueClass{"Retry-After": class}, class.want)
		if class.want(time.Now()).IsZero() {
			limited("pair/HTTP 429/"+class.name+" beside a valid reset", map[string]headerValueClass{"Retry-After": class, "X-RateLimit-Reset": validReset}, validReset.want)
			cases = append(cases, headerValueCase{name: "pair/HTTP 200 answer/" + class.name + " beside no request left and a valid reset", status: http.StatusOK, mode: "whole",
				headers: map[string]headerValueClass{"Retry-After": class, "X-RateLimit-Remaining": noneLeft, "X-RateLimit-Reset": validReset}, found: true, want: validReset.want})
		}
	}
	for _, class := range resetClasses {
		limited("alone/HTTP 429/"+class.name, map[string]headerValueClass{"X-RateLimit-Reset": class}, class.want)
		if class.want(time.Now()).IsZero() {
			limited("pair/HTTP 429/"+class.name+" beside Retry-After 600", map[string]headerValueClass{"X-RateLimit-Reset": class, "Retry-After": validRetryAfter}, validRetryAfter.want)
			cases = append(cases, headerValueCase{name: "pair/HTTP 200 answer/" + class.name + " beside no request left", status: http.StatusOK, mode: "whole",
				headers: map[string]headerValueClass{"X-RateLimit-Reset": class, "X-RateLimit-Remaining": noneLeft}, found: true, want: noTime})
		}
	}
	for _, class := range remainingClasses {
		want := noTime
		if class.noneLeft {
			want = validReset.want
		}
		cases = append(cases, headerValueCase{name: "alone/HTTP 200 answer/" + class.name + " beside a valid reset", status: http.StatusOK, mode: "whole",
			headers: map[string]headerValueClass{"X-RateLimit-Remaining": class, "X-RateLimit-Reset": validReset}, found: true, want: want})
		if !class.noneLeft {
			cases = append(cases, headerValueCase{name: "pair/HTTP 200 answer/" + class.name + " beside Retry-After 600", status: http.StatusOK, mode: "whole",
				headers: map[string]headerValueClass{"X-RateLimit-Remaining": class, "Retry-After": validRetryAfter}, found: true, want: validRetryAfter.want})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				now := time.Now()
				for name, class := range tc.headers {
					if class.values != nil {
						w.Header()[name] = class.values(now)
					}
				}
				body := `{"message":"API rate limit exceeded"}`
				if tc.found {
					body = answer
				}
				if tc.mode == "truncated" {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+1000))
					body = body[:len(body)/2]
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			before := time.Now()
			got, err := readLineChange(t.Context(), "acme/app", feedbackSHA, "cmd/main.go", 5, newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo GitHub App"))
			after := time.Now()
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want exactly one", requests.Load())
			}
			var carried time.Time
			if tc.found {
				if err != nil || !got.FileFound || got.Commit != lineChangeCommit {
					t.Fatalf("got %+v err %v, want the whole answer", got, err)
				}
				carried = got.RetryAt
			} else {
				if ErrorCategory(err) != ForgeRateLimited || got.FileFound {
					t.Fatalf("got %+v err %v category %q, want an unanswered forge_rate_limited", got, err, ErrorCategory(err))
				}
				carried = StatedRetryAt(err)
				if limitedAt, _ := RateLimitRetryAt(err); !limitedAt.Equal(carried) {
					t.Fatalf("RateLimitRetryAt %v, StatedRetryAt %v, want the same time", limitedAt, carried)
				}
			}
			low, high := tc.want(before), tc.want(after)
			switch {
			case low.IsZero() && !carried.IsZero():
				t.Fatalf("carried %v, want no time", carried)
			case !low.IsZero() && (carried.Before(low.Add(-time.Second)) || carried.After(high.Add(time.Second))):
				t.Fatalf("carried %v, want between %v and %v", carried, low, high)
			}
		})
	}
}
