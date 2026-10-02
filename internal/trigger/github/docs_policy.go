package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// DocumentationPolicyDiscovery contains only exact-revision observations of a
// repository's .conveyor/docs.yaml. Repository content grants no authority.
type DocumentationPolicyDiscovery struct {
	Revision string
	State    string // present | absent | malformed | permission | transport | unknown_revision
	Bytes    []byte
	Hash     string
	Policy   *docsconfig.Config
}

// DiscoverDocumentationPolicy reads .conveyor/docs.yaml at the exact revision
// through the workspace GitHub App using the same contents path as
// DiscoverVerification, capped at verification.MaxManifestBytes.
func (c *AppClient) DiscoverDocumentationPolicy(ctx context.Context, st AppStore, workspace, slug, revision string) DocumentationPolicyDiscovery {
	d := DocumentationPolicyDiscovery{Revision: revision, State: "unavailable"}
	sha, err := hex.DecodeString(revision)
	if err != nil || (len(sha) != 20 && len(sha) != 32) || strings.ToLower(revision) != revision {
		d.State = "unknown_revision"
		return d
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(slug, "?#%\\") {
		return d
	}
	token, err := c.WorkspaceToken(ctx, st, workspace)
	if err != nil {
		d.State = "transport"
		if ErrorCategory(err) == ForgePermission {
			d.State = "permission"
		}
		return d
	}
	if err = c.RequireRepository(ctx, workspace, token, slug); err != nil {
		d.State = "transport"
		if ErrorCategory(err) == ForgePermission {
			d.State = "permission"
		}
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
		raw, _, status, reqErr := r.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
		if reqErr != nil {
			d.State = "transport"
			if status == 401 || status == 403 {
				d.State = "permission"
			}
			if status == 404 || status == 422 {
				d.State = "unknown_revision"
			}
			return false
		}
		if json.Unmarshal(raw, target) != nil {
			d.State = "malformed"
			return false
		}
		return true
	}
	var commit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if !get("/repos/"+slug+"/git/commits/"+revision, &commit) {
		return d
	}
	if commit.SHA != revision || commit.Tree.SHA == "" {
		d.State = "unknown_revision"
		return d
	}
	var tree struct {
		Truncated bool                                     `json:"truncated"`
		SHA       string                                   `json:"sha"`
		Entries   []struct{ Path, Mode, Type, SHA string } `json:"tree"`
	}
	if !get("/repos/"+slug+"/git/trees/"+commit.Tree.SHA+"?recursive=1", &tree) {
		return d
	}
	if tree.Truncated {
		d.State = "truncated"
		return d
	}
	if tree.SHA != commit.Tree.SHA || tree.Entries == nil {
		d.State = "malformed"
		return d
	}
	blobOID, mode := "", ""
	for _, e := range tree.Entries {
		if e.Path != docsconfig.FilePath {
			continue
		}
		blobOID, mode = e.SHA, e.Mode
		break
	}
	if blobOID == "" {
		d.State = "absent"
		return d
	}
	if mode != "100644" && mode != "100755" {
		d.State = "malformed"
		return d
	}
	var blob struct {
		Encoding, Content string
		Size              int
	}
	if !get("/repos/"+slug+"/git/blobs/"+blobOID, &blob) {
		if d.State == "unknown_revision" {
			d.State = "unavailable"
		}
		return d
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil || blob.Encoding != "base64" || len(data) != blob.Size || len(data) > verification.MaxManifestBytes {
		d.State = "truncated"
		return d
	}
	d.Bytes = data
	d.Hash = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	policy, err := docsconfig.Parse(data)
	if err != nil {
		d.State = "malformed"
		return d
	}
	d.Policy = policy
	d.State = "present"
	return d
}

// AgentAuthoredPullRequestBody returns the portions of a pull request body
// outside Conveyor's generated lifecycle region and typed verification
// regions. The docs-none matcher scans only this text.
func AgentAuthoredPullRequestBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body, _ = preserveVerificationRegions(body)
	if start := strings.Index(body, pullRequestLifecycleMarker); start >= 0 {
		end := legacyPullRequestLifecycleEnd(body[start:]) + start
		body = body[:start] + body[end:]
	}
	return strings.TrimSpace(body)
}
