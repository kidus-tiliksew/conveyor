package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestTaskAssignmentDoesNotChangeFIFOOrder(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	for i, id := range []string{"old-unassigned", "middle-assigned", "new-unassigned"} {
		task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: id + "-implement", TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id + "-implement", TaskID: id, JobID: id + "-implement", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now.Add(time.Duration(i) * time.Second), QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := storetestFor(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetMemoryWorkspaceMember(st, "demo", "usr-alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, "middle-assigned", "usr-alice"); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListWorkOrders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{orders[0].ID, orders[1].ID, orders[2].ID}; !reflect.DeepEqual(got, []string{"old-unassigned-implement", "middle-assigned-implement", "new-unassigned-implement"}) {
		t.Fatalf("FIFO order = %v", got)
	}
}

func TestMemoryDependencyOutcomesUnlinkAndQueueClock(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	dependency := core.Task{ID: "dependency", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	dependent := core.Task{ID: "dependent", Workspace: "demo", Repo: "ui", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTaskWithDependencies(ctx, dependent, []string{dependency.ID}); err != nil {
		t.Fatalf("cross-repository dependency rejected: %v", err)
	}
	queuedAt := time.Now().UTC().Add(-2 * time.Hour)
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement} {
		job := core.Job{ID: dependent.ID + "-" + string(stage), TaskID: dependent.ID, Stage: stage, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		deadline := queuedAt.Add(time.Hour)
		if stage == core.StageSpec {
			deadline = time.Now().UTC().Add(time.Hour)
		}
		if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{
			ID: job.ID, TaskID: dependent.ID, JobID: job.ID, Stage: stage,
			QueueEnteredAt: queuedAt, QueueDeadline: deadline, CreatedAt: queuedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, dependent.ID+"-spec", core.WorkOrderClaim{
		SessionID: "spec-session", ClientToken: "spec-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	}); err != nil {
		t.Fatalf("blocked spec order was not claimable: %v", err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, dependent.ID+"-implement", core.WorkOrderClaim{
		SessionID: "implement-session", ClientToken: "implement-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	}); err == nil || !strings.Contains(err.Error(), dependency.ID) {
		t.Fatalf("blocked implement claim error=%v", err)
	}

	if _, err := taskops.New(st).Cancel(ctx, core.Intervention{
		TaskID: dependency.ID, Action: core.InterventionCancel, ReasonCode: "will-not-merge",
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.CountEvents(ctx, dependent.ID, "task.dependency_unsatisfiable"); err != nil || count != 1 {
		t.Fatalf("unsatisfiable events=%d err=%v", count, err)
	}
	blockers, err := st.ListDependencyBlockers(ctx, []string{dependent.ID})
	if err != nil {
		t.Fatal(err)
	}
	order, err := st.GetWorkOrder(ctx, dependent.ID+"-implement")
	if err != nil {
		t.Fatal(err)
	}
	order.BlockingTaskIDs = blockers[dependent.ID].BlockingTaskIDs
	order.UnsatisfiableTaskIDs = blockers[dependent.ID].UnsatisfiableTaskIDs
	stalled := StalledTask([]core.WorkOrder{order})
	if stalled == nil || !stalled.UnsatisfiableEdge || len(stalled.BlockingTaskIDs) != 1 || stalled.BlockingTaskIDs[0] != dependency.ID {
		t.Fatalf("stalled dependency projection=%+v", stalled)
	}

	request := DependencyRemovalRequest{
		TaskID: dependent.ID, DependsOnTaskID: dependency.ID,
		Reason: "operator chose independent delivery", RequestID: "unlink-1",
	}
	result, err := st.RemoveTaskDependency(ctx, request)
	if err != nil || !result.Removed || len(result.Task.Dependencies) != 0 {
		t.Fatalf("unlink result=%+v err=%v", result, err)
	}
	retry, err := st.RemoveTaskDependency(ctx, request)
	if err != nil || retry.Removed {
		t.Fatalf("idempotent unlink=%+v err=%v", retry, err)
	}
	changed := request
	changed.Reason = "different"
	if _, err = st.RemoveTaskDependency(ctx, changed); err == nil {
		t.Fatal("request_id reuse with different unlink inputs succeeded")
	}
	if count, err := st.CountEvents(ctx, dependent.ID, "task.dependency_removed"); err != nil || count != 1 {
		t.Fatalf("dependency removed events=%d err=%v", count, err)
	}
	order, err = st.GetWorkOrder(ctx, dependent.ID+"-implement")
	if err != nil {
		t.Fatal(err)
	}
	if !order.QueueBlockedAt.IsZero() || order.State != core.WorkOrderQueued || !order.Claimable ||
		!order.QueueEnteredAt.Equal(queuedAt) || !order.QueueDeadline.After(time.Now().UTC()) {
		t.Fatalf("resumed queue clock=%+v", order)
	}
}

func TestMemoryBlueprintClosesAfterRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	parent := core.Task{
		ID: "memory-blueprint-parent", State: core.TaskQueued,
		NextStage: core.StageImplement, CreatedAt: time.Now(),
	}
	child := core.Task{
		ID: "memory-blueprint-child", State: core.TaskQueued,
		NextStage: core.StageImplement, ParentTaskID: parent.ID, CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, parent.ID, taskops.Command{
		Kind: core.TaskDispatchFailFinal, RecoveryStage: core.StageImplement, ProjectStages: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}
	outcome, err := taskops.New(st).Perform(ctx, parent.ID, taskops.Command{
		Kind: core.TaskRecover, NextStage: core.StageImplement, ProjectStages: true,
	})
	if err != nil || outcome.Task.State != core.TaskClosed {
		t.Fatalf("recovered blueprint=%+v err=%v", outcome.Task, err)
	}
	if count, countErr := st.CountEvents(ctx, parent.ID, "blueprint.closed"); countErr != nil || count != 1 {
		t.Fatalf("blueprint.closed events=%d err=%v", count, countErr)
	}
}

func TestMemoryCancelTaskIsAtomicAndCancelledSessionIsTerminal(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	task := core.Task{ID: "cancel-task", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "cancel-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	completedJob := core.Job{ID: "completed-order", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, completedJob); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: completedJob.ID, TaskID: task.ID, JobID: completedJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	completedOrder, err := storetestFor(st).ClaimWorkOrder(ctx, completedJob.ID, core.WorkOrderClaim{SessionID: "completed-session", ClientToken: "completed-secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	completedOrder.State = core.WorkOrderCompleted
	if err = storetestFor(st).UpdateWorkOrder(ctx, completedOrder, core.WorkOrderCmdSubmitSpec); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRenew, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return st.RenewWorkerClaimCommand(ctx, lease, job.ID, core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "different-claimant", SessionID: "session"}, time.Minute)
	}); !errors.Is(err, ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-claimant renew error=%v", err)
	}
	attemptID := claimed.AttemptID
	cancelled, err := storetestFor(st).CancelTask(ctx, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	if err != nil || cancelled.State != core.TaskClosed || cancelled.NextStage != "" {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	order, _ := st.GetWorkOrder(ctx, job.ID)
	completed, _ := st.GetWorkOrder(ctx, completedJob.ID)
	if order.State != core.WorkOrderCancelled || order.SessionID != "" || order.WorkerID != "" || order.AttemptID != "" || order.LastAttemptID != attemptID || order.LastAttemptOutcome != core.WorkOrderOutcomeCancelled || completed.State != core.WorkOrderCompleted {
		t.Fatalf("orders cancelled=%+v completed=%+v", order, completed)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil || eventAttemptID(t, events, "work_order.cancelled", job.ID) != attemptID {
		t.Fatalf("cancelled event identity err=%v want=%q", err, attemptID)
	}
	if _, err = storetestFor(st).RenewWorkerClaim(ctx, job.ID, "worker", "wrong-session", time.Minute); !errors.Is(err, ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-session renew error=%v", err)
	}
	if _, err = storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: "wrong-session"}); !errors.Is(err, ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-session release error=%v", err)
	}
	if _, err = storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: "session"}); !errors.Is(err, ErrWorkOrderCancelled) {
		t.Fatalf("release error=%v", err)
	}
	if _, err = storetestFor(st).RenewWorkerClaim(ctx, job.ID, "worker", "session", time.Minute); !errors.Is(err, ErrWorkOrderCancelled) {
		t.Fatalf("renew error=%v", err)
	}
	claimed.Progress = "must not land"
	if err = storetestFor(st).UpdateWorkOrder(ctx, claimed); !errors.Is(err, ErrWorkOrderCancelled) {
		t.Fatalf("update error=%v", err)
	}
	interventions, _ := st.ListInterventions(ctx, task.ID)
	cancelEvents, _ := st.CountEvents(ctx, task.ID, "task.cancelled")
	if len(interventions) != 1 || interventions[0].ActorID != "operator" || cancelEvents != 1 {
		t.Fatalf("interventions=%+v events=%d", interventions, cancelEvents)
	}
	if _, err = storetestFor(st).CancelTask(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "again"}); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("second cancel error=%v", err)
	}
	cancelEvents, _ = st.CountEvents(ctx, task.ID, "task.cancelled")
	if cancelEvents != 1 {
		t.Fatalf("duplicate cancellation event count=%d", cancelEvents)
	}
}

func TestStalledTaskDerivesOnlyActionableNonTerminalOrders(t *testing.T) {
	stalled := StalledTask([]core.WorkOrder{{ID: "retry", State: core.WorkOrderQueued, RetrySuppressed: true, LastFailureMessage: "provider rejected model"}})
	if stalled == nil || stalled.WorkOrder.ID != "retry" || stalled.LastFailure == "" {
		t.Fatalf("stalled=%+v", stalled)
	}
	if got := StalledTask([]core.WorkOrder{{ID: "expired", State: core.WorkOrderStale, LastFailureDetail: "queue wait exceeded"}}); got == nil || got.Reason != "queue deadline expired" || got.LastFailure != "queue wait exceeded" {
		t.Fatalf("expired order stalled=%+v", got)
	}
	if got := StalledTask([]core.WorkOrder{{ID: "loop", State: core.WorkOrderQueued, AutomaticRetryCount: 2, LastFailureMessage: "dispatch failed"}}); got == nil || got.Reason != "dispatch is failing repeatedly" {
		t.Fatalf("loop stalled=%+v", got)
	}
	for _, state := range []core.WorkOrderState{
		core.WorkOrderClaimed,
		core.WorkOrderSubmitted,
	} {
		t.Run(string(state), func(t *testing.T) {
			got := StalledTask([]core.WorkOrder{{
				ID:                  string(state),
				State:               state,
				AutomaticRetryCount: 3,
				LastFailureMessage:  "historical dispatch failure",
				LastFailureDetail:   "historical child error",
			}})
			if got != nil {
				t.Fatalf("%s order stalled=%+v", state, got)
			}
		})
	}
	for _, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderCancelled} {
		if got := StalledTask([]core.WorkOrder{{ID: string(state), State: state, RetrySuppressed: true}}); got != nil {
			t.Fatalf("%s order stalled=%+v", state, got)
		}
	}
}
