package store

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestWorkOrderOwnerUserIDUsesAuthenticatedRunOrDurableWorkerOwner(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "test")
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
	ctx := WithWorkspace(context.Background(), "test")
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
