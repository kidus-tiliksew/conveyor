package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestOwnedWorkerAuthorizationAndRevocationCascadeIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "worker-lifecycle-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Worker Org", Email: "worker-owner@example.test", DisplayName: "Worker Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	workspace := "worker-lifecycle-" + core.NewTaskID()
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err := st.CreateWorkspace(operatorCtx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	member, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{
		ID: "usr_worker_" + core.NewTaskID(), Email: "worker-member-" + core.NewTaskID() + "@example.test", DisplayName: "Worker Member",
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	ctx := store.WithCredential(store.WithWorkspace(operatorCtx, workspace), credential)
	if _, err := st.GrantWorkspaceRole(ctx, member.Email, workspace, core.WorkspaceRoleContributor); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	owned := core.Worker{ID: "worker-owned-" + core.NewTaskID(), Workspace: workspace, OwnerUserID: member.ID, Name: "owned", CredentialHash: "owned-hash-" + core.NewTaskID(), CreatedAt: now}
	legacyWorker := core.Worker{ID: "worker-legacy-" + core.NewTaskID(), Workspace: workspace, Name: "legacy", CredentialHash: "legacy-hash-" + core.NewTaskID(), CreatedAt: now}
	for _, worker := range []core.Worker{owned, legacyWorker} {
		if err := st.CreateWorker(ctx, worker); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AuthenticateWorker(ctx, owned.CredentialHash); err != nil {
		t.Fatalf("owned worker initial authentication: %v", err)
	}
	createClaimable := func(prefix string) core.WorkOrder {
		t.Helper()
		taskID := prefix + "-" + core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Source: "test", Title: prefix, Repo: "repo", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		jobID := taskID + "-implement-1"
		if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: jobID, TaskID: taskID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	ownedOrder := createClaimable("owned-race")
	if _, err := st.pool.Exec(t.Context(), `DELETE FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspace, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateWorker(ctx, owned.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("binding-less owned worker authentication error=%v", err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, ownedOrder.ID, core.WorkOrderClaim{
		SessionID: "owned-race", ClientToken: "owned-race", WorkerID: owned.ID, OwnerUserID: member.ID, Lease: time.Minute,
	}); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("durable claim after preliminary auth and binding revocation error=%v", err)
	}
	if _, err := st.AuthenticateWorker(ctx, legacyWorker.CredentialHash); err != nil {
		t.Fatalf("ownerless legacy worker authentication: %v", err)
	}
	legacyOrder := createClaimable("legacy-unassigned")
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, legacyOrder.ID, core.WorkOrderClaim{
		SessionID: "legacy-unassigned", ClientToken: "legacy-unassigned", WorkerID: legacyWorker.ID, OwnerUserID: "forged", Lease: time.Minute,
	}); err != nil {
		t.Fatalf("ownerless legacy worker unassigned claim: %v", err)
	}
	if _, err := st.GrantWorkspaceRole(ctx, member.Email, workspace, core.WorkspaceRoleContributor); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorkspaceRole(ctx, member.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateWorker(ctx, owned.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("membership-revoked worker authentication error=%v", err)
	}
	var revoked, audited int
	if err := st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM workers WHERE id=$1 AND revoked_at IS NOT NULL),
		(SELECT count(*) FROM events WHERE workspace_id=$2 AND kind='worker.revoked'
		 AND payload_json->>'worker_id'=$1 AND payload_json->>'reason'='workspace_membership_revoked')`, owned.ID, workspace).Scan(&revoked, &audited); err != nil || revoked != 1 || audited != 1 {
		t.Fatalf("membership cascade revoked=%d audited=%d err=%v", revoked, audited, err)
	}

	if _, err := st.GrantWorkspaceRole(ctx, member.Email, workspace, core.WorkspaceRoleContributor); err != nil {
		t.Fatal(err)
	}
	owned2 := core.Worker{ID: "worker-owned-" + core.NewTaskID(), Workspace: workspace, OwnerUserID: member.ID, Name: "owned-2", CredentialHash: "owned-hash-" + core.NewTaskID(), CreatedAt: now}
	if err := st.CreateWorker(ctx, owned2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeactivateIdentityUser(operatorCtx, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateWorker(ctx, owned2.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("deactivated-owner worker authentication error=%v", err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM workers WHERE id=$1 AND revoked_at IS NOT NULL),
		(SELECT count(*) FROM events WHERE workspace_id=$2 AND kind='worker.revoked'
		 AND payload_json->>'worker_id'=$1 AND payload_json->>'reason'='identity_user_deactivated')`, owned2.ID, workspace).Scan(&revoked, &audited); err != nil || revoked != 1 || audited != 1 {
		t.Fatalf("deactivation cascade revoked=%d audited=%d err=%v", revoked, audited, err)
	}
	if _, err := st.AuthenticateWorker(ctx, legacyWorker.CredentialHash); err != nil {
		t.Fatalf("ownerless legacy worker changed by user deactivation: %v", err)
	}
}
