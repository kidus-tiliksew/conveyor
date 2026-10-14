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

// REQ-9/AC-9.1, AC-9.4; DEC-71: exact base-revision proof distinguishes absence from unavailable.
func TestDiscoverDocumentationPolicy(t *testing.T) {
	_, private := appTestKey(t)
	sha, treeSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, mode := range []string{"present", "absent", "malformed", "unavailable", "truncated", "permission", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations/12":
					fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"},"suspended_at":null}`)
				case "/app/installations/12/access_tokens":
					json.NewEncoder(w).Encode(map[string]any{"token": "fixture-secret", "expires_at": time.Now().Add(time.Hour)})
				case "/installation/repositories":
					if mode == "permission" {
						w.WriteHeader(403)
						return
					}
					fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
				case "/repos/org/repo/git/commits/" + sha:
					if mode == "unavailable" {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": treeSHA}})
				case "/repos/org/repo/git/trees/" + treeSHA:
					if r.URL.Query().Get("recursive") != "1" {
						t.Error("missing recursive proof")
					}
					entries := []map[string]string{}
					if mode != "absent" {
						fileMode := "100644"
						if mode == "symlink" {
							fileMode = "120000"
						}
						entries = append(entries, map[string]string{"path": ".conveyor/docs.yaml", "mode": fileMode, "type": "blob", "sha": blobSHA})
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA, "truncated": mode == "truncated", "tree": entries})
				case "/repos/org/repo/git/blobs/" + blobSHA:
					content := "schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: [internal/**]\nrule:\n  reason_required: true\n"
					if mode == "malformed" {
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
			app.credential.AppID = 41
			app.credential.InstallationID = 12
			app.credential.WorkspaceID = "demo"
			app.credential.Connected = true
			got := NewAppClient(server.Client(), server.URL).DiscoverDocumentationPolicy(t.Context(), app, "demo", "org/repo", sha)
			want := mode
			if mode == "truncated" || mode == "permission" {
				want = "unavailable"
			}
			if mode == "symlink" {
				want = "malformed"
			}
			if got.State != want || got.Revision != sha {
				t.Fatalf("discovery=%+v want=%s", got, want)
			}
			if mode == "present" && (got.Policy == nil || len(got.Policy.Docs[0].Governs) != 1) {
				t.Fatalf("policy mapping lost: %+v", got)
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
