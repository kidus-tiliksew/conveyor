package store

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

func riskTask(mode riskpolicy.Mode, gate bool) core.Task {
	return core.Task{ID: "risk-task", MergeApproval: gate, SetupContract: config.PolicyContract{Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}}
}

func pathRated(tier riskpolicy.Tier, approvals int) []core.RiskRating {
	return []core.RiskRating{{Seq: 1, Source: core.RiskSourcePathRule, Tier: tier, HeadSHA: "head", HumanApprovals: approvals}}
}

// TestResolveReviewRoundSeatsRiskRows covers the risk row of the seat table
// (req-risk-based-review REQ-7, AC-7.1; req-review-gates-evidence AC-2.3,
// AC-2.4, AC-2.7): the tier names the source only when it raises the floor,
// and larger declared, carried, and planned counts survive.
func TestResolveReviewRoundSeatsRiskRows(t *testing.T) {
	count := func(n int) *int { return &n }
	for _, tc := range []struct {
		name   string
		in     ReviewRoundSeatInputs
		seats  int
		source string
		floor  int
	}{
		{"RiskRaisesMinimum", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, RiskSeatCount: 2}, 2, ReviewSeatSourceRisk, 2},
		{"RiskTiesMinimum", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 2, RiskSeatCount: 2}, 2, ReviewSeatSourceMinimum, 2},
		{"RiskTiesPlanned", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, PlannedSeatCount: count(3), RiskSeatCount: 3}, 3, ReviewSeatSourcePlanned, 3},
		{"RiskAbovePlanned", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, PlannedSeatCount: count(2), RiskSeatCount: 4}, 4, ReviewSeatSourceRisk, 4},
		{"PlannedAboveRisk", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, PlannedSeatCount: count(5), RiskSeatCount: 2}, 5, ReviewSeatSourcePlanned, 5},
		{"LargerDeclarationSurvives", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1, ImplementationWorkOrderID: "impl", DeclaredSeatCount: count(5), RiskSeatCount: 2}, 5, ReviewSeatSourceDeclared, 2},
		{"LargerCarriedSurvives", ReviewRoundSeatInputs{Round: 2, MinimumSeatCount: 1, PreviousRound: 1, PreviousSeatCount: 3, PreviousRecorded: true, RiskSeatCount: 2}, 3, ReviewSeatSourceCarried, 2},
		{"SmallerCarriedGivesRisk", ReviewRoundSeatInputs{Round: 2, MinimumSeatCount: 1, PreviousRound: 1, PreviousSeatCount: 1, PreviousRecorded: true, RiskSeatCount: 2}, 2, ReviewSeatSourceRisk, 2},
		{"NoRiskUnchanged", ReviewRoundSeatInputs{Round: 1, MinimumSeatCount: 1}, 1, ReviewSeatSourceMinimum, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveReviewRoundSeats(tc.in)
			if got.SeatCount != tc.seats || got.Source != tc.source || got.FloorSeatCount != tc.floor {
				t.Fatalf("resolution=%+v, want %d seats from %s at floor %d", got, tc.seats, tc.source, tc.floor)
			}
		})
	}
}

// TestCheckReviewRoundSeatsRisk covers the in-transaction revalidation of the
// risk floor and source.
func TestCheckReviewRoundSeatsRisk(t *testing.T) {
	setup := config.PolicyContract{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}
	for _, tc := range []struct {
		name  string
		want  ReviewRoundSeatResolution
		risk  int
		count int
		ok    bool
	}{
		{"RiskMatches", ReviewRoundSeatResolution{SeatCount: 2, Source: ReviewSeatSourceRisk, MinimumSeatCount: 1}, 2, 2, true},
		{"BelowRiskFloor", ReviewRoundSeatResolution{SeatCount: 1, Source: ReviewSeatSourceMinimum, MinimumSeatCount: 1}, 2, 1, false},
		{"RiskWithoutRaise", ReviewRoundSeatResolution{SeatCount: 1, Source: ReviewSeatSourceRisk, MinimumSeatCount: 1}, 1, 1, false},
		{"RiskCountMismatch", ReviewRoundSeatResolution{SeatCount: 3, Source: ReviewSeatSourceRisk, MinimumSeatCount: 1}, 2, 3, false},
		{"CarriedAboveRisk", ReviewRoundSeatResolution{SeatCount: 3, Source: ReviewSeatSourceCarried, MinimumSeatCount: 1}, 2, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckReviewRoundSeats(tc.want, setup, nil, tc.risk, nil, tc.count)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrReviewRoundSeatsInvalid)) {
				t.Fatalf("err=%v, want ok=%t", err, tc.ok)
			}
		})
	}
	if !ValidReviewSeatSource(ReviewSeatSourceRisk) || (ReviewRoundSeats{TaskID: "t", ReviewRound: 1, SeatCount: 2, SeatCountSource: ReviewSeatSourceRisk, MinimumSeatCount: 1}).Validate() != nil {
		t.Fatal("risk is not a valid seat source")
	}
}

// TestRiskRequirementsOnlyUnderEnforce covers the seat count, the merge hold,
// and the merge refusal (req-risk-based-review REQ-2, AC-2.1, REQ-7, REQ-8,
// AC-8.1, AC-8.2, AC-8.7).
func TestRiskRequirementsOnlyUnderEnforce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		task      core.Task
		ratings   []core.RiskRating
		seats     int
		required  int
		hold      bool
		automatic bool
		operator  bool
	}{
		{"OffNeverHolds", core.Task{ID: "off"}, nil, 0, 0, false, false, false},
		{"RecordHighNeverHolds", riskTask(riskpolicy.ModeRecord, false), pathRated(riskpolicy.TierHigh, 3), 0, 0, false, false, false},
		{"EnforceUnratedIsLow", riskTask(riskpolicy.ModeEnforce, false), nil, 1, 0, false, false, false},
		{"EnforceLow", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierLow, 0), 1, 0, false, false, false},
		{"EnforceLowGateOn", riskTask(riskpolicy.ModeEnforce, true), pathRated(riskpolicy.TierLow, 0), 1, 1, true, true, false},
		{"EnforceMedium", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierMedium, 0), 1, 1, true, true, false},
		{"EnforcePathMinimum", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierLow, 2), 1, 2, true, true, true},
		{"EnforceHigh", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierHigh, 0), 2, 2, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RiskSeatCount(tc.task, tc.ratings); got != tc.seats {
				t.Fatalf("RiskSeatCount=%d, want %d", got, tc.seats)
			}
			if required, hold := RiskMergeHold(tc.task, tc.ratings); required != tc.required || hold != tc.hold {
				t.Fatalf("RiskMergeHold=%d,%t, want %d,%t", required, hold, tc.required, tc.hold)
			}
			if err := RiskMergeRefusal(tc.task, tc.ratings, true); (err != nil) != tc.automatic || (err != nil && !errors.Is(err, ErrRefusal)) {
				t.Fatalf("automatic refusal=%v, want refused=%t", err, tc.automatic)
			}
			if err := RiskMergeRefusal(tc.task, tc.ratings, false); (err != nil) != tc.operator {
				t.Fatalf("operator refusal=%v, want refused=%t", err, tc.operator)
			}
		})
	}
}

// TestPlanRiskSeatAddition covers when a raised tier adds seats: only to the
// open, unsettled, latest round of an enforcing task, only up to the tier's
// count, and never for a replay of an older round.
func TestPlanRiskSeatAddition(t *testing.T) {
	task := riskTask(riskpolicy.ModeEnforce, false)
	task.State = core.TaskRunning
	high := pathRated(riskpolicy.TierHigh, 0)
	seat := func(round, number int) core.WorkOrder {
		return core.WorkOrder{ID: "seat", TaskID: task.ID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: number, HeadSHA: "head"}
	}
	trigger := RiskSeatTrigger{RequestID: "risk:seat", Reason: "raised", ActorID: "review:round:1", Round: 1}
	plan, add, err := PlanRiskSeatAddition(RiskSeatAdditionInputs{Task: task, Orders: []core.WorkOrder{seat(1, 1)}, Ratings: high, Trigger: trigger, Now: time.Now()})
	if err != nil || !add || len(plan.Orders) != 1 || plan.Orders[0].ReviewSeat != 2 || plan.Record.SeatCount != 2 || plan.Record.SeatCountSource != ReviewSeatSourceRisk || plan.Payload["source"] != ReviewSeatSourceRisk {
		t.Fatalf("plan=%+v add=%t err=%v", plan, add, err)
	}
	for name, in := range map[string]RiskSeatAdditionInputs{
		"AlreadyAtCount": {Task: task, Orders: []core.WorkOrder{seat(1, 1), seat(1, 2)}, Ratings: high, Trigger: trigger},
		"OlderRound":     {Task: task, Orders: []core.WorkOrder{seat(1, 1), seat(2, 1)}, Ratings: high, Trigger: trigger},
		"Settled": {Task: task, Orders: []core.WorkOrder{seat(1, 1)}, Ratings: high, Trigger: trigger,
			Events: []core.Event{{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 1})}}},
		"NoRound": {Task: task, Ratings: high, Trigger: trigger},
		"Record":  {Task: riskTask(riskpolicy.ModeRecord, false), Orders: []core.WorkOrder{seat(1, 1)}, Ratings: high, Trigger: trigger},
	} {
		if _, add, err := PlanRiskSeatAddition(in); err != nil || add {
			t.Fatalf("%s: add=%t err=%v, want no addition", name, add, err)
		}
	}
}
