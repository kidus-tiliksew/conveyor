package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const githubAPIBaseURL = "https://api.github.com"

var (
	defaultRESTHTTPClient = http.DefaultClient
	defaultRESTBaseURL    = githubAPIBaseURL
)

type credentialContextKey struct{}

type Credential struct {
	Token    string
	Identity string
}

// WithCredential binds the explicit stored credential used by subsequent
// forge operations. The value is never read from process environment state.
func WithCredential(ctx context.Context, token, identity string) context.Context {
	return context.WithValue(ctx, credentialContextKey{}, Credential{Token: token, Identity: identity})
}

func credentialFromContext(ctx context.Context) (Credential, bool) {
	credential, ok := ctx.Value(credentialContextKey{}).(Credential)
	credential.Token = strings.TrimSpace(credential.Token)
	credential.Identity = strings.TrimSpace(credential.Identity)
	return credential, ok && credential.Token != ""
}

func ghWithTokenAndIdentity(token, identity string) ghRunner {
	return newRESTRunner(defaultRESTHTTPClient, defaultRESTBaseURL, token, identity)
}

// RESTRunner returns the explicit-token operation seam used by the monitor.
// It never consults process environment or host credential state.
func RESTRunner(token, identity string) func(context.Context, ...string) ([]byte, error) {
	return ghWithTokenAndIdentity(token, identity)
}

type restClient struct {
	http     *http.Client
	baseURL  string
	token    string
	identity string
}

type secretSafeError struct {
	message string
	err     error
}

func (e *secretSafeError) Error() string { return e.message }
func (e *secretSafeError) Unwrap() error { return e.err }

func newRESTRunner(client *http.Client, baseURL, token, identity string) ghRunner {
	api := &restClient{http: client, baseURL: strings.TrimRight(baseURL, "/"), token: strings.TrimSpace(token), identity: strings.TrimSpace(identity)}
	return api.run
}

func (c *restClient) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.token == "" {
		return nil, PermissionError(fmt.Errorf("%s is missing; add or replace it in settings", c.identityLabel()))
	}
	if len(args) == 0 {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("GitHub REST operation is required")}
	}
	switch args[0] {
	case "api":
		return c.api(ctx, args[1:]...)
	case "issue":
		return c.issue(ctx, args[1:]...)
	case "label":
		return c.label(ctx, args[1:]...)
	case "pr":
		return c.pullRequest(ctx, args[1:]...)
	default:
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("unsupported GitHub REST operation %q", args[0])}
	}
}

func (c *restClient) identityLabel() string {
	if c.identity != "" {
		return c.identity
	}
	return "forge token"
}

func (c *restClient) request(ctx context.Context, method, endpoint, accept string, body any) ([]byte, http.Header, int, error) {
	raw, signals, err := c.exchange(ctx, method, endpoint, accept, body)
	return raw, signals.header, signals.Status, err
}

// responseSignals is what a GitHub response states before its body is read:
// its status, the forge category that status decides by itself, its
// headers, and the time they arrived. exchange takes them as soon as the
// headers arrive and returns them on every path after that, so a body that
// cannot be read, is cut short, or is not what the caller expects loses
// none of them. Received is false when no response arrived: a connect
// failure, a timeout before the headers, or a cancelled context.
type responseSignals struct {
	Received bool `json:"received"`
	Status   int  `json:"status"`
	// Category is the category of a refused status (statusCategory), and
	// empty for a 2xx status.
	Category ForgeErrorCategory `json:"category"`
	header   http.Header
	at       time.Time
}

// readResponseSignals takes the signals of a response from its status line
// and headers alone.
func readResponseSignals(status int, header http.Header, now time.Time) responseSignals {
	return responseSignals{Received: true, Status: status, Category: statusCategory(status, header), header: header, at: now}
}

// statusCategory is the forge category a refused status decides by itself:
// a rate limit for 429, and for 403 or 422 with no request left or a
// Retry-After header, since GitHub can answer abuse protection with either;
// a permission failure for any other 401 or 403; and a status failure for
// every other status outside 2xx. It is empty for a 2xx status.
func statusCategory(status int, header http.Header) ForgeErrorCategory {
	if status >= 200 && status < 300 {
		return ""
	}
	limited := header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != ""
	switch {
	case status == http.StatusTooManyRequests || (status == http.StatusForbidden || status == http.StatusUnprocessableEntity) && limited:
		return ForgeRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ForgePermission
	}
	return ForgeStatus
}

// exchange sends one request and reads its answer. The signals of the
// response are taken before its body is read and come back on every path
// after the response arrived. What a caller of request gets is unchanged: a
// body that cannot be read is a forge_response failure whatever the status,
// and a refused status with a readable body is a failure of the category the
// status decides.
func (c *restClient) exchange(ctx context.Context, method, endpoint, accept string, body any) ([]byte, responseSignals, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, responseSignals{}, &Error{Category: ForgeRequest, Err: fmt.Errorf("encode GitHub REST request: %w", err)}
		}
		payload = bytes.NewReader(encoded)
	}
	requestURL := endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		requestURL = c.baseURL + "/" + strings.TrimLeft(endpoint, "/")
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, payload)
	if err != nil {
		return nil, responseSignals{}, &Error{Category: ForgeRequest, Err: fmt.Errorf("build GitHub REST request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, responseSignals{}, &Error{Category: ForgeRequest, Err: fmt.Errorf("GitHub REST transport: %w", c.safeError(err))}
	}
	defer response.Body.Close()
	signals := readResponseSignals(response.StatusCode, response.Header, time.Now())
	raw, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return nil, signals, &Error{Category: ForgeResponse, Err: fmt.Errorf("read GitHub REST response: %w", c.safeError(readErr))}
	}
	if signals.Category == "" {
		return raw, signals, nil
	}
	if signals.Category == ForgePermission {
		return nil, signals, &Error{Category: ForgePermission, status: signals.Status, Err: fmt.Errorf("%s is expired, revoked, or lacks permission; replace it in settings (GitHub HTTP %d)", c.identityLabel(), signals.Status)}
	}
	failure := &Error{Category: signals.Category, status: signals.Status, Err: fmt.Errorf("GitHub REST status %d: %s", signals.Status, c.secretSafe(secretSafeMessage(raw)))}
	if signals.Category == ForgeRateLimited {
		failure.RetryAt = rateLimitRetryAt(signals.header, signals.at)
	}
	return nil, signals, failure
}

func (c *restClient) secretSafe(value string) string {
	if c.token == "" {
		return value
	}
	return strings.ReplaceAll(value, c.token, "[REDACTED]")
}

func (c *restClient) safeError(err error) error {
	return &secretSafeError{message: c.secretSafe(err.Error()), err: err}
}

func secretSafeMessage(raw []byte) string {
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &response) == nil && strings.TrimSpace(response.Message) != "" {
		return strings.TrimSpace(response.Message)
	}
	return http.StatusText(http.StatusBadGateway)
}

func (c *restClient) api(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("GitHub REST endpoint is required")}
	}
	if args[0] == "graphql" {
		return c.graphql(ctx, args[1:]...)
	}
	method, endpoint, accept := http.MethodGet, "", "application/vnd.github+json"
	fields := map[string]any{}
	paginate, slurp := false, false
	// maxPages bounds a --paginate walk; 0 leaves it unbounded. A walk that
	// reaches the bound while GitHub still names a next page fails instead of
	// returning a truncated list, so a caller never reads absence from pages
	// it did not read.
	maxPages := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--max-pages":
			i++
			if i < len(args) {
				limit, err := strconv.Atoi(args[i])
				if err != nil || limit < 1 {
					return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("GitHub REST page bound %q is not a positive integer", args[i])}
				}
				maxPages = limit
			}
		case "--method":
			i++
			if i < len(args) {
				method = strings.ToUpper(args[i])
			}
		case "--paginate":
			paginate = true
		case "--slurp":
			slurp = true
		case "-H":
			i++
			if i < len(args) && strings.HasPrefix(strings.ToLower(args[i]), "accept:") {
				accept = strings.TrimSpace(strings.TrimPrefix(args[i], "Accept:"))
			}
		case "-f", "-F":
			typed := args[i] == "-F"
			i++
			if i < len(args) {
				addField(fields, args[i], typed)
			}
		case "--jq":
			i++
		default:
			if !strings.HasPrefix(args[i], "-") && endpoint == "" {
				endpoint = args[i]
			}
		}
	}
	if endpoint == "" {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("GitHub REST endpoint is required")}
	}
	if method == http.MethodGet && len(fields) != 0 {
		endpoint = addQuery(endpoint, fields)
		fields = nil
	}
	if !paginate {
		raw, _, _, err := c.request(ctx, method, endpoint, accept, fields)
		return raw, err
	}
	var pages []json.RawMessage
	next := endpoint
	for next != "" {
		raw, headers, _, err := c.request(ctx, method, next, accept, fields)
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, forgeResponseError("parse paginated GitHub REST response")
		}
		pages = append(pages, append(json.RawMessage(nil), raw...))
		next = nextLink(headers.Get("Link"))
		if next != "" && maxPages > 0 && len(pages) >= maxPages {
			return nil, forgeResponseError("GitHub REST response has more than %d pages", maxPages)
		}
	}
	if slurp {
		return json.Marshal(pages)
	}
	return bytes.Join(rawPages(pages), nil), nil
}

func rawPages(pages []json.RawMessage) [][]byte {
	result := make([][]byte, len(pages))
	for i := range pages {
		result[i] = pages[i]
	}
	return result
}

func addField(fields map[string]any, field string, typed bool) {
	key, value, ok := strings.Cut(field, "=")
	if !ok {
		return
	}
	if typed {
		if number, err := strconv.Atoi(value); err == nil {
			fields[key] = number
			return
		}
	}
	fields[key] = value
}

func addQuery(endpoint string, fields map[string]any) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	for key, value := range fields {
		query.Set(key, fmt.Sprint(value))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func nextLink(header string) string {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		if len(parts) == 2 && strings.TrimSpace(parts[1]) == `rel="next"` {
			return strings.Trim(strings.TrimSpace(parts[0]), "<>")
		}
	}
	return ""
}

// graphqlRetryEnvelope is the flag with which a caller asks the GraphQL
// call for GitHub's retry signals beside the body. Only the line read
// passes it. Every other caller gets the body alone, as before.
const graphqlRetryEnvelope = "--retry-envelope"

// graphqlEnvelope is the answer of a GraphQL call made with
// graphqlRetryEnvelope: the body as GitHub sent it, and the signals of the
// same HTTP response, taken before its body was read. GitHub answers a
// GraphQL rate limit with HTTP 200, so these headers are the only place its
// retry time appears. Each of the three retry signals is read on its own
// (retryAfterSignal, resetSignal, noRequestLeftSignal) into a usable time,
// or no time, which is the zero time; one signal that cannot be used never
// changes what another one gives. No usable time lies past the year 9999,
// so the envelope always encodes. The caller
// decides from these and the body which time and category apply.
type graphqlEnvelope struct {
	Body []byte `json:"body"`
	responseSignals
	RetryAfter    time.Time `json:"retry_after"`
	Reset         time.Time `json:"reset"`
	RemainingZero bool      `json:"remaining_zero"`
}

// lineEnvelope builds the envelope of one response from its signals. A 403
// or 422 that states no request is left is a rate limit for the line read
// even when statusCategory, which every caller shares, reads the header
// more narrowly, such as "00": the line read never treats a stated limit as
// something else.
func lineEnvelope(raw []byte, signals responseSignals) graphqlEnvelope {
	envelope := graphqlEnvelope{
		Body:            raw,
		responseSignals: signals,
		RetryAfter:      retryAfterSignal(signals.header, signals.at),
		Reset:           resetSignal(signals.header),
		RemainingZero:   noRequestLeftSignal(signals.header),
	}
	if envelope.RemainingZero && (signals.Status == http.StatusForbidden || signals.Status == http.StatusUnprocessableEntity) {
		envelope.Category = ForgeRateLimited
	}
	return envelope
}

// maxRetryAfterSeconds is the longest Retry-After delay that a Go duration
// holds, about 292 years. A longer delay is no usable time.
const maxRetryAfterSeconds = uint64(1<<63-1) / uint64(time.Second)

// maxResetSeconds is the last second of the year 9999 in Unix seconds, the
// latest time JSON carries. A later reset is no usable time.
const maxResetSeconds = 253402300799

// headerDigits reads a header value of decimal digits only, the form
// GitHub and HTTP use for these headers, after HTTP has removed the
// optional whitespace around it. A value with a sign, a decimal point, a
// space inside, or any other character is not a number. ok is false for
// such a value and when the number does not fit in 64 bits.
func headerDigits(value string) (n uint64, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	return n, err == nil
}

// retryAfterSignal is the usable time Retry-After states, or the zero time.
// A value is either a delay in whole seconds after now, up to
// maxRetryAfterSeconds, or an HTTP date in any of the three forms HTTP
// allows (IMF-fixdate, RFC 850, asctime). Zero seconds or a past date is a
// usable time that asks for no wait. When the header is sent more than
// once, the latest usable time of its values is kept, so that no request is
// sent before any time GitHub stated; an unusable value adds nothing.
func retryAfterSignal(header http.Header, now time.Time) time.Time {
	var at time.Time
	for _, value := range header.Values("Retry-After") {
		value = strings.TrimSpace(value)
		var usable time.Time
		if seconds, ok := headerDigits(value); ok {
			if seconds <= maxRetryAfterSeconds {
				usable = now.Add(time.Duration(seconds) * time.Second).UTC()
			}
		} else if date, err := http.ParseTime(value); err == nil && date.Year() <= 9999 {
			usable = date.UTC()
		}
		at = laterTime(at, usable)
	}
	return at
}

// resetSignal is the usable time X-RateLimit-Reset states in Unix seconds,
// up to maxResetSeconds, or the zero time. Zero is the Unix epoch, and it
// and any past time are usable times that ask for no wait. When the header
// is sent more than once, the latest usable time is kept.
func resetSignal(header http.Header) time.Time {
	var at time.Time
	for _, value := range header.Values("X-RateLimit-Reset") {
		if seconds, ok := headerDigits(value); ok && seconds <= maxResetSeconds {
			at = laterTime(at, time.Unix(int64(seconds), 0).UTC())
		}
	}
	return at
}

// noRequestLeftSignal reports whether X-RateLimit-Remaining states that no
// request is left: a value of digits only whose number is 0. Any other
// value, a positive count or one that is not a number, states nothing about
// a wait. When the header is sent more than once, one value of 0 is enough.
func noRequestLeftSignal(header http.Header) bool {
	for _, value := range header.Values("X-RateLimit-Remaining") {
		if n, ok := headerDigits(value); ok && n == 0 {
			return true
		}
	}
	return false
}

func (c *restClient) graphql(ctx context.Context, args ...string) ([]byte, error) {
	query := ""
	variables := map[string]any{}
	envelope := false
	for i := 0; i < len(args); i++ {
		if args[i] == graphqlRetryEnvelope {
			envelope = true
			continue
		}
		if (args[i] == "-f" || args[i] == "-F") && i+1 < len(args) {
			typed := args[i] == "-F"
			i++
			key, value, ok := strings.Cut(args[i], "=")
			if !ok {
				continue
			}
			if key == "query" {
				query = value
			} else {
				addField(variables, key+"="+value, typed)
			}
		}
	}
	request := map[string]any{"query": query, "variables": variables}
	if !envelope {
		raw, _, _, err := c.request(ctx, http.MethodPost, "graphql", "application/vnd.github+json", request)
		return raw, err
	}
	// With the flag, the envelope comes back whenever GitHub answered, also
	// beside the error of a refused status or of a body that could not be
	// read, so the line read reads the status and retry signals of every
	// response. No answer, such as a transport failure or a timeout, has no
	// signals.
	raw, signals, err := c.exchange(ctx, http.MethodPost, "graphql", "application/vnd.github+json", request)
	if !signals.Received {
		return nil, err
	}
	encoded, encodeErr := json.Marshal(lineEnvelope(raw, signals))
	if encodeErr != nil {
		return nil, &Error{Category: ForgeResponse, Err: fmt.Errorf("encode GitHub GraphQL answer: %w", encodeErr)}
	}
	return encoded, err
}

func option(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func (c *restClient) issue(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("GitHub issue operation is required")}
	}
	repo := option(args, "--repo")
	switch args[0] {
	case "list":
		endpoint := fmt.Sprintf("repos/%s/issues?state=%s&labels=%s&per_page=100", repo, url.QueryEscape(option(args, "--state")), url.QueryEscape(option(args, "--label")))
		var issues []json.RawMessage
		for endpoint != "" {
			raw, headers, _, err := c.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
			if err != nil {
				return nil, err
			}
			var page []json.RawMessage
			if json.Unmarshal(raw, &page) != nil {
				return nil, forgeResponseError("parse GitHub issue listing")
			}
			issues = append(issues, page...)
			endpoint = nextLink(headers.Get("Link"))
		}
		raw, err := json.Marshal(issues)
		if err != nil {
			return nil, forgeResponseError("encode GitHub issue listing")
		}
		return normalizeIssues(raw)
	case "create":
		raw, _, _, err := c.request(ctx, http.MethodPost, "repos/"+repo+"/issues", "application/vnd.github+json", map[string]any{"title": option(args, "--title"), "body": option(args, "--body")})
		if err != nil {
			return nil, err
		}
		return jsonStringField(raw, "html_url")
	case "view":
		raw, _, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/%s", repo, args[1]), "application/vnd.github+json", nil)
		if err != nil {
			return nil, err
		}
		return normalizeIssue(raw)
	case "edit":
		body := map[string]any{}
		if value := option(args, "--body"); value != "" {
			body["body"] = value
		}
		if remove, add := option(args, "--remove-label"), option(args, "--add-label"); remove != "" || add != "" {
			raw, _, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/%s", repo, args[1]), "application/vnd.github+json", nil)
			if err != nil {
				return nil, err
			}
			var current struct {
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			}
			if json.Unmarshal(raw, &current) != nil {
				return nil, forgeResponseError("parse issue labels")
			}
			labels := []string{}
			for _, label := range current.Labels {
				if label.Name != remove {
					labels = append(labels, label.Name)
				}
			}
			if add != "" {
				labels = append(labels, add)
			}
			body["labels"] = labels
		}
		raw, _, _, err := c.request(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%s", repo, args[1]), "application/vnd.github+json", body)
		return raw, err
	case "comment":
		raw, _, _, err := c.request(ctx, http.MethodPost, fmt.Sprintf("repos/%s/issues/%s/comments", repo, args[1]), "application/vnd.github+json", map[string]any{"body": option(args, "--body")})
		return raw, err
	default:
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("unsupported GitHub issue operation %q", args[0])}
	}
}

func (c *restClient) label(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) < 2 || args[0] != "create" {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("unsupported GitHub label operation")}
	}
	repo, name := option(args, "--repo"), args[1]
	body := map[string]any{"name": name, "color": option(args, "--color"), "description": option(args, "--description")}
	raw, _, status, err := c.request(ctx, http.MethodPost, "repos/"+repo+"/labels", "application/vnd.github+json", body)
	if err == nil {
		return raw, nil
	}
	if status != http.StatusUnprocessableEntity {
		return nil, err
	}
	delete(body, "name")
	raw, _, _, err = c.request(ctx, http.MethodPatch, "repos/"+repo+"/labels/"+url.PathEscape(name), "application/vnd.github+json", body)
	return raw, err
}

func (c *restClient) pullRequest(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, &Error{Category: ForgeRequest, Err: errors.New("GitHub pull request operation is required")}
	}
	repo := option(args, "--repo")
	switch args[0] {
	case "list":
		owner, _, _ := strings.Cut(repo, "/")
		endpoint := fmt.Sprintf("repos/%s/pulls?head=%s&state=%s", repo, url.QueryEscape(owner+":"+option(args, "--head")), option(args, "--state"))
		raw, _, _, err := c.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
		if err != nil {
			return nil, err
		}
		var pulls []struct {
			URL string `json:"html_url"`
		}
		if json.Unmarshal(raw, &pulls) != nil {
			return nil, forgeResponseError("parse pull request listing")
		}
		if len(pulls) == 0 {
			return []byte{}, nil
		}
		return []byte(pulls[0].URL), nil
	case "view":
		raw, err := c.pullForBranch(ctx, repo, args[1])
		if err != nil {
			return nil, err
		}
		if option(args, "--jq") == ".body" {
			return jsonStringField(raw, "body")
		}
		return normalizePull(raw)
	case "create":
		raw, _, _, err := c.request(ctx, http.MethodPost, "repos/"+repo+"/pulls", "application/vnd.github+json", map[string]any{"head": option(args, "--head"), "base": option(args, "--base"), "title": option(args, "--title"), "body": option(args, "--body")})
		if err != nil {
			return nil, err
		}
		return jsonStringField(raw, "html_url")
	case "edit":
		raw, err := c.pullForBranch(ctx, repo, args[1])
		if err != nil {
			return nil, err
		}
		number, err := jsonIntField(raw, "number")
		if err != nil {
			return nil, err
		}
		raw, _, _, err = c.request(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repo, number), "application/vnd.github+json", map[string]any{"body": option(args, "--body")})
		return raw, err
	case "merge":
		raw, _, _, err := c.request(ctx, http.MethodPut, fmt.Sprintf("repos/%s/pulls/%s/merge", repo, args[1]), "application/vnd.github+json", mergePayload(ctx))
		return raw, err
	case "diff":
		raw, err := c.pullForBranch(ctx, repo, args[1])
		if err != nil {
			return nil, err
		}
		number, err := jsonIntField(raw, "number")
		if err != nil {
			return nil, err
		}
		raw, _, _, err = c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo, number), "application/vnd.github.v3.diff", nil)
		return raw, err
	default:
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("unsupported GitHub pull request operation %q", args[0])}
	}
}

func (c *restClient) pullForBranch(ctx context.Context, repo, branch string) ([]byte, error) {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("invalid GitHub repository %q", repo)}
	}
	raw, _, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls?head=%s&state=all&per_page=1", repo, url.QueryEscape(owner+":"+branch)), "application/vnd.github+json", nil)
	if err != nil {
		return nil, err
	}
	var pulls []json.RawMessage
	if json.Unmarshal(raw, &pulls) != nil {
		return nil, forgeResponseError("parse pull request for branch %s", branch)
	}
	if len(pulls) == 0 {
		return nil, &Error{Category: ForgeStatus, Err: fmt.Errorf("%w for branch %s", ErrPullRequestNotFound, branch)}
	}
	var summary struct {
		Number int `json:"number"`
	}
	if json.Unmarshal(pulls[0], &summary) != nil || summary.Number == 0 {
		return nil, forgeResponseError("parse pull request for branch %s", branch)
	}
	return c.pullForNumber(ctx, repo, summary.Number)
}

func (c *restClient) pullForNumber(ctx context.Context, repo string, number int) ([]byte, error) {
	if number <= 0 || strings.Count(repo, "/") != 1 {
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("invalid GitHub repository or pull request")}
	}
	raw, _, status, err := c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo, number), "application/vnd.github+json", nil)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, &Error{Category: ForgeStatus, Err: fmt.Errorf("%w for pull request %d", ErrPullRequestNotFound, number)}
		}
		return nil, err
	}
	return raw, nil
}

func mergePayload(ctx context.Context) map[string]any {
	payload := map[string]any{"merge_method": "merge"}
	if message, ok := ctx.Value(mergeMessageKey{}).(string); ok && message != "" {
		payload["commit_message"] = message
	}
	if head, ok := ctx.Value(expectedHeadKey{}).(string); ok && head != "" {
		payload["sha"] = head
	}
	return payload
}

func normalizePull(raw []byte) ([]byte, error) {
	var pull struct {
		Number   int    `json:"number"`
		URL      string `json:"html_url"`
		State    string `json:"state"`
		MergedAt any    `json:"merged_at"`
		MergedBy struct {
			Login string `json:"login"`
		} `json:"merged_by"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
		Body           string `json:"body"`
		Head           struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	}
	if json.Unmarshal(raw, &pull) != nil {
		return nil, forgeResponseError("parse GitHub pull request response")
	}
	mergeable := "UNKNOWN"
	if pull.Mergeable != nil {
		if *pull.Mergeable {
			mergeable = "MERGEABLE"
		} else {
			mergeable = "CONFLICTING"
		}
	}
	return json.Marshal(map[string]any{"number": pull.Number, "url": pull.URL, "state": strings.ToUpper(pull.State), "mergedAt": pull.MergedAt, "mergedBy": strings.TrimSpace(pull.MergedBy.Login), "mergeCommit": strings.TrimSpace(pull.MergeCommitSHA), "mergeable": mergeable, "headRefOid": pull.Head.SHA, "headRefName": pull.Head.Ref, "baseRefOid": pull.Base.SHA, "body": pull.Body})
}

func normalizeIssues(raw []byte) ([]byte, error) {
	var issues []json.RawMessage
	if json.Unmarshal(raw, &issues) != nil {
		return nil, forgeResponseError("parse GitHub issue listing")
	}
	normalized := make([]json.RawMessage, 0, len(issues))
	for _, issue := range issues {
		value, err := normalizeIssue(issue)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, value)
	}
	return json.Marshal(normalized)
}

func normalizeIssue(raw []byte) ([]byte, error) {
	var issue struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"html_url"`
	}
	if json.Unmarshal(raw, &issue) != nil || issue.Number == 0 {
		return nil, forgeResponseError("parse GitHub issue response")
	}
	return json.Marshal(Issue{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL})
}

func jsonStringField(raw []byte, key string) ([]byte, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil, forgeResponseError("parse GitHub REST response")
	}
	var result string
	if json.Unmarshal(value[key], &result) != nil || result == "" {
		return nil, forgeResponseError("parse GitHub REST response field %s", key)
	}
	return []byte(result), nil
}

func jsonIntField(raw []byte, key string) (int, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return 0, forgeResponseError("parse GitHub REST response")
	}
	var result int
	if json.Unmarshal(value[key], &result) != nil || result == 0 {
		return 0, forgeResponseError("parse GitHub REST response field %s", key)
	}
	return result, nil
}
