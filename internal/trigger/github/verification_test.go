package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type verificationAppFixture struct {
	credential core.WorkspaceGitHubAppCredential
}

func (s verificationAppFixture) GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error) {
	return s.credential, nil
}
func TestVerificationDiscoveryExactRevision(t *testing.T) {
	_, private := appTestKey(t)
	sha, treeSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	manifest := `schema_version: 1
kits:
  - id: fixture
    name: Fixture
    version: "1"
    path: .conveyor/kits/fixture
    governing_pins:
      requirements: [{document_id: req-fixture, version: 1}]
      system_designs: []
    exercises:
      - id: check
        stages: [verify]
        kind: script
        argv: ["./check.sh"]
        cwd: "."
        timeout_seconds: 30
        prerequisites: []
        permissions: []
        inputs: []
        required_assertions: []
        retry_policy: safe_to_replay
        safety_basis: read-only
        operations: []
        evidence_outputs: []
        supports: []
`
	for _, mode := range []string{"present", "no_manifest", "malformed", "permission", "unknown_revision", "truncated", "transport", "unavailable", "token_transport", "invalid_tree"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations/12":
					if mode == "token_transport" {
						w.WriteHeader(503)
						return
					}
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
					if mode == "unknown_revision" {
						w.WriteHeader(404)
						return
					}
					if mode == "transport" {
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": treeSHA}})
				case "/repos/org/repo/git/trees/" + treeSHA:
					if r.URL.Query().Get("recursive") != "1" {
						t.Error("tree not recursive")
					}
					entries := []map[string]string{}
					if mode != "no_manifest" {
						entries = append(entries, map[string]string{"path": ".conveyor/kits/manifest.yaml", "mode": "100644", "type": "blob", "sha": blobSHA}, map[string]string{"path": ".conveyor/kits/fixture", "mode": "040000", "type": "tree", "sha": treeSHA}, map[string]string{"path": ".conveyor/kits/fixture/check.sh", "mode": "100755", "type": "blob", "sha": sha})
					}
					if mode == "invalid_tree" {
						entries[0]["path"] = "../manifest.yaml"
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA, "truncated": mode == "truncated", "tree": entries})
				case "/repos/org/repo/git/blobs/" + blobSHA:
					if mode == "unavailable" {
						w.WriteHeader(404)
						return
					}
					content := manifest
					if mode == "malformed" {
						content = "schema_version: [invalid"
					}
					json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)), "size": len(content)})
				default:
					t.Errorf("unresolved revision or unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client := NewAppClient(server.Client(), server.URL)
			app := verificationAppFixture{core.WorkspaceGitHubAppCredential{PrivateKey: private}}
			app.credential.AppID = 41
			app.credential.InstallationID = 12
			app.credential.WorkspaceID = "demo"
			app.credential.Connected = true
			got := client.DiscoverVerification(t.Context(), app, "demo", "org/repo", sha)
			expected := mode
			if mode == "token_transport" {
				expected = "transport"
			}
			if mode == "invalid_tree" {
				expected = "malformed"
			}
			if got.State != expected {
				t.Fatalf("state %q want %q", got.State, expected)
			}
			if mode == "present" {
				if got.ManifestHash == "" || string(got.ManifestBytes) != manifest || len(got.Trees["fixture"]) != 1 {
					t.Fatalf("snapshot incomplete: %+v", got)
				}
				receipt := verification.Evaluate(got.Manifest, verification.SelectionContext{Stage: "verify", Pins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, ManifestRevision: sha}, got.Trees)
				if receipt.Invalid() || receipt.Kits[0].Eligibility != "eligible" || receipt.Kits[0].Digest == "" {
					t.Fatalf("selection %+v", receipt)
				}
			}
		})
	}
}
