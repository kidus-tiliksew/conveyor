package store

import (
	"errors"
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

func headRiskOrder(taskID, id, head string, state core.WorkOrderState, paths ...string) core.RiskOrder {
	return core.RiskOrder{Order: core.WorkOrder{ID: id, TaskID: taskID, Stage: core.StageRisk, State: state, HeadSHA: head, BaselineSHA: "base"},
		Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base"}, ChangedPaths: paths}
}

func riskTaskWith(mode riskpolicy.Mode, table riskpolicy.Table) core.Task {
	task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview}
	if mode != "" {
		task.SetupContract.Risk = &riskpolicy.Policy{Mode: mode, Tiers: table}
	}
	return task
}

// TestPlanHeadRiskOrder proves the head handoff's need decision and
// cancellations: off plans nothing, high creates nothing, a head covered by
// the latest completed head order creates nothing, an open order covers
// nothing, a plan order is never cancelled, and every open order of another
// head is cancelled whatever the decision answers (component-risk-review
// invariant 12; req-risk-based-review AC-5.8, AC-5.9, AC-5.11, AC-12.3).
func TestPlanHeadRiskOrder(t *testing.T) {
	record := riskTaskWith(riskpolicy.ModeRecord, riskpolicy.DefaultTable())
	request := HeadRiskOrderRequest{TaskID: "task", Subject: core.RiskOrderSubject{HeadSHA: "h2", BaseSHA: "base"}, ChangedPaths: []string{"a.go"}}
	plan := core.RiskOrder{Order: core.WorkOrder{ID: "task-risk-9", TaskID: "task", Stage: core.StageRisk, State: core.WorkOrderQueued}, Subject: core.RiskOrderSubject{PlanVersion: 1}}
	high := []core.RiskRating{{Key: core.PathRiskKey("h2"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierHigh}}
	for _, tc := range []struct {
		name    string
		task    core.Task
		ratings []core.RiskRating
		orders  []core.RiskOrder
		create  bool
		cancel  []string
	}{
		{name: "Off", task: riskTaskWith("", riskpolicy.Table{}), orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h1", core.WorkOrderQueued)}},
		{name: "FirstHead", task: record, orders: []core.RiskOrder{plan}, create: true, cancel: []string{}},
		{name: "HighCancelsAndCreatesNothing", task: record, ratings: high, orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h1", core.WorkOrderClaimed)}, cancel: []string{"task-risk-1"}},
		{name: "Covered", task: record, orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h1", core.WorkOrderCompleted, "a.go", "b.go")}, cancel: []string{}},
		{name: "NewPath", task: record, orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h1", core.WorkOrderCompleted, "b.go")}, create: true, cancel: []string{}},
		{name: "OpenOrderCoversNothing", task: record, orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h1", core.WorkOrderQueued, "a.go")}, create: true, cancel: []string{"task-risk-1"}},
		{name: "SameHeadReplays", task: record, orders: []core.RiskOrder{headRiskOrder("task", "task-risk-1", "h2", core.WorkOrderQueued, "a.go")}, create: true, cancel: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanHeadRiskOrder(tc.task, tc.ratings, tc.orders, request)
			if err != nil {
				t.Fatal(err)
			}
			if (got.Create != nil) != tc.create || !slices.Equal(got.CancelIDs, tc.cancel) {
				t.Fatalf("plan=%+v, want create=%t cancel=%v", got, tc.create, tc.cancel)
			}
		})
	}
	if _, err := PlanHeadRiskOrder(record, nil, nil, HeadRiskOrderRequest{TaskID: "task", Subject: core.RiskOrderSubject{PlanVersion: 1}}); err == nil {
		t.Fatal("a plan subject was accepted as a head")
	}
}

// TestReviewRoundRiskHold proves the hold applies only under enforce, only to
// an open risk order of exactly the round's head, and never to a terminal
// order, a plan order, or an older head (req-risk-based-review AC-1.6,
// AC-5.10; component-risk-review invariant 13).
func TestReviewRoundRiskHold(t *testing.T) {
	enforce := riskTaskWith(riskpolicy.ModeEnforce, riskpolicy.DefaultTable())
	record := riskTaskWith(riskpolicy.ModeRecord, riskpolicy.DefaultTable())
	plan := core.RiskOrder{Order: core.WorkOrder{ID: "plan", Stage: core.StageRisk, State: core.WorkOrderQueued}, Subject: core.RiskOrderSubject{PlanVersion: 1}}
	for _, tc := range []struct {
		name  string
		task  core.Task
		order core.RiskOrder
		held  bool
	}{
		{"Queued", enforce, headRiskOrder("task", "r", "h1", core.WorkOrderQueued), true},
		{"Claimed", enforce, headRiskOrder("task", "r", "h1", core.WorkOrderClaimed), true},
		{"Stale", enforce, headRiskOrder("task", "r", "h1", core.WorkOrderStale), true},
		{"Completed", enforce, headRiskOrder("task", "r", "h1", core.WorkOrderCompleted), false},
		{"Cancelled", enforce, headRiskOrder("task", "r", "h1", core.WorkOrderCancelled), false},
		{"OlderHead", enforce, headRiskOrder("task", "r", "h0", core.WorkOrderQueued), false},
		{"PlanOrder", enforce, plan, false},
		{"Record", record, headRiskOrder("task", "r", "h1", core.WorkOrderQueued), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, held := ReviewRoundRiskHold(tc.task, []core.RiskOrder{tc.order}, "h1"); held != tc.held {
				t.Fatalf("held=%t, want %t", held, tc.held)
			}
		})
	}
	if !errors.Is(ReviewRoundHeldError(core.WorkOrder{ID: "r"}), ErrReviewRoundHeldByRiskOrder) {
		t.Fatal("held error does not wrap ErrReviewRoundHeldByRiskOrder")
	}
}

// TestDeclaredCountBelowARaisedRiskFloor proves that a declared count the
// submission admitted resolves to the tier's agent reviews with source risk
// when a later risk rating raised the floor, and that every other declaration
// below the floor stays declared, so the round-building command refuses it
// (feature-risk-based-review RR-12; req-review-gates-evidence AC-2.3;
// req-risk-based-review AC-7.1).
func TestDeclaredCountBelowARaisedRiskFloor(t *testing.T) {
	two, four := 2, 4
	for _, tc := range []struct {
		name          string
		declared      int
		planned       *int
		risk, atAdmit int
		count         int
		source        string
	}{
		{name: "LaterRiskRaise", declared: 2, risk: 3, atAdmit: 1, count: 3, source: ReviewSeatSourceRisk},
		{name: "AtOrAboveFloorStaysDeclared", declared: 3, risk: 3, atAdmit: 1, count: 3, source: ReviewSeatSourceDeclared},
		{name: "BelowItsSubmissionFloor", declared: 2, risk: 3, atAdmit: 3, count: 2, source: ReviewSeatSourceDeclared},
		{name: "BelowANonRiskFloor", declared: 2, planned: &four, risk: 3, atAdmit: 1, count: 2, source: ReviewSeatSourceDeclared},
		{name: "PlannedFloorAboveRisk", declared: 2, planned: &four, risk: 5, atAdmit: 1, count: 2, source: ReviewSeatSourceDeclared},
		{name: "PlannedAdmittedRiskRaise", declared: 4, planned: &four, risk: 5, atAdmit: 2, count: 5, source: ReviewSeatSourceRisk},
		{name: "NotEnforcing", declared: 2, planned: &two, count: 2, source: ReviewSeatSourceDeclared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared := tc.declared
			got := ResolveReviewRoundSeats(ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, PlannedSeatCount: tc.planned,
				ImplementationWorkOrderID: "impl", DeclaredSeatCount: &declared, RiskSeatCount: tc.risk, DeclaredRiskSeatCount: tc.atAdmit})
			if got.SeatCount != tc.count || got.Source != tc.source || got.ImplementationWorkOrderID != "impl" {
				t.Fatalf("resolution=%+v, want %d from %s", got, tc.count, tc.source)
			}
			// The round-building command accepts the risk resolution and
			// refuses a declared count below the floor.
			err := CheckReviewRoundSeats(got, config.PolicyContract{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}, tc.planned, tc.risk, nil, got.SeatCount)
			if belowFloor := got.SeatCount < got.FloorSeatCount; (err != nil) != belowFloor {
				t.Fatalf("check err=%v, below floor=%t", err, belowFloor)
			}
		})
	}
}

// TestRiskSeatCountAtSubmission proves the admitted floor counts only the
// ratings recorded up to the head's path rating.
func TestRiskSeatCountAtSubmission(t *testing.T) {
	table := riskpolicy.DefaultTable()
	table.Medium.AgentReviews, table.High.AgentReviews = 3, 3
	task := riskTaskWith(riskpolicy.ModeEnforce, table)
	ratings := []core.RiskRating{
		{Seq: 1, Key: core.PathRiskKey("h1"), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierLow},
		{Seq: 2, Key: core.RiskAgentKey("task-risk-1"), Source: core.RiskSourceRiskAgent, Tier: riskpolicy.TierMedium},
	}
	if now, admitted := RiskSeatCount(task, ratings), RiskSeatCountAtSubmission(task, ratings, "h1"); now != 3 || admitted != 1 {
		t.Fatalf("now=%d admitted=%d, want 3 and 1", now, admitted)
	}
	if admitted := RiskSeatCountAtSubmission(task, ratings, "unrated"); admitted != 3 {
		t.Fatalf("a head without a path rating admitted at %d, want every rating counted", admitted)
	}
	if admitted := RiskSeatCountAtSubmission(riskTaskWith(riskpolicy.ModeRecord, table), ratings, "h1"); admitted != 0 {
		t.Fatalf("record task admitted at %d", admitted)
	}
}

// TestPolicyHandoffWaitsForTheHeadRiskOrder proves a handoff to review keeps
// the submitted head at review and opens no round while an enforcing task's
// risk order for that head is open, opens the round otherwise, and that a
// claimed risk order blocks no policy change (feature-risk-based-review RR-12).
func TestPolicyHandoffWaitsForTheHeadRiskOrder(t *testing.T) {
	disabled := false
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeEnforce, riskpolicy.ModeRecord} {
		t.Run(string(mode), func(t *testing.T) {
			task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head",
				SetupContract: config.PolicyContract{VerifyStage: true, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)},
					Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}}
			verify := core.WorkOrder{ID: "verify", TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head", BaselineSHA: "base"}
			risk := headRiskOrder(task.ID, "task-risk-1", "head", core.WorkOrderClaimed)
			result, err := PlanTaskPolicyChange(task, []core.WorkOrder{verify, risk.Order}, SetupChangeRequest{Policy: &TaskPolicyChange{VerifyStage: &disabled}},
				PolicyHandoffSeatInputs{RiskOrders: []core.RiskOrder{risk}})
			if err != nil {
				t.Fatal(err)
			}
			if result.NextStage != core.StageReview || result.ReviewTransition != "policy_handoff" || !slices.Equal(result.SupersedeWorkOrderIDs, []string{"verify"}) {
				t.Fatalf("handoff=%+v", result)
			}
			held := mode == riskpolicy.ModeEnforce
			if opened := len(result.NewWorkOrders) > 0 && result.ReviewRoundSeats != nil; opened == held {
				t.Fatalf("mode %s opened=%t orders=%+v seats=%+v", mode, opened, result.NewWorkOrders, result.ReviewRoundSeats)
			}
			if held && (len(result.NewJobs) != 0 || result.ResultingReviewRound != 0) {
				t.Fatalf("held handoff wrote round members: jobs=%+v round=%d", result.NewJobs, result.ResultingReviewRound)
			}
		})
	}
}
