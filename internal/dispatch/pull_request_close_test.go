package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	for _, scenario := range []string{"success", "lost_comment", "lost_close", "exhausted", "absent", "closed", "merged", "missing_app", "revoked", "no_repository", "recovery", "final_lost_close", "crash_after_final_close", "concurrent", "expired", "confirmed_open_then_unavailable"} {
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
			postPatchReads := 0
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
						if scenario != "confirmed_open_then_unavailable" {
							state = "closed"
						}
						if (scenario == "lost_close" || scenario == "final_lost_close") && closes == 1 {
							http.Error(w, "close-installation-secret", 500)
							return
						}
					} else if r.Method != http.MethodGet {
						t.Errorf("unexpected PR operation %s", r.Method)
					}
					if scenario == "confirmed_open_then_unavailable" && r.Method == http.MethodGet && closes > 0 {
						postPatchReads++
						if postPatchReads == 2 {
							http.Error(w, "follow-up unavailable", 500)
							return
						}
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
					p.ForgeErrorCategory = string(github.ForgeRequest)
					p.LastError = closeUncertainMarker
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
				if scenario == "confirmed_open_then_unavailable" && attempt == 1 {
					if err == nil || p.LastError == closeUncertainMarker {
						t.Fatalf("confirmed-open helper left uncertainty: %+v err=%v", p, err)
					}
					state = "closed" // Human close after helper proved the PATCH did not close it.
				}
				if p.Terminal() {
					break
				}
			}
			want := "closed"
			switch scenario {
			case "exhausted", "missing_app", "revoked", "no_repository", "expired":
				want = "failed"
			case "absent", "closed", "merged", "confirmed_open_then_unavailable":
				want = "skipped"
			}
			if p.State != want || p.ForgeAuthorClass != core.ForgeAuthorWorkspace || p.RestartingOperatorID != "restarting-operator" {
				t.Fatalf("projection=%+v last error=%v", p, err)
			}
			if scenario == "confirmed_open_then_unavailable" && (p.Outcome != "already_closed" || closes != 1 || postPatchReads != 3) {
				t.Fatalf("composed confirmation=%+v closes=%d reads=%d", p, closes, postPatchReads)
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

func TestObservePullRequestPrefersRecordedNumber(t *testing.T) {
	var sawNumber, sawBranch bool
	w := pullRequestCloseWorker{dispatcher: &Dispatcher{
		PullRequestForNumber: func(_ context.Context, repo string, number int) (github.PullRequest, error) {
			sawNumber = true
			if repo != "org/repo" || number != 42 {
				t.Fatalf("number observe repo=%s number=%d", repo, number)
			}
			return github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}, nil
		},
		PullRequestForClose: func(context.Context, string, string) (github.PullRequest, error) {
			sawBranch = true
			return github.PullRequest{}, fmt.Errorf("branch lookup")
		},
	}}
	pr, err := w.observePullRequest(t.Context(), core.PullRequestClose{Repository: "org/repo", Branch: "other-branch", Number: 42})
	if err != nil || !sawNumber || sawBranch || pr.Number != 42 {
		t.Fatalf("pr=%+v sawNumber=%t sawBranch=%t err=%v", pr, sawNumber, sawBranch, err)
	}
}

func TestQueueStartedOverPRCopiesOpenedIdentity(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(t.Context(), "demo")
	ctx = store.WithActor(ctx, store.Actor{ID: "user:restarting-operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	old := core.Task{ID: core.NewTaskID(), Workspace: "demo", Title: "Start over", Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-old", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: old.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 42, "url": "https://github.com/org/repo/pull/42"})}); err != nil {
		t.Fatal(err)
	}
	result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "revised scope"})
	if err != nil {
		t.Fatal(err)
	}
	d := New(st, cfg, nil)
	retired, err := st.GetTask(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.queueStartedOverPR(ctx, retired); err != nil {
		t.Fatal(err)
	}
	p, ok, err := st.GetPullRequestClose(ctx, old.ID)
	if err != nil || !ok || p.Number != 42 || p.URL != "https://github.com/org/repo/pull/42" || p.SuccessorID != result.Successor.ID {
		t.Fatalf("close=%+v exists=%v err=%v", p, ok, err)
	}
}

func closeOwnershipFixture(t *testing.T, recorded bool) (context.Context, store.Store, *Dispatcher, core.Task, core.Task) {
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	ctx = store.WithActor(ctx, store.Actor{ID: "user:restarting-operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	{
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
		case "/app/installations/12/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "test-token", "expires_at": time.Now().Add(time.Hour)})
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	d.GitHubApps = github.NewAppClient(server.Client(), server.URL)
	old := core.Task{ID: core.NewTaskID(), Workspace: "demo", Title: "retired", Repo: "repo", BaseBranch: "main", Branch: "feature/shared", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	if recorded {
		if err := st.AppendEvent(ctx, core.Event{TaskID: old.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 42, "url": "https://github.com/org/repo/pull/42"})}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "new scope"}); err != nil {
		t.Fatal(err)
	}
	old, err := st.GetTask(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.queueStartedOverPR(ctx, old); err != nil {
		t.Fatal(err)
	}
	other := core.Task{ID: core.NewTaskID(), Workspace: "demo", Title: "new holder", Repo: "repo", BaseBranch: "main", Branch: "feature/other", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, other); err != nil {
		t.Fatal(err)
	}
	return ctx, st, d, old, other
}

func TestStartOverCloseOwnership(t *testing.T) {
	for _, scenario := range []string{"branch_holder", "persisted_branch_number", "recorded_same_number", "recorded_other_number", "contest_after_observation"} {
		t.Run(scenario, func(t *testing.T) {
			recorded := strings.HasPrefix(scenario, "recorded") || scenario == "contest_after_observation"
			ctx, st, d, old, other := closeOwnershipFixture(t, recorded)
			if scenario != "contest_after_observation" {
				if _, err := st.AttachTaskBranch(ctx, other.ID, old.Branch); err != nil {
					t.Fatal(err)
				}
			}
			recordOther := func(number int) {
				if err := st.AppendEvent(ctx, core.Event{TaskID: other.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": number})}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "recorded_same_number" {
				recordOther(42)
			}
			if scenario == "recorded_other_number" {
				recordOther(99)
			}
			if scenario == "persisted_branch_number" {
				p, _, _ := st.GetPullRequestClose(ctx, old.ID)
				p.State = "retrying"
				p.Attempts = 1
				p.Number = 42
				p.URL = "https://github.com/org/repo/pull/42"
				if err := st.UpdatePullRequestClose(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			closes, branchReads, numberReads := 0, 0, 0
			pr := github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}
			d.PullRequestForClose = func(context.Context, string, string) (github.PullRequest, error) { branchReads++; return pr, nil }
			d.PullRequestForNumber = func(context.Context, string, int) (github.PullRequest, error) { numberReads++; return pr, nil }
			d.ClosePullRequest = func(context.Context, string, int, string, string) error {
				closes++
				pr.State = "closed"
				if scenario == "contest_after_observation" {
					recordOther(42)
				}
				return nil
			}
			worker := pullRequestCloseWorker{d}
			err := worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 1, 1, 5))
			p, _, _ := st.GetPullRequestClose(ctx, old.ID)
			if scenario == "recorded_other_number" {
				if err != nil || p.State != "closed" || closes != 1 || branchReads != 0 || numberReads != 2 {
					t.Fatalf("close=%+v err=%v calls=%d/%d/%d", p, err, closes, branchReads, numberReads)
				}
			} else {
				expectedCloses := 0
				if scenario == "contest_after_observation" {
					expectedCloses = 1
				}
				if err == nil || p.State != "retrying" || p.LastError == closeUncertainMarker || closes != expectedCloses {
					t.Fatalf("close=%+v err=%v closes=%d", p, err, closes)
				}
				if expectedCloses == 0 {
					wantReads := 0
					if recorded {
						wantReads = 1
					}
					if branchReads+numberReads != wantReads {
						t.Fatalf("reads=%d want=%d", branchReads+numberReads, wantReads)
					}
				}
			}
		})
	}
}

type closeLockTrackingStore struct {
	store.Store
	branchLocked bool
}

func (s *closeLockTrackingStore) WithTaskSideEffectLock(ctx context.Context, key string, fn func(context.Context) error) error {
	return s.Store.WithTaskSideEffectLock(ctx, key, func(ctx context.Context) error {
		if strings.HasPrefix(key, "branch-close:") {
			s.branchLocked = true
			defer func() { s.branchLocked = false }()
		}
		return fn(ctx)
	})
}

// A late opened event must not convert a branch-discovered retry into the
// recorded-number path, even when its number and URL match exactly.
func TestStartOverCloseRetryPreservesBranchOrigin(t *testing.T) {
	for _, holder := range []bool{false, true} {
		t.Run(fmt.Sprintf("holder=%t", holder), func(t *testing.T) {
			ctx, st, d, old, other := closeOwnershipFixture(t, false)
			locks := &closeLockTrackingStore{Store: st}
			d.Store = locks
			assertBranchLock := func() {
				t.Helper()
				if !locks.branchLocked {
					t.Fatal("branch-derived forge operation escaped branch ownership lock")
				}
			}
			pr := github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}
			branchReads, numberReads, closes := 0, 0, 0
			d.PullRequestForClose = func(context.Context, string, string) (github.PullRequest, error) {
				assertBranchLock()
				branchReads++
				return pr, nil
			}
			d.PullRequestForNumber = func(context.Context, string, int) (github.PullRequest, error) {
				assertBranchLock()
				numberReads++
				return pr, nil
			}
			d.ClosePullRequest = func(context.Context, string, int, string, string) error {
				assertBranchLock()
				closes++
				return errors.New("pre-mutation failure")
			}
			worker := pullRequestCloseWorker{d}
			job := testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 1, 1, 5)
			if err := worker.Work(ctx, job); err == nil {
				t.Fatal("first attempt should fail")
			}
			p, _, err := st.GetPullRequestClose(ctx, old.ID)
			if err != nil || p.State != "retrying" || p.Number != 42 || p.Attempts != 1 {
				t.Fatalf("first attempt: %+v, %v", p, err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: old.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": pr.Number, "url": pr.URL})}); err != nil {
				t.Fatal(err)
			}
			if holder {
				if _, err := st.AttachTaskBranch(ctx, other.ID, old.Branch); err != nil {
					t.Fatal(err)
				}
			}
			branchReads, numberReads, closes = 0, 0, 0
			d.ClosePullRequest = func(context.Context, string, int, string, string) error {
				assertBranchLock()
				closes++
				pr.State = "closed"
				return nil
			}
			err = worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 2, 2, 5))
			p, _, _ = st.GetPullRequestClose(ctx, old.ID)
			if holder {
				if err == nil || p.State != "retrying" || closes != 0 || numberReads != 0 || branchReads != 0 || p.LastError == closeUncertainMarker {
					t.Fatalf("branch origin lost: %+v err=%v calls=%d/%d/%d", p, err, closes, branchReads, numberReads)
				}
			} else if err != nil || p.State != "closed" || closes != 1 || numberReads != 2 || branchReads != 0 {
				t.Fatalf("retry did not address persisted number: %+v err=%v calls=%d/%d/%d", p, err, closes, branchReads, numberReads)
			}
		})
	}
}

func TestStartOverCloseUncertaintyClearedByOpenObservation(t *testing.T) {
	ctx, st, d, old, _ := closeOwnershipFixture(t, true)
	pr := github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}
	d.PullRequestForNumber = func(context.Context, string, int) (github.PullRequest, error) { return pr, nil }
	d.ClosePullRequest = func(context.Context, string, int, string, string) error { return github.ErrMutationUncertain }
	worker := pullRequestCloseWorker{d}
	if err := worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 1, 1, 5)); err == nil {
		t.Fatal("open result accepted")
	}
	p, _, _ := st.GetPullRequestClose(ctx, old.ID)
	if p.LastError == closeUncertainMarker {
		t.Fatalf("open result retained uncertainty: %+v", p)
	}
	pr.State = "closed" // Human closes it after the failed attempt.
	if err := worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 2, 2, 5)); err != nil {
		t.Fatal(err)
	}
	p, _, _ = st.GetPullRequestClose(ctx, old.ID)
	if p.State != "skipped" || p.Outcome != "already_closed" {
		t.Fatalf("human close reconciled: %+v", p)
	}
}

func TestStartOverCloseSerializesAttach(t *testing.T) {
	ctx, st, d, old, other := closeOwnershipFixture(t, false)
	observed, allowClose := make(chan struct{}), make(chan struct{})
	closed := false
	d.PullRequestForClose = func(context.Context, string, string) (github.PullRequest, error) {
		close(observed)
		<-allowClose
		return github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}, nil
	}
	d.PullRequestForNumber = func(context.Context, string, int) (github.PullRequest, error) {
		if !closed {
			t.Error("re-observation before close")
		}
		return github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "closed"}, nil
	}
	d.ClosePullRequest = func(context.Context, string, int, string, string) error { closed = true; return nil }
	done := make(chan error, 1)
	go func() {
		done <- (&pullRequestCloseWorker{d}).Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 1, 1, 5))
	}()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("close never observed")
	}
	attached := make(chan error, 1)
	go func() { _, err := st.AttachTaskBranch(ctx, other.ID, old.Branch); attached <- err }()
	select {
	case err := <-attached:
		close(allowClose)
		t.Fatalf("attach passed active close: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowClose)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close deadlocked")
	}
	select {
	case err := <-attached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach deadlocked")
	}
}

func TestStartOverCloseUnavailableConfirmationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closeErr error
		want     string
	}{
		{"pre_mutation", fmt.Errorf("read failed"), "already_closed"},
		{"refused", github.PermissionError(fmt.Errorf("refused")), "already_closed"},
		{"confirmed_open", &github.Error{Category: github.ForgeResponse, Err: fmt.Errorf("GitHub did not confirm pull request closure")}, "already_closed"},
		{"ambiguous_patch", github.ErrMutationUncertain, "reconciled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, d, old, _ := closeOwnershipFixture(t, true)
			reads := 0
			pr := github.PullRequest{Number: 42, URL: "https://github.com/org/repo/pull/42", State: "open"}
			d.PullRequestForNumber = func(context.Context, string, int) (github.PullRequest, error) {
				reads++
				if reads == 2 {
					return github.PullRequest{}, fmt.Errorf("confirmation unavailable")
				}
				return pr, nil
			}
			d.ClosePullRequest = func(context.Context, string, int, string, string) error { return tc.closeErr }
			worker := pullRequestCloseWorker{d}
			if err := worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 1, 1, 5)); err == nil {
				t.Fatal("missing failure")
			}
			p, _, _ := st.GetPullRequestClose(ctx, old.ID)
			if (p.LastError == closeUncertainMarker) != (tc.want == "reconciled") {
				t.Fatalf("marker=%+v", p)
			}
			pr.State = "closed"
			if err := worker.Work(ctx, testJob(queue.PullRequestCloseArgs{WorkspaceID: "demo", TaskID: old.ID}, 2, 2, 5)); err != nil {
				t.Fatal(err)
			}
			p, _, _ = st.GetPullRequestClose(ctx, old.ID)
			if p.Outcome != tc.want {
				t.Fatalf("outcome=%+v want=%s", p, tc.want)
			}
		})
	}
}
