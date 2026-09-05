package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func reviewRound(taskID string, round int) ([]core.Job, []core.WorkOrder) {
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= 2; seat++ {
		id := core.NewTaskID()
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	return jobs, orders
}

func runReviewRounds(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, interrupted := range []bool{false, true} {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		first.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
		claim := core.WorkOrderClaim{SessionID: "seat-two", ClientToken: "two", Lease: time.Minute, ExecutionTimeout: time.Nanosecond}
		if interrupted {
			claim.Lease = time.Nanosecond
			claim.ExecutionTimeout = time.Hour
		}
		_, err = ClaimWorkOrder(ctx, st, orders[1].ID, claim)
		requireOK(t, err)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		if interrupted {
			request := store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: "recover", Round: 1}
			_, err = RecoverInterruptedReviewRound(ctx, st, request, time.Hour)
			requireOK(t, err)
			_, err = RecoverInterruptedReviewRound(ctx, st, request, time.Hour)
			requireOK(t, err)
			retained, err := st.GetWorkOrder(ctx, orders[0].ID)
			requireOK(t, err)
			recovered, err := st.GetWorkOrder(ctx, orders[1].ID)
			requireOK(t, err)
			if retained.State != core.WorkOrderCompleted || recovered.State != core.WorkOrderQueued || recovered.RetrySuppressed {
				t.Fatal("interrupted recovery changed completed seat or failed to recover missing seat")
			}
		} else {
			jobs2, orders2 := reviewRound(task.ID, 2)
			request := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "retry", PriorRound: 1, Reason: "seat timed out", PRHead: "fixture-head"}
			result, err := RetryReviewRound(ctx, st, request, jobs2, orders2)
			requireOK(t, err)
			if result.NewRound != 2 || len(result.WorkOrders) != 2 {
				t.Fatal("retry did not create round two")
			}
			_, err = RetryReviewRound(ctx, st, request, jobs2, orders2)
			requireOK(t, err)
			request.Reason = "different"
			if _, err := RetryReviewRound(ctx, st, request, jobs2, orders2); !errors.Is(err, store.ErrReviewRetryConflict) {
				t.Fatalf("changed retry error=%v", err)
			}
			persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
			requireOK(t, err)
			if len(persisted) != 4 {
				t.Fatal("retry lost historical orders or duplicated seats")
			}
		}
	}
}

func runReviewAcceptance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	for i, order := range orders {
		decision := core.ReviewDecision{TaskID: task.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: i + 1, Verdict: "approve", ReasonCode: "verified", Summary: "fixture acceptance", MaxBounces: 3}
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
	}
	current, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if current.State != core.TaskAwaiting {
		t.Fatalf("accepted review task state=%s", current.State)
	}
	events, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	count := 0
	for _, event := range events {
		if event.Kind == "review.round_completed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("round completion events=%d", count)
	}
}
