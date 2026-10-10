package singlestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// singlestoreOwnerFixture provisions active contributors, owned workers, and
// queued implement orders in the identity fixture's workspace
// (component-work-orders, Worker owner admission).
type singlestoreOwnerFixture struct {
	st  *Store
	ctx context.Context
}

const ownerFixtureWorkspace = "identity-fixture"

func newSingleStoreOwnerFixture(t *testing.T) singlestoreOwnerFixture {
	t.Helper()
	st, ctx, _ := ownedIdentityFixture(t)
	return singlestoreOwnerFixture{st: st, ctx: ctx}
}

func (f singlestoreOwnerFixture) member(t *testing.T, name string) core.IdentityUser {
	t.Helper()
	email := name + "-" + core.NewTaskID() + "@example.test"
	user, err := f.st.ProvisionIdentityUser(f.ctx, email, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GrantWorkspaceRole(f.ctx, email, ownerFixtureWorkspace, core.WorkspaceRoleContributor); err != nil {
		t.Fatal(err)
	}
	return user
}

func (f singlestoreOwnerFixture) worker(t *testing.T, name, owner string) core.Worker {
	t.Helper()
	worker := core.Worker{ID: "worker-" + name + "-" + core.NewTaskID(), Workspace: ownerFixtureWorkspace, OwnerUserID: owner, Name: name, CredentialHash: name + "-hash-" + core.NewTaskID(), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := f.st.CreateWorker(f.ctx, worker); err != nil {
		t.Fatal(err)
	}
	return worker
}

func (f singlestoreOwnerFixture) order(t *testing.T, prefix string) core.WorkOrder {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	taskID := prefix + "-" + core.NewTaskID()
	if err := f.st.CreateTask(f.ctx, core.Task{ID: taskID, Workspace: ownerFixtureWorkspace, Source: "test", Title: prefix, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, CreatedAt: now}); err != nil {
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

func singlestoreOwnerClaim(worker core.Worker, session string) core.WorkOrderClaim {
	return core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: worker.ID, WorkerID: worker.ID, Agent: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}
}

// TestWorkerOwnerClaimDeactivationSerializationIntegration orders a worker
// claim and an owner deactivation both ways with the claim's context hooks and
// channels. SingleStore exposes no deactivation operation, so a transaction
// sets the owner's status. Deactivation first: that transaction commits while
// the claim stands at its owner read, and the claim's locked read refuses it.
// Claim first: the claim holds the owner's user row FOR UPDATE when the
// deactivation starts; the deactivation's update waits, and inside its own
// transaction it then reads the order already claimed, so the claim committed
// while ownership was active. Later use is refused. No step sleeps.
func TestWorkerOwnerClaimDeactivationSerializationIntegration(t *testing.T) {
	f := newSingleStoreOwnerFixture(t)
	system := store.WithActor(t.Context(), store.SystemActor())

	t.Run("deactivation first", func(t *testing.T) {
		member := f.member(t, "deactivated-first")
		worker := f.worker(t, "deactivated-first", member.ID)
		order := f.order(t, "deactivated-first")
		if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); err != nil {
			t.Fatalf("preliminary authentication: %v", err)
		}
		deactivation, err := f.st.db.BeginTx(system, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer deactivation.Rollback() //nolint:errcheck
		if _, err := deactivation.ExecContext(system, `UPDATE users SET status='deactivated' WHERE id=?`, member.ID); err != nil {
			t.Fatal(err)
		}
		committed := false
		hookCtx := store.WithWorkerOwnerTestHook(f.ctx, func(stage string) error {
			if stage == store.WorkerOwnerHookClaimOwnerRead && !committed {
				committed = true
				return deactivation.Commit()
			}
			return nil
		})
		if _, err := storetest.For(f.st).ClaimWorkOrder(hookCtx, order.ID, singlestoreOwnerClaim(worker, "deactivated-first")); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("claim after committed deactivation error=%v", err)
		}
		if !committed {
			t.Fatal("claim never reached its owner read")
		}
		if persisted, err := f.st.GetWorkOrder(f.ctx, order.ID); err != nil || persisted.State != core.WorkOrderQueued || persisted.WorkerID != "" {
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
			_, err := storetest.For(f.st).ClaimWorkOrder(hookCtx, order.ID, singlestoreOwnerClaim(worker, "claim-first"))
			claimed <- err
		}()
		<-locked
		type deactivationResult struct {
			orderState string
			err        error
		}
		deactivated := make(chan deactivationResult, 1)
		go func() {
			tx, err := f.st.db.BeginTx(system, nil)
			if err != nil {
				deactivated <- deactivationResult{err: err}
				return
			}
			defer tx.Rollback() //nolint:errcheck
			if _, err := tx.ExecContext(system, `UPDATE users SET status='deactivated' WHERE id=?`, member.ID); err != nil {
				deactivated <- deactivationResult{err: err}
				return
			}
			var state string
			if err := tx.QueryRowContext(system, `SELECT state FROM work_orders WHERE workspace_id=? AND id=?`, ownerFixtureWorkspace, order.ID).Scan(&state); err != nil {
				deactivated <- deactivationResult{err: err}
				return
			}
			deactivated <- deactivationResult{orderState: state, err: tx.Commit()}
		}()
		close(release)
		if err := <-claimed; err != nil {
			t.Fatalf("claim holding active ownership: %v", err)
		}
		result := <-deactivated
		if result.err != nil {
			t.Fatalf("deactivation after claim: %v", result.err)
		}
		if result.orderState != string(core.WorkOrderClaimed) {
			t.Fatalf("deactivation observed order state %q, want the committed claim", result.orderState)
		}
		if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("worker authenticated after owner deactivation: %v", err)
		}
		next := f.order(t, "claim-first-next")
		if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, next.ID, singlestoreOwnerClaim(worker, "claim-first-next")); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("claim after owner deactivation error=%v", err)
		}
	})
}

// TestWorkerMissingOwnerFailsClosedIntegration seeds a historical worker row
// whose owner reference names no user, as SingleStore has no foreign keys, and
// proves authentication, heartbeat, and claim refuse it while it stays listed.
// Production writers are unchanged: the row is seeded directly.
func TestWorkerMissingOwnerFailsClosedIntegration(t *testing.T) {
	f := newSingleStoreOwnerFixture(t)
	system := store.WithActor(t.Context(), store.SystemActor())
	worker := core.Worker{ID: "worker-dangling-" + core.NewTaskID(), Workspace: ownerFixtureWorkspace, OwnerUserID: "usr_missing_" + core.NewTaskID(), Name: "dangling", CredentialHash: "dangling-hash-" + core.NewTaskID()}
	if _, err := f.st.db.ExecContext(system, `INSERT INTO workers (id,workspace_id,owner_user_id,name,credential_hash,probe_results,created_at) VALUES (?,?,?,?,?,?,?)`,
		worker.ID, worker.Workspace, worker.OwnerUserID, worker.Name, worker.CredentialHash, []byte("null"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	workers, err := f.st.ListWorkers(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, candidate := range workers {
		listed = listed || candidate.ID == worker.ID && candidate.OwnerUserID == worker.OwnerUserID && candidate.RevokedAt.IsZero()
	}
	if !listed {
		t.Fatalf("dangling-owner worker is not listed: %+v", workers)
	}
	if _, err := f.st.AuthenticateWorker(f.ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("dangling-owner authentication error=%v", err)
	}
	if _, err := f.st.HeartbeatWorker(f.ctx, worker.ID, time.Now().UTC().Add(time.Minute), nil); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("dangling-owner heartbeat error=%v", err)
	}
	order := f.order(t, "dangling-owner")
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, order.ID, singlestoreOwnerClaim(worker, "dangling-owner")); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("dangling-owner claim error=%v", err)
	}
	if persisted, err := f.st.GetWorkOrder(f.ctx, order.ID); err != nil || persisted.State != core.WorkOrderQueued {
		t.Fatalf("refused claim changed order: %+v %v", persisted, err)
	}
}
