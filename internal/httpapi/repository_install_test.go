package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func installationServer(t *testing.T) (*Server, *fakeWorkspaceConfigStore, context.Context) {
	t.Helper()
	backend := store.NewMemory()
	s := NewServer(backend)
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	f := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: contextualWorkspaceDocument(), Version: 1}}
	s.ConfigStore = f
	s.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Repos: f.record.Document.Repos, Execution: f.record.Document.Execution}, nil
	}
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Install Conveyor", nil }
	return s, f, store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "registrar", Role: core.ActorUser})
}

func saveInstallation(t *testing.T, s *Server, f *fakeWorkspaceConfigStore, ctx context.Context, repo map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	document := f.record.Document
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["repos"] = []any{repo}
	data, err = json.Marshal(map[string]any{"document": raw})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/v1/workspaces/demo/config", strings.NewReader(string(data))).WithContext(ctx)
	r.Header.Set("If-Match", fmt.Sprint(f.record.Version))
	w := httptest.NewRecorder()
	s.putWorkspaceConfig(w, r)
	return w
}

func TestRepositoryRegistrationValidationAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, url, slug, want string
		off, reject           bool
	}{
		{name: "https", url: "https://github.com/Owner/Repo.git", want: "owner/repo"},
		{name: "ssh", url: "git@github.com:Owner/Repo.git", want: "owner/repo"},
		{name: "explicit off", url: "https://github.com/owner/repo", want: "owner/repo", off: true},
		{name: "mismatch", url: "https://github.com/owner/repo", slug: "other/repo", want: "owner/repo", reject: true},
		{name: "non github", url: "https://git.example/owner/repo", slug: "ignored/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, ctx := installationServer(t)
			repo := map[string]any{"name": "conveyor", "url": tc.url, "base": "develop", "github": tc.slug}
			if tc.off {
				repo["install_conveyor"] = false
			}
			w := saveInstallation(t, s, f, ctx, repo)
			if tc.reject {
				if w.Code != 422 || !strings.Contains(w.Body.String(), tc.slug) || !strings.Contains(w.Body.String(), tc.want) || f.updates != 0 {
					t.Fatalf("mismatch response=%d %s", w.Code, w.Body)
				}
				return
			}
			if w.Code != 200 {
				t.Fatalf("save=%d %s", w.Code, w.Body)
			}
			stored := f.record.Document.Repos[0]
			if stored.GitHub != tc.want || stored.InstallConveyor == nil || stored.InstallEnabled() == tc.off {
				t.Fatalf("stored=%+v", stored)
			}
			tasks, err := s.Store.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.off {
				want = 0
			}
			if len(tasks) != want {
				t.Fatalf("tasks=%d", len(tasks))
			}
			if want == 1 {
				task := tasks[0]
				if task.Source != core.RepositoryRegistrationSource || task.NextStage != core.StageTriage || task.BaseBranch != "develop" || !task.SpecApproval || !task.MergeApproval {
					t.Fatalf("task=%+v", task)
				}
				for _, text := range []string{"conveyor", "develop", "conveyor repo init", "task worktree", "commit, push, and submit for review"} {
					if !strings.Contains(task.Body, text) {
						t.Fatalf("body=%s", task.Body)
					}
				}
				events, err := s.Store.ListEvents(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(events) == 0 || events[0].ActorID != "registrar" {
					t.Fatalf("events=%+v", events)
				}
			}
		})
	}
}

func TestRepositoryRegistrationRetryAndRead(t *testing.T) {
	s, f, ctx := installationServer(t)
	repo := map[string]any{"name": "conveyor", "url": "https://github.com/owner/repo", "base": "main"}
	for i := 0; i < 2; i++ {
		w := saveInstallation(t, s, f, ctx, repo)
		if w.Code != 200 {
			t.Fatalf("save=%d %s", w.Code, w.Body)
		}
	}
	attempts, err := s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts=%+v", attempts)
	}
	first := attempts[0].TaskID
	w := httptest.NewRecorder()
	s.getWorkspaceConfig(w, httptest.NewRequest(http.MethodGet, "/config", nil).WithContext(ctx))
	if w.Code != 200 || !strings.Contains(w.Body.String(), first) || !strings.Contains(w.Body.String(), `"install_conveyor":true`) || !strings.Contains(w.Body.String(), `"state":"queued"`) {
		t.Fatalf("read=%d %s", w.Code, w.Body)
	}
	_, err = taskops.New(s.Store).Cancel(ctx, core.Intervention{TaskID: first, Action: core.InterventionCancel, ReasonCode: "operator_cancel"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w := saveInstallation(t, s, f, ctx, repo)
		if w.Code != 200 {
			t.Fatalf("refile=%d %s", w.Code, w.Body)
		}
	}
	attempts, err = s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[1].Attempt != 2 || attempts[1].TaskID == first {
		t.Fatalf("attempts=%+v", attempts)
	}
	a, err := s.Store.GetTask(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Store.GetTask(ctx, attempts[1].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if a.IntakeKey == b.IntakeKey || b.IntakeKey != "repo-install-demo-conveyor-2" {
		t.Fatal("retry key reused")
	}
}

func TestRepositoryRegistrationRecoversIntakeFailure(t *testing.T) {
	s, f, ctx := installationServer(t)
	repo := map[string]any{"name": "conveyor", "url": "https://github.com/owner/repo", "base": "main"}
	title := s.GenerateTaskTitle
	s.GenerateTaskTitle = nil
	w := saveInstallation(t, s, f, ctx, repo)
	if w.Code != 503 || f.updates != 1 || !strings.Contains(w.Body.String(), "repository_install_pending") {
		t.Fatalf("response=%d %s", w.Code, w.Body)
	}
	s.GenerateTaskTitle = title
	w = saveInstallation(t, s, f, ctx, repo)
	if w.Code != 200 {
		t.Fatalf("retry=%d %s", w.Code, w.Body)
	}
	attempts, err := s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Attempt != 1 {
		t.Fatalf("attempts=%+v", attempts)
	}
}

func TestRepositoryRegistrationOffThenOnAndMergedReuse(t *testing.T) {
	s, f, ctx := installationServer(t)
	repo := map[string]any{"name": "conveyor", "url": "https://github.com/owner/repo", "base": "main", "install_conveyor": false}
	w := saveInstallation(t, s, f, ctx, repo)
	if w.Code != 200 {
		t.Fatalf("off=%d %s", w.Code, w.Body)
	}
	tasks, err := s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatal("off filed a task")
	}
	repo["install_conveyor"] = true
	w = saveInstallation(t, s, f, ctx, repo)
	if w.Code != 200 {
		t.Fatalf("on=%d %s", w.Code, w.Body)
	}
	tasks, err = s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatal("on did not file one task")
	}

	s, f, ctx = installationServer(t)
	merged := core.Task{ID: "already-installed", Workspace: "demo", Repo: "conveyor", Source: core.RepositoryRegistrationSource, RepositoryInstallAttempt: 1, IntakeKey: store.RepositoryInstallKey("demo", "conveyor", 1), Title: "Install Conveyor", Branch: "conveyor/task-already-installed", BaseBranch: "main", State: core.TaskMerged}
	if err = s.Store.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	w = saveInstallation(t, s, f, ctx, repo)
	if w.Code != 200 {
		t.Fatalf("merged=%d %s", w.Code, w.Body)
	}
	tasks, err = s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].TaskID != merged.ID {
		t.Fatalf("merged task refiled: %+v", tasks)
	}
}

func TestRepositoryRegistrationConcurrentSave(t *testing.T) {
	s, f, ctx := installationServer(t)
	repo := map[string]any{"name": "conveyor", "url": "https://github.com/owner/repo", "base": "main"}
	started, finish := make(chan struct{}), make(chan struct{})
	count := 0
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		count++
		close(started)
		<-finish
		return "Install Conveyor", nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- saveInstallation(t, s, f, ctx, repo) }()
	<-started
	// The first write has committed its configuration but not yet its task.
	second := make(chan *httptest.ResponseRecorder, 1)
	data, err := json.Marshal(map[string]any{"document": f.record.Document})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(string(data))).WithContext(ctx)
	r.Header.Set("If-Match", fmt.Sprint(f.record.Version))
	go func() { w := httptest.NewRecorder(); s.putWorkspaceConfig(w, r); second <- w }()
	close(finish)
	for _, ch := range []chan *httptest.ResponseRecorder{first, second} {
		w := <-ch
		if w.Code != 200 {
			t.Fatalf("save=%d %s", w.Code, w.Body)
		}
	}
	tasks, err := s.Store.ListRepositoryInstallTasks(ctx, "conveyor")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || count != 1 {
		t.Fatalf("tasks=%+v titles=%d", tasks, count)
	}
}
