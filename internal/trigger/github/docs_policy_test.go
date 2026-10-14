package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// REQ-9/AC-9.1, AC-9.4, AC-9.6; DEC-71: no recursive tree or unknown-read opt-in.
func TestDiscoverDocumentationPolicy(t *testing.T) {
	_, private := appTestKey(t)
	sha, treeSHA, dirSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	for _, tc := range []struct{ mode, want string }{
		{"present", "present"}, {"absent", "absent"}, {"malformed", "malformed"},
		{"unavailable", "unavailable"}, {"root-truncated-without-declaration", "unknown"},
		{"root-truncated-with-declaration", "present"}, {"permission", "unknown"},
		{"symlink", "malformed"}, {"transient-root", "absent"}, {"transient-blob", "present"},
		{"unavailable-root", "unknown"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			rootReads, blobReads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations/12":
					fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"},"suspended_at":null}`)
				case "/app/installations/12/access_tokens":
					json.NewEncoder(w).Encode(map[string]any{"token": "fixture-secret", "expires_at": time.Now().Add(time.Hour)})
				case "/installation/repositories":
					if tc.mode == "permission" {
						w.WriteHeader(403)
						return
					}
					fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
				case "/repos/org/repo/git/commits/" + sha:
					json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": treeSHA}})
				case "/repos/org/repo/git/trees/" + treeSHA:
					rootReads++
					if r.URL.Query().Get("recursive") != "" {
						t.Error("declaration discovery must not request a recursive repository tree")
					}
					if tc.mode == "unavailable-root" || tc.mode == "transient-root" && rootReads == 1 {
						w.WriteHeader(503)
						return
					}
					entries := []map[string]string{}
					if tc.mode != "absent" && tc.mode != "transient-root" && tc.mode != "root-truncated-without-declaration" {
						entries = append(entries, map[string]string{"path": ".conveyor", "mode": "040000", "type": "tree", "sha": dirSHA})
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA, "truncated": strings.HasPrefix(tc.mode, "root-truncated"), "tree": entries})
				case "/repos/org/repo/git/trees/" + dirSHA:
					if r.URL.Query().Get("recursive") != "" {
						t.Error("declaration directory must not be recursive")
					}
					mode := "100644"
					if tc.mode == "symlink" {
						mode = "120000"
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": dirSHA, "truncated": false, "tree": []map[string]string{{"path": "docs.yaml", "mode": mode, "type": "blob", "sha": blobSHA}}})
				case "/repos/org/repo/git/blobs/" + blobSHA:
					blobReads++
					if tc.mode == "unavailable" || tc.mode == "transient-blob" && blobReads == 1 {
						w.WriteHeader(503)
						return
					}
					content := "schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: [internal/**]\nrule:\n  reason_required: true\n"
					if tc.mode == "malformed" {
						content += "unknown: true\n"
					}
					json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)), "size": len(content)})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			app := verificationAppFixture{core.WorkspaceGitHubAppCredential{PrivateKey: private}}
			app.credential.AppID, app.credential.InstallationID = 41, 12
			app.credential.WorkspaceID, app.credential.Connected = "demo", true
			got := NewAppClient(server.Client(), server.URL).DiscoverDocumentationPolicy(t.Context(), app, "demo", "org/repo", sha)
			if got.State != tc.want || got.Revision != sha {
				t.Fatalf("discovery=%+v want=%s", got, tc.want)
			}
			if tc.want == "present" && (got.Policy == nil || len(got.Policy.Docs[0].Governs) != 1) {
				t.Fatalf("policy mapping lost: %+v", got)
			}
			if tc.mode == "transient-root" && rootReads != 2 || tc.mode == "transient-blob" && blobReads != 2 {
				t.Fatalf("transient read not retried: root=%d blob=%d", rootReads, blobReads)
			}
		})
	}
}
func TestAgentAuthoredDocumentationBody(t *testing.T) {
	generated := pullRequestLifecycleMarker + "\ndocs: none generated reason\n"
	if got := AgentAuthoredPullRequestBody(generated); strings.Contains(got, "docs: none") {
		t.Fatalf("generated statement accepted: %q", got)
	}
	if got := AgentAuthoredPullRequestBody("docs: none agent reason\r\n\r\n" + generated); !strings.Contains(got, "docs: none agent reason") || strings.Contains(got, "generated reason") {
		t.Fatalf("agent extraction=%q", got)
	}
}
