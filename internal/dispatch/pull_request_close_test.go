package dispatch

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// This fixture exercises start-over, the post-commit hook/recovery, a minted
// App token, real REST requests, retry, events and task projection together.
func TestStartOverPullRequestCloseForgeEndToEnd(t *testing.T) {
	for _, scenario := range []string{"success", "lost_comment", "lost_close", "exhausted", "absent", "closed", "merged", "missing_app", "revoked", "no_repository", "recovery", "final_lost_close", "crash_after_final_close", "concurrent", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "ambient-must-not-be-used")
			st := store.NewVolatileBackend()
			defer st.Close()
			ctx := store.WithWorkspace(t.Context(), "demo")
			ctx = store.WithActor(ctx, store.Actor{ID: "user:restarting-operator", Role: core.ActorHuman})
			cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo", Base: "main"}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing_app" {
				st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{41}, 32))
				key, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
				if _, err = st.StoreWorkspaceGitHubApp(ctx, "demo", core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-test", ClientID: "client"}, PrivateKey: private}); err != nil {
					t.Fatal(err)
				}
				if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, "demo", 41, 12, "org"); err != nil {
					t.Fatal(err)
				}
			}
			state := "open"
			if scenario == "closed" || scenario == "merged" {
				state = "closed"
			}
			comment := ""
			posts, closes, mints := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/repos/") || r.URL.Path == "/installation/repositories" {
					if r.Header.Get("Authorization") != "Bearer close-installation-secret" {
						t.Error("forge request lacks workspace App token")
						w.WriteHeader(401)
						return
					}
				}
				switch r.URL.Path {
				case "/app/installations/12":
					fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
				case "/app/installations/12/access_tokens":
					mints++
					if scenario == "revoked" {
						http.Error(w, "close-installation-secret", 403)
						return
					}
					expiry := time.Now().Add(time.Hour).Truncate(time.Second)
					if scenario == "expired" {
						expiry = time.Now().Add(-time.Hour)
					}
					json.NewEncoder(w).Encode(map[string]any{"token": "close-installation-secret", "expires_at": expiry})
				case "/installation/repositories":
					if scenario == "no_repository" {
						fmt.Fprint(w, `{"repositories":[]}`)
					} else {
						fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
					}
				case "/repos/org/repo/pulls":
					if scenario == "absent" {
						fmt.Fprint(w, `[]`)
					} else {
						fmt.Fprint(w, `[{"number":42}]`)
					}
				case "/repos/org/repo/pulls/42":
					if r.Method == http.MethodPatch {
						closes++
						if comment == "" {
							t.Error("close before comment")
						}
						if scenario == "exhausted" {
							http.Error(w, "close-installation-secret", 500)
							return
						}
						state = "closed"
						if (scenario == "lost_close" || scenario == "final_lost_close") && closes == 1 {
							http.Error(w, "close-installation-secret", 500)
							return
						}
					} else if r.Method != http.MethodGet {
						t.Errorf("unexpected PR operation %s", r.Method)
					}
					var mergedAt any
					if scenario == "merged" {
						mergedAt = "2026-01-01T00:00:00Z"
					}
					json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/org/repo/pull/42", "state": state, "merged": scenario == "merged", "merged_at": mergedAt, "mergeable": true, "head": map[string]string{"sha": "head"}, "base": map[string]string{"sha": "base"}})
				case "/repos/org/repo/issues/42/comments":
					if r.Method == http.MethodPost {
						posts++
						var body struct {
							Body string `json:"body"`
						}
						json.NewDecoder(r.Body).Decode(&body)
						comment = body.Body
						if scenario == "lost_comment" && posts == 1 {
							http.Error(w, "close-installation-secret", 500)
							return
						}
						json.NewEncoder(w).Encode(map[string]any{"id": 9, "body": comment})
					} else if comment == "" {
						fmt.Fprint(w, `[]`)
					} else {
						json.NewEncoder(w).Encode([]any{map[string]any{"id": 9, "body": comment}})
					}
				default:
					t.Errorf("unexpected request (branch must remain): %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			transport := http.DefaultTransport
			http.DefaultTransport = appTestTransport(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				u := *r.URL
				if u.Host == "api.github.com" {
					u.Scheme = "http"
					u.Host = strings.TrimPrefix(server.URL, "http://")
				}
				clone.URL = &u
				return transport.RoundTrip(clone)
			})
			defer func() { http.DefaultTransport = transport }()
			d := New(st, cfg, nil)
			d.DisableMemoryQueueForTest()
			d.GitHubApps = github.NewAppClient(server.Client(), server.URL)
			old := core.Task{ID: core.NewTaskID(), Workspace: "demo", Title: "Start over", Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-old", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
			if err := st.CreateTask(ctx, old); err != nil {
				t.Fatal(err)
			}
			result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "revised scope"})
			if err != nil {
				t.Fatal(err)
			}
			successor, err := st.GetTask(ctx, result.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "recovery" {
				d.Enqueue(ctx, successor.ID)
				d.Enqueue(ctx, successor.ID)
			}
			// A fresh dispatcher repairs a crash between start-over commit and hook.
			if _, err = d.ReconcileGitHubLifecycles(ctx); err != nil {
				t.Fatal(err)
			}
			p, ok, err := st.GetPullRequestClose(ctx, old.ID)
			if err != nil || !ok {
				t.Fatalf("missing intent: %v", err)
			}
			if scenario == "final_lost_close" || scenario == "crash_after_final_close" {
				p.State = "retrying"
				for p.Attempts < 4 {
					p.Attempts++
					if err = st.UpdatePullRequestClose(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "crash_after_final_close" {
					p.Attempts = 5
					p.Number = 42
					p.URL = "https://github.com/org/repo/pull/42"
					if err = st.UpdatePullRequestClose(ctx, p); err != nil {
						t.Fatal(err)
					}
					state = "closed"
					comment = p.Comment()
					posts = 1
					closes = 1
				}
			}
			worker := pullRequestCloseWorker{dispatcher: d}
			if scenario == "concurrent" {
				results := make(chan error, 2)
				for i := range 2 {
					go func(i int) {
						results <- worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, int64(i+1), 1, 5))
					}(i)
				}
				for range 2 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
			}

			for attempt := 1; attempt <= core.PullRequestCloseMaxAttempts; attempt++ {
				err = worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, int64(attempt), attempt, core.PullRequestCloseMaxAttempts))
				if err != nil && (strings.Contains(err.Error(), "close-installation-secret") || strings.Contains(err.Error(), "ambient-must-not-be-used")) {
					t.Fatal("credential leaked")
				}
				p, _, _ = st.GetPullRequestClose(ctx, old.ID)
				if p.Terminal() {
					break
				}
			}
			want := "closed"
			switch scenario {
			case "exhausted", "missing_app", "revoked", "no_repository", "expired":
				want = "failed"
			case "absent", "closed", "merged":
				want = "skipped"
			}
			if p.State != want || p.ForgeAuthorClass != core.ForgeAuthorWorkspace || p.RestartingOperatorID != "restarting-operator" {
				t.Fatalf("projection=%+v last error=%v", p, err)
			}
			if want == "closed" {
				if posts != 1 || closes != 1 || comment != p.Comment() || state != "closed" {
					t.Fatalf("posts=%d closes=%d comment=%q state=%s", posts, closes, comment, state)
				}
			}
			if want == "failed" {
				if p.Attempts != 5 || state != "open" || p.ForgeErrorCategory == "" {
					t.Fatalf("failure=%+v state=%s", p, state)
				}
			}
			if scenario == "missing_app" || scenario == "revoked" || scenario == "no_repository" {
				if p.ForgeErrorCategory != "forge_permission" || !strings.Contains(p.LastError, "workspace settings") || posts != 0 || closes != 0 {
					t.Fatalf("credential failure=%+v", p)
				}
			}
			beforePosts, beforeCloses, beforeMints := posts, closes, mints
			if _, err = d.ReconcileGitHubLifecycles(ctx); err != nil {
				t.Fatal(err)
			}
			if err = worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 9, 1, 5)); err != nil {
				t.Fatal(err)
			}
			if posts != beforePosts || closes != beforeCloses || mints != beforeMints {
				t.Fatal("terminal job performed forge operations")
			}
			after, err := st.GetTask(ctx, successor.ID)
			if err != nil || !reflect.DeepEqual(after, successor) {
				t.Fatal("successor changed")
			}
			retired, err := st.GetTask(ctx, old.ID)
			if err != nil || retired.PullRequestCloseState != want {
				t.Fatal("retired task projection missing")
			}
			events, _ := st.ListEvents(ctx, old.ID)
			queued, completed := 0, 0
			for _, e := range events {
				if e.Kind == "pull_request.close_queued" {
					queued++
				}
				if e.Kind == store.PullRequestCloseEvent(p).Kind {
					completed++
				}
				if strings.Contains(string(e.Payload), "close-installation-secret") {
					t.Fatal("event leaked token")
				}
			}
			if queued != 1 || completed != 1 {
				t.Fatalf("queued=%d completed=%d", queued, completed)
			}
		})
	}
}
