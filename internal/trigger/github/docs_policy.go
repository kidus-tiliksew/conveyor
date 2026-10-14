package github

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
)

type DocumentationPolicyDiscovery struct {
	Revision string
	State    string // present | absent | malformed | unavailable (known declaration) | unknown
	Bytes    []byte
	Policy   *docsconfig.Config
}

// DiscoverDocumentationPolicy checks only the root and declaration directory at the PR base SHA (DEC-71).
// An unreadable repository does not establish an opt-in; a located, unreadable declaration does.
func (c *AppClient) DiscoverDocumentationPolicy(ctx context.Context, st AppStore, workspace, slug, revision string) DocumentationPolicyDiscovery {
	d := DocumentationPolicyDiscovery{Revision: revision, State: "unknown"}
	sha, err := hex.DecodeString(revision)
	if err != nil || len(sha) != 20 || strings.ToLower(revision) != revision {
		return d
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(slug, "?#%\\") {
		return d
	}
	token, err := c.WorkspaceToken(ctx, st, workspace)
	if err != nil {
		return d
	}
	if err = c.RequireRepository(ctx, workspace, token, slug); err != nil {
		return d
	}
	client, base := c.HTTP, c.BaseURL
	if client == nil {
		client = http.DefaultClient
	}
	if base == "" {
		base = "https://api.github.com"
	}
	r := &restClient{http: client, baseURL: strings.TrimRight(base, "/"), token: token, identity: AppIdentity(workspace)}
	get := func(endpoint string, target any) bool {
		for attempt := range 3 {
			raw, _, status, err := r.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
			if err == nil {
				return json.Unmarshal(raw, target) == nil
			}
			if attempt == 2 || status != 0 && status != http.StatusTooManyRequests && status < 500 {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
		return false
	}
	var commit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if !get("/repos/"+slug+"/git/commits/"+revision, &commit) || commit.SHA != revision || commit.Tree.SHA == "" {
		return d
	}
	type treeEntry struct{ Path, Mode, Type, SHA string }
	type gitTree struct {
		Truncated bool        `json:"truncated"`
		SHA       string      `json:"sha"`
		Entries   []treeEntry `json:"tree"`
	}
	var tree gitTree
	if !get("/repos/"+slug+"/git/trees/"+commit.Tree.SHA, &tree) || tree.SHA != commit.Tree.SHA || tree.Entries == nil {
		return d
	}
	directorySHA := ""
	for _, entry := range tree.Entries {
		if entry.Path == ".conveyor" && entry.Type == "tree" {
			directorySHA = entry.SHA
			break
		}
	}
	if directorySHA == "" {
		if !tree.Truncated {
			d.State = "absent"
		}
		return d
	}
	var directory gitTree
	if !get("/repos/"+slug+"/git/trees/"+directorySHA, &directory) || directory.SHA != directorySHA || directory.Entries == nil {
		return d
	}
	blobOID, mode, kind := "", "", ""
	for _, entry := range directory.Entries {
		if entry.Path == "docs.yaml" {
			blobOID, mode, kind = entry.SHA, entry.Mode, entry.Type
			break
		}
	}
	if blobOID == "" {
		if !directory.Truncated {
			d.State = "absent"
		}
		return d
	}
	d.State = "unavailable"
	if kind != "blob" || mode != "100644" && mode != "100755" {
		d.State = "malformed"
		return d
	}
	var blob struct {
		Encoding, Content string
		Size              int
	}
	if !get("/repos/"+slug+"/git/blobs/"+blobOID, &blob) {
		return d
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil || blob.Encoding != "base64" || len(data) != blob.Size || len(data) > docsconfig.MaxFileBytes {
		d.State = "malformed"
		return d
	}
	d.Bytes = data
	policy, err := docsconfig.Parse(data)
	if err != nil {
		d.State = "malformed"
		return d
	}
	d.Policy, d.State = policy, "present"
	return d
}

// AgentAuthoredPullRequestBody excludes generated lifecycle and verification regions.
func AgentAuthoredPullRequestBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body, _ = preserveVerificationRegions(body)
	if start := strings.Index(body, pullRequestLifecycleMarker); start >= 0 {
		end := legacyPullRequestLifecycleEnd(body[start:]) + start
		body = body[:start] + body[end:]
	}
	return strings.TrimSpace(body)
}
