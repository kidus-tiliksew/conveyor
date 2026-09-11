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
	req, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-delegated-execution", Title: "Delegated execution"}, core.RequirementVersion{Content: "# Fixture", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve history."}}})
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
	runMixedDocumentEventPages(t, factory)
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

// DocumentEventMembershipFixture records expected history through the legacy
// event APIs, independently of ListDocumentEventPage.
type DocumentEventMembershipFixture struct {
	Requirement, Design, ConfirmedTask string
	Expected                           map[core.LineageNodeType][]core.Event
}

// SeedDocumentEventMembership mixes every membership branch and its exclusions.
// req-accounts-and-membership AC-4.4; req-260802-72fc68 AC-5.1.
func SeedDocumentEventMembership(t *testing.T, st store.Store, ctx context.Context) DocumentEventMembershipFixture {
	t.Helper()
	ctx = store.WithActor(ctx, store.Actor{ID: "fixture", Role: core.ActorUser})
	f := DocumentEventMembershipFixture{Expected: map[core.LineageNodeType][]core.Event{}}
	for i := 0; i < 2; i++ {
		req, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-membership-" + core.NewTaskID(), Title: "Membership " + core.NewTaskID()}, core.RequirementVersion{Content: "# Membership", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve history."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, 1); err != nil {
			t.Fatal(err)
		}
		design, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-membership-" + core.NewTaskID(), Title: "Membership " + core.NewTaskID(), Category: "Architecture"}, core.SystemDesignVersion{Content: "# Membership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, 1); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.Requirement, f.Design = req.ID, design.ID
		}
	}
	var err error
	f.Expected[core.LineageRequirement], err = st.ListRequirementEvents(ctx, f.Requirement)
	if err != nil {
		t.Fatal(err)
	}
	f.Expected[core.LineageSystemDesign], err = st.ListSystemDesignEvents(ctx, f.Design)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := store.WorkspaceFromContext(ctx)
	at := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, branch := range []string{"confirmed", "pending", "unrelated"} {
		taskID := "membership-" + branch + "-" + core.NewTaskID()
		if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning}); err != nil {
			t.Fatal(err)
		}
		if branch != "unrelated" {
			proposal, err := st.ProposeRequirementServes(ctx, taskID, f.Requirement, core.RequirementServesOperator, branch == "confirmed")
			if err != nil {
				t.Fatal(err)
			}
			want := core.RequirementServesProposed
			if branch == "confirmed" {
				want = core.RequirementServesConfirmed
			}
			if proposal.State != want {
				t.Fatalf("%s proposal state=%s", branch, proposal.State)
			}
		}
		for _, event := range []core.Event{
			{Kind: "fixture.activity", Payload: core.JSONPayload(map[string]any{"requirement_id": f.Requirement, "marker": "payload-on-task"})},
			{Kind: "fixture.activity", Payload: core.JSONPayload(map[string]any{"marker": "without-document"})},
			{Kind: "system_design.consulted", Payload: core.JSONPayload(map[string]any{"document_id": f.Design, "marker": "target-design"})},
			{Kind: "system_design.consulted", Payload: core.JSONPayload(map[string]any{"document_id": "other-design", "marker": "other-design"})},
			{Kind: "fixture.activity", Payload: core.JSONPayload(map[string]any{"document_id": f.Design, "marker": "wrong-kind"})},
		} {
			event.TaskID, event.At = taskID, at
			if err = st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if branch == "confirmed" {
			f.ConfirmedTask = taskID
			f.Expected[core.LineageRequirement] = append(f.Expected[core.LineageRequirement], events...)
		}
		for _, event := range events {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["marker"] == "target-design" {
				f.Expected[core.LineageSystemDesign] = append(f.Expected[core.LineageSystemDesign], event)
			}
		}
	}
	for _, events := range f.Expected {
		sort.Slice(events, func(i, j int) bool {
			if events[i].At.Equal(events[j].At) {
				return events[i].ID > events[j].ID
			}
			return events[i].At.After(events[j].At)
		})
	}
	return f
}

func runMixedDocumentEventPages(t *testing.T, factory RequirementFactory) {
	t.Run("mixed document event membership", func(t *testing.T) {
		f := factory(t, requirementConformanceRepos)
		st, ctx := f.Store, f.Context
		for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
			fixture := SeedDocumentEventMembership(t, st, ctx)
			id := fixture.Requirement
			if kind == core.LineageSystemDesign {
				id = fixture.Design
			}
			expected := fixture.Expected[kind]
			var snapshot int64
			for _, event := range expected {
				snapshot = max(snapshot, event.ID)
			}
			// Pinning uses event identity rather than timestamp or offset. Also
			// exercise an older snapshot that excludes some original members.
			for _, pinned := range []int64{0, snapshot, snapshot - 1} {
				want := []core.Event{}
				for _, event := range expected {
					if pinned == 0 || event.ID <= pinned {
						want = append(want, event)
					}
				}
				for _, offset := range []int{0, 1, 3, len(want), len(want) + 1} {
					page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 3, Offset: offset, SnapshotID: pinned})
					wantSnapshot := snapshot
					if pinned > 0 {
						wantSnapshot = pinned
					}
					if err != nil || page.Total != len(want) || page.SnapshotID != wantSnapshot || page.Limit != 3 || page.Offset != offset {
						t.Fatalf("%s pinned=%d offset=%d: %+v err=%v", kind, pinned, offset, page, err)
					}
					assertDocumentEventsEqual(t, page.Events, want[min(offset, len(want)):min(offset+3, len(want))])
				}
			}
			for _, at := range []time.Time{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)} {
				if err := st.AppendEvent(ctx, core.Event{TaskID: fixture.ConfirmedTask, Kind: "system_design.consulted", At: at, Payload: core.JSONPayload(map[string]any{"document_id": fixture.Design})}); err != nil {
					t.Fatal(err)
				}
			}
			page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 200, SnapshotID: snapshot})
			if err != nil || page.Total != len(expected) || page.SnapshotID != snapshot {
				t.Fatalf("%s changed pinned history: %+v err=%v", kind, page, err)
			}
			assertDocumentEventsEqual(t, page.Events, expected)
			for _, pinned := range []int64{0, snapshot} {
				foreign, err := st.ListDocumentEventPage(store.WithWorkspace(ctx, "other-"+core.NewTaskID()), kind, id, store.DocumentEventQuery{Limit: 3, SnapshotID: pinned})
				if err != nil || foreign.Total != 0 || len(foreign.Events) != 0 || foreign.SnapshotID != pinned {
					t.Fatalf("foreign %s: %+v err=%v", kind, foreign, err)
				}
			}
		}
	})
}

func assertDocumentEventsEqual(t *testing.T, got, want []core.Event) {
	t.Helper()
	normalize := func(events []core.Event) []core.Event {
		out := append([]core.Event{}, events...)
		for i := range out {
			out[i].At = out[i].At.UTC()
			var payload any
			if err := json.Unmarshal(out[i].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			out[i].Payload, _ = json.Marshal(payload)
		}
		return out
	}
	if !reflect.DeepEqual(normalize(got), normalize(want)) {
		t.Fatalf("document history differs:\ngot: %+v\nwant: %+v", got, want)
	}
}
