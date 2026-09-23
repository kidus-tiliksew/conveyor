package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// VerificationDiscovery contains only exact-revision repository observations.
// Repository content grants no execution authority (DEC-43, VK-2).
type VerificationDiscovery struct {
	Revision       string                              `json:"revision"`
	State          string                              `json:"state"`
	ManifestBytes  []byte                              `json:"manifest_bytes,omitempty"`
	ManifestHash   string                              `json:"manifest_hash,omitempty"`
	Manifest       *verification.Manifest              `json:"manifest,omitempty"`
	Trees          map[string][]verification.TreeEntry `json:"trees,omitempty"`
	MetadataHashes map[string]string                   `json:"metadata_hashes,omitempty"`
}

func (c *AppClient) DiscoverVerification(ctx context.Context, st AppStore, workspace, slug, revision string) VerificationDiscovery {
	d := VerificationDiscovery{Revision: revision, State: "unavailable"}
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
		raw, _, status, err := r.request(ctx, http.MethodGet, endpoint, "application/vnd.github+json", nil)
		if err != nil {
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
	entries := map[string]verification.TreeEntry{}
	for _, e := range tree.Entries {
		oid, oidErr := hex.DecodeString(e.SHA)
		validMode := e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755" || e.Mode == "120000") || e.Type == "tree" && e.Mode == "040000" || e.Type == "commit" && e.Mode == "160000"
		if _, found := entries[e.Path]; found || e.Path == "" || path.IsAbs(e.Path) || path.Clean(e.Path) != e.Path || e.Path == ".." || strings.HasPrefix(e.Path, "../") || oidErr != nil || (len(oid) != 20 && len(oid) != 32) || !validMode {
			d.State = "malformed"
			return d
		}
		entries[e.Path] = verification.TreeEntry{Path: e.Path, Mode: e.Mode, BlobOID: e.SHA}
	}
	m, exists := entries[".conveyor/kits/manifest.yaml"]
	if !exists {
		d.State = "no_manifest"
		return d
	}
	if m.Mode != "100644" && m.Mode != "100755" {
		d.State = "malformed"
		return d
	}
	var blob struct {
		Encoding, Content string
		Size              int
	}
	if !get("/repos/"+slug+"/git/blobs/"+m.BlobOID, &blob) {
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
	d.ManifestBytes = data
	d.ManifestHash = fmt.Sprintf("%x", sha256.Sum256(data))
	d.MetadataHashes = map[string]string{".conveyor/kits/manifest.yaml": d.ManifestHash}
	symlinks := map[string]string{}
	var check func(string, string) error
	check = func(boundary, relative string) error {
		for depth := 0; depth < 40; depth++ {
			if path.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") || (boundary != "." && relative != boundary && !strings.HasPrefix(relative, boundary+"/")) {
				return fmt.Errorf("path escapes kit")
			}
			if relative == "." {
				return nil
			}
			resolved := false
			for p := relative; p != "."; p = path.Dir(p) {
				e, ok := entries[p]
				if !ok {
					continue
				}
				if e.Mode == "160000" {
					return fmt.Errorf("gitlink cannot expand verification scope")
				}
				if e.Mode != "120000" {
					continue
				}
				target, ok := symlinks[p]
				if !ok {
					var link struct {
						Encoding, Content string
						Size              int
					}
					if !get("/repos/"+slug+"/git/blobs/"+e.BlobOID, &link) {
						return fmt.Errorf("symlink metadata unavailable")
					}
					raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(link.Content, "\n", ""))
					if err != nil || link.Encoding != "base64" || len(raw) != link.Size || len(raw) > 4096 {
						return fmt.Errorf("invalid symlink metadata")
					}
					target = string(raw)
					symlinks[p] = target
					d.MetadataHashes[p] = fmt.Sprintf("%x", sha256.Sum256(raw))
				}
				if path.IsAbs(target) {
					return fmt.Errorf("symlink escapes repository")
				}
				suffix := strings.TrimPrefix(relative, p)
				relative = path.Clean(path.Join(path.Dir(p), target) + suffix)
				resolved = true
				break
			}
			if resolved {
				continue
			}
			if _, ok := entries[relative]; !ok {
				return os.ErrNotExist
			}
			return nil
		}
		return fmt.Errorf("symlink cycle")
	}
	d.State = "present"
	d.Manifest, err = verification.Parse(bytes.NewReader(data), check)
	if err != nil && d.State == "present" {
		d.State = "malformed"
	}
	d.Trees = map[string][]verification.TreeEntry{}
	if d.Manifest != nil {
		for _, kit := range d.Manifest.Kits {
			var kitTree []verification.TreeEntry
			for p, e := range entries {
				if !strings.HasPrefix(p, kit.Path+"/") || e.Mode == "040000" {
					continue
				}
				if e.Mode == "160000" || (e.Mode == "120000" && check(kit.Path, p) != nil) {
					d.State = "unavailable"
					continue
				}
				e.Path = strings.TrimPrefix(p, kit.Path+"/")
				kitTree = append(kitTree, e)
			}
			d.Trees[kit.ID] = kitTree
		}
	}
	return d
}
