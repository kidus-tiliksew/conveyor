package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// SeedDocumentEventMeasurement supplies the same named endpoints and 600-event
// workload to the HTTP measurement and both store implementations.
func SeedDocumentEventMeasurement(t *testing.T, st store.Store, ctx context.Context) (string, string, []string) {
	t.Helper()
	ctx = store.WithActor(ctx, store.Actor{ID: "fixture", Role: core.ActorUser})
	req, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-delegated-execution", Title: "Delegated execution"}, core.RequirementVersion{Content: "Fixture", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve history."}}})
	if err != nil {
		t.Fatal(err)
	}
	design, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-260805-973cd4", Title: "Work orders", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Work orders\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, req.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmSystemDesignVersion(ctx, design.ID, 1); err != nil {
		t.Fatal(err)
	}
	sessionID := "fixture-session-" + core.NewTaskID()
	if _, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: sessionID, Title: "Fixture"}); err != nil {
		t.Fatal(err)
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	taskIDs := []string{}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for j := 0; j < 2; j++ {
		id := "fixture-blueprint-" + core.NewTaskID()
		taskIDs = append(taskIDs, id)
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning}); err != nil {
			t.Fatal(err)
		}
		childID := "fixture-child-" + core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: childID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + childID, State: core.TaskRunning, ParentTaskID: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProposeRequirementServes(ctx, id, req.ID, core.RequirementServesOperator, true); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 300; i++ {
			if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "fixture.activity", At: at, Payload: core.JSONPayload(map[string]any{"message": strings.Repeat("x", 2048), "index": i})}); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordSystemDesignConsulted(ctx, design.ID, 1, sessionID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	return req.ID, design.ID, taskIDs
}

func runDocumentEventPages(t *testing.T, factory RequirementFactory) {
	t.Run("system design pages include task events with document and workspace filtering", func(t *testing.T) {
		f := factory(t, requirementConformanceRepos)
		st, ctx := f.Store, f.Context
		_, design, tasks := SeedDocumentEventMeasurement(t, st, ctx)
		baseline, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, design, store.DocumentEventQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		for i := 0; i < 3; i++ {
			if err := st.AppendEvent(ctx, core.Event{TaskID: tasks[0], Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": design, "index": i})}); err != nil {
				t.Fatal(err)
			}
		}
		// Neither a different document nor a different event kind belongs in this history.
		for _, event := range []core.Event{
			{TaskID: tasks[0], Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": "other-design"})},
			{TaskID: tasks[0], Kind: "fixture.activity", At: at, Payload: core.JSONPayload(map[string]any{"document_id": design})},
		} {
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		first, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, design, store.DocumentEventQuery{Limit: 2})
		if err != nil || first.Total != baseline.Total+3 || len(first.Events) != 2 {
			t.Fatalf("task-associated first page: total=%d want=%d events=%d err=%v", first.Total, baseline.Total+3, len(first.Events), err)
		}
		// A matching task event appended after page one must not shift page two.
		if err := st.AppendEvent(ctx, core.Event{TaskID: tasks[0], Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": design, "index": 3})}); err != nil {
			t.Fatal(err)
		}
		second, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, design, store.DocumentEventQuery{Limit: 2, Offset: 2, SnapshotID: first.SnapshotID})
		if err != nil || second.Total != first.Total || second.SnapshotID != first.SnapshotID || len(second.Events) != 2 {
			t.Fatalf("task-associated second page: %+v err=%v", second, err)
		}
		for i, event := range append(first.Events, second.Events[0]) {
			var payload struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if event.TaskID != tasks[0] || event.Kind != "system_design.consulted" || payload.Index != 2-i {
				t.Fatalf("event %d: %+v", i, event)
			}
		}
		if second.Events[1].ID != baseline.Events[0].ID {
			t.Fatalf("page two did not resume document history: %+v", second.Events)
		}
		foreign, err := st.ListDocumentEventPage(store.WithWorkspace(ctx, "other-"+core.NewTaskID()), core.LineageSystemDesign, design, store.DocumentEventQuery{Limit: 2})
		if err != nil || foreign.Total != 0 || len(foreign.Events) != 0 {
			t.Fatalf("foreign task-associated history: %+v err=%v", foreign, err)
		}
	})
	t.Run("document event pages retain history and snapshot across appends", func(t *testing.T) {
		f := factory(t, requirementConformanceRepos)
		st, ctx := f.Store, f.Context
		req, design, tasks := SeedDocumentEventMeasurement(t, st, ctx)
		for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
			t.Run(string(kind), func(t *testing.T) {
				id := req
				expected, err := st.ListRequirementEvents(ctx, req)
				if kind == core.LineageSystemDesign {
					id = design
					expected, err = st.ListSystemDesignEvents(ctx, design)
				} else {
					for _, task := range tasks {
						items, listErr := st.ListEvents(ctx, task)
						if listErr != nil {
							t.Fatal(listErr)
						}
						expected = append(expected, items...)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				sort.Slice(expected, func(i, j int) bool {
					if expected[i].At.Equal(expected[j].At) {
						return expected[i].ID > expected[j].ID
					}
					return expected[i].At.After(expected[j].At)
				})
				page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50})
				if err != nil || page.Total != len(expected) || len(page.Events) != 50 {
					t.Fatalf("first page total=%d events=%d err=%v", page.Total, len(page.Events), err)
				}
				snapshot := page.SnapshotID
				// Both a newer timestamp and a backdated append must stay outside the snapshot.
				for _, at := range []time.Time{time.Now().UTC().Add(time.Minute), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)} {
					if err := st.AppendEvent(ctx, core.Event{TaskID: tasks[0], Kind: "fixture.append", At: at}); err != nil {
						t.Fatal(err)
					}
				}
				got := append([]core.Event{}, page.Events...)
				for offset := 50; offset < page.Total; offset += 50 {
					next, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 50, Offset: offset, SnapshotID: snapshot})
					if err != nil || next.Total != page.Total || next.SnapshotID != snapshot {
						t.Fatalf("page %d: %+v %v", offset, next, err)
					}
					got = append(got, next.Events...)
				}
				normalize := func(events []core.Event) {
					for i := range events {
						events[i].At = events[i].At.UTC()
						var payload any
						if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
							t.Fatal(err)
						}
						events[i].Payload, _ = json.Marshal(payload)
					}
				}
				normalize(got)
				normalize(expected)
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("paged history differs: got %d expected %d", len(got), len(expected))
				}
				empty, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 200, Offset: len(expected) + 100, SnapshotID: snapshot})
				if err != nil || len(empty.Events) != 0 || empty.Total != len(expected) {
					t.Fatalf("empty page: %+v %v", empty, err)
				}
				foreign, err := st.ListDocumentEventPage(store.WithWorkspace(ctx, "other-"+core.NewTaskID()), kind, id, store.DocumentEventQuery{Limit: 50})
				if err != nil || foreign.Total != 0 || len(foreign.Events) != 0 {
					t.Fatalf("foreign page: %+v %v", foreign, err)
				}
				for _, q := range []store.DocumentEventQuery{{Limit: 0}, {Limit: 201}, {Limit: 1, Offset: -1}, {Limit: 1, SnapshotID: -1}} {
					if _, err := st.ListDocumentEventPage(ctx, kind, id, q); err == nil {
						t.Fatal(fmt.Sprintf("accepted invalid query %+v", q))
					}
				}
			})
		}
	})
}
