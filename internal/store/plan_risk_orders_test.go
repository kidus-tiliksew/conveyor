package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestPlanRiskMoveFor covers the plan risk effects of every plan-state move
// (req-risk-based-review AC-4.7, AC-4.9; feature-risk-based-review RR-13).
func TestPlanRiskMoveFor(t *testing.T) {
	create := PlanRiskMove{Create: true, Cancel: PlanRiskReplacedReason}
	cases := []struct {
		name     string
		from, to core.PlanState
		cause    core.PlanStateCause
		recorded bool
		want     PlanRiskMove
	}{
		{"submission", core.PlanNeeded, core.PlanAwaitingApproval, core.PlanCausePlanSubmitted, true, create},
		{"attach from not required", core.PlanNotRequired, core.PlanAwaitingApproval, core.PlanCausePlanAttached, false, create},
		{"version recorded while awaiting", core.PlanAwaitingApproval, core.PlanAwaitingApproval, core.PlanCausePlanSubmitted, true, create},
		{"no move and no version", core.PlanAwaitingApproval, core.PlanAwaitingApproval, core.PlanCausePlanSubmitted, false, PlanRiskMove{}},
		{"approval", core.PlanAwaitingApproval, core.PlanCompleted, core.PlanCausePlanApproved, false, PlanRiskMove{}},
		{"redirect", core.PlanAwaitingApproval, core.PlanNeeded, core.PlanCausePlanRedirected, false, PlanRiskMove{Cancel: PlanRiskRedirectedReason}},
		{"operator to needed", core.PlanAwaitingApproval, core.PlanNeeded, core.PlanCauseOperator, false, PlanRiskMove{Cancel: PlanRiskOperatorExitReason}},
		{"operator to not required", core.PlanAwaitingApproval, core.PlanNotRequired, core.PlanCauseOperator, false, PlanRiskMove{Cancel: PlanRiskOperatorExitReason}},
		{"revision from completed", core.PlanCompleted, core.PlanNeeded, core.PlanCausePlanRevisionApproved, false, PlanRiskMove{}},
		{"operator from completed", core.PlanCompleted, core.PlanNotRequired, core.PlanCauseOperator, false, PlanRiskMove{}},
	}
	for _, tc := range cases {
		if got := PlanRiskMoveFor(tc.from, tc.to, tc.cause, tc.recorded); got != tc.want {
			t.Errorf("%s: PlanRiskMoveFor = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestPlanRiskCancelIDsAndRefusal covers which open plan orders a move
// cancels and when an approval waits (req-risk-based-review AC-4.8, AC-4.9,
// AC-1.6; component-risk-review invariant 13).
func TestPlanRiskCancelIDsAndRefusal(t *testing.T) {
	order := func(id string, version int, head string, state core.WorkOrderState) core.RiskOrder {
		subject := core.RiskOrderSubject{PlanVersion: version}
		if head != "" {
			subject = core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base"}
		}
		return core.RiskOrder{Order: core.WorkOrder{ID: id, State: state}, Subject: subject}
	}
	orders := []core.RiskOrder{
		order("plan-1", 1, "", core.WorkOrderClaimed),
		order("plan-2", 2, "", core.WorkOrderQueued),
		order("plan-3", 3, "", core.WorkOrderCompleted),
		order("head-1", 0, "abc", core.WorkOrderQueued),
		order("plan-4", 4, "", core.WorkOrderTimedOut),
	}
	if got := PlanRiskCancelIDs(orders, 2); !reflect.DeepEqual(got, []string{"plan-1", "plan-4"}) {
		t.Fatalf("PlanRiskCancelIDs(keep 2) = %v", got)
	}
	if got := PlanRiskCancelIDs(orders, 0); !reflect.DeepEqual(got, []string{"plan-1", "plan-2", "plan-4"}) {
		t.Fatalf("PlanRiskCancelIDs(keep none) = %v", got)
	}
	task := func(mode riskpolicy.Mode) core.Task {
		return core.Task{ID: "t", SetupContract: config.PolicyContract{Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}}
	}
	for version, want := range map[int]string{1: "plan-1", 2: "plan-2", 4: "plan-4", 3: "", 5: ""} {
		err := PlanApprovalRiskRefusal(task(riskpolicy.ModeEnforce), orders, version)
		var refusal *PlanRiskRatingOpenError
		if want == "" {
			if err != nil {
				t.Fatalf("enforce version %d: %v, want no wait", version, err)
			}
			continue
		}
		if !errors.As(err, &refusal) || refusal.WorkOrderID != want || !errors.Is(err, ErrPlanRiskRatingOpen) {
			t.Fatalf("enforce version %d: %v, want a refusal naming %s", version, err, want)
		}
	}
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeOff} {
		if err := PlanApprovalRiskRefusal(task(mode), orders, 1); err != nil {
			t.Fatalf("%s: approval waited: %v", mode, err)
		}
	}
	if PlanRiskWorkOrderID(orders, 2) != "plan-2" || PlanRiskWorkOrderID(orders, 9) != "" {
		t.Fatal("PlanRiskWorkOrderID")
	}
}

// TestPlanRiskOrderRequest covers the need decision for a plan version
// (component-risk-review invariant 12; req-risk-based-review AC-1.1, AC-12.3).
func TestPlanRiskOrderRequest(t *testing.T) {
	task := core.Task{ID: "t", SetupContract: config.PolicyContract{Risk: &riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}}}
	request, ok := PlanRiskOrderRequest(task, nil, 2, core.Task{}.CreatedAt)
	if !ok || request.TaskID != "t" || request.Subject.PlanVersion != 2 {
		t.Fatalf("request=%+v ok=%t", request, ok)
	}
	high := []core.RiskRating{{Source: core.RiskSourcePerson, Tier: riskpolicy.TierHigh}}
	if _, ok := PlanRiskOrderRequest(task, high, 2, request.Now); ok {
		t.Fatal("a task at high got a plan risk order")
	}
	pendingHigh := []core.RiskRating{{Source: core.RiskSourceRiskAgent, Tier: riskpolicy.TierHigh, PlanVersion: 1, PlanPending: true}}
	if _, ok := PlanRiskOrderRequest(task, pendingHigh, 2, request.Now); !ok {
		t.Fatal("an unapproved version's rating decided the need")
	}
	if _, ok := PlanRiskOrderRequest(core.Task{ID: "off"}, nil, 1, request.Now); ok {
		t.Fatal("a task with no risk policy got a plan risk order")
	}
}
