package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runCrates holds every backend to component-crates invariants 2, 4 to 13,
// 22, and 29: records, membership, the closed-crate refusal, counts,
// shipped, requirement coverage, list order, activity, the insight inputs,
// and the briefing store (feature-crates CR-1; feature-crate-insights CI-1
// and CI-2; req-crates REQ-1 to REQ-4). Each case starts from a fresh workspace.
func runCrates(t *testing.T, factory Factory) {
	t.Run("CrateCreateRecordsOneEvent", func(t *testing.T) { runCrateCreate(t, factory.fresh(t, nil)) })
	t.Run("CrateRefusalsRecordNothing", func(t *testing.T) { runCrateRefusals(t, factory.fresh(t, nil)) })
	t.Run("CrateEditsRecordOldAndNew", func(t *testing.T) { runCrateEdits(t, factory.fresh(t, nil)) })
	t.Run("CrateMembership", func(t *testing.T) { runCrateMembership(t, factory.fresh(t, nil)) })
	t.Run("CrateClosedRefusesJoinAndIntake", func(t *testing.T) { runCrateClosed(t, factory.fresh(t, nil)) })
	t.Run("CrateCloseRacesJoins", func(t *testing.T) { runCrateCloseRace(t, factory.fresh(t, nil)) })
	t.Run("CrateCountsAndShipped", func(t *testing.T) { runCrateCounts(t, factory.fresh(t, nil)) })
	t.Run("CrateListOrder", func(t *testing.T) { runCrateListOrder(t, factory.fresh(t, nil)) })
	t.Run("CrateRequirementCoverage", func(t *testing.T) { runCrateCoverage(t, factory.fresh(t, nil)) })
	t.Run("CrateActivity", func(t *testing.T) { runCrateActivity(t, factory.fresh(t, nil)) })
	t.Run("CrateTaskFilter", func(t *testing.T) { runCrateTaskFilter(t, factory.fresh(t, nil)) })
	t.Run("CrateIntakeRecordsCrate", func(t *testing.T) { runCrateIntake(t, factory.fresh(t, nil)) })
	t.Run("CrateMemberRead", func(t *testing.T) { runCrateMemberRead(t, factory.fresh(t, nil)) })
	t.Run("CrateMembersLimited", func(t *testing.T) { runCrateMembersLimited(t, factory.fresh(t, nil)) })
	t.Run("CrateInsightInputs", func(t *testing.T) { runCrateInsightInputs(t, factory.fresh(t, nil)) })
	t.Run("CrateInsightInputsBoundedStatements", func(t *testing.T) {
		runCrateInsightInputsBounded(t, factory.fresh(t, nil), factory.ProductionCapable)
	})
	t.Run("CrateInsightInputsMemberLeavesMidRead", func(t *testing.T) {
		runCrateInsightInputsMemberLeaves(t, factory.fresh(t, nil), factory.ProductionCapable)
	})
	runCrateBriefings(t, factory)
}

func crateText(value string) *string { return &value }

func crateRequirement(t *testing.T, x Fixture, confirm bool) string {
	t.Helper()
	label := core.NewTaskID()
	id := "req-crate-" + label
	version := core.RequirementVersion{RequirementID: id, Content: "# " + id + "\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Group crate work " + label + ".\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Group crate work " + label + "."}}, Origin: core.RequirementOriginOperator}
	_, created, err := x.Backend.CreateRequirement(x.Context, core.Requirement{ID: id, Title: "Crate requirement " + label}, version)
	requireOK(t, err)
	if confirm {
		_, _, err = x.Backend.ConfirmRequirementVersion(x.Context, id, created.Version)
		requireOK(t, err)
	}
	return id
}

func crateTask(t *testing.T, x Fixture, state core.TaskState, crateID string) core.Task {
	t.Helper()
	id := fixtureTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Crate member " + strings.ToUpper(string(state)) + " " + id, Body: "Crate member.", State: state, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	if crateID != "" {
		task.Crate = &core.TaskCrate{ID: crateID}
	}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	return task
}

func crateEvents(t *testing.T, x Fixture) []core.Event {
	t.Helper()
	events, err := x.WorkspaceEvents(x.Context, "crate.")
	requireOK(t, err)
	return events
}

func taskEventKinds(t *testing.T, x Fixture, taskID string) []string {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	kinds := make([]string, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}

func decodeCratePayload(t *testing.T, event core.Event) store.CrateEventPayload {
	t.Helper()
	var payload store.CrateEventPayload
	requireOK(t, jsonUnmarshal(event.Payload, &payload))
	return payload
}

func runCrateCreate(t *testing.T, x Fixture) {
	requirement := crateRequirement(t, x, true)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "  Spring release  ", TargetDate: crateText("2027-03-31"), RequirementIDs: []string{requirement, requirement}})
	requireOK(t, err)
	if !strings.HasPrefix(crate.ID, "crate-") || len(crate.ID) != len("crate-261118-a1b2c3") {
		t.Fatalf("crate ID %q is not crate-<yymmdd>-<6 hex>", crate.ID)
	}
	if crate.Name != "Spring release" || crate.State != store.CrateOpen || crate.TargetDate == nil || *crate.TargetDate != "2027-03-31" || crate.CreatedBy == "" || crate.CreatedAt.IsZero() || crate.ClosedAt != nil {
		t.Fatalf("created crate = %+v", crate)
	}
	if !slices.Equal(crate.RequirementIDs, []string{requirement}) {
		t.Fatalf("declared requirements = %v, want one %s", crate.RequirementIDs, requirement)
	}
	read, err := x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	if read.Name != crate.Name || !read.CreatedAt.Equal(crate.CreatedAt) || read.CreatedBy != crate.CreatedBy || !slices.Equal(read.RequirementIDs, []string{requirement}) || len(read.TaskIDs) != 0 {
		t.Fatalf("read crate = %+v, created %+v", read, crate)
	}
	events := crateEvents(t, x)
	if len(events) != 1 || events[0].Kind != store.CrateCreatedEvent || events[0].ActorID == "" || events[0].TaskID != "" {
		t.Fatalf("crate events = %+v, want one crate.created with an actor", events)
	}
	payload := decodeCratePayload(t, events[0])
	if payload.CrateID != crate.ID || payload.Changes["name"].New != "Spring release" || payload.Changes["state"].New != "open" {
		t.Fatalf("crate.created payload = %+v", payload)
	}
	names, err := x.Backend.CrateNames(x.Context, []string{crate.ID, "crate-000000-000000"})
	requireOK(t, err)
	if len(names) != 1 || names[crate.ID] != "Spring release" {
		t.Fatalf("crate names = %v", names)
	}
}

func runCrateRefusals(t *testing.T, x Fixture) {
	unconfirmed := crateRequirement(t, x, false)
	archived := crateRequirement(t, x, true)
	requireOK(t, x.Backend.ArchiveRequirement(x.Context, archived, "operator", nil))
	existing, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Existing"})
	requireOK(t, err)
	before := len(crateEvents(t, x))
	cases := []struct {
		name  string
		input store.CrateInput
		check func(error) bool
	}{
		{"empty name", store.CrateInput{Name: "   "}, fieldRefusal("name")},
		{"long name", store.CrateInput{Name: strings.Repeat("n", 121)}, fieldRefusal("name")},
		{"bad date", store.CrateInput{Name: "Dated", TargetDate: crateText("2027-02-30")}, fieldRefusal("target_date")},
		{"date form", store.CrateInput{Name: "Dated", TargetDate: crateText("2027-3-1")}, fieldRefusal("target_date")},
		{"date before year 1000", store.CrateInput{Name: "Dated", TargetDate: crateText("0999-12-31")}, fieldRefusal("target_date")},
		{"missing requirement", store.CrateInput{Name: "Needs", RequirementIDs: []string{"req-crate-missing"}}, referenceRefusal("req-crate-missing")},
		{"unconfirmed requirement", store.CrateInput{Name: "Needs", RequirementIDs: []string{unconfirmed}}, referenceRefusal(unconfirmed)},
		{"archived requirement", store.CrateInput{Name: "Needs", RequirementIDs: []string{archived}}, referenceRefusal(archived)},
	}
	for _, tc := range cases {
		if _, err := x.Backend.CreateCrate(x.Context, tc.input); !tc.check(err) {
			t.Fatalf("%s: create err = %v", tc.name, err)
		}
	}
	if long := strings.Repeat("é", 120); true {
		crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: long})
		requireOK(t, err)
		if crate.Name != long {
			t.Fatalf("120-character name = %q", crate.Name)
		}
		before++
	}
	patches := []struct {
		name  string
		id    string
		patch store.CratePatch
		check func(error) bool
	}{
		{"missing crate", "crate-000000-000000", store.CratePatch{Name: crateText("Gone")}, notFoundRefusal},
		{"empty name", existing.ID, store.CratePatch{Name: crateText("")}, fieldRefusal("name")},
		{"bad state", existing.ID, store.CratePatch{State: func() *store.CrateState { s := store.CrateState("done"); return &s }()}, fieldRefusal("state")},
		{"bad date", existing.ID, store.CratePatch{TargetDateSet: true, TargetDate: crateText("tomorrow")}, fieldRefusal("target_date")},
		{"archived requirement", existing.ID, store.CratePatch{RequirementIDs: &[]string{archived}}, referenceRefusal(archived)},
	}
	for _, tc := range patches {
		if _, err := x.Backend.UpdateCrate(x.Context, tc.id, tc.patch); !tc.check(err) {
			t.Fatalf("%s: update err = %v", tc.name, err)
		}
	}
	crates, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{})
	requireOK(t, err)
	if len(crates) != 2 {
		t.Fatalf("crates after refusals = %+v, want the two accepted crates", crates)
	}
	if after := len(crateEvents(t, x)); after != before {
		t.Fatalf("crate events after refusals = %d, want %d", after, before)
	}
	read, err := x.Backend.GetCrate(x.Context, existing.ID)
	requireOK(t, err)
	if read.Name != "Existing" || read.State != store.CrateOpen || read.TargetDate != nil {
		t.Fatalf("refused edits changed the crate: %+v", read)
	}
	if _, err := x.Backend.GetCrate(x.Context, "crate-000000-000000"); !notFoundRefusal(err) {
		t.Fatalf("missing crate read err = %v", err)
	}
	if _, err := x.Backend.ListCrateActivity(x.Context, "crate-000000-000000", store.CrateActivityQuery{Limit: 10}); !notFoundRefusal(err) {
		t.Fatalf("missing crate activity err = %v", err)
	}
}

func fieldRefusal(field string) func(error) bool {
	return func(err error) bool {
		var refusal *store.CrateFieldError
		return errors.As(err, &refusal) && refusal.Field == field
	}
}

func referenceRefusal(id string) func(error) bool {
	return func(err error) bool {
		var reference *store.TaskContextReferenceError
		var archived *store.RequirementArchivedError
		return (errors.As(err, &reference) && reference.ID == id) || (errors.As(err, &archived) && archived.RequirementID == id)
	}
}

func notFoundRefusal(err error) bool {
	var refusal *store.CrateNotFoundError
	return errors.As(err, &refusal)
}

func closedRefusal(err error) bool {
	var refusal *store.CrateClosedError
	return errors.As(err, &refusal)
}

func runCrateEdits(t *testing.T, x Fixture) {
	first := crateRequirement(t, x, true)
	second := crateRequirement(t, x, true)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Edits", TargetDate: crateText("2027-01-15"), RequirementIDs: []string{first}})
	requireOK(t, err)
	member := crateTask(t, x, core.TaskQueued, crate.ID)
	_, err = x.Backend.UpdateTaskContext(x.Context, member.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{first}}})
	requireOK(t, err)
	contextBefore := taskEventKinds(t, x, member.ID)

	updated, err := x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{Name: crateText("Edits renamed"), TargetDateSet: true, RequirementIDs: &[]string{second}})
	requireOK(t, err)
	if updated.Name != "Edits renamed" || updated.TargetDate != nil || !slices.Equal(updated.RequirementIDs, []string{second}) || updated.TotalTaskCount != 1 {
		t.Fatalf("updated crate = %+v", updated)
	}
	// An omitted field stays, and a patch that changes nothing records nothing.
	same, err := x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{Name: crateText("Edits renamed")})
	requireOK(t, err)
	if same.Name != "Edits renamed" || !slices.Equal(same.RequirementIDs, []string{second}) {
		t.Fatalf("no-op edit = %+v", same)
	}
	closed, err := x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: func() *store.CrateState { s := store.CrateClosed; return &s }()})
	requireOK(t, err)
	if closed.State != store.CrateClosed || closed.ClosedAt == nil || closed.TotalTaskCount != 1 || closed.Shipped {
		t.Fatalf("closed crate = %+v", closed)
	}
	reopened, err := x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: func() *store.CrateState { s := store.CrateOpen; return &s }()})
	requireOK(t, err)
	if reopened.State != store.CrateOpen || reopened.ClosedAt != nil || reopened.TotalTaskCount != 1 {
		t.Fatalf("reopened crate = %+v", reopened)
	}
	record, err := x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	if !slices.Equal(record.TaskIDs, []string{member.ID}) {
		t.Fatalf("members after close and reopen = %v", record.TaskIDs)
	}
	events := crateEvents(t, x)
	kinds := make([]string, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	if want := []string{store.CrateCreatedEvent, store.CrateUpdatedEvent, store.CrateClosedEvent, store.CrateReopenedEvent}; !slices.Equal(kinds, want) {
		t.Fatalf("crate event kinds = %v, want %v", kinds, want)
	}
	update := decodeCratePayload(t, events[1])
	if update.Changes["name"].Old != "Edits" || update.Changes["name"].New != "Edits renamed" || update.Changes["target_date"].Old != "2027-01-15" || update.Changes["target_date"].New != nil {
		t.Fatalf("crate.updated payload = %+v", update)
	}
	if _, ok := update.Changes["requirement_ids"]; !ok || len(update.Changes) != 3 {
		t.Fatalf("crate.updated changes = %+v", update.Changes)
	}
	if closePayload := decodeCratePayload(t, events[2]); closePayload.Changes["state"].Old != "open" || closePayload.Changes["state"].New != "closed" {
		t.Fatalf("crate.closed payload = %+v", closePayload)
	}
	for _, event := range events[1:] {
		if event.ActorID == "" {
			t.Fatalf("crate event %s has no actor", event.Kind)
		}
	}
	// Replacing the declared list changes no task's context attachments
	// (req-crates AC-3.3).
	if after := taskEventKinds(t, x, member.ID); !slices.Equal(after, contextBefore) {
		t.Fatalf("member events changed with the declared list: %v, before %v", after, contextBefore)
	}
	active, err := store.TaskContextForTask(x.Context, x.Backend, member.ID)
	requireOK(t, err)
	if len(active.Requirements) != 1 || active.Requirements[0].ID != first {
		t.Fatalf("member context = %+v, want only %s", active.Requirements, first)
	}
}

func runCrateMembership(t *testing.T, x Fixture) {
	first, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "First"})
	requireOK(t, err)
	second, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Second"})
	requireOK(t, err)
	requirement := crateRequirement(t, x, true)
	dependency := crateTask(t, x, core.TaskQueued, "")
	task := core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Member", Body: "Member.", State: core.TaskQueued, NextStage: core.StageImplement, MergeApproval: true, PlanState: core.PlanNeeded, CreatedAt: time.Now().UTC(), Assignee: nil}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, x.Backend.CreateTaskWithDependenciesAndContext(x.Context, task, []string{dependency.ID}, store.TaskContextInput{RequirementIDs: []string{requirement}}))
	before, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	contextBefore, err := store.TaskContextForTask(x.Context, x.Backend, task.ID)
	requireOK(t, err)

	joined, err := x.Backend.SetTaskCrate(x.Context, task.ID, first.ID)
	requireOK(t, err)
	if joined.TaskCrateID() != first.ID {
		t.Fatalf("joined crate = %v", joined.Crate)
	}
	moved, err := x.Backend.SetTaskCrate(x.Context, task.ID, second.ID)
	requireOK(t, err)
	if moved.TaskCrateID() != second.ID {
		t.Fatalf("moved crate = %v", moved.Crate)
	}
	// Joining the crate the task holds changes nothing and records nothing.
	if _, err := x.Backend.SetTaskCrate(x.Context, task.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	firstRecord, err := x.Backend.GetCrate(x.Context, first.ID)
	requireOK(t, err)
	secondRecord, err := x.Backend.GetCrate(x.Context, second.ID)
	requireOK(t, err)
	if len(firstRecord.TaskIDs) != 0 || !slices.Equal(secondRecord.TaskIDs, []string{task.ID}) {
		t.Fatalf("members first=%v second=%v, want the task in the second crate only", firstRecord.TaskIDs, secondRecord.TaskIDs)
	}
	cleared, err := x.Backend.ClearTaskCrate(x.Context, task.ID)
	requireOK(t, err)
	if cleared.Crate != nil {
		t.Fatalf("cleared crate = %v", cleared.Crate)
	}
	if _, err := x.Backend.ClearTaskCrate(x.Context, task.ID); err != nil {
		t.Fatal(err)
	}
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	var moves []store.TaskCrateMovePayload
	var kinds []string
	for _, event := range events {
		if event.Kind != store.TaskCrateSetEvent && event.Kind != store.TaskCrateClearedEvent {
			continue
		}
		if event.ActorID == "" {
			t.Fatalf("membership event %s has no actor", event.Kind)
		}
		var payload store.TaskCrateMovePayload
		requireOK(t, jsonUnmarshal(event.Payload, &payload))
		moves = append(moves, payload)
		kinds = append(kinds, event.Kind)
	}
	wantKinds := []string{store.TaskCrateSetEvent, store.TaskCrateSetEvent, store.TaskCrateClearedEvent}
	wantMoves := []store.TaskCrateMovePayload{{TaskID: task.ID, CrateID: first.ID}, {TaskID: task.ID, CrateID: second.ID, LeftCrateID: first.ID}, {TaskID: task.ID, LeftCrateID: second.ID}}
	if !slices.Equal(kinds, wantKinds) || !slices.Equal(moves, wantMoves) {
		t.Fatalf("membership events = %v %+v, want %v %+v", kinds, moves, wantKinds, wantMoves)
	}
	// A membership act changes the task's crate and nothing else on the task
	// (req-crates AC-4.1; component-crates invariant 1).
	after, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if after.State != before.State || after.GateKind != before.GateKind || after.PlanState != before.PlanState || after.MergeApproval != before.MergeApproval ||
		after.PolicyVersion != before.PolicyVersion || !jsonEqual(t, after.SetupContract, before.SetupContract) || !jsonEqual(t, after.Assignee, before.Assignee) ||
		after.NextStage != before.NextStage || !jsonEqual(t, after.Dependencies, before.Dependencies) || after.Crate != nil {
		t.Fatalf("membership changed the task: before %+v after %+v", before, after)
	}
	contextAfter, err := store.TaskContextForTask(x.Context, x.Backend, task.ID)
	requireOK(t, err)
	if !jsonEqual(t, contextAfter, contextBefore) {
		t.Fatalf("membership changed context: before %+v after %+v", contextBefore, contextAfter)
	}
	if _, err := x.Backend.SetTaskCrate(x.Context, "missing-task", first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("join of a missing task err = %v", err)
	}
	eventsBefore := len(events)
	if _, err := x.Backend.SetTaskCrate(x.Context, task.ID, "crate-000000-000000"); !notFoundRefusal(err) {
		t.Fatalf("join of a missing crate err = %v", err)
	}
	if _, err := x.Backend.SetTaskCrate(x.Context, task.ID, "   "); !fieldRefusal("crate_id")(err) {
		t.Fatalf("join of a blank crate err = %v", err)
	}
	if events, err := x.Backend.ListEvents(x.Context, task.ID); err != nil || len(events) != eventsBefore {
		t.Fatalf("refused join recorded events: %d, want %d (%v)", len(events), eventsBefore, err)
	}
}

func runCrateClosed(t *testing.T, x Fixture) {
	open, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Open"})
	requireOK(t, err)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Closing"})
	requireOK(t, err)
	member := crateTask(t, x, core.TaskQueued, crate.ID)
	outsider := crateTask(t, x, core.TaskQueued, open.ID)
	closedState := store.CrateClosed
	_, err = x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: &closedState})
	requireOK(t, err)
	eventsBefore := taskEventKinds(t, x, outsider.ID)
	if _, err := x.Backend.SetTaskCrate(x.Context, outsider.ID, crate.ID); !closedRefusal(err) {
		t.Fatalf("join of a closed crate err = %v", err)
	}
	if after := taskEventKinds(t, x, outsider.ID); !slices.Equal(after, eventsBefore) {
		t.Fatalf("refused join recorded events: %v", after)
	}
	if task, err := x.Backend.GetTask(x.Context, outsider.ID); err != nil || task.TaskCrateID() != open.ID {
		t.Fatalf("refused join moved the task: %+v %v", task.Crate, err)
	}
	tasksBefore, err := x.Backend.ListTasks(x.Context)
	requireOK(t, err)
	refused := core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Late", Body: "Late.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
	refused.Branch = "conveyor/task-" + refused.ID
	if err := x.Backend.CreateTask(x.Context, refused); !closedRefusal(err) {
		t.Fatalf("intake into a closed crate err = %v", err)
	}
	missing := refused
	missing.ID, missing.Crate = fixtureTaskID(), &core.TaskCrate{ID: "crate-000000-000000"}
	missing.Branch = "conveyor/task-" + missing.ID
	if err := x.Backend.CreateTask(x.Context, missing); !notFoundRefusal(err) {
		t.Fatalf("intake into a missing crate err = %v", err)
	}
	tasksAfter, err := x.Backend.ListTasks(x.Context)
	requireOK(t, err)
	if len(tasksAfter) != len(tasksBefore) {
		t.Fatalf("refused intake recorded a task: %d tasks, want %d", len(tasksAfter), len(tasksBefore))
	}
	for _, id := range []string{refused.ID, missing.ID} {
		if events, err := x.Backend.ListEvents(x.Context, id); err == nil && len(events) > 0 {
			t.Fatalf("refused intake recorded events for %s: %+v", id, events)
		}
	}
	// A task may leave a closed crate, and the closed crate stays readable.
	left, err := x.Backend.ClearTaskCrate(x.Context, member.ID)
	requireOK(t, err)
	if left.Crate != nil {
		t.Fatalf("task did not leave the closed crate: %+v", left.Crate)
	}
	record, err := x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	if record.State != store.CrateClosed || len(record.TaskIDs) != 0 {
		t.Fatalf("closed crate = %+v", record)
	}
	closedList, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{State: store.CrateClosed})
	requireOK(t, err)
	openList, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{State: store.CrateOpen})
	requireOK(t, err)
	if len(closedList) != 1 || closedList[0].ID != crate.ID || len(openList) != 1 || openList[0].ID != open.ID {
		t.Fatalf("closed list %+v open list %+v", closedList, openList)
	}
}

// runCrateCloseRace closes a crate while tasks join it and while intake names
// it. Every join and intake either lands before the close or is refused as
// closed, and once the close returns no task can join (component-crates
// invariant 6; req-crates AC-2.4).
func runCrateCloseRace(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Race"})
	requireOK(t, err)
	const joiners = 6
	tasks := make([]core.Task, joiners)
	for i := range tasks {
		tasks[i] = crateTask(t, x, core.TaskQueued, "")
	}
	intake := make([]core.Task, joiners)
	for i := range intake {
		intake[i] = core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Racing intake", Body: "Racing intake.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
		intake[i].Branch = "conveyor/task-" + intake[i].ID
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	joinErrs := make([]error, joiners)
	intakeErrs := make([]error, joiners)
	for i := range joiners {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, joinErrs[i] = x.Backend.SetTaskCrate(x.Context, tasks[i].ID, crate.ID)
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			intakeErrs[i] = x.Backend.CreateTask(x.Context, intake[i])
		}(i)
	}
	closedState := store.CrateClosed
	var closeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, closeErr = x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: &closedState})
	}()
	close(start)
	wg.Wait()
	requireOK(t, closeErr)
	members := map[string]bool{}
	for i := range joiners {
		switch {
		case joinErrs[i] == nil:
			members[tasks[i].ID] = true
		case !closedRefusal(joinErrs[i]):
			t.Fatalf("racing join %d err = %v", i, joinErrs[i])
		}
		switch {
		case intakeErrs[i] == nil:
			members[intake[i].ID] = true
		case !closedRefusal(intakeErrs[i]):
			t.Fatalf("racing intake %d err = %v", i, intakeErrs[i])
		}
	}
	record, err := x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	if record.State != store.CrateClosed || len(record.TaskIDs) != len(members) {
		t.Fatalf("crate after the race = %+v, want closed with %d members", record, len(members))
	}
	for _, id := range record.TaskIDs {
		if !members[id] {
			t.Fatalf("task %s joined without a successful act", id)
		}
	}
	for i := range joiners {
		if intakeErrs[i] != nil {
			if _, err := x.Backend.GetTask(x.Context, intake[i].ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused racing intake %s left a task: %v", intake[i].ID, err)
			}
		}
	}
	late := crateTask(t, x, core.TaskQueued, "")
	if _, err := x.Backend.SetTaskCrate(x.Context, late.ID, crate.ID); !closedRefusal(err) {
		t.Fatalf("join after the close err = %v", err)
	}
}

func runCrateCounts(t *testing.T, x Fixture) {
	empty, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Empty"})
	requireOK(t, err)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Counted"})
	requireOK(t, err)
	crateTask(t, x, core.TaskMerged, crate.ID)
	crateTask(t, x, core.TaskClosed, crate.ID)
	running := crateTask(t, x, core.TaskRunning, crate.ID)
	crateTask(t, x, core.TaskRunning, "")
	assertCounts := func(label string, got store.Crate, open, merged, total int, shipped bool) {
		t.Helper()
		if got.OpenTaskCount != open || got.MergedTaskCount != merged || got.TotalTaskCount != total || got.Shipped != shipped {
			t.Fatalf("%s counts open=%d merged=%d total=%d shipped=%v, want %d %d %d %v", label, got.OpenTaskCount, got.MergedTaskCount, got.TotalTaskCount, got.Shipped, open, merged, total, shipped)
		}
	}
	record, err := x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	assertCounts("with a running member", record.Crate, 1, 1, 3, false)
	if _, err := x.Backend.ClearTaskCrate(x.Context, running.ID); err != nil {
		t.Fatal(err)
	}
	record, err = x.Backend.GetCrate(x.Context, crate.ID)
	requireOK(t, err)
	assertCounts("merged and closed members", record.Crate, 0, 1, 2, true)
	listed, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{State: store.CrateOpen})
	requireOK(t, err)
	byID := map[string]store.Crate{}
	for _, row := range listed {
		byID[row.ID] = row
	}
	assertCounts("listed", byID[crate.ID], 0, 1, 2, true)
	if _, ok := byID[empty.ID]; !ok {
		t.Fatalf("open crate list = %+v, want the empty crate %s", listed, empty.ID)
	}
	assertCounts("listed empty", byID[empty.ID], 0, 0, 0, false)
	closedState := store.CrateClosed
	closed, err := x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: &closedState})
	requireOK(t, err)
	assertCounts("closed", closed, 0, 1, 2, false)
}

func runCrateListOrder(t *testing.T, x Fixture) {
	create := func(name string, date *string) store.Crate {
		crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: name, TargetDate: date})
		requireOK(t, err)
		return crate
	}
	late := create("Alpha", crateText("2027-06-01"))
	soonB := create("Bravo", crateText("2027-01-01"))
	soonA := create("Able", crateText("2027-01-01"))
	undatedZ := create("Zulu", nil)
	undatedC := create("Charlie", nil)
	twinA := create("Charlie", nil)
	closedState := store.CrateClosed
	closed := create("Closed", crateText("2026-01-01"))
	_, err := x.Backend.UpdateCrate(x.Context, closed.ID, store.CratePatch{State: &closedState})
	requireOK(t, err)
	charlies := []string{undatedC.ID, twinA.ID}
	slices.Sort(charlies)
	want := []string{soonA.ID, soonB.ID, late.ID, charlies[0], charlies[1], undatedZ.ID}
	listed, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{State: store.CrateOpen})
	requireOK(t, err)
	got := make([]string, len(listed))
	for i, crate := range listed {
		got[i] = crate.ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("open crate order = %v, want %v", got, want)
	}
	all, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{})
	requireOK(t, err)
	if len(all) != 7 {
		t.Fatalf("all crates = %d, want 7", len(all))
	}
	if all[0].ID != closed.ID {
		t.Fatalf("first crate = %s, want the closed 2026 crate %s", all[0].ID, closed.ID)
	}
}

func runCrateCoverage(t *testing.T, x Fixture) {
	declaredCovered := crateRequirement(t, x, true)
	declaredUncovered := crateRequirement(t, x, true)
	undeclared := crateRequirement(t, x, true)
	removed := crateRequirement(t, x, true)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Coverage", RequirementIDs: []string{declaredCovered, declaredUncovered}})
	requireOK(t, err)
	first := crateTask(t, x, core.TaskQueued, crate.ID)
	// A merged member counts too. A terminal task takes no context edit, so
	// it carries its requirements from creation.
	second := core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Merged member", Body: "Merged member.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
	second.Branch = "conveyor/task-" + second.ID
	requireOK(t, x.Backend.CreateTaskWithDependenciesAndContext(x.Context, second, nil, store.TaskContextInput{RequirementIDs: []string{declaredCovered, undeclared}}))
	outsider := crateTask(t, x, core.TaskQueued, "")
	for _, attach := range []struct{ task, requirement string }{
		{first.ID, declaredCovered}, {first.ID, removed}, {outsider.ID, declaredUncovered},
	} {
		_, err := x.Backend.UpdateTaskContext(x.Context, attach.task, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{attach.requirement}}})
		requireOK(t, err)
	}
	_, err = x.Backend.UpdateTaskContext(x.Context, first.ID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{removed}}})
	requireOK(t, err)
	detail, err := store.ReadCrateDetail(x.Context, x.Backend, crate.ID)
	requireOK(t, err)
	wantDeclared := map[string]int{declaredCovered: 2, declaredUncovered: 0}
	if len(detail.Requirements) != 2 {
		t.Fatalf("declared coverage = %+v", detail.Requirements)
	}
	for _, row := range detail.Requirements {
		count, ok := wantDeclared[row.ID]
		if !ok {
			t.Fatalf("declared coverage row %+v is not a declared requirement, or repeats one", row)
		}
		delete(wantDeclared, row.ID)
		if row.TaskCount != count || row.Title == "" {
			t.Fatalf("declared coverage row = %+v, want count %d and a title", row, count)
		}
	}
	if len(detail.UndeclaredRequirements) != 1 || detail.UndeclaredRequirements[0].ID != undeclared || detail.UndeclaredRequirements[0].TaskCount != 1 {
		t.Fatalf("undeclared coverage = %+v, want only %s with 1", detail.UndeclaredRequirements, undeclared)
	}
	if len(detail.Tasks) != 2 || detail.Tasks[0].ID != first.ID || detail.Tasks[1].ID != second.ID {
		t.Fatalf("detail members = %+v, want %s then %s", detail.Tasks, first.ID, second.ID)
	}
}

func runCrateActivity(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Activity"})
	requireOK(t, err)
	created := crateTask(t, x, core.TaskQueued, crate.ID)
	joined := crateTask(t, x, core.TaskQueued, "")
	_, err = x.Backend.SetTaskCrate(x.Context, joined.ID, crate.ID)
	requireOK(t, err)
	former := crateTask(t, x, core.TaskQueued, crate.ID)
	_, err = x.Backend.ClearTaskCrate(x.Context, former.ID)
	requireOK(t, err)
	outsider := crateTask(t, x, core.TaskQueued, "")
	for _, event := range []core.Event{
		{TaskID: created.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "url": "https://github.com/acme/widgets/pull/7", "repository": "acme/widgets"})},
		{TaskID: created.ID, Kind: "task.progress_reported", Payload: core.JSONPayload(map[string]any{"message": "not activity"})},
		{TaskID: joined.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approve", "review_round": 1})},
		{TaskID: outsider.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 8})},
	} {
		requireOK(t, x.Backend.AppendEvent(x.Context, event))
	}
	page, err := x.Backend.ListCrateActivity(x.Context, crate.ID, store.CrateActivityQuery{Limit: 50})
	requireOK(t, err)
	type item struct{ task, kind string }
	got := make([]item, len(page.Entries))
	titles := map[string]string{created.ID: created.Title, joined.ID: joined.Title}
	for i, entry := range page.Entries {
		got[i] = item{entry.Event.TaskID, entry.Event.Kind}
		if want, ok := titles[entry.Event.TaskID]; !ok || entry.TaskTitle != want {
			t.Fatalf("entry %d of task %s has title %q, want %q", i, entry.Event.TaskID, entry.TaskTitle, want)
		}
		if i > 0 {
			previous := page.Entries[i-1].Event
			if entry.Event.At.After(previous.At) || (entry.Event.At.Equal(previous.At) && entry.Event.ID > previous.ID) {
				t.Fatalf("activity is not newest first at %d: %+v after %+v", i, entry.Event, previous)
			}
		}
	}
	want := []item{
		{joined.ID, "review.round_completed"}, {created.ID, "pull_request.opened"},
		{joined.ID, store.TaskCrateSetEvent}, {joined.ID, "task.created"}, {created.ID, "task.created"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("activity = %+v, want %+v", got, want)
	}
	if page.NextCursor != "" {
		t.Fatalf("single page cursor = %q", page.NextCursor)
	}
	var paged []item
	cursor := ""
	ended := false
	for range 10 {
		next, err := x.Backend.ListCrateActivity(x.Context, crate.ID, store.CrateActivityQuery{Limit: 2, Cursor: cursor})
		requireOK(t, err)
		if len(next.Entries) > 2 {
			t.Fatalf("page holds %d entries, limit 2", len(next.Entries))
		}
		for _, entry := range next.Entries {
			paged = append(paged, item{entry.Event.TaskID, entry.Event.Kind})
		}
		if next.NextCursor == "" {
			ended = true
			break
		}
		cursor = next.NextCursor
	}
	if !ended {
		t.Fatal("activity pages never cleared their cursor")
	}
	if !slices.Equal(paged, want) {
		t.Fatalf("paged activity = %+v, want %+v", paged, want)
	}
	if _, err := x.Backend.ListCrateActivity(x.Context, crate.ID, store.CrateActivityQuery{Limit: 0}); !fieldRefusal("limit")(err) {
		t.Fatalf("zero limit err = %v", err)
	}
	if _, err := x.Backend.ListCrateActivity(x.Context, crate.ID, store.CrateActivityQuery{Limit: 5, Cursor: "%%%"}); !fieldRefusal("cursor")(err) {
		t.Fatalf("bad cursor err = %v", err)
	}
}

func runCrateTaskFilter(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Filter"})
	requireOK(t, err)
	other, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Other"})
	requireOK(t, err)
	queued := crateTask(t, x, core.TaskQueued, crate.ID)
	merged := crateTask(t, x, core.TaskMerged, crate.ID)
	crateTask(t, x, core.TaskQueued, other.ID)
	crateTask(t, x, core.TaskQueued, "")
	ids := func(tasks []core.Task) []string {
		out := make([]string, len(tasks))
		for i, task := range tasks {
			out[i] = task.ID
		}
		slices.Sort(out)
		return out
	}
	want := []string{queued.ID, merged.ID}
	slices.Sort(want)
	members, err := x.Backend.ListTasksFiltered(x.Context, store.TaskFilter{Crate: crate.ID})
	requireOK(t, err)
	if got := ids(members); !slices.Equal(got, want) {
		t.Fatalf("crate filter = %v, want %v", got, want)
	}
	for _, member := range members {
		if member.TaskCrateID() != crate.ID {
			t.Fatalf("member %s carries crate %v", member.ID, member.Crate)
		}
	}
	composed, err := x.Backend.ListTasksFiltered(x.Context, store.TaskFilter{Crate: crate.ID, States: []core.TaskState{core.TaskQueued}})
	requireOK(t, err)
	if got := ids(composed); !slices.Equal(got, []string{queued.ID}) {
		t.Fatalf("crate and state filter = %v, want %s", got, queued.ID)
	}
	page, err := x.Backend.ListTaskPage(x.Context, store.TaskOperationsQuery{TaskFilter: store.TaskFilter{Crate: crate.ID}, Limit: 1})
	requireOK(t, err)
	if page.Total != 2 || len(page.Tasks) != 1 || page.Tasks[0].TaskCrateID() != crate.ID || !slices.Contains(want, page.Tasks[0].ID) {
		t.Fatalf("crate filter page total=%d rows=%+v, want 2 and one member of %s", page.Total, page.Tasks, crate.ID)
	}
	none, err := x.Backend.ListTasksFiltered(x.Context, store.TaskFilter{Crate: "crate-000000-000000"})
	requireOK(t, err)
	if len(none) != 0 {
		t.Fatalf("unknown crate filter = %v", ids(none))
	}
	if !(store.TaskFilter{Crate: crate.ID}).Active() {
		t.Fatal("a crate filter must be active")
	}
}

func runCrateIntake(t *testing.T, x Fixture) {
	requirement := crateRequirement(t, x, true)
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Intake", RequirementIDs: []string{requirement}})
	requireOK(t, err)
	task := core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Intake member", Body: "Intake member.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, x.Backend.CreateTaskWithDependenciesAndContext(x.Context, task, nil, store.TaskContextInput{}))
	read, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if read.TaskCrateID() != crate.ID {
		t.Fatalf("intake member crate = %v", read.Crate)
	}
	// A crate attaches no requirement by itself (req-crates AC-3.1).
	active, err := store.TaskContextForTask(x.Context, x.Backend, task.ID)
	requireOK(t, err)
	if len(active.Requirements) != 0 {
		t.Fatalf("intake member inherited requirements: %+v", active.Requirements)
	}
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	var recorded store.TaskCreatedEvent
	for _, event := range events {
		if event.Kind == "task.created" {
			requireOK(t, jsonUnmarshal(event.Payload, &recorded))
		}
		if event.Kind == store.TaskCrateSetEvent {
			t.Fatalf("intake recorded a separate membership event: %+v", event)
		}
	}
	if recorded.CrateID != crate.ID {
		t.Fatalf("task.created crate_id = %q, want %s", recorded.CrateID, crate.ID)
	}
	// Multipart intake validates the crate in the same transaction.
	upload := core.Task{ID: fixtureTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Upload member", Body: "Upload member.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
	upload.Branch = "conveyor/task-" + upload.ID
	requireOK(t, x.Backend.CreateTaskWithAttachments(x.Context, upload, nil, store.TaskContextInput{RequirementIDs: []string{requirement}}, []store.ArtifactUpload{{Name: "note.txt", ContentType: "text/plain", Content: []byte("note")}}))
	if read, err := x.Backend.GetTask(x.Context, upload.ID); err != nil || read.TaskCrateID() != crate.ID {
		t.Fatalf("multipart member crate = %v %v", read.Crate, err)
	}
	closedState := store.CrateClosed
	_, err = x.Backend.UpdateCrate(x.Context, crate.ID, store.CratePatch{State: &closedState})
	requireOK(t, err)
	refused := upload
	refused.ID = fixtureTaskID()
	refused.Branch = "conveyor/task-" + refused.ID
	if err := x.Backend.CreateTaskWithAttachments(x.Context, refused, nil, store.TaskContextInput{}, []store.ArtifactUpload{{Name: "note.txt", ContentType: "text/plain", Content: []byte("note")}}); !closedRefusal(err) {
		t.Fatalf("multipart intake into a closed crate err = %v", err)
	}
	if _, err := x.Backend.GetTask(x.Context, refused.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused multipart intake left a task: %v", err)
	}
	artifacts, err := x.Backend.ListArtifacts(x.Context)
	requireOK(t, err)
	for _, artifact := range artifacts {
		if artifact.TaskID == refused.ID {
			t.Fatalf("refused multipart intake left an artifact: %+v", artifact)
		}
	}
}

// runCrateMemberRead holds ListCrateMembers to its contract: each listed
// crate's member tasks with ID, title, and state, ordered by creation time and
// then ID, no entry for a crate with no member or of another workspace, and
// the membership as it stands at the read (component-crates invariants 14 and
// 20).
func runCrateMemberRead(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Members"})
	requireOK(t, err)
	other, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Other members"})
	requireOK(t, err)
	empty, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "No members"})
	requireOK(t, err)
	base := store.CrateTime(time.Now().Add(-time.Hour))
	member := func(state core.TaskState, crateID string, at time.Time) core.Task {
		t.Helper()
		id := fixtureTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Member read " + id, Body: "Member read.", State: state, NextStage: core.StageImplement, CreatedAt: at, Branch: "conveyor/task-" + id}
		if crateID != "" {
			task.Crate = &core.TaskCrate{ID: crateID}
		}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		return task
	}
	// Created last but at the earliest time, so creation time, not insertion,
	// orders it first. Two members share one time, so the ID breaks the tie.
	late := member(core.TaskQueued, crate.ID, base.Add(2*time.Second))
	tieA := member(core.TaskMerged, crate.ID, base.Add(time.Second))
	tieB := member(core.TaskRunning, crate.ID, base.Add(time.Second))
	first := member(core.TaskClosed, crate.ID, base)
	otherMember := member(core.TaskQueued, other.ID, base)
	member(core.TaskQueued, "", base)
	ties := []core.Task{tieA, tieB}
	slices.SortFunc(ties, func(a, b core.Task) int { return strings.Compare(a.ID, b.ID) })
	want := []store.CrateMemberTask{}
	for _, task := range []core.Task{first, ties[0], ties[1], late} {
		want = append(want, store.CrateMemberTask{ID: task.ID, Title: task.Title, State: task.State})
	}
	members, err := x.Backend.ListCrateMembers(x.Context, []string{crate.ID, other.ID, empty.ID, "crate-000000-000000"})
	requireOK(t, err)
	if !slices.Equal(members[crate.ID], want) {
		t.Fatalf("crate members = %+v, want %+v", members[crate.ID], want)
	}
	if got := members[other.ID]; len(got) != 1 || got[0] != (store.CrateMemberTask{ID: otherMember.ID, Title: otherMember.Title, State: otherMember.State}) {
		t.Fatalf("other crate members = %+v, want only %s", got, otherMember.ID)
	}
	if _, found := members[empty.ID]; found || len(members) != 2 {
		t.Fatalf("member read holds %d crates (%v), want entries only for the two crates with members", len(members), members)
	}
	none, err := x.Backend.ListCrateMembers(x.Context, nil)
	requireOK(t, err)
	if len(none) != 0 {
		t.Fatalf("member read of no crate = %v", none)
	}
	foreign, err := x.Backend.ListCrateMembers(store.WithWorkspace(x.Context, "ws-crate-member-read-other"), []string{crate.ID})
	requireOK(t, err)
	if len(foreign) != 0 {
		t.Fatalf("member read from another workspace = %v, want nothing", foreign)
	}
	_, err = x.Backend.ClearTaskCrate(x.Context, first.ID)
	requireOK(t, err)
	members, err = x.Backend.ListCrateMembers(x.Context, []string{crate.ID})
	requireOK(t, err)
	if !slices.Equal(members[crate.ID], want[1:]) {
		t.Fatalf("crate members after a removal = %+v, want %+v", members[crate.ID], want[1:])
	}
}

// runCrateMembersLimited holds ListCrateMembersLimited to its contract: at
// most limit members of one crate in creation time and then ID order, the
// named task left out before the limit, more true only when another eligible
// member follows, and no member for an empty, unknown, or foreign crate or
// an empty crate ID (component-crates invariant 20; DEC-105(4)). Members are inserted out of
// order, two share one creation time, and the excluded task sits inside the
// ordered prefix. With three eligible members, limits 4, 3, and 2 put the
// member count one under, exactly at, and one over the limit.
func runCrateMembersLimited(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Limited members"})
	requireOK(t, err)
	other, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Other limited members"})
	requireOK(t, err)
	empty, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "No limited members"})
	requireOK(t, err)
	base := store.CrateTime(time.Now().Add(-time.Hour))
	member := func(crateID string, at time.Time) core.Task {
		t.Helper()
		id := fixtureTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Limited member " + id, Body: "Limited member.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: at, Branch: "conveyor/task-" + id}
		if crateID != "" {
			task.Crate = &core.TaskCrate{ID: crateID}
		}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		return task
	}
	tieA := member(crate.ID, base.Add(2*time.Second))
	tieB := member(crate.ID, base.Add(2*time.Second))
	excluded := member(crate.ID, base.Add(time.Second))
	first := member(crate.ID, base)
	// A member of another crate and a task in no crate, both created before
	// every member, must never appear.
	member(other.ID, base.Add(-time.Second))
	member("", base.Add(-time.Second))
	ties := []core.Task{tieA, tieB}
	slices.SortFunc(ties, func(a, b core.Task) int { return strings.Compare(a.ID, b.ID) })
	project := func(tasks ...core.Task) []store.CrateMemberTask {
		out := make([]store.CrateMemberTask, len(tasks))
		for i, task := range tasks {
			out[i] = store.CrateMemberTask{ID: task.ID, Title: task.Title, State: task.State}
		}
		return out
	}
	check := func(label string, ctx context.Context, crateID, exclude string, limit int, want []store.CrateMemberTask, wantMore bool) {
		t.Helper()
		got, more, err := x.Backend.ListCrateMembersLimited(ctx, crateID, exclude, limit)
		requireOK(t, err)
		if len(got) != len(want) || (len(want) > 0 && !slices.Equal(got, want)) || more != wantMore {
			t.Fatalf("%s: members = %+v more = %t, want %+v more = %t", label, got, more, want, wantMore)
		}
	}
	eligible := project(first, ties[0], ties[1])
	check("count one under the limit", x.Context, crate.ID, excluded.ID, 4, eligible, false)
	check("count exactly at the limit", x.Context, crate.ID, excluded.ID, 3, eligible, false)
	check("count one over the limit", x.Context, crate.ID, excluded.ID, 2, eligible[:2], true)
	check("a limit of one", x.Context, crate.ID, excluded.ID, 1, eligible[:1], true)

	// An empty exclusion leaves every member eligible, the excluded task
	// included, in the same order.
	all := project(first, excluded, ties[0], ties[1])
	check("no exclusion, count one under the limit", x.Context, crate.ID, "", 5, all, false)
	check("no exclusion, count exactly at the limit", x.Context, crate.ID, "", 4, all, false)
	check("no exclusion, count one over the limit", x.Context, crate.ID, "", 3, all[:3], true)
	check("exclusion of a nonmember", x.Context, crate.ID, "task-000000-000000", 4, all, false)

	check("empty crate", x.Context, empty.ID, "", 10, nil, false)
	check("unknown crate", x.Context, "crate-000000-000000", "", 10, nil, false)
	// A task in no crate exists, so an empty crate ID must not match it.
	check("empty crate ID", x.Context, "", "", 10, nil, false)
	check("crate of another workspace", store.WithWorkspace(x.Context, "ws-crate-member-limited-other"), crate.ID, "", 10, nil, false)

	for _, limit := range []int{0, -1, math.MaxInt} {
		_, _, err := x.Backend.ListCrateMembersLimited(x.Context, crate.ID, "", limit)
		var field *store.CrateFieldError
		if !errors.As(err, &field) || field.Field != "limit" {
			t.Fatalf("limit %d: error = %v, want a limit CrateFieldError", limit, err)
		}
	}

	// The read reflects membership as it stands.
	_, err = x.Backend.ClearTaskCrate(x.Context, first.ID)
	requireOK(t, err)
	check("after a removal", x.Context, crate.ID, excluded.ID, 2, project(ties[0], ties[1]), false)
}

func jsonUnmarshal(raw []byte, target any) error { return json.Unmarshal(raw, target) }

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	requireOK(t, err)
	right, err := json.Marshal(b)
	requireOK(t, err)
	return string(left) == string(right)
}

// insightEvent is one event as the insight read returns it, without its ID.
type insightEvent struct {
	Kind    string
	At      time.Time
	Stage   core.Stage
	Payload string
}

func insightEvents(t *testing.T, member store.CrateInsightMember) []insightEvent {
	t.Helper()
	out := make([]insightEvent, len(member.Events))
	for i, event := range member.Events {
		if i > 0 {
			previous := member.Events[i-1]
			if event.At.Before(previous.At) || (event.At.Equal(previous.At) && event.ID <= previous.ID) {
				t.Fatalf("member %s events are not in (at, id) order at %d: %+v after %+v", member.Task.ID, i, event, previous)
			}
		}
		payload := ""
		if len(event.Payload) > 0 {
			var decoded map[string]any
			requireOK(t, json.Unmarshal(event.Payload, &decoded))
			if value, ok := decoded["marker"].(string); ok {
				payload = value
			} else {
				payload = "payload"
			}
		}
		out[i] = insightEvent{Kind: event.Kind, At: event.At.UTC(), Stage: event.Stage, Payload: payload}
	}
	return out
}

// crateInsightDepend records that task depends on dependency through the
// public dependency write.
func crateInsightDepend(t *testing.T, x Fixture, task, dependency core.Task) {
	t.Helper()
	result, err := x.Backend.AddTaskDependency(x.Context, store.DependencyAdditionRequest{TaskID: task.ID, DependsOnTaskID: dependency.ID, Reason: "crate insight dependency", RequestID: "crate-insight-" + task.ID + "-" + dependency.ID})
	requireOK(t, err)
	if !result.Added {
		t.Fatalf("dependency %s on %s was not added", task.ID, dependency.ID)
	}
}

// crateInsightPerform applies one lifecycle command through the command
// plane, as task_map's fixtures move a dependency to merged or closed after
// the edge exists.
func crateInsightPerform(t *testing.T, x Fixture, task core.Task, command core.TaskCommand) {
	t.Helper()
	if _, err := taskops.New(x.Backend).Perform(x.Context, task.ID, taskops.Command{Kind: command}); err != nil {
		t.Fatalf("%s %s: %v", task.ID, command, err)
	}
}

// crateInsightDependencyOf is the dependency row the read returns for task
// in state.
func crateInsightDependencyOf(task core.Task, state core.TaskState) store.CrateInsightDependency {
	return store.CrateInsightDependency{ID: task.ID, Title: task.Title, State: state}
}

// crateInsightDependencies sorts the expected rows by ID, the read's order.
func crateInsightDependencies(rows ...store.CrateInsightDependency) []store.CrateInsightDependency {
	out := append([]store.CrateInsightDependency{}, rows...)
	slices.SortFunc(out, func(a, b store.CrateInsightDependency) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// runCrateInsightInputs holds invariant 22: the read returns the crate row
// and, for each current member in creation order, its task row, its
// dependencies in ID order with each target's ID, title, and state, and its
// listed events in (at, id) order, including events from before it joined,
// each claim with its order's stage and no payload. A dependency outside
// the crate, a merged one, and a closed one are included. A former member,
// an outsider, their dependency edges, and unlisted kinds are absent. An
// empty crate reads with no member, and a missing or foreign crate is not
// found.
func runCrateInsightInputs(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Insight", TargetDate: crateText("2026-12-31")})
	requireOK(t, err)
	other, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Insight elsewhere"})
	requireOK(t, err)
	base := store.CrateTime(time.Now().Add(-48 * time.Hour))
	member := func(state core.TaskState, stage core.Stage, crateID, base string, created time.Time) core.Task {
		t.Helper()
		id := fixtureTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Insight member " + id, Body: "Insight member body.", State: state, NextStage: stage, CreatedAt: created, BaseBranch: base, Branch: "conveyor/task-" + id}
		if crateID != "" {
			task.Crate = &core.TaskCrate{ID: crateID}
		}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		return task
	}
	appendAt := func(task core.Task, kind string, at time.Time, payload any, job string) {
		t.Helper()
		requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: task.ID, JobID: job, Kind: kind, At: at.UTC(), Payload: core.JSONPayload(payload)}))
	}
	marker := func(value string) map[string]any { return map[string]any{"marker": value} }

	// The building member: created in the crate, claimed for implement, a
	// pull request, a review round that asked for changes, and an unlisted
	// progress report.
	building := member(core.TaskRunning, core.StageImplement, crate.ID, "main", base.Add(2*time.Hour))
	implement := core.WorkOrder{ID: building.ID + "-implement-1", TaskID: building.ID, JobID: building.ID + "-implement-1", Stage: core.StageImplement, State: core.WorkOrderClaimed}
	appendAt(building, "work_order.claimed", base.Add(3*time.Hour), store.WorkOrderEventPayload(implement), "")
	appendAt(building, "task.progress_reported", base.Add(3*time.Hour+time.Minute), marker("unlisted"), "")
	appendAt(building, "pull_request.opened", base.Add(4*time.Hour), map[string]any{"marker": "pull", "number": 7}, "")
	appendAt(building, "review.round_completed", base.Add(5*time.Hour), map[string]any{"marker": "round", "verdict": "changes_requested"}, "")

	// The joined member: it starts in the other crate with a plan state
	// change and a legacy claim whose payload names only its review order,
	// then joins this crate.
	joined := member(core.TaskRunning, core.StageReview, other.ID, "develop", base.Add(time.Hour))
	review := core.WorkOrder{ID: joined.ID + "-review-1", TaskID: joined.ID, JobID: joined.ID + "-review-1", Stage: core.StageReview, State: core.WorkOrderQueued,
		QueueEnteredAt: base, QueueDeadline: base.Add(72 * time.Hour)}
	requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: review.JobID, TaskID: joined.ID, Stage: core.StageReview, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(x.Context, x.Backend, review))
	appendAt(joined, core.EventTaskPlanStateChanged, base.Add(90*time.Minute), map[string]any{"marker": "plan", "to": "awaiting_approval", "cause": "plan_submitted"}, "")
	appendAt(joined, "work_order.claimed", base.Add(100*time.Minute), map[string]any{"id": review.ID}, review.JobID)
	_, err = x.Backend.SetTaskCrate(x.Context, joined.ID, crate.ID)
	requireOK(t, err)

	// The merged and closed members, and one member with the same creation
	// time as the merged one so the ID breaks the tie.
	merged := member(core.TaskMerged, "", crate.ID, "main", base.Add(3*time.Hour))
	appendAt(merged, "merge.confirmed", base.Add(6*time.Hour), marker("merge"), "")
	appendAt(merged, "task.state_changed", base.Add(6*time.Hour+time.Second), map[string]any{"marker": "merged", "from": "approved", "to": "merged"}, "")
	tie := member(core.TaskQueued, core.StageSpec, crate.ID, "main", base.Add(3*time.Hour))
	closed := member(core.TaskClosed, core.StageImplement, crate.ID, "main", base.Add(4*time.Hour))
	appendAt(closed, "task.cancelled", base.Add(7*time.Hour), marker("cancel"), "")

	// A former member and an outsider: neither appears, nor do their events.
	former := member(core.TaskQueued, core.StageImplement, crate.ID, "main", base)
	appendAt(former, "pull_request.opened", base.Add(time.Hour), marker("former"), "")
	_, err = x.Backend.ClearTaskCrate(x.Context, former.ID)
	requireOK(t, err)
	outsider := member(core.TaskQueued, core.StageImplement, "", "main", base)
	appendAt(outsider, "pull_request.opened", base.Add(time.Hour), marker("outsider"), "")

	// Dependencies: the building member depends on a queued member, a task
	// outside the crate that later merges, and one that later closes. The
	// joined member depends on the building member, the former member, and
	// an open task outside the crate. The outsider's and the former
	// member's own edges belong to no member.
	outsideOpen := member(core.TaskQueued, core.StageImplement, "", "main", base)
	outsideMerged := member(core.TaskQueued, core.StageImplement, "", "main", base)
	outsideClosed := member(core.TaskQueued, core.StageImplement, "", "main", base)
	crateInsightDepend(t, x, building, tie)
	crateInsightDepend(t, x, building, outsideMerged)
	crateInsightDepend(t, x, building, outsideClosed)
	crateInsightDepend(t, x, joined, building)
	crateInsightDepend(t, x, joined, former)
	crateInsightDepend(t, x, joined, outsideOpen)
	crateInsightDepend(t, x, outsider, building)
	crateInsightDepend(t, x, former, tie)
	crateInsightPerform(t, x, outsideMerged, core.TaskMergeRecover)
	crateInsightPerform(t, x, outsideClosed, core.TaskCancel)

	inputs, err := x.Backend.ReadCrateInsightInputs(x.Context, crate.ID)
	requireOK(t, err)
	if inputs.Crate.ID != crate.ID || inputs.Crate.Name != "Insight" || inputs.Crate.TargetDate == nil || *inputs.Crate.TargetDate != "2026-12-31" ||
		inputs.Crate.State != string(store.CrateOpen) || inputs.Crate.TotalTaskCount != 5 || inputs.Crate.MergedTaskCount != 1 || inputs.Crate.OpenTaskCount != 3 {
		t.Fatalf("crate row %+v", inputs.Crate)
	}
	ties := []core.Task{merged, tie}
	slices.SortFunc(ties, func(a, b core.Task) int { return strings.Compare(a.ID, b.ID) })
	wantOrder := []core.Task{joined, building, ties[0], ties[1], closed}
	if len(inputs.Members) != len(wantOrder) {
		t.Fatalf("read %d members, want %d", len(inputs.Members), len(wantOrder))
	}
	byID := map[string]store.CrateInsightMember{}
	for i, got := range inputs.Members {
		want := wantOrder[i]
		if got.Task.ID != want.ID || got.Task.BaseBranch != want.BaseBranch || got.Task.NextStage != want.NextStage || got.Task.State != want.State ||
			!got.Task.CreatedAt.Equal(want.CreatedAt) || got.Task.Branch != want.Branch || got.Task.TaskCrateID() != crate.ID {
			t.Fatalf("member %d = %+v, want task %s (%s from %s, next %s, %s)", i, got.Task, want.ID, want.State, want.BaseBranch, want.NextStage, want.Branch)
		}
		byID[got.Task.ID] = got
	}
	wantDependencies := map[string][]store.CrateInsightDependency{
		building.ID: crateInsightDependencies(crateInsightDependencyOf(tie, core.TaskQueued), crateInsightDependencyOf(outsideMerged, core.TaskMerged), crateInsightDependencyOf(outsideClosed, core.TaskClosed)),
		joined.ID:   crateInsightDependencies(crateInsightDependencyOf(building, core.TaskRunning), crateInsightDependencyOf(former, core.TaskQueued), crateInsightDependencyOf(outsideOpen, core.TaskQueued)),
		merged.ID:   {},
		tie.ID:      {},
		closed.ID:   {},
	}
	for id, want := range wantDependencies {
		got := byID[id].Dependencies
		if got == nil || !slices.Equal(got, want) {
			t.Fatalf("member %s dependencies\n got %+v\nwant %+v", id, got, want)
		}
	}
	kinds := func(member store.CrateInsightMember) []string {
		out := []string{}
		for _, event := range insightEvents(t, member) {
			out = append(out, event.Kind)
		}
		return out
	}
	// Creation records its own events, such as task.created and the intake
	// plan state, and on some backends a membership event. The case compares
	// only the events it appended: claims and payloads with a marker.
	recorded := func(member store.CrateInsightMember) []insightEvent {
		out := []insightEvent{}
		for _, event := range insightEvents(t, member) {
			if event.Kind == "work_order.claimed" || (event.Payload != "" && event.Payload != "payload") {
				out = append(out, event)
			}
		}
		return out
	}
	wantBuilding := []insightEvent{
		{Kind: "work_order.claimed", At: base.Add(3 * time.Hour), Stage: core.StageImplement},
		{Kind: "pull_request.opened", At: base.Add(4 * time.Hour), Payload: "pull"},
		{Kind: "review.round_completed", At: base.Add(5 * time.Hour), Payload: "round"},
	}
	if got := recorded(byID[building.ID]); !slices.Equal(got, wantBuilding) {
		t.Fatalf("building member events:\n got %+v\nwant %+v", got, wantBuilding)
	}
	wantMerged := []insightEvent{
		{Kind: "merge.confirmed", At: base.Add(6 * time.Hour), Payload: "merge"},
		{Kind: "task.state_changed", At: base.Add(6*time.Hour + time.Second), Payload: "merged"},
	}
	if got := recorded(byID[merged.ID]); !slices.Equal(got, wantMerged) {
		t.Fatalf("merged member events:\n got %+v\nwant %+v", got, wantMerged)
	}
	if got, want := recorded(byID[closed.ID]), []insightEvent{{Kind: "task.cancelled", At: base.Add(7 * time.Hour), Payload: "cancel"}}; !slices.Equal(got, want) {
		t.Fatalf("closed member events:\n got %+v\nwant %+v", got, want)
	}
	if got := recorded(byID[tie.ID]); len(got) != 0 {
		t.Fatalf("queued member events %+v, want none beyond creation", got)
	}
	if got := kinds(byID[joined.ID]); !slices.Contains(got, store.TaskCrateSetEvent) {
		t.Fatalf("joined member kinds %v lack its join", got)
	}
	gotJoined := insightEvents(t, byID[joined.ID])
	var claims, plans, crateSets int
	for _, event := range gotJoined {
		switch event.Kind {
		case "work_order.claimed":
			claims++
			if event.Stage != core.StageReview || event.Payload != "" || !event.At.Equal(base.Add(100*time.Minute)) {
				t.Fatalf("legacy claim %+v, want the review stage from its order and no payload", event)
			}
		case core.EventTaskPlanStateChanged:
			if event.Payload == "plan" {
				plans++
			}
		case store.TaskCrateSetEvent:
			crateSets++
		case "task.created":
		default:
			t.Fatalf("joined member holds unexpected event %+v", event)
		}
	}
	if claims != 1 || plans != 1 || crateSets < 1 {
		t.Fatalf("joined member events %+v: want the legacy claim, the plan state change from before it joined, and both crate joins", gotJoined)
	}
	for _, got := range inputs.Members {
		for _, event := range got.Events {
			if !slices.Contains(store.CrateInsightEventKinds, event.Kind) {
				t.Fatalf("member %s holds unlisted kind %s", got.Task.ID, event.Kind)
			}
			if event.Kind != "work_order.claimed" && event.Stage != "" {
				t.Fatalf("event %+v carries a stage", event)
			}
		}
	}

	empty, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Insight empty"})
	requireOK(t, err)
	none, err := x.Backend.ReadCrateInsightInputs(x.Context, empty.ID)
	requireOK(t, err)
	if none.Crate.ID != empty.ID || none.Members == nil || len(none.Members) != 0 || none.Crate.TotalTaskCount != 0 {
		t.Fatalf("empty crate inputs %+v", none)
	}
	if _, err := x.Backend.ReadCrateInsightInputs(x.Context, "crate-000000-000000"); !notFoundRefusal(err) {
		t.Fatalf("missing crate err = %v", err)
	}
	if _, err := x.Backend.ReadCrateInsightInputs(store.WithWorkspace(x.Context, "ws-crate-insight-other"), crate.ID); !notFoundRefusal(err) {
		t.Fatalf("foreign crate err = %v", err)
	}
}

// seedCrateInsight creates an open crate with size members of one fixed
// shape: every third member merged with a merge and a state change, every
// fifth closed, and the rest open with a claim, a pull request, and a
// review round.
func seedCrateInsight(t *testing.T, x Fixture, size int) string {
	t.Helper()
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: fmt.Sprintf("Insight scale %d", size)})
	requireOK(t, err)
	base := store.CrateTime(time.Now().Add(-24 * time.Hour))
	// Every open member depends on one open task outside the crate and on
	// the open member before it, so the read carries edges at every size.
	outsideID := fixtureTaskID()
	outside := core.Task{ID: outsideID, Workspace: x.Workspace, Repo: "conveyor", Title: "Insight scale outside " + outsideID, Body: "Insight scale.", State: core.TaskQueued, NextStage: core.StageImplement,
		CreatedAt: base, BaseBranch: "main", Branch: "conveyor/task-" + outsideID}
	requireOK(t, x.Backend.CreateTask(x.Context, outside))
	var previous *core.Task
	for i := range size {
		id := fixtureTaskID()
		state := core.TaskRunning
		switch {
		case i%3 == 0:
			state = core.TaskMerged
		case i%5 == 0:
			state = core.TaskClosed
		}
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Insight scale " + id, Body: "Insight scale.", State: state, NextStage: core.StageImplement,
			CreatedAt: base.Add(time.Duration(i) * time.Second), BaseBranch: "main", Branch: "conveyor/task-" + id, Crate: &core.TaskCrate{ID: crate.ID}}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		if state == core.TaskRunning {
			crateInsightDepend(t, x, task, outside)
			if previous != nil {
				crateInsightDepend(t, x, task, *previous)
			}
			previous = &task
		}
		order := core.WorkOrder{ID: id + "-implement-1", TaskID: id, JobID: id + "-implement-1", Stage: core.StageImplement, State: core.WorkOrderClaimed}
		for _, event := range []core.Event{
			{TaskID: id, Kind: "work_order.claimed", At: base.Add(time.Minute), Payload: store.WorkOrderEventPayload(order)},
			{TaskID: id, Kind: "pull_request.opened", At: base.Add(2 * time.Minute), Payload: core.JSONPayload(map[string]any{"number": i + 1})},
			{TaskID: id, Kind: "review.round_completed", At: base.Add(3 * time.Minute), Payload: core.JSONPayload(map[string]any{"verdict": "approve"})},
		} {
			requireOK(t, x.Backend.AppendEvent(x.Context, event))
		}
		if state == core.TaskMerged {
			requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: id, Kind: "merge.confirmed", At: base.Add(4 * time.Minute), Payload: core.JSONPayload(map[string]any{"sha": "abc"})}))
			requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: id, Kind: "task.state_changed", At: base.Add(4 * time.Minute), Payload: core.JSONPayload(map[string]any{"from": "approved", "to": "merged"})}))
		}
	}
	return crate.ID
}

// runCrateInsightInputsBounded holds invariant 22's statement bound on the
// SQL backends: the insight read issues the same number of statements for
// a crate of 4 members and one of 60, counting only the read itself. Fixture
// setup and the next-action projection run under other read labels and are
// drained before the read.
func runCrateInsightInputsBounded(t *testing.T, x Fixture, productionCapable bool) {
	if x.DrainReadQueries == nil {
		if productionCapable {
			t.Fatal("production-capable conformance fixture must provide DrainReadQueries")
		}
		t.Skip("the memory backend runs no SQL statements")
	}
	counts := map[int]uint64{}
	for _, size := range []int{4, 60} {
		crateID := seedCrateInsight(t, x, size)
		x.DrainReadQueries()
		inputs, err := x.Backend.ReadCrateInsightInputs(x.Context, crateID)
		statements := x.DrainReadQueries()["ReadCrateInsightInputs"]
		requireOK(t, err)
		if len(inputs.Members) != size {
			t.Fatalf("size %d: read %d members", size, len(inputs.Members))
		}
		edges := 0
		for _, member := range inputs.Members {
			if len(member.Events) < 3 {
				t.Fatalf("size %d: member %s holds %d events", size, member.Task.ID, len(member.Events))
			}
			if member.Dependencies == nil {
				t.Fatalf("size %d: member %s holds nil dependencies", size, member.Task.ID)
			}
			if member.Task.State == core.TaskRunning && len(member.Dependencies) == 0 {
				t.Fatalf("size %d: open member %s holds no dependency", size, member.Task.ID)
			}
			edges += len(member.Dependencies)
		}
		if edges == 0 {
			t.Fatalf("size %d: the read carries no dependency edge", size)
		}
		counts[size] = statements
		t.Logf("crate insight inputs: %d members, %d dependency edges, %d statements", size, edges, statements)
	}
	if counts[4] == 0 || counts[4] != counts[60] {
		t.Fatalf("insight read statements %v, want one equal nonzero count for 4 and 60 members", counts)
	}
}

// runCrateInsightInputsMemberLeaves holds invariant 22 when a member leaves
// the crate between the insight read's member-row statement and its later
// statements: the read returns that member with every event recorded before
// the read, because the later statements read the member IDs the first one
// returned. A read after the move no longer lists the member. The SQL
// backends run store.CrateInsightMembersReadHook between the statements.
// The memory backend reads under one lock, so the move cannot interleave
// and lands after the read.
func runCrateInsightInputsMemberLeaves(t *testing.T, x Fixture, productionCapable bool) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Insight member leaves"})
	requireOK(t, err)
	base := store.CrateTime(time.Now().Add(-24 * time.Hour))
	member := func(created time.Time) core.Task {
		t.Helper()
		id := fixtureTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Insight leaver " + id, Body: "Insight leaver.", State: core.TaskRunning, NextStage: core.StageReview,
			CreatedAt: created, BaseBranch: "main", Branch: "conveyor/task-" + id, Crate: &core.TaskCrate{ID: crate.ID}}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
		return task
	}
	leaver := member(base)
	stayer := member(base.Add(time.Second))
	// The leaver depends on an open task outside the crate, and the stayer
	// depends on the leaver.
	anchorID := fixtureTaskID()
	anchor := core.Task{ID: anchorID, Workspace: x.Workspace, Repo: "conveyor", Title: "Insight anchor " + anchorID, Body: "Insight anchor.", State: core.TaskQueued, NextStage: core.StageImplement,
		CreatedAt: base, BaseBranch: "main", Branch: "conveyor/task-" + anchorID}
	requireOK(t, x.Backend.CreateTask(x.Context, anchor))
	crateInsightDepend(t, x, leaver, anchor)
	crateInsightDepend(t, x, stayer, leaver)
	wantDependencies := map[string][]store.CrateInsightDependency{
		leaver.ID: {crateInsightDependencyOf(anchor, core.TaskQueued)},
		stayer.ID: {crateInsightDependencyOf(leaver, core.TaskRunning)},
	}
	for _, task := range []core.Task{leaver, stayer} {
		requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: task.ID, Kind: "pull_request.opened", At: base.Add(time.Hour), Payload: core.JSONPayload(map[string]any{"marker": "pull", "number": 3})}))
		requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: task.ID, Kind: "review.round_completed", At: base.Add(2 * time.Hour), Payload: core.JSONPayload(map[string]any{"marker": "round", "verdict": "changes_requested"})}))
	}
	moved := false
	store.CrateInsightMembersReadHook = func() {
		if moved {
			return
		}
		moved = true
		if _, err := x.Backend.ClearTaskCrate(x.Context, leaver.ID); err != nil {
			t.Errorf("move the member out between the insight statements: %v", err)
		}
	}
	inputs, err := x.Backend.ReadCrateInsightInputs(x.Context, crate.ID)
	store.CrateInsightMembersReadHook = nil
	requireOK(t, err)
	if productionCapable && !moved {
		t.Fatal("a production-capable backend must run CrateInsightMembersReadHook between its member-row statement and its later statements")
	}
	recorded := func(m store.CrateInsightMember) []string {
		out := []string{}
		for _, event := range m.Events {
			var payload map[string]any
			if len(event.Payload) > 0 && json.Unmarshal(event.Payload, &payload) == nil {
				if marker, ok := payload["marker"].(string); ok {
					out = append(out, event.Kind+":"+marker)
				}
			}
		}
		return out
	}
	want := []string{"pull_request.opened:pull", "review.round_completed:round"}
	if len(inputs.Members) != 2 {
		t.Fatalf("read %d members, want the leaver and the stayer captured by the member-row statement", len(inputs.Members))
	}
	for _, got := range inputs.Members {
		if got.Task.ID != leaver.ID && got.Task.ID != stayer.ID {
			t.Fatalf("unexpected member %s", got.Task.ID)
		}
		if events := recorded(got); !slices.Equal(events, want) {
			t.Fatalf("member %s (moved mid-read: %t) events %v, want its recorded history %v", got.Task.ID, moved && got.Task.ID == leaver.ID, events, want)
		}
		if !slices.Equal(got.Dependencies, wantDependencies[got.Task.ID]) {
			t.Fatalf("member %s (moved mid-read: %t) dependencies %+v, want %+v", got.Task.ID, moved && got.Task.ID == leaver.ID, got.Dependencies, wantDependencies[got.Task.ID])
		}
	}
	if !moved {
		// The memory backend never interleaves: the move lands after the read.
		_, err = x.Backend.ClearTaskCrate(x.Context, leaver.ID)
		requireOK(t, err)
	}
	after, err := x.Backend.ReadCrateInsightInputs(x.Context, crate.ID)
	requireOK(t, err)
	if len(after.Members) != 1 || after.Members[0].Task.ID != stayer.ID || !slices.Equal(recorded(after.Members[0]), want) {
		t.Fatalf("read after the move holds %d members, want only %s with its history", len(after.Members), stayer.ID)
	}
	// The leaver is no source after the move, and it stays the stayer's
	// dependency as a task outside the crate.
	if got := after.Members[0].Dependencies; !slices.Equal(got, wantDependencies[stayer.ID]) {
		t.Fatalf("stayer dependencies after the move %+v, want %+v", got, wantDependencies[stayer.ID])
	}
}
