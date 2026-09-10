package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SnapshotClient uses only the request's workspace installation credential.
// DEC-32, DEC-41; req-delivery-and-forge AC-1.4 and AC-2.3.
type SnapshotClient struct {
	HTTP    *http.Client
	BaseURL string
}

func NewSnapshotClient(client *http.Client, base string) *SnapshotClient {
	if client == nil {
		client = http.DefaultClient
	}
	if base == "" {
		base = githubAPIBaseURL
	}
	return &SnapshotClient{HTTP: client, BaseURL: strings.TrimRight(base, "/")}
}

var snapshotSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func ValidSnapshotSHA(sha string) bool { return snapshotSHA.MatchString(sha) }

type SnapshotCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
		Committer struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Files []SnapshotFile `json:"files"`
}
type SnapshotFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        *int64 `json:"additions"`
	Deletions        *int64 `json:"deletions"`
	Changes          *int64 `json:"changes"`
}

func (c *SnapshotClient) request(ctx context.Context, slug, endpoint string, archive bool) (*http.Response, error) {
	credential, ok := credentialFromContext(ctx)
	if !ok {
		return nil, PermissionError(fmt.Errorf("repository %s requires a workspace GitHub App; connect it in workspace settings", slug))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("invalid snapshot request")}
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := *c.HTTP
	base, _ := url.Parse(c.BaseURL)
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !archive || len(via) > 3 {
			return fmt.Errorf("snapshot redirect limit or unexpected redirect")
		}
		previous := via[len(via)-1]
		if next.Response == nil || (next.Response.StatusCode != 301 && next.Response.StatusCode != 302 && next.Response.StatusCode != 303 && next.Response.StatusCode != 307 && next.Response.StatusCode != 308) {
			return fmt.Errorf("unsupported snapshot redirect status")
		}
		sameOrigin := next.URL.Scheme == base.Scheme && next.URL.Host == base.Host
		githubDownload := next.URL.Scheme == "https" && next.URL.Host == "codeload.github.com"
		if next.URL.User != nil || next.URL.Fragment != "" || (!sameOrigin && !githubDownload) {
			return fmt.Errorf("unsafe snapshot redirect")
		}
		// Once the request leaves the API origin it must never carry the token.
		next.Header.Del("Authorization")
		if sameOrigin && previous.URL.Scheme == base.Scheme && previous.URL.Host == base.Host && previous.Header.Get("Authorization") != "" {
			next.Header.Set("Authorization", "Bearer "+credential.Token)
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, &Error{Category: ForgeRequest, Err: fmt.Errorf("repository %s snapshot request or redirect failed", slug)}
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		category := ForgeStatus
		if response.StatusCode == 429 || response.StatusCode == 403 && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "") {
			category = ForgeRateLimited
		} else if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
			category = ForgePermission
		}
		if category == ForgePermission {
			return nil, PermissionError(fmt.Errorf("repository %s is unavailable to the workspace GitHub App; check installation coverage in workspace settings (HTTP %d)", slug, response.StatusCode))
		}
		return nil, &Error{Category: category, Err: fmt.Errorf("repository %s snapshot HTTP %d", slug, response.StatusCode)}
	}
	return response, nil
}
func (c *SnapshotClient) json(ctx context.Context, slug, endpoint string, out any) (http.Header, int, error) {
	response, err := c.request(ctx, slug, endpoint, false)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, len(raw), forgeResponseError("snapshot response read failed; completeness unavailable")
	}
	if len(raw) > 1<<20 {
		return nil, len(raw), forgeResponseError("snapshot completeness limit: response exceeds 1 MiB")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return nil, len(raw), forgeResponseError("malformed snapshot response; completeness unavailable")
	}
	return response.Header, len(raw), nil
}
func (c *SnapshotClient) ResolveCommit(ctx context.Context, slug, base string) (string, error) {
	var commit SnapshotCommit
	_, _, err := c.json(ctx, slug, c.BaseURL+"/repos/"+slug+"/commits/"+url.PathEscape(base), &commit)
	if err != nil {
		return "", err
	}
	if !ValidSnapshotSHA(commit.SHA) {
		return "", forgeResponseError("snapshot pin is not a full commit SHA")
	}
	return commit.SHA, nil
}
func (c *SnapshotClient) OpenArchive(ctx context.Context, slug, sha string) (io.ReadCloser, error) {
	if !ValidSnapshotSHA(sha) {
		return nil, forgeResponseError("snapshot revision is not a full commit SHA")
	}
	response, err := c.request(ctx, slug, c.BaseURL+"/repos/"+slug+"/tarball/"+sha, true)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}
func (c *SnapshotClient) History(ctx context.Context, slug, sha, path string, n int) ([]SnapshotCommit, error) {
	if !ValidSnapshotSHA(sha) || n < 1 || n > 50 {
		return nil, forgeResponseError("invalid bounded snapshot history request")
	}
	endpoint := c.BaseURL + "/repos/" + slug + "/commits?" + url.Values{"sha": {sha}, "path": {path}, "per_page": {strconv.Itoa(n)}, "page": {"1"}}.Encode()
	var commits []SnapshotCommit
	_, _, err := c.json(ctx, slug, endpoint, &commits)
	if err != nil {
		return nil, err
	}
	if commits == nil || len(commits) > n {
		return nil, forgeResponseError("invalid snapshot history page")
	}
	seen := map[string]bool{}
	for _, commit := range commits {
		if !ValidSnapshotSHA(commit.SHA) || seen[commit.SHA] {
			return nil, forgeResponseError("malformed or duplicate snapshot history commit")
		}
		seen[commit.SHA] = true
	}
	return commits, nil
}

// CommitDetail acquires the complete file set for ONE selected commit before
// filtering. Exact 3000-file responses are ambiguous even without a next link.
func (c *SnapshotClient) CommitDetail(ctx context.Context, slug, sha string) (SnapshotCommit, error) {
	if !ValidSnapshotSHA(sha) {
		return SnapshotCommit{}, forgeResponseError("invalid history detail SHA")
	}
	resource := c.BaseURL + "/repos/" + slug + "/commits/" + sha
	endpoint := resource + "?per_page=100&page=1"
	var result SnapshotCommit
	seenPages := map[string]bool{}
	seenFiles := map[string]bool{}
	total := 0
	for page := 1; page <= 30; page++ {
		if seenPages[endpoint] {
			return result, forgeResponseError("history detail completeness: pagination cycle")
		}
		seenPages[endpoint] = true
		var part SnapshotCommit
		headers, size, err := c.json(ctx, slug, endpoint, &part)
		if err != nil {
			return result, fmt.Errorf("history detail completeness failed: %w", err)
		}
		total += size
		if total > 8<<20 {
			return result, forgeResponseError("history detail completeness limit: response exceeds 8 MiB")
		}
		if part.SHA != sha || part.Files == nil || len(part.Files) > 100 {
			return result, forgeResponseError("history detail completeness: inconsistent SHA or malformed files")
		}
		if page == 1 {
			result = part
			result.Files = nil
		}
		for _, file := range part.Files {
			if !validSnapshotFilename(file.Filename) || seenFiles[file.Filename] || file.Additions == nil || file.Deletions == nil || file.Changes == nil || *file.Additions < 0 || *file.Deletions < 0 || *file.Changes < 0 {
				return result, forgeResponseError("history detail completeness: malformed or duplicate file")
			}
			switch file.Status {
			case "added", "removed", "modified", "renamed", "copied", "changed", "unchanged":
			default:
				return result, forgeResponseError("history detail completeness: malformed file status")
			}
			if file.PreviousFilename != "" && !validSnapshotFilename(file.PreviousFilename) || file.Status == "renamed" && file.PreviousFilename == "" {
				return result, forgeResponseError("history detail completeness: malformed previous filename")
			}
			seenFiles[file.Filename] = true
			result.Files = append(result.Files, file)
		}
		if len(result.Files) >= 3000 {
			return result, forgeResponseError("history detail completeness ambiguous at GitHub's 3000-file limit")
		}
		next, err := snapshotNextLink(headers.Values("Link"), resource, page)
		if err != nil {
			return result, err
		}
		if next == "" {
			return result, nil
		}
		endpoint = next
	}
	return result, forgeResponseError("history detail completeness limit: next link remains after 30 pages")
}
func validSnapshotFilename(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}
func snapshotNextLink(headers []string, resource string, page int) (string, error) {
	next := ""
	base, _ := url.Parse(resource)
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			parts := strings.Split(strings.TrimSpace(item), ";")
			if len(parts) < 2 || !strings.HasPrefix(parts[0], "<") || !strings.HasSuffix(parts[0], ">") {
				return "", forgeResponseError("history detail completeness: malformed pagination")
			}
			relation := ""
			for _, p := range parts[1:] {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "rel=") {
					relation = strings.Trim(strings.TrimPrefix(p, "rel="), "\"")
				}
			}
			if relation == "" {
				return "", forgeResponseError("history detail completeness: missing pagination relation")
			}
			if relation != "next" {
				continue
			}
			if next != "" {
				return "", forgeResponseError("history detail completeness: duplicate next link")
			}
			u, err := url.Parse(strings.TrimSuffix(strings.TrimPrefix(parts[0], "<"), ">"))
			if err != nil {
				return "", forgeResponseError("history detail completeness: malformed next URL")
			}
			u = base.ResolveReference(u)
			q, err := url.ParseQuery(u.RawQuery)
			if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.Path != base.Path || u.User != nil || u.Fragment != "" || len(q) != 2 || len(q["per_page"]) != 1 || q.Get("per_page") != "100" || len(q["page"]) != 1 || q.Get("page") != strconv.Itoa(page+1) {
				return "", forgeResponseError("history detail completeness: unsafe, cyclic, or inconsistent pagination")
			}
			u.RawQuery = q.Encode()
			next = u.String()
		}
	}
	return next, nil
}
