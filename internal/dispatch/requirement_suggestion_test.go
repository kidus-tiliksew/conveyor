package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Triage proposes a requirement relation instead of the retired feature
// placement. The proposal is advisory: it records
// which intent a stray task appears to serve and confirms nothing, and it is
// only ever recorded against a requirement that actually exists in the corpus
// the prompt offered — an agent cannot invent a relation.

func newRequirementSuggestionFixture(t *testing.T) (*Dispatcher, context.Context, core.Task, core.Requirement) {
	t.Helper()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-nightly-reconciliation", Workspace: "demo",
		Title: "Nightly Reconciliation",
	}, core.RequirementVersion{
		Content: "Payments must reconcile nightly.",
		Statements: []core.RequirementStatement{
			{ID: "REQ-1", Statement: "Every payment reconciles within 24 hours."},
		},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := core.Task{
		ID: "requirement-suggestion-task", Workspace: "demo", Repo: "conveyor",
		Branch: "conveyor/task-requirement-suggestion-task",
		State:  core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{Store: st}, ctx, task, requirement
}

func suggestedRequirementEvents(t *testing.T, d *Dispatcher, ctx context.Context, taskID string) []core.Event {
	t.Helper()
	events, err := d.Store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var suggested []core.Event
	for _, event := range events {
		if event.Kind == "task.requirement_suggested" {
			suggested = append(suggested, event)
		}
		if event.Kind == "triage.feature_suggested" {
			t.Errorf("retired event triage.feature_suggested was emitted")
		}
	}
	return suggested
}

func TestRequirementSuggestionRecordsListedRequirement(t *testing.T) {
	t.Parallel()
	d, ctx, task, requirement := newRequirementSuggestionFixture(t)

	if err := d.recordRequirementSuggestion(ctx, task, requirement.ID, core.RequirementServesTriage); err != nil {
		t.Fatal(err)
	}

	suggested := suggestedRequirementEvents(t, d, ctx, task.ID)
	if len(suggested) != 1 {
		t.Fatalf("task.requirement_suggested events = %d, want 1", len(suggested))
	}
	var payload struct {
		RequirementID    string `json:"requirement_id"`
		RequirementSlug  string `json:"requirement_slug"`
		RequirementTitle string `json:"requirement_title"`
	}
	if err := json.Unmarshal(suggested[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RequirementID != requirement.ID {
		t.Errorf("payload requirement_id = %q, want %q", payload.RequirementID, requirement.ID)
	}
	// The slug and title travel with the proposal so a surface can render the
	// suggestion without a second read.
	if payload.RequirementSlug != "nightly-reconciliation" {
		t.Errorf("payload requirement_slug = %q", payload.RequirementSlug)
	}
	if payload.RequirementTitle != "Nightly Reconciliation" {
		t.Errorf("payload requirement_title = %q", payload.RequirementTitle)
	}
}

func TestRequirementSuggestionIgnoresUnlistedAndEmptyProposals(t *testing.T) {
	t.Parallel()
	for _, proposed := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace", value: "   "},
		// An agent that invents an id, or names one from another workspace, must
		// not have it recorded as a relation.
		{name: "not in corpus", value: "req-invented"},
	} {
		t.Run(proposed.name, func(t *testing.T) {
			t.Parallel()
			d, ctx, task, _ := newRequirementSuggestionFixture(t)

			if err := d.recordRequirementSuggestion(ctx, task, proposed.value, core.RequirementServesTriage); err != nil {
				t.Fatal(err)
			}

			if suggested := suggestedRequirementEvents(t, d, ctx, task.ID); len(suggested) != 0 {
				t.Errorf("proposal %q recorded %d events, want 0", proposed.value, len(suggested))
			}
		})
	}
}

// A confirmed requirement is not required for a suggestion: triage proposes
// against the corpus, and confirmation is a separate operator act. A pending
// seed is still a listable requirement.
func TestRequirementSuggestionAcceptsPendingRequirement(t *testing.T) {
	t.Parallel()
	d, ctx, task, requirement := newRequirementSuggestionFixture(t)
	stored, err := d.Store.GetRequirement(ctx, requirement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CurrentVersion != 0 {
		t.Fatalf("fixture requirement is already confirmed at version %d", stored.CurrentVersion)
	}

	if err := d.recordRequirementSuggestion(ctx, task, requirement.ID, core.RequirementServesTriage); err != nil {
		t.Fatal(err)
	}

	if suggested := suggestedRequirementEvents(t, d, ctx, task.ID); len(suggested) != 1 {
		t.Errorf("pending requirement suggestions = %d, want 1", len(suggested))
	}
}
