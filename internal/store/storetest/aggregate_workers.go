package storetest

import (
	"context"
	"encoding/json"
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
		// Worker IDs are global while the lookup above is workspace-scoped, so a
		// likely cause is a fixed fixture ID already enrolled in another
		// workspace of a reused database; fixtures should use per-run IDs.
		return core.Worker{}, fmt.Errorf("enroll fixture worker %s in workspace %s (possible cause: worker IDs are global, so a fixed ID enrolled in another workspace of a reused database collides): %w", workerID, workspace, err)
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

// deactivateUser marks an account inactive through the backend's deactivation
// operation, or through the fixture's hook where the backend has none.
func deactivateUser(t *testing.T, x Fixture, ctx context.Context, userID string) {
	t.Helper()
	if x.DeactivateUser != nil {
		x.DeactivateUser(t, ctx, userID)
		return
	}
	deactivator, ok := x.Backend.(identityDeactivator)
	if !ok {
		t.Fatal("backend cannot deactivate identities and the fixture supplies no hook")
	}
	_, err := deactivator.DeactivateIdentityUser(ctx, userID)
	requireOK(t, err)
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
		// The worker routes bind the enrolled worker's actor before claiming.
		workerCtx := store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(worker.ID), Role: core.ActorWorker})
		claimed, err := For(st).ClaimWorkOrder(workerCtx, order.ID, workerClaim(worker, "active-session", ""))
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
		owned := enrollOwnedWorker(t, x, operatorCtx, "deactivated")
		deactivateUser(t, x, operatorCtx, owned.owner.ID)
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
		owned := enrollOwnedWorker(t, x, operatorCtx, "stale")
		stale, err := st.AuthenticateWorker(ctx, owned.worker.CredentialHash)
		requireOK(t, err)
		order := queuedWorkerOrder(t, x, ctx, "stale")
		deactivateUser(t, x, operatorCtx, owned.owner.ID)
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
	// Hold retirement (DEC-74(3)): a stored historical hold never narrows
	// worker eligibility, and every request-changes bounce re-queues an
	// ordinary eligible implement order (component-work-orders,
	// component-task-lifecycle).
	t.Run("StoredTaskHoldIsWorkerClaimable", func(t *testing.T) { runStoredTaskHoldIsWorkerClaimable(t, x, operatorCtx) })
	t.Run("UserRunRequestChangesRemainsWorkerClaimable", func(t *testing.T) {
		for _, seedHold := range []bool{false, true} {
			t.Run(map[bool]string{false: "unheld", true: "historical_hold"}[seedHold], func(t *testing.T) {
				runRequestChangesRemainsWorkerClaimable(t, x, operatorCtx, true, seedHold)
			})
		}
	})
	t.Run("WorkerRunRequestChangesRemainsWorkerClaimable", func(t *testing.T) {
		runRequestChangesRemainsWorkerClaimable(t, x, operatorCtx, false, false)
	})
	t.Run("HistoricalHoldEventsRemainReadable", func(t *testing.T) { runHistoricalHoldEventsRemainReadable(t, x) })
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

// requireWorkerService returns the fixture's real worker-service exercise or
// fails the case; the hold-retirement cases never skip (DEC-74(3)).
func requireWorkerService(t *testing.T, x Fixture) WorkerExercise {
	t.Helper()
	if x.WorkerService == nil {
		t.Fatal("conformance fixture must provide WorkerService for the hold-retirement cases (DEC-74(3))")
	}
	service := x.WorkerService(t)
	if service.ListClaimable == nil || service.ListVisibleOrders == nil || service.ClaimForWorker == nil {
		t.Fatal("WorkerService must provide ListClaimable, ListVisibleOrders, and ClaimForWorker")
	}
	return service
}

// liveOwnedWorker enrolls an active owner's worker and heartbeats it, so the
// worker service treats it as live for the next minute.
func liveOwnedWorker(t *testing.T, x Fixture, operatorCtx context.Context, name string) (workerOwnerFixture, context.Context) {
	t.Helper()
	owned := enrollOwnedWorker(t, x, operatorCtx, name)
	now := time.Now().UTC()
	_, err := x.Backend.HeartbeatWorker(x.Context, owned.worker.ID, now.Add(time.Minute), []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	requireOK(t, err)
	// The worker routes pass the freshly authenticated enrollment, whose
	// liveness lease the heartbeat just renewed.
	authenticated, err := x.Backend.AuthenticateWorker(x.Context, owned.worker.CredentialHash)
	requireOK(t, err)
	if authenticated.CredentialHash == "" {
		authenticated.CredentialHash = owned.worker.CredentialHash
	}
	owned.worker = authenticated
	// The worker routes bind the enrolled worker's actor before listing or
	// claiming.
	return owned, store.WithActor(x.Context, store.Actor{ID: store.WorkerActorID(owned.worker.ID), Role: core.ActorWorker})
}

func ordersContain(orders []core.WorkOrder, id string) bool {
	for _, order := range orders {
		if order.ID == id {
			return true
		}
	}
	return false
}

// requireWorkerListing checks whether the real worker service lists one order
// as claimable and as visible.
func requireWorkerListing(t *testing.T, service WorkerExercise, ctx context.Context, worker core.Worker, orderID string, claimable, visible bool) {
	t.Helper()
	listed, err := service.ListClaimable(ctx, worker)
	requireOK(t, err)
	if got := ordersContain(listed, orderID); got != claimable {
		t.Fatalf("ListClaimable includes %s=%v, want %v", orderID, got, claimable)
	}
	shown, err := service.ListVisibleOrders(ctx, worker)
	requireOK(t, err)
	if got := ordersContain(shown, orderID); got != visible {
		t.Fatalf("ListVisibleOrders includes %s=%v, want %v", orderID, got, visible)
	}
}

// requireNoHoldEvents proves no hold event was appended for the task: the
// retired reservation has no writer (DEC-74(3)).
func requireNoHoldEvents(t *testing.T, x Fixture, taskID string) {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "task.hold") {
			t.Fatalf("hold event appended after the retirement: %+v", event)
		}
	}
}

// requireTaskJSONOmitsHold proves the task projection carries no hold key.
func requireTaskJSONOmitsHold(t *testing.T, x Fixture, taskID string) {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	encoded, err := json.Marshal(task)
	requireOK(t, err)
	var fields map[string]json.RawMessage
	requireOK(t, json.Unmarshal(encoded, &fields))
	if _, present := fields["hold"]; present {
		t.Fatalf("task JSON still carries hold: %s", encoded)
	}
}

func runStoredTaskHoldIsWorkerClaimable(t *testing.T, x Fixture, operatorCtx context.Context) {
	service := requireWorkerService(t, x)
	seed := requireStoredTaskHoldSeed(t, x.SeedStoredTaskHold)
	st, ctx := x.Backend, x.Context
	owned, workerCtx := liveOwnedWorker(t, x, operatorCtx, "hold-retired")

	held := queuedWorkerOrder(t, x, ctx, "hold-retired")
	seed(t, ctx, held.TaskID)
	assertStoredHold(t, ctx, st, held.TaskID, true)
	requireWorkerListing(t, service, workerCtx, owned.worker, held.ID, true, true)
	claimed, err := service.ClaimForWorker(workerCtx, owned.worker, held.ID, workerClaim(owned.worker, "hold-retired-session", ""))
	requireOK(t, err)
	if claimed.WorkerID != owned.worker.ID || claimed.ClaimantID != owned.worker.ID || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claim=%+v", claimed)
	}
	events, err := st.ListEvents(ctx, held.TaskID)
	requireOK(t, err)
	attributed := false
	for _, event := range events {
		attributed = attributed || event.Kind == "work_order.claimed" && event.ActorID == store.WorkerActorID(owned.worker.ID) && event.ActorRole == core.ActorWorker
	}
	if !attributed {
		t.Fatalf("claim event lacks worker attribution: %+v", events)
	}
	assertStoredHold(t, ctx, st, held.TaskID, true)
	requireTaskJSONOmitsHold(t, x, held.TaskID)
	requireNoHoldEvents(t, x, held.TaskID)

	// Assignment to the worker's owner still admits the claim.
	mine := queuedWorkerOrder(t, x, ctx, "hold-retired-mine")
	seed(t, ctx, mine.TaskID)
	_, err = taskops.New(st).SetAssignee(ctx, mine.TaskID, owned.owner.ID)
	requireOK(t, err)
	requireWorkerListing(t, service, workerCtx, owned.worker, mine.ID, true, true)
	if _, err := service.ClaimForWorker(workerCtx, owned.worker, mine.ID, workerClaim(owned.worker, "hold-retired-mine-session", "")); err != nil {
		t.Fatalf("assigned-to-owner claim: %v", err)
	}

	// Assignment to another user still refuses, so the retirement broadens
	// no other eligibility (DEC-74(4)).
	otherEmail := "hold-retired-other-" + strings.ToLower(core.NewTaskID()) + "@workers.test"
	other, err := st.ProvisionIdentityUser(operatorCtx, otherEmail, "Other assignee")
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(operatorCtx, otherEmail, x.Workspace, core.WorkspaceRoleContributor)
	requireOK(t, err)
	theirs := queuedWorkerOrder(t, x, ctx, "hold-retired-theirs")
	seed(t, ctx, theirs.TaskID)
	_, err = taskops.New(st).SetAssignee(ctx, theirs.TaskID, other.ID)
	requireOK(t, err)
	requireWorkerListing(t, service, workerCtx, owned.worker, theirs.ID, false, false)
	if _, err := service.ClaimForWorker(workerCtx, owned.worker, theirs.ID, workerClaim(owned.worker, "hold-retired-theirs-session", "")); err == nil {
		t.Fatal("worker claimed a task assigned to another user")
	}
	if persisted, err := st.GetWorkOrder(ctx, theirs.ID); err != nil || persisted.State != core.WorkOrderQueued {
		t.Fatalf("refused claim changed order: %+v %v", persisted, err)
	}

	// Dependencies still block the claim; the blocked order stays visible.
	dependency := core.Task{ID: "workers-hold-dependency-" + core.NewTaskID(), Workspace: x.Workspace, Title: "hold dependency", State: core.TaskQueued, NextStage: core.StageImplement, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-workers-hold-dependency", CreatedAt: time.Now().UTC()}
	requireOK(t, st.CreateTask(ctx, dependency))
	blockedTask := core.Task{ID: "workers-hold-blocked-" + core.NewTaskID(), Workspace: x.Workspace, Title: "hold blocked", State: core.TaskQueued, NextStage: core.StageImplement, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-workers-hold-blocked", CreatedAt: time.Now().UTC()}
	requireOK(t, st.CreateTaskWithDependencies(ctx, blockedTask, []string{dependency.ID}))
	job := core.Job{ID: blockedTask.ID + "-implement", TaskID: blockedTask.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().UTC()}
	blocked := core.WorkOrder{ID: job.ID, TaskID: blockedTask.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	_, err = For(st).CreateStageWorkOrder(ctx, job, blocked)
	requireOK(t, err)
	seed(t, ctx, blockedTask.ID)
	requireWorkerListing(t, service, workerCtx, owned.worker, blocked.ID, false, true)
	if _, err := service.ClaimForWorker(workerCtx, owned.worker, blocked.ID, workerClaim(owned.worker, "hold-retired-blocked-session", "")); err == nil {
		t.Fatal("worker claimed a dependency-blocked order")
	}
}

// runRequestChangesRemainsWorkerClaimable reaches the merge gate after a
// user-run or worker-run implement claim, requests changes, and proves the
// fresh implement order is an ordinary eligible order: listed by the worker
// service, claimed by an eligible worker, and clearing the returned-for-
// changes attention, with no hold written (DEC-74(3)).
func runRequestChangesRemainsWorkerClaimable(t *testing.T, x Fixture, operatorCtx context.Context, userRun, seedHold bool) {
	service := requireWorkerService(t, x)
	name := map[bool]string{false: "worker-bounce", true: "user-bounce"}[userRun]
	if seedHold {
		name += "-held"
	}
	st := x.Backend
	owned, workerCtx := liveOwnedWorker(t, x, operatorCtx, name)
	requester := "requester-" + core.NewTaskID()
	ctx := store.WithActor(x.Context, store.Actor{ID: store.UserActorID(requester), Role: core.ActorUser})
	now := time.Now().UTC()
	task := core.Task{ID: "workers-" + name + "-" + core.NewTaskID(), Workspace: x.Workspace, Title: "bounce " + name, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-workers-" + name, State: core.TaskRunning, NextStage: core.StageReview, RecoveryStage: core.StageImplement, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, task))
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: now}
	requireOK(t, st.CreateJob(ctx, reviewJob))
	claimant := "worker-previous-" + core.NewTaskID()
	if userRun {
		claimant = core.TaskRunClaimantID(requester)
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{ID: task.ID + "-implement-1", Stage: core.StageImplement, ClaimantID: claimant})}))
	if seedHold {
		requireStoredTaskHoldSeed(t, x.SeedStoredTaskHold)(t, ctx, task.ID)
	}
	_, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	requireOK(t, err)

	feedback := "Line one.\n\n  exact indentation for " + name
	updated, err := taskops.New(st).RequestChanges(ctx, taskops.RequestChanges{TaskID: task.ID, JobID: reviewJob.ID, Feedback: feedback, MaxBounces: 3})
	requireOK(t, err)
	if updated.State != core.TaskQueued || updated.NextStage != core.StageImplement {
		t.Fatalf("bounced task=%+v", updated)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	requireOK(t, err)
	if len(interventions) != 1 || interventions[0].Comment != feedback {
		t.Fatalf("interventions=%+v, want the feedback verbatim", interventions)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	requireOK(t, err)
	if len(markers) != 1 || !markers[0].UserChangesRequested {
		t.Fatalf("markers after bounce=%+v", markers)
	}
	requireNoHoldEvents(t, x, task.ID)
	assertStoredHold(t, ctx, st, task.ID, seedHold)

	// The dispatcher mints the fresh implement order; storetest cannot import
	// dispatch, so the case mints it through the same store command.
	job := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().UTC()}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	created, err := For(st).CreateStageWorkOrder(ctx, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("fresh implement order was not created")
	}
	requireWorkerListing(t, service, workerCtx, owned.worker, order.ID, true, true)
	claimed, err := service.ClaimForWorker(workerCtx, owned.worker, order.ID, workerClaim(owned.worker, name+"-session", ""))
	requireOK(t, err)
	if claimed.WorkerID != owned.worker.ID || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claim=%+v", claimed)
	}
	markers, err = st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	requireOK(t, err)
	if len(markers) != 1 || markers[0].UserChangesRequested {
		t.Fatalf("markers after the worker claim=%+v", markers)
	}
	requireNoHoldEvents(t, x, task.ID)
	assertStoredHold(t, ctx, st, task.ID, seedHold)
	requireTaskJSONOmitsHold(t, x, task.ID)
}

// runHistoricalHoldEventsRemainReadable proves events recorded before the
// retirement stay in the ledger exactly as stored (DEC-10, DEC-74(3)).
func runHistoricalHoldEventsRemainReadable(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	historical := []core.Event{
		{TaskID: task.ID, Kind: "task.hold.set", Payload: core.JSONPayload(map[string]any{"hold": true})},
		{TaskID: task.ID, Kind: "task.hold.cleared", Payload: core.JSONPayload(map[string]any{"hold": false})},
		{TaskID: task.ID, Kind: "task.hold_changed", Payload: core.JSONPayload(map[string]any{"hold": true, "reason_code": store.UserRequestChangesReason})},
	}
	for _, event := range historical {
		requireOK(t, st.AppendEvent(ctx, event))
	}
	events, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	for _, want := range historical {
		found := false
		for _, event := range events {
			if event.Kind != want.Kind {
				continue
			}
			var got, expected map[string]any
			requireOK(t, json.Unmarshal(event.Payload, &got))
			requireOK(t, json.Unmarshal(want.Payload, &expected))
			found = found || fmt.Sprint(got) == fmt.Sprint(expected)
		}
		if !found {
			t.Fatalf("historical %s event is not readable as stored: %+v", want.Kind, events)
		}
	}
	requireTaskJSONOmitsHold(t, x, task.ID)
}
