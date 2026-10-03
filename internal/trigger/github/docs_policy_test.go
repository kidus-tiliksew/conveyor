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
)

const docsPolicyFixture = `schema_version: 1
docs:
  - path: docs/knowledge-base/**
    description: knowledge base
rule:
  none_statement: "docs: none"
  reason_required: true
  text: Durable docs describe merged behavior only.
`

func TestDiscoverDocumentationPolicy(t *testing.T) {
	_, private := appTestKey(t)
	sha, treeSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, mode := range []string{"present", "absent", "malformed", "permission", "unknown_revision", "transport"} {
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
					if mode != "absent" {
						entries = append(entries, map[string]string{"path": ".conveyor/docs.yaml", "mode": "100644", "type": "blob", "sha": blobSHA})
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA, "truncated": false, "tree": entries})
				case "/repos/org/repo/git/blobs/" + blobSHA:
					content := docsPolicyFixture
					if mode == "malformed" {
						content = "schema_version: 9\ndocs: []\n"
					}
					json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)), "size": len(content)})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
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
			got := client.DiscoverDocumentationPolicy(t.Context(), app, "demo", "org/repo", sha)
			if got.State != mode {
				t.Fatalf("state=%q want %q", got.State, mode)
			}
			if mode == "present" {
				if got.Policy == nil || got.Policy.Rule.NoneStatement != "docs: none" || got.Hash == "" {
					t.Fatalf("discovery=%+v", got)
				}
				if !got.Policy.MatchPath("docs/knowledge-base/page.md") {
					t.Fatal("declared glob did not match")
				}
			}
		})
	}
}

func TestDiscoverDocumentationPolicyRejectsInvalidRevision(t *testing.T) {
	app := verificationAppFixture{}
	client := NewAppClient(http.DefaultClient, "http://127.0.0.1:1")
	got := client.DiscoverDocumentationPolicy(context.Background(), app, "demo", "org/repo", "not-a-sha")
	if got.State != "unknown_revision" {
		t.Fatalf("state=%q", got.State)
	}
}

func TestAgentAuthoredPullRequestBody(t *testing.T) {
	lifecycle := pullRequestLifecycleMarker + "\nConveyor task `task`\n\nSource: mcp" + pullRequestLifecycleEndMarker
	body := "## Agent notes\n\ndocs: none — internal refactor\n\n" + lifecycle + "\n\n## Trailing agent note\ntail\n"
	got := AgentAuthoredPullRequestBody(body)
	if !strings.Contains(got, "docs: none — internal refactor") || !strings.Contains(got, "## Trailing agent note") {
		t.Fatalf("agent body=%q", got)
	}
	if strings.Contains(got, pullRequestLifecycleMarker) || strings.Contains(got, "Conveyor task") {
		t.Fatalf("lifecycle region leaked: %q", got)
	}
}
