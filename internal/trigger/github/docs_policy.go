package github

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	found := c.readConveyorFile(ctx, st, workspace, slug, revision, "docs.yaml", docsconfig.MaxFileBytes)
	d.State = found.state
	if found.state != "present" {
		return d
	}
	d.Bytes = found.data
	policy, err := docsconfig.Parse(found.data)
	if err != nil {
		d.State = "malformed"
		return d
	}
	d.Policy = policy
	return d
}

// conveyorFile is what readConveyorFile found: state is present, absent,
// malformed, unavailable (a located file that could not be read), or unknown
// (the revision or the repository could not be read). reason describes every
// state but present and absent without naming the file's contents.
type conveyorFile struct {
	state  string
	data   []byte
	reason string
}

// readConveyorFile reads .conveyor/<name> at revision with the workspace App
// token. It reads only the commit, the root tree, the .conveyor tree, and the
// file's blob, so a declaration is always read at the named commit and never
// at a branch head (DEC-71; feature-risk-based-review RR-2).
func (c *AppClient) readConveyorFile(ctx context.Context, st AppStore, workspace, slug, revision, name string, maxBytes int) conveyorFile {
	unknown := func(reason string) conveyorFile { return conveyorFile{state: "unknown", reason: reason} }
	sha, err := hex.DecodeString(revision)
	if err != nil || len(sha) != 20 || strings.ToLower(revision) != revision {
		return unknown("the base revision is not a full commit SHA")
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(slug, "?#%\\") {
		return unknown("the repository slug is not owner/name")
	}
	token, err := c.WorkspaceToken(ctx, st, workspace)
	if err != nil {
		return unknown("the workspace GitHub App token is unavailable")
	}
	if err = c.RequireRepository(ctx, workspace, token, slug); err != nil {
		return unknown("the workspace GitHub App cannot read the repository")
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
		return unknown("the base commit could not be read")
	}
	type treeEntry struct{ Path, Mode, Type, SHA string }
	type gitTree struct {
		Truncated bool        `json:"truncated"`
		SHA       string      `json:"sha"`
		Entries   []treeEntry `json:"tree"`
	}
	var tree gitTree
	if !get("/repos/"+slug+"/git/trees/"+commit.Tree.SHA, &tree) || tree.SHA != commit.Tree.SHA || tree.Entries == nil {
		return unknown("the base commit's tree could not be read")
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
			return conveyorFile{state: "absent"}
		}
		return unknown("the base commit's tree listing is truncated")
	}
	var directory gitTree
	if !get("/repos/"+slug+"/git/trees/"+directorySHA, &directory) || directory.SHA != directorySHA || directory.Entries == nil {
		return unknown("the .conveyor directory could not be read")
	}
	blobOID, mode, kind := "", "", ""
	for _, entry := range directory.Entries {
		if entry.Path == name {
			blobOID, mode, kind = entry.SHA, entry.Mode, entry.Type
			break
		}
	}
	if blobOID == "" {
		if !directory.Truncated {
			return conveyorFile{state: "absent"}
		}
		return unknown("the .conveyor directory listing is truncated")
	}
	if kind != "blob" || mode != "100644" && mode != "100755" {
		return conveyorFile{state: "malformed", reason: "the declaration is not a regular file"}
	}
	var blob struct {
		Encoding, Content string
		Size              int
	}
	if !get("/repos/"+slug+"/git/blobs/"+blobOID, &blob) {
		return conveyorFile{state: "unavailable", reason: "the declaration blob could not be read"}
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil || blob.Encoding != "base64" || len(data) != blob.Size {
		return conveyorFile{state: "malformed", reason: "the declaration blob is not readable base64 content"}
	}
	if len(data) > maxBytes {
		return conveyorFile{state: "malformed", reason: fmt.Sprintf("the declaration exceeds %d bytes", maxBytes)}
	}
	return conveyorFile{state: "present", data: data}
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
