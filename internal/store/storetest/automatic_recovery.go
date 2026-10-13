package storetest

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
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
	requireRetrying(t, st, ctx, taskID, scheduled, tick, 1)
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
		requireRetrying(t, st, ctx, taskID, scheduled, tick, recovered.AutomaticRecoveryCount+1)
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
	action, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{Now: tick})
	if err != nil || action.Actor != nextaction.ActorOperator || action.Kind != nextaction.KindStalled || action.Details.StallReason != nextaction.StallAutomaticRecoveryExhausted || !store.TaskNeedsAttention(action) {
		t.Fatalf("exhausted next action=%+v err=%v", action, err)
	}
	if action.CurrentOrder == nil || action.CurrentOrder.ID != orderID {
		t.Fatalf("exhausted next action order=%+v", action.CurrentOrder)
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

// requireRetrying asserts the store-backed next action projects the order's
// scheduled automatic recovery (AR-6) with its window-local attempt.
func requireRetrying(t *testing.T, st store.Store, ctx context.Context, taskID string, scheduled core.WorkOrder, now time.Time, attempt int) {
	t.Helper()
	action, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{Now: now})
	if err != nil || action.Actor != nextaction.ActorFactory || action.Kind != nextaction.KindRetrying || action.Details.ReasonCode != "automatic_recovery" || store.TaskNeedsAttention(action) {
		t.Fatalf("scheduled next action=%+v err=%v", action, err)
	}
	if action.CurrentOrder == nil || action.CurrentOrder.ID != scheduled.ID || action.Deadline == nil || !action.Deadline.Equal(scheduled.NextAutomaticRecoveryAt) {
		t.Fatalf("scheduled next action order=%+v deadline=%v want %s at %s", action.CurrentOrder, action.Deadline, scheduled.ID, scheduled.NextAutomaticRecoveryAt)
	}
	if !strings.Contains(action.Summary, "(attempt "+strconv.Itoa(attempt)+")") {
		t.Fatalf("scheduled summary=%q want attempt %d", action.Summary, attempt)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultRecoveryPolicy()
	flow := store.FoldTaskFlow(events, action, policy, now)
	if want := (store.FlowRetrying{Attempt: attempt, Of: policy.AutomaticRecoveries}); flow.Side.Retrying == nil || *flow.Side.Retrying != want {
		t.Fatalf("side.retrying=%+v want %+v", flow.Side.Retrying, want)
	}
}
