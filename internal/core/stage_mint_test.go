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

// WorkOrderHoldsStage keeps stored claims exclusive across stage history but
// delegates non-claimed holding states to the projected-state mint predicate.
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
		{"queued history does not hold", orders(WorkOrderQueued, StageImplement), StageImplement, ""},
		{"stale history does not hold", orders(WorkOrderStale, StageImplement), StageImplement, ""},
		{"timed-out history does not hold", orders(WorkOrderTimedOut, StageImplement), StageImplement, ""},
		{"claimed review history holds", orders(WorkOrderClaimed, StageReview), StageReview, "order-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, held := WorkOrderHoldsStage(tc.orders, tc.stage)
			if held != (tc.holder != "") || holder.ID != tc.holder {
				t.Fatalf("holder=%q held=%v want %q", holder.ID, held, tc.holder)
			}
		})
	}
}
