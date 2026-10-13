package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestWorkOrderOwnerUserIDUsesAuthenticatedRunOrDurableWorkerOwner(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "test")
	st := NewMemory()
	runOwner, err := WorkOrderOwnerUserID(ctx, st, core.WorkOrder{ID: "run-order", ClaimantID: core.TaskRunClaimantID("usr-run")})
	if err != nil || runOwner != "usr-run" {
		t.Fatalf("run owner=%q err=%v", runOwner, err)
	}
	if err = st.CreateWorker(ctx, core.Worker{ID: "worker-1", Workspace: "test", OwnerUserID: "usr-worker", Name: "worker", CredentialHash: "hash", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	workerOwner, err := WorkOrderOwnerUserID(ctx, st, core.WorkOrder{ID: "worker-order", WorkerID: "worker-1", ClaimantID: "spoofed"})
	if err != nil || workerOwner != "usr-worker" {
		t.Fatalf("worker owner=%q err=%v", workerOwner, err)
	}
}

func TestApprovingOperatorUserIDUsesLatestUserApproval(t *testing.T) {
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "test")
	st := NewMemory()
	task := core.Task{ID: "task-approval", Workspace: "test", State: core.TaskApproved, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, intervention := range []core.Intervention{
		{TaskID: task.ID, Action: core.InterventionApprove, ActorID: UserActorID("usr-first"), ActorRole: core.ActorUser, At: time.Now().UTC().Add(-time.Minute)},
		{TaskID: task.ID, Action: core.InterventionApprove, ActorID: AgentActorID("reviewer"), ActorRole: core.ActorAgent, At: time.Now().UTC()},
		{TaskID: task.ID, Action: core.InterventionApprove, ActorID: UserActorID("usr-approver"), ActorRole: core.ActorUser, At: time.Now().UTC().Add(time.Minute)},
	} {
		if err := st.CreateIntervention(ctx, intervention); err != nil {
			t.Fatal(err)
		}
	}
	userID, ok, err := ApprovingOperatorUserID(ctx, st, task.ID)
	if err != nil || !ok || userID != "usr-approver" {
		t.Fatalf("user=%q ok=%t err=%v", userID, ok, err)
	}
}

// TestActorFromContextRequiresExplicitActor pins the removed silent fallback:
// an absent, empty, or partial actor yields an empty Actor and a missing-actor
// error, and explicit user, agent, worker, and system actors keep their typed
// attribution (component-persistence, Actor context).
func TestActorFromContextRequiresExplicitActor(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"absent":     t.Context(),
		"empty":      WithActor(t.Context(), Actor{}),
		"id only":    WithActor(t.Context(), Actor{ID: "user:usr-1"}),
		"role only":  WithActor(t.Context(), Actor{Role: core.ActorUser}),
		"blank id":   WithActor(t.Context(), Actor{ID: "  ", Role: core.ActorSystem}),
		"credential": WithCredential(t.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "usr-1", Kind: core.CredentialUser}),
	} {
		if got := ActorFromContext(ctx); got != (Actor{}) {
			t.Fatalf("%s: ActorFromContext = %+v, want empty actor", name, got)
		}
		if got, err := RequireActor(ctx); !errors.Is(err, ErrMissingActor) || !errors.Is(err, core.ErrMissingActor) || got != (Actor{}) {
			t.Fatalf("%s: RequireActor = %+v, %v", name, got, err)
		}
	}
	for _, actor := range []Actor{
		{ID: UserActorID("usr-1"), Role: core.ActorUser},
		{ID: AgentActorID("agent-1"), Role: core.ActorAgent},
		{ID: WorkerActorID("worker-1"), Role: core.ActorWorker},
		SystemActor(),
		SystemActor("dispatcher"),
	} {
		ctx := WithActor(WithWorkspace(t.Context(), "test"), actor)
		if got := ActorFromContext(ctx); got != actor {
			t.Fatalf("ActorFromContext = %+v, want %+v", got, actor)
		}
		if got, err := RequireActor(ctx); err != nil || got != actor {
			t.Fatalf("RequireActor = %+v, %v; want %+v", got, err, actor)
		}
	}
	if got := SystemActor(); got.ID != core.SystemActorID || got.Role != core.ActorSystem {
		t.Fatalf("store.SystemActor = %+v", got)
	}
}
