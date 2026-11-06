package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type issueTitleSecrets struct {
	values []string
	err    error
}

func (s issueTitleSecrets) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.values, s.err
}

func issueTitleDispatcher(t *testing.T, secrets issueTitleSecrets) (*Dispatcher, store.Store, context.Context, *int) {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor("dispatcher")), "demo")
	d := New(st, &config.Config{Workspace: "demo"}, nil)
	d.DisableMemoryQueueForTest()
	d.RedactionSecrets = secrets
	moved := 0
	d.MarkIssueDispatched = func(context.Context, string, int, string) error {
		moved++
		return nil
	}
	return d, st, ctx, &moved
}

var issueTitleRepo = config.Repo{Name: "api", GitHub: "acme/api", Base: "main"}

const issueTitleStoredKey = "fixture-stored-github-app-key-value"

// TestTakeInReadyIssueRedactsTitleAndRecordsCounts covers the intake body, with
// a stored secret only the source knows, and shows the raw form would fail
// (req-security-boundaries REQ-3, AC-3.1).
func TestTakeInReadyIssueRedactsTitleAndRecordsCounts(t *testing.T) {
	d, st, ctx, moved := issueTitleDispatcher(t, issueTitleSecrets{values: []string{issueTitleStoredKey}})
	body := "Body keeps " + issueTitleStoredKey
	if err := d.TakeInReadyIssue(ctx, ctx, "demo", issueTitleRepo, github.Issue{Number: 7, Title: "Fix " + issueTitleStoredKey, Body: body}); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/api#7"})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %d err = %v", len(tasks), err)
	}
	task := tasks[0]
	if strings.Contains(task.Title, issueTitleStoredKey) || !strings.Contains(task.Title, "[REDACTED:exact]") {
		t.Fatal("the stored title was not redacted")
	}
	if task.Body != body {
		t.Fatal("the body did not stay as submitted")
	}
	if *moved != 1 {
		t.Fatalf("label moves = %d", *moved)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if strings.Contains(string(event.Payload), issueTitleStoredKey) {
			t.Fatalf("event %s holds the secret", event.Kind)
		}
		if event.Kind != "task.created" {
			continue
		}
		found = true
		var created store.TaskCreatedEvent
		if err := json.Unmarshal(event.Payload, &created); err != nil {
			t.Fatal(err)
		}
		if created.Redactions == nil || *created.Redactions != (core.RedactionStats{Exact: 1}) {
			t.Fatalf("task.created redactions = %+v", created.Redactions)
		}
	}
	if !found {
		t.Fatal("no task.created event")
	}
	// Control: a clean title stays as written and records no count.
	if err := d.TakeInReadyIssue(ctx, ctx, "demo", issueTitleRepo, github.Issue{Number: 8, Title: "Plain issue", Body: "Plain"}); err != nil {
		t.Fatal(err)
	}
	clean, err := st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/api#8"})
	if err != nil || len(clean) != 1 || clean[0].Title != "Plain issue" {
		t.Fatalf("clean issue: tasks = %d err = %v", len(clean), err)
	}
	events, err = st.ListEvents(ctx, clean[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "task.created" && strings.Contains(string(event.Payload), "redactions") {
			t.Fatal("a clean title recorded a count")
		}
	}
}

// A failed secret read stores nothing and moves no label, so the issue stays
// ready and the next poll or delivery takes it in (DEC-8).
func TestTakeInReadyIssueFailedSecretReadCreatesNoTask(t *testing.T) {
	errRead := errors.New("secrets unavailable")
	d, st, ctx, moved := issueTitleDispatcher(t, issueTitleSecrets{err: errRead})
	issue := github.Issue{Number: 9, Title: "Fix " + issueTitleStoredKey, Body: "Body", URL: "https://example.test/issues/9"}
	err := d.TakeInReadyIssue(ctx, ctx, "demo", issueTitleRepo, issue)
	if !errors.Is(err, errRead) {
		t.Fatalf("intake err = %v", err)
	}
	if strings.Contains(err.Error(), issueTitleStoredKey) {
		t.Fatal("the refusal echoes the issue title")
	}
	tasks, err := st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/api#9"})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %d err = %v", len(tasks), err)
	}
	if *moved != 0 {
		t.Fatalf("label moves = %d", *moved)
	}
	d.RedactionSecrets = issueTitleSecrets{values: []string{issueTitleStoredKey}}
	if err := d.TakeInReadyIssue(ctx, ctx, "demo", issueTitleRepo, issue); err != nil {
		t.Fatal(err)
	}
	if tasks, err = st.ListTasksFiltered(ctx, store.TaskFilter{Query: "github:acme/api#9"}); err != nil || len(tasks) != 1 {
		t.Fatalf("the retry created %d task(s): %v", len(tasks), err)
	}
}

// The store's own secret source reaches intake without separate wiring, so a
// constructed dispatcher can't silently lose stored-key matching.
func TestNewTakesRedactionSecretsFromStore(t *testing.T) {
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	if d := New(st, &config.Config{Workspace: "demo"}, nil); d.RedactionSecrets == nil {
		t.Fatal("the dispatcher has no secret source")
	}
}
