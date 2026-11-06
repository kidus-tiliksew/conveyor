package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const titleStoredKey = "fixture-stored-github-app-key-value"

type titleSecrets struct {
	values []string
	err    error
}

func (s titleSecrets) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.values, s.err
}

func titleServer(t *testing.T, secrets titleSecrets, title string) (*Server, store.Store, context.Context, *int) {
	t.Helper()
	st := store.NewMemory()
	s := NewServer(st)
	s.Repos, s.Workspace, s.BearerToken = []string{"api"}, "demo", "token"
	s.WorkOrders = &workorder.Service{RedactionSecrets: secrets}
	generated := 0
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		generated++
		return title, nil
	}
	return s, st, store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), &generated
}

func createdEventRedactions(t *testing.T, st store.Store, ctx context.Context, id string) *core.RedactionStats {
	t.Helper()
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), titleStoredKey) {
			t.Fatalf("event %s holds the secret", event.Kind)
		}
		if event.Kind == "task.created" {
			var created store.TaskCreatedEvent
			if err := json.Unmarshal(event.Payload, &created); err != nil {
				t.Fatal(err)
			}
			return created.Redactions
		}
	}
	t.Fatal("no task.created event")
	return nil
}

// An exact retry of a create_task whose generated title held a secret answers
// the original task, even though the stored title now differs from what the
// model wrote and the secret read now fails. The comparison still holds the
// body exactly (req-intake-and-triage AC-2.1; DEC-8).
func TestCreateTaskRetryAnswersOriginalWhenGeneratedTitleHeldSecret(t *testing.T) {
	s, st, ctx, generated := titleServer(t, titleSecrets{values: []string{titleStoredKey}}, "Fix "+titleStoredKey)
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithActor(request.Context(), store.SystemActor()))
	arguments := map[string]any{"workspace_id": "demo", "body": "Body keeps " + titleStoredKey, "repo": "api", "idempotency_key": "title-retry"}
	first, err := s.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatal(err)
	}
	original := first.(map[string]any)["task"].(core.Task)
	if strings.Contains(original.Title, titleStoredKey) || original.Body != arguments["body"] {
		t.Fatal("the created task has an unredacted title or a changed body")
	}
	if stats := createdEventRedactions(t, st, ctx, original.ID); stats == nil || *stats != (core.RedactionStats{Exact: 1}) {
		t.Fatalf("task.created redactions = %+v", stats)
	}
	for name, secrets := range map[string]titleSecrets{"readable": {values: []string{titleStoredKey}}, "failing": {err: errors.New("secrets unavailable")}} {
		s.WorkOrders.RedactionSecrets = secrets
		again, err := s.callMCPTool(request, "create_task", arguments)
		if err != nil {
			t.Fatalf("%s retry: %v", name, err)
		}
		if got := again.(map[string]any)["task"].(core.Task); got.ID != original.ID || got.Title != original.Title {
			t.Fatalf("%s retry answered a different task", name)
		}
	}
	if *generated != 1 {
		t.Fatalf("a retry generated a title: %d generations", *generated)
	}
	changed := map[string]any{"workspace_id": "demo", "body": "A different body", "repo": "api", "idempotency_key": "title-retry"}
	if _, err := s.callMCPTool(request, "create_task", changed); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("a changed body under the key: %v", err)
	}
}

// A failed secret read answers as a failed generation does, with fixed text,
// and stores nothing (DEC-8; component-task-lifecycle invariant 13).
func TestCreateTaskFailedSecretReadRefusesLikeFailedTitle(t *testing.T) {
	const cause = "driver detail fixture"
	s, st, ctx, _ := titleServer(t, titleSecrets{err: errors.New(cause)}, "Fix "+titleStoredKey)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"body":"Needs a generated title","repo":"api"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if text := response.Body.String(); strings.Contains(text, cause) || strings.Contains(text, titleStoredKey) {
		t.Fatal("the refusal carries the cause or the title")
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %d err = %v", len(tasks), err)
	}
}

// Control: with no secret in the title, it stays as generated and records no
// count, so the checks above can fail.
func TestCreateTaskCleanGeneratedTitleRecordsNoCount(t *testing.T) {
	s, st, ctx, _ := titleServer(t, titleSecrets{values: []string{titleStoredKey}}, "Plain generated title")
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"body":"Plain body","repo":"api"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}
	var task core.Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Title != "Plain generated title" {
		t.Fatal("a clean title changed")
	}
	if stats := createdEventRedactions(t, st, ctx, task.ID); stats != nil {
		t.Fatalf("a clean title recorded %+v", *stats)
	}
}
