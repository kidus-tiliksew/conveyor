package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// ownerFixture is a memory store with the worker service, one queued
// implement order, and an operator context (component-work-orders, Worker
// owner admission).
type ownerFixture struct {
	st      store.Store
	service *Service
	ctx     context.Context
	now     time.Time
}

func newOwnerFixture(t *testing.T) ownerFixture {
	t.Helper()
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	return ownerFixture{st: st, service: service, ctx: ctx, now: now}
}

func (f ownerFixture) order(t *testing.T, id string) core.WorkOrder {
	t.Helper()
	task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: f.now}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
	if err := f.st.CreateJob(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: f.now, QueueDeadline: f.now.Add(time.Hour), CreatedAt: f.now}
	if err := storetest.For(f.st).CreateWorkOrder(f.ctx, order); err != nil {
		t.Fatal(err)
	}
	return order
}

func (f ownerFixture) worker(t *testing.T, id, owner string, member bool) core.Worker {
	t.Helper()
	worker := core.Worker{ID: id, Workspace: "demo", OwnerUserID: owner, Name: id, CredentialHash: "credential-" + id, LeaseExpiresAt: f.now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}, CreatedAt: f.now}
	if owner != "" {
		if err := store.SetMemoryWorkspaceMember(f.st, "demo", owner, member); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.CreateWorker(f.ctx, worker); err != nil {
		t.Fatal(err)
	}
	return worker
}

func userPairingContext(ctx context.Context, owner string) context.Context {
	credential := core.AuthenticatedCredential{ID: owner + "-pat", OwnerUserID: owner, Kind: core.CredentialUser}
	return store.WithActor(store.WithCredential(ctx, credential), store.Actor{ID: store.UserActorID(owner), Role: core.ActorUser})
}

// TestIssuePairingRequiresUserCredential refuses pairing without a complete
// user credential before any token, pairing row, or event exists.
func TestIssuePairingRequiresUserCredential(t *testing.T) {
	f := newOwnerFixture(t)
	refused := map[string]context.Context{
		"absent credential": f.ctx,
		"actor only":        store.WithActor(f.ctx, store.Actor{ID: store.UserActorID("usr-actor"), Role: core.ActorUser}),
		"missing id":        store.WithCredential(f.ctx, core.AuthenticatedCredential{OwnerUserID: "usr-owner", Kind: core.CredentialUser}),
		"missing owner":     store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "pat", Kind: core.CredentialUser}),
		"agent kind":        store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "agent", OwnerUserID: "usr-owner", Kind: core.CredentialAgent}),
	}
	for name, ctx := range refused {
		before, err := f.st.ListEvents(f.ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		token, pairing, err := f.service.IssuePairing(ctx, time.Minute)
		if !errors.Is(err, store.ErrWorkerUnauthorized) || token != "" || pairing != (core.WorkerPairing{}) {
			t.Fatalf("%s: token=%q pairing=%+v err=%v", name, token, pairing, err)
		}
		after, err := f.st.ListEvents(f.ctx, "")
		if err != nil || len(after) != len(before) {
			t.Fatalf("%s: refused pairing appended events %d->%d err=%v", name, len(before), len(after), err)
		}
	}
	token, pairing, err := f.service.IssuePairing(userPairingContext(f.ctx, "usr-owner"), time.Minute)
	if err != nil || token == "" || pairing.OwnerUserID != "usr-owner" || pairing.TokenHash == token {
		t.Fatalf("user pairing=%+v token=%q err=%v", pairing, token, err)
	}
	events, err := f.st.ListEvents(f.ctx, "")
	if err != nil || len(events) != 1 || events[0].Kind != "worker.pairing_issued" || events[0].ActorID != store.UserActorID("usr-owner") {
		t.Fatalf("pairing events=%+v err=%v", events, err)
	}
}

// TestPairingPreservesOwnerAndLimits proves the credential owner reaches the
// enrolled worker, the default and maximum lifetimes, single use, and expiry.
func TestPairingPreservesOwnerAndLimits(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := userPairingContext(f.ctx, "usr-owner")
	for _, ttl := range []time.Duration{0, -time.Second, 2 * time.Hour} {
		if _, pairing, err := f.service.IssuePairing(ctx, ttl); err != nil || !pairing.ExpiresAt.Equal(f.now.Add(DefaultPairingTTL)) {
			t.Fatalf("ttl %s expires=%s err=%v", ttl, pairing.ExpiresAt, err)
		}
	}
	if _, pairing, err := f.service.IssuePairing(ctx, time.Hour); err != nil || !pairing.ExpiresAt.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("maximum ttl expires=%s err=%v", pairing.ExpiresAt, err)
	}
	token, _, err := f.service.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := f.service.Enroll(f.ctx, token, "laptop")
	if err != nil || enrollment.Worker.OwnerUserID != "usr-owner" || enrollment.Worker.CredentialHash == enrollment.Credential {
		t.Fatalf("enrollment=%+v err=%v", enrollment, err)
	}
	if _, err := f.service.Enroll(f.ctx, token, "again"); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("reused pairing err=%v", err)
	}
	expired, _, err := f.service.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Now = func() time.Time { return f.now.Add(2 * time.Minute) }
	if _, err := f.service.Enroll(f.ctx, expired, "late"); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("expired pairing err=%v", err)
	}
	if err := store.SetMemoryWorkspaceMember(f.st, "demo", "usr-owner", true); err != nil {
		t.Fatal(err)
	}
	if _, worker, err := f.service.Authenticate(f.ctx, enrollment.Credential, "demo"); err != nil || worker.OwnerUserID != "usr-owner" {
		t.Fatalf("authenticated worker=%+v err=%v", worker, err)
	}
}

// TestClaimForWorkerRejectsInvalidEnrollment refuses ownerless, missing,
// revoked, and inactive-owner enrollments and stale supplied Worker values;
// a forged claim owner rescues none of them.
func TestClaimForWorkerRejectsInvalidEnrollment(t *testing.T) {
	f := newOwnerFixture(t)
	active := f.worker(t, "worker-active", "usr-active", true)
	ownerless := f.worker(t, "worker-ownerless", "", false)
	inactive := f.worker(t, "worker-inactive", "usr-inactive", false)
	revoked := f.worker(t, "worker-revoked", "usr-active", true)
	if err := f.st.RevokeWorker(f.ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	missing := core.Worker{ID: "worker-missing", Workspace: "demo", OwnerUserID: "usr-active", CredentialHash: "credential-missing", LeaseExpiresAt: f.now.Add(time.Minute)}
	// A stale value names an active enrollment's ID with another credential.
	stale := active
	stale.CredentialHash = "credential-rotated-away"
	for name, candidate := range map[string]core.Worker{"ownerless": ownerless, "inactive owner": inactive, "revoked": revoked, "missing": missing, "stale value": stale} {
		order := f.order(t, "invalid-"+candidate.ID+"-"+core.NewTaskID())
		_, err := f.service.ClaimForWorker(f.ctx, candidate, order.ID, core.WorkOrderClaim{SessionID: name, ClientToken: name, OwnerUserID: "usr-active"})
		if !errors.Is(err, store.ErrWorkerUnauthorized) {
			t.Fatalf("%s claim error=%v", name, err)
		}
		if persisted, err := f.st.GetWorkOrder(f.ctx, order.ID); err != nil || persisted.State != core.WorkOrderQueued {
			t.Fatalf("%s refused claim changed order: %+v %v", name, persisted, err)
		}
	}
	// The locked claim refuses a worker whose owner loses membership after a
	// preliminary authentication, even through a direct store claim that
	// supplies an active owner.
	order := f.order(t, "deactivated-after-auth")
	if err := store.SetMemoryWorkspaceMember(f.st, "demo", "usr-active", false); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, order.ID, core.WorkOrderClaim{SessionID: "late", ClientToken: "late", ClaimantID: active.ID, WorkerID: active.ID, OwnerUserID: "usr-active", Lease: time.Minute}); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("locked claim after membership loss error=%v", err)
	}
	if err := store.SetMemoryWorkspaceMember(f.st, "demo", "usr-active", true); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.service.ClaimForWorker(f.ctx, active, order.ID, core.WorkOrderClaim{SessionID: "valid", ClientToken: "valid", OwnerUserID: "forged"})
	if err != nil || claimed.WorkerID != active.ID || claimed.ClaimantID != active.ID {
		t.Fatalf("active claim=%+v err=%v", claimed, err)
	}
	if owner, err := store.WorkOrderOwnerUserID(f.ctx, f.st, claimed); err != nil || owner != "usr-active" {
		t.Fatalf("claim owner=%q err=%v", owner, err)
	}
}

// TestListClaimableReauthenticatesEnrollment lists nothing for an invalid
// enrollment and uses the stored enrollment, not the caller's value.
func TestListClaimableReauthenticatesEnrollment(t *testing.T) {
	f := newOwnerFixture(t)
	f.order(t, "listed")
	active := f.worker(t, "worker-list", "usr-list", true)
	listed, err := f.service.ListClaimable(f.ctx, active)
	if err != nil || len(listed) != 1 {
		t.Fatalf("active listing=%+v err=%v", listed, err)
	}
	// A caller-supplied liveness or owner does not override the enrollment.
	forged := active
	forged.OwnerUserID = "usr-forged"
	forged.LeaseExpiresAt = f.now.Add(time.Hour)
	if listed, err = f.service.ListClaimable(f.ctx, forged); err != nil || len(listed) != 1 {
		t.Fatalf("forged-owner listing=%+v err=%v", listed, err)
	}
	if err := store.SetMemoryWorkspaceMember(f.st, "demo", "usr-list", false); err != nil {
		t.Fatal(err)
	}
	if listed, err = f.service.ListClaimable(f.ctx, active); !errors.Is(err, store.ErrWorkerUnauthorized) || len(listed) != 0 {
		t.Fatalf("inactive-owner listing=%+v err=%v", listed, err)
	}
	ownerless := f.worker(t, "worker-list-ownerless", "", false)
	if _, err = f.service.ListClaimable(f.ctx, ownerless); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("ownerless listing err=%v", err)
	}
	if _, err = f.service.ListClaimable(f.ctx, core.Worker{ID: "worker-unknown", Workspace: "demo", CredentialHash: "credential-unknown"}); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("missing enrollment listing err=%v", err)
	}
	if _, err = f.service.ListVisibleOrders(f.ctx, ownerless); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("ownerless visible orders err=%v", err)
	}
}
