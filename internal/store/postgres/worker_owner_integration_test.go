package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// workerOwnerFixture is an isolated PostgreSQL schema with an operator, a
// workspace, and helpers that enroll owned workers and queue implement
// orders (component-work-orders, Worker owner admission).
type workerOwnerFixture struct {
	st          *Store
	operatorCtx context.Context
	ctx         context.Context
	workspace   string
}

func newWorkerOwnerFixture(t *testing.T) workerOwnerFixture {
	t.Helper()
	st := newIdentityIntegrationStore(t, 0)
	system := store.WithActor(t.Context(), store.SystemActor())
	if _, err := st.BootstrapIdentity(system, config.FirstOperatorIdentity{OrganizationName: "Owner Org", Email: "owner-admission@example.test", DisplayName: "Owner"}, "owner-admission-token"); err != nil {
		t.Fatal(err)
	}
	operator, err := st.VerifyPersonalAccessToken(system, "owner-admission-token")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "worker-owner-" + core.NewTaskID()
	operatorCtx := store.WithCredential(system, core.AuthenticatedCredential{ID: "owner-admission", OwnerUserID: operator.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithWorkspace(store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(operator.ID), Role: core.ActorUser}), workspace)
	if _, err := st.CreateWorkspace(operatorCtx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	return workerOwnerFixture{st: st, operatorCtx: operatorCtx, ctx: operatorCtx, workspace: workspace}
}

func (f workerOwnerFixture) member(t *testing.T, name string) db.User {
	t.Helper()
	user, err := f.st.queries.InsertIdentityUser(store.WithActor(t.Context(), store.SystemActor()), db.InsertIdentityUserParams{
		ID: "usr_" + name + "_" + core.NewTaskID(), Email: name + "-" + core.NewTaskID() + "@example.test", DisplayName: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GrantWorkspaceRole(f.operatorCtx, user.Email, f.workspace, core.WorkspaceRoleContributor); err != nil {
		t.Fatal(err)
	}
	return user
}

func (f workerOwnerFixture) worker(t *testing.T, name, owner string) core.Worker {
	t.Helper()
	worker := core.Worker{ID: "worker-" + name + "-" + core.NewTaskID(), Workspace: f.workspace, OwnerUserID: owner, Name: name, CredentialHash: name + "-hash-" + core.NewTaskID(), CreatedAt: time.Now().UTC()}
	if err := f.st.CreateWorker(f.ctx, worker); err != nil {
		t.Fatal(err)
	}
	return worker
}

func (f workerOwnerFixture) order(t *testing.T, prefix string) core.WorkOrder {
	t.Helper()
	now := time.Now().UTC()
	taskID := prefix + "-" + core.NewTaskID()
	if err := f.st.CreateTask(f.ctx, core.Task{ID: taskID, Workspace: f.workspace, Source: "test", Title: prefix, Repo: "repo", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	jobID := taskID + "-implement-1"
	if err := f.st.CreateJob(f.ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: jobID, TaskID: taskID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(f.st).CreateWorkOrder(f.ctx, order); err != nil {
		t.Fatal(err)
	}
	return order
}

func workerOwnerClaim(worker core.Worker, session string) core.WorkOrderClaim {
	return core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}
}

func (f workerOwnerFixture) eventID(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := f.st.pool.QueryRow(store.WithActor(t.Context(), store.SystemActor()), query, args...).Scan(&id); err != nil {
		t.Fatalf("event lookup: %v", err)
	}
	return id
}

// TestWorkerOwnerClaimDeactivationSerializationIntegration orders a worker
// claim and an owner deactivation both ways with the claim's context hooks and
// channels. Deactivation first: a transaction that has already marked the
// owner deactivated commits while the claim stands at its owner read, and the
// locked owner read refuses the claim. Claim first: the claim holds the owner's
// user and binding rows FOR SHARE when the deactivation starts, so the claim
// commits while ownership is active, the deactivation's revocation cascade is
// recorded after the claim event, and later use is refused. No step sleeps.
func TestWorkerOwnerClaimDeactivationSerializationIntegration(t *testing.T) {
	f := newWorkerOwnerFixture(t)

	t.Run("deactivation first", func(t *testing.T) {
		member := f.member(t, "deactivated-first")
		worker := f.worker(t, "deactivated-first", member.ID)
		order := f.order(t, "deactivated-first")
		if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); err != nil {
			t.Fatalf("preliminary authentication: %v", err)
		}
		deactivation, err := f.st.pool.Begin(store.WithActor(t.Context(), store.SystemActor()))
		if err != nil {
			t.Fatal(err)
		}
		defer deactivation.Rollback(context.Background()) //nolint:errcheck
		if _, err := deactivation.Exec(t.Context(), `UPDATE users SET status='deactivated' WHERE id=$1`, member.ID); err != nil {
			t.Fatal(err)
		}
		committed := false
		hookCtx := store.WithWorkerOwnerTestHook(f.ctx, func(stage string) error {
			if stage == store.WorkerOwnerHookClaimOwnerRead && !committed {
				committed = true
				return deactivation.Commit(context.Background())
			}
			return nil
		})
		if _, err := storetest.For(f.st).ClaimWorkOrder(hookCtx, order.ID, workerOwnerClaim(worker, "deactivated-first")); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("claim after committed deactivation error=%v", err)
		}
		if !committed {
			t.Fatal("claim never reached its owner read")
		}
		persisted, err := f.st.GetWorkOrder(f.ctx, order.ID)
		if err != nil || persisted.State != core.WorkOrderQueued || persisted.WorkerID != "" {
			t.Fatalf("refused claim changed order: %+v %v", persisted, err)
		}
	})

	t.Run("claim first", func(t *testing.T) {
		member := f.member(t, "claim-first")
		worker := f.worker(t, "claim-first", member.ID)
		order := f.order(t, "claim-first")
		locked := make(chan struct{})
		release := make(chan struct{})
		hookCtx := store.WithWorkerOwnerTestHook(f.ctx, func(stage string) error {
			if stage == store.WorkerOwnerHookClaimOwnerLocked {
				close(locked)
				<-release
			}
			return nil
		})
		claimed := make(chan error, 1)
		go func() {
			_, err := storetest.For(f.st).ClaimWorkOrder(hookCtx, order.ID, workerOwnerClaim(worker, "claim-first"))
			claimed <- err
		}()
		<-locked
		deactivated := make(chan error, 1)
		go func() {
			_, err := f.st.DeactivateIdentityUser(f.operatorCtx, member.ID)
			deactivated <- err
		}()
		close(release)
		if err := <-claimed; err != nil {
			t.Fatalf("claim holding active ownership: %v", err)
		}
		if err := <-deactivated; err != nil {
			t.Fatalf("deactivation after claim: %v", err)
		}
		persisted, err := f.st.GetWorkOrder(f.ctx, order.ID)
		if err != nil || persisted.State != core.WorkOrderClaimed || persisted.WorkerID != worker.ID {
			t.Fatalf("claim-first order=%+v err=%v", persisted, err)
		}
		claimEvent := f.eventID(t, `SELECT id FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='work_order.claimed'`, f.workspace, order.TaskID)
		revokedEvent := f.eventID(t, `SELECT id FROM events WHERE workspace_id=$1 AND kind='worker.revoked' AND payload_json->>'worker_id'=$2`, f.workspace, worker.ID)
		if revokedEvent <= claimEvent {
			t.Fatalf("deactivation cascade event %d did not follow claim event %d", revokedEvent, claimEvent)
		}
		if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("worker authenticated after owner deactivation: %v", err)
		}
		next := f.order(t, "claim-first-next")
		if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, next.ID, workerOwnerClaim(worker, "claim-first-next")); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("claim after owner deactivation error=%v", err)
		}
	})
}

// TestWorkerOwnerDeletionFailsClosedIntegration deletes a worker's owner so
// ON DELETE SET NULL leaves a listable, unrevoked, ownerless row, then proves
// authentication, heartbeat, and claim all refuse it.
func TestWorkerOwnerDeletionFailsClosedIntegration(t *testing.T) {
	f := newWorkerOwnerFixture(t)
	member := f.member(t, "deleted-owner")
	worker := f.worker(t, "deleted-owner", member.ID)
	if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); err != nil {
		t.Fatalf("owned worker authentication before deletion: %v", err)
	}
	if _, err := f.st.pool.Exec(store.WithActor(t.Context(), store.SystemActor()), `DELETE FROM users WHERE id=$1`, member.ID); err != nil {
		t.Fatal(err)
	}
	workers, err := f.st.ListWorkers(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, candidate := range workers {
		listed = listed || candidate.ID == worker.ID && candidate.OwnerUserID == "" && candidate.RevokedAt.IsZero()
	}
	if !listed {
		t.Fatalf("orphaned worker is not listed as ownerless: %+v", workers)
	}
	if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("orphaned worker authentication error=%v", err)
	}
	if _, err := f.st.HeartbeatWorker(f.ctx, worker.ID, time.Now().UTC().Add(time.Minute), nil); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("orphaned worker heartbeat error=%v", err)
	}
	order := f.order(t, "deleted-owner")
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, order.ID, workerOwnerClaim(worker, "deleted-owner")); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("orphaned worker claim error=%v", err)
	}
	if persisted, err := f.st.GetWorkOrder(f.ctx, order.ID); err != nil || persisted.State != core.WorkOrderQueued {
		t.Fatalf("refused claim changed order: %+v %v", persisted, err)
	}
}
