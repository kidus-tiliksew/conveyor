package dispatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func seats(value int) *int { return &value }

func seatOrders(round, count int) []core.WorkOrder {
	orders := make([]core.WorkOrder, 0, count)
	for seat := 1; seat <= count; seat++ {
		orders = append(orders, core.WorkOrder{ID: fmt.Sprintf("review-%d-seat-%d", round, seat), Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: round, ReviewSeat: seat})
	}
	return orders
}

func implementOrder(id string, state core.WorkOrderState, at time.Time, declared *int) core.WorkOrder {
	return core.WorkOrder{ID: id, Stage: core.StageImplement, State: state, CreatedAt: at, DeclaredReviewSeats: declared}
}

// TestResolveReviewRoundSeats reaches every RS-2 row through the shared input
// builder (feature-review-seat-minimum RS-2; DEC-77, DEC-78;
// req-review-gates-evidence AC-2.4).
func TestResolveReviewRoundSeats(t *testing.T) {
	base := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	superseded := core.Event{Kind: "task.setup.changed", Payload: core.JSONPayload(map[string]any{"review_transition": map[string]any{"superseded_work_order_ids": []string{"review-1-seat-3"}}})}
	for _, tc := range []struct {
		name      string
		setup     config.ExecutionSetup
		workspace int
		round     int
		orders    []core.WorkOrder
		events    []core.Event
		records   []store.ReviewRoundSeats
		planned   *int
		count     int
		source    string
		minimum   int
		floor     int
		impl      string
	}{
		{name: "Minimum", setup: config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}, round: 1, count: 2, source: store.ReviewSeatSourceMinimum, minimum: 2, floor: 2},
		{name: "ZeroSeatPolicy", setup: config.ExecutionSetup{Review: config.ReviewPanel{Seats: []config.ReviewSeat{}}}, workspace: 3, round: 1, count: 1, source: store.ReviewSeatSourceMinimum, minimum: 1, floor: 1},
		{name: "WorkspaceMinimumWithoutFrozenSeats", workspace: 3, round: 1, count: 3, source: store.ReviewSeatSourceMinimum, minimum: 3, floor: 3},
		{name: "Planned", workspace: 2, round: 1, planned: seats(4), count: 4, source: store.ReviewSeatSourcePlanned, minimum: 2, floor: 4},
		{name: "Declared", workspace: 2, round: 1, planned: seats(3),
			orders: []core.WorkOrder{implementOrder("impl-1", core.WorkOrderSubmitted, base, seats(5)), implementOrder("impl-newer-claim", core.WorkOrderClaimed, base.Add(time.Hour), seats(9))},
			count:  5, source: store.ReviewSeatSourceDeclared, minimum: 2, floor: 3, impl: "impl-1"},
		{name: "DeclaredAtFloor", workspace: 2, round: 1, planned: seats(3),
			orders: []core.WorkOrder{implementOrder("impl-1", core.WorkOrderCompleted, base, seats(3))},
			count:  3, source: store.ReviewSeatSourceDeclared, minimum: 2, floor: 3, impl: "impl-1"},
		{name: "DeclaredBelowFloorRejectedByBuilder", workspace: 2, round: 1,
			orders: []core.WorkOrder{implementOrder("impl-1", core.WorkOrderSubmitted, base, seats(1))},
			count:  1, source: store.ReviewSeatSourceDeclared, minimum: 2, floor: 2, impl: "impl-1"},
		{name: "DeclaredAlreadyUsed", workspace: 2, round: 3,
			orders:  append(append([]core.WorkOrder{implementOrder("impl-1", core.WorkOrderSubmitted, base, seats(5))}, seatOrders(1, 5)...), seatOrders(2, 4)...),
			records: []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 1, SeatCount: 5, SeatCountSource: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl-1"}, {TaskID: "task", ReviewRound: 2, SeatCount: 4, SeatCountSource: store.ReviewSeatSourceCarried, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl-1"}},
			count:   4, source: store.ReviewSeatSourceCarried, minimum: 2, floor: 2, impl: "impl-1"},
		{name: "RecordedCarry", workspace: 2, round: 2, orders: seatOrders(1, 3),
			records: []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 1, SeatCount: 3, SeatCountSource: store.ReviewSeatSourcePlanned, MinimumSeatCount: 2}},
			count:   3, source: store.ReviewSeatSourceCarried, minimum: 2, floor: 2},
		{name: "RecordedCarryIncludingAddedSeats", workspace: 2, round: 2, orders: seatOrders(1, 2),
			records: []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 1, SeatCount: 4, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2}},
			count:   4, source: store.ReviewSeatSourceCarried, minimum: 2, floor: 2},
		{name: "LegacyCarry", workspace: 2, round: 2, orders: seatOrders(1, 3), count: 3, source: store.ReviewSeatSourceCarried, minimum: 2, floor: 2},
		{name: "LegacySupersededSeatExcluded", workspace: 3, round: 2, orders: seatOrders(1, 3), events: []core.Event{superseded}, count: 3, source: store.ReviewSeatSourceMinimum, minimum: 3, floor: 3},
		{name: "PreviousBelowPlannedFloor", workspace: 2, round: 2, orders: seatOrders(1, 2), planned: seats(4),
			records: []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 1, SeatCount: 2, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2}},
			count:   4, source: store.ReviewSeatSourcePlanned, minimum: 2, floor: 4},
		{name: "PreviousBelowMinimum", setup: config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 3)}}, round: 2, orders: seatOrders(1, 1), count: 3, source: store.ReviewSeatSourceMinimum, minimum: 3, floor: 3},
		{name: "CarryAtFloor", workspace: 2, round: 2, orders: seatOrders(1, 4), planned: seats(4), count: 4, source: store.ReviewSeatSourceCarried, minimum: 2, floor: 4},
		{name: "NoPlanCount", workspace: 2, round: 2, orders: seatOrders(1, 1), count: 2, source: store.ReviewSeatSourceMinimum, minimum: 2, floor: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minimum := store.ReviewSeatMinimum(tc.setup, tc.workspace)
			got := ResolveReviewRoundSeats(store.ReviewRoundSeatInputsFor(minimum, tc.round, tc.orders, tc.events, tc.records, tc.planned))
			want := store.ReviewRoundSeatResolution{SeatCount: tc.count, Source: tc.source, MinimumSeatCount: tc.minimum, FloorSeatCount: tc.floor, ImplementationWorkOrderID: tc.impl}
			if got != want {
				t.Fatalf("resolution=%+v, want %+v", got, want)
			}
			cfg := &config.Config{WorkOrderQueueTimeout: time.Hour}
			_, orders, err := BuildReviewRound(cfg, core.Task{ID: "task"}, config.StageRoute{TimeoutText: "45m"}, tc.round, got.SeatCount, got.FloorSeatCount)
			if tc.name == "DeclaredBelowFloorRejectedByBuilder" {
				if err == nil || orders != nil {
					t.Fatalf("builder accepted a count below the floor: orders=%+v err=%v", orders, err)
				}
				return
			}
			if err != nil || len(orders) != tc.count {
				t.Fatalf("builder orders=%d err=%v, want %d", len(orders), err, tc.count)
			}
		})
	}
}

func TestBuildReviewRoundRejectsBelowFloor(t *testing.T) {
	cfg := &config.Config{WorkOrderQueueTimeout: time.Hour, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}}}
	for _, tc := range []struct{ count, floor int }{{1, 2}, {0, 0}, {2, 3}} {
		jobs, orders, err := BuildReviewRound(cfg, core.Task{ID: "floor"}, config.StageRoute{TimeoutText: "45m"}, 1, tc.count, tc.floor)
		if err == nil || jobs != nil || orders != nil {
			t.Fatalf("count=%d floor=%d built jobs=%+v orders=%+v err=%v", tc.count, tc.floor, jobs, orders, err)
		}
	}
}

func TestBuildReviewRoundBeyondConfiguredSeats(t *testing.T) {
	cfg := &config.Config{WorkOrderQueueTimeout: time.Hour, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Harness: "codex", Model: "pinned", Effort: "high"}}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "45m", Harness: "codex", Model: "route"}}}}
	task := core.Task{ID: "beyond", SetupContract: config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}}
	jobs, orders, err := BuildReviewRound(cfg, task, config.StageRoute{TimeoutText: "45m", Harness: "codex", Model: "route"}, 2, 3, 1)
	if err != nil || len(jobs) != 3 || len(orders) != 3 {
		t.Fatalf("jobs=%+v orders=%+v err=%v", jobs, orders, err)
	}
	for i, order := range orders {
		want := fmt.Sprintf("beyond-review-2-seat-%d", i+1)
		if order.ID != want || jobs[i].ID != want || order.ReviewSeat != i+1 || order.ReviewRound != 2 || order.ExecutionTimeoutText != "45m" {
			t.Fatalf("seat %d=%+v job=%+v", i+1, order, jobs[i])
		}
		if order.RequiredHarness != "" || order.RequiredModel != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil || order.DeclaredReviewSeats != nil {
			t.Fatalf("seat %d carries pins: %+v", i+1, order)
		}
	}
}

// TestDispatcherOpensResolvedReviewRound proves the dispatcher opens a round
// with its RS-2 resolved count, beyond the frozen single seat, and writes the
// round's record (feature-review-seat-minimum RS-2).
func TestDispatcherOpensResolvedReviewRound(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "dispatch-seats", Workspace: "demo", Repo: "api", Title: "Review seats", PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview,
		Branch: "conveyor/task-dispatch-seats", BaseBranch: "main", CreatedAt: time.Now(),
		SetupContract: config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}, ExecutionSettings: config.ContextualExecutionSettings{Review: config.ReviewExecutionSettings{TimeoutText: "45m"}}}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "plan", ReviewSeats: seats(3)})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "api", Base: "main"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "45m"}}}}
	if err = New(st, cfg, nil).DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewSeats := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == 1 {
			reviewSeats++
		}
	}
	records, err := st.ListReviewRoundSeats(ctx, task.ID)
	if err != nil || reviewSeats != 3 || len(records) != 1 || records[0].SeatCount != 3 || records[0].SeatCountSource != store.ReviewSeatSourcePlanned || records[0].MinimumSeatCount != 1 {
		t.Fatalf("review seats=%d records=%+v err=%v", reviewSeats, records, err)
	}
}
