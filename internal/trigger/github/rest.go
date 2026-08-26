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
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, 0, &Error{Category: ForgeRequest, Err: fmt.Errorf("encode GitHub REST request: %w", err)}
		}
		payload = bytes.NewReader(encoded)
	}
	requestURL := endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		requestURL = c.baseURL + "/" + strings.TrimLeft(endpoint, "/")
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, payload)
	if err != nil {
		return nil, nil, 0, &Error{Category: ForgeRequest, Err: fmt.Errorf("build GitHub REST request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, &Error{Category: ForgeRequest, Err: fmt.Errorf("GitHub REST transport: %w", err)}
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return nil, response.Header, response.StatusCode, &Error{Category: ForgeResponse, Err: fmt.Errorf("read GitHub REST response: %w", readErr)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		category := ForgeStatus
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "") {
			category = ForgeRateLimited
		} else if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			category = ForgePermission
		}
		if category == ForgePermission {
			return nil, response.Header, response.StatusCode, &Error{Category: category, Err: fmt.Errorf("%s is expired, revoked, or lacks permission; replace it in settings (GitHub HTTP %d)", c.identityLabel(), response.StatusCode)}
		}
		return nil, response.Header, response.StatusCode, &Error{Category: category, Err: fmt.Errorf("GitHub REST status %d: %s", response.StatusCode, secretSafeMessage(raw))}
	}
	return raw, response.Header, response.StatusCode, nil
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
	for i := 0; i < len(args); i++ {
		switch args[i] {
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

func (c *restClient) graphql(ctx context.Context, args ...string) ([]byte, error) {
	query := ""
	variables := map[string]any{}
	for i := 0; i < len(args); i++ {
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
	raw, _, _, err := c.request(ctx, http.MethodPost, "graphql", "application/vnd.github+json", map[string]any{"query": query, "variables": variables})
	return raw, err
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
		raw, _, _, err := c.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
		if err != nil {
			return nil, err
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
		raw, _, _, err := c.request(ctx, http.MethodPut, fmt.Sprintf("repos/%s/pulls/%s/merge", repo, args[1]), "application/vnd.github+json", map[string]any{"merge_method": "merge"})
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
	raw, _, _, err = c.request(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo, summary.Number), "application/vnd.github+json", nil)
	return raw, err
}

func normalizePull(raw []byte) ([]byte, error) {
	var pull struct {
		Number         int    `json:"number"`
		URL            string `json:"html_url"`
		State          string `json:"state"`
		MergedAt       any    `json:"merged_at"`
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
		Body           string `json:"body"`
		Head           struct {
			SHA string `json:"sha"`
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
	return json.Marshal(map[string]any{"number": pull.Number, "url": pull.URL, "state": strings.ToUpper(pull.State), "mergedAt": pull.MergedAt, "mergeable": mergeable, "headRefOid": pull.Head.SHA, "baseRefOid": pull.Base.SHA, "body": pull.Body})
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
