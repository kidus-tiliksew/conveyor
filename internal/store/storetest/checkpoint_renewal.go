package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunCheckpointRenewalConformance verifies identical post-release
// classification in the memory and PostgreSQL stores.
func RunCheckpointRenewalConformance(t *testing.T, st store.Store, ctx context.Context) {
	claimShapes := []struct {
		name       string
		workerID   string
		claimantID string
	}{
		{name: "worker claim", workerID: "worker-checkpoint", claimantID: "worker-checkpoint"},
		{name: "run claim without worker id", claimantID: core.TaskRunClaimantID("checkpoint-user")},
	}
	for _, shape := range claimShapes {
		for _, reason := range []string{
			core.WorkOrderReleaseReasonOperatorCheckpointReached,
			core.WorkOrderReleaseReasonPlanRevisionRequested,
		} {
			t.Run(shape.name+"/"+reason, func(t *testing.T) {
				taskID := core.NewTaskID()
				orderID := taskID + "-implement-1"
				task := core.Task{ID: taskID, Workspace: workspaceName(ctx), Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
				job := core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				if err := st.CreateJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
					t.Fatal(err)
				}
				const sessionID = "session-checkpoint"
				if _, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{
					WorkerID: shape.workerID, ClaimantID: shape.claimantID, SessionID: sessionID,
					ClientToken: "checkpoint-secret", Lease: time.Minute, ExecutionTimeout: time.Hour,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := ReleaseWorkerClaim(ctx, st, orderID, shape.workerID, core.WorkOrderRelease{
					SessionID: sessionID, Reason: reason, Cause: core.WorkOrderReleaseCauseOperatorAction,
					Outcome: core.WorkOrderOutcomeReleased,
				}); err != nil {
					t.Fatal(err)
				}
				renew := func(claim core.WorkOrderClaimIdentity) error {
					_, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRenew, func(lease taskops.TaskLease) (core.WorkOrder, error) {
						return st.RenewWorkerClaimCommand(ctx, lease, orderID, claim, time.Minute)
					})
					return err
				}
				claim := core.WorkOrderClaimIdentity{WorkerID: shape.workerID, ClaimantID: shape.claimantID, SessionID: sessionID}
				if err := renew(claim); !errors.Is(err, store.ErrWorkOrderReleasedAtCheckpoint) {
					t.Fatalf("same-session renew error=%v", err)
				}
				claim.SessionID = "different-session"
				if err := renew(claim); !errors.Is(err, store.ErrWorkOrderClaimLost) {
					t.Fatalf("different-session renew error=%v", err)
				}
				claim.SessionID = sessionID
				claim.ClaimantID = core.TaskRunClaimantID("foreign-user")
				if shape.workerID != "" {
					claim.ClaimantID = "foreign-worker"
				}
				if err := renew(claim); !errors.Is(err, store.ErrWorkOrderClaimLost) {
					t.Fatalf("foreign-claim renew error=%v", err)
				}
			})
		}
	}
}

func workspaceName(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}
