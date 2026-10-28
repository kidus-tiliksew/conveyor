package storetest

import (
	"encoding/json"
	"reflect"
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
	RunTaskFilterOpenConformance(t, fixture)
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

// runTaskListHydration pins every relation a task list read hydrates to the
// single-task read: dependencies by ID with BlockingTaskIDs, children by
// origin plan version, sub ID, and ID, the assignee with its profile, the
// GitHub lifecycle, and the pull-request close projection, plus nil assignee
// and nil GitHub where none exists. Work-order lists carry the same assignee
// as the task read (component-persistence; req-task-lifecycle-and-queue
// AC-2.6, AC-4.4).
func runTaskListHydration(t *testing.T, x Fixture) {
	st := x.Backend
	ownerCtx, owner := bootstrapOwner(t, x)
	ctx := x.Context
	plane := taskops.New(st)
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	newTask := func(name string, edit func(*core.Task), dependencies ...string) core.Task {
		t.Helper()
		created = created.Add(time.Second)
		id := name + "-" + core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: name, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created}
		if edit != nil {
			edit(&task)
		}
		requireOK(t, st.CreateTaskWithDependencies(ctx, task, dependencies))
		return task
	}
	merged := newTask("dep-merged", nil)
	open := newTask("dep-open", nil)
	dependent := newTask("dependent", nil, open.ID, merged.ID)
	_, err := plane.Perform(ctx, merged.ID, taskops.Command{Kind: core.TaskMergeRecover})
	requireOK(t, err)
	parent := newTask("parent", nil)
	children := map[string]core.Task{}
	for _, child := range []struct {
		name    string
		version int
		sub     string
	}{{"child-late", 2, "SUB-0"}, {"child-b", 1, "SUB-2"}, {"child-a", 1, "SUB-1"}} {
		children[child.name] = newTask(child.name, func(task *core.Task) {
			task.ParentTaskID, task.OriginSpecVersion, task.OriginSubID = parent.ID, child.version, child.sub
		})
	}
	assigned := newTask("assigned", nil)
	_, err = plane.SetAssignee(ownerCtx, assigned.ID, owner.ID)
	requireOK(t, err)
	requireOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: assigned.ID, Repository: "acme/conveyor", SpecVersion: 1}))
	restarted := newTask("restarted", func(task *core.Task) { task.State = core.TaskRunning })
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: restarted.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "url": "https://github.com/acme/conveyor/pull/7"})}))
	result, err := plane.StartOver(ctx, core.TaskStartOverRequest{TaskID: restarted.ID, RequestID: "hydration-restart", Reason: "replace"})
	requireOK(t, err)
	requireOK(t, st.QueuePullRequestClose(ctx, core.PullRequestClose{WorkspaceID: x.Workspace, TaskID: restarted.ID, Repository: "acme/conveyor", Branch: restarted.Branch, SuccessorID: result.Successor.ID, SuccessorBranch: result.Successor.Branch, Reason: "replace", RestartingOperatorID: "operator", ForgeAuthorClass: core.ForgeAuthorWorkspace, State: "queued", Number: 7, URL: "https://github.com/acme/conveyor/pull/7"}))
	plain := newTask("plain", nil)
	for _, task := range []core.Task{assigned, plain, children["child-a"]} {
		job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, StartedAt: time.Now().UTC()}
		requireOK(t, st.CreateJob(ctx, job))
		requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}))
	}

	single := map[string]core.Task{}
	for _, id := range []string{merged.ID, open.ID, dependent.ID, parent.ID, children["child-late"].ID, children["child-b"].ID, children["child-a"].ID, assigned.ID, restarted.ID, result.Successor.ID, plain.ID} {
		task, err := st.GetTask(ctx, id)
		requireOK(t, err)
		single[id] = task
	}
	// The single-task read is the oracle; these pin it so the comparison
	// below cannot pass on two equally wrong reads.
	if got := single[dependent.ID]; len(got.Dependencies) != 2 || got.Dependencies[0].ID != min(merged.ID, open.ID) || len(got.BlockingTaskIDs) != 1 || got.BlockingTaskIDs[0] != open.ID {
		t.Fatalf("dependent relations=%+v blocking=%v", got.Dependencies, got.BlockingTaskIDs)
	}
	if got := single[parent.ID].Children; len(got) != 3 || got[0].ID != children["child-a"].ID || got[1].ID != children["child-b"].ID || got[2].ID != children["child-late"].ID {
		t.Fatalf("parent children=%+v", got)
	}
	if got := single[assigned.ID]; got.Assignee == nil || got.Assignee.UserID != owner.ID || got.GitHub == nil || got.GitHub.Repository != "acme/conveyor" {
		t.Fatalf("assigned task assignee=%+v github=%+v", got.Assignee, got.GitHub)
	}
	if got := single[restarted.ID]; got.PullRequestClose == nil || got.PullRequestCloseState != "queued" || got.PullRequestClose.Number != 7 {
		t.Fatalf("restarted task close=%+v state=%q", got.PullRequestClose, got.PullRequestCloseState)
	}
	if got := single[plain.ID]; got.Assignee != nil || got.GitHub != nil || got.PullRequestClose != nil || got.Dependencies != nil || got.Children != nil || got.BlockingTaskIDs != nil {
		t.Fatalf("plain task carries relations: %+v", got)
	}

	requireListed := func(label string, listed []core.Task) {
		t.Helper()
		seen := map[string]bool{}
		for _, task := range listed {
			want, ok := single[task.ID]
			if !ok {
				continue
			}
			seen[task.ID] = true
			if !reflect.DeepEqual(task, want) {
				got, _ := json.Marshal(task)
				expected, _ := json.Marshal(want)
				t.Fatalf("%s %s differs from the single-task read:\n got=%s\nwant=%s", label, task.ID, got, expected)
			}
		}
		if len(seen) != len(single) {
			t.Fatalf("%s listed %d of %d seeded tasks", label, len(seen), len(single))
		}
	}
	all, err := st.ListTasks(ctx)
	requireOK(t, err)
	requireListed("ListTasks", all)
	filtered, err := st.ListTasksFiltered(ctx, store.TaskFilter{Repositories: []string{"conveyor"}})
	requireOK(t, err)
	requireListed("ListTasksFiltered", filtered)
	var paged []core.Task
	for offset := 0; ; offset += 4 {
		page, err := st.ListTaskPage(ctx, store.TaskOperationsQuery{TaskFilter: store.TaskFilter{Repositories: []string{"conveyor"}}, Limit: 4, Offset: offset})
		requireOK(t, err)
		paged = append(paged, page.Tasks...)
		if len(page.Tasks) < 4 {
			break
		}
	}
	requireListed("ListTaskPage", paged)

	orders, err := st.ListWorkOrders(ctx)
	requireOK(t, err)
	checked := 0
	for _, order := range orders {
		task, ok := single[order.TaskID]
		if !ok {
			continue
		}
		checked++
		if !reflect.DeepEqual(order.Assignee, task.Assignee) {
			t.Fatalf("work order %s assignee=%+v, task read=%+v", order.ID, order.Assignee, task.Assignee)
		}
		got, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if !reflect.DeepEqual(got.Assignee, order.Assignee) {
			t.Fatalf("work order %s single read assignee=%+v, list=%+v", order.ID, got.Assignee, order.Assignee)
		}
	}
	if checked != 3 {
		t.Fatalf("ListWorkOrders returned %d seeded orders, want 3", checked)
	}
}
