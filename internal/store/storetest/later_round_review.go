package storetest

// Per-task later-round review change conformance (feature-delta-review-rounds
// DR-6; req-review-gates-evidence AC-12.8; component-task-lifecycle invariants
// 44 and 45). Every case runs unchanged on memory, PostgreSQL, and SingleStore.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runLaterRoundReview(t *testing.T, x Fixture) {
	t.Run("AuditedChangeAndReplay", func(t *testing.T) { runLaterRoundAudited(t, x) })
	t.Run("AcceptedWhileOrderClaimed", func(t *testing.T) { runLaterRoundClaimed(t, x) })
	t.Run("AcceptedWhileVerdictSubmitted", func(t *testing.T) { runLaterRoundSubmitted(t, x) })
	t.Run("AcceptedForApprovedTask", func(t *testing.T) { runLaterRoundTaskState(t, x, core.TaskApproved) })
	t.Run("AcceptedAtGate", func(t *testing.T) { runLaterRoundTaskState(t, x, core.TaskAwaiting) })
	t.Run("TerminalTaskRefused", func(t *testing.T) { runLaterRoundTerminal(t, x) })
	t.Run("MixedRequestKeepsRefusals", func(t *testing.T) { runLaterRoundMixed(t, x) })
}

func lrScope(value string) *string { return &value }

func lrChange(x Fixture, taskID, requestID, value string) (store.SetupChangeResult, error) {
	return changeSeatPolicy(x, store.SetupChangeRequest{TaskID: taskID, RequestID: requestID, Reason: "operator reason " + requestID, Policy: &store.TaskPolicyChange{LaterRoundReview: lrScope(value)}})
}

// lrState is the comparable persisted state a later-round change must leave
// unchanged: the stage, approval, and recorded comparison of the task, and the
// state, session, head, scope, and baseline of each order. Events are counted
// by kind, so the one audited event can be told apart from any other effect.
type lrState struct {
	nextStage                          core.Stage
	state                              core.TaskState
	reviewedHead, refreshScope, policy string
	orders                             []string
	kinds                              map[string]int
}

func lrSnapshot(t *testing.T, x Fixture, taskID string) lrState {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	orders, err := x.Backend.ListTaskWorkOrders(x.Context, taskID)
	requireOK(t, err)
	state := lrState{nextStage: task.NextStage, state: task.State, reviewedHead: task.ReviewedHeadSHA, refreshScope: task.RefreshReviewScope, kinds: ffKinds(t, x, taskID)}
	contract := task.SetupContract
	contract.LaterRoundReview = ""
	encoded, err := json.Marshal(contract)
	requireOK(t, err)
	state.policy = string(encoded)
	for _, order := range orders {
		state.orders = append(state.orders, fmt.Sprintf("%s/%s/%s/%s/%s/%s/%d", order.ID, order.State, order.SessionID, order.HeadSHA, order.ReviewScope, order.BaselineSHA, order.ReviewRound))
	}
	sort.Strings(state.orders)
	return state
}

// lrRequireOnlyAudited asserts that the change recorded exactly one more
// task.setup.changed event and left every other persisted fact unchanged.
func lrRequireOnlyAudited(t *testing.T, label string, got, before lrState) {
	t.Helper()
	want := before
	want.kinds = map[string]int{}
	for kind, count := range before.kinds {
		want.kinds[kind] = count
	}
	want.kinds["task.setup.changed"]++
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s changed more than the frozen value:\n got %+v\nwant %+v", label, got, want)
	}
}

func lrRequireValue(t *testing.T, x Fixture, taskID, want string) {
	t.Helper()
	stored, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	// A task frozen before the member existed holds no value and reads as full.
	raw := stored.SetupContract.LaterRoundReview
	if stored.SetupContract.LaterRoundReviewScope() != want || (raw != "" && raw != want) {
		t.Fatalf("frozen later_round_review=%q, want %q", stored.SetupContract.LaterRoundReview, want)
	}
}

func lrTask(t *testing.T, x Fixture, state core.TaskState, next core.Stage) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: state, NextStage: next, ReviewedHeadSHA: seatFixtureHead, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.PolicyContract{VerifyStage: true, MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}, RefreshReview: config.RefreshReviewDelta}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	return task
}

// A task with verify_stage true waiting at review would hand off to
// verification under a verify_stage change; this change records the audited
// event with the actor, reason, and new value and changes nothing else. A
// matching request ID replays without another event, and a reused request ID
// with different input is refused.
func runLaterRoundAudited(t *testing.T, x Fixture) {
	task := lrTask(t, x, core.TaskRunning, core.StageReview)
	before := lrSnapshot(t, x, task.ID)
	requestID := "lr-delta-" + task.ID
	result, err := lrChange(x, task.ID, requestID, config.LaterRoundReviewDelta)
	requireOK(t, err)
	if result.ReviewTransition != "policy_only" || len(result.CreatedWorkOrders)+len(result.SupersededWorkOrders)+len(result.UpdatedWorkOrders) != 0 {
		t.Fatalf("later-round change result=%+v", result)
	}
	if result.Task.SetupContract.LaterRoundReview != config.LaterRoundReviewDelta || result.Task.NextStage != core.StageReview {
		t.Fatalf("result task next=%s policy=%+v", result.Task.NextStage, result.Task.SetupContract)
	}
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewDelta)
	after := lrSnapshot(t, x, task.ID)
	lrRequireOnlyAudited(t, "later-round change", after, before)
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if stored.SetupContract.StageTimeouts.Verify != "" || !stored.SetupContract.VerifyStage {
		t.Fatalf("later-round change altered another policy member: %+v", stored.SetupContract)
	}
	events, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	audited := 0
	for _, event := range events {
		if event.Kind != "task.setup.changed" {
			continue
		}
		var payload struct {
			Actor     string `json:"actor"`
			RequestID string `json:"request_id"`
			Reason    string `json:"reason"`
			Previous  struct {
				LaterRoundReview string `json:"later_round_review"`
			} `json:"previous_setup"`
			NewSetup struct {
				LaterRoundReview string `json:"later_round_review"`
			} `json:"new_setup"`
		}
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Reason == "operator reason "+requestID && payload.RequestID == requestID && payload.NewSetup.LaterRoundReview == config.LaterRoundReviewDelta &&
			payload.Previous.LaterRoundReview == "" && payload.Actor != "" {
			audited++
		}
	}
	if audited != 1 {
		t.Fatalf("audited events holding the actor, reason, and new value = %d: %+v", audited, events)
	}
	replayed, err := lrChange(x, task.ID, requestID, config.LaterRoundReviewDelta)
	if err != nil {
		t.Fatalf("replay err=%v", err)
	}
	if replayed.RequestID != result.RequestID || replayed.ReviewTransition != result.ReviewTransition || replayed.Task.SetupContract.LaterRoundReview != config.LaterRoundReviewDelta {
		t.Fatalf("replay result=%+v, want %+v", replayed, result)
	}
	if got := lrSnapshot(t, x, task.ID); !reflect.DeepEqual(got, after) {
		t.Fatalf("replay changed persisted state:\n got %+v\nwant %+v", got, after)
	}
	if _, err := lrChange(x, task.ID, requestID, config.LaterRoundReviewFull); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("different input under one request ID err=%v", err)
	}
	if got := lrSnapshot(t, x, task.ID); !reflect.DeepEqual(got, after) {
		t.Fatalf("refused replay changed persisted state:\n got %+v\nwant %+v", got, after)
	}
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewDelta)
	result, err = lrChange(x, task.ID, "lr-full-"+task.ID, config.LaterRoundReviewFull)
	requireOK(t, err)
	if result.ReviewTransition != "policy_only" {
		t.Fatalf("second change result=%+v", result)
	}
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewFull)
}

// A claimed seat neither refuses the change nor is moved by it: the claim, its
// session, head, scope, and baseline, and the round stay as recorded.
func runLaterRoundClaimed(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 2, false)
	claimed := dmHold(t, x, orders[0], "gpt-6")
	before := lrSnapshot(t, x, task.ID)
	if _, err := lrChange(x, task.ID, "lr-claimed-"+task.ID, config.LaterRoundReviewDelta); err != nil {
		t.Fatalf("change while a seat is claimed err=%v", err)
	}
	lrRequireOnlyAudited(t, "change while claimed", lrSnapshot(t, x, task.ID), before)
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewDelta)
	if current := ffOrder(t, x, claimed.ID); current.State != core.WorkOrderClaimed || current.SessionID != claimed.SessionID {
		t.Fatalf("claimed seat after change state=%s session=%s", current.State, current.SessionID)
	}
	// The claim still completes under the session that held it.
	dmSubmit(t, x, claimed)
}

// A submitted review verdict neither refuses the change nor is changed by it.
func runLaterRoundSubmitted(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 1, false)
	claimed := dmHold(t, x, orders[0], "gpt-6")
	dmSubmit(t, x, claimed)
	before := lrSnapshot(t, x, task.ID)
	if _, err := lrChange(x, task.ID, "lr-submitted-"+task.ID, config.LaterRoundReviewDelta); err != nil {
		t.Fatalf("change while a verdict is submitted err=%v", err)
	}
	lrRequireOnlyAudited(t, "change while submitted", lrSnapshot(t, x, task.ID), before)
	if current := ffOrder(t, x, claimed.ID); current.State != core.WorkOrderSubmitted || current.SessionID != claimed.SessionID {
		t.Fatalf("submitted seat after change state=%s session=%s", current.State, current.SessionID)
	}
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewDelta)
}

// An approved task and a task waiting at a gate accept the change. Any other
// policy field keeps the refusal of component-task-lifecycle invariant 45.
func runLaterRoundTaskState(t *testing.T, x Fixture, state core.TaskState) {
	task := lrTask(t, x, state, "")
	before := lrSnapshot(t, x, task.ID)
	if _, err := lrChange(x, task.ID, "lr-state-"+task.ID, config.LaterRoundReviewDelta); err != nil {
		t.Fatalf("change on a %s task err=%v", state, err)
	}
	lrRequireOnlyAudited(t, "change on a "+string(state)+" task", lrSnapshot(t, x, task.ID), before)
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewDelta)
	if _, err := dmChange(x, task.ID, "lr-state-dm-"+task.ID, true); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("distinct-models change on a %s task err=%v", state, err)
	}
}

// A terminal task refuses the change and keeps its frozen value.
func runLaterRoundTerminal(t *testing.T, x Fixture) {
	for _, state := range []core.TaskState{core.TaskMerged, core.TaskClosed} {
		task := lrTask(t, x, state, "")
		before := lrSnapshot(t, x, task.ID)
		if _, err := lrChange(x, task.ID, "lr-terminal-"+task.ID, config.LaterRoundReviewDelta); !errors.Is(err, store.ErrSetupChangeConflict) {
			t.Fatalf("change on a %s task err=%v", state, err)
		}
		if got := lrSnapshot(t, x, task.ID); !reflect.DeepEqual(got, before) {
			t.Fatalf("refused change on a %s task changed persisted state:\n got %+v\nwant %+v", state, got, before)
		}
		lrRequireValue(t, x, task.ID, config.LaterRoundReviewFull)
	}
}

// A request that also holds another policy field keeps every refusal of
// component-task-lifecycle invariant 45, here a claimed seat.
func runLaterRoundMixed(t *testing.T, x Fixture) {
	task, orders := dmRound(t, x, 2, false)
	dmHold(t, x, orders[0], "gpt-6")
	before := lrSnapshot(t, x, task.ID)
	distinct := true
	_, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "lr-mixed-" + task.ID, Reason: "mixed",
		Policy: &store.TaskPolicyChange{LaterRoundReview: lrScope(config.LaterRoundReviewDelta), ReviewDistinctModels: &distinct}})
	if !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("mixed change while a seat is claimed err=%v", err)
	}
	if got := lrSnapshot(t, x, task.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused mixed change changed persisted state:\n got %+v\nwant %+v", got, before)
	}
	lrRequireValue(t, x, task.ID, config.LaterRoundReviewFull)
	enabled := true
	if _, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "lr-verify-" + task.ID, Reason: "mixed verify",
		Policy: &store.TaskPolicyChange{LaterRoundReview: lrScope(config.LaterRoundReviewDelta), VerifyStage: &enabled}}); !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("mixed verify_stage change while a seat is claimed err=%v", err)
	}
	if _, err := lrChange(x, task.ID, "lr-invalid-"+task.ID, "none"); err == nil || errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("invalid later_round_review err=%v", err)
	}
}
