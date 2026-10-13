package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Stale and timed-out orders are recoverable stops owned by the DEC-62
// order clock: the dispatcher never mints a same-stage successor for them
// (component-work-orders "Stages and order minting"; feature-bounded-automatic-recovery AR-3).

func staleMintFixture(t *testing.T, id string) (context.Context, store.Store, *Dispatcher, core.Task) {
	t.Helper()
	return staleMintFixtureAt(t, id, core.StageImplement)
}

func staleMintFixtureAt(t *testing.T, id string, stage core.Stage) (context.Context, store.Store, *Dispatcher, core.Task) {
	t.Helper()
	settings := config.ContextualExecutionSettings{
		ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
		Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "45m"},
	}
	harness := config.Harness{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}
	setup := config.ExecutionSetup{Name: "backend", ExecutionSettings: settings, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Harness: "codex"}}}}
	cfg := (&config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Setups: []config.ExecutionSetup{setup}, DefaultSetup: "backend"}).WithSetup(setup)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: id, Workspace: "demo", Title: "Stale", SetupName: setup.Name, SetupContract: setup, State: core.TaskQueued, NextStage: stage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := New(st, cfg, nil)
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	first := task.ID + "-implement-1"
	if stage == core.StageReview {
		first = task.ID + "-review-1-seat-1"
	}
	requireOrderIDs(t, ctx, st, task.ID, first)
	return ctx, st, d, task
}

func requireOrderIDs(t *testing.T, ctx context.Context, st store.Store, taskID string, want ...string) []core.WorkOrder {
	t.Helper()
	orders, err := st.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != len(want) {
		t.Fatalf("orders=%d want %v: %+v", len(orders), want, orders)
	}
	for i, order := range orders {
		if order.ID != want[i] {
			t.Fatalf("order %d=%s want %s", i, order.ID, want[i])
		}
	}
	return orders
}

// expireQueue moves the order's queue deadline into the past so it reads stale.
func expireQueue(t *testing.T, ctx context.Context, st store.Store, id string) {
	t.Helper()
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	order.QueueEnteredAt = time.Now().UTC().Add(-2 * time.Hour)
	order.QueueDeadline = time.Now().UTC().Add(-time.Second)
	if err = storetest.UpdateWorkOrder(ctx, st, order); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetWorkOrder(ctx, id); got.State != core.WorkOrderStale {
		t.Fatalf("order %s state=%s want stale", id, got.State)
	}
}

func countOrderEvents(t *testing.T, ctx context.Context, st store.Store, taskID, kind string) (n int, automatic int) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		n++
		var payload struct {
			Automatic bool `json:"automatic"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Automatic {
			automatic++
		}
	}
	return n, automatic
}

func TestStaleOrderIsRedispatchedNotReissued(t *testing.T) {
	ctx, st, d, task := staleMintFixture(t, "stale-holds")
	orderID := task.ID + "-implement-1"
	clock := taskops.New(st)
	for round := 1; ; round++ {
		expireQueue(t, ctx, st, orderID)
		// The daily reissue: a dispatch run over a stale order must not mint.
		if err := d.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		requireOrderIDs(t, ctx, st, task.ID, orderID)
		if _, err := clock.TickOrderClock(ctx, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		scheduled, err := st.GetWorkOrder(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if scheduled.RetrySuppressed {
			// Past the DEC-62 bound the order escalates once and keeps holding
			// the stage for the operator (AC-5.2).
			if round != config.DefaultRecoveryPolicy().AutomaticRecoveries+1 || scheduled.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason {
				t.Fatalf("round %d exhausted=%+v", round, scheduled)
			}
			break
		}
		if scheduled.NextAutomaticRecoveryAt.IsZero() {
			t.Fatalf("round %d stale order not scheduled: %+v", round, scheduled)
		}
		if _, err = clock.TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
			t.Fatal(err)
		}
		recovered, err := st.GetWorkOrder(ctx, orderID)
		if err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != round {
			t.Fatalf("round %d recovered=%+v err=%v", round, recovered, err)
		}
		if n, automatic := countOrderEvents(t, ctx, st, task.ID, "work_order.redispatched"); n != round || automatic != round {
			t.Fatalf("round %d redispatched events=%d automatic=%d", round, n, automatic)
		}
		if err = d.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		requireOrderIDs(t, ctx, st, task.ID, orderID)
	}
	for range 3 {
		if err := d.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := clock.TickOrderClock(ctx, time.Now().UTC().Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	requireOrderIDs(t, ctx, st, task.ID, orderID)
	if n, _ := countOrderEvents(t, ctx, st, task.ID, "work_order.recovery_exhausted"); n != 1 {
		t.Fatalf("recovery_exhausted events=%d", n)
	}
}

func TestTimedOutOrderIsRecoveredNotReissued(t *testing.T) {
	ctx, st, d, task := staleMintFixture(t, "timed-out-holds")
	orderID := task.ID + "-implement-1"
	if _, err := storetest.ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if got, _ := st.GetWorkOrder(ctx, orderID); got.State != core.WorkOrderTimedOut {
		t.Fatalf("state=%s want timed_out", got.State)
	}
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	requireOrderIDs(t, ctx, st, task.ID, orderID)
	clock := taskops.New(st)
	if _, err := clock.TickOrderClock(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || scheduled.State != core.WorkOrderTimedOut || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("scheduled=%+v err=%v", scheduled, err)
	}
	if _, err = clock.TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != 1 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	requireOrderIDs(t, ctx, st, task.ID, orderID)
}

// Risk 2 of the approved plan: a task that moved past the stage and bounced
// back through a real stage.bounce gets a fresh order. The old stale order is
// history behind the later review order, not a recoverable stop at the
// current stage, so it neither blocks the mint nor stays live beside it.
func TestBounceBackPastStaleOrderMintsFreshOrder(t *testing.T) {
	ctx, st, d, task := staleMintFixture(t, "bounce-stale")
	first := task.ID + "-implement-1"
	stale, err := st.GetWorkOrder(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	// The order clock persists the stale state before the task moves on.
	stale.State = core.WorkOrderStale
	if err = storetest.UpdateWorkOrder(ctx, st, stale, core.WorkOrderCmdMarkStale); err != nil {
		t.Fatal(err)
	}
	ops := taskops.New(st)
	perform := func(command taskops.Command) {
		t.Helper()
		if _, err := ops.Perform(ctx, task.ID, command); err != nil {
			t.Fatalf("%s: %v", command.Kind, err)
		}
	}
	// The task leaves implement for review and a review order is minted there.
	perform(taskops.Command{Kind: core.TaskDispatchStart})
	perform(taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	later := task.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: later, TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: later, TaskID: task.ID, JobID: later, Stage: core.StageReview, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Review bounces the task back to implement.
	perform(taskops.Command{Kind: core.TaskDispatchStart})
	perform(taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true})
	bounced, err := st.GetTask(ctx, task.ID)
	if err != nil || bounced.State != core.TaskQueued || bounced.NextStage != core.StageImplement {
		t.Fatalf("bounced task=%+v err=%v", bounced, err)
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]core.WorkOrder{}
	for _, order := range orders {
		byID[order.ID] = order
	}
	fresh, ok := byID[task.ID+"-implement-2"]
	if !ok || fresh.State != core.WorkOrderQueued {
		t.Fatalf("no fresh implement order after bounce: %+v", orders)
	}
	if old := byID[first]; old.State != core.WorkOrderCancelled || old.RetrySuppressionReason != "superseded" {
		t.Fatalf("implement-1 was not retired as superseded: %+v", old)
	}
}

func TestActiveWorkOrderBlockingStates(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	for _, tc := range []struct {
		state  core.WorkOrderState
		reason string
		want   bool
	}{
		{core.WorkOrderQueued, "", true},
		{core.WorkOrderClaimed, "", true},
		{core.WorkOrderStale, "", true},
		{core.WorkOrderTimedOut, "", true},
		{core.WorkOrderSubmitted, "", false},
		{core.WorkOrderCompleted, "", false},
		{core.WorkOrderCancelled, "", false},
		{core.WorkOrderStale, "merge-conflict", false},
		{core.WorkOrderTimedOut, "merge-conflict", false},
		{core.WorkOrderQueued, "merge-conflict", true},
	} {
		t.Run(string(tc.state)+"/"+tc.reason, func(t *testing.T) {
			order := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: tc.state, ReasonCode: tc.reason}
			d := &Dispatcher{Store: fixedOrders{Store: store.NewMemory(), orders: []core.WorkOrder{order}}}
			_, got, err := d.activeWorkOrder(ctx, "t", core.StageImplement, tc.reason)
			if err != nil || got != tc.want {
				t.Fatalf("active=%v want %v err=%v", got, tc.want, err)
			}
		})
	}
}

type fixedOrders struct {
	store.Store
	orders []core.WorkOrder
}

func (f fixedOrders) ListTaskWorkOrders(context.Context, string) ([]core.WorkOrder, error) {
	return f.orders, nil
}

// A stale review seat is a recoverable stop like any other order: redelivered
// dispatch reuses the round and the order clock redispatches the seat, so no
// second live round is minted (component-work-orders; DEC-62).
func TestStaleReviewSeatIsRedispatchedNotReRounded(t *testing.T) {
	ctx, st, d, task := staleMintFixtureAt(t, "stale-seat", core.StageReview)
	seat := task.ID + "-review-1-seat-1"
	expireQueue(t, ctx, st, seat)
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	requireOrderIDs(t, ctx, st, task.ID, seat)
	clock := taskops.New(st)
	if _, err := clock.TickOrderClock(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.GetWorkOrder(ctx, seat)
	if err != nil || scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("seat not scheduled=%+v err=%v", scheduled, err)
	}
	if _, err = clock.TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetWorkOrder(ctx, seat)
	if err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != 1 {
		t.Fatalf("seat recovered=%+v err=%v", recovered, err)
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	requireOrderIDs(t, ctx, st, task.ID, seat)
}
