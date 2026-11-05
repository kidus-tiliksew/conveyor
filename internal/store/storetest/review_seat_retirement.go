package storetest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runReviewSeatRetirement proves on every backend that no operator retirement
// removes a seat from a review round. A queued or stale seat refuses with
// ErrWorkOrderPreemptConflict and changes no round, the round still settles, a
// claimed seat still requeues, and an order outside review still retires
// (req-review-gates-evidence AC-2.6; DEC-77(2); component-work-orders).
func runReviewSeatRetirement(t *testing.T, x Fixture) {
	t.Run("QueuedSeatRefusedAndRoundSettles", func(t *testing.T) { runQueuedSeatRetirementRefused(t, x) })
	t.Run("StaleSeatRefused", func(t *testing.T) { runStaleSeatRetirementRefused(t, x) })
	t.Run("ClaimedSeatRequeues", func(t *testing.T) { runClaimedSeatPreemptRequeues(t, x) })
	t.Run("QueuedImplementOrderRetires", func(t *testing.T) { runQueuedImplementOrderRetires(t, x) })
}

func preemptFixtureOrder(x Fixture, order core.WorkOrder, requestID string) (store.WorkOrderPreemptResult, error) {
	return taskops.ExecuteWorkOrder(x.Context, x.Backend, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
		return x.Backend.PreemptWorkOrderCommand(x.Context, lease, store.WorkOrderPreemptRequest{WorkOrderID: order.ID, RequestID: requestID, Reason: "fixture retirement"})
	})
}

// seatRoundSnapshot holds everything a refused retirement must leave unchanged.
type seatRoundSnapshot struct {
	Task    core.TaskState
	Records []store.ReviewRoundSeats
	Orders  []seatRoundOrder
	Jobs    map[string]core.JobState
	Events  int
}

type seatRoundOrder struct {
	ID              string
	State           core.WorkOrderState
	Round, Seat     int
	Outcome         string
	RetrySuppressed bool
}

func snapshotSeatRound(t *testing.T, x Fixture, taskID string) seatRoundSnapshot {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	records, err := x.Backend.ListReviewRoundSeats(x.Context, taskID)
	requireOK(t, err)
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	jobs, err := x.Backend.ListJobs(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	snapshot := seatRoundSnapshot{Task: task.State, Records: records, Jobs: map[string]core.JobState{}, Events: len(events)}
	for _, order := range orders {
		snapshot.Orders = append(snapshot.Orders, seatRoundOrder{order.ID, order.State, order.ReviewRound, order.ReviewSeat, order.LastAttemptOutcome, order.RetrySuppressed})
	}
	for _, job := range jobs {
		snapshot.Jobs[job.ID] = job.State
	}
	return snapshot
}

func requireSeatRetirementRefused(t *testing.T, x Fixture, seat core.WorkOrder) {
	t.Helper()
	before := snapshotSeatRound(t, x, seat.TaskID)
	requestID := "retire-seat-" + core.NewTaskID()
	// The refused request records nothing, so its ID refuses the same way again.
	for range 2 {
		if _, err := preemptFixtureOrder(x, seat, requestID); !errors.Is(err, store.ErrWorkOrderPreemptConflict) {
			t.Fatalf("retiring review seat %s err=%v, want ErrWorkOrderPreemptConflict", seat.ID, err)
		}
	}
	if after := snapshotSeatRound(t, x, seat.TaskID); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused retirement changed the round:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func runQueuedSeatRetirementRefused(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 2, false)
	requireSeatRetirementRefused(t, x, orders[1])
	stored, err := x.Backend.GetWorkOrder(x.Context, orders[1].ID)
	requireOK(t, err)
	if stored.State != core.WorkOrderQueued || !stored.Claimable {
		t.Fatalf("refused seat=%+v, want a claimable queued order", stored)
	}
	// The scenario that stranded a round: seat 2 retired, seat 1 approved. Both
	// seats now answer, and the round settles once.
	ffVerdict(t, x, orders[0], "approve")
	if _, count := ffRoundCompleted(t, x, task.ID); count != 0 {
		t.Fatal("round settled before its second seat answered")
	}
	ffVerdict(t, x, orders[1], "approve")
	settlement, count := ffRoundCompleted(t, x, task.ID)
	if count != 1 || settlement.Verdict != "approve" || len(settlement.Reviews) != 2 {
		t.Fatalf("settlement=%+v count=%d", settlement, count)
	}
}

func runStaleSeatRetirementRefused(t *testing.T, x Fixture) {
	_, orders := ffRound(t, x, 2, false)
	moveRetirementOrder(t, x, orders[1].ID, core.WorkOrderStale, core.WorkOrderCmdMarkStale)
	stale, err := x.Backend.GetWorkOrder(x.Context, orders[1].ID)
	requireOK(t, err)
	if stale.State != core.WorkOrderStale {
		t.Fatalf("fixture seat state=%s, want stale", stale.State)
	}
	requireSeatRetirementRefused(t, x, stale)
}

func runClaimedSeatPreemptRequeues(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 2, false)
	records, err := x.Backend.ListReviewRoundSeats(x.Context, task.ID)
	requireOK(t, err)
	session := ffClaim(t, x, orders[1])
	result, err := preemptFixtureOrder(x, orders[1], "preempt-seat-"+core.NewTaskID())
	requireOK(t, err)
	if result.RevokedSessionID != session {
		t.Fatalf("preempt revoked session %q, want %q", result.RevokedSessionID, session)
	}
	requeued, err := x.Backend.GetWorkOrder(x.Context, orders[1].ID)
	requireOK(t, err)
	if requeued.State != core.WorkOrderQueued || requeued.LastAttemptOutcome != core.WorkOrderOutcomePreempted ||
		requeued.ReviewRound != orders[1].ReviewRound || requeued.ReviewSeat != orders[1].ReviewSeat {
		t.Fatalf("preempted seat=%+v, want a requeued seat of the same round", requeued)
	}
	after, err := x.Backend.ListReviewRoundSeats(x.Context, task.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(records, after) {
		t.Fatalf("preempt changed the round record:\nbefore=%+v\nafter=%+v", records, after)
	}
	// Once requeued the seat is a queued seat, so a retirement refuses it too.
	requireSeatRetirementRefused(t, x, requeued)
	ffVerdict(t, x, orders[0], "approve")
	ffVerdict(t, x, requeued, "approve")
	if settlement, count := ffRoundCompleted(t, x, task.ID); count != 1 || settlement.Verdict != "approve" {
		t.Fatalf("settlement=%+v count=%d", settlement, count)
	}
}

func runQueuedImplementOrderRetires(t *testing.T, x Fixture) {
	taskID := newRetirementTask(t, x, core.TaskQueued, core.StageImplement)
	id := addRetirementOrder(t, x, taskID, taskID+"-implement-1", core.StageImplement, 0, 0)
	order, err := x.Backend.GetWorkOrder(x.Context, id)
	requireOK(t, err)
	result, err := preemptFixtureOrder(x, order, "retire-implement-"+core.NewTaskID())
	requireOK(t, err)
	if result.WorkOrder.State != core.WorkOrderCancelled {
		t.Fatalf("retired order state=%s, want cancelled", result.WorkOrder.State)
	}
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	retired := 0
	for _, event := range events {
		if event.Kind == "work_order.retired" {
			retired++
		}
	}
	if retired != 1 {
		t.Fatalf("work_order.retired events=%d, want 1", retired)
	}
}
