package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runRedispatchOfferParity proves on every backend that the statement's
// redispatch choice agrees with EnsureTaskEnqueued, the backend check behind
// the redispatch route on a queued task: a stage the backend holds is withheld
// with a refusal naming the holder, and an unheld queued task keeps the offer.
// A claim whose lease elapsed behind a later order holds until the order clock
// persists its expiry, though every read projects it as expired
// (req-operator-next-action AC-1.4; component-next-action, Invariants 7;
// component-durable-queue).
func runRedispatchOfferParity(t *testing.T, x Fixture) {
	if x.LineageHTTP == nil {
		t.Fatal("conformance fixture must provide LineageHTTP for the redispatch offer")
	}
	st, ctx := x.Backend, x.Context
	handler := x.LineageHTTP(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	newTask := func(next core.Stage) string {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Redispatch " + id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: next, CreatedAt: now}))
		return id
	}
	addOrder := func(order core.WorkOrder) {
		t.Helper()
		order.JobID = order.ID
		requireOK(t, st.CreateJob(ctx, core.Job{ID: order.ID, TaskID: order.TaskID, Stage: order.Stage, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, order))
	}
	addStale := func(taskID string) string {
		t.Helper()
		id := taskID + "-implement-1"
		addOrder(core.WorkOrder{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-48 * time.Hour), QueueDeadline: now.Add(-time.Minute)})
		return id
	}
	// requireParity compares the shared predicate over the orders a read returns
	// with the backend's own stored check.
	requireParity := func(taskID, holder string) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		refusal := core.NextActionRedispatchRefusal(task, orders)
		ensureErr := st.EnsureTaskEnqueued(ctx, taskID)
		if holder == "" {
			if refusal != "" || ensureErr != nil {
				t.Fatalf("task %s refusal=%q ensure=%v, want both to accept", taskID, refusal, ensureErr)
			}
			return
		}
		if !errors.Is(ensureErr, store.ErrTaskHeldByWorkOrder) || !strings.Contains(ensureErr.Error(), holder) {
			t.Fatalf("task %s ensure=%v, want held by %s", taskID, ensureErr, holder)
		}
		if !strings.Contains(refusal, core.TaskHeldByWorkOrderRefusal) || !strings.Contains(refusal, holder) {
			t.Fatalf("task %s refusal=%q, want held by %s", taskID, refusal, holder)
		}
	}
	redispatchRefusal := func(action nextaction.NextAction) string {
		for _, refusal := range action.Details.Refusals {
			if refusal.Choice == "redispatch" {
				return refusal.Text
			}
		}
		return ""
	}
	offered := func(action nextaction.NextAction) bool {
		for _, choice := range action.Choices {
			if choice.ID == "redispatch" {
				return true
			}
		}
		return false
	}

	// A stale order at the task's next stage holds it.
	held := newTask(core.StageImplement)
	heldOrder := addStale(held)
	action := claimFailureNextAction(t, handler, x.Workspace, held)
	if action.Details.StallReason != nextaction.StallStale || offered(action) || !strings.Contains(redispatchRefusal(action), heldOrder) {
		t.Fatalf("held statement stall=%q choices=%+v refusals=%+v", action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(held, heldOrder)

	// The same order behind a return to the plan stage holds nothing.
	moved := newTask(core.StageSpec)
	addStale(moved)
	action = claimFailureNextAction(t, handler, x.Workspace, moved)
	if action.Details.StallReason != nextaction.StallStale || !offered(action) || redispatchRefusal(action) != "" {
		t.Fatalf("unheld statement stall=%q choices=%+v refusals=%+v", action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(moved, "")

	// A claim behind a later review seat holds its stage, live or expired.
	lease := 100 * time.Millisecond
	claimed := newTask(core.StageImplement)
	claimedOrder := claimed + "-implement-1"
	addOrder(core.WorkOrder{ID: claimedOrder, TaskID: claimed, Stage: core.StageImplement, State: core.WorkOrderQueued})
	_, err := ClaimWorkOrder(ctx, st, claimedOrder, core.WorkOrderClaim{SessionID: "redispatch-offer-" + claimed, ClientToken: "redispatch-offer-token-" + claimed, Lease: lease, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	ops := taskops.New(st)
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	requireOK(t, err)
	addOrder(core.WorkOrder{ID: claimed + "-review-1-seat-1", TaskID: claimed, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderQueued})
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskDispatchStart})
	requireOK(t, err)
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true})
	requireOK(t, err)
	time.Sleep(2 * lease)
	action = claimFailureNextAction(t, handler, x.Workspace, claimed)
	if action.Kind != nextaction.KindStalled || offered(action) || !strings.Contains(redispatchRefusal(action), claimedOrder) {
		t.Fatalf("expired claim statement %s/%s choices=%+v refusals=%+v", action.Kind, action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(claimed, claimedOrder)

	// The persisted expiry ends the hold on the backend and in the predicate.
	_, err = ops.TickOrderClock(ctx, time.Now().UTC())
	requireOK(t, err)
	requireParity(claimed, "")
}
