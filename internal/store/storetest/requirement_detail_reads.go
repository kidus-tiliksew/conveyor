package storetest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requirementDetailBodySentinel and requirementDetailPlanSentinel must never
// appear in a requirement detail batch read.
const (
	requirementDetailBodySentinel = "REQUIREMENT-DETAIL-BODY-SENTINEL"
	requirementDetailPlanSentinel = "REQUIREMENT-DETAIL-PLAN-SENTINEL"
)

// runRequirementDetailReads proves the requirement detail batch reads on
// every backend: each equals the same backend's single-task reads for its
// selection, deduplicates its input, skips missing and other-workspace IDs,
// returns no unrequested row, and returns no result with an error
// (component-persistence; DEC-38, DEC-39).
func runRequirementDetailReads(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newTask := func(ctx context.Context, workspace, title, parent string, state core.TaskState) core.Task {
		t.Helper()
		id := core.NewTaskID()
		created = created.Add(time.Second)
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: title,
			Body: requirementDetailBodySentinel + " " + title, State: state, NextStage: core.StageImplement, ParentTaskID: parent, CreatedAt: created}
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	plan := func(ctx context.Context, taskID string, approve bool) int {
		t.Helper()
		spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: requirementDetailPlanSentinel + " plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), CreatedAt: time.Now().UTC()})
		requireOK(t, err)
		if approve {
			requireOK(t, st.ApproveSpecVersion(ctx, taskID, spec.Version))
		}
		return spec.Version
	}

	anchor := newTask(ctx, ws, "Anchor", "", core.TaskRunning)
	child := newTask(ctx, ws, "Child", anchor.ID, core.TaskQueued)
	noPlan := newTask(ctx, ws, "No plan", "", core.TaskMerged)
	olderApproved := newTask(ctx, ws, "Older approved", "", core.TaskQueued)
	latestApproved := newTask(ctx, ws, "Latest approved", "", core.TaskRunning)
	unrequested := newTask(ctx, ws, "Unrequested", "", core.TaskQueued)
	newTask(ctx, ws, "Unrequested child", unrequested.ID, core.TaskQueued)
	plan(ctx, olderApproved.ID, true)
	plan(ctx, olderApproved.ID, false)
	plan(ctx, latestApproved.ID, false)
	plan(ctx, latestApproved.ID, true)
	plan(ctx, unrequested.ID, true)

	other := ws + "-requirement-detail"
	otherCtx := store.WithActor(store.WithWorkspace(context.Background(), other), store.SystemActor())
	_, err := st.CreateWorkspace(otherCtx, other, "Requirement detail", &config.Config{Workspace: other, Repos: requirementConformanceRepos})
	requireOK(t, err)
	foreign := newTask(otherCtx, other, "Foreign", "", core.TaskQueued)
	newTask(otherCtx, other, "Foreign child", foreign.ID, core.TaskQueued)
	plan(otherCtx, foreign.ID, true)

	missing := "missing-" + core.NewTaskID()
	requested := []string{anchor.ID, child.ID, noPlan.ID, olderApproved.ID, latestApproved.ID, missing, foreign.ID, anchor.ID, olderApproved.ID}

	t.Run("FollowUpsSelectExactWorkspaceIntakeKeys", func(t *testing.T) {
		key := "requirement-follow-up-" + core.NewTaskID()
		body := strings.Repeat(requirementDetailBodySentinel, 1024)
		followUp := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "conveyor", BaseBranch: "main", Title: "Selected follow-up", Body: body, State: core.TaskQueued, NextStage: core.StageImplement, IntakeKey: key, CreatedAt: time.Now().UTC()}
		requireOK(t, st.CreateTask(ctx, followUp))
		foreignFollowUp := followUp
		foreignFollowUp.ID = core.NewTaskID()
		foreignFollowUp.Workspace = other
		foreignFollowUp.Title = "Foreign follow-up"
		requireOK(t, st.CreateTask(otherCtx, foreignFollowUp))
		if x.DrainReadBytes != nil {
			x.DrainReadBytes()
		}
		got, err := st.ListRequirementFollowUps(ctx, []string{key, key, "missing-follow-up"})
		requireOK(t, err)
		want := map[string]store.RequirementDetailTask{
			key: {ID: followUp.ID, Title: followUp.Title, State: followUp.State},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("follow-up summaries = %+v, want %+v", got, want)
		}
		if x.DrainReadBytes != nil {
			if received := x.DrainReadBytes()["ListRequirementFollowUps"]; received >= uint64(len(body)) {
				t.Fatalf("follow-up read received %d bytes, including an unused task body", received)
			}
		}
		empty, err := st.ListRequirementFollowUps(ctx, nil)
		requireOK(t, err)
		if len(empty) != 0 {
			t.Fatalf("empty intake keys selected %d follow-ups", len(empty))
		}
	})

	// A duplicate intake key cannot come from a command: PostgreSQL's unique
	// index refuses it, and the memory and SingleStore commands check it. A
	// SingleStore row has no unique key, so an older writer could leave one.
	// Every backend that holds a duplicate keeps the smallest task ID in byte
	// order, so a requirement's follow-up is the same task everywhere
	// (component-lineage invariant 30).
	t.Run("FollowUpDuplicateIntakeKeyKeepsSmallestID", func(t *testing.T) {
		if x.SeedTaskIntakeKey == nil {
			t.Fatal("fixture has no SeedTaskIntakeKey hook")
		}
		key := "requirement-follow-up-duplicate-" + core.NewTaskID()
		suffix := core.NewTaskID()
		create := func(id, intakeKey string) core.Task {
			t.Helper()
			task := core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Duplicate " + id, State: core.TaskQueued, NextStage: core.StageImplement, IntakeKey: intakeKey, CreatedAt: time.Now().UTC()}
			requireOK(t, st.CreateTask(ctx, task))
			return task
		}
		// The largest ID holds the key first and the smallest is written in
		// between, so neither the first row, the last row, nor the largest
		// ID is the smallest.
		largest := create("zz-"+suffix, key)
		smallest := create("aa-"+suffix, "")
		middle := create("mm-"+suffix, "")
		storedSmallest := x.SeedTaskIntakeKey(t, ctx, smallest.ID, key)
		storedMiddle := x.SeedTaskIntakeKey(t, ctx, middle.ID, key)
		if storedSmallest != storedMiddle {
			t.Fatalf("duplicate intake key stored for %s=%t and %s=%t", smallest.ID, storedSmallest, middle.ID, storedMiddle)
		}
		// A backend that refuses the duplicate keeps one holder, the task
		// that created the key.
		want := largest
		if storedSmallest {
			want = smallest
		}
		got, err := st.ListRequirementFollowUps(ctx, []string{key, "missing-follow-up"})
		requireOK(t, err)
		if expected := map[string]store.RequirementDetailTask{key: {ID: want.ID, Title: want.Title, State: want.State}}; !reflect.DeepEqual(got, expected) {
			t.Fatalf("duplicate-key follow-up = %+v, want %s (duplicate stored: %t)", got, want.ID, storedSmallest)
		}
	})

	t.Run("TasksEqualSingleTaskReads", func(t *testing.T) {
		got, err := st.ListRequirementDetailTasks(ctx, requested)
		requireOK(t, err)
		want := map[string]store.RequirementDetailTask{}
		for _, id := range []string{anchor.ID, child.ID, noPlan.ID, olderApproved.ID, latestApproved.ID} {
			task, getErr := st.GetTask(ctx, id)
			requireOK(t, getErr)
			want[id] = store.RequirementDetailTask{ID: task.ID, Title: task.Title, State: task.State, HasChildren: core.BlueprintAnchor(task)}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("tasks = %+v\nwant %+v", got, want)
		}
		if !got[anchor.ID].HasChildren || got[child.ID].HasChildren || got[noPlan.ID].State != core.TaskMerged {
			t.Fatalf("fixture did not exercise child presence and state: %+v", got)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), requirementDetailBodySentinel) {
			t.Fatalf("task read carried a body: %+v", got)
		}
	})

	t.Run("PlansEqualLatestPlanReads", func(t *testing.T) {
		got, err := st.ListLatestPlanSummaries(ctx, requested)
		requireOK(t, err)
		want := map[string]store.PlanSummary{}
		for _, id := range []string{anchor.ID, child.ID, noPlan.ID, olderApproved.ID, latestApproved.ID} {
			spec, found, getErr := st.GetLatestSpecVersion(ctx, id)
			requireOK(t, getErr)
			if found {
				want[id] = store.PlanSummary{Version: spec.Version, Approved: spec.Approved}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("plans = %+v\nwant %+v", got, want)
		}
		if got[olderApproved.ID] != (store.PlanSummary{Version: 2, Approved: false}) || got[latestApproved.ID] != (store.PlanSummary{Version: 2, Approved: true}) {
			t.Fatalf("latest plan versus older approved plan = %+v", got)
		}
		if _, ok := got[noPlan.ID]; ok {
			t.Fatalf("a task without a plan returned a summary: %+v", got)
		}
	})

	t.Run("EmptySelectionsReturnEmptyMaps", func(t *testing.T) {
		for _, ids := range [][]string{nil, {}} {
			tasks, err := st.ListRequirementDetailTasks(ctx, ids)
			if err != nil || tasks == nil || len(tasks) != 0 {
				t.Fatalf("tasks for %#v = %+v, %v", ids, tasks, err)
			}
			plans, err := st.ListLatestPlanSummaries(ctx, ids)
			if err != nil || plans == nil || len(plans) != 0 {
				t.Fatalf("plans for %#v = %+v, %v", ids, plans, err)
			}
		}
	})

	t.Run("OtherWorkspaceReadsOnlyItsOwnRows", func(t *testing.T) {
		tasks, err := st.ListRequirementDetailTasks(otherCtx, requested)
		requireOK(t, err)
		if len(tasks) != 1 || tasks[foreign.ID] != (store.RequirementDetailTask{ID: foreign.ID, Title: "Foreign", State: core.TaskQueued, HasChildren: true}) {
			t.Fatalf("other workspace tasks = %+v", tasks)
		}
		plans, err := st.ListLatestPlanSummaries(otherCtx, requested)
		requireOK(t, err)
		if len(plans) != 1 || plans[foreign.ID] != (store.PlanSummary{Version: 1, Approved: true}) {
			t.Fatalf("other workspace plans = %+v", plans)
		}
	})

	// Kept from the retired DocumentVersionSummaries suite: the list routes'
	// summary reads equal the same backend's per-document version reads,
	// skip an ID of another workspace, and drop historical requirement
	// content (component-persistence; DEC-19; DEC-38).
	t.Run("VersionSummariesEqualVersionReads", func(t *testing.T) {
		requirementVersion := func(id string, n int) core.RequirementVersion {
			return core.RequirementVersion{RequirementID: id, Content: fmt.Sprintf("# %s\n\nversion %d", id, n), Origin: core.RequirementOriginOperator,
				Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: fmt.Sprintf("Version %d holds.", n)}}}
		}
		requirementID, designID, foreignID := "req-summary-"+core.NewTaskID(), "design-summary-"+core.NewTaskID(), "req-summary-foreign-"+core.NewTaskID()
		_, first, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: requirementID}, requirementVersion(requirementID, 1))
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirementID, first.Version)
		requireOK(t, err)
		second, err := st.ProposeRequirementVersion(ctx, requirementVersion(requirementID, 2))
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirementID, second.Version)
		requireOK(t, err)
		_, firstDesign, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: designID, Title: designID, Category: "Component design"}, core.SystemDesignVersion{DocumentID: designID, Content: designContent("summary v1"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, designID, firstDesign.Version)
		requireOK(t, err)
		_, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: designID, Content: designContent("summary v2"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		foreignVersion := requirementVersion(foreignID, 1)
		_, foreignFirst, err := st.CreateRequirement(otherCtx, core.Requirement{ID: foreignID, Title: foreignID}, foreignVersion)
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(otherCtx, foreignID, foreignFirst.Version)
		requireOK(t, err)

		requirement, err := st.GetRequirement(ctx, requirementID)
		requireOK(t, err)
		requirements, err := st.ListRequirementVersionSummaries(ctx, []store.DocumentVersionSelection{{ID: requirementID, CurrentVersion: requirement.CurrentVersion}, {ID: foreignID, CurrentVersion: foreignFirst.Version}})
		requireOK(t, err)
		fullRequirement, err := st.ListRequirementVersions(ctx, requirementID)
		requireOK(t, err)
		if len(fullRequirement) != 2 {
			t.Fatalf("full requirement read returned %d versions, want 2", len(fullRequirement))
		}
		wantRequirement := make([]core.RequirementVersion, 0, len(fullRequirement))
		for _, version := range fullRequirement {
			if version.Version != requirement.CurrentVersion && (version.Confirmed || version.Retired) {
				version.Content = ""
			}
			wantRequirement = append(wantRequirement, version)
		}
		if len(requirements) != 1 || !reflect.DeepEqual(requirements[requirementID], wantRequirement) {
			t.Fatalf("requirement summaries = %+v\nwant only %s: %+v", requirements, requirementID, wantRequirement)
		}
		if historical := requirements[requirementID][0]; historical.Version != first.Version || historical.Content != "" || len(historical.Statements) == 0 {
			t.Fatalf("historical requirement summary = %+v, want v%d with statements and no content", historical, first.Version)
		}

		design, err := st.GetSystemDesign(ctx, designID)
		requireOK(t, err)
		designs, err := st.ListSystemDesignVersionSummaries(ctx, []store.DocumentVersionSelection{{ID: designID, CurrentVersion: design.CurrentVersion}, {ID: foreignID, CurrentVersion: foreignFirst.Version}})
		requireOK(t, err)
		fullDesign, err := st.ListSystemDesignVersions(ctx, designID)
		requireOK(t, err)
		var wantDesign []core.SystemDesignVersion
		for _, version := range fullDesign {
			if version.Version == design.CurrentVersion || (!version.Confirmed && !version.Dismissed) {
				wantDesign = append(wantDesign, version)
			}
		}
		if len(designs) != 1 || len(wantDesign) != 2 || !reflect.DeepEqual(designs[designID], wantDesign) {
			t.Fatalf("design summaries = %+v\nwant only %s with its current and pending versions: %+v", designs, designID, wantDesign)
		}
	})

	t.Run("DoneContextReturnsNoResult", func(t *testing.T) {
		done, cancel := context.WithCancel(ctx)
		cancel()
		if tasks, err := st.ListRequirementDetailTasks(done, requested); err == nil || tasks != nil {
			t.Fatalf("tasks with a done context = %+v, %v; want no result and an error", tasks, err)
		}
		if plans, err := st.ListLatestPlanSummaries(done, requested); err == nil || plans != nil {
			t.Fatalf("plans with a done context = %+v, %v; want no result and an error", plans, err)
		}
	})
}

// SeedRequirementDetail creates one confirmed requirement served through
// attached task context by serving tasks. The first linked of them also hold
// a confirmed requirement link and two plan versions, the first approved.
// Every task names the repository "conveyor", which the context's workspace
// must register.
// Both durable backends' statement-count tests and the HTTP tests read the
// detail route over it. It returns the requirement ID.
func SeedRequirementDetail(t *testing.T, st store.Store, ctx context.Context, name string, serving, linked int) string {
	t.Helper()
	if linked > serving {
		t.Fatalf("linked %d exceeds serving %d", linked, serving)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	workspace, _ := store.WorkspaceFromContext(ctx)
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-detail-" + name, Title: "Requirement detail " + name}, core.RequirementVersion{
		Content: "# Requirement detail " + name, Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The detail reads its tasks in a fixed number of statements."}},
	})
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version)
	requireOK(t, err)
	for i := 0; i < serving; i++ {
		id := fmt.Sprintf("detail-%s-%03d-%s", name, i, core.NewTaskID())
		requireOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id,
			Title: fmt.Sprintf("Serving task %d of %s", i, name), Body: requirementDetailBodySentinel + " " + strings.Repeat("b", 2048),
			State: core.TaskQueued, NextStage: core.StageImplement}))
		if i < linked {
			_, err = st.ProposeRequirementServes(ctx, id, requirement.ID, core.RequirementServesOperator, true)
			requireOK(t, err)
			for _, approve := range []bool{true, false} {
				spec, specErr := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: requirementDetailPlanSentinel + " " + strings.Repeat("p", 2048),
					Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), CreatedAt: time.Now().UTC()})
				requireOK(t, specErr)
				if approve {
					requireOK(t, st.ApproveSpecVersion(ctx, id, spec.Version))
				}
			}
			continue
		}
		_, err = st.UpdateTaskContext(ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}})
		requireOK(t, err)
	}
	return requirement.ID
}
