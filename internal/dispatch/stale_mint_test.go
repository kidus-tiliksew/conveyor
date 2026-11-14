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
	setup := config.PolicyContract{StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "2h", Review: "45m"}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}
	routes := map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP},
		"review":    {Execution: config.ExecutionMCP},
	}
	cfg := (&config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Triage: config.TriageRoute{Model: "control", TimeoutText: "20m"}, Stages: routes}, Review: setup.Review}).WithPolicy(setup)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: id, Workspace: "demo", Title: "Stale", SetupName: "backend", SetupContract: setup, State: core.TaskQueued, NextStage: stage, CreatedAt: time.Now()}
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

// expireQueue moves the order's stored queue deadline into the past. The
// order still reads queued and claimable: an order nobody claimed has no
// queue clock (component-work-orders invariant 5).
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
	if got, _ := st.GetWorkOrder(ctx, id); got.State != core.WorkOrderQueued || !got.Claimable {
		t.Fatalf("order %s state=%s claimable=%t want queued and claimable", id, got.State, got.Claimable)
	}
}

// requireWaiting proves the order waits in place with no automatic recovery
// scheduled, counted, or exhausted and no stale or redispatch audit.
func requireWaiting(t *testing.T, ctx context.Context, st store.Store, taskID, id string) {
	t.Helper()
	got, err := st.GetWorkOrder(ctx, id)
	if err != nil || got.State != core.WorkOrderQueued || !got.Claimable || got.RetrySuppressed || got.RedispatchCount != 0 || got.AutomaticRecoveryCount != 0 || !got.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("order %s=%+v err=%v, want it waiting in place", id, got, err)
	}
	for _, kind := range []string{"work_order.stale", "work_order.redispatched", "work_order.recovered", "work_order.recovery_exhausted"} {
		if n, _ := countOrderEvents(t, ctx, st, taskID, kind); n != 0 {
			t.Fatalf("order %s recorded %d %s events, want none", id, n, kind)
		}
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

// An order nobody claimed waits past its stored queue deadline: redelivered
// dispatch mints no successor, and ticks of the order clock, a day apart,
// schedule, count, redispatch, and exhaust nothing. This replaces the retired
// expectation that the clock marked it stale, redispatched it to the bound,
// and escalated it (req-task-lifecycle-and-queue AC-9.5; component-work-orders
// invariant 5; DEC-117(2)).
func TestWaitingOrderIsNotReissuedOrRedispatched(t *testing.T) {
	ctx, st, d, task := staleMintFixture(t, "stale-holds")
	orderID := task.ID + "-implement-1"
	clock := taskops.New(st)
	expireQueue(t, ctx, st, orderID)
	for round := range 4 {
		if err := d.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		requireOrderIDs(t, ctx, st, task.ID, orderID)
		if _, err := clock.TickOrderClock(ctx, time.Now().UTC().Add(time.Duration(round)*25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		requireWaiting(t, ctx, st, task.ID, orderID)
	}
}

func TestTimedOutOrderIsRecoveredNotReissued(t *testing.T) {
	ctx, st, d, task := staleMintFixture(t, "timed-out-holds")
	orderID := task.ID + "-implement-1"
	if _, err := storetest.ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Millisecond}); err != nil {
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

func (f fixedOrders) ListTaskWorkOrdersStored(context.Context, string) ([]core.WorkOrder, error) {
	return f.orders, nil
}

func (f fixedOrders) ListWorkOrdersForTasks(context.Context, []string) ([]core.WorkOrder, error) {
	return f.orders, nil
}

// projectingOrders mirrors the backend read contract pinned by storetest
// StoredReadKeepsExpiredClaimUnprojected: the stored read returns rows as
// persisted, the task read projects elapsed clocks.
type projectingOrders struct {
	store.Store
	orders []core.WorkOrder
}

func (p projectingOrders) ListTaskWorkOrders(context.Context, string) ([]core.WorkOrder, error) {
	now := time.Now().UTC()
	out := make([]core.WorkOrder, 0, len(p.orders))
	for _, order := range p.orders {
		out = append(out, store.ProjectWorkOrderAt(order, now))
	}
	return out, nil
}

func (p projectingOrders) ListWorkOrdersForTasks(ctx context.Context, _ []string) ([]core.WorkOrder, error) {
	return p.ListTaskWorkOrders(ctx, "")
}

func (p projectingOrders) ListTaskWorkOrdersStored(context.Context, string) ([]core.WorkOrder, error) {
	return p.orders, nil
}

// A claimed merge-conflict fix order whose lease or execution deadline has
// elapsed is no longer admitted as active, even before the order clock
// persists its expiry: conflict fix orders keep their own admission rule over
// projected state (component-work-orders "Stages and order minting";
// component-durable-queue dispatch outcomes). Stage holds over stored claims
// are pinned by TestDispatchJobCompletesForExpiredHistoricalClaim.
func TestMergeConflictAdmissionUsesProjectedClaimState(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	now := time.Now().UTC()
	claimed := func(lease, deadline time.Time) core.WorkOrder {
		return core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderClaimed, ReasonCode: "merge-conflict", ClaimantID: "worker", SessionID: "s", AttemptID: "a", LeaseExpiresAt: lease, ExecutionDeadline: deadline, CreatedAt: now.Add(-time.Hour)}
	}
	for _, tc := range []struct {
		name         string
		order        core.WorkOrder
		conflictLive bool
	}{
		{"live", claimed(now.Add(time.Hour), now.Add(time.Hour)), true},
		{"lease-expired", claimed(now.Add(-time.Minute), now.Add(time.Hour)), false},
		{"deadline-passed", claimed(now.Add(time.Hour), now.Add(-time.Minute)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Dispatcher{Store: projectingOrders{Store: store.NewMemory(), orders: []core.WorkOrder{tc.order}}}
			if _, got, err := d.activeWorkOrder(ctx, "t", core.StageImplement, "merge-conflict"); err != nil || got != tc.conflictLive {
				t.Fatalf("merge-conflict active=%v want %v err=%v", got, tc.conflictLive, err)
			}
		})
	}
}

// A review seat nobody claimed waits past its stored queue deadline like any
// other order: redelivered dispatch reuses the round, and the order clock
// leaves the seat alone, so no second live round is minted. This replaces the
// retired expectation that the clock marked the seat stale and redispatched it
// (component-work-orders invariant 5; DEC-117(2)).
func TestWaitingReviewSeatIsNotReRounded(t *testing.T) {
	ctx, st, d, task := staleMintFixtureAt(t, "stale-seat", core.StageReview)
	seat := task.ID + "-review-1-seat-1"
	expireQueue(t, ctx, st, seat)
	clock := taskops.New(st)
	for round := range 3 {
		if err := d.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		requireOrderIDs(t, ctx, st, task.ID, seat)
		if _, err := clock.TickOrderClock(ctx, time.Now().UTC().Add(time.Duration(round)*25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		requireWaiting(t, ctx, st, task.ID, seat)
	}
}
