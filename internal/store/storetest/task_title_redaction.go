package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const (
	// Obviously fake values that still match the real shapes. The App key is
	// stored, so an exact removal proves the store's secret source was read.
	titleFixtureAppKey   = "conformance-title-fixture-app-key-material"
	titleFixturePattern  = "ghp_fixturefixturefixturefixturefixture01"
	titleFixtureBoundary = "title-fixture-boundary"
)

// titleFixtureSecrets is one title holding one secret of each class it tests.
var (
	titleFixtureText  = "Fix login for " + titleFixtureAppKey + " and " + titleFixturePattern
	titleFixtureCount = core.RedactionStats{Exact: 1, Pattern: 1}
)

// titleRedactionFixture holds one workspace whose stored GitHub App key the
// redactor must read, and whose read can be made to fail.
type titleRedactionFixture struct {
	x   Fixture
	st  store.Backend
	ctx context.Context
	key []byte
}

func newTitleRedactionFixture(t *testing.T, x Fixture) *titleRedactionFixture {
	t.Helper()
	if x.TaskIntakeHTTP == nil || x.GitHubIssueIntake == nil {
		t.Fatal("conformance fixture must provide TaskIntakeHTTP and GitHubIssueIntake for the task title cases")
	}
	ctx, owner := bootstrapOwner(t, x)
	ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	f := &titleRedactionFixture{x: x, st: x.Backend, ctx: ctx, key: bytes.Repeat([]byte{51}, 32)}
	f.st.ConfigureGitHubAppKeyEncryptionKey(f.key)
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 51, AppSlug: "title-fixture", ClientID: "title-client"}, PrivateKey: titleFixtureAppKey}
	_, err := f.st.StoreWorkspaceGitHubApp(ctx, x.Workspace, app)
	requireOK(t, err)
	t.Cleanup(func() {
		f.st.ConfigureGitHubAppKeyEncryptionKey(f.key)
		if err := f.st.DeleteWorkspaceGitHubApp(ctx, x.Workspace); err != nil {
			t.Errorf("delete title fixture app: %v", err)
		}
		f.st.ConfigureGitHubAppKeyEncryptionKey(nil)
	})
	return f
}

// withFailedSecretRead runs fn while ListGitHubAppKeysForRedaction fails.
func (f *titleRedactionFixture) withFailedSecretRead(t *testing.T, fn func()) {
	t.Helper()
	f.st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{52}, 32))
	defer f.st.ConfigureGitHubAppKeyEncryptionKey(f.key)
	if _, err := f.st.ListGitHubAppKeysForRedaction(f.ctx); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("the fixture's secret read did not fail: %v", err)
	}
	fn()
}

// titleViolation reports what a task's stored title and event history expose.
// It names the kind of failure and never echoes a stored value.
func titleViolation(ctx context.Context, st store.Store, taskID string, want core.RedactionStats) error {
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		return err
	}
	for _, secret := range []string{titleFixtureAppKey, titleFixturePattern} {
		if strings.Contains(task.Title, secret) {
			return errors.New("the stored title holds a secret")
		}
		for _, event := range events {
			if strings.Contains(string(event.Payload), secret) {
				return fmt.Errorf("event %s holds a secret", event.Kind)
			}
		}
	}
	for _, event := range events {
		if event.Kind != "task.created" {
			continue
		}
		var created store.TaskCreatedEvent
		if err := json.Unmarshal(event.Payload, &created); err != nil {
			return err
		}
		got := core.RedactionStats{}
		if created.Redactions != nil {
			got = *created.Redactions
		}
		if got != want {
			return fmt.Errorf("task.created redactions = %+v, want %+v", got, want)
		}
		return nil
	}
	return errors.New("the task has no task.created event")
}

// requireTitleCheckCanFail is the control each path runs: a task stored raw,
// as the path's writer stored it before redaction, fails the same check that
// the path must pass, and a clean title with no count passes it.
func (f *titleRedactionFixture) requireTitleCheckCanFail(t *testing.T) {
	t.Helper()
	create := func(title string) string {
		id := core.NewTaskID()
		requireOK(t, f.st.CreateTask(f.ctx, core.Task{ID: id, Workspace: f.x.Workspace, Source: "title-control", Title: title, Body: "Control", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}))
		return id
	}
	if titleViolation(f.ctx, f.st, create(titleFixtureText), titleFixtureCount) == nil {
		t.Fatal("control: the check accepted a raw stored title")
	}
	clean := create("Plain control title")
	if err := titleViolation(f.ctx, f.st, clean, core.RedactionStats{}); err != nil {
		t.Fatalf("control: the check refused a clean title: %v", err)
	}
	if titleViolation(f.ctx, f.st, clean, titleFixtureCount) == nil {
		t.Fatal("control: the check accepted a missing count")
	}
}

func (f *titleRedactionFixture) request(t *testing.T, handler http.Handler, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func (f *titleRedactionFixture) postJSON(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, handler, "application/json", core.JSONPayload(map[string]any{"body": body, "repo": "conveyor"}))
}

func (f *titleRedactionFixture) postKeyed(t *testing.T, handler http.Handler, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	task := core.JSONPayload(map[string]any{"body": body, "repo": "conveyor"})
	var form strings.Builder
	for _, part := range [][2]string{{"task", string(task)}, {"idempotency_key", key}} {
		form.WriteString("--" + titleFixtureBoundary + "\r\nContent-Disposition: form-data; name=\"" + part[0] + "\"\r\n\r\n" + part[1] + "\r\n")
	}
	form.WriteString("--" + titleFixtureBoundary + "--\r\n")
	return f.request(t, handler, "multipart/form-data; boundary="+titleFixtureBoundary, []byte(form.String()))
}

func createdTaskID(t *testing.T, response *httptest.ResponseRecorder, want int) string {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d", response.Code, want)
	}
	var task core.Task
	requireOK(t, json.Unmarshal(response.Body.Bytes(), &task))
	if strings.Contains(task.Title, titleFixtureAppKey) || strings.Contains(task.Title, titleFixturePattern) {
		t.Fatal("the response title holds a secret")
	}
	return task.ID
}

// runTaskTitleRedaction proves every task title path stores its title
// redacted and records the removal on task.created by count and class
// (req-security-boundaries REQ-3, AC-3.1; DEC-8).
func runTaskTitleRedaction(t *testing.T, x Fixture) {
	t.Run("RESTGeneratedTitle", func(t *testing.T) { runRESTTitleRedaction(t, x) })
	t.Run("GitHubIssueTitle", func(t *testing.T) { runGitHubTitleRedaction(t, x) })
	t.Run("BlueprintChildTitle", func(t *testing.T) { runBlueprintTitleRedaction(t, x) })
	t.Run("StartOverTitle", func(t *testing.T) { runStartOverTitleRedaction(t, x) })
}

func runRESTTitleRedaction(t *testing.T, x Fixture) {
	f := newTitleRedactionFixture(t, x)
	f.requireTitleCheckCanFail(t)
	var generated atomic.Int64
	title := titleFixtureText
	handler := x.TaskIntakeHTTP(t, func(context.Context, core.Task) (string, error) {
		generated.Add(1)
		return title, nil
	})
	body := "The body keeps " + titleFixturePattern + " as submitted."

	t.Run("JSON", func(t *testing.T) {
		title = titleFixtureText
		id := createdTaskID(t, f.postJSON(t, handler, body), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, id, titleFixtureCount))
		task, err := f.st.GetTask(f.ctx, id)
		requireOK(t, err)
		if task.Body != body {
			t.Fatal("the body did not stay as submitted")
		}
	})
	t.Run("CleanTitleKeepsItsTextAndRecordsNoCount", func(t *testing.T) {
		title = "Plain generated title"
		id := createdTaskID(t, f.postJSON(t, handler, "A plain body"), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, id, core.RedactionStats{}))
		task, err := f.st.GetTask(f.ctx, id)
		requireOK(t, err)
		if task.Title != title {
			t.Fatal("a clean title changed")
		}
	})
	t.Run("PlaceholderStaysWithinTheBound", func(t *testing.T) {
		title = strings.Repeat("x", 200-len(titleFixturePattern)-1) + " " + titleFixturePattern
		id := createdTaskID(t, f.postJSON(t, handler, "Bounded title"), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, id, core.RedactionStats{Pattern: 1}))
		task, err := f.st.GetTask(f.ctx, id)
		requireOK(t, err)
		if len(task.Title) > 200 {
			t.Fatalf("title holds %d bytes", len(task.Title))
		}
		// A short secret's placeholder can outgrow it, so the bound applies again.
		title = strings.Repeat("y", 150) + " " + titleFixtureAppKey
		id = createdTaskID(t, f.postJSON(t, handler, "Lengthened title"), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, id, core.RedactionStats{Exact: 1}))
	})
	t.Run("IdempotentRetryAnswersTheOriginalTask", func(t *testing.T) {
		title = titleFixtureText
		key := "title-retry-" + core.NewTaskID()
		original := createdTaskID(t, f.postKeyed(t, handler, body, key), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, original, titleFixtureCount))
		before := generated.Load()
		if got := createdTaskID(t, f.postKeyed(t, handler, body, key), http.StatusOK); got != original {
			t.Fatal("the retry answered a different task")
		}
		f.withFailedSecretRead(t, func() {
			if got := createdTaskID(t, f.postKeyed(t, handler, body, key), http.StatusOK); got != original {
				t.Fatal("the retry during a failed secret read answered a different task")
			}
		})
		if generated.Load() != before {
			t.Fatal("a retry generated a new title")
		}
		if response := f.postKeyed(t, handler, body+" changed", key); response.Code != http.StatusConflict {
			t.Fatalf("a different body under the key answered %d", response.Code)
		}
		requireOK(t, titleViolation(f.ctx, f.st, original, titleFixtureCount))
	})
	t.Run("FailedSecretReadCreatesNoTask", func(t *testing.T) {
		title = titleFixtureText
		key := "title-refused-" + core.NewTaskID()
		f.withFailedSecretRead(t, func() {
			for name, response := range map[string]*httptest.ResponseRecorder{
				"keyed": f.postKeyed(t, handler, body, key),
				"json":  f.postJSON(t, handler, "Refused "+key),
			} {
				if response.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status = %d, want 503", name, response.Code)
				}
				text := response.Body.String()
				if strings.Contains(text, titleFixturePattern) || strings.Contains(text, titleFixtureAppKey) || strings.Contains(text, "decrypt") {
					t.Fatalf("%s: the refusal carries stored or store text", name)
				}
			}
		})
		if _, found, err := f.st.GetTaskByIntakeKey(f.ctx, key); err != nil || found {
			t.Fatalf("a refused request stored a task: found=%v err=%v", found, err)
		}
		tasks, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{Query: "Refused " + key})
		requireOK(t, err)
		if len(tasks) != 0 {
			t.Fatalf("a refused request stored %d task(s)", len(tasks))
		}
		// The refusal loses nothing: the same request succeeds once secrets read.
		id := createdTaskID(t, f.postKeyed(t, handler, body, key), http.StatusCreated)
		requireOK(t, titleViolation(f.ctx, f.st, id, titleFixtureCount))
	})
}

func runGitHubTitleRedaction(t *testing.T, x Fixture) {
	f := newTitleRedactionFixture(t, x)
	f.requireTitleCheckCanFail(t)
	repo := x.Config.Repos[0]
	repo.GitHub = "acme/title-fixture"
	ctx := store.WithActor(f.ctx, store.SystemActor("dispatcher"))
	source := func(number int) string { return fmt.Sprintf("github:%s#%d", repo.GitHub, number) }
	task := func(t *testing.T, number int) (core.Task, bool) {
		t.Helper()
		tasks, err := f.st.ListTasksFiltered(ctx, store.TaskFilter{Query: source(number)})
		requireOK(t, err)
		for _, candidate := range tasks {
			if candidate.Source == source(number) {
				return candidate, true
			}
		}
		return core.Task{}, false
	}
	body := "The issue body keeps " + titleFixturePattern + " as submitted."

	t.Run("TitleAndEvent", func(t *testing.T) {
		requireOK(t, x.GitHubIssueIntake(ctx, repo, github.Issue{Number: 1, Title: titleFixtureText, Body: body}))
		stored, found := task(t, 1)
		if !found {
			t.Fatal("the issue created no task")
		}
		requireOK(t, titleViolation(ctx, f.st, stored.ID, titleFixtureCount))
		if stored.Body != body {
			t.Fatal("the body did not stay as submitted")
		}
	})
	t.Run("CleanTitleKeepsItsTextAndRecordsNoCount", func(t *testing.T) {
		requireOK(t, x.GitHubIssueIntake(ctx, repo, github.Issue{Number: 2, Title: "Plain issue title", Body: "Plain"}))
		stored, found := task(t, 2)
		if !found || stored.Title != "Plain issue title" {
			t.Fatalf("clean issue found=%v", found)
		}
		requireOK(t, titleViolation(ctx, f.st, stored.ID, core.RedactionStats{}))
	})
	t.Run("FailedSecretReadCreatesNoTask", func(t *testing.T) {
		f.withFailedSecretRead(t, func() {
			err := x.GitHubIssueIntake(ctx, repo, github.Issue{Number: 3, Title: titleFixtureText, Body: body})
			if !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
				t.Fatalf("intake err = %v", err)
			}
		})
		if _, found := task(t, 3); found {
			t.Fatal("a failed secret read created a task")
		}
		// The next poll or delivery takes the same issue in.
		requireOK(t, x.GitHubIssueIntake(ctx, repo, github.Issue{Number: 3, Title: titleFixtureText, Body: body}))
		stored, found := task(t, 3)
		if !found {
			t.Fatal("the retried issue created no task")
		}
		requireOK(t, titleViolation(ctx, f.st, stored.ID, titleFixtureCount))
	})
}

func runBlueprintTitleRedaction(t *testing.T, x Fixture) {
	f := newTitleRedactionFixture(t, x)
	f.requireTitleCheckCanFail(t)
	repo := x.Config.Repos[0].Name
	parent := func(t *testing.T) core.Task {
		t.Helper()
		task := blueprintParent(x.Workspace)
		task.Repo = repo
		requireOK(t, f.st.CreateTask(f.ctx, task))
		return task
	}
	longSummary := strings.Repeat("界", 60) + " " + titleFixtureAppKey

	t.Run("ChildTitlesAndEvents", func(t *testing.T) {
		task := parent(t)
		spec := createBlueprintSpec(t, f.ctx, f.st, task.ID, []blueprintItem{
			{ID: "SUB-1", Repo: repo, Summary: titleFixtureText},
			{ID: "SUB-2", Repo: repo, Summary: "Plain child summary"},
			{ID: "SUB-3", Repo: repo, Summary: longSummary},
		})
		children := tasksByOrigin(approveBlueprint(t, f.ctx, f.st, task.ID, spec.Version))
		requireOK(t, titleViolation(f.ctx, f.st, children["SUB-1"].ID, titleFixtureCount))
		requireOK(t, titleViolation(f.ctx, f.st, children["SUB-2"].ID, core.RedactionStats{}))
		requireOK(t, titleViolation(f.ctx, f.st, children["SUB-3"].ID, core.RedactionStats{Exact: 1}))
		stored, err := f.st.GetTask(f.ctx, children["SUB-1"].ID)
		requireOK(t, err)
		if !strings.Contains(stored.Body, titleFixturePattern) {
			t.Fatal("the child's body did not stay as submitted")
		}
		plain, err := f.st.GetTask(f.ctx, children["SUB-2"].ID)
		requireOK(t, err)
		if plain.Title != "Plain child summary" {
			t.Fatal("a clean child title changed")
		}
		long, err := f.st.GetTask(f.ctx, children["SUB-3"].ID)
		requireOK(t, err)
		if len(long.Title) > 200 {
			t.Fatalf("child title holds %d bytes", len(long.Title))
		}
		// Re-approval reuses each child and appends no second count.
		before, err := f.st.ListEvents(f.ctx, children["SUB-1"].ID)
		requireOK(t, err)
		approveBlueprint(t, f.ctx, f.st, task.ID, spec.Version)
		after, err := f.st.ListEvents(f.ctx, children["SUB-1"].ID)
		requireOK(t, err)
		if len(after) != len(before) {
			t.Fatal("re-approval appended to a child's history")
		}
	})
	t.Run("FailedSecretReadRefusesAndStoresNoTitle", func(t *testing.T) {
		task := parent(t)
		spec := createBlueprintSpec(t, f.ctx, f.st, task.ID, []blueprintItem{{ID: "SUB-1", Repo: repo, Summary: titleFixtureText}})
		f.withFailedSecretRead(t, func() {
			if _, err := f.st.ApproveSpecVersionAndMaterialize(f.ctx, task.ID, spec.Version); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
				t.Fatalf("approval err = %v", err)
			}
		})
		events, err := f.st.ListEvents(f.ctx, task.ID)
		requireOK(t, err)
		for _, event := range events {
			if event.Kind == "spec.version_approved" || event.Kind == "blueprint.materialized" {
				t.Fatalf("a refused approval recorded %s", event.Kind)
			}
		}
		// The operator's approval is not lost: it succeeds once secrets read.
		children := approveBlueprint(t, f.ctx, f.st, task.ID, spec.Version)
		if len(children) != 1 {
			t.Fatalf("children = %d", len(children))
		}
		requireOK(t, titleViolation(f.ctx, f.st, children[0].ID, titleFixtureCount))
	})
	t.Run("ApprovalWritingNoTitleNeedsNoSecretRead", func(t *testing.T) {
		task := parent(t)
		spec, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: task.ID, Content: "plain plan", Acceptance: core.JSONPayload([]any{})})
		requireOK(t, err)
		f.withFailedSecretRead(t, func() {
			if _, err := f.st.ApproveSpecVersionAndMaterialize(f.ctx, task.ID, spec.Version); err != nil {
				t.Fatalf("an approval with no child was refused: %v", err)
			}
		})
	})
}

func runStartOverTitleRedaction(t *testing.T, x Fixture) {
	f := newTitleRedactionFixture(t, x)
	f.requireTitleCheckCanFail(t)
	ctx := store.WithActor(f.ctx, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
	raw := func(t *testing.T, title string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Source: "test", Title: title, Body: "The body keeps " + titleFixturePattern, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: x.Config.FreezePolicy(), CreatedAt: time.Now().UTC()}
		requireOK(t, f.st.CreateTask(ctx, task))
		return task
	}

	t.Run("SuccessorTitleAndEvent", func(t *testing.T) {
		old := raw(t, titleFixtureText)
		got, err := taskops.New(f.st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "title-" + old.ID, Reason: "Fresh approach"})
		requireOK(t, err)
		requireOK(t, titleViolation(ctx, f.st, got.Successor.ID, titleFixtureCount))
		if got.Successor.Body != old.Body {
			t.Fatal("the successor's body did not stay as submitted")
		}
		// Rewrite no recorded row (req-security-boundaries REQ-5).
		retired, err := f.st.GetTask(ctx, old.ID)
		requireOK(t, err)
		if retired.Title != old.Title {
			t.Fatal("start-over rewrote the retired task's title")
		}
	})
	t.Run("CleanTitleKeepsItsTextAndRecordsNoCount", func(t *testing.T) {
		old := raw(t, "Plain retired title")
		got, err := taskops.New(f.st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "title-" + old.ID, Reason: "Fresh approach"})
		requireOK(t, err)
		if got.Successor.Title != old.Title {
			t.Fatal("a clean title changed")
		}
		requireOK(t, titleViolation(ctx, f.st, got.Successor.ID, core.RedactionStats{}))
	})
	t.Run("FailedSecretReadRefusesAndChangesNothing", func(t *testing.T) {
		old := raw(t, titleFixtureText)
		request := core.TaskStartOverRequest{TaskID: old.ID, RequestID: "title-" + old.ID, Reason: "Fresh approach"}
		f.withFailedSecretRead(t, func() {
			if _, err := taskops.New(f.st).StartOver(ctx, request); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
				t.Fatalf("start-over err = %v", err)
			}
		})
		kept, err := f.st.GetTask(ctx, old.ID)
		requireOK(t, err)
		if kept.State != old.State || kept.SupersededBy != "" {
			t.Fatal("a refused start-over changed the task")
		}
		// The request is not lost, and its replay needs no secret read.
		got, err := taskops.New(f.st).StartOver(ctx, request)
		requireOK(t, err)
		requireOK(t, titleViolation(ctx, f.st, got.Successor.ID, titleFixtureCount))
		f.withFailedSecretRead(t, func() {
			replay, err := taskops.New(f.st).StartOver(ctx, request)
			requireOK(t, err)
			if replay.Successor.ID != got.Successor.ID {
				t.Fatal("the replay answered a different successor")
			}
		})
	})
}
