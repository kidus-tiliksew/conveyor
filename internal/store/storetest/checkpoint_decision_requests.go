package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunCheckpointDecisionRequestConformance verifies that both stores retain
// the exact optional checkpoint structure on the order and release event.
func RunCheckpointDecisionRequestConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	taskID := core.NewTaskID()
	orderID := taskID + "-implement-1"
	now := time.Now().UTC()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspaceName(ctx), Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	const workerID = "worker-checkpoint-metadata"
	const sessionID = "session-checkpoint-metadata"
	if _, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: workerID, ClaimantID: workerID, SessionID: sessionID, ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	want := &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the authority revision.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: "req-authority", CitedVersion: 3, StatementOrSectionID: "AC-2.1"}}}
	released, err := ReleaseWorkerClaim(ctx, st, orderID, workerID, core.WorkOrderRelease{SessionID: sessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: want, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	if err != nil {
		t.Fatal(err)
	}
	if released.Checkpoint == nil || released.Checkpoint.DecisionRequest != want.DecisionRequest || released.Checkpoint.Class != want.Class || len(released.Checkpoint.Citations) != 1 || released.Checkpoint.Citations[0] != want.Citations[0] {
		t.Fatalf("released checkpoint=%+v want=%+v", released.Checkpoint, want)
	}
	persisted, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || persisted.Checkpoint == nil || persisted.Checkpoint.DecisionRequest != want.DecisionRequest {
		t.Fatalf("persisted=%+v err=%v", persisted.Checkpoint, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != "work_order.released" {
			continue
		}
		var payload struct {
			Checkpoint *core.WorkOrderCheckpoint `json:"checkpoint"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Checkpoint != nil && payload.Checkpoint.DecisionRequest == want.DecisionRequest {
			found = true
		}
	}
	if !found {
		t.Fatalf("work_order.released event omitted checkpoint")
	}
}
