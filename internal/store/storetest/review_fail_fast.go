package storetest

// Fail-fast review settlement conformance (task 261022-15e673; confirmed
// req-review-gates-evidence v13 AC-2.1 opt-in exception and AC-10.1 cancelled
// seats; component-work-orders). Every case runs unchanged on memory,
// PostgreSQL, and SingleStore. Case IDs cite tests/cases/fail-fast.cases.json.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runReviewFailFast(t *testing.T, x Fixture) {
	t.Run("DisabledWaitsForEverySeat", func(t *testing.T) { runFailFastDisabled(t, x) })
	t.Run("PolicyPinnedInTaskContract", func(t *testing.T) { runFailFastPinned(t, x) })
	t.Run("FirstChangesRequestedSettles", func(t *testing.T) { runFailFastSettles(t, x) })
	t.Run("SettlementScopedToRound", func(t *testing.T) { runFailFastRoundScoped(t, x) })
	t.Run("ApprovalWaitsForEverySeat", func(t *testing.T) { runFailFastApprovalWaits(t, x) })
	t.Run("LateVerdictRejected", func(t *testing.T) { runFailFastLateVerdict(t, x) })
	t.Run("LateClaimRejected", func(t *testing.T) { runFailFastLateClaim(t, x) })
	t.Run("StaleHeadRejected", func(t *testing.T) { runFailFastStaleHead(t, x) })
	t.Run("DuplicateDeliveryAndRecovery", func(t *testing.T) { runFailFastDuplicateAndRecovery(t, x) })
	t.Run("ConcurrentVerdicts", func(t *testing.T) { runFailFastConcurrent(t, x) })
	t.Run("SameOutcomeAsDisabledPolicy", func(t *testing.T) { runFailFastSameOutcome(t, x) })
	t.Run("NextRoundKeepsPanelSize", func(t *testing.T) { runFailFastNextRound(t, x) })
	t.Run("SettledRoundRefusesAdditions", func(t *testing.T) { runFailFastAdditionRefused(t, x) })
	t.Run("SummaryEligibility", func(t *testing.T) { runFailFastSummary(t, x) })
}

const ffWorker = "ff-worker"

// ffTask is a running review task whose frozen policy has seats seats and
// the given fail-fast value.
func ffTask(t *testing.T, x Fixture, seats int, failFast bool) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: seatFixtureHead, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.ExecutionSetup{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats), FailFast: failFast}, ExecutionSettings: config.ContextualExecutionSettings{Review: config.ReviewExecutionSettings{TimeoutText: "1h"}}}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	return stored
}

// ffRound opens review round 1 through the seat resolver and returns its
// orders in seat order.
func ffRound(t *testing.T, x Fixture, seats int, failFast bool) (core.Task, []core.WorkOrder) {
	t.Helper()
	task := ffTask(t, x, seats, failFast)
	resolution := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, resolution.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, resolution, jobs, orders))
	return task, orders
}

// ffClaim claims a seat as a fixture worker and returns its session.
func ffClaim(t *testing.T, x Fixture, order core.WorkOrder) string {
	t.Helper()
	session := order.ID + "-session"
	_, err := ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{WorkerID: ffWorker, ClaimantID: ffWorker, SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	return session
}

func ffDecision(order core.WorkOrder, verdict, session string) core.ReviewDecision {
	decision := core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: order.ReviewRound, ReviewSeat: order.ReviewSeat,
		ClaimSession: session, Verdict: verdict, ReasonCode: "verified", Summary: fmt.Sprintf("seat %d", order.ReviewSeat),
		HeadSHA: seatFixtureHead, ReviewedCommitSHA: seatFixtureHead, PolicyVersion: 1, MaxBounces: 3}
	if verdict == "changes_requested" {
		decision.ReasonCode, decision.Feedback = "defect", fmt.Sprintf("seat %d defect", order.ReviewSeat)
	}
	return decision
}

func ffAccept(x Fixture, decision core.ReviewDecision) error {
	return taskops.New(x.Backend).AcceptReviewDecision(x.Context, decision)
}

// ffVerdict claims a seat and submits its verdict through the claimed path.
func ffVerdict(t *testing.T, x Fixture, order core.WorkOrder, verdict string) core.ReviewDecision {
	t.Helper()
	decision := ffDecision(order, verdict, ffClaim(t, x, order))
	requireOK(t, ffAccept(x, decision))
	return decision
}

type ffState struct {
	orders []string
	events int
}

func ffSnapshot(t *testing.T, x Fixture, taskID string) ffState {
	t.Helper()
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	state := ffState{events: len(events)}
	for _, order := range orders {
		state.orders = append(state.orders, fmt.Sprintf("%s/%s/%s", order.ID, order.State, order.SessionID))
	}
	sort.Strings(state.orders)
	return state
}

func ffRequireUnchanged(t *testing.T, label string, got, want ffState) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s changed persisted state: got %+v, want %+v", label, got, want)
	}
}

func ffKinds(t *testing.T, x Fixture, taskID string) map[string]int {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	kinds := map[string]int{}
	for _, event := range events {
		kinds[event.Kind]++
	}
	return kinds
}

type ffSettlement struct {
	Verdict        string `json:"verdict"`
	Feedback       string `json:"feedback"`
	Settlement     string `json:"settlement"`
	CancelledSeats []int  `json:"cancelled_seats"`
	Reviews        []struct {
		ReviewSeat int    `json:"review_seat"`
		Verdict    string `json:"verdict"`
	} `json:"reviews"`
}

func ffRoundCompleted(t *testing.T, x Fixture, taskID string) (ffSettlement, int) {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	var settlement ffSettlement
	count := 0
	for _, event := range events {
		if event.Kind != "review.round_completed" {
			continue
		}
		count++
		requireOK(t, json.Unmarshal(event.Payload, &settlement))
	}
	return settlement, count
}

func ffSeatVerdicts(settlement ffSettlement) map[int]string {
	verdicts := map[int]string{}
	for _, review := range settlement.Reviews {
		verdicts[review.ReviewSeat] = review.Verdict
	}
	return verdicts
}

func ffOrder(t *testing.T, x Fixture, id string) core.WorkOrder {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, id)
	requireOK(t, err)
	return order
}

// case: FF-001
// case: FF-002
// case: FF-019
func runFailFastDisabled(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, false)
	if task.SetupContract.Review.FailFast {
		t.Fatal("fail_fast defaulted on")
	}
	ffVerdict(t, x, orders[0], "changes_requested")
	if _, count := ffRoundCompleted(t, x, task.ID); count != 0 {
		t.Fatalf("disabled round settled after one of three verdicts (%d settlements)", count)
	}
	for _, order := range orders[1:] {
		if got := ffOrder(t, x, order.ID); got.State == core.WorkOrderCancelled {
			t.Fatalf("disabled round cancelled sibling seat %d", got.ReviewSeat)
		}
	}
	ffVerdict(t, x, orders[1], "approve")
	ffVerdict(t, x, orders[2], "approve")
	settlement, count := ffRoundCompleted(t, x, task.ID)
	if count != 1 || settlement.Verdict != "changes_requested" || settlement.Settlement != "" || len(settlement.CancelledSeats) != 0 || len(settlement.Reviews) != 3 {
		t.Fatalf("disabled settlement=%+v count=%d", settlement, count)
	}
	if kinds := ffKinds(t, x, task.ID); kinds["pipeline.bounced"] != 1 || kinds["work_order.cancelled"] != 0 {
		t.Fatalf("disabled round bounced=%d cancelled=%d", kinds["pipeline.bounced"], kinds["work_order.cancelled"])
	}
}

// case: FF-003
func runFailFastPinned(t *testing.T, x Fixture) {
	// The workspace setting is the opposite of each task's frozen value while
	// the verdicts land; only the frozen value decides settlement.
	original := x.Config.Review.FailFast
	t.Cleanup(func() { x.Config.Review.FailFast = original })
	for _, frozen := range []bool{true, false} {
		task, orders := ffRound(t, x, 3, frozen)
		x.Config.Review.FailFast = !frozen
		ffVerdict(t, x, orders[0], "changes_requested")
		_, settled := ffRoundCompleted(t, x, task.ID)
		cancelled := ffOrder(t, x, orders[1].ID).State == core.WorkOrderCancelled
		if (settled == 1) != frozen || cancelled != frozen {
			t.Fatalf("frozen fail_fast=%t with workspace %t: settlements=%d sibling cancelled=%t", frozen, !frozen, settled, cancelled)
		}
	}
}

// case: FF-004
// case: FF-005
// case: FF-006
func runFailFastSettles(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 4, true)
	ffVerdict(t, x, orders[0], "approve")
	claimedSession := ffClaim(t, x, orders[1])
	settledBy := ffVerdict(t, x, orders[3], "changes_requested")

	settlement, count := ffRoundCompleted(t, x, task.ID)
	if count != 1 || settlement.Verdict != "changes_requested" || settlement.Settlement != store.ReviewFailFastSettlement {
		t.Fatalf("settlement=%+v count=%d", settlement, count)
	}
	if verdicts := ffSeatVerdicts(settlement); !reflect.DeepEqual(verdicts, map[int]string{1: "approve", 4: "changes_requested"}) {
		t.Fatalf("settled reviews=%v, want the accepted approve and changes-requested verdicts only", verdicts)
	}
	if !reflect.DeepEqual(settlement.CancelledSeats, []int{2, 3}) || !strings.Contains(settlement.Feedback, "seat 4 defect") {
		t.Fatalf("cancelled seats=%v feedback=%q", settlement.CancelledSeats, settlement.Feedback)
	}
	for _, order := range orders[1:3] {
		got := ffOrder(t, x, order.ID)
		if got.State != core.WorkOrderCancelled || got.Claimable || got.SessionID != "" || got.AttemptID != "" || !got.RetrySuppressed || got.RetrySuppressionReason != core.ReviewRoundSettledSuppressionReason {
			t.Fatalf("seat %d after settlement=%+v", got.ReviewSeat, got)
		}
	}
	if got := ffOrder(t, x, orders[1].ID); got.LastAttemptOutcome != core.WorkOrderOutcomeCancelled {
		t.Fatalf("claimed seat outcome=%q, want cancelled (session %s)", got.LastAttemptOutcome, claimedSession)
	}
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	cancelled := 0
	for _, event := range events {
		if event.Kind != "work_order.cancelled" {
			continue
		}
		cancelled++
		var payload struct {
			Reason    string `json:"reason"`
			SettledBy string `json:"settled_by_work_order_id"`
			Round     int    `json:"review_round"`
		}
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Reason != core.ReviewRoundSettledSuppressionReason || payload.SettledBy != settledBy.ReviewWorkOrderID || payload.Round != 1 || event.ActorRole != core.ActorAgent {
			t.Fatalf("cancellation event=%s actor=%s", event.Payload, event.ActorRole)
		}
	}
	kinds := ffKinds(t, x, task.ID)
	if cancelled != 2 || kinds["pipeline.bounced"] != 1 || kinds["review.accepted"] != 2 {
		t.Fatalf("cancelled=%d bounced=%d accepted=%d", cancelled, kinds["pipeline.bounced"], kinds["review.accepted"])
	}
	current, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if current.State == core.TaskRunning || current.State == core.TaskApproved {
		t.Fatalf("task state after fail-fast bounce=%s", current.State)
	}
}

// case: FF-004
// runFailFastRoundScoped proves settlement retires only the settled round's
// unfinished siblings: another round's live seats and the deciding seat itself
// (on the unclaimed dispatcher path, where it is still open) keep their state.
// Eleven seats make seat IDs sort differently from seat numbers, so the
// recorded cancelled seats must still be in seat order.
func runFailFastRoundScoped(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 11, true)
	other := resolveSeats(t, x, task.ID, 2)
	jobs, otherOrders := seatMembers(task.ID, 2, other.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, other, jobs, otherOrders))
	requireOK(t, ffAccept(x, ffDecision(orders[0], "changes_requested", "")))
	want := map[string]core.WorkOrderState{orders[0].ID: core.WorkOrderQueued}
	wantSeats := []int{}
	for _, order := range orders[1:] {
		want[order.ID] = core.WorkOrderCancelled
		wantSeats = append(wantSeats, order.ReviewSeat)
	}
	for _, order := range otherOrders {
		want[order.ID] = core.WorkOrderQueued
	}
	for id, state := range want {
		got, err := x.Backend.GetWorkOrder(x.Context, id)
		requireOK(t, err)
		if got.State != state {
			t.Fatalf("seat %s state=%s after round 1 fail-fast settlement, want %s", id, got.State, state)
		}
	}
	if settlement, count := ffRoundCompleted(t, x, task.ID); count != 1 || !reflect.DeepEqual(settlement.CancelledSeats, wantSeats) {
		t.Fatalf("settlements=%d cancelled_seats=%v, want %v", count, settlement.CancelledSeats, wantSeats)
	}
}

// case: FF-013
func runFailFastApprovalWaits(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	ffVerdict(t, x, orders[0], "approve")
	ffVerdict(t, x, orders[1], "approve")
	if _, count := ffRoundCompleted(t, x, task.ID); count != 0 {
		t.Fatal("fail-fast round settled on approvals before every seat answered")
	}
	ffVerdict(t, x, orders[2], "approve")
	settlement, count := ffRoundCompleted(t, x, task.ID)
	if count != 1 || settlement.Verdict != "approve" || settlement.Settlement != "" || len(settlement.Reviews) != 3 {
		t.Fatalf("approval settlement=%+v count=%d", settlement, count)
	}
}

// case: FF-007
func runFailFastLateVerdict(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	claimed := ffClaim(t, x, orders[1])
	ffVerdict(t, x, orders[0], "changes_requested")
	before := ffSnapshot(t, x, task.ID)
	for _, late := range []core.ReviewDecision{
		ffDecision(orders[1], "approve", claimed),
		ffDecision(orders[1], "changes_requested", claimed),
		ffDecision(orders[2], "approve", ""),
	} {
		if err := ffAccept(x, late); !errors.Is(err, store.ErrReviewRoundSettled) {
			t.Fatalf("late %s from seat %d err=%v, want %v", late.Verdict, late.ReviewSeat, err, store.ErrReviewRoundSettled)
		}
	}
	ffRequireUnchanged(t, "late verdicts", ffSnapshot(t, x, task.ID), before)
	if settlement, count := ffRoundCompleted(t, x, task.ID); count != 1 || len(settlement.Reviews) != 1 {
		t.Fatalf("late verdicts altered settlement %+v count=%d", settlement, count)
	}
}

// case: FF-008
func runFailFastLateClaim(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	claimed := ffClaim(t, x, orders[1])
	ffVerdict(t, x, orders[0], "changes_requested")
	before := ffSnapshot(t, x, task.ID)
	if _, err := RenewWorkerClaim(x.Context, x.Backend, orders[1].ID, ffWorker, claimed, time.Hour); err == nil {
		t.Fatal("renewal of a seat cancelled by settlement succeeded")
	}
	if _, err := ClaimWorkOrder(x.Context, x.Backend, orders[2].ID, core.WorkOrderClaim{WorkerID: ffWorker, ClaimantID: ffWorker, SessionID: "late", ClientToken: "late-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err == nil {
		t.Fatal("claim of a seat cancelled by settlement succeeded")
	}
	ffRequireUnchanged(t, "late claims", ffSnapshot(t, x, task.ID), before)
}

// case: FF-009
func runFailFastStaleHead(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 2, true)
	stale := ffDecision(orders[0], "changes_requested", ffClaim(t, x, orders[0]))
	stale.ReviewedCommitSHA = "moved-head"
	before := ffSnapshot(t, x, task.ID)
	if err := ffAccept(x, stale); !errors.Is(err, store.ErrReviewHeadStale) {
		t.Fatalf("stale-head verdict err=%v, want %v", err, store.ErrReviewHeadStale)
	}
	ffRequireUnchanged(t, "stale-head verdict", ffSnapshot(t, x, task.ID), before)
	if kinds := ffKinds(t, x, task.ID); kinds["review.accepted"] != 0 || kinds["review.round_completed"] != 0 {
		t.Fatalf("stale-head verdict recorded %v", kinds)
	}

	// Absent head evidence is not stale evidence: the verdict settles the
	// round exactly as a matching head would.
	for _, clear := range []func(*core.ReviewDecision){
		func(d *core.ReviewDecision) { d.ReviewedCommitSHA = "" },
		func(d *core.ReviewDecision) { d.HeadSHA = "" },
	} {
		task, orders := ffRound(t, x, 2, true)
		decision := ffDecision(orders[0], "changes_requested", ffClaim(t, x, orders[0]))
		clear(&decision)
		requireOK(t, ffAccept(x, decision))
		if settlement, count := ffRoundCompleted(t, x, task.ID); count != 1 || settlement.Settlement != store.ReviewFailFastSettlement {
			t.Fatalf("verdict without head evidence did not settle: %+v count=%d", settlement, count)
		}
	}
}

// case: FF-010
// case: FF-012
func runFailFastDuplicateAndRecovery(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	decision := ffVerdict(t, x, orders[0], "changes_requested")
	before := ffSnapshot(t, x, task.ID)
	requireOK(t, ffAccept(x, decision))
	unclaimed := decision
	unclaimed.ClaimSession = ""
	requireOK(t, ffAccept(x, unclaimed))
	ffRequireUnchanged(t, "duplicate delivery", ffSnapshot(t, x, task.ID), before)

	// A restarted recovery sweep reads the settled round and finds nothing to
	// recover, retry, or re-mint.
	current, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	all, err := x.Backend.ListTaskWorkOrders(x.Context, task.ID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	if state := store.InterruptedReviewRecoveryNeeded(current, all, events); state != nil {
		t.Fatalf("interrupted recovery after settlement=%+v", state)
	}
	if state := store.ReviewRecoveryNeeded(all, events); state != nil {
		t.Fatalf("round retry after settlement=%+v", state)
	}
	if kinds := ffKinds(t, x, task.ID); kinds["pipeline.bounced"] != 1 || kinds["review.round_completed"] != 1 {
		t.Fatalf("after replay bounced=%d settlements=%d", kinds["pipeline.bounced"], kinds["review.round_completed"])
	}
}

// case: FF-011
func runFailFastConcurrent(t *testing.T, x Fixture) {
	for _, verdicts := range [][]string{
		{"changes_requested", "changes_requested", "approve"},
		{"changes_requested", "approve", "approve"},
	} {
		task, orders := ffRound(t, x, 3, true)
		decisions := make([]core.ReviewDecision, len(orders))
		for i, order := range orders {
			decisions[i] = ffDecision(order, verdicts[i], ffClaim(t, x, order))
		}
		errs := make([]error, len(decisions))
		var start, done sync.WaitGroup
		start.Add(1)
		for i := range decisions {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				errs[i] = ffAccept(x, decisions[i])
			}()
		}
		start.Done()
		done.Wait()

		settlement, count := ffRoundCompleted(t, x, task.ID)
		kinds := ffKinds(t, x, task.ID)
		if count != 1 || settlement.Verdict != "changes_requested" || kinds["pipeline.bounced"] != 1 {
			t.Fatalf("%v: settlements=%d verdict=%q bounced=%d errs=%v", verdicts, count, settlement.Verdict, kinds["pipeline.bounced"], errs)
		}
		accepted := ffSeatVerdicts(settlement)
		for i, err := range errs {
			seat := orders[i].ReviewSeat
			order := ffOrder(t, x, orders[i].ID)
			switch {
			case err == nil:
				if accepted[seat] != verdicts[i] {
					t.Fatalf("%v: accepted seat %d missing from settlement %v", verdicts, seat, accepted)
				}
			case errors.Is(err, store.ErrReviewRoundSettled):
				if _, ok := accepted[seat]; ok || order.State != core.WorkOrderCancelled {
					t.Fatalf("%v: rejected seat %d state=%s in settlement=%t", verdicts, seat, order.State, ok)
				}
			default:
				t.Fatalf("%v: seat %d err=%v", verdicts, seat, err)
			}
		}
		if kinds["review.accepted"] != len(accepted) {
			t.Fatalf("%v: accepted events=%d, settled reviews=%d", verdicts, kinds["review.accepted"], len(accepted))
		}
	}
}

// ffOutcome is the task-visible result of a settled round.
type ffOutcome struct {
	State     core.TaskState
	NextStage core.Stage
	GateKind  core.GateKind
	Verdict   string
	Bounced   int
	Limit     int
}

func ffTaskOutcome(t *testing.T, x Fixture, taskID string) ffOutcome {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	settlement, _ := ffRoundCompleted(t, x, taskID)
	kinds := ffKinds(t, x, taskID)
	return ffOutcome{State: task.State, NextStage: task.NextStage, GateKind: task.GateKind, Verdict: settlement.Verdict, Bounced: kinds["pipeline.bounced"], Limit: kinds["pipeline.bounce_limit"]}
}

// case: FF-014
// case: FF-017
func runFailFastSameOutcome(t *testing.T, x Fixture) {
	for _, maxBounces := range []int{3, 1} {
		outcome := func(failFast bool, verdicts ...string) ffOutcome {
			task, orders := ffRound(t, x, len(verdicts), failFast)
			for i, verdict := range verdicts {
				if failFast && ffOrder(t, x, orders[i].ID).State == core.WorkOrderCancelled {
					// The settlement retired this seat; a late verdict is
					// LateVerdictRejected's concern.
					continue
				}
				decision := ffDecision(orders[i], verdict, ffClaim(t, x, orders[i]))
				decision.MaxBounces = maxBounces
				requireOK(t, ffAccept(x, decision))
			}
			return ffTaskOutcome(t, x, task.ID)
		}
		disabled := outcome(false, "changes_requested", "approve", "approve")
		enabled := outcome(true, "changes_requested", "approve", "approve")
		if enabled != disabled || enabled.Bounced != 1 {
			t.Fatalf("max_bounces=%d changes-requested outcome enabled=%+v disabled=%+v", maxBounces, enabled, disabled)
		}
		if maxBounces == 1 && enabled.Limit != 1 {
			t.Fatalf("bounce cap not applied to fail-fast settlement: %+v", enabled)
		}
		approvedDisabled := outcome(false, "approve", "approve")
		approvedEnabled := outcome(true, "approve", "approve")
		if approvedEnabled != approvedDisabled || approvedEnabled.Bounced != 0 {
			t.Fatalf("max_bounces=%d approval outcome enabled=%+v disabled=%+v", maxBounces, approvedEnabled, approvedDisabled)
		}
	}
}

// case: FF-015
func runFailFastNextRound(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 4, true)
	ffVerdict(t, x, orders[0], "changes_requested")
	next := resolveSeats(t, x, task.ID, 2)
	if next.SeatCount != 4 || next.Source != store.ReviewSeatSourceCarried {
		t.Fatalf("next round after fail-fast=%+v, want 4 carried seats", next)
	}
	// The pinned policy settles the next round too: an earlier round's
	// settlement must not make the new round's verdicts look late.
	jobs, nextOrders := seatMembers(task.ID, 2, next.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, next, jobs, nextOrders))
	ffVerdict(t, x, nextOrders[1], "changes_requested")
	if settlement, count := ffRoundCompleted(t, x, task.ID); count != 2 || settlement.Settlement != store.ReviewFailFastSettlement || !reflect.DeepEqual(settlement.CancelledSeats, []int{1, 3, 4}) {
		t.Fatalf("round 2 settlements=%d last=%+v, want fail-fast settlement cancelling seats 1, 3, 4", count, settlement)
	}
	disabled, disabledOrders := ffRound(t, x, 4, false)
	for i, verdict := range []string{"changes_requested", "approve", "approve", "approve"} {
		ffVerdict(t, x, disabledOrders[i], verdict)
	}
	if want := resolveSeats(t, x, disabled.ID, 2); want.SeatCount != next.SeatCount || want.Source != next.Source {
		t.Fatalf("fail-fast next round=%+v, disabled next round=%+v", next, want)
	}
}

// case: FF-016
func runFailFastAdditionRefused(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	added, err := rs5Add(x, rs5Request(task.ID, "ff-open-"+task.ID, 1))
	requireOK(t, err)
	if added.SeatCount != 4 {
		t.Fatalf("open fail-fast round addition=%+v", added)
	}
	ffVerdict(t, x, orders[0], "changes_requested")
	settlement, _ := ffRoundCompleted(t, x, task.ID)
	if !reflect.DeepEqual(settlement.CancelledSeats, []int{2, 3, 4}) {
		t.Fatalf("added seat not retired by settlement: %v", settlement.CancelledSeats)
	}
	before := ffSnapshot(t, x, task.ID)
	if _, err := rs5Add(x, rs5Request(task.ID, "ff-settled-"+task.ID, 1)); !errors.Is(err, store.ErrReviewRoundSettled) {
		t.Fatalf("addition to settled round err=%v", err)
	}
	ffRequireUnchanged(t, "settled addition", ffSnapshot(t, x, task.ID), before)
}

// case: FF-018
func runFailFastSummary(t *testing.T, x Fixture) {
	task, orders := ffRound(t, x, 3, true)
	ffVerdict(t, x, orders[0], "approve")
	// Before the changes-requested verdict the fail-fast round is an ordinary
	// unsettled round: a seat without a verdict still refuses a summary.
	requireSummaryRefusal(t, x, "unsettled", summaryActorRequest(task, 1, "ff-unsettled"), store.ErrReviewSummaryRoundNotSettled)
	ffVerdict(t, x, orders[2], "changes_requested")
	requireSummaryRefusal(t, x, "uncovered", summaryActorRequest(task, 1, "ff-uncovered"), store.ErrReviewSummaryInvalid)
	requireSummaryRefusal(t, x, "cancelled seat", summaryActorRequest(task, 1, "ff-cancelled", blockingFinding(2, 3)), store.ErrReviewSummaryInvalid)
	before := ffSnapshot(t, x, task.ID)
	kinds := ffKinds(t, x, task.ID)
	recordSummary(t, x, summaryActorRequest(task, 1, "ff-summary", blockingFinding(3)))
	after := ffSnapshot(t, x, task.ID)
	if !reflect.DeepEqual(after.orders, before.orders) || after.events != before.events+1 {
		t.Fatalf("summary changed orders or wrote more than its event: %+v -> %+v", before, after)
	}
	if got := ffKinds(t, x, task.ID); got["pipeline.bounced"] != kinds["pipeline.bounced"] || got["review.accepted"] != kinds["review.accepted"] {
		t.Fatalf("summary changed verdicts or bounces: %v -> %v", kinds, got)
	}
}
