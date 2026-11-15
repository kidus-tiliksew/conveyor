package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func laterRound(value string) *string { return &value }

// feature-delta-review-rounds DR-6: later_round_review accepts full or delta,
// travels with any field but plan_state, and only a change holding it alone
// skips the task-state and order exclusions.
func TestLaterRoundReviewPolicyChangeValidation(t *testing.T) {
	enabled := true
	needed := core.PlanNeeded
	for _, tc := range []struct {
		name         string
		policy       TaskPolicyChange
		fail         string
		only, noMove bool
	}{
		{name: "full", policy: TaskPolicyChange{LaterRoundReview: laterRound("full")}, only: true, noMove: true},
		{name: "delta", policy: TaskPolicyChange{LaterRoundReview: laterRound("delta")}, only: true, noMove: true},
		{name: "none", policy: TaskPolicyChange{LaterRoundReview: laterRound("none")}, fail: "later_round_review must be full or delta"},
		{name: "blank", policy: TaskPolicyChange{LaterRoundReview: laterRound("")}, fail: "later_round_review must be full or delta"},
		{name: "with plan_state", policy: TaskPolicyChange{PlanState: &needed, LaterRoundReview: laterRound("delta")}, fail: "plan_state must be the only policy field"},
		{name: "with distinct models", policy: TaskPolicyChange{LaterRoundReview: laterRound("delta"), ReviewDistinctModels: &enabled}, noMove: true},
		{name: "with verify stage", policy: TaskPolicyChange{LaterRoundReview: laterRound("delta"), VerifyStage: &enabled}},
		{name: "with verify timeout", policy: TaskPolicyChange{LaterRoundReview: laterRound("delta"), StageTimeouts: map[string]string{"verify": "2h"}}},
		{name: "empty", policy: TaskPolicyChange{}, fail: "later_round_review is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("Validate err=%v, want %q", err, tc.fail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.policy.OnlyLaterRoundReview(); got != tc.only {
				t.Fatalf("OnlyLaterRoundReview=%t, want %t", got, tc.only)
			}
			if got := tc.policy.MovesNoStage(); got != tc.noMove {
				t.Fatalf("MovesNoStage=%t, want %t", got, tc.noMove)
			}
		})
	}
}

// A change holding only later_round_review returns a policy-only plan before
// every task-state and order exclusion, and it carries no order, round, or
// comparison mutation. Any other field keeps the exclusions.
func TestLaterRoundReviewPolicyOnlyPlan(t *testing.T) {
	contract := config.PolicyContract{VerifyStage: true, MaxBounces: 3, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}, RefreshReview: config.RefreshReviewNone}
	claimed := core.WorkOrder{ID: "task-review-1-seat-1", TaskID: "task", Stage: core.StageReview, State: core.WorkOrderClaimed, ReviewRound: 1, ReviewSeat: 1, HeadSHA: "head", ReviewScope: "delta", BaselineSHA: "base"}
	submitted := claimed
	submitted.State = core.WorkOrderSubmitted
	for _, tc := range []struct {
		name   string
		task   core.Task
		orders []core.WorkOrder
	}{
		{"claimed order", core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview, SetupContract: contract}, []core.WorkOrder{claimed}},
		{"submitted verdict", core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview, SetupContract: contract}, []core.WorkOrder{submitted}},
		{"approved task", core.Task{ID: "task", State: core.TaskApproved, SetupContract: contract}, nil},
		{"task at a gate", core.Task{ID: "task", State: core.TaskAwaiting, SetupContract: contract}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := SetupChangeRequest{TaskID: "task", RequestID: "request", Reason: "reason", Policy: &TaskPolicyChange{LaterRoundReview: laterRound("delta")},
				NewWorkOrders: []core.WorkOrder{{ID: "caller"}}, SupersedeWorkOrderIDs: []string{"caller"}, ResultingReviewRound: 9}
			planned, err := PlanTaskPolicyChange(tc.task, tc.orders, request, PolicyHandoffSeatInputs{})
			if err != nil {
				t.Fatal(err)
			}
			want := contract
			want.LaterRoundReview = config.LaterRoundReviewDelta
			got, _ := json.Marshal(planned.Setup)
			wantJSON, _ := json.Marshal(want)
			if string(got) != string(wantJSON) {
				t.Fatalf("planned setup %s, want %s", got, wantJSON)
			}
			if planned.ReviewTransition != "policy_only" || planned.NextStage != tc.task.NextStage || len(planned.NewJobs)+len(planned.NewWorkOrders)+len(planned.WorkOrderUpdates)+len(planned.SupersedeWorkOrderIDs) != 0 ||
				planned.PriorReviewRound != 0 || planned.ResultingReviewRound != 0 || planned.ReviewRoundSeats != nil || planned.PlanStateEffects != nil {
				t.Fatalf("planned %+v", planned)
			}
			enabled := true
			mixed := request
			mixed.Policy = &TaskPolicyChange{LaterRoundReview: laterRound("delta"), ReviewDistinctModels: &enabled}
			if _, err := PlanTaskPolicyChange(tc.task, tc.orders, mixed, PolicyHandoffSeatInputs{}); !errors.Is(err, ErrSetupChangeConflict) {
				t.Fatalf("mixed change err=%v", err)
			}
		})
	}
}

// A task that predates policy freezing stays unfrozen after the change, so
// dispatch keeps applying the workspace policy to every other member.
func TestLaterRoundReviewPolicyOnUnfrozenTask(t *testing.T) {
	task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageImplement}
	request := SetupChangeRequest{TaskID: "task", RequestID: "request", Reason: "reason", Policy: &TaskPolicyChange{LaterRoundReview: laterRound("delta")}}
	planned, err := PlanTaskPolicyChange(task, nil, request, PolicyHandoffSeatInputs{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Setup.HasFrozenPolicy() || planned.Setup.LaterRoundReviewScope() != config.LaterRoundReviewDelta || planned.Setup.StageTimeouts.Verify != "" {
		t.Fatalf("planned setup %+v frozen=%t", planned.Setup, planned.Setup.HasFrozenPolicy())
	}
}

// Normalization copies the pointer, and the replay identity includes the
// value while an absent value leaves an earlier identity's JSON unchanged.
func TestLaterRoundReviewPolicyReplayIdentity(t *testing.T) {
	value := "delta"
	request, err := PrepareSetupChangeRequest(SetupChangeRequest{TaskID: " task ", RequestID: " request ", Reason: " reason ", Policy: &TaskPolicyChange{LaterRoundReview: &value}})
	if err != nil {
		t.Fatal(err)
	}
	value = "full"
	if *request.Policy.LaterRoundReview != "delta" {
		t.Fatal("normalization kept the caller's pointer")
	}
	other := request
	other.Policy = &TaskPolicyChange{LaterRoundReview: laterRound("full")}
	if SameSetupChange(request, other) {
		t.Fatal("different later_round_review values share a replay identity")
	}
	again := request
	again.Policy = &TaskPolicyChange{LaterRoundReview: laterRound("delta")}
	if !SameSetupChange(request, again) {
		t.Fatal("matching later_round_review values differ in replay identity")
	}
	identity, _ := json.Marshal(SetupChangeIdentity(request))
	if !strings.Contains(string(identity), `"later_round_review":"delta"`) {
		t.Fatalf("identity %s", identity)
	}
	enabled := true
	earlier, _ := json.Marshal(SetupChangeIdentity(SetupChangeRequest{TaskID: "task", RequestID: "request", Reason: "reason", Policy: &TaskPolicyChange{ReviewDistinctModels: &enabled}}))
	if strings.Contains(string(earlier), "later_round_review") {
		t.Fatalf("an earlier identity gained the member: %s", earlier)
	}
}
