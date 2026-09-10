package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type appTestTransport func(*http.Request) (*http.Response, error)

func (f appTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWorkspaceGitHubAppResolverUsesInstallationIdentity(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-token-must-not-be-used")
	t.Setenv("PATH", t.TempDir())
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{41}, 32))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-test", ClientID: "client"}, PrivateKey: private}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, "demo", app); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, "demo", 41, 12, "org"); err != nil {
		t.Fatal(err)
	}
	var forgeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
		case "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "resolver-installation-secret", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
		case "/repos/org/repo/compare/base-sha...named-head":
			if r.Header.Get("Authorization") != "Bearer resolver-installation-secret" {
				t.Error("compare missing workspace app credential")
			}
			if strings.Contains(r.Header.Get("Accept"), ".diff") {
				fmt.Fprint(w, "diff --git a/internal/change.go b/internal/change.go\n+change\n")
				return
			}
			if r.URL.Query().Get("page") != "1" {
				t.Error("compare files paginated")
			}
			fmt.Fprint(w, `{"files":[{"filename":"internal/change.go"}],"total_commits":1000}`)
		case "/repos/org/repo/pulls/12/merge":
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer resolver-installation-secret" {
				t.Error("merge did not authenticate with the App")
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["commit_message"] != "Approved-by: Approving Operator <approver@example.com>" {
				t.Errorf("merge message=%v", payload["commit_message"])
			}
			fmt.Fprint(w, `{"merged":true}`)
		case "/repos/org/repo/pulls/1/files":
			forgeCalls++
			if r.Header.Get("Authorization") != "Bearer resolver-installation-secret" {
				t.Error("forge act did not authenticate with minted token")
			}
			fmt.Fprint(w, `[{"filename":"README.md"}]`)
		default:
			t.Errorf("unexpected GitHub endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := github.NewAppClient(server.Client(), server.URL)
	originalRunner := client.Runner
	client.Runner = func(token, identity string) github.AppRunner {
		if identity != "workspace demo GitHub App" {
			t.Error("wrong identity attribution")
		}
		return originalRunner(token, identity)
	}
	originalTransport := http.DefaultTransport
	http.DefaultTransport = appTestTransport(func(r *http.Request) (*http.Response, error) {
		cloned := r.Clone(r.Context())
		u := *r.URL
		if u.Host == "api.github.com" {
			u.Scheme = "http"
			u.Host = strings.TrimPrefix(server.URL, "http://")
		}
		cloned.URL = &u
		return originalTransport.RoundTrip(cloned)
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	d := New(st, cfg, nil)
	d.GitHubApps = client
	resolve := func(c context.Context, repo string) (context.Context, error) { return d.workspaceForgeContext(c, repo) }
	_, err = d.ListPullRequestFiles(ctx, "org/repo", 1)

	if err != nil {
		t.Fatal(err)
	}
	if forgeCalls != 1 {
		t.Fatal("forge act was not executed")
	}
	task := core.Task{ID: "compare-governance", Workspace: "demo", Repo: "repo", Branch: "mutable-branch", BaseBranch: "base-sha", ReviewedHeadSHA: "named-head", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "compare-design", Title: "Compare", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Compare\n\n```conveyor:governs\n- repo: repo\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	paths, err := d.ReviewChangedPaths(ctx, cfg, task)
	if err != nil || len(paths) != 1 || paths[0] != "internal/change.go" {
		t.Fatalf("compare paths=%v err=%v", paths, err)
	}
	attached, err := st.AttachSubmissionGovernance(ctx, task.ID, task.Repo, paths, store.SubmissionGovernanceAttribution{})
	if err != nil || len(attached) != 1 || attached[0].ID != design.ID {
		t.Fatalf("compare governance=%+v err=%v", attached, err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "base-sha", "head_sha": "named-head"})}); err != nil {
		t.Fatal(err)
	}
	// Mutating the task's branch/base/head projections cannot change review input.
	task.BaseBranch, task.ReviewedHeadSHA = "moved-base", "moved-head"
	diff, err := d.ReviewDiff(ctx, cfg, task)
	if err != nil || !strings.Contains(diff, "+change") {
		t.Fatalf("diff=%q err=%v", diff, err)
	}
	if _, err = resolve(context.Background(), "org/repo"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("implicit workspace accepted")
	}
	if _, err = resolve(ctx, "org/uncovered"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("uncovered repository accepted")
	}
	mergeTask := core.Task{ID: "app-merge", Workspace: "demo", Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-app-merge", State: core.TaskApproved, MergeApproval: true, ReviewedHeadSHA: "head", ApprovedHeadSHA: "head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, mergeTask); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: mergeTask.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	d.Store = mergeIdentityStore{st}
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (github.PullRequest, error) {
		views++
		return github.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: views > 1, HeadSHA: "head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, mergeTask); err != nil {
		t.Fatal(err)
	}
	mergeEvents, err := st.ListEvents(ctx, mergeTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range mergeEvents {
		if event.Kind != "merge.confirmed" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		found = payload["forge_author_class"] == "workspace" && payload["approving_operator_user_id"] == "usr-approver" && payload["approving_operator_display_name"] == "Approving Operator"
	}
	if !found {
		t.Fatal("merge confirmation omitted the approving operator")
	}
	if err = st.DeleteWorkspaceGitHubApp(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err = resolve(ctx, "org/repo"); github.ErrorCategory(err) != github.ForgePermission {
		t.Fatal("deleted app fell back to cache or legacy token")
	}
	events, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resolver-installation-secret") || strings.Contains(string(raw), private) {
		t.Fatal("forge credential in event")
	}
}
