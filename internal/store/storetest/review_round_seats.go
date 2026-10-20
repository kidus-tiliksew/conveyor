package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunReviewRoundSeatsConformance proves feature-review-seat-minimum RS-2 on
// every backend: each round-building command writes its review_round_seats
// record and one review.round_created event atomically with its orders, a
// replay writes nothing, the planned floor reads the latest approved plan
// version, a legacy round keeps its order-count checks and gets no record,
// failures leave no partial data, and records are workspace-scoped
// (DEC-77, DEC-78; req-review-gates-evidence AC-2.4, AC-2.6, AC-2.7).
func RunReviewRoundSeatsConformance(t *testing.T, x Fixture) {
	t.Run("CreateAtomicRecordAndEvent", func(t *testing.T) { runSeatCreateAtomic(t, x) })
	t.Run("CreateReplay", func(t *testing.T) { runSeatCreateReplay(t, x) })
	t.Run("RetryCarriesAndReplays", func(t *testing.T) { runSeatRetry(t, x, false) })
	t.Run("FailedRetryRollsBack", func(t *testing.T) { runSeatRetry(t, x, true) })
	t.Run("PolicyHandoffCarriesAndReplays", func(t *testing.T) { runSeatPolicyHandoff(t, x, false) })
	t.Run("FailedPolicyHandoffRollsBack", func(t *testing.T) { runSeatPolicyHandoff(t, x, true) })
	t.Run("LatestApprovedPlanFloor", func(t *testing.T) { runSeatLatestApprovedPlanFloor(t, x) })
	t.Run("ApprovedOmittedCountClearsFloor", func(t *testing.T) { runSeatApprovedOmittedCount(t, x) })
	t.Run("LegacyCarryAndReplay", func(t *testing.T) { runSeatLegacyCarry(t, x) })
	t.Run("LegacyPartialRoundRefused", func(t *testing.T) { runSeatLegacyPartial(t, x) })
	t.Run("FailedCreateRollsBack", func(t *testing.T) { runSeatFailedCreate(t, x) })
	t.Run("WorkspaceIsolation", func(t *testing.T) { runSeatWorkspaceIsolation(t, x) })
}

const seatFixtureHead, seatFixtureBase = "submitted-head", "submitted-base"

// seatTask is a running review task whose frozen policy has the given seat
// count and a submitted head.
func seatTask(t *testing.T, x Fixture, seats int) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: seatFixtureHead, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.ExecutionSetup{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats)}, ExecutionSettings: config.ContextualExecutionSettings{Review: config.ReviewExecutionSettings{TimeoutText: "1h"}}}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	return stored
}

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
	in, err := store.LoadReviewRoundSeatInputs(x.Context, x.Backend, taskID, store.ReviewSeatMinimum(task.SetupContract, 0), round)
	requireOK(t, err)
	return store.ResolveReviewRoundSeats(in)
}

func createSeatRound(x Fixture, taskID string, seats store.ReviewRoundSeatResolution, jobs []core.Job, orders []core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, x.Backend.CreateReviewRoundCommand(x.Context, lease, taskID, seats, jobs, orders)
	})
	return err
}

func retrySeatRound(x Fixture, request store.ReviewRoundRetryRequest, seats store.ReviewRoundSeatResolution, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	return taskops.ExecuteWorkOrder(x.Context, x.Backend, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewRoundRetryResult, error) {
		return x.Backend.RetryReviewRoundCommand(x.Context, lease, request, seats, jobs, orders)
	})
}

func changeSeatPolicy(x Fixture, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	return taskops.ExecuteSetupChange(x.Context, x.Backend, request.TaskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
		return x.Backend.ChangeTaskPolicyCommand(x.Context, lease, request)
	})
}

// seatSnapshot is the comparable persisted state of one task.
type seatSnapshot struct {
	orders, events, roundCreated int
	records                      []store.ReviewRoundSeats
}

func takeSeatSnapshot(t *testing.T, x Fixture, taskID string) seatSnapshot {
	t.Helper()
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	records, err := x.Backend.ListReviewRoundSeats(x.Context, taskID)
	requireOK(t, err)
	snapshot := seatSnapshot{orders: len(orders), events: len(events), records: records}
	for _, event := range events {
		if event.Kind == "review.round_created" {
			snapshot.roundCreated++
		}
	}
	return snapshot
}

func requireSeatSnapshot(t *testing.T, label string, got, want seatSnapshot) {
	t.Helper()
	if got.orders != want.orders || got.events != want.events || got.roundCreated != want.roundCreated || len(got.records) != len(want.records) {
		t.Fatalf("%s changed persisted state: got orders=%d events=%d round_created=%d records=%+v, want orders=%d events=%d round_created=%d records=%+v",
			label, got.orders, got.events, got.roundCreated, got.records, want.orders, want.events, want.roundCreated, want.records)
	}
}

func requireSeatRecord(t *testing.T, x Fixture, taskID string, round, count int, source string, minimum int) store.ReviewRoundSeats {
	t.Helper()
	records, err := x.Backend.ListReviewRoundSeats(x.Context, taskID)
	requireOK(t, err)
	for _, record := range records {
		if record.ReviewRound != round {
			continue
		}
		if record.TaskID != taskID || record.SeatCount != count || record.SeatCountSource != source || record.MinimumSeatCount != minimum || record.CreatedAt.IsZero() {
			t.Fatalf("round %d record=%+v, want count=%d source=%s minimum=%d", round, record, count, source, minimum)
		}
		return record
	}
	t.Fatalf("round %d has no record among %+v", round, records)
	return store.ReviewRoundSeats{}
}

// requireRoundCreated finds exactly one review.round_created event for round
// and checks its seat fields.
func requireRoundCreated(t *testing.T, x Fixture, taskID string, round, count int, source string, minimum int) map[string]any {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	var found []map[string]any
	for _, event := range events {
		if event.Kind != "review.round_created" {
			continue
		}
		var payload map[string]any
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if number, _ := payload["review_round"].(float64); int(number) == round {
			found = append(found, payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("round %d review.round_created events=%+v, want one", round, found)
	}
	payload := found[0]
	gotCount, _ := payload["seat_count"].(float64)
	gotMinimum, _ := payload["minimum_seat_count"].(float64)
	if int(gotCount) != count || payload["seat_count_source"] != source || int(gotMinimum) != minimum {
		t.Fatalf("round %d review.round_created=%+v, want count=%d source=%s minimum=%d", round, payload, count, source, minimum)
	}
	return payload
}

func reviewOrdersInRound(t *testing.T, x Fixture, taskID string, round int) []core.WorkOrder {
	t.Helper()
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	var result []core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == round {
			result = append(result, order)
		}
	}
	return result
}

func approvePlan(t *testing.T, x Fixture, taskID string, seats *int, approve bool) core.SpecVersion {
	t.Helper()
	spec, err := x.Backend.CreateSpecVersion(x.Context, core.SpecVersion{TaskID: taskID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}), ReviewSeats: seats})
	requireOK(t, err)
	if approve {
		requireOK(t, x.Backend.ApproveSpecVersion(x.Context, taskID, spec.Version))
	}
	return spec
}

func seatCount(value int) *int { return &value }

func runSeatCreateAtomic(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seats := resolveSeats(t, x, task.ID, 1)
	if seats.SeatCount != 2 || seats.Source != store.ReviewSeatSourceMinimum || seats.MinimumSeatCount != 2 {
		t.Fatalf("first round resolution=%+v", seats)
	}
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	requireSeatRecord(t, x, task.ID, 1, 2, store.ReviewSeatSourceMinimum, 2)
	requireRoundCreated(t, x, task.ID, 1, 2, store.ReviewSeatSourceMinimum, 2)
	if created := reviewOrdersInRound(t, x, task.ID, 1); len(created) != 2 {
		t.Fatalf("round 1 orders=%+v", created)
	}
}

func runSeatCreateReplay(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	before := takeSeatSnapshot(t, x, task.ID)
	// A recorded round replays as a no-op, even with different metadata.
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	changed := seats
	changed.Source = store.ReviewSeatSourceCarried
	requireOK(t, createSeatRound(x, task.ID, changed, jobs, orders))
	requireSeatSnapshot(t, "create replay", takeSeatSnapshot(t, x, task.ID), before)
}

// seatTimedOutRound opens round 1 for task through the resolver and leaves it
// recoverable: seat 1 completed and every other seat timed out.
func seatTimedOutRound(t *testing.T, x Fixture, task core.Task) store.ReviewRoundSeatResolution {
	t.Helper()
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	first, err := ClaimWorkOrder(x.Context, x.Backend, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one-" + task.ID, ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	first.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, first, core.WorkOrderCmdSubmitReviewVerdict))
	for i, order := range orders[1:] {
		_, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{SessionID: fmt.Sprintf("seat-%d-%s", i+2, task.ID), ClientToken: "timeout", Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
		requireOK(t, err)
	}
	_, err = taskops.New(x.Backend).TickOrderClock(x.Context, time.Now().UTC().Add(time.Second))
	requireOK(t, err)
	return seats
}

func runSeatRetry(t *testing.T, x Fixture, failFirst bool) {
	task := seatTask(t, x, 1)
	// An approved planned count of two opens round 1 with two seats.
	approvePlan(t, x, task.ID, seatCount(2), true)
	first := seatTimedOutRound(t, x, task)
	if first.SeatCount != 2 || first.Source != store.ReviewSeatSourcePlanned {
		t.Fatalf("round 1 resolution=%+v", first)
	}
	seats := resolveSeats(t, x, task.ID, 2)
	if seats.SeatCount != 2 || seats.Source != store.ReviewSeatSourceCarried || seats.MinimumSeatCount != 1 {
		t.Fatalf("retried round resolution=%+v", seats)
	}
	request := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "seat-retry-" + task.ID, PriorRound: 1, Reason: "seat timed out", PRHead: "fixture-head"}
	jobs, orders := seatMembers(task.ID, 2, seats.SeatCount)
	if failFirst {
		before := takeSeatSnapshot(t, x, task.ID)
		below := store.ReviewRoundSeatResolution{SeatCount: 1, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1, FloorSeatCount: 1}
		belowJobs, belowOrders := seatMembers(task.ID, 2, 1)
		if _, err := retrySeatRound(x, request, below, belowJobs, belowOrders); !errors.Is(err, store.ErrReviewRetryConflict) || !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
			t.Fatalf("below-floor retry error=%v", err)
		}
		requireSeatSnapshot(t, "failed retry", takeSeatSnapshot(t, x, task.ID), before)
		if _, err := x.Backend.GetWorkOrder(x.Context, belowOrders[0].ID); err == nil {
			t.Fatal("failed retry persisted a seat order")
		}
	}
	result, err := retrySeatRound(x, request, seats, jobs, orders)
	requireOK(t, err)
	if result.NewRound != 2 || len(result.WorkOrders) != 2 {
		t.Fatalf("retry result=%+v", result)
	}
	requireSeatRecord(t, x, task.ID, 2, 2, store.ReviewSeatSourceCarried, 1)
	payload := requireRoundCreated(t, x, task.ID, 2, 2, store.ReviewSeatSourceCarried, 1)
	if payload["retry_request_id"] != request.RequestID {
		t.Fatalf("retried round_created=%+v", payload)
	}
	before := takeSeatSnapshot(t, x, task.ID)
	replay, err := retrySeatRound(x, request, seats, jobs, orders)
	requireOK(t, err)
	if replay.NewRound != 2 || len(replay.WorkOrders) != 2 {
		t.Fatalf("retry replay=%+v", replay)
	}
	requireSeatSnapshot(t, "retry replay", takeSeatSnapshot(t, x, task.ID), before)
}

// seatHandoffTask has a recorded round 1 of three planned seats, a frozen
// minimum of two, verification enabled so round 1 is superseded by a queued
// verify order, and a later approved plan without a count.
func seatHandoffTask(t *testing.T, x Fixture) core.Task {
	t.Helper()
	task := seatTask(t, x, 2)
	approvePlan(t, x, task.ID, seatCount(3), true)
	seats := resolveSeats(t, x, task.ID, 1)
	if seats.SeatCount != 3 || seats.Source != store.ReviewSeatSourcePlanned {
		t.Fatalf("round 1 resolution=%+v", seats)
	}
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	enabled := true
	result, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "seat-enable-" + task.ID, Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	requireOK(t, err)
	if result.Task.NextStage != core.StageVerify || len(result.SupersededWorkOrders) != 3 || len(result.CreatedWorkOrders) != 1 {
		t.Fatalf("verify handoff=%+v", result)
	}
	approvePlan(t, x, task.ID, nil, true)
	return task
}

func runSeatPolicyHandoff(t *testing.T, x Fixture, fail bool) {
	task := seatHandoffTask(t, x)
	disabled := false
	request := store.SetupChangeRequest{TaskID: task.ID, RequestID: "seat-disable-" + task.ID, Reason: "disable verification", Policy: &store.TaskPolicyChange{VerifyStage: &disabled}}
	if fail {
		// A job already holding the handoff round's first seat ID makes the
		// transaction fail after planning; nothing of the round may persist.
		requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: fmt.Sprintf("%s-review-2-seat-1", task.ID), TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}))
		before := takeSeatSnapshot(t, x, task.ID)
		if _, err := changeSeatPolicy(x, request); err == nil {
			t.Fatal("policy handoff over a conflicting seat job succeeded")
		}
		requireSeatSnapshot(t, "failed policy handoff", takeSeatSnapshot(t, x, task.ID), before)
		stored, err := x.Backend.GetTask(x.Context, task.ID)
		requireOK(t, err)
		if stored.NextStage != core.StageVerify || !stored.SetupContract.VerifyStage {
			t.Fatalf("failed policy handoff changed the task: %+v", stored)
		}
		return
	}
	result, err := changeSeatPolicy(x, request)
	requireOK(t, err)
	if result.Task.NextStage != core.StageReview || len(result.CreatedWorkOrders) != 3 {
		t.Fatalf("review handoff=%+v", result)
	}
	// Round 1's seats were superseded, so only its record carries the count.
	requireSeatRecord(t, x, task.ID, 2, 3, store.ReviewSeatSourceCarried, 2)
	requireRoundCreated(t, x, task.ID, 2, 3, store.ReviewSeatSourceCarried, 2)
	for _, order := range reviewOrdersInRound(t, x, task.ID, 2) {
		if order.HeadSHA != seatFixtureHead || order.BaselineSHA != seatFixtureBase || order.RequiredHarness != "" || order.RequiredModel != "" {
			t.Fatalf("handoff seat=%+v", order)
		}
	}
	before := takeSeatSnapshot(t, x, task.ID)
	replay, err := changeSeatPolicy(x, request)
	requireOK(t, err)
	if len(replay.CreatedWorkOrders) != 3 {
		t.Fatalf("handoff replay=%+v", replay)
	}
	requireSeatSnapshot(t, "policy handoff replay", takeSeatSnapshot(t, x, task.ID), before)
}

func runSeatLatestApprovedPlanFloor(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	approved := approvePlan(t, x, task.ID, seatCount(3), true)
	draft := approvePlan(t, x, task.ID, seatCount(5), false)
	for _, check := range []struct {
		version int
		want    int
	}{{approved.Version, 3}, {draft.Version, 5}} {
		spec, ok, err := x.Backend.GetSpecVersion(x.Context, task.ID, check.version)
		requireOK(t, err)
		if !ok || spec.ReviewSeats == nil || *spec.ReviewSeats != check.want {
			t.Fatalf("plan version %d review_seats=%v, want %d", check.version, spec.ReviewSeats, check.want)
		}
	}
	current, ok, err := x.Backend.GetApprovedSpecVersion(x.Context, task.ID)
	requireOK(t, err)
	if !ok || current.Version != approved.Version || current.ReviewSeats == nil || *current.ReviewSeats != 3 {
		t.Fatalf("approved plan=%+v", current)
	}
	seats := resolveSeats(t, x, task.ID, 1)
	if seats.SeatCount != 3 || seats.Source != store.ReviewSeatSourcePlanned || seats.FloorSeatCount != 3 {
		t.Fatalf("resolution read the unapproved draft: %+v", seats)
	}
	before := takeSeatSnapshot(t, x, task.ID)
	below := store.ReviewRoundSeatResolution{SeatCount: 2, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2, FloorSeatCount: 2}
	belowJobs, belowOrders := seatMembers(task.ID, 1, 2)
	if err := createSeatRound(x, task.ID, below, belowJobs, belowOrders); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
		t.Fatalf("below-floor create error=%v", err)
	}
	requireSeatSnapshot(t, "below-floor create", takeSeatSnapshot(t, x, task.ID), before)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	requireSeatRecord(t, x, task.ID, 1, 3, store.ReviewSeatSourcePlanned, 2)
	requireRoundCreated(t, x, task.ID, 1, 3, store.ReviewSeatSourcePlanned, 2)
}

func runSeatApprovedOmittedCount(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	first := approvePlan(t, x, task.ID, seatCount(4), true)
	approvePlan(t, x, task.ID, nil, true)
	current, ok, err := x.Backend.GetApprovedSpecVersion(x.Context, task.ID)
	requireOK(t, err)
	if !ok || current.Version == first.Version || current.ReviewSeats != nil {
		t.Fatalf("approved plan without a count=%+v", current)
	}
	seats := resolveSeats(t, x, task.ID, 1)
	if seats.SeatCount != 2 || seats.Source != store.ReviewSeatSourceMinimum || seats.FloorSeatCount != 2 {
		t.Fatalf("an approved version without a count kept the floor: %+v", seats)
	}
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	requireSeatRecord(t, x, task.ID, 1, 2, store.ReviewSeatSourceMinimum, 2)
}

// seatLegacyRound persists round 1's seats without a record, as rounds
// created before migration 149/0024 exist.
func seatLegacyRound(t *testing.T, x Fixture, task core.Task, count int) ([]core.Job, []core.WorkOrder) {
	t.Helper()
	jobs, orders := seatMembers(task.ID, 1, count)
	for i := range jobs {
		requireOK(t, x.Backend.CreateJob(x.Context, jobs[i]))
		requireOK(t, CreateWorkOrder(x.Context, x.Backend, orders[i]))
	}
	return jobs, orders
}

func runSeatLegacyCarry(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	jobs, orders := seatLegacyRound(t, x, task, 3)
	before := takeSeatSnapshot(t, x, task.ID)
	// A complete legacy round replays through the order-count check and gets
	// no backfilled record.
	legacy := store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceCarried, MinimumSeatCount: 2, FloorSeatCount: 2}
	requireOK(t, createSeatRound(x, task.ID, legacy, jobs, orders))
	requireSeatSnapshot(t, "legacy replay", takeSeatSnapshot(t, x, task.ID), before)
	if len(before.records) != 0 {
		t.Fatalf("legacy round has records %+v", before.records)
	}
	seats := resolveSeats(t, x, task.ID, 2)
	if seats.SeatCount != 3 || seats.Source != store.ReviewSeatSourceCarried {
		t.Fatalf("legacy carry resolution=%+v", seats)
	}
	jobs2, orders2 := seatMembers(task.ID, 2, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs2, orders2))
	records, err := x.Backend.ListReviewRoundSeats(x.Context, task.ID)
	requireOK(t, err)
	if len(records) != 1 || records[0].ReviewRound != 2 || records[0].SeatCount != 3 || records[0].SeatCountSource != store.ReviewSeatSourceCarried {
		t.Fatalf("records after legacy carry=%+v", records)
	}
}

func runSeatLegacyPartial(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seatLegacyRound(t, x, task, 2)
	before := takeSeatSnapshot(t, x, task.ID)
	jobs, orders := seatMembers(task.ID, 1, 3)
	seats := store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceCarried, MinimumSeatCount: 2, FloorSeatCount: 2}
	if err := createSeatRound(x, task.ID, seats, jobs, orders); err == nil {
		t.Fatal("partially persisted legacy round was accepted")
	}
	requireSeatSnapshot(t, "partial legacy round", takeSeatSnapshot(t, x, task.ID), before)
}

func runSeatFailedCreate(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	// The last seat's job already exists, so the round fails after its first
	// member; no member, record, or event may persist.
	requireOK(t, x.Backend.CreateJob(x.Context, jobs[len(jobs)-1]))
	before := takeSeatSnapshot(t, x, task.ID)
	if err := createSeatRound(x, task.ID, seats, jobs, orders); err == nil {
		t.Fatal("round over a conflicting seat job succeeded")
	}
	requireSeatSnapshot(t, "failed create", takeSeatSnapshot(t, x, task.ID), before)
	if _, err := x.Backend.GetWorkOrder(x.Context, orders[0].ID); err == nil {
		t.Fatal("failed create persisted its first seat order")
	}
	// Seat metadata that does not match the order count is refused too.
	if err := createSeatRound(x, task.ID, seats, jobs[:1], orders[:1]); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
		t.Fatalf("mismatched seat metadata error=%v", err)
	}
	requireSeatSnapshot(t, "mismatched create", takeSeatSnapshot(t, x, task.ID), before)
}

func runSeatWorkspaceIsolation(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	foreign := x.Workspace + "-seats-" + core.NewTaskID()[:6]
	foreignCtx := store.WithWorkspace(x.Context, foreign)
	_, err := x.Backend.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	records, err := x.Backend.ListReviewRoundSeats(foreignCtx, task.ID)
	requireOK(t, err)
	if len(records) != 0 {
		t.Fatalf("foreign workspace read records %+v", records)
	}
	home, err := x.Backend.ListReviewRoundSeats(x.Context, task.ID)
	requireOK(t, err)
	if len(home) != 1 {
		t.Fatalf("home workspace records=%+v", home)
	}
}
