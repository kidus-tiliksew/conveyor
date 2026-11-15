package core

import "testing"

// A review round holds the stage through any of its seats, not only the
// latest one: a stale seat 1 keeps round 1 held after seat 2 was cancelled,
// while a finished round, or a round behind a later stage, holds nothing
// (component-work-orders "Stages and order minting"; DEC-62).
func TestWorkOrderBlocksStageMintReviewRound(t *testing.T) {
	seat := func(id string, round int, state WorkOrderState) WorkOrder {
		return WorkOrder{ID: id, Stage: StageReview, ReviewRound: round, State: state}
	}
	for _, tc := range []struct {
		name   string
		orders []WorkOrder
		holder string
	}{
		{"stale seat behind cancelled latest seat", []WorkOrder{seat("r1s1", 1, WorkOrderStale), seat("r1s2", 1, WorkOrderCancelled)}, "r1s1"},
		{"timed-out seat behind submitted latest seat", []WorkOrder{seat("r1s1", 1, WorkOrderTimedOut), seat("r1s2", 1, WorkOrderSubmitted)}, "r1s1"},
		{"earlier round does not hold", []WorkOrder{seat("r1s1", 1, WorkOrderStale), seat("r2s1", 2, WorkOrderCompleted)}, ""},
		{"finished round", []WorkOrder{seat("r1s1", 1, WorkOrderCompleted), seat("r1s2", 1, WorkOrderCancelled)}, ""},
		{"later stage", []WorkOrder{seat("r1s1", 1, WorkOrderStale), {ID: "impl", Stage: StageImplement, State: WorkOrderQueued}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, held := WorkOrderBlocksStageMint(tc.orders, StageReview)
			if held != (tc.holder != "") || holder.ID != tc.holder {
				t.Fatalf("holder=%q held=%v want %q", holder.ID, held, tc.holder)
			}
		})
	}
}

// WorkOrderHoldsStage keeps stored claims exclusive across non-review stage
// history, preferring the newest claimed order, and delegates projected-state
// admission for review and non-claimed states.
func TestWorkOrderHoldsStage(t *testing.T) {
	orders := func(state WorkOrderState, stage Stage) []WorkOrder {
		return []WorkOrder{
			{ID: "order-1", Stage: stage, State: state},
			{ID: "review-1", Stage: StageReview, ReviewRound: 1, State: WorkOrderCompleted},
		}
	}
	for _, tc := range []struct {
		name   string
		orders []WorkOrder
		stage  Stage
		holder string
	}{
		{"claimed history holds", orders(WorkOrderClaimed, StageImplement), StageImplement, "order-1"},
		{"newest claimed history wins", []WorkOrder{
			{ID: "older", Stage: StageImplement, State: WorkOrderClaimed},
			{ID: "newer", Stage: StageImplement, State: WorkOrderClaimed},
			{ID: "review", Stage: StageReview, ReviewRound: 1, State: WorkOrderCompleted},
		}, StageImplement, "newer"},
		{"queued history does not hold", orders(WorkOrderQueued, StageImplement), StageImplement, ""},
		{"stale history does not hold", orders(WorkOrderStale, StageImplement), StageImplement, ""},
		{"timed-out history does not hold", orders(WorkOrderTimedOut, StageImplement), StageImplement, ""},
		{"claimed review history delegates to round predicate", orders(WorkOrderClaimed, StageReview), StageReview, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, held := WorkOrderHoldsStage(tc.orders, tc.stage)
			if held != (tc.holder != "") || holder.ID != tc.holder {
				t.Fatalf("holder=%q held=%v want %q", holder.ID, held, tc.holder)
			}
		})
	}
}

// A risk order is created beside a task's other orders and never counts as
// the latest order: a later risk order leaves a held verify order or review
// round held, a genuine later stage still releases the hold, and a risk-only
// history holds no stage (component-work-orders invariants 2 and 62;
// DEC-135(10)).
func TestWorkOrderBlocksStageMintIgnoresRiskOrders(t *testing.T) {
	risk := func(id string, state WorkOrderState) WorkOrder {
		return WorkOrder{ID: id, Stage: StageRisk, State: state}
	}
	seat := func(id string, round int, state WorkOrderState) WorkOrder {
		return WorkOrder{ID: id, Stage: StageReview, ReviewRound: round, State: state}
	}
	verify := func(id string, state WorkOrderState) WorkOrder {
		return WorkOrder{ID: id, Stage: StageVerify, State: state}
	}
	for _, tc := range []struct {
		name   string
		orders []WorkOrder
		stage  Stage
		holder string
	}{
		{"timed-out review seat behind a head risk order", []WorkOrder{seat("r1s1", 1, WorkOrderTimedOut), risk("risk-1", WorkOrderQueued)}, StageReview, "r1s1"},
		{"queued review seat behind a claimed risk order", []WorkOrder{seat("r1s1", 1, WorkOrderQueued), risk("risk-1", WorkOrderClaimed)}, StageReview, "r1s1"},
		{"review round behind several risk orders", []WorkOrder{seat("r1s1", 1, WorkOrderStale), seat("r1s2", 1, WorkOrderSubmitted), risk("risk-1", WorkOrderCancelled), risk("risk-2", WorkOrderQueued)}, StageReview, "r1s1"},
		{"timed-out verify order behind a risk order", []WorkOrder{verify("verify-1", WorkOrderTimedOut), risk("risk-1", WorkOrderQueued)}, StageVerify, "verify-1"},
		{"queued verify order behind a completed risk order", []WorkOrder{verify("verify-1", WorkOrderQueued), risk("risk-1", WorkOrderCompleted)}, StageVerify, "verify-1"},
		{"finished review round behind a risk order", []WorkOrder{seat("r1s1", 1, WorkOrderCompleted), risk("risk-1", WorkOrderQueued)}, StageReview, ""},
		{"later stage still releases behind a risk order", []WorkOrder{verify("verify-1", WorkOrderTimedOut), risk("risk-1", WorkOrderQueued), {ID: "impl", Stage: StageImplement, State: WorkOrderQueued}, risk("risk-2", WorkOrderQueued)}, StageVerify, ""},
		{"later review stage releases a verify order behind a risk order", []WorkOrder{verify("verify-1", WorkOrderTimedOut), seat("r1s1", 1, WorkOrderQueued), risk("risk-1", WorkOrderQueued)}, StageVerify, ""},
		{"risk-only history holds no stage", []WorkOrder{risk("risk-1", WorkOrderQueued), risk("risk-2", WorkOrderClaimed)}, StageRisk, ""},
		{"risk-only history holds no review stage", []WorkOrder{risk("risk-1", WorkOrderQueued)}, StageReview, ""},
		{"risk order holds no risk stage behind a verify order", []WorkOrder{verify("verify-1", WorkOrderCompleted), risk("risk-1", WorkOrderQueued)}, StageRisk, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, held := WorkOrderBlocksStageMint(tc.orders, tc.stage)
			if held != (tc.holder != "") || holder.ID != tc.holder {
				t.Fatalf("holder=%q held=%v want %q", holder.ID, held, tc.holder)
			}
		})
	}
	// WorkOrderHoldsStage delegates to the same rule for review and for a
	// non-claimed verify order.
	if holder, held := WorkOrderHoldsStage([]WorkOrder{seat("r1s1", 1, WorkOrderTimedOut), risk("risk-1", WorkOrderQueued)}, StageReview); !held || holder.ID != "r1s1" {
		t.Fatalf("review holder=%q held=%v", holder.ID, held)
	}
	if holder, held := WorkOrderHoldsStage([]WorkOrder{verify("verify-1", WorkOrderTimedOut), risk("risk-1", WorkOrderQueued)}, StageVerify); !held || holder.ID != "verify-1" {
		t.Fatalf("verify holder=%q held=%v", holder.ID, held)
	}
}
