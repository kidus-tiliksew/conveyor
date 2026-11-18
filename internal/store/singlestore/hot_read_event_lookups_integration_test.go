package singlestore

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestHotReadEventLookupsIntegration compares each ready digest lookup with the
// legacy original predicate on one prepared fixture. The legacy Store shares the
// database but has an empty layout, so it runs only the raw guard. The case
// probes (sess-A vs SESS-A) test whether the digest prefilter is exact under the
// table collation. A mismatch there is a finding to report, not a reason to
// change the comparison.
func TestHotReadEventLookupsIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, index := range []string{"events_document_key_idx", "events_session_key_idx", "tasks_workspace_intake_lookup_idx"} {
		if !st.hotReadIndexReady(index) {
			t.Skipf("%s is not prepared", index)
		}
	}
	legacy := &Store{db: st.db}
	ws := "hot-lookups-" + core.NewTaskID()
	other := "hot-lookups-other-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, ws)
	otherCtx := documentPlanWorkspace(t, st, other)

	seedHotLookupEvent(t, st, ctx, "system_design.created", map[string]any{"document_id": "doc-A"})
	seedHotLookupEvent(t, st, ctx, "system_design.updated", map[string]any{"document_id": "doc-A"})
	seedHotLookupEvent(t, st, ctx, "system_design.created", map[string]any{"document_id": "DOC-A"})
	seedHotLookupEvent(t, st, ctx, "system_design.created", map[string]any{"title": "no document id"})
	seedHotLookupEvent(t, st, ctx, "reference_document.created", map[string]any{"document_id": "ref-A"})
	seedHotLookupEvent(t, st, ctx, "planning.noted", map[string]any{"session_id": "sess-A"})
	seedHotLookupEvent(t, st, ctx, "planning.noted", map[string]any{"session_id": "SESS-A"})
	seedHotLookupEvent(t, st, otherCtx, "system_design.created", map[string]any{"document_id": "doc-A"})
	seedHotLookupEvent(t, st, otherCtx, "planning.noted", map[string]any{"session_id": "sess-A"})

	now := time.Now().UTC()
	for _, key := range []string{"REQ-1", "req-1"} {
		id := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: key, State: core.TaskQueued, NextStage: core.StageImplement, IntakeKey: key, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("system design events", func(t *testing.T) {
		for _, id := range []string{"doc-A", "DOC-A", "missing-" + core.NewTaskID()} {
			compareHotLookup(t, "system design "+id, func() ([]core.Event, error) { return legacy.ListSystemDesignEvents(ctx, id) }, func() ([]core.Event, error) { return st.ListSystemDesignEvents(ctx, id) })
		}
		got, err := st.ListSystemDesignEvents(ctx, "doc-A")
		if err != nil || len(got) != 2 {
			t.Fatalf("doc-A events = %d err=%v, want the two workspace events", len(got), err)
		}
	})

	t.Run("reference document events", func(t *testing.T) {
		compareHotLookup(t, "reference ref-A", func() ([]core.Event, error) { return legacy.ListReferenceDocumentEvents(ctx, "ref-A") }, func() ([]core.Event, error) { return st.ListReferenceDocumentEvents(ctx, "ref-A") })
		compareHotLookup(t, "reference missing", func() ([]core.Event, error) { return legacy.ListReferenceDocumentEvents(ctx, "missing") }, func() ([]core.Event, error) { return st.ListReferenceDocumentEvents(ctx, "missing") })
	})

	t.Run("planning session events", func(t *testing.T) {
		for _, id := range []string{"sess-A", "SESS-A", "missing-" + core.NewTaskID()} {
			compareHotLookup(t, "session "+id, func() ([]core.Event, error) { return legacy.ListPlanningSessionEvents(ctx, id) }, func() ([]core.Event, error) { return st.ListPlanningSessionEvents(ctx, id) })
		}
		// Both layouts compare the session ID byte for byte: the case-distinct
		// session and the other workspace's same ID stay out.
		for name, reader := range map[string]*Store{"prepared": st, "legacy": legacy} {
			for _, id := range []string{"sess-A", "SESS-A"} {
				got, err := reader.ListPlanningSessionEvents(ctx, id)
				if err != nil || len(got) != 1 || !strings.Contains(string(got[0].Payload), `"`+id+`"`) {
					t.Fatalf("%s session %s events = %+v err=%v, want only its own workspace event", name, id, got, err)
				}
			}
		}
	})

	t.Run("requirement follow-up intake batch", func(t *testing.T) {
		keys := []string{"REQ-1", "req-1", "missing-" + core.NewTaskID()}
		compareHotLookup(t, "intake batch", func() (map[string]store.RequirementDetailTask, error) {
			return legacy.ListRequirementFollowUps(ctx, keys)
		}, func() (map[string]store.RequirementDetailTask, error) { return st.ListRequirementFollowUps(ctx, keys) })
	})

	t.Run("explain names the ready digest", func(t *testing.T) {
		docFilter, docArgs := st.hotReadLookup("events_document_key_idx", "event_document_key", "BINARY JSON_EXTRACT_STRING(payload_json,'document_id')=BINARY ?", "doc-A")
		hotReadExplainContains(t, st, "events_columnstore document", "event_document_key", "SELECT id FROM events_columnstore WHERE workspace_id=? AND kind LIKE 'system_design.%' AND "+docFilter+" ORDER BY at,id", append([]any{ws}, docArgs...)...)
		sessionFilter, sessionArgs := st.hotReadLookup("events_session_key_idx", "event_session_key", "BINARY JSON_EXTRACT_STRING(payload_json,'session_id')=BINARY ?", "sess-A")
		hotReadExplainContains(t, st, "events_columnstore session", "event_session_key", "SELECT id FROM events_columnstore WHERE workspace_id=? AND "+sessionFilter+" ORDER BY at,id", append([]any{ws}, sessionArgs...)...)
		intakeFilter, intakeArgs := st.hotReadLookup("tasks_workspace_intake_lookup_idx", "task_intake_key", "BINARY intake_key=BINARY ?", "REQ-1")
		hotReadExplainContains(t, st, "tasks intake", "task_intake_key", "SELECT intake_key,id,title,state FROM tasks WHERE workspace_id=? AND ("+intakeFilter+")", append([]any{ws}, intakeArgs...)...)
	})
}

func TestScopedRequirementEventLookupsIntegration(t *testing.T) {
	st := integrationStore(t)
	if !st.hotReadIndexReady("events_requirement_key_idx") {
		t.Skip("events_requirement_key_idx is not prepared")
	}
	legacy := &Store{db: st.db}
	ctx := documentPlanWorkspace(t, st, "scoped-req-"+core.NewTaskID())
	other := documentPlanWorkspace(t, st, "scoped-req-other-"+core.NewTaskID())
	seedHotLookupEvent(t, st, ctx, "requirement.created", map[string]any{"requirement_id": "req-A"})
	seedHotLookupEvent(t, st, ctx, "requirement.updated", map[string]any{"requirement_id": "req-A"})
	seedHotLookupEvent(t, st, ctx, "requirement.created", map[string]any{"requirement_id": "REQ-A"})
	seedHotLookupEvent(t, st, other, "requirement.created", map[string]any{"requirement_id": "req-A"})
	for range 100 {
		seedHotLookupEvent(t, st, ctx, "requirement.noise", map[string]any{"requirement_id": "noise", "body": strings.Repeat("x", 1024)})
	}
	ids := []string{"req-A", "missing", "req-A"}
	compareHotLookup(t, "requirement batch", func() (map[string][]core.Event, error) {
		return legacy.ListRequirementEventsForRequirements(ctx, ids)
	}, func() (map[string][]core.Event, error) {
		return st.ListRequirementEventsForRequirements(ctx, ids)
	})
	got, err := st.ListRequirementEventsForRequirements(ctx, ids)
	if err != nil || len(got) != 2 || len(got["req-A"]) != 2 || got["missing"] == nil {
		t.Fatalf("scoped requirement events = %#v, %v", got, err)
	}
	// The single-requirement read runs the same scoped predicate in both
	// layouts and returns only the requested requirement's workspace events.
	for _, id := range []string{"req-A", "REQ-A", "missing"} {
		compareHotLookup(t, "requirement "+id, func() ([]core.Event, error) { return legacy.ListRequirementEvents(ctx, id) }, func() ([]core.Event, error) { return st.ListRequirementEvents(ctx, id) })
	}
	for name, reader := range map[string]*Store{"prepared": st, "legacy": legacy} {
		for id, want := range map[string]int{"req-A": 2, "REQ-A": 1, "missing": 0} {
			events, err := reader.ListRequirementEvents(ctx, id)
			if err != nil || events == nil || len(events) != want {
				t.Fatalf("%s requirement %s events = %+v err=%v, want %d", name, id, events, err, want)
			}
		}
	}
}

// compareHotLookup requires the prepared result to equal the legacy result for
// the same fixture. Both results are complete; nothing is truncated.
func compareHotLookup[T any](t *testing.T, name string, legacy, prepared func() (T, error)) {
	t.Helper()
	want, err := legacy()
	if err != nil {
		t.Fatalf("%s legacy: %v", name, err)
	}
	got, err := prepared()
	if err != nil {
		t.Fatalf("%s prepared: %v", name, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: prepared=%+v legacy=%+v", name, got, want)
	}
}

func seedHotLookupEvent(t *testing.T, st *Store, ctx context.Context, kind string, payload map[string]any) {
	t.Helper()
	if err := st.documentTx(ctx, func(tx *sql.Tx) error {
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind, Payload: core.JSONPayload(payload)})
	}); err != nil {
		t.Fatal(err)
	}
}

// hotReadExplainContains records the actual EXPLAIN of a prepared shape and
// requires the generated key's name in the plan text.
func hotReadExplainContains(t *testing.T, st *Store, name, column, query string, args ...any) {
	t.Helper()
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("%s explain: %v", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if v.Valid {
				plan = append(plan, v.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	t.Logf("%s EXPLAIN:\n%s", name, text)
	if !strings.Contains(text, column) {
		t.Fatalf("%s plan does not name %s", name, column)
	}
}
