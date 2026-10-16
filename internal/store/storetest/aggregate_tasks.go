package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func newAggregateTask(t *testing.T, x Fixture, stages ...core.Stage) core.Task {
	t.Helper()
	id := core.NewTaskID()
	stage := core.StageImplement
	if len(stages) > 0 {
		stage = stages[0]
	}
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
	}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	return task
}

func runTaskPagination(t *testing.T, x Fixture) {
	created := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	ids := []string{"oldest-" + core.NewTaskID(), "zeta-" + core.NewTaskID(), "alpha-" + core.NewTaskID(), "newest-" + core.NewTaskID()}
	for i, at := range []time.Time{created.Add(-time.Hour), created, created, created.Add(time.Hour)} {
		task := core.Task{ID: ids[i], Workspace: x.Workspace, Title: ids[i], Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + ids[i], State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: at}
		requireOK(t, x.Backend.CreateTask(x.Context, task))
	}
	RunTaskOperationsPaginationConformance(t, TaskOperationsFixture{Store: x.Backend, Context: x.Context, WantTotal: 4, WantOrder: []string{ids[3], ids[2], ids[1], ids[0]}, Filter: store.TaskFilter{Repositories: []string{"conveyor"}}})
}

func runTaskFilter(t *testing.T, x Fixture) {
	ctx, _ := bootstrapOwner(t, x)
	user, err := x.Backend.ProvisionIdentityUser(ctx, "assignee@example.test", "Assignee")
	requireOK(t, err)
	_, err = x.Backend.GrantWorkspaceRole(ctx, user.Email, x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	fixture := TaskFilterFixture{Store: x.Backend, Context: ctx, Workspace: x.Workspace, Repo: "conveyor", Suffix: core.NewTaskID(), AssigneeUserID: user.ID,
		Assign: func(t *testing.T, taskID, userID string) {
			t.Helper()
			_, err := taskops.New(x.Backend).SetAssignee(ctx, taskID, userID)
			requireOK(t, err)
		},
	}
	foreign := x.Workspace + "-filter-" + core.NewTaskID()[:6]
	fixture.ForeignWorkspace, fixture.ForeignContext = foreign, store.WithWorkspace(ctx, foreign)
	_, err = x.Backend.BootstrapWorkspaceConfig(fixture.ForeignContext, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	SeedTaskFilterFixture(t, fixture)
	RunTaskFilterConformance(t, fixture)
	RunTaskFilterUpdatedConformance(t, fixture)
}

func runTaskAssigneeMembership(t *testing.T, x Fixture) {
	ctx, owner := bootstrapOwner(t, x)
	x.Context = ctx
	task := newAggregateTask(t, x)
	viewer, err := x.Backend.ProvisionIdentityUser(ctx, "viewer@example.test", "Viewer")
	requireOK(t, err)
	_, err = x.Backend.GrantWorkspaceRole(ctx, viewer.Email, x.Workspace, core.WorkspaceRoleViewer)
	requireOK(t, err)
	revoked, err := x.Backend.ProvisionIdentityUser(ctx, "revoked@example.test", "Revoked")
	requireOK(t, err)
	_, err = x.Backend.GrantWorkspaceRole(ctx, revoked.Email, x.Workspace, core.WorkspaceRoleExecutor)
	requireOK(t, err)
	requireOK(t, x.Backend.RevokeWorkspaceRole(ctx, revoked.ID, x.Workspace))
	RunTaskAssigneeMembershipConformance(t, TaskAssigneeMembershipFixture{Store: x.Backend, Context: ctx, TaskID: task.ID, ActiveUserID: owner.ID, InactiveUserID: revoked.ID, NonMemberID: "absent", ViewerUserID: viewer.ID})
}

func runTaskLifecycle(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	// Task activity serializes these lists verbatim; a fresh task's must be
	// [] on every backend, never null (the web iterates them unguarded).
	interventions, err := st.ListInterventions(ctx, task.ID)
	requireOK(t, err)
	requireJSONArray(t, "fresh task interventions", interventions)
	jobs, err := st.ListJobs(ctx, task.ID)
	requireOK(t, err)
	requireJSONArray(t, "fresh task jobs", jobs)
	requireOK(t, st.UpdateTaskClassification(ctx, task.ID, "chore"))
	current, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if current.Class != "chore" {
		t.Fatal("classification was not persisted")
	}
	intervention := core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", Comment: "conformance cancellation"}
	cancelled, err := taskops.New(st).Cancel(ctx, intervention)
	requireOK(t, err)
	if cancelled.State != core.TaskClosed {
		t.Fatalf("cancelled state=%s", cancelled.State)
	}
	interventions, err = st.ListInterventions(ctx, task.ID)
	requireOK(t, err)
	if len(interventions) != 1 {
		t.Fatal("cancellation intervention missing")
	}
	// A rejected lifecycle command must leave both projections and events
	// unchanged. Reads use the same public transaction boundary as callers.
	before, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	if _, err := taskops.New(st).Cancel(ctx, intervention); err == nil {
		t.Fatal("terminal task accepted another cancellation")
	}
	after, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	current, err = st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if len(before) != len(after) || current.State != cancelled.State {
		t.Fatal("rejected cancellation partially committed")
	}
}

func requireJSONArray(t *testing.T, what string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	requireOK(t, err)
	if string(data) != "[]" {
		t.Fatalf("%s serialized as %s, want []", what, data)
	}
}

func runTaskEventAtomicity(t *testing.T, x Fixture) {
	runDurableTaskSideEffectLocks(t, x)
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	before, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	beforeTask, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	// A NUL in the audit actor cannot be represented by the reference
	// backend's text ledger. Cancellation moves the task row before that
	// insert, so this public input tests rollback after projection mutation;
	// the volatile backend refuses the same input before its first write.
	badActor := store.WithActor(ctx, store.Actor{ID: "invalid\x00actor", Role: core.ActorUser})
	intervention := core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", Comment: "atomicity probe"}
	if _, err := taskops.New(st).Cancel(badActor, intervention); err == nil {
		t.Fatal("cancellation committed despite an unrepresentable audit actor")
	}
	current, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	after, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	interventions, err := st.ListInterventions(ctx, task.ID)
	requireOK(t, err)
	if current.State != beforeTask.State || len(after) != len(before) || len(interventions) != 0 {
		t.Fatal("failed event insertion left a committed projection or partial event")
	}
}
