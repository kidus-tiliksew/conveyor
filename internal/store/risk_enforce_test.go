package store

import (
	"errors"
	"strings"
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

// TestRiskRequirementsOnlyUnderEnforce covers the seat count and the merge
// hold (req-risk-based-review REQ-2, AC-2.1, REQ-7, REQ-8, AC-8.1, AC-8.2).
// The automatic merge refusal reads authors and trust too, so the dispatcher
// tests cover it.
func TestRiskRequirementsOnlyUnderEnforce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		task      core.Task
		ratings   []core.RiskRating
		seats     int
		required  int
		hold      bool
		automatic bool
	}{
		{"OffNeverHolds", core.Task{ID: "off"}, nil, 0, 0, false, false},
		{"RecordHighNeverHolds", riskTask(riskpolicy.ModeRecord, false), pathRated(riskpolicy.TierHigh, 3), 0, 0, false, false},
		{"EnforceUnratedIsLow", riskTask(riskpolicy.ModeEnforce, false), nil, 1, 0, false, false},
		{"EnforceLow", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierLow, 0), 1, 0, false, false},
		{"EnforceLowGateOn", riskTask(riskpolicy.ModeEnforce, true), pathRated(riskpolicy.TierLow, 0), 1, 1, true, true},
		{"EnforceMedium", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierMedium, 0), 1, 1, true, true},
		{"EnforcePathMinimum", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierLow, 2), 1, 2, true, true},
		{"EnforceHigh", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierHigh, 0), 2, 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RiskSeatCount(tc.task, tc.ratings); got != tc.seats {
				t.Fatalf("RiskSeatCount=%d, want %d", got, tc.seats)
			}
			if required, hold := RiskMergeHold(tc.task, tc.ratings); required != tc.required || hold != tc.hold {
				t.Fatalf("RiskMergeHold=%d,%t, want %d,%t", required, hold, tc.required, tc.hold)
			}
			if tc.hold != tc.automatic {
				t.Fatalf("hold=%t, automatic refusal expected=%t", tc.hold, tc.automatic)
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

// TestRiskApprovalsRequiredErrorWording names the count with its noun.
func TestRiskApprovalsRequiredErrorWording(t *testing.T) {
	one := (&RiskApprovalsRequiredError{Required: 1, Tier: riskpolicy.TierMedium, Automatic: true}).Error()
	two := (&RiskApprovalsRequiredError{Required: 2, Tier: riskpolicy.TierHigh, Automatic: true}).Error()
	if !strings.Contains(one, "requires 1 human approval for this medium-tier task") || strings.Contains(one, "approvals for") {
		t.Fatalf("one approval: %q", one)
	}
	if !strings.Contains(two, "requires 2 human approvals for this high-tier task") {
		t.Fatalf("two approvals: %q", two)
	}
}

// TestRiskAcceptanceHoldCountsAuthors covers the acceptance count of
// component-risk-review invariant 16 over invariant 19's authors: one
// approval for any untrusted or unknown author, the trust list and each
// author's capability read on every call, no author read when another floor
// already holds or the mode does not enforce, and a read error returned
// instead of a trusting answer (req-risk-based-review AC-8.2, AC-9.1 to
// AC-9.3, AC-1.1, AC-1.6).
func TestRiskAcceptanceHoldCountsAuthors(t *testing.T) {
	claimed := func(claimants ...string) []core.Event {
		var events []core.Event
		for _, claimant := range claimants {
			events = append(events, core.Event{Kind: "work_order.claimed", Payload: WorkOrderEventPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: claimant})})
		}
		return events
	}
	reads := func(events []core.Event, orders []core.WorkOrder, trusted []string, gates map[string]bool) (RiskAcceptanceReads, *int) {
		calls := 0
		return RiskAcceptanceReads{
			Claims:         func() ([]core.Event, []core.WorkOrder, error) { calls++; return events, orders, nil },
			TrustedAuthors: func() ([]string, error) { calls++; return trusted, nil },
			OperateGates:   func(userID string) (bool, error) { calls++; return gates[userID], nil },
		}, &calls
	}
	alice, bob := core.TaskRunClaimantID("usr-alice"), core.TaskRunClaimantID("usr-bob")
	for _, tc := range []struct {
		name     string
		task     core.Task
		ratings  []core.RiskRating
		events   []core.Event
		orders   []core.WorkOrder
		trusted  []string
		gates    map[string]bool
		required int
		reads    bool
	}{
		{"NoAuthorNoHold", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierLow, 0), nil, nil, nil, nil, 0, true},
		{"GatekeeperAuthorTrusted", riskTask(riskpolicy.ModeEnforce, false), nil, claimed(alice), nil, nil, map[string]bool{"usr-alice": true}, 0, true},
		{"AuthorWithoutGatesUntrusted", riskTask(riskpolicy.ModeEnforce, false), nil, claimed(alice), nil, nil, map[string]bool{}, 1, true},
		{"ListedAuthorTrusted", riskTask(riskpolicy.ModeEnforce, false), nil, claimed(alice), nil, []string{"usr-alice"}, nil, 0, true},
		{"UnlistedGatekeeperUntrusted", riskTask(riskpolicy.ModeEnforce, false), nil, claimed(alice), nil, []string{"usr-bob"}, map[string]bool{"usr-alice": true}, 1, true},
		{"CurrentOrderAuthorCounts", riskTask(riskpolicy.ModeEnforce, false), nil, claimed(alice), []core.WorkOrder{{Stage: core.StageImplement, ClaimantID: bob}}, []string{"usr-alice"}, nil, 1, true},
		{"UnknownAuthorUntrusted", riskTask(riskpolicy.ModeEnforce, false), nil, claimed("agent:session"), nil, []string{"usr-alice"}, nil, 1, true},
		{"TierFloorSkipsAuthors", riskTask(riskpolicy.ModeEnforce, false), pathRated(riskpolicy.TierHigh, 0), claimed(alice), nil, nil, nil, 2, false},
		{"GateFloorSkipsAuthors", riskTask(riskpolicy.ModeEnforce, true), nil, claimed(alice), nil, nil, nil, 1, false},
		{"RecordReadsNothing", riskTask(riskpolicy.ModeRecord, false), nil, claimed(alice), nil, nil, nil, 0, false},
		{"OffReadsNothing", core.Task{ID: "off"}, nil, claimed(alice), nil, nil, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, calls := reads(tc.events, tc.orders, tc.trusted, tc.gates)
			required, hold, err := RiskAcceptanceHold(tc.task, tc.ratings, in)
			if err != nil || required != tc.required || hold != (tc.required >= 1) {
				t.Fatalf("required=%d hold=%t err=%v, want %d", required, hold, err, tc.required)
			}
			if (*calls > 0) != tc.reads {
				t.Fatalf("reads=%d, want reads=%t", *calls, tc.reads)
			}
		})
	}
	t.Run("ReadErrorsFail", func(t *testing.T) {
		failed := errors.New("read failed")
		task := riskTask(riskpolicy.ModeEnforce, false)
		for name, in := range map[string]RiskAcceptanceReads{
			"Claims": {Claims: func() ([]core.Event, []core.WorkOrder, error) { return nil, nil, failed }},
			"TrustedAuthors": {Claims: func() ([]core.Event, []core.WorkOrder, error) { return claimed(alice), nil, nil },
				TrustedAuthors: func() ([]string, error) { return nil, failed }},
			"OperateGates": {Claims: func() ([]core.Event, []core.WorkOrder, error) { return claimed(alice), nil, nil },
				TrustedAuthors: func() ([]string, error) { return nil, nil }, OperateGates: func(string) (bool, error) { return false, failed }},
		} {
			if _, hold, err := RiskAcceptanceHold(task, nil, in); !errors.Is(err, failed) || hold {
				t.Fatalf("%s: hold=%t err=%v, want the read error", name, hold, err)
			}
		}
	})
}

// TestStoredTrustedAuthors reads one repository's list from a stored
// workspace document and refuses a document it cannot parse.
func TestStoredTrustedAuthors(t *testing.T) {
	data := []byte("repos:\n  - name: conveyor\n    trusted_authors: [usr-a, usr-b]\n  - name: app\n")
	if got, err := StoredTrustedAuthors(data, "conveyor"); err != nil || strings.Join(got, ",") != "usr-a,usr-b" {
		t.Fatalf("conveyor list=%v err=%v", got, err)
	}
	for _, repo := range []string{"app", "missing"} {
		if got, err := StoredTrustedAuthors(data, repo); err != nil || len(got) != 0 {
			t.Fatalf("%s list=%v err=%v, want empty", repo, got, err)
		}
	}
	if got, err := StoredTrustedAuthors(nil, "conveyor"); err != nil || len(got) != 0 {
		t.Fatalf("empty document list=%v err=%v", got, err)
	}
	if _, err := StoredTrustedAuthors([]byte("repos: [unterminated"), "conveyor"); err == nil {
		t.Fatal("a malformed document returned a list")
	}
}
