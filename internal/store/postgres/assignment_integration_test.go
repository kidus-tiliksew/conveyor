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
	pairing := core.WorkerPairing{TokenHash: "pair-" + core.NewTaskID(), Workspace: workspace, OwnerUserID: member.ID, ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err = st.CreateWorkerPairing(ctx, pairing); err != nil {
		t.Fatal(err)
	}
	consumed, err := st.ConsumeWorkerPairing(ctx, pairing.TokenHash, now)
	if err != nil || consumed.OwnerUserID != member.ID {
		t.Fatalf("consumed pairing owner=%q err=%v", consumed.OwnerUserID, err)
	}
	worker := core.Worker{ID: "worker-" + core.NewTaskID(), Workspace: workspace, OwnerUserID: consumed.OwnerUserID, Name: "owned-worker", CredentialHash: "worker-hash-" + core.NewTaskID(), CreatedAt: now}
	if err = st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	authenticated, err := st.AuthenticateWorker(ctx, worker.CredentialHash)
	if err != nil || authenticated.OwnerUserID != member.ID {
		t.Fatalf("authenticated worker owner=%q err=%v", authenticated.OwnerUserID, err)
	}
	task := core.Task{ID: "task-" + core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/assignment", State: core.TaskRunning, CreatedAt: now}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	inactive, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: "usr_inactive_" + core.NewTaskID(), Email: "inactive-" + core.NewTaskID() + "@example.test", DisplayName: "Inactive User",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE users SET status='deactivated' WHERE id=$1`, inactive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'user')`, workspace, inactive.ID); err != nil {
		t.Fatal(err)
	}
	viewer, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: "usr_viewer_" + core.NewTaskID(), Email: "viewer-" + core.NewTaskID() + "@example.test", DisplayName: "Viewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.GrantWorkspaceRole(ctx, viewer.Email, workspace, core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	storetest.RunTaskAssigneeMembershipConformance(t, storetest.TaskAssigneeMembershipFixture{
		Store: st, Context: ctx, TaskID: task.ID, ActiveUserID: member.ID, InactiveUserID: inactive.ID, NonMemberID: "usr-not-a-member", ViewerUserID: viewer.ID,
	})
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
