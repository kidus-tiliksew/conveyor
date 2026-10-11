package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runRecoveryRefusal(t *testing.T, x Fixture) {
	t.Run("PolicyEnableThenRecover", func(t *testing.T) { runRecoveryKeepsEnabledPolicy(t, x) })
	t.Run("IntakeVerifyIgnoresWorkspaceOff", func(t *testing.T) { runRecoveryKeepsIntakePolicy(t, x) })
	t.Run("RefuseWriteFree", func(t *testing.T) { runRecoveryRefuseWriteFree(t, x) })
	t.Run("PredicateMatrix", func(t *testing.T) { runRecoveryRefusalMatrix(t, x) })
}

func policyChange(t *testing.T, x Fixture, taskID string, r store.SetupChangeRequest) (store.SetupChangeResult, error) {
	t.Helper()
	return taskops.ExecuteSetupChange(x.Context, x.Backend, taskID, func(l taskops.TaskLease) (store.SetupChangeResult, error) {
		return x.Backend.ChangeTaskPolicyCommand(x.Context, l, r)
	})
}

func snapshotRecovery(t *testing.T, x Fixture, taskID, orderID string) (core.WorkOrder, core.Job, core.Task, int) {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, orderID)
	requireOK(t, err)
	jobs, err := x.Backend.ListJobs(x.Context, taskID)
	requireOK(t, err)
	var job core.Job
	for _, candidate := range jobs {
		if candidate.ID == order.JobID {
			job = candidate
			break
		}
	}
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	return order, job, task, len(events)
}

func requireNoSetupRefreeze(t *testing.T, x Fixture, taskID string) {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	for _, event := range events {
		if event.Kind == "task.setup.refrozen" {
			t.Fatalf("recovery rewrote frozen policy: %+v", event)
		}
	}
}

func timeoutQueuedOrder(t *testing.T, x Fixture, order core.WorkOrder) core.WorkOrder {
	t.Helper()
	claim := core.WorkOrderClaim{
		SessionID: "timeout-" + order.ID, ClientToken: "timeout-" + order.ID,
		ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Nanosecond,
	}
	claimed, err := ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
	requireOK(t, err)
	_, err = taskops.New(x.Backend).TickOrderClock(x.Context, time.Now().UTC())
	requireOK(t, err)
	timedOut, err := x.Backend.GetWorkOrder(x.Context, claimed.ID)
	requireOK(t, err)
	if timedOut.State != core.WorkOrderTimedOut {
		t.Fatalf("clock state=%s, want timed_out", timedOut.State)
	}
	return timedOut
}

func createReviewSource(t *testing.T, x Fixture, task core.Task) {
	t.Helper()
	source := core.WorkOrder{
		ID: task.ID + "-review-1", JobID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview,
		State: core.WorkOrderQueued, BaselineSHA: "submitted-base", HeadSHA: task.ReviewedHeadSHA, ReviewRound: 1,
		CreatedAt: time.Now().UTC(),
	}
	requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: source.JobID, TaskID: task.ID, Stage: source.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(x.Context, x.Backend, source))
}

func runRecoveryKeepsEnabledPolicy(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", State: core.TaskRunning,
		NextStage: core.StageReview, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC(),
	}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.ExecutionSetup{
		MaxBounces: 3,
		Review:     config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
		ExecutionSettings: config.ContextualExecutionSettings{
			Review: config.ReviewExecutionSettings{TimeoutText: "1h"},
		},
	}
	requireOK(t, st.CreateTask(ctx, task))
	createReviewSource(t, x, task)
	enabled := true
	result, err := policyChange(t, x, task.ID, store.SetupChangeRequest{
		TaskID: task.ID, RequestID: core.NewTaskID(), Reason: "enable verification",
		Policy: &store.TaskPolicyChange{VerifyStage: &enabled, StageTimeouts: map[string]string{"verify": "45m"}},
	})
	requireOK(t, err)
	verify, err := st.GetWorkOrder(ctx, result.CreatedWorkOrders[0])
	requireOK(t, err)
	timeoutQueuedOrder(t, x, verify)
	recovered, err := RecoverWorkOrder(ctx, st, verify.ID, "recover-enabled-"+task.ID, time.Hour)
	requireOK(t, err)
	if recovered.State != core.WorkOrderQueued || recovered.ExecutionTimeoutText != "45m" {
		t.Fatalf("recovered=%+v", recovered)
	}
	stored, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if !stored.SetupContract.VerifyStage || stored.SetupContract.ExecutionSettings.Verify.TimeoutText != "45m" {
		t.Fatalf("frozen policy changed: %+v", stored.SetupContract)
	}
	requireNoSetupRefreeze(t, x, task.ID)
	claimed, err := ClaimWorkOrder(ctx, st, recovered.ID, core.WorkOrderClaim{
		SessionID: "claim-enabled", ClientToken: "claim-enabled", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute,
	})
	requireOK(t, err)
	if claimed.HeadSHA != "submitted-head" || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claim=%+v", claimed)
	}
}

func runRecoveryKeepsIntakePolicy(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", State: core.TaskRunning,
		NextStage: core.StageVerify, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC(),
	}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.ExecutionSetup{
		VerifyStage: true,
		ExecutionSettings: config.ContextualExecutionSettings{
			Verify: config.ImplementationSettings{TimeoutText: "45m"},
		},
	}
	requireOK(t, st.CreateTask(ctx, task))
	order := core.WorkOrder{
		ID: task.ID + "-verify-1", JobID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify,
		State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, CreatedAt: time.Now().UTC(),
		QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), ExecutionTimeoutText: "45m",
	}
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, order))
	timeoutQueuedOrder(t, x, order)
	recovered, err := RecoverWorkOrder(ctx, st, order.ID, "recover-intake-"+task.ID, time.Hour)
	requireOK(t, err)
	if recovered.ExecutionTimeoutText != "45m" {
		t.Fatalf("timeout drifted: %+v", recovered)
	}
	stored, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if !stored.SetupContract.VerifyStage || stored.SetupContract.ExecutionSettings.Verify.TimeoutText != "45m" {
		t.Fatalf("intake policy changed: %+v", stored.SetupContract)
	}
	requireNoSetupRefreeze(t, x, task.ID)
	if _, err = ClaimWorkOrder(ctx, st, recovered.ID, core.WorkOrderClaim{
		SessionID: "claim-intake", ClientToken: "claim-intake", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertUnchanged(t *testing.T, beforeOrder, afterOrder core.WorkOrder, beforeJob, afterJob core.Job, beforeTask, afterTask core.Task, beforeEvents, afterEvents int) {
	t.Helper()
	if afterOrder.State != beforeOrder.State || afterOrder.UpdatedAt != beforeOrder.UpdatedAt || afterOrder.RedispatchCount != beforeOrder.RedispatchCount {
		t.Fatalf("order mutated: before=%+v after=%+v", beforeOrder, afterOrder)
	}
	if afterJob.State != beforeJob.State {
		t.Fatalf("job mutated: before=%+v after=%+v", beforeJob, afterJob)
	}
	if afterTask.SetupContract.VerifyStage != beforeTask.SetupContract.VerifyStage || afterTask.NextStage != beforeTask.NextStage {
		t.Fatalf("task mutated: before=%+v after=%+v", beforeTask, afterTask)
	}
	if afterEvents != beforeEvents {
		t.Fatalf("events %d -> %d", beforeEvents, afterEvents)
	}
}

func runRecoveryRefuseWriteFree(t *testing.T, x Fixture) {
	cases := []struct {
		name   string
		mutate func(*core.Task, *core.WorkOrder)
		want   string
	}{
		{
			name: "approval_stale_refresh_head",
			mutate: func(task *core.Task, order *core.WorkOrder) {
				task.SetupContract.VerifyStage = true
				task.NextStage = core.StageVerify
				task.ReviewedHeadSHA = "reviewed"
				task.ApprovalStale = true
				task.RefreshHeadSHA = "refresh-head"
				order.HeadSHA = "order-head"
			},
			want: "differs from the verify head refresh-head (the refresh head, because approval_stale is set)",
		},
		{
			name: "next_stage_implement",
			mutate: func(task *core.Task, order *core.WorkOrder) {
				task.SetupContract.VerifyStage = true
				task.NextStage = core.StageImplement
				task.ReviewedHeadSHA = "head"
				order.HeadSHA = "head"
			},
			want: "next_stage is implement, not verify",
		},
		{
			name: "missing_head",
			mutate: func(task *core.Task, order *core.WorkOrder) {
				task.SetupContract.VerifyStage = true
				task.NextStage = core.StageVerify
				task.ReviewedHeadSHA = ""
				order.HeadSHA = ""
			},
			want: "the order has no head",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/recover", func(t *testing.T) { refuseRecoverWriteFree(t, x, tc.mutate, tc.want) })
		t.Run(tc.name+"/redispatch", func(t *testing.T) { refuseRedispatchWriteFree(t, x, tc.mutate, tc.want) })
	}
}

func refuseRecoverWriteFree(t *testing.T, x Fixture, mutate func(*core.Task, *core.WorkOrder), want string) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	order := core.WorkOrder{
		ID: task.ID + "-verify-1", JobID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify,
		State: core.WorkOrderQueued, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour),
	}
	mutate(&task, &order)
	requireOK(t, st.CreateTask(ctx, task))
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, order))
	persisted, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	persisted.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, persisted, core.WorkOrderCmdTimeout))
	beforeOrder, beforeJob, beforeTask, beforeEvents := snapshotRecovery(t, x, task.ID, order.ID)
	requestID := "refuse-recover-" + task.ID
	_, err = RecoverWorkOrder(ctx, st, order.ID, requestID, time.Hour)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("recover err=%v want %q", err, want)
	}
	afterOrder, afterJob, afterTask, afterEvents := snapshotRecovery(t, x, task.ID, order.ID)
	assertUnchanged(t, beforeOrder, afterOrder, beforeJob, afterJob, beforeTask, afterTask, beforeEvents, afterEvents)
	_, retry := RecoverWorkOrder(ctx, st, order.ID, requestID, time.Hour)
	if retry == nil || !strings.Contains(retry.Error(), want) {
		t.Fatalf("retry recover err=%v", retry)
	}
	afterRetry, _, _, afterRetryEvents := snapshotRecovery(t, x, task.ID, order.ID)
	if afterRetry.State != beforeOrder.State || afterRetryEvents != beforeEvents {
		t.Fatal("retry recorded recovery")
	}
}

func refuseRedispatchWriteFree(t *testing.T, x Fixture, mutate func(*core.Task, *core.WorkOrder), want string) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	order := core.WorkOrder{
		ID: task.ID + "-verify-1", JobID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify,
		State: core.WorkOrderQueued, CreatedAt: now, QueueEnteredAt: now.Add(-2 * time.Hour), QueueDeadline: now.Add(-time.Hour),
	}
	mutate(&task, &order)
	requireOK(t, st.CreateTask(ctx, task))
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, order))
	beforeOrder, beforeJob, beforeTask, beforeEvents := snapshotRecovery(t, x, task.ID, order.ID)
	_, err := RedispatchWorkOrder(ctx, st, order.ID, time.Hour)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("redispatch err=%v want %q", err, want)
	}
	afterOrder, afterJob, afterTask, afterEvents := snapshotRecovery(t, x, task.ID, order.ID)
	assertUnchanged(t, beforeOrder, afterOrder, beforeJob, afterJob, beforeTask, afterTask, beforeEvents, afterEvents)
}

func runRecoveryRefusalMatrix(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC()
	type cell struct {
		name        string
		task        core.Task
		order       core.WorkOrder
		successor   bool
		clock       core.WorkOrderState
		queuedRetry bool
		want        func(taskID, orderID, successorID string) string
	}
	cells := []cell{
		{
			name:        "running/verify-on/equal/verify/queued",
			task:        core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order:       core.WorkOrder{Stage: core.StageVerify, HeadSHA: "head"},
			clock:       core.WorkOrderQueued,
			queuedRetry: true,
			want:        func(string, string, string) string { return "" },
		},
		{
			name:  "running/verify-on/equal/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order: core.WorkOrder{Stage: core.StageVerify, HeadSHA: "head"},
			clock: core.WorkOrderStale,
			want:  func(string, string, string) string { return "" },
		},
		{
			name:  "running/verify-off/equal/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head"},
			order: core.WorkOrder{Stage: core.StageVerify, HeadSHA: "head"},
			clock: core.WorkOrderStale,
			want: func(taskID, orderID, _ string) string {
				return "verify order " + orderID + " cannot be claimed: verify stage is off in task " + taskID + "'s frozen policy"
			},
		},
		{
			name:  "running/verify-on/mismatch/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "task-head", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order: core.WorkOrder{Stage: core.StageVerify, HeadSHA: "order-head"},
			clock: core.WorkOrderStale,
			want: func(_, orderID, _ string) string {
				return "verify order " + orderID + " cannot be claimed: order head order-head differs from the verify head task-head"
			},
		},
		{
			name:  "running/verify-on/empty/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageVerify, SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order: core.WorkOrder{Stage: core.StageVerify},
			clock: core.WorkOrderStale,
			want: func(_, orderID, _ string) string {
				return "verify order " + orderID + " cannot be claimed: the order has no head"
			},
		},
		{
			name:  "running/verify-on/refresh/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "reviewed", ApprovalStale: true, RefreshHeadSHA: "refresh", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order: core.WorkOrder{Stage: core.StageVerify, HeadSHA: "order"},
			clock: core.WorkOrderStale,
			want: func(_, orderID, _ string) string {
				return "verify order " + orderID + " cannot be claimed: order head order differs from the verify head refresh (the refresh head, because approval_stale is set)"
			},
		},
		{
			name:  "running/next-review/verify/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "head", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order: core.WorkOrder{Stage: core.StageVerify, HeadSHA: "head"},
			clock: core.WorkOrderStale,
			want: func(taskID, orderID, _ string) string {
				return "verify order " + orderID + " cannot be claimed: task " + taskID + " next_stage is review, not verify"
			},
		},
		{
			name:  "awaiting/recovery-implement/spec/stale",
			task:  core.Task{State: core.TaskAwaiting, RecoveryStage: core.StageImplement, NextStage: core.StageSpec},
			order: core.WorkOrder{Stage: core.StageSpec},
			clock: core.WorkOrderStale,
			want: func(taskID, orderID, _ string) string {
				return "work order " + orderID + " cannot be recovered because task " + taskID + " has passed the plan gate"
			},
		},
		{
			name:  "running/implement-advanced/implement/stale",
			task:  core.Task{State: core.TaskRunning, NextStage: core.StageReview},
			order: core.WorkOrder{Stage: core.StageImplement},
			clock: core.WorkOrderStale,
			want: func(taskID, orderID, _ string) string {
				return "work order " + orderID + " cannot be recovered because task " + taskID + " has advanced to review"
			},
		},
		{
			name:      "running/verify-on/equal/verify/stale/successor",
			task:      core.Task{State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head", SetupContract: config.ExecutionSetup{VerifyStage: true}},
			order:     core.WorkOrder{Stage: core.StageVerify, HeadSHA: "head"},
			clock:     core.WorkOrderStale,
			successor: true,
			want: func(_, orderID, successorID string) string {
				return "work order " + orderID + " was superseded by same-stage successor " + successorID
			},
		},
	}
	for _, item := range cells {
		item := item
		t.Run(item.name, func(t *testing.T) {
			task := item.task
			task.ID = core.NewTaskID()
			task.Workspace = x.Workspace
			task.Repo = "conveyor"
			task.CreatedAt = now
			task.Branch = "conveyor/task-" + task.ID
			requireOK(t, st.CreateTask(ctx, task))
			order := item.order
			order.ID = task.ID + "-" + string(order.Stage) + "-1"
			order.JobID = order.ID
			order.TaskID = task.ID
			order.State = core.WorkOrderQueued
			order.CreatedAt = now
			order.QueueEnteredAt = now
			order.QueueDeadline = now.Add(time.Hour)
			requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}))
			requireOK(t, CreateWorkOrder(ctx, st, order))
			if item.queuedRetry {
				claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{
					SessionID: "retry-" + order.ID, ClientToken: "retry-" + order.ID,
					ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute,
				})
				requireOK(t, err)
				_, err = ReleaseWorkerClaim(ctx, st, order.ID, claimed.WorkerID, core.WorkOrderRelease{SessionID: claimed.SessionID})
				requireOK(t, err)
			}
			if item.clock == core.WorkOrderStale {
				_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
					return st.ApplyWorkOrderClock(ctx, lease, task.ID, order.QueueDeadline.Add(time.Second))
				})
				requireOK(t, err)
			}
			successorID := ""
			if item.successor {
				later := core.WorkOrder{
					ID: task.ID + "-" + string(order.Stage) + "-2", JobID: task.ID + "-" + string(order.Stage) + "-2",
					TaskID: task.ID, Stage: order.Stage, State: core.WorkOrderQueued, HeadSHA: order.HeadSHA,
					CreatedAt: now.Add(time.Second), QueueEnteredAt: now.Add(time.Second), QueueDeadline: now.Add(2 * time.Hour),
				}
				requireOK(t, st.CreateJob(ctx, core.Job{ID: later.JobID, TaskID: task.ID, Stage: later.Stage, State: core.JobPending}))
				requireOK(t, CreateWorkOrder(ctx, st, later))
				successorID = later.ID
			}
			current, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			want := item.want(task.ID, current.ID, successorID)
			_, recoverErr := RecoverWorkOrder(ctx, st, current.ID, "matrix-"+current.ID, time.Hour)
			if want != "" {
				if recoverErr == nil || recoverErr.Error() != want {
					t.Fatalf("recover err=%v want %q", recoverErr, want)
				}
			} else if recoverErr != nil {
				t.Fatalf("recover err=%v", recoverErr)
			}
			if item.clock == core.WorkOrderStale {
				currentAfter, getErr := st.GetWorkOrder(ctx, current.ID)
				requireOK(t, getErr)
				if currentAfter.State == core.WorkOrderStale {
					_, redispatchErr := RedispatchWorkOrder(ctx, st, current.ID, time.Hour)
					if want != "" {
						if redispatchErr == nil || redispatchErr.Error() != want {
							t.Fatalf("redispatch err=%v want %q", redispatchErr, want)
						}
					} else if redispatchErr != nil && !strings.Contains(redispatchErr.Error(), "already claimed") {
						t.Fatalf("redispatch err=%v", redispatchErr)
					}
				}
			}
			if recoverErr != nil {
				return
			}
			recovered, getErr := st.GetWorkOrder(ctx, current.ID)
			requireOK(t, getErr)
			_, claimErr := ClaimWorkOrder(ctx, st, recovered.ID, core.WorkOrderClaim{
				SessionID: "matrix-claim-" + recovered.ID, ClientToken: "matrix-claim-" + recovered.ID,
				ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute,
			})
			if claimErr != nil && deterministicClaimRefusal(claimErr) {
				t.Fatalf("claim refused after successful recover: %v", claimErr)
			}
		})
	}
}

func deterministicClaimRefusal(err error) bool {
	text := err.Error()
	return strings.Contains(text, "cannot be claimed") ||
		strings.Contains(text, "cannot be recovered") ||
		strings.Contains(text, "was superseded") ||
		strings.Contains(text, "is superseded")
}
