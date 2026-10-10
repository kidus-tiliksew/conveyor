package storetest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type workerOwnerProvisioner interface {
	ProvisionIdentityUser(context.Context, string, string) (core.IdentityUser, error)
	GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error)
}

type identityDeactivator interface {
	DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
}

var fixtureEmailUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// EnsureWorkerEnrollment gives a fixture worker ID a real enrollment whose
// owner is an active workspace member, because worker claims refuse a missing
// worker row (component-work-orders, Worker owner admission). An existing
// enrollment of that ID in the bound workspace is returned unchanged, so a
// fixture that revoked or orphaned its own worker keeps that state. Backends
// with identity provisioning get a provisioned contributor; the plain memory
// store gets its membership projection seeded for the owner.
func EnsureWorkerEnrollment(ctx context.Context, st store.Store, workerID string) (core.Worker, error) {
	workers, err := st.ListWorkers(ctx)
	if err != nil {
		return core.Worker{}, err
	}
	for _, worker := range workers {
		if worker.ID == workerID {
			return worker, nil
		}
	}
	// The plain memory store tolerates an unbound workspace; durable stores
	// refuse it in ListWorkers above.
	workspace, _ := store.WorkspaceFromContext(ctx)
	owner, err := enrollFixtureOwner(ctx, st, workspace, workerID)
	if err != nil {
		return core.Worker{}, fmt.Errorf("enroll fixture worker %s owner: %w", workerID, err)
	}
	worker := core.Worker{
		ID: workerID, Workspace: workspace, OwnerUserID: owner, Name: "fixture " + workerID,
		CredentialHash: "fixture-credential-" + workspace + "-" + workerID, CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	workerCtx := store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(workerID), Role: core.ActorWorker})
	if err := st.CreateWorker(workerCtx, worker); err != nil {
		return core.Worker{}, fmt.Errorf("enroll fixture worker %s: %w", workerID, err)
	}
	return worker, nil
}

func enrollFixtureOwner(ctx context.Context, st store.Store, workspace, workerID string) (string, error) {
	provisioner, ok := st.(workerOwnerProvisioner)
	if !ok {
		owner := "owner-" + workerID
		return owner, store.SetMemoryWorkspaceMember(st, workspace, owner, true)
	}
	local := strings.Trim(fixtureEmailUnsafe.ReplaceAllString(strings.ToLower(workspace+"-"+workerID), "-"), "-")
	email := local + "@worker-owner.test"
	user, err := provisioner.ProvisionIdentityUser(ctx, email, "Worker owner "+workerID)
	if err != nil {
		return "", err
	}
	operatorCtx := store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture-operator", OwnerUserID: user.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	if _, err := provisioner.GrantWorkspaceRole(operatorCtx, email, workspace, core.WorkspaceRoleContributor); err != nil {
		return "", err
	}
	return user.ID, nil
}

// workerOwnerFixture is one enrolled worker with its owner in a Workers case.
type workerOwnerFixture struct {
	worker core.Worker
	owner  core.IdentityUser
	email  string
}

func enrollOwnedWorker(t *testing.T, x Fixture, ctx context.Context, name string) workerOwnerFixture {
	t.Helper()
	email := name + "-" + strings.ToLower(core.NewTaskID()) + "@workers.test"
	owner, err := x.Backend.ProvisionIdentityUser(ctx, email, "Worker owner "+name)
	requireOK(t, err)
	_, err = x.Backend.GrantWorkspaceRole(ctx, email, x.Workspace, core.WorkspaceRoleContributor)
	requireOK(t, err)
	worker := core.Worker{ID: "worker-" + name + "-" + core.NewTaskID(), Workspace: x.Workspace, OwnerUserID: owner.ID, Name: name, CredentialHash: "credential-" + name + "-" + core.NewTaskID(), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	requireOK(t, x.Backend.CreateWorker(store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(worker.ID), Role: core.ActorWorker}), worker))
	return workerOwnerFixture{worker: worker, owner: owner, email: email}
}

func queuedWorkerOrder(t *testing.T, x Fixture, ctx context.Context, name string) core.WorkOrder {
	t.Helper()
	task := core.Task{ID: "workers-" + name + "-" + core.NewTaskID(), Workspace: x.Workspace, Title: "worker owner " + name, State: core.TaskQueued, NextStage: core.StageImplement, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-workers-" + name, CreatedAt: time.Now().UTC()}
	requireOK(t, x.Backend.CreateTask(ctx, task))
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().UTC()}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	created, err := For(x.Backend).CreateStageWorkOrder(ctx, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("stage work order was not created")
	}
	return order
}

func workerClaim(worker core.Worker, session, owner string) core.WorkOrderClaim {
	return core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: worker.ID, WorkerID: worker.ID, OwnerUserID: owner, Agent: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}
}

// requireWorkerRefused proves authenticate, heartbeat, and a taskops-backed
// claim all refuse the worker with ErrWorkerUnauthorized, that the heartbeat
// changed nothing, and that the order stays queued without a claim event.
func requireWorkerRefused(t *testing.T, x Fixture, ctx context.Context, worker core.Worker, order core.WorkOrder, forgedOwner string) {
	t.Helper()
	if _, err := x.Backend.AuthenticateWorker(ctx, worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("authenticate error=%v", err)
	}
	before := listedWorker(t, x, ctx, worker.ID)
	if _, err := x.Backend.HeartbeatWorker(ctx, worker.ID, time.Now().UTC().Add(time.Hour), []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: time.Now().UTC()}}); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("heartbeat error=%v", err)
	}
	after := listedWorker(t, x, ctx, worker.ID)
	if !after.LeaseExpiresAt.Equal(before.LeaseExpiresAt) || !after.LastSeenAt.Equal(before.LastSeenAt) || len(after.Probes) != len(before.Probes) {
		t.Fatalf("refused heartbeat changed worker: before=%+v after=%+v", before, after)
	}
	if _, err := For(x.Backend).ClaimWorkOrder(ctx, order.ID, workerClaim(worker, "refused-"+core.NewTaskID(), forgedOwner)); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("claim error=%v", err)
	}
	persisted, err := x.Backend.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if persisted.State != core.WorkOrderQueued || persisted.WorkerID != "" {
		t.Fatalf("refused claim changed order: %+v", persisted)
	}
	if count, err := x.Backend.CountEvents(ctx, order.TaskID, "work_order.claimed"); err != nil || count != 0 {
		t.Fatalf("refused claim appended %d claim events: %v", count, err)
	}
}

func listedWorker(t *testing.T, x Fixture, ctx context.Context, id string) core.Worker {
	t.Helper()
	workers, err := x.Backend.ListWorkers(ctx)
	requireOK(t, err)
	for _, worker := range workers {
		if worker.ID == id {
			return worker
		}
	}
	t.Fatalf("worker %s is not listed", id)
	return core.Worker{}
}

func runWorkers(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	pair := core.WorkerPairing{TokenHash: "conformance-pair", Workspace: x.Workspace, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	requireOK(t, st.CreateWorkerPairing(ctx, pair))
	consumed, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now)
	requireOK(t, err)
	if consumed.Workspace != x.Workspace || consumed.ConsumedAt.IsZero() {
		t.Fatal("pairing was not consumed")
	}
	if _, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("reused pairing error=%v", err)
	}
	pair.TokenHash = "expired-pair"
	pair.ExpiresAt = now.Add(-time.Second)
	requireOK(t, st.CreateWorkerPairing(ctx, pair))
	if _, err := st.ConsumeWorkerPairing(ctx, pair.TokenHash, now); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("expired pairing error=%v", err)
	}
	operatorCtx, _ := bootstrapOwner(t, x)

	t.Run("ActiveOwner", func(t *testing.T) {
		owned := enrollOwnedWorker(t, x, operatorCtx, "active")
		worker := owned.worker
		auth, err := st.AuthenticateWorker(ctx, worker.CredentialHash)
		requireOK(t, err)
		if auth.ID != worker.ID || auth.OwnerUserID != owned.owner.ID {
			t.Fatalf("worker credential identifies %+v", auth)
		}
		before, err := st.ListEvents(ctx, "")
		requireOK(t, err)
		beat, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(time.Minute), []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
		requireOK(t, err)
		if !beat.LeaseExpiresAt.Equal(now.Add(time.Minute)) || len(beat.Probes) != 1 || !beat.Probes[0].Healthy {
			t.Fatal("heartbeat did not persist lease and probe")
		}
		after, err := st.ListEvents(ctx, "")
		requireOK(t, err)
		if len(after) != len(before) {
			t.Fatal("heartbeat appended a lifecycle event")
		}
		order := queuedWorkerOrder(t, x, ctx, "active")
		claimed, err := For(st).ClaimWorkOrder(ctx, order.ID, workerClaim(worker, "active-session", ""))
		requireOK(t, err)
		if claimed.WorkerID != worker.ID || claimed.ClaimantID != worker.ID || claimed.State != core.WorkOrderClaimed {
			t.Fatalf("claim=%+v", claimed)
		}
		events, err := st.ListEvents(ctx, order.TaskID)
		requireOK(t, err)
		attributed := false
		for _, event := range events {
			attributed = attributed || event.Kind == "work_order.claimed" && event.ActorID == store.WorkerActorID(worker.ID) && event.ActorRole == core.ActorWorker
		}
		if !attributed {
			t.Fatalf("claim event lacks worker attribution: %+v", events)
		}
		resolved, err := store.WorkOrderOwnerUserID(ctx, st, claimed)
		if err != nil || resolved != owned.owner.ID {
			t.Fatalf("claim owner=%q err=%v", resolved, err)
		}
		failures, err := st.ListHarnessModelFailures(ctx)
		requireOK(t, err)
		if len(failures) != 0 {
			t.Fatal("healthy worker has model failure")
		}
	})
	t.Run("Ownerless", func(t *testing.T) {
		worker := core.Worker{ID: "worker-ownerless-" + core.NewTaskID(), Workspace: x.Workspace, Name: "ownerless", CredentialHash: "ownerless-" + core.NewTaskID(), CreatedAt: now}
		requireOK(t, st.CreateWorker(ctx, worker))
		requireWorkerRefused(t, x, ctx, worker, queuedWorkerOrder(t, x, ctx, "ownerless"), "")
		if listed := listedWorker(t, x, ctx, worker.ID); listed.OwnerUserID != "" || !listed.RevokedAt.IsZero() {
			t.Fatalf("ownerless historical row changed: %+v", listed)
		}
	})
	t.Run("DeactivatedOwner", func(t *testing.T) {
		deactivator, ok := st.(identityDeactivator)
		if !ok {
			t.Fatal("backend cannot deactivate identities")
		}
		owned := enrollOwnedWorker(t, x, operatorCtx, "deactivated")
		_, err := deactivator.DeactivateIdentityUser(operatorCtx, owned.owner.ID)
		requireOK(t, err)
		// A retained, unrevoked worker of an already-deactivated account proves
		// owner status is judged at use, independently of the revocation cascade.
		retained := core.Worker{ID: "worker-retained-" + core.NewTaskID(), Workspace: x.Workspace, OwnerUserID: owned.owner.ID, Name: "retained", CredentialHash: "retained-" + core.NewTaskID(), CreatedAt: now}
		requireOK(t, st.CreateWorker(ctx, retained))
		if listed := listedWorker(t, x, ctx, retained.ID); !listed.RevokedAt.IsZero() {
			t.Fatalf("retained worker was revoked: %+v", listed)
		}
		requireWorkerRefused(t, x, ctx, retained, queuedWorkerOrder(t, x, ctx, "deactivated"), owned.owner.ID)
		requireWorkerRefused(t, x, ctx, owned.worker, queuedWorkerOrder(t, x, ctx, "deactivated-cascade"), owned.owner.ID)
	})
	t.Run("RevokedWorker", func(t *testing.T) {
		owned := enrollOwnedWorker(t, x, operatorCtx, "revoked")
		requireOK(t, st.RevokeWorker(operatorCtx, owned.worker.ID))
		requireWorkerRefused(t, x, ctx, owned.worker, queuedWorkerOrder(t, x, ctx, "revoked"), owned.owner.ID)
	})
	t.Run("MissingMembership", func(t *testing.T) {
		email := "nonmember-" + strings.ToLower(core.NewTaskID()) + "@workers.test"
		user, err := st.ProvisionIdentityUser(operatorCtx, email, "Nonmember")
		requireOK(t, err)
		worker := core.Worker{ID: "worker-nonmember-" + core.NewTaskID(), Workspace: x.Workspace, OwnerUserID: user.ID, Name: "nonmember", CredentialHash: "nonmember-" + core.NewTaskID(), CreatedAt: now}
		requireOK(t, st.CreateWorker(ctx, worker))
		requireWorkerRefused(t, x, ctx, worker, queuedWorkerOrder(t, x, ctx, "nonmember"), user.ID)
	})
	t.Run("ForgedClaimOwner", func(t *testing.T) {
		active := enrollOwnedWorker(t, x, operatorCtx, "forger")
		worker := core.Worker{ID: "worker-forged-" + core.NewTaskID(), Workspace: x.Workspace, Name: "forged", CredentialHash: "forged-" + core.NewTaskID(), CreatedAt: now}
		requireOK(t, st.CreateWorker(ctx, worker))
		// A client-supplied active owner cannot rescue an ownerless enrollment,
		// and a missing worker row is never treated as a valid worker.
		requireWorkerRefused(t, x, ctx, worker, queuedWorkerOrder(t, x, ctx, "forged"), active.owner.ID)
		missing := core.Worker{ID: "worker-missing-" + core.NewTaskID(), Workspace: x.Workspace}
		order := queuedWorkerOrder(t, x, ctx, "missing")
		if _, err := taskops.New(st).ClaimWorkOrder(ctx, order.TaskID, order.ID, workerClaim(missing, "missing-session", active.owner.ID)); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("missing worker claim error=%v", err)
		}
	})
	t.Run("ClaimAfterOwnerDeactivation", func(t *testing.T) {
		deactivator, ok := st.(identityDeactivator)
		if !ok {
			t.Fatal("backend cannot deactivate identities")
		}
		owned := enrollOwnedWorker(t, x, operatorCtx, "stale")
		stale, err := st.AuthenticateWorker(ctx, owned.worker.CredentialHash)
		requireOK(t, err)
		order := queuedWorkerOrder(t, x, ctx, "stale")
		_, err = deactivator.DeactivateIdentityUser(operatorCtx, owned.owner.ID)
		requireOK(t, err)
		if _, err := For(st).ClaimWorkOrder(ctx, order.ID, workerClaim(stale, "stale-session", owned.owner.ID)); !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("stale caller claim error=%v", err)
		}
		if persisted, err := st.GetWorkOrder(ctx, order.ID); err != nil || persisted.State != core.WorkOrderQueued {
			t.Fatalf("stale claim changed order: %+v %v", persisted, err)
		}
	})
	if !st.IsDurable() {
		t.Run("ClaimDeactivationSerialization", func(t *testing.T) { runVolatileClaimDeactivationSerialization(t, x, operatorCtx) })
	}
	workers, err := st.ListWorkers(ctx)
	requireOK(t, err)
	if len(workers) == 0 {
		t.Fatal("worker listing is empty")
	}
}

// runVolatileClaimDeactivationSerialization orders a worker claim against an
// owner deactivation with the claim's context hook and channels. The volatile
// backend serializes both under its mutex: a claim that holds the owner
// check commits before the deactivation, and the deactivation then refuses
// later use. A deactivation that commits first refuses the claim.
func runVolatileClaimDeactivationSerialization(t *testing.T, x Fixture, operatorCtx context.Context) {
	st, ctx := x.Backend, x.Context
	deactivator := st.(identityDeactivator)
	owned := enrollOwnedWorker(t, x, operatorCtx, "serial-claim-first")
	order := queuedWorkerOrder(t, x, ctx, "serial-claim-first")
	locked := make(chan struct{})
	release := make(chan struct{})
	hookCtx := store.WithWorkerOwnerTestHook(ctx, func(stage string) error {
		if stage == store.WorkerOwnerHookClaimOwnerLocked {
			close(locked)
			<-release
		}
		return nil
	})
	claimed := make(chan error, 1)
	go func() {
		_, err := For(st).ClaimWorkOrder(hookCtx, order.ID, workerClaim(owned.worker, "serial-session", ""))
		claimed <- err
	}()
	<-locked
	deactivated := make(chan error, 1)
	go func() {
		_, err := deactivator.DeactivateIdentityUser(operatorCtx, owned.owner.ID)
		deactivated <- err
	}()
	close(release)
	requireOK(t, <-claimed)
	requireOK(t, <-deactivated)
	if persisted, err := st.GetWorkOrder(ctx, order.ID); err != nil || persisted.State != core.WorkOrderClaimed || persisted.WorkerID != owned.worker.ID {
		t.Fatalf("claim-first order=%+v err=%v", persisted, err)
	}
	if _, err := st.AuthenticateWorker(ctx, owned.worker.CredentialHash); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("worker authenticated after deactivation: %v", err)
	}

	second := enrollOwnedWorker(t, x, operatorCtx, "serial-deactivation-first")
	secondOrder := queuedWorkerOrder(t, x, ctx, "serial-deactivation-first")
	_, err := deactivator.DeactivateIdentityUser(operatorCtx, second.owner.ID)
	requireOK(t, err)
	if _, err := For(st).ClaimWorkOrder(ctx, secondOrder.ID, workerClaim(second.worker, "serial-late", second.owner.ID)); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("deactivation-first claim error=%v", err)
	}
}
