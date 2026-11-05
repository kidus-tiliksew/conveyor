package storetest

// Distinct review seat model conformance (req-review-gates-evidence AC-2.10,
// AC-2.11; DEC-104(4); component-work-orders; component-task-lifecycle). Every
// case runs unchanged on memory, PostgreSQL, and SingleStore.

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runReviewDistinctModels(t *testing.T, x Fixture) {
	t.Run("RepeatedNameRefused", func(t *testing.T) { runDistinctModelsRefusal(t, x) })
	t.Run("BlankNameRefused", func(t *testing.T) { runDistinctModelsBlank(t, x) })
	t.Run("PolicyOffComparesNothing", func(t *testing.T) { runDistinctModelsOff(t, x) })
	t.Run("ReleasedSeatNameReusable", func(t *testing.T) { runDistinctModelsReleased(t, x) })
	t.Run("ExpiredSeatNameReusable", func(t *testing.T) { runDistinctModelsExpired(t, x) })
	t.Run("VerdictsHoldTheirNames", func(t *testing.T) { runDistinctModelsVerdicts(t, x) })
	t.Run("OperatorAddedSeat", func(t *testing.T) { runDistinctModelsAddedSeat(t, x) })
	t.Run("LaterRoundReusesName", func(t *testing.T) { runDistinctModelsLaterRound(t, x) })
	t.Run("ConcurrentSiblingClaims", func(t *testing.T) { runDistinctModelsConcurrent(t, x) })
	t.Run("PolicyOnlyChangeResolvesNoHandoff", func(t *testing.T) { runDistinctModelsPolicyOnly(t, x) })
	t.Run("ChangeAppliesToLaterClaims", func(t *testing.T) { runDistinctModelsLaterClaims(t, x) })
	t.Run("ChangeRefusedWhileSeatExecuting", func(t *testing.T) { runDistinctModelsChangeRefused(t, x) })
}

// runDistinctModelsNoClaimFailure refuses a seat through the real work-order
// service: the refusal keeps its code and records no claim failure, so it can
// never raise the claim-failure stall (component-work-orders, Claim admission).
func runDistinctModelsNoClaimFailure(t *testing.T, x Fixture, claimService ClaimServiceFunc) {
	if claimService == nil {
		t.Fatal("conformance factory must provide ClaimService for the distinct-models refusal")
	}
	_, orders := dmRound(t, x, 2, true)
	dmHold(t, x, orders[0], "gpt-6")
	claim := claimService(x.Backend, x.Config)
	session := orders[1].ID + "-service"
	_, err := claim(x.Context, orders[1].ID, core.WorkOrderClaim{ClaimantID: ffWorker, SessionID: session, ClientToken: session + "-token", Model: "GPT-6", Agent: "conformance", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if !errors.Is(err, store.ErrReviewSeatModelNotDistinct) || !strings.Contains(err.Error(), "gpt-6") {
		t.Fatalf("service claim err=%v", err)
	}
	records, err := x.Backend.ListWorkOrderClaimFailures(x.Context, []string{orders[1].ID})
	requireOK(t, err)
	if len(records) != 0 {
		t.Fatalf("refusal recorded claim failures %+v", records)
	}
	if seat := ffOrder(t, x, orders[1].ID); seat.State != core.WorkOrderQueued || !seat.Claimable {
		t.Fatalf("refused seat state=%s claimable=%t", seat.State, seat.Claimable)
	}
}

func dmTask(t *testing.T, x Fixture, seats int, distinct bool) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: seatFixtureHead, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats), DistinctModels: distinct}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	return stored
}

func dmRound(t *testing.T, x Fixture, seats int, distinct bool) (core.Task, []core.WorkOrder) {
	t.Helper()
	task := dmTask(t, x, seats, distinct)
	resolution := resolveSeats(t, x, task.ID, 1)
	jobs, orders := seatMembers(task.ID, 1, resolution.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, resolution, jobs, orders))
	return task, orders
}

// dmClaim claims a seat under a fresh session, so one seat can be claimed
// again after a release or an expiry.
func dmClaim(x Fixture, order core.WorkOrder, model string, lease time.Duration) (core.WorkOrder, error) {
	session := order.ID + "-" + core.NewTaskID()
	return ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: ffWorker, SessionID: session, ClientToken: session + "-token", Model: model, Agent: "conformance", Lease: lease, ExecutionTimeout: time.Hour})
}

func dmHold(t *testing.T, x Fixture, order core.WorkOrder, model string) core.WorkOrder {
	t.Helper()
	claimed, err := dmClaim(x, order, model, time.Hour)
	requireOK(t, err)
	return claimed
}

// dmRequireRefused asserts a claim's refusal names the held model names and
// leaves the seat queued, claimable, and the task's persisted state unchanged.
func dmRequireRefused(t *testing.T, x Fixture, order core.WorkOrder, model string, held ...string) {
	t.Helper()
	before := ffSnapshot(t, x, order.TaskID)
	_, err := dmClaim(x, order, model, time.Hour)
	if !errors.Is(err, store.ErrReviewSeatModelNotDistinct) {
		t.Fatalf("claim of seat %d with %q err=%v, want %v", order.ReviewSeat, model, err, store.ErrReviewSeatModelNotDistinct)
	}
	if !strings.Contains(err.Error(), "review_seat_model_not_distinct") {
		t.Fatalf("refusal %q does not carry its code", err)
	}
	for _, name := range held {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("refusal %q does not name held model %q", err, name)
		}
	}
	ffRequireUnchanged(t, "refused claim", ffSnapshot(t, x, order.TaskID), before)
	seat := ffOrder(t, x, order.ID)
	if seat.State != core.WorkOrderQueued || !seat.Claimable || seat.SessionID != "" {
		t.Fatalf("refused seat state=%s claimable=%t session=%q", seat.State, seat.Claimable, seat.SessionID)
	}
}

// The failure scenario of AC-2.10: a repeated name differing only in letter
// case and surrounding whitespace is refused, and the seat can still be
// claimed under a different name. The literal auto compares like any name.
func runDistinctModelsRefusal(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 4, true)
	dmHold(t, x, orders[0], "claude-sonnet-5-5")
	dmRequireRefused(t, x, orders[1], "Claude-Sonnet-5-5", "claude-sonnet-5-5")
	dmRequireRefused(t, x, orders[1], "  claude-sonnet-5-5\t", "claude-sonnet-5-5")
	claimed := dmHold(t, x, orders[1], "gpt-6")
	if claimed.Model != "gpt-6" {
		t.Fatalf("claimed model=%q", claimed.Model)
	}
	dmHold(t, x, orders[2], "auto")
	dmRequireRefused(t, x, orders[3], "AUTO", "claude-sonnet-5-5", "gpt-6", "auto")
}

func runDistinctModelsBlank(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 2, true)
	dmRequireRefused(t, x, orders[0], "")
	dmHold(t, x, orders[0], "gpt-6")
	dmRequireRefused(t, x, orders[1], "   ", "gpt-6")
}

func runDistinctModelsOff(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 3, false)
	dmHold(t, x, orders[0], "claude-sonnet-5-5")
	dmHold(t, x, orders[1], "claude-sonnet-5-5")
	dmHold(t, x, orders[2], "")
}

func runDistinctModelsReleased(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 2, true)
	claimed := dmHold(t, x, orders[0], "gpt-6")
	dmRequireRefused(t, x, orders[1], "gpt-6", "gpt-6")
	_, err := ReleaseWorkerClaim(x.Context, x.Backend, claimed.ID, "", core.WorkOrderRelease{SessionID: claimed.SessionID, Reason: "abandoned"})
	requireOK(t, err)
	dmHold(t, x, orders[1], "gpt-6")
}

// A claimed seat whose lease has lapsed has expired, even before a clock sweep
// moves its state.
func runDistinctModelsExpired(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 2, true)
	if _, err := dmClaim(x, orders[0], "gpt-6", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	dmHold(t, x, orders[1], "gpt-6")
}

func dmSubmit(t *testing.T, x Fixture, claimed core.WorkOrder) {
	t.Helper()
	claimed.State = core.WorkOrderSubmitted
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, claimed, core.WorkOrderCmdSubmitForReview))
}

func dmVerdict(t *testing.T, x Fixture, order core.WorkOrder, model, verdict string) {
	t.Helper()
	claimed := dmHold(t, x, order, model)
	requireOK(t, ffAccept(x, ffDecision(order, verdict, claimed.SessionID)))
}

func runDistinctModelsVerdicts(t *testing.T, x Fixture) {
	_, orders := dmRound(t, x, 3, true)
	dmSubmit(t, x, dmHold(t, x, orders[0], "submitted-model"))
	dmRequireRefused(t, x, orders[2], "SUBMITTED-MODEL", "submitted-model")
	dmVerdict(t, x, orders[1], "accepted-model", "approve")
	if state := ffOrder(t, x, orders[1].ID).State; state != core.WorkOrderCompleted {
		t.Fatalf("accepted seat state=%s", state)
	}
	dmRequireRefused(t, x, orders[2], "accepted-model", "submitted-model", "accepted-model")
	dmHold(t, x, orders[2], "third-model")
}

func runDistinctModelsAddedSeat(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 1, true)
	dmHold(t, x, orders[0], "gpt-6")
	added, err := rs5Add(x, rs5Request(task.ID, "dm-add-"+task.ID, 1))
	requireOK(t, err)
	if added.SeatCount != 2 {
		t.Fatalf("addition=%+v", added)
	}
	all, err := x.Backend.ListTaskWorkOrders(x.Context, task.ID)
	requireOK(t, err)
	var seat core.WorkOrder
	for _, order := range all {
		if order.Stage == core.StageReview && order.ReviewSeat == 2 {
			seat = order
		}
	}
	if seat.ID == "" {
		t.Fatalf("added seat missing from %+v", all)
	}
	dmRequireRefused(t, x, seat, "gpt-6", "gpt-6")
	dmHold(t, x, seat, "claude-opus-5-5")
}

func runDistinctModelsLaterRound(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 2, true)
	dmVerdict(t, x, orders[0], "gpt-6", "changes_requested")
	dmVerdict(t, x, orders[1], "claude-opus-5-5", "approve")
	next := resolveSeats(t, x, task.ID, 2)
	jobs, nextOrders := seatMembers(task.ID, 2, next.SeatCount)
	requireOK(t, createSeatRound(x, task.ID, next, jobs, nextOrders))
	dmHold(t, x, nextOrders[0], "claude-opus-5-5")
	dmRequireRefused(t, x, nextOrders[1], "claude-opus-5-5", "claude-opus-5-5")
	dmHold(t, x, nextOrders[1], "gpt-6")
}

// Per-task claim serialization lets exactly one concurrent sibling hold a name.
func runDistinctModelsConcurrent(t *testing.T, x Fixture) {
	for _, names := range [][]string{
		{"gpt-6", "gpt-6", "gpt-6", "gpt-6"},
		{"gpt-6", "GPT-6", " gpt-6 ", "Gpt-6"},
	} {
		task, orders := dmRound(t, x, len(names), true)
		errs := make([]error, len(orders))
		var start, done sync.WaitGroup
		start.Add(1)
		for i := range orders {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				_, errs[i] = dmClaim(x, orders[i], names[i], time.Hour)
			}()
		}
		start.Done()
		done.Wait()
		winners := 0
		for i, err := range errs {
			state := ffOrder(t, x, orders[i].ID).State
			switch {
			case err == nil:
				winners++
				if state != core.WorkOrderClaimed {
					t.Fatalf("winning seat %d state=%s", orders[i].ReviewSeat, state)
				}
			case errors.Is(err, store.ErrReviewSeatModelNotDistinct):
				if state != core.WorkOrderQueued {
					t.Fatalf("refused seat %d state=%s", orders[i].ReviewSeat, state)
				}
			default:
				t.Fatalf("seat %d err=%v", orders[i].ReviewSeat, err)
			}
		}
		if winners != 1 {
			t.Fatalf("task %s: %d seats claimed %v, errs=%v", task.ID, winners, names, errs)
		}
	}
}

func dmChange(x Fixture, taskID, requestID string, value bool) (store.SetupChangeResult, error) {
	return changeSeatPolicy(x, store.SetupChangeRequest{TaskID: taskID, RequestID: requestID, Reason: "operator reason " + requestID, Policy: &store.TaskPolicyChange{ReviewDistinctModels: &value}})
}

// A task with verify_stage true waiting at review would hand off to
// verification under any other policy change; this one resolves no handoff.
func runDistinctModelsPolicyOnly(t *testing.T, x Fixture) {
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: seatFixtureHead, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.PolicyContract{VerifyStage: true, MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	before := ffKinds(t, x, task.ID)
	requestID := "dm-only-" + task.ID
	result, err := dmChange(x, task.ID, requestID, true)
	requireOK(t, err)
	if result.ReviewTransition != "policy_only" || len(result.CreatedWorkOrders)+len(result.SupersededWorkOrders)+len(result.UpdatedWorkOrders) != 0 {
		t.Fatalf("policy-only change result=%+v", result)
	}
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if !stored.SetupContract.Review.DistinctModels || !stored.SetupContract.VerifyStage || stored.NextStage != core.StageReview || stored.SetupContract.StageTimeouts.Verify != "" {
		t.Fatalf("task after policy-only change next=%s policy=%+v", stored.NextStage, stored.SetupContract)
	}
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, task.ID)
	requireOK(t, err)
	if len(orders) != 0 {
		t.Fatalf("policy-only change created orders %+v", orders)
	}
	after := ffKinds(t, x, task.ID)
	if after["task.setup.changed"] != before["task.setup.changed"]+1 || after["review.round_created"] != before["review.round_created"] || after["work_order.created"] != before["work_order.created"] {
		t.Fatalf("event kinds before=%v after=%v", before, after)
	}
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	audited := false
	for _, event := range events {
		if event.Kind != "task.setup.changed" {
			continue
		}
		var payload struct {
			Reason   string `json:"reason"`
			NewSetup struct {
				Review struct {
					DistinctModels bool `json:"distinct_models"`
				} `json:"review"`
			} `json:"new_setup"`
		}
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		audited = payload.Reason == "operator reason "+requestID && payload.NewSetup.Review.DistinctModels
	}
	if !audited {
		t.Fatalf("no audited event holds the reason and new value: %+v", events)
	}
	if _, err := dmChange(x, task.ID, requestID, true); err != nil {
		t.Fatalf("replay err=%v", err)
	}
	if got := ffKinds(t, x, task.ID)["task.setup.changed"]; got != after["task.setup.changed"] {
		t.Fatalf("replay recorded another change: %d", got)
	}
	if _, err := dmChange(x, task.ID, requestID, false); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("different input under one request ID err=%v", err)
	}
}

func runDistinctModelsLaterClaims(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 3, false)
	_, err := dmChange(x, task.ID, "dm-on-"+task.ID, true)
	requireOK(t, err)
	dmVerdict(t, x, orders[0], "gpt-6", "approve")
	dmRequireRefused(t, x, orders[1], "gpt-6", "gpt-6")
	// An accepted verdict is not in flight, so the policy can change again.
	verdicts := ffKinds(t, x, task.ID)["review.completed"]
	_, err = dmChange(x, task.ID, "dm-off-"+task.ID, false)
	requireOK(t, err)
	if got := ffKinds(t, x, task.ID)["review.completed"]; got != verdicts {
		t.Fatalf("policy change altered verdicts: %d -> %d", verdicts, got)
	}
	dmHold(t, x, orders[1], "gpt-6")
}

func runDistinctModelsChangeRefused(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 2, false)
	claimed := dmHold(t, x, orders[0], "gpt-6")
	if _, err := dmChange(x, task.ID, "dm-claimed-"+task.ID, true); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("change while a seat is claimed err=%v", err)
	}
	dmSubmit(t, x, claimed)
	if _, err := dmChange(x, task.ID, "dm-submitted-"+task.ID, true); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("change while a seat holds a submitted verdict err=%v", err)
	}
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if stored.SetupContract.Review.DistinctModels {
		t.Fatal("refused change altered the frozen policy")
	}
}
