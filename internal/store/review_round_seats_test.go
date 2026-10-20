package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func seatValue(value int) *int { return &value }

// TestReviewRoundSeatsDispatchParity proves the dispatch function forwards to
// the one shared resolution the policy planner uses (feature-review-seat-minimum
// RS-2): there is no second copy of the table.
func TestReviewRoundSeatsDispatchParity(t *testing.T) {
	for _, in := range []store.ReviewRoundSeatInputs{
		{Round: 1, MinimumSeatCount: 0},
		{Round: 1, MinimumSeatCount: 2, PlannedSeatCount: seatValue(3)},
		{Round: 2, MinimumSeatCount: 2, PreviousRound: 1, PreviousSeatCount: 4, PreviousRecorded: true},
		{Round: 2, MinimumSeatCount: 2, PreviousRound: 1, PreviousSeatCount: 1, PlannedSeatCount: seatValue(2)},
		{Round: 3, MinimumSeatCount: 1, ImplementationWorkOrderID: "impl", DeclaredSeatCount: seatValue(5)},
		{Round: 3, MinimumSeatCount: 1, ImplementationWorkOrderID: "impl", DeclaredSeatCount: seatValue(5), DeclarationConsumed: true, PreviousRound: 2, PreviousSeatCount: 5},
	} {
		if shared, forwarded := store.ResolveReviewRoundSeats(in), dispatch.ResolveReviewRoundSeats(in); shared != forwarded {
			t.Fatalf("inputs %+v: store=%+v dispatch=%+v", in, shared, forwarded)
		}
	}
}

// TestReviewRoundSeatsCheck covers the in-transaction revalidation each
// round-building command runs before it writes.
func TestReviewRoundSeatsCheck(t *testing.T) {
	frozenTwo := config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}
	used := []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 1, SeatCount: 4, SeatCountSource: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl-used"}}
	for _, tc := range []struct {
		name    string
		seats   store.ReviewRoundSeatResolution
		setup   config.ExecutionSetup
		planned *int
		orders  int
		ok      bool
	}{
		{"Minimum", store.ReviewRoundSeatResolution{SeatCount: 2, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2}, frozenTwo, nil, 2, true},
		{"WorkspaceMinimumTrusted", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 3}, config.ExecutionSetup{}, nil, 3, true},
		{"FrozenMinimumMismatch", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 3}, frozenTwo, nil, 3, false},
		{"OrderCountMismatch", store.ReviewRoundSeatResolution{SeatCount: 2, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2}, frozenTwo, nil, 1, false},
		{"UnknownSource", store.ReviewRoundSeatResolution{SeatCount: 2, Source: "workspace", MinimumSeatCount: 2}, frozenTwo, nil, 2, false},
		{"BelowPlannedFloor", store.ReviewRoundSeatResolution{SeatCount: 2, Source: store.ReviewSeatSourceCarried, MinimumSeatCount: 2}, frozenTwo, seatValue(3), 2, false},
		{"CarriedAtFloor", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceCarried, MinimumSeatCount: 2}, frozenTwo, seatValue(3), 3, true},
		{"PlannedMatches", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourcePlanned, MinimumSeatCount: 2}, frozenTwo, seatValue(3), 3, true},
		{"PlannedWithoutPlan", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourcePlanned, MinimumSeatCount: 2}, frozenTwo, nil, 3, false},
		{"MinimumAbovePlannedFloorSource", store.ReviewRoundSeatResolution{SeatCount: 3, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 2}, frozenTwo, seatValue(3), 3, false},
		{"DeclaredFresh", store.ReviewRoundSeatResolution{SeatCount: 4, Source: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl-fresh"}, frozenTwo, nil, 4, true},
		{"DeclaredAlreadyUsed", store.ReviewRoundSeatResolution{SeatCount: 4, Source: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2, ImplementationWorkOrderID: "impl-used"}, frozenTwo, nil, 4, false},
		{"DeclaredWithoutImplementation", store.ReviewRoundSeatResolution{SeatCount: 4, Source: store.ReviewSeatSourceDeclared, MinimumSeatCount: 2}, frozenTwo, nil, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.CheckReviewRoundSeats(tc.seats, tc.setup, tc.planned, used, tc.orders)
			if tc.ok != (err == nil) || (err != nil && !errors.Is(err, store.ErrReviewRoundSeatsInvalid)) {
				t.Fatalf("check error=%v, want ok=%t", err, tc.ok)
			}
		})
	}
	for _, record := range []store.ReviewRoundSeats{
		{ReviewRound: 1, SeatCount: 1, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1},
		{TaskID: "task", ReviewRound: 0, SeatCount: 1, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1},
		{TaskID: "task", ReviewRound: 1, SeatCount: 0, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1},
		{TaskID: "task", ReviewRound: 1, SeatCount: 1, SeatCountSource: store.ReviewSeatSourceMinimum, MinimumSeatCount: 0},
		{TaskID: "task", ReviewRound: 1, SeatCount: 1, SeatCountSource: "legacy", MinimumSeatCount: 1},
	} {
		if err := record.Validate(); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
			t.Fatalf("record %+v validated: %v", record, err)
		}
	}
}

// TestReviewRoundSeatsMemoryCopies proves the memory store copies nullable
// seat counts on write and on read, so a caller cannot mutate stored state.
func TestReviewRoundSeatsMemoryCopies(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemoryWithConfig(&config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", Base: "main"}}})
	task := core.Task{ID: "copies", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	input := seatValue(3)
	created, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "plan", ReviewSeats: input})
	if err != nil {
		t.Fatal(err)
	}
	*input = 9
	*created.ReviewSeats = 8
	if err = st.ApproveSpecVersion(ctx, task.ID, created.Version); err != nil {
		t.Fatal(err)
	}
	read, ok, err := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || !ok || read.ReviewSeats == nil || *read.ReviewSeats != 3 {
		t.Fatalf("approved plan=%+v ok=%t err=%v", read, ok, err)
	}
	*read.ReviewSeats = 7
	for _, get := range []func() (core.SpecVersion, bool, error){
		func() (core.SpecVersion, bool, error) { return st.GetApprovedSpecVersion(ctx, task.ID) },
		func() (core.SpecVersion, bool, error) { return st.GetLatestSpecVersion(ctx, task.ID) },
		func() (core.SpecVersion, bool, error) { return st.GetSpecVersion(ctx, task.ID, created.Version) },
	} {
		again, ok, err := get()
		if err != nil || !ok || again.ReviewSeats == nil || *again.ReviewSeats != 3 {
			t.Fatalf("stored planned count changed through a returned value: %+v", again)
		}
	}
	// A caller-supplied declared count never reaches a stored order.
	order := core.WorkOrder{ID: "copies-implement-1", JobID: "copies-implement-1", TaskID: task.ID, Stage: core.StageImplement, DeclaredReviewSeats: seatValue(4)}
	if err = st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.CreateWorkOrder(ctx, st, order); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || stored.DeclaredReviewSeats != nil {
		t.Fatalf("creation stored a declared count: %+v err=%v", stored, err)
	}
}
