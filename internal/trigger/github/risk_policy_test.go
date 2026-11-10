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
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestDiscoverRiskDeclaration reads .conveyor/risk.yaml at the named base
// commit only and maps each read to a declaration state; the bytes are
// returned as read for internal/riskpolicy to parse (req-risk-based-review
// AC-3.2, AC-3.4, AC-3.5; feature-risk-based-review RR-2).
func TestDiscoverRiskDeclaration(t *testing.T) {
	_, private := appTestKey(t)
	sha, treeSHA, dirSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	declaration := "schema_version: 1\nrules:\n  - paths: [\"internal/billing/**\"]\n    min_tier: high\n    description: billing\n"
	for _, tc := range []struct {
		mode   string
		want   riskpolicy.DeclarationState
		reason string
	}{
		{"present", riskpolicy.DeclarationPresent, ""},
		{"present-invalid-yaml", riskpolicy.DeclarationPresent, ""},
		{"absent", riskpolicy.DeclarationAbsent, ""},
		{"only-docs", riskpolicy.DeclarationAbsent, ""},
		{"symlink", riskpolicy.DeclarationMalformed, "not a regular file"},
		{"oversize", riskpolicy.DeclarationMalformed, "exceeds"},
		{"unavailable", riskpolicy.DeclarationUnavailable, "could not be read"},
		{"permission", riskpolicy.DeclarationUnknown, "cannot read the repository"},
		{"bad-revision", riskpolicy.DeclarationUnknown, "not a full commit SHA"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			content := declaration
			switch tc.mode {
			case "present-invalid-yaml":
				content = "schema_version: 1.5\n"
			case "oversize":
				content = strings.Repeat("#", riskpolicy.MaxDeclarationBytes+1)
			}
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
					entries := []map[string]string{}
					if tc.mode != "absent" {
						entries = append(entries, map[string]string{"path": ".conveyor", "mode": "040000", "type": "tree", "sha": dirSHA})
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA, "truncated": false, "tree": entries})
				case "/repos/org/repo/git/trees/" + dirSHA:
					mode := "100644"
					if tc.mode == "symlink" {
						mode = "120000"
					}
					entries := []map[string]string{{"path": "docs.yaml", "mode": "100644", "type": "blob", "sha": strings.Repeat("e", 40)}}
					if tc.mode != "only-docs" {
						entries = append(entries, map[string]string{"path": "risk.yaml", "mode": mode, "type": "blob", "sha": blobSHA})
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": dirSHA, "truncated": false, "tree": entries})
				case "/repos/org/repo/git/blobs/" + blobSHA:
					if tc.mode == "unavailable" {
						w.WriteHeader(404)
						return
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
			revision := sha
			if tc.mode == "bad-revision" {
				revision = "main"
			}
			got := NewAppClient(server.Client(), server.URL).DiscoverRiskDeclaration(t.Context(), app, "demo", "org/repo", revision)
			if got.State != tc.want || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("discovery=%+v want state %s reason %q", got, tc.want, tc.reason)
			}
			if tc.want == riskpolicy.DeclarationPresent && string(got.Bytes) != content {
				t.Fatalf("present bytes=%q, want the blob as read", got.Bytes)
			}
			if tc.want != riskpolicy.DeclarationPresent && len(got.Bytes) != 0 {
				t.Fatalf("state %s carries bytes", got.State)
			}
		})
	}
}
