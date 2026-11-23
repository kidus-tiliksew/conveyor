package storetest

// Fixture helpers that kept conformance suites share. They came from suite
// files that the 53-suite core retired. Each helper stays because a kept
// suite, a kept fixture, or a test outside this package still calls it
// (DEC-38).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
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

// PR907MandatoryValidation is the exact unresolved entry from the accepted
// 260910-9899e4-review-1-seat-1 verdict at head 962caf43aeea0e9a7ff2b32537144fcbecf83499.
const PR907MandatoryValidation = "`make test`, `make test-integration`, and configured-backend `make test-integration-singlestore-ci` pass. Environment limitations are reported separately; focused checks do not substitute for the mandatory aggregates."

const ReviewDoneCriteriaPlan = "## Approach\nVerify acceptance.\n\n## Files touched\n- internal/store/store.go\n\n## Ordering\n1. Verify.\n\n## Risks\n- Contradictory approval.\n\n## Done criteria\n- " + PR907MandatoryValidation

const seatFixtureHead, seatFixtureBase = "submitted-head", "submitted-base"

// seatMembers builds count content-free seats with the production job IDs.
func seatMembers(taskID string, round, count int) ([]core.Job, []core.WorkOrder) {
	now := time.Now().UTC()
	jobs := make([]core.Job, 0, count)
	orders := make([]core.WorkOrder, 0, count)
	for seat := 1; seat <= count; seat++ {
		id := fmt.Sprintf("%s-review-%d-seat-%d", taskID, round, seat)
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat,
			HeadSHA: seatFixtureHead, BaselineSHA: seatFixtureBase, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
	}
	return jobs, orders
}

func resolveSeats(t *testing.T, x Fixture, taskID string, round int) store.ReviewRoundSeatResolution {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	in, err := store.LoadReviewRoundSeatInputs(x.Context, x.Backend, task, store.ReviewSeatMinimum(task.SetupContract, 0), round)
	requireOK(t, err)
	return store.ResolveReviewRoundSeats(in)
}

func createSeatRound(x Fixture, taskID string, seats store.ReviewRoundSeatResolution, jobs []core.Job, orders []core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, x.Backend.CreateReviewRoundCommand(x.Context, lease, taskID, seats, jobs, orders)
	})
	return err
}

func seedReviewRoundWithVerdict(t *testing.T, st store.Store, ctx context.Context, workspace string) (core.Task, []core.WorkOrder) {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, st.CreateTask(ctx, task))
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "verdict-" + task.ID, ClientToken: "verdict", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	first.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
	return task, orders
}

// expireReviewSeat loses one claim's lease so the seat is interrupted.
func expireReviewSeat(t *testing.T, st store.Store, ctx context.Context, id, label string) {
	t.Helper()
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "worker", SessionID: label + "-" + id, ClientToken: label + "-" + id, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

type automaticRoundEvent struct {
	Automatic             bool     `json:"automatic"`
	ExhaustedWorkOrderIDs []string `json:"exhausted_work_order_ids"`
}

func automaticRoundEvents(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) []automaticRoundEvent {
	t.Helper()
	var out []automaticRoundEvent
	for _, event := range listEventsOK(t, st, ctx, taskID) {
		if event.Kind != kind {
			continue
		}
		var payload automaticRoundEvent
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Automatic {
			out = append(out, payload)
		}
	}
	return out
}

func acceptReviewRound(t *testing.T, st store.Store, ctx context.Context, taskID string, decision func(core.WorkOrder, int) core.ReviewDecision) {
	t.Helper()
	jobs, orders := reviewRound(taskID, 1)
	requireOK(t, CreateReviewRound(ctx, st, taskID, jobs, orders))
	for i, order := range orders {
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(order, i+1)))
	}
}

func accumulateAgentBounces(t *testing.T, plane *taskops.Plane, st store.Store, ctx context.Context, id string, n, max int) {
	t.Helper()
	for round := 1; round <= n; round++ {
		if round > 1 {
			if _, err := plane.Perform(ctx, id, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
				t.Fatal(err)
			}
		}
		jobs, orders := reviewRound(id, round)
		requireOK(t, CreateReviewRound(ctx, st, id, jobs, orders))
		for i, order := range orders {
			requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{
				TaskID: id, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: round, ReviewSeat: i + 1,
				Verdict: "changes_requested", ReasonCode: "fix", Summary: "bounce", Feedback: "again", MaxBounces: max,
			}))
		}
	}
}

func createRunningTask(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	return createTask(t, st, ctx, workspace, core.TaskRunning, "")
}

func createTask(t *testing.T, st store.Store, ctx context.Context, workspace string, state core.TaskState, next core.Stage) string {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, NextStage: next, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GateKind != "" {
		t.Fatalf("new task gate_kind=%q", stored.GateKind)
	}
	return id
}

func designContent(version string) string {
	return fmt.Sprintf("# Queue authority %s\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", version)
}

func designProposalContent(label string) string {
	return "# " + label + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/workorder/**\n```"
}

func authorityRequirementVersion(statement string) core.RequirementVersion {
	return core.RequirementVersion{Content: "# Authority\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}}
}

// tickTask runs the order clock for one task at at, as the order clock does.
func tickTask(t *testing.T, x Fixture, taskID string, at time.Time) int {
	t.Helper()
	acted, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, taskID, core.WorkOrderCmdTimeout, func(lease taskops.TaskLease) (int, error) {
		return x.Backend.ApplyWorkOrderClock(x.Context, lease, taskID, at)
	})
	requireOK(t, err)
	return acted
}

// seedStoredStale writes the retired stale value on one stored order. It is
// the only hand-set state in this case: no command writes stale any more
// (component-work-orders invariant 5).
func seedStoredStale(t *testing.T, x Fixture, order core.WorkOrder, exhausted bool, scheduledAt time.Time) {
	t.Helper()
	if x.SeedStoredStaleWorkOrder == nil {
		t.Fatal("conformance fixture must provide SeedStoredStaleWorkOrder for the stored stale cases (component-work-orders invariant 5)")
	}
	x.SeedStoredStaleWorkOrder(t, x.Context, order.ID, exhausted, scheduledAt)
	if stored := storedOrder(t, x, order); stored.State != core.WorkOrderStale {
		t.Fatalf("seed did not store order %s as stale: %+v", order.ID, stored)
	}
}

func storedOrder(t *testing.T, x Fixture, order core.WorkOrder) core.WorkOrder {
	t.Helper()
	stored, err := x.Backend.ListTaskWorkOrdersStored(x.Context, order.TaskID)
	requireOK(t, err)
	for _, candidate := range stored {
		if candidate.ID == order.ID {
			return candidate
		}
	}
	t.Fatalf("order %s missing from the stored read", order.ID)
	return core.WorkOrder{}
}

// countReconciled binds a drift recorder to ctx and returns a count of each
// task's queue repairs: the dispatch.reconciled events operator redispatch
// writes plus the repairs ReconcileQueuedTasks reported, which the start-up
// check records as drift.repaired (component-durable-queue, Reconciliation
// between tasks and jobs; component-persistence, Drift repair reports).
func countReconciled(ctx context.Context, st store.Store) (context.Context, func(string) (int, error)) {
	var mu sync.Mutex
	reported := map[string]int{}
	ctx = store.WithDriftRecorder(ctx, func(repair store.DriftRepair) {
		mu.Lock()
		reported[repair.TaskID]++
		mu.Unlock()
	})
	return ctx, func(taskID string) (int, error) {
		events, err := st.CountEvents(ctx, taskID, "dispatch.reconciled")
		mu.Lock()
		defer mu.Unlock()
		return events + reported[taskID], err
	}
}
