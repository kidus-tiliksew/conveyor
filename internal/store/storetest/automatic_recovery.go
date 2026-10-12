package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func RunAutomaticRecoveryConformance(t *testing.T, st store.Store, ctx context.Context) {
	now := time.Now().UTC()
	workspace, _ := store.WorkspaceFromContext(ctx)
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	orderID := taskID + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	tick := now.Add(2 * time.Minute)
	if _, err := taskops.New(st).TickOrderClock(ctx, tick); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || scheduled.State != core.WorkOrderTimedOut || scheduled.NextAutomaticRecoveryAt.IsZero() || !scheduled.NextAutomaticRecoveryAt.After(tick) {
		t.Fatalf("scheduled=%+v err=%v", scheduled, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != 1 || recovered.AutomaticRecoveryWindowStartedAt.IsZero() || !recovered.NextAutomaticRecoveryAt.IsZero() || recovered.RetrySuppressed {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var automatic bool
	for _, event := range events {
		if event.Kind != "work_order.recovered" {
			continue
		}
		var payload struct {
			Automatic      bool   `json:"automatic"`
			RecoveryNumber int    `json:"recovery_number"`
			WindowStarted  string `json:"window_started_at"`
			RequestID      string `json:"request_id"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if event.ActorID != "queue:"+orderID || !payload.Automatic || payload.RecoveryNumber != 1 || payload.WindowStarted == "" || payload.RequestID == "" {
			t.Fatalf("event actor=%s payload=%+v", event.ActorID, payload)
		}
		automatic = true
	}
	if !automatic {
		t.Fatal("automatic recovery event missing")
	}
	for recovered.AutomaticRecoveryCount < 2 {
		if _, err = ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session-" + recovered.ID, ClientToken: "token-" + recovered.ID, Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		tick = time.Now().UTC().Add(2 * time.Minute)
		if _, err = taskops.New(st).TickOrderClock(ctx, tick); err != nil {
			t.Fatal(err)
		}
		scheduled, err = st.GetWorkOrder(ctx, orderID)
		if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
			t.Fatalf("reschedule=%+v err=%v", scheduled, err)
		}
		if _, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
			t.Fatal(err)
		}
		recovered, err = st.GetWorkOrder(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session-last", ClientToken: "token-last", Lease: time.Minute, ExecutionTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	tick = time.Now().UTC().Add(2 * time.Minute)
	if _, err = taskops.New(st).TickOrderClock(ctx, tick); err != nil {
		t.Fatal(err)
	}
	exhausted, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || exhausted.State != core.WorkOrderTimedOut || !exhausted.RetrySuppressed || exhausted.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason {
		t.Fatalf("exhausted=%+v err=%v", exhausted, err)
	}
	reset, err := RecoverWorkOrder(ctx, st, orderID, "operator-reset", time.Hour)
	if err != nil || reset.State != core.WorkOrderQueued || reset.AutomaticRecoveryCount != 0 || reset.RetrySuppressed || reset.RetrySuppressionReason != "" {
		t.Fatalf("operator reset=%+v err=%v", reset, err)
	}
	staleTask := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{ID: staleTask, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + staleTask, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	staleID := staleTask + "-implement"
	if err = st.CreateJob(ctx, core.Job{ID: staleID, TaskID: staleTask, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = CreateWorkOrder(ctx, st, core.WorkOrder{ID: staleID, TaskID: staleTask, JobID: staleID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Minute), QueueDeadline: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, now); err != nil {
		t.Fatal(err)
	}
	stale, err := st.GetWorkOrder(ctx, staleID)
	if err != nil || stale.State != core.WorkOrderStale || stale.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("stale schedule=%+v err=%v", stale, err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, stale.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	redispatched, err := st.GetWorkOrder(ctx, staleID)
	if err != nil || redispatched.State != core.WorkOrderQueued || redispatched.AutomaticRecoveryCount != 1 {
		t.Fatalf("stale redispatch=%+v err=%v", redispatched, err)
	}
	runAutomaticRecoveryRemaining(t, st, ctx, workspace)
}
