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
