package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runReviewSeatAdditionConformance proves feature-review-seat-minimum RS-5 on
// every backend: an operator addition atomically creates seats k+1 through
// k+add with the round's metadata, updates or inserts the round record, writes
// one ledger row and one review.seats_added event, refuses a task without an
// open round and a settled round, replays by workspace-scoped request ID, and
// serializes with the final verdict (req-review-gates-evidence AC-2.5, AC-2.6).
func runReviewSeatAdditionConformance(t *testing.T, x Fixture) {
	t.Run("RS5AddAtomicRecordLedgerEvent", func(t *testing.T) { runRS5AddAtomic(t, x) })
	t.Run("RS5LegacyCarried", func(t *testing.T) { runRS5LegacyCarried(t, x) })
	t.Run("RS5NoRoundRefused", func(t *testing.T) { runRS5NoRound(t, x) })
	t.Run("RS5SettledRefused", func(t *testing.T) { runRS5Settled(t, x) })
	t.Run("RS5RequiresUserActorAndLease", func(t *testing.T) { runRS5Authority(t, x) })
	t.Run("RS5ReplayAfterSettlement", func(t *testing.T) { runRS5ReplayAfterSettlement(t, x) })
	t.Run("RS5ReplayAfterLaterRound", func(t *testing.T) { runRS5ReplayAfterLaterRound(t, x) })
	t.Run("RS5ReplayConflict", func(t *testing.T) { runRS5ReplayConflict(t, x) })
	t.Run("RS5ConcurrentReplay", func(t *testing.T) { runRS5ConcurrentReplay(t, x) })
	t.Run("RS5ConcurrentAdditions", func(t *testing.T) { runRS5ConcurrentAdditions(t, x) })
	t.Run("RS5WorkspaceIsolation", func(t *testing.T) { runRS5WorkspaceIsolation(t, x) })
	t.Run("RS5FailedAdditionRollsBack", func(t *testing.T) { runRS5FailedAddition(t, x) })
	t.Run("RS5AdditionBeforeFinalVerdict", func(t *testing.T) { runRS5AdditionBeforeVerdict(t, x) })
	t.Run("RS5FinalVerdictBeforeAddition", func(t *testing.T) { runRS5VerdictBeforeAddition(t, x) })
	t.Run("RS5AdditionVerdictRace", func(t *testing.T) { runRS5AdditionVerdictRace(t, x) })
}

const rs5Operator = "user:rs5-operator"

func rs5OperatorContext(x Fixture) Fixture {
	x.Context = store.WithActor(x.Context, store.Actor{ID: rs5Operator, Role: core.ActorUser})
	return x
}

func rs5Add(x Fixture, request store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	ctx := rs5OperatorContext(x).Context
	return taskops.ExecuteWorkOrder(ctx, x.Backend, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
		return x.Backend.AddReviewSeatsCommand(ctx, lease, request)
	})
}

func rs5Request(taskID, requestID string, add int) store.ReviewSeatAdditionRequest {
	return store.ReviewSeatAdditionRequest{TaskID: taskID, RequestID: requestID, Reason: "second opinion on the migration", Add: add, WorkspaceReviewSeats: 1}
}

// rs5Round opens round 1 of a frozen-minimum task with count seats through
// the resolver and returns the task and the round's orders.
func rs5Round(t *testing.T, x Fixture, count int) (core.Task, []core.WorkOrder) {
	t.Helper()
	task := seatTask(t, x, count)
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	return task, orders
}

func rs5Verdict(x Fixture, order core.WorkOrder, verdict string) error {
	return taskops.New(x.Backend).AcceptReviewDecision(x.Context, core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID,
		ReviewRound: order.ReviewRound, ReviewSeat: order.ReviewSeat, Verdict: verdict, ReasonCode: "verified", Summary: "rs5 verdict", Feedback: "rs5 feedback", MaxBounces: 3})
}

// rs5State is the comparable persisted state of one task, including the
// record contents a refusal must leave unchanged.
type rs5State struct {
	orders  []string
	events  int
	records []store.ReviewRoundSeats
}

func rs5Snapshot(t *testing.T, x Fixture, taskID string) rs5State {
	t.Helper()
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	records, err := x.Backend.ListReviewRoundSeats(x.Context, taskID)
	requireOK(t, err)
	state := rs5State{events: len(events), records: records}
	for _, order := range orders {
		state.orders = append(state.orders, fmt.Sprintf("%s/%s", order.ID, order.State))
	}
	return state
}

func rs5RequireUnchanged(t *testing.T, label string, got, want rs5State) {
	t.Helper()
	if len(got.orders) != len(want.orders) || got.events != want.events || !reflect.DeepEqual(got.records, want.records) {
		t.Fatalf("%s changed persisted state: got %+v, want %+v", label, got, want)
	}
}

func rs5SeatsAdded(t *testing.T, x Fixture, taskID string) []core.Event {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	var added []core.Event
	for _, event := range events {
		if event.Kind == "review.seats_added" {
			added = append(added, event)
		}
	}
	return added
}

func rs5RoundCompleted(t *testing.T, x Fixture, taskID string, round int) (map[string]any, bool) {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	for _, event := range events {
		if event.Kind != "review.round_completed" {
			continue
		}
		var payload map[string]any
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if number, _ := payload["review_round"].(float64); int(number) == round {
			return payload, true
		}
	}
	return nil, false
}

func rs5RequireResult(t *testing.T, got store.ReviewSeatAdditionResult, want store.ReviewSeatAdditionResult) {
	t.Helper()
	if got != want {
		t.Fatalf("addition result=%+v, want %+v", got, want)
	}
}

func runRS5AddAtomic(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 2)
	before := rs5Snapshot(t, x, task.ID)
	request := rs5Request(task.ID, "rs5-atomic-"+task.ID, 2)
	result, err := rs5Add(x, request)
	requireOK(t, err)
	rs5RequireResult(t, result, store.ReviewSeatAdditionResult{RequestID: request.RequestID, TaskID: task.ID, ReviewRound: 1, PriorSeatCount: 2, SeatCount: 4})
	orders := reviewOrdersInRound(t, x, task.ID, 1)
	if len(orders) != 4 {
		t.Fatalf("round 1 orders=%+v", orders)
	}
	for _, order := range orders {
		if order.ReviewSeat <= 2 {
			continue
		}
		wantID := fmt.Sprintf("%s-review-1-seat-%d", task.ID, order.ReviewSeat)
		if order.ID != wantID || order.JobID != wantID || order.State != core.WorkOrderQueued || order.HeadSHA != round[0].HeadSHA || order.BaselineSHA != round[0].BaselineSHA ||
			order.ReviewKind != round[0].ReviewKind || order.RequiredModel != "" || order.RequiredHarness != "" || order.QueueDeadline.IsZero() {
			t.Fatalf("added seat=%+v", order)
		}
	}
	// The record keeps its source and minimum; only seat_count moves.
	requireSeatRecord(t, x, task.ID, 1, 4, store.ReviewSeatSourceMinimum, 2)
	after := rs5Snapshot(t, x, task.ID)
	if after.events != before.events+5 {
		t.Fatalf("events grew %d -> %d, want two job.created, two work_order.created, and one review.seats_added", before.events, after.events)
	}
	added := rs5SeatsAdded(t, x, task.ID)
	if len(added) != 1 || added[0].ActorID != rs5Operator {
		t.Fatalf("review.seats_added events=%+v", added)
	}
	var payload map[string]any
	requireOK(t, json.Unmarshal(added[0].Payload, &payload))
	want := map[string]any{"request_id": request.RequestID, "workspace_id": x.Workspace, "task_id": task.ID, "review_round": float64(1), "prior_seat_count": float64(2),
		"seat_count": float64(4), "reason": request.Reason, "actor": rs5Operator,
		"work_order_ids": []any{fmt.Sprintf("%s-review-1-seat-3", task.ID), fmt.Sprintf("%s-review-1-seat-4", task.ID)}}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("review.seats_added payload=%+v, want %+v", payload, want)
	}
	// The ledger row reconstructs the result on replay without writes.
	replay, err := rs5Add(x, request)
	requireOK(t, err)
	rs5RequireResult(t, replay, result)
	rs5RequireUnchanged(t, "replay", rs5Snapshot(t, x, task.ID), after)
}

func runRS5LegacyCarried(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	seatLegacyRound(t, x, task, 3)
	result, err := rs5Add(x, rs5Request(task.ID, "rs5-legacy-"+task.ID, 1))
	requireOK(t, err)
	rs5RequireResult(t, result, store.ReviewSeatAdditionResult{RequestID: "rs5-legacy-" + task.ID, TaskID: task.ID, ReviewRound: 1, PriorSeatCount: 3, SeatCount: 4})
	record := requireSeatRecord(t, x, task.ID, 1, 4, store.ReviewSeatSourceCarried, 2)
	if record.ImplementationWorkOrderID != "" {
		t.Fatalf("legacy carried record=%+v", record)
	}
	// The next round carries the count, additions included (RS-6).
	seats := resolveSeats(t, x, task.ID, 2)
	if seats.SeatCount != 4 || seats.Source != store.ReviewSeatSourceCarried {
		t.Fatalf("next round after a legacy addition=%+v", seats)
	}
}

func runRS5NoRound(t *testing.T, x Fixture) {
	task := seatTask(t, x, 2)
	before := rs5Snapshot(t, x, task.ID)
	request := rs5Request(task.ID, "rs5-no-round-"+task.ID, 1)
	if _, err := rs5Add(x, request); !errors.Is(err, store.ErrReviewRoundNotOpen) {
		t.Fatalf("no-round addition error=%v", err)
	}
	rs5RequireUnchanged(t, "no-round refusal", rs5Snapshot(t, x, task.ID), before)
	// No ledger row was written: the same request ID adds once a round opens.
	seats := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	result, err := rs5Add(x, request)
	requireOK(t, err)
	if result.PriorSeatCount != 2 || result.SeatCount != 3 {
		t.Fatalf("addition after refusal=%+v", result)
	}
	if _, err := rs5Add(x, rs5Request("missing-"+task.ID, "rs5-missing-"+task.ID, 1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task error=%v", err)
	}
}

func runRS5Settled(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 1)
	requireOK(t, rs5Verdict(x, round[0], "approve"))
	if _, settled := rs5RoundCompleted(t, x, task.ID, 1); !settled {
		t.Fatal("one approving seat did not settle the round")
	}
	before := rs5Snapshot(t, x, task.ID)
	if _, err := rs5Add(x, rs5Request(task.ID, "rs5-settled-"+task.ID, 1)); !errors.Is(err, store.ErrReviewRoundSettled) {
		t.Fatalf("settled addition error=%v", err)
	}
	rs5RequireUnchanged(t, "settled refusal", rs5Snapshot(t, x, task.ID), before)
}

func runRS5Authority(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	before := rs5Snapshot(t, x, task.ID)
	request := rs5Request(task.ID, "rs5-authority-"+task.ID, 1)
	for _, actor := range []store.Actor{{ID: "agent:rs5", Role: core.ActorAgent}, {ID: "worker:rs5", Role: core.ActorWorker}, store.SystemActor()} {
		ctx := store.WithActor(x.Context, actor)
		_, err := taskops.ExecuteWorkOrder(ctx, x.Backend, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
			return x.Backend.AddReviewSeatsCommand(ctx, lease, request)
		})
		if !errors.Is(err, store.ErrReviewSeatAdditionForbidden) {
			t.Fatalf("%s addition error=%v", actor.Role, err)
		}
	}
	operator := rs5OperatorContext(x).Context
	if _, err := x.Backend.AddReviewSeatsCommand(operator, taskops.TaskLease{}, request); err == nil {
		t.Fatal("addition without a taskops lease succeeded")
	}
	if _, err := taskops.ExecuteWorkOrder(operator, x.Backend, task.ID, core.WorkOrderCmdClaim, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
		return x.Backend.AddReviewSeatsCommand(operator, lease, request)
	}); err == nil {
		t.Fatal("addition under a claim lease succeeded")
	}
	invalid := request
	invalid.Reason = "  "
	if _, err := rs5Add(x, invalid); !errors.Is(err, store.ErrReviewSeatAdditionInvalid) {
		t.Fatalf("blank reason error=%v", err)
	}
	rs5RequireUnchanged(t, "refused authority", rs5Snapshot(t, x, task.ID), before)
}

func runRS5ReplayAfterSettlement(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 1)
	request := rs5Request(task.ID, "rs5-replay-settled-"+task.ID, 1)
	result, err := rs5Add(x, request)
	requireOK(t, err)
	for _, order := range reviewOrdersInRound(t, x, task.ID, 1) {
		requireOK(t, rs5Verdict(x, order, "approve"))
	}
	if _, settled := rs5RoundCompleted(t, x, task.ID, 1); !settled || len(round) != 1 {
		t.Fatal("round did not settle after every added verdict")
	}
	before := rs5Snapshot(t, x, task.ID)
	replay, err := rs5Add(x, request)
	requireOK(t, err)
	rs5RequireResult(t, replay, result)
	rs5RequireUnchanged(t, "replay after settlement", rs5Snapshot(t, x, task.ID), before)
}

func runRS5ReplayAfterLaterRound(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	request := rs5Request(task.ID, "rs5-replay-later-"+task.ID, 2)
	result, err := rs5Add(x, request)
	requireOK(t, err)
	for _, order := range reviewOrdersInRound(t, x, task.ID, 1) {
		requireOK(t, rs5Verdict(x, order, "changes_requested"))
	}
	if payload, settled := rs5RoundCompleted(t, x, task.ID, 1); !settled || payload["verdict"] != "changes_requested" {
		t.Fatalf("round 1 settlement=%+v", payload)
	}
	seats := resolveSeats(t, x, task.ID, 2)
	if seats.SeatCount != 3 || seats.Source != store.ReviewSeatSourceCarried {
		t.Fatalf("round 2 resolution=%+v", seats)
	}
	jobs, orders := seatMembers(task.ID, 2, seats.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, seats, jobs, orders))
	before := rs5Snapshot(t, x, task.ID)
	replay, err := rs5Add(x, request)
	requireOK(t, err)
	rs5RequireResult(t, replay, result)
	rs5RequireUnchanged(t, "replay after a later round", rs5Snapshot(t, x, task.ID), before)
	// A new request now targets the later round.
	later, err := rs5Add(x, rs5Request(task.ID, "rs5-later-round-"+task.ID, 1))
	requireOK(t, err)
	if later.ReviewRound != 2 || later.PriorSeatCount != 3 || later.SeatCount != 4 {
		t.Fatalf("later-round addition=%+v", later)
	}
}

func runRS5ReplayConflict(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	other, _ := rs5Round(t, x, 1)
	request := rs5Request(task.ID, "rs5-conflict-"+task.ID, 1)
	_, err := rs5Add(x, request)
	requireOK(t, err)
	before, otherBefore := rs5Snapshot(t, x, task.ID), rs5Snapshot(t, x, other.ID)
	changedCount, changedReason, changedTask := request, request, request
	changedCount.Add = 2
	changedReason.Reason = request.Reason + "."
	changedTask.TaskID = other.ID
	for label, changed := range map[string]store.ReviewSeatAdditionRequest{"count": changedCount, "reason": changedReason, "task": changedTask} {
		if _, err := rs5Add(x, changed); !errors.Is(err, store.ErrReviewSeatAdditionConflict) {
			t.Fatalf("changed %s error=%v", label, err)
		}
	}
	rs5RequireUnchanged(t, "conflicting replay", rs5Snapshot(t, x, task.ID), before)
	rs5RequireUnchanged(t, "conflicting replay on another task", rs5Snapshot(t, x, other.ID), otherBefore)
}

// rs5Concurrently starts every call at one barrier and returns their results.
func rs5Concurrently(n int, call func(int) (store.ReviewSeatAdditionResult, error)) ([]store.ReviewSeatAdditionResult, []error) {
	results, errs := make([]store.ReviewSeatAdditionResult, n), make([]error, n)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = call(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

func runRS5ConcurrentReplay(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 2)
	request := rs5Request(task.ID, "rs5-concurrent-replay-"+task.ID, 1)
	results, errs := rs5Concurrently(4, func(int) (store.ReviewSeatAdditionResult, error) { return rs5Add(x, request) })
	for i := range results {
		requireOK(t, errs[i])
		rs5RequireResult(t, results[i], store.ReviewSeatAdditionResult{RequestID: request.RequestID, TaskID: task.ID, ReviewRound: 1, PriorSeatCount: 2, SeatCount: 3})
	}
	if added := rs5SeatsAdded(t, x, task.ID); len(added) != 1 {
		t.Fatalf("concurrent replay wrote %d review.seats_added events", len(added))
	}
	if orders := reviewOrdersInRound(t, x, task.ID, 1); len(orders) != 3 {
		t.Fatalf("concurrent replay orders=%d", len(orders))
	}
	requireSeatRecord(t, x, task.ID, 1, 3, store.ReviewSeatSourceMinimum, 2)
}

func runRS5ConcurrentAdditions(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	results, errs := rs5Concurrently(2, func(i int) (store.ReviewSeatAdditionResult, error) {
		return rs5Add(x, rs5Request(task.ID, fmt.Sprintf("rs5-concurrent-%d-%s", i, task.ID), 1))
	})
	priors := map[int]bool{}
	for i := range results {
		requireOK(t, errs[i])
		if results[i].SeatCount != results[i].PriorSeatCount+1 {
			t.Fatalf("concurrent addition=%+v", results[i])
		}
		priors[results[i].PriorSeatCount] = true
	}
	if !priors[1] || !priors[2] {
		t.Fatalf("concurrent additions did not serialize: %+v", results)
	}
	orders := reviewOrdersInRound(t, x, task.ID, 1)
	seats := map[int]bool{}
	for _, order := range orders {
		seats[order.ReviewSeat] = true
	}
	if len(orders) != 3 || !seats[1] || !seats[2] || !seats[3] {
		t.Fatalf("concurrent addition seats=%+v", orders)
	}
	requireSeatRecord(t, x, task.ID, 1, 3, store.ReviewSeatSourceMinimum, 1)
	if added := rs5SeatsAdded(t, x, task.ID); len(added) != 2 {
		t.Fatalf("concurrent additions wrote %d events", len(added))
	}
}

func runRS5WorkspaceIsolation(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	requestID := "rs5-isolated-" + task.ID
	home, err := rs5Add(x, rs5Request(task.ID, requestID, 1))
	requireOK(t, err)
	foreign := x
	foreign.Workspace = x.Workspace + "-rs5-" + core.NewTaskID()[:6]
	foreign.Context = store.WithWorkspace(x.Context, foreign.Workspace)
	_, err = x.Backend.BootstrapWorkspaceConfig(foreign.Context, &config.Config{Workspace: foreign.Workspace, Repos: x.Config.Repos})
	requireOK(t, err)
	// The home task is invisible from the foreign workspace.
	if _, err := rs5Add(foreign, rs5Request(task.ID, "rs5-cross-"+task.ID, 1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace addition error=%v", err)
	}
	foreignTask, _ := rs5Round(t, foreign, 2)
	// The same request ID with a different task and count is independent.
	away, err := rs5Add(foreign, rs5Request(foreignTask.ID, requestID, 2))
	requireOK(t, err)
	rs5RequireResult(t, away, store.ReviewSeatAdditionResult{RequestID: requestID, TaskID: foreignTask.ID, ReviewRound: 1, PriorSeatCount: 2, SeatCount: 4})
	replay, err := rs5Add(x, rs5Request(task.ID, requestID, 1))
	requireOK(t, err)
	rs5RequireResult(t, replay, home)
}

func runRS5FailedAddition(t *testing.T, x Fixture) {
	task, _ := rs5Round(t, x, 1)
	// The second added seat's job already exists, so the addition fails after
	// its first member; nothing may persist.
	requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: fmt.Sprintf("%s-review-1-seat-3", task.ID), TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}))
	before := rs5Snapshot(t, x, task.ID)
	request := rs5Request(task.ID, "rs5-rollback-"+task.ID, 2)
	if _, err := rs5Add(x, request); err == nil {
		t.Fatal("addition over a conflicting seat job succeeded")
	}
	rs5RequireUnchanged(t, "failed addition", rs5Snapshot(t, x, task.ID), before)
	if added := rs5SeatsAdded(t, x, task.ID); len(added) != 0 {
		t.Fatalf("failed addition wrote %+v", added)
	}
	// No ledger row survived: the request ID admits a different count.
	request.Add = 1
	result, err := rs5Add(x, request)
	requireOK(t, err)
	if result.SeatCount != 2 {
		t.Fatalf("addition after rollback=%+v", result)
	}
}

func runRS5AdditionBeforeVerdict(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 2)
	requireOK(t, rs5Verdict(x, round[0], "approve"))
	_, err := rs5Add(x, rs5Request(task.ID, "rs5-before-verdict-"+task.ID, 1))
	requireOK(t, err)
	requireOK(t, rs5Verdict(x, round[1], "approve"))
	if _, settled := rs5RoundCompleted(t, x, task.ID, 1); settled {
		t.Fatal("round settled before the added seat's verdict")
	}
	added := reviewOrdersInRound(t, x, task.ID, 1)
	var third core.WorkOrder
	for _, order := range added {
		if order.ReviewSeat == 3 {
			third = order
		}
	}
	if third.ID == "" {
		t.Fatalf("added seat missing from %+v", added)
	}
	requireOK(t, rs5Verdict(x, third, "changes_requested"))
	payload, settled := rs5RoundCompleted(t, x, task.ID, 1)
	if !settled || payload["verdict"] != "changes_requested" {
		t.Fatalf("round with a changes-requested added seat settled as %+v", payload)
	}
	if reviews, _ := payload["reviews"].([]any); len(reviews) != 3 {
		t.Fatalf("settlement counted %d verdicts, want 3", len(reviews))
	}
}

func runRS5VerdictBeforeAddition(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 2)
	requireOK(t, rs5Verdict(x, round[0], "approve"))
	requireOK(t, rs5Verdict(x, round[1], "approve"))
	before := rs5Snapshot(t, x, task.ID)
	if _, err := rs5Add(x, rs5Request(task.ID, "rs5-after-verdict-"+task.ID, 1)); !errors.Is(err, store.ErrReviewRoundSettled) {
		t.Fatalf("addition after the final verdict error=%v", err)
	}
	rs5RequireUnchanged(t, "addition after settlement", rs5Snapshot(t, x, task.ID), before)
}

// runRS5AdditionVerdictRace starts an addition and the round's final verdict
// at one barrier. Only the two serial outcomes are accepted.
func runRS5AdditionVerdictRace(t *testing.T, x Fixture) {
	task, round := rs5Round(t, x, 1)
	var verdictErr error
	results, errs := rs5Concurrently(2, func(i int) (store.ReviewSeatAdditionResult, error) {
		if i == 0 {
			verdictErr = rs5Verdict(x, round[0], "approve")
			return store.ReviewSeatAdditionResult{}, nil
		}
		return rs5Add(x, rs5Request(task.ID, "rs5-race-"+task.ID, 1))
	})
	requireOK(t, verdictErr)
	orders := reviewOrdersInRound(t, x, task.ID, 1)
	_, settled := rs5RoundCompleted(t, x, task.ID, 1)
	switch {
	case errs[1] == nil:
		// Addition first: the round waits for the added seat.
		if results[1].SeatCount != 2 || len(orders) != 2 || settled {
			t.Fatalf("addition-first outcome result=%+v orders=%d settled=%t", results[1], len(orders), settled)
		}
	case errors.Is(errs[1], store.ErrReviewRoundSettled):
		// Verdict first: the round settled and the addition wrote nothing.
		if len(orders) != 1 || !settled || len(rs5SeatsAdded(t, x, task.ID)) != 0 {
			t.Fatalf("verdict-first outcome orders=%d settled=%t", len(orders), settled)
		}
	default:
		t.Fatalf("race outcome error=%v", errs[1])
	}
}
