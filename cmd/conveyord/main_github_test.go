package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Recorded GitHub deliveries, trimmed to the fields GitHub sends that the
// receiver reads (req-quiet-idle-runtime AC-3.1).
const (
	recordedExternalMerge = `{"action":"closed","number":41,"pull_request":{"number":41,"html_url":"https://github.com/acme/app/pull/41","state":"closed","merged":true,"merge_commit_sha":"landed-41","merged_by":{"login":"octocat"},"head":{"sha":"head-a","ref":"conveyor/task-a"},"base":{"sha":"base-41","ref":"main"}},"repository":{"full_name":"acme/app"},"sender":{"login":"octocat"}}`
	recordedPendingStatus = `{"sha":"head-b","state":"pending","context":"ci","branches":[{"name":"conveyor/task-b","commit":{"sha":"head-b"}}],"repository":{"full_name":"acme/app"}}`
	recordedGreenSuite    = `{"action":"completed","check_suite":{"head_sha":"head-b","head_branch":"conveyor/task-b","status":"completed","conclusion":"success","pull_requests":[{"number":42,"head":{"ref":"conveyor/task-b","sha":"head-b"}}]},"repository":{"full_name":"acme/app"}}`
	recordedOtherStatus   = `{"sha":"unrelated","state":"success","branches":[{"name":"feature/elsewhere"}],"repository":{"full_name":"acme/app"}}`
	recordedBasePush      = `{"ref":"refs/heads/main","before":"aaa","after":"bbb","deleted":false,"repository":{"full_name":"acme/app"}}`
	recordedBranchPush    = `{"ref":"refs/heads/feature/elsewhere","deleted":false,"repository":{"full_name":"acme/app"}}`
	recordedReadyLabel    = `{"action":"labeled","label":{"name":"conveyor:ready"},"issue":{"number":7,"title":"Add a health page","body":"Serve /health.","html_url":"https://github.com/acme/app/issues/7","state":"open","labels":[{"name":"conveyor:ready"}]},"repository":{"full_name":"acme/app"}}`
)

const replaySecret = "replay-webhook-secret-0123456789abcdef"

type replayForge struct {
	mu       sync.Mutex
	views    map[string]int
	merges   map[int]int
	rest     []string
	mergedB  bool
	readyB   bool
	monitorN int
}

func (f *replayForge) restCalls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.rest {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// TestGitHubWebhookReplayAgainstFakeForge replays recorded GitHub deliveries
// through the real receiver and applier on the volatile backend against a
// fake forge: an external merge completes its approved task, a pending
// status leaves a gate-off approved task unmerged and the green check suite
// then auto-merges it, an unrelated status changes nothing, a push to the
// monitored base branch runs one monitor poll while a push elsewhere runs
// none, a conveyor:ready label files one task and moves the issue's labels,
// and a repeated delivery applies nothing (req-quiet-idle-runtime AC-3.1,
// AC-3.3; req-delivery-and-forge AC-3.5; req-review-gates-evidence AC-1.1;
// req-intake-and-triage AC-4.1; component-runtime, GitHub webhook applier).
func TestGitHubWebhookReplayAgainstFakeForge(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", GitHub: "acme/app", Base: "main"}}}
	cfg.Monitor.Enabled = true
	cfg.Monitor.Repositories = []string{"app"}
	cfg.Monitor.PollInterval = time.Minute
	cfg.Monitor.StartupWindow = time.Hour
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{41}, 32))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-demo", ClientID: "client"}, PrivateKey: private, WebhookSecret: replaySecret}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, "demo", app); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, "demo", 41, 12, "acme"); err != nil {
		t.Fatal(err)
	}

	forge := &replayForge{views: map[string]int{}, merges: map[int]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "replay-installation-token", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case r.URL.Path == "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"acme"}}`)
		case r.URL.Path == "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"acme/app"}]}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/"):
			forge.mu.Lock()
			forge.rest = append(forge.rest, r.Method+" "+r.URL.Path)
			forge.mu.Unlock()
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected GitHub endpoint %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
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

	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	d.WorkspaceGitHubApps = st
	d.GitHubApps = githubtrigger.NewAppClient(server.Client(), server.URL)
	d.ReadCommitChecks = dispatch.NoChecksReportedForTest
	d.ViewPullRequest = func(_ context.Context, repo, branch string) (githubtrigger.PullRequest, error) {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		forge.views[branch]++
		switch {
		case branch == "conveyor/task-b" && forge.mergedB:
			return githubtrigger.PullRequest{Number: 42, State: "closed", Merged: true, HeadSHA: "head-b", HeadRef: branch, BaseSHA: "base-42", MergeCommitSHA: "landed-42"}, nil
		case branch == "conveyor/task-b":
			// GitHub reports a conflict-free pull request mergeable while
			// required checks run; branch protection refuses the merge.
			return githubtrigger.PullRequest{Number: 42, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head-b", HeadRef: branch, BaseSHA: "base-42"}, nil
		}
		return githubtrigger.PullRequest{}, fmt.Errorf("%w for branch %s", githubtrigger.ErrPullRequestNotFound, branch)
	}
	d.RequestMerge = func(_ context.Context, repo string, number int) error {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		if !forge.readyB {
			return githubtrigger.StatusError(http.StatusMethodNotAllowed, errors.New("GitHub REST status 405: required status check \"ci\" is expected"))
		}
		forge.merges[number]++
		forge.mergedB = true
		return nil
	}
	for _, seed := range []struct{ id, head string }{{"task-a", "head-a"}, {"task-b", "head-b"}} {
		task := core.Task{ID: seed.id, Workspace: "demo", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + seed.id, State: core.TaskApproved, CreatedAt: time.Now().UTC()}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err = st.BindTaskApproval(ctx, seed.id, seed.head); err != nil {
			t.Fatal(err)
		}
	}
	for _, opened := range []struct {
		id     string
		number int
		head   string
	}{{"task-a", 41, "head-a"}, {"task-b", 42, "head-b"}} {
		if err = st.AppendEvent(ctx, core.Event{TaskID: opened.id, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "number": opened.number, "head_sha": opened.head})}); err != nil {
			t.Fatal(err)
		}
	}

	service := &monitor.Service{Store: st, WorkspaceID: "demo", Enabled: true, Repositories: map[string]struct{}{"app": {}}, Intake: func(context.Context, monitor.TaskRequest) (monitor.IntakeResult, error) {
		t.Error("monitor filed a task")
		return monitor.IntakeResult{}, nil
	}}
	polls := &monitorPoller{Store: st, Dispatcher: d, Service: service,
		Config: func(context.Context) (*config.Config, error) { return cfg, nil },
		Runner: func(_ context.Context, workspace, slug string) (func(context.Context, ...string) ([]byte, error), error) {
			forge.mu.Lock()
			forge.monitorN++
			forge.mu.Unlock()
			// The default branch has no commit in the startup window.
			return func(_ context.Context, args ...string) ([]byte, error) { return quietMonitorRead(args...), nil }, nil
		},
	}
	var delivered []string
	applier := newGitHubWebhookApplier(st, d, polls.PushedRef, polls.ChecksCompleted, func(workspace string) { delivered = append(delivered, workspace) }, t.Logf)
	applier.Start(t.Context())
	receiver := httpapi.NewServer(st)
	receiver.GitHubWebhookApplier = applier
	deliver := func(id, event, body string) int {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(replaySecret))
		mac.Write([]byte(body))
		req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook/demo", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		receiver.Handler().ServeHTTP(w, req)
		applier.Wait()
		return w.Code
	}
	state := func(id string) core.TaskState {
		t.Helper()
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return task.State
	}

	if code := deliver("d-external-merge", "pull_request", recordedExternalMerge); code != http.StatusAccepted || state("task-a") != core.TaskMerged {
		t.Fatalf("external merge: code=%d state=%s", code, state("task-a"))
	}
	if count, _ := st.CountEvents(ctx, "task-a", "merge.reconciled"); count != 1 {
		t.Fatalf("task-a merge.reconciled=%d", count)
	}
	if code := deliver("d-pending", "status", recordedPendingStatus); code != http.StatusAccepted || state("task-b") != core.TaskApproved || forge.merges[42] != 0 {
		t.Fatalf("pending status: code=%d state=%s merges=%v", code, state("task-b"), forge.merges)
	}
	// The refusal is a deferral: recorded once and not retried.
	if count, _ := st.CountEvents(ctx, "task-b", "merge.failed"); count != 1 {
		t.Fatalf("pending status merge.failed=%d, want one recorded refusal", count)
	}
	forge.mu.Lock()
	forge.readyB = true
	forge.mu.Unlock()
	if code := deliver("d-green", "check_suite", recordedGreenSuite); code != http.StatusAccepted || state("task-b") != core.TaskMerged || forge.merges[42] != 1 {
		t.Fatalf("green suite: code=%d state=%s merges=%v", code, state("task-b"), forge.merges)
	}
	viewsBefore := forge.views["conveyor/task-b"]
	if code := deliver("d-other", "status", recordedOtherStatus); code != http.StatusAccepted || forge.views["conveyor/task-b"] != viewsBefore {
		t.Fatalf("unrelated status: code=%d views=%v", code, forge.views)
	}
	if code := deliver("d-push-main", "push", recordedBasePush); code != http.StatusAccepted || forge.monitorN != 1 {
		t.Fatalf("base push: code=%d monitor polls=%d", code, forge.monitorN)
	}
	if code := deliver("d-push-branch", "push", recordedBranchPush); code != http.StatusAccepted || forge.monitorN != 1 {
		t.Fatalf("branch push: code=%d monitor polls=%d", code, forge.monitorN)
	}
	if code := deliver("d-ready", "issues", recordedReadyLabel); code != http.StatusAccepted {
		t.Fatalf("ready label: code=%d", code)
	}
	tasks, err := st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/app#7"})
	if err != nil || len(tasks) != 1 || tasks[0].Title != "Add a health page" || tasks[0].Repo != "app" {
		t.Fatalf("intake tasks=%+v err=%v", tasks, err)
	}
	if forge.restCalls("PATCH /repos/acme/app/issues/7")+forge.restCalls("POST /repos/acme/app/issues/7/labels")+forge.restCalls("DELETE /repos/acme/app/issues/7/labels") == 0 {
		t.Fatalf("issue labels not moved: %v", forge.rest)
	}
	// Every delivery is recorded once; a redelivery applies nothing.
	restBefore := len(forge.rest)
	if code := deliver("d-ready", "issues", recordedReadyLabel); code != http.StatusOK {
		t.Fatalf("duplicate delivery: %d", code)
	}
	if again, _ := st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/app#7"}); len(again) != 1 || len(forge.rest) != restBefore {
		t.Fatalf("duplicate delivery applied: tasks=%d rest=%d->%d", len(again), restBefore, len(forge.rest))
	}
	if len(delivered) != 7 || delivered[0] != "demo" {
		t.Fatalf("fallback notified %v", delivered)
	}
}

// TestIssueIntakePollRunsOnlyWithFlag: the default -poll-github of zero
// starts no timed intake and reads no issue; a positive interval polls every
// workspace at start and on each tick (req-quiet-idle-runtime AC-3.7;
// component-runtime, Timed issue intake).
func TestIssueIntakePollRunsOnlyWithFlag(t *testing.T) {
	flags := flag.NewFlagSet("conveyord", flag.ContinueOnError)
	values := registerDaemonFlags(flags)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	polled := []string{}
	poll := func(ctx context.Context) {
		workspace, _ := store.WorkspaceFromContext(ctx)
		mu.Lock()
		polled = append(polled, workspace)
		mu.Unlock()
	}
	workspaces := func(context.Context) ([]string, error) { return []string{"a", "b"}, nil }
	tickerCalls := 0
	ticker := func(time.Duration) (<-chan time.Time, func()) {
		tickerCalls++
		return make(chan time.Time), func() {}
	}
	if started := startIssueIntakePoll(t.Context(), *values.pollGitHub, workspaces, poll, ticker, t.Logf); started || tickerCalls != 0 || len(polled) != 0 {
		t.Fatalf("default flag started=%t ticker=%d polled=%v", started, tickerCalls, polled)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	if !startIssueIntakePoll(ctx, time.Minute, workspaces, poll, func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { close(stopped) }
	}, t.Logf) {
		t.Fatal("positive interval did not start")
	}
	ticks <- time.Now()
	// The second send completes only after the poll goroutine has run the
	// start-up pass and the first tick's pass.
	ticks <- time.Now()
	cancel()
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if len(polled) < 4 || polled[0] != "a" || polled[1] != "b" {
		t.Fatalf("polled=%v", polled)
	}
}

// retryStore fails the first merge.requested append when armed.
type retryStore struct {
	store.Backend
	failMergeRequest atomic.Bool
}

func (s *retryStore) AppendEvent(ctx context.Context, event core.Event) error {
	if event.Kind == "merge.requested" && s.failMergeRequest.CompareAndSwap(true, false) {
		return errors.New("database unavailable")
	}
	return s.Backend.AppendEvent(ctx, event)
}

type retryFixture struct {
	t           *testing.T
	st          store.Backend
	ctx         context.Context
	receiver    *httpapi.Server
	applier     *githubtrigger.WebhookApplier
	failView    atomic.Bool
	failRunner  atomic.Bool
	failLabels  atomic.Bool
	store       *retryStore
	mu          sync.Mutex
	views       int
	merges      int
	merged      bool
	runnerCalls int
	labelMoves  int
	lines       []string
}

// newRetryFixture wires the real receiver, applier, dispatcher, and monitor
// poller on the volatile backend against a fake forge, with the applier's
// retry waits removed so no test waits real seconds.
func newRetryFixture(t *testing.T) *retryFixture {
	t.Helper()
	f := &retryFixture{t: t}
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	f.st = backend
	f.store = &retryStore{Backend: backend}
	f.ctx = store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", GitHub: "acme/app", Base: "main"}}}
	cfg.Monitor.Enabled = true
	cfg.Monitor.Repositories = []string{"app"}
	cfg.Monitor.PollInterval = time.Minute
	cfg.Monitor.StartupWindow = time.Hour
	if _, err := backend.BootstrapWorkspaceConfig(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	backend.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{41}, 32))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-demo", ClientID: "client"}, PrivateKey: private, WebhookSecret: replaySecret}
	if _, err = backend.StoreWorkspaceGitHubApp(f.ctx, "demo", app); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.RecordWorkspaceGitHubAppInstallation(f.ctx, "demo", 41, 12, "acme"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"acme"}}`)
		case r.URL.Path == "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "retry-installation-token", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case r.URL.Path == "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"acme/app"}]}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/issues/7"):
			if f.failLabels.CompareAndSwap(true, false) {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"message":"bad gateway"}`)
				return
			}
			f.mu.Lock()
			f.labelMoves++
			f.mu.Unlock()
			fmt.Fprint(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/"):
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected GitHub endpoint %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
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

	d := dispatch.New(f.store, cfg, nil)
	d.DisableMemoryQueueForTest()
	d.WorkspaceGitHubApps = backend
	d.GitHubApps = githubtrigger.NewAppClient(server.Client(), server.URL)
	d.ReadCommitChecks = dispatch.NoChecksReportedForTest
	d.ViewPullRequest = func(_ context.Context, _, branch string) (githubtrigger.PullRequest, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.views++
		if f.failView.CompareAndSwap(true, false) {
			return githubtrigger.PullRequest{}, githubtrigger.StatusError(http.StatusBadGateway, errors.New("GitHub REST status 502"))
		}
		if f.merged {
			return githubtrigger.PullRequest{Number: 42, State: "closed", Merged: true, HeadSHA: "head-b", HeadRef: branch, BaseSHA: "base-42", MergeCommitSHA: "landed-42"}, nil
		}
		return githubtrigger.PullRequest{Number: 42, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head-b", HeadRef: branch, BaseSHA: "base-42"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.merges++
		f.merged = true
		return nil
	}
	service := &monitor.Service{Store: backend, WorkspaceID: "demo", Enabled: true, Repositories: map[string]struct{}{"app": {}}, Intake: func(context.Context, monitor.TaskRequest) (monitor.IntakeResult, error) {
		t.Error("monitor filed a task")
		return monitor.IntakeResult{}, nil
	}}
	polls := &monitorPoller{Store: backend, Dispatcher: d, Service: service,
		Config: func(context.Context) (*config.Config, error) { return cfg, nil },
		Runner: func(context.Context, string, string) (func(context.Context, ...string) ([]byte, error), error) {
			f.mu.Lock()
			f.runnerCalls++
			f.mu.Unlock()
			if f.failRunner.CompareAndSwap(true, false) {
				return nil, githubtrigger.StatusError(http.StatusBadGateway, errors.New("installation token unavailable"))
			}
			return func(_ context.Context, args ...string) ([]byte, error) { return quietMonitorRead(args...), nil }, nil
		},
	}
	f.applier = newGitHubWebhookApplier(backend, d, polls.PushedRef, polls.ChecksCompleted, func(string) {}, func(format string, args ...any) {
		f.mu.Lock()
		f.lines = append(f.lines, fmt.Sprintf(format, args...))
		f.mu.Unlock()
	})
	f.applier.Sleep = func(context.Context, time.Duration) error { return nil }
	f.applier.Start(t.Context())
	f.receiver = httpapi.NewServer(backend)
	f.receiver.GitHubWebhookApplier = f.applier
	return f
}

func (f *retryFixture) seedApprovedTask() {
	f.t.Helper()
	task := core.Task{ID: "task-b", Workspace: "demo", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-b", State: core.TaskApproved, CreatedAt: time.Now().UTC()}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.BindTaskApproval(f.ctx, task.ID, "head-b"); err != nil {
		f.t.Fatal(err)
	}
}

// deliver posts one signed delivery and waits for its application,
// retries included.
func (f *retryFixture) deliver(id, event, body string) {
	f.t.Helper()
	mac := hmac.New(sha256.New, []byte(replaySecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook/demo", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", event)
	w := httptest.NewRecorder()
	f.receiver.Handler().ServeHTTP(w, req)
	f.applier.Wait()
	if w.Code != http.StatusAccepted {
		f.t.Fatalf("delivery %s: %d %s", id, w.Code, w.Body.String())
	}
}

func (f *retryFixture) taskState(id string) core.TaskState {
	f.t.Helper()
	task, err := f.st.GetTask(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return task.State
}

func (f *retryFixture) notApplied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, line := range f.lines {
		if strings.Contains(line, "not applied") {
			out = append(out, line)
		}
	}
	return out
}

// TestGitHubWebhookRetriesFailedApplications: a one-time failure while
// applying a delivery is returned to the applier, which retries it, so with
// no later delivery the final green check suite still merges its task once
// after a GitHub 5xx and after a store failure, a push still polls its
// repository after a runner failure, and a conveyor:ready label still
// finishes intake of exactly one task after a failed label move
// (req-quiet-idle-runtime AC-3.1; component-runtime, GitHub webhook
// applier; component-submission-merge, Merge readiness from webhook
// deliveries).
func TestGitHubWebhookRetriesFailedApplications(t *testing.T) {
	t.Run("GitHub 5xx on the final green check suite", func(t *testing.T) {
		f := newRetryFixture(t)
		f.seedApprovedTask()
		f.failView.Store(true)
		f.deliver("green-suite", "check_suite", recordedGreenSuite)
		if f.taskState("task-b") != core.TaskMerged || f.merges != 1 || f.views < 2 {
			t.Fatalf("state=%s merges=%d views=%d", f.taskState("task-b"), f.merges, f.views)
		}
		if lines := f.notApplied(); len(lines) != 0 {
			t.Fatalf("retried delivery reported as not applied: %v", lines)
		}
	})
	t.Run("store failure on the final green check suite", func(t *testing.T) {
		f := newRetryFixture(t)
		f.seedApprovedTask()
		f.store.failMergeRequest.Store(true)
		f.deliver("green-suite", "check_suite", recordedGreenSuite)
		if f.taskState("task-b") != core.TaskMerged || f.merges != 1 || f.store.failMergeRequest.Load() {
			t.Fatalf("state=%s merges=%d", f.taskState("task-b"), f.merges)
		}
		if count, _ := f.st.CountEvents(f.ctx, "task-b", "merge.requested"); count != 1 {
			t.Fatalf("merge.requested=%d", count)
		}
	})
	t.Run("runner failure on a base push", func(t *testing.T) {
		f := newRetryFixture(t)
		f.failRunner.Store(true)
		f.deliver("base-push", "push", recordedBasePush)
		if f.runnerCalls != 2 || len(f.notApplied()) != 0 {
			t.Fatalf("runner calls=%d lines=%v, want one failure and one retried poll", f.runnerCalls, f.lines)
		}
		status, err := f.st.MonitorStatus(f.ctx, true, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if status.CurrentError != "" || status.LastSuccessfulAt.IsZero() {
			t.Fatalf("the retried poll did not succeed after the recorded failure: error=%q last=%s", status.CurrentError, status.LastSuccessfulAt)
		}
	})
	t.Run("label move failure on a ready issue", func(t *testing.T) {
		f := newRetryFixture(t)
		f.failLabels.Store(true)
		f.deliver("ready-label", "issues", recordedReadyLabel)
		tasks, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{Query: "github:acme/app#7"})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("intake tasks=%+v err=%v", tasks, err)
		}
		if tasks[0].State == core.TaskClaiming || f.labelMoves == 0 || len(f.notApplied()) != 0 {
			t.Fatalf("intake not resumed: state=%s label moves=%d lines=%v", tasks[0].State, f.labelMoves, f.lines)
		}
	})
}
