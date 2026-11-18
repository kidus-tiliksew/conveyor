package storetest

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runScopedDocumentEventReads proves that the single-requirement and
// planning-session event reads select exactly the requested ID: byte for
// byte, in event order, inside the caller's workspace, and with one statement
// that transfers only the selected rows on a durable backend
// (component-document-corpus).
func runScopedDocumentEventReads(t *testing.T, x Fixture) {
	st := x.Backend
	ctx := store.WithActor(x.Context, store.SystemActor())
	foreign := x.Workspace + "-scoped-events"
	foreignCtx := store.WithActor(store.WithWorkspace(context.Background(), foreign), store.SystemActor())
	_, err := st.CreateWorkspace(foreignCtx, foreign, "Scoped events", &config.Config{Workspace: foreign, Repos: requirementConformanceRepos})
	requireOK(t, err)

	t.Run("single requirement events", func(t *testing.T) {
		suffix := core.NewTaskID()
		lower, upper, noise := "scoped-req-"+suffix, "SCOPED-REQ-"+suffix, "scoped-noise-"+suffix
		// Titles differ by more than case, so the derived slugs never collide.
		create := func(ctx context.Context, id, title string) {
			t.Helper()
			_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{
				Content: "# " + title + "\n\nIntent", Origin: core.RequirementOriginOperator,
				Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}},
			})
			requireOK(t, err)
		}
		create(ctx, lower, "Scoped lower "+suffix)
		create(ctx, upper, "Scoped upper "+suffix)
		create(ctx, noise, "Scoped noise "+suffix)
		create(foreignCtx, lower, "Scoped foreign "+suffix)
		for range 3 {
			_, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
				RequirementID: lower, Content: "# Scoped revision\n\nIntent", Origin: core.RequirementOriginOperator,
				Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve revised intent."}},
			})
			requireOK(t, err)
		}
		_, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: noise, Content: "# Noise revision\n\nIntent", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve noise intent."}},
		})
		requireOK(t, err)

		// The case-distinct requirement has only its creation events, so its
		// count is the creation count every other requirement starts from.
		cased, err := st.ListRequirementEvents(ctx, upper)
		requireOK(t, err)
		if len(cased) == 0 {
			t.Fatal("case-distinct requirement has no creation events")
		}
		requireScopedEvents(t, "case-distinct requirement", cased, "requirement_id", upper, x.Workspace)
		got, err := st.ListRequirementEvents(ctx, lower)
		requireOK(t, err)
		if len(got) != len(cased)+3 {
			t.Fatalf("requirement events = %d, want %d creation events and three proposals", len(got), len(cased))
		}
		requireScopedEvents(t, "requirement", got, "requirement_id", lower, x.Workspace)
		batch, err := st.ListRequirementEventsForRequirements(ctx, []string{lower})
		requireOK(t, err)
		if !reflect.DeepEqual(got, batch[lower]) {
			t.Fatalf("single read = %#v, batch read = %#v", got, batch[lower])
		}
		away, err := st.ListRequirementEvents(foreignCtx, lower)
		requireOK(t, err)
		if len(away) != len(cased) {
			t.Fatalf("foreign workspace requirement events = %d, want its %d creation events", len(away), len(cased))
		}
		requireScopedEvents(t, "foreign requirement", away, "requirement_id", lower, foreign)
		missing, err := st.ListRequirementEvents(ctx, "missing-"+suffix)
		requireOK(t, err)
		if missing == nil || len(missing) != 0 {
			t.Fatalf("missing requirement events = %#v, want an empty slice", missing)
		}
		if x.DrainReadQueries != nil && x.DrainReadRows != nil {
			x.DrainReadQueries()
			x.DrainReadRows()
			_, err := st.ListRequirementEvents(ctx, lower)
			requireOK(t, err)
			queries, rows := x.DrainReadQueries(), x.DrainReadRows()
			if queries["ListRequirementEvents"] != 1 || rows["ListRequirementEvents"] != uint64(len(got)) {
				t.Fatalf("single requirement read: statements=%d rows=%d, want 1 statement and %d rows (all labels: %v)", queries["ListRequirementEvents"], rows["ListRequirementEvents"], len(got), rows)
			}
		}
	})

	t.Run("planning session events", func(t *testing.T) {
		events, ok := st.(planningSessionEventStore)
		if !ok {
			t.Fatal("store cannot list planning-session events")
		}
		suffix := core.NewTaskID()
		lower, upper := "plan-scope-"+suffix, "PLAN-SCOPE-"+suffix
		for _, session := range []struct {
			ctx context.Context
			id  string
		}{{ctx, lower}, {ctx, upper}, {foreignCtx, lower}} {
			_, err := st.CreatePlanningSession(session.ctx, core.PlanningSession{ID: session.id, Title: "Scoped " + session.id})
			requireOK(t, err)
		}
		_, err := st.AbandonPlanningSession(ctx, lower, "Scope probe")
		requireOK(t, err)

		got, err := events.ListPlanningSessionEvents(ctx, lower)
		requireOK(t, err)
		if len(got) != 2 || got[0].Kind != "planning_session.created" || got[1].Kind != "planning_session.abandoned" {
			t.Fatalf("planning session events = %#v, want creation then abandonment", got)
		}
		requireScopedEvents(t, "planning session", got, "session_id", lower, x.Workspace)
		cased, err := events.ListPlanningSessionEvents(ctx, upper)
		requireOK(t, err)
		if len(cased) != 1 || cased[0].Kind != "planning_session.created" {
			t.Fatalf("case-distinct planning session events = %#v, want its creation only", cased)
		}
		requireScopedEvents(t, "case-distinct planning session", cased, "session_id", upper, x.Workspace)
		away, err := events.ListPlanningSessionEvents(foreignCtx, lower)
		requireOK(t, err)
		if len(away) != 1 || away[0].Kind != "planning_session.created" {
			t.Fatalf("foreign planning session events = %#v, want its creation only", away)
		}
		requireScopedEvents(t, "foreign planning session", away, "session_id", lower, foreign)
	})
}

// requireScopedEvents checks that every event names the requested ID byte for
// byte and that the events arrive in (at, id) order.
func requireScopedEvents(t *testing.T, name string, events []core.Event, key, id, workspace string) {
	t.Helper()
	for i, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("%s event %d payload: %v", name, i, err)
		}
		if payload[key] != id {
			t.Fatalf("%s event %d %s = %v, want %q", name, i, key, payload[key], id)
		}
		if got, ok := payload["workspace_id"]; ok && got != workspace {
			t.Fatalf("%s event %d workspace = %v, want %q", name, i, got, workspace)
		}
		if i > 0 && (event.At.Before(events[i-1].At) || event.At.Equal(events[i-1].At) && event.ID <= events[i-1].ID) {
			t.Fatalf("%s events out of order at %d: %#v", name, i, events)
		}
	}
}
