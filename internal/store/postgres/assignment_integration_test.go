package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestTaskAssignmentClaimEligibilityIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "assignment-legacy-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Assignment Org", Email: "owner-assignment@example.test", DisplayName: "Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	workspace := "assignment-" + core.NewTaskID()
	if seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), workspace), isolationConfig(workspace)); err != nil || !seeded {
		t.Fatalf("workspace seeded=%t err=%v", seeded, err)
	}
	if err := st.queries.UpsertRepo(t.Context(), db.UpsertRepoParams{WorkspaceID: workspace, Name: "conveyor", DefaultBase: "main"}); err != nil {
		t.Fatal(err)
	}
	member, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: "usr_assignee_" + core.NewTaskID(), Email: "assignee-" + core.NewTaskID() + "@example.test", DisplayName: "Assigned User",
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	ctx := store.WithCredential(store.WithWorkspace(t.Context(), workspace), credential)
	ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = st.GrantWorkspaceRole(ctx, member.Email, workspace, core.WorkspaceRoleUser); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: "task-" + core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/assignment", State: core.TaskRunning, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: task.ID + "-implement", TaskID: task.ID, JobID: task.ID + "-implement", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	assigned, err := taskops.New(st).SetAssignee(ctx, task.ID, member.ID)
	if err != nil || assigned.Assignee == nil || assigned.Assignee.UserID != member.ID || assigned.Assignee.DisplayName != "Assigned User" {
		t.Fatalf("assigned=%+v err=%v", assigned.Assignee, err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "owner", ClientToken: "owner", OwnerUserID: owner.ID}); err == nil || !strings.Contains(err.Error(), member.ID) {
		t.Fatalf("non-assignee claim error=%v", err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "assignee", ClientToken: "assignee", OwnerUserID: member.ID}); err != nil {
		t.Fatalf("assignee claim: %v", err)
	}
	var eventActor, eventRole string
	if err = st.pool.QueryRow(ctx, `SELECT actor_id,actor_role FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='task.assignee.set'`, workspace, task.ID).Scan(&eventActor, &eventRole); err != nil {
		t.Fatal(err)
	}
	if eventActor != store.UserActorID(owner.ID) || eventRole != string(core.ActorUser) {
		t.Fatalf("assignment actor=%s/%s", eventActor, eventRole)
	}
}

func TestTaskAssignmentRejectsNonMemberIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	workspace := "assignment-member-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser})
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	if err := st.queries.UpsertRepo(t.Context(), db.UpsertRepoParams{WorkspaceID: workspace, Name: "conveyor", DefaultBase: "main"}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "task-" + core.NewTaskID(), Workspace: workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, task.ID, "usr-not-a-member"); err == nil || !strings.Contains(err.Error(), "not an active member") {
		t.Fatalf("non-member assignment error=%v", err)
	}
}
