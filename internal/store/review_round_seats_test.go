package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// TestReviewSeatAdditionValidation covers the RS-5 request checks: required
// and representable strings kept verbatim, the add bounds, and the replay
// identity of workspace-scoped request IDs (feature-review-seat-minimum RS-5).
func TestReviewSeatAdditionValidation(t *testing.T) {
	valid := store.ReviewSeatAdditionRequest{TaskID: "task", RequestID: "req-1", Reason: "  second opinion  ", Add: 1}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	longest := strings.Repeat("é", 255)
	if err := (store.ReviewSeatAdditionRequest{TaskID: "task", RequestID: longest, Reason: "r", Add: store.MaxReviewSeatCount}).Validate(); err != nil {
		t.Fatalf("255-character request ID and maximum add refused: %v", err)
	}
	for name, request := range map[string]store.ReviewSeatAdditionRequest{
		"NoTask":           {RequestID: "r", Reason: "r", Add: 1},
		"BlankRequestID":   {TaskID: "task", RequestID: " \t", Reason: "r", Add: 1},
		"BlankReason":      {TaskID: "task", RequestID: "r", Reason: "\n", Add: 1},
		"LongRequestID":    {TaskID: "task", RequestID: longest + "x", Reason: "r", Add: 1},
		"InvalidUTF8ID":    {TaskID: "task", RequestID: "r\xff", Reason: "r", Add: 1},
		"InvalidUTF8":      {TaskID: "task", RequestID: "r", Reason: "bad\xfe", Add: 1},
		"NULRequestID":     {TaskID: "task", RequestID: "r\x00", Reason: "r", Add: 1},
		"NULReason":        {TaskID: "task", RequestID: "r", Reason: "a\x00b", Add: 1},
		"ZeroAdd":          {TaskID: "task", RequestID: "r", Reason: "r", Add: 0},
		"NegativeAdd":      {TaskID: "task", RequestID: "r", Reason: "r", Add: -2},
		"UnrepresentedAdd": {TaskID: "task", RequestID: "r", Reason: "r", Add: store.MaxReviewSeatCount + 1},
	} {
		if err := request.Validate(); !errors.Is(err, store.ErrReviewSeatAdditionInvalid) {
			t.Errorf("%s: error=%v, want invalid", name, err)
		}
	}
	prior := store.ReviewSeatAddition{RequestID: "req-1", TaskID: "task", ReviewRound: 2, PriorSeatCount: 3, SeatCount: 4, Reason: valid.Reason, ActorID: "user:a"}
	got, err := store.ReviewSeatAdditionReplay(prior, valid)
	if err != nil || got != (store.ReviewSeatAdditionResult{RequestID: "req-1", TaskID: "task", ReviewRound: 2, PriorSeatCount: 3, SeatCount: 4}) {
		t.Fatalf("replay=%+v err=%v", got, err)
	}
	for name, changed := range map[string]store.ReviewSeatAdditionRequest{
		"Task":          {TaskID: "other", RequestID: "req-1", Reason: valid.Reason, Add: 1},
		"Reason":        {TaskID: "task", RequestID: "req-1", Reason: "second opinion", Add: 1},
		"Count":         {TaskID: "task", RequestID: "req-1", Reason: valid.Reason, Add: 2},
		"TrimmedReason": {TaskID: "task", RequestID: "req-1", Reason: strings.TrimSpace(valid.Reason), Add: 1},
	} {
		if _, err := store.ReviewSeatAdditionReplay(prior, changed); !errors.Is(err, store.ErrReviewSeatAdditionConflict) {
			t.Errorf("changed %s replay error=%v, want conflict", name, err)
		}
	}
	// The trusted service inputs are not part of the identity.
	trusted := valid
	trusted.WorkspaceReviewSeats, trusted.QueueTimeout = 9, time.Minute
	if _, err := store.ReviewSeatAdditionReplay(prior, trusted); err != nil {
		t.Fatalf("trusted inputs changed the identity: %v", err)
	}
}

// TestReviewSeatAdditionPlan covers the pure RS-5 planner: refusals for a
// missing, settled, superseded, or terminal round; seats k+1 through k+add
// with the round's metadata and no pins; the recorded and legacy records;
// the ledger row and event payload; and arithmetic overflow.
func TestReviewSeatAdditionPlan(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	task := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview, SetupContract: config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}}
	seat := func(round, number int) core.WorkOrder {
		id := fmt.Sprintf("task-review-%d-seat-%d", round, number)
		return core.WorkOrder{ID: id, JobID: id, TaskID: "task", Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: round, ReviewSeat: number,
			ReviewKind: "refresh", ReviewScope: "delta", BaselineSHA: "base", HeadSHA: "head", ExecutionTimeoutText: "45m",
			RequiredModel: "pinned-model", RequiredHarness: "pinned-harness", SessionID: "claimed-session"}
	}
	orders := []core.WorkOrder{seat(1, 1), seat(2, 2), seat(2, 1), {ID: "task-implement-1", TaskID: "task", Stage: core.StageImplement}}
	request := store.ReviewSeatAdditionRequest{TaskID: "task", RequestID: "req", Reason: "second opinion", Add: 2, WorkspaceReviewSeats: 5, QueueTimeout: time.Hour}
	recorded := []store.ReviewRoundSeats{{TaskID: "task", ReviewRound: 2, SeatCount: 2, SeatCountSource: store.ReviewSeatSourcePlanned, MinimumSeatCount: 2, ImplementationWorkOrderID: "task-implement-1", CreatedAt: now.Add(-time.Hour)}}
	in := store.ReviewSeatAdditionInputs{Workspace: "demo", Task: task, Orders: orders, Records: recorded, Request: request, ActorID: "user:operator", Now: now}

	plan, err := store.PlanReviewSeatAddition(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Result != (store.ReviewSeatAdditionResult{RequestID: "req", TaskID: "task", ReviewRound: 2, PriorSeatCount: 2, SeatCount: 4}) {
		t.Fatalf("result=%+v", plan.Result)
	}
	if len(plan.Jobs) != 2 || len(plan.Orders) != 2 {
		t.Fatalf("members jobs=%d orders=%d", len(plan.Jobs), len(plan.Orders))
	}
	for i, order := range plan.Orders {
		wantID := fmt.Sprintf("task-review-2-seat-%d", i+3)
		job := plan.Jobs[i]
		if job.ID != wantID || job.TaskID != "task" || job.Stage != core.StageReview || job.State != core.JobPending || job.Harness != "external-mcp" || job.Runner != "external" {
			t.Fatalf("job %d=%+v", i, job)
		}
		if order.ID != wantID || order.JobID != wantID || order.ReviewRound != 2 || order.ReviewSeat != i+3 || order.State != core.WorkOrderQueued || !order.Claimable ||
			order.ReviewKind != "refresh" || order.ReviewScope != "delta" || order.BaselineSHA != "base" || order.HeadSHA != "head" || order.ExecutionTimeoutText != "45m" {
			t.Fatalf("order %d=%+v", i, order)
		}
		if order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredHarnessConfig != nil || order.SessionID != "" || order.ServedRequirementSnapshot != nil || order.GovernanceSnapshot != nil {
			t.Fatalf("order %d carried a pin or claim state: %+v", i, order)
		}
		if !order.CreatedAt.Equal(now) || !order.QueueEnteredAt.Equal(now) || !order.QueueDeadline.Equal(now.Add(time.Hour)) {
			t.Fatalf("order %d clocks=%+v", i, order)
		}
	}
	if !plan.RecordExists || plan.Record != (store.ReviewRoundSeats{TaskID: "task", ReviewRound: 2, SeatCount: 4, SeatCountSource: store.ReviewSeatSourcePlanned, MinimumSeatCount: 2, ImplementationWorkOrderID: "task-implement-1", CreatedAt: now.Add(-time.Hour)}) {
		t.Fatalf("recorded round record=%+v exists=%t", plan.Record, plan.RecordExists)
	}
	if plan.Ledger != (store.ReviewSeatAddition{RequestID: "req", TaskID: "task", ReviewRound: 2, PriorSeatCount: 2, SeatCount: 4, Reason: "second opinion", ActorID: "user:operator", CreatedAt: now}) {
		t.Fatalf("ledger=%+v", plan.Ledger)
	}
	payload, _ := json.Marshal(plan.Payload)
	if string(payload) != `{"actor":"user:operator","prior_seat_count":2,"reason":"second opinion","request_id":"req","review_round":2,"seat_count":4,"task_id":"task","work_order_ids":["task-review-2-seat-3","task-review-2-seat-4"],"workspace_id":"demo"}` {
		t.Fatalf("payload=%s", payload)
	}
	if recorded[0].SeatCount != 2 {
		t.Fatal("planning mutated the input record")
	}

	t.Run("LegacyCarried", func(t *testing.T) {
		legacy := in
		legacy.Records, legacy.Task.SetupContract = nil, config.ExecutionSetup{}
		legacy.Request.QueueTimeout = 0
		plan, err := store.PlanReviewSeatAddition(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if plan.RecordExists || plan.Record != (store.ReviewRoundSeats{TaskID: "task", ReviewRound: 2, SeatCount: 4, SeatCountSource: store.ReviewSeatSourceCarried, MinimumSeatCount: 5, CreatedAt: now}) {
			t.Fatalf("legacy record=%+v exists=%t", plan.Record, plan.RecordExists)
		}
		if !plan.Orders[0].QueueDeadline.Equal(now.Add(config.DefaultWorkOrderQueueTimeout)) {
			t.Fatalf("default queue deadline=%s", plan.Orders[0].QueueDeadline)
		}
	})
	refusal := func(name string, mutate func(*store.ReviewSeatAdditionInputs), want error) {
		t.Run(name, func(t *testing.T) {
			changed := in
			changed.Orders = append([]core.WorkOrder(nil), in.Orders...)
			mutate(&changed)
			if _, err := store.PlanReviewSeatAddition(changed); !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
		})
	}
	refusal("NoRound", func(in *store.ReviewSeatAdditionInputs) { in.Orders = in.Orders[3:] }, store.ErrReviewRoundNotOpen)
	refusal("LegacyRoundZero", func(in *store.ReviewSeatAdditionInputs) {
		in.Orders = []core.WorkOrder{{ID: "task-review", TaskID: "task", Stage: core.StageReview}}
	}, store.ErrReviewRoundNotOpen)
	refusal("TerminalTask", func(in *store.ReviewSeatAdditionInputs) { in.Task.State = core.TaskClosed }, store.ErrReviewRoundNotOpen)
	refusal("Settled", func(in *store.ReviewSeatAdditionInputs) {
		in.Events = []core.Event{{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 2})}}
	}, store.ErrReviewRoundSettled)
	refusal("Superseded", func(in *store.ReviewSeatAdditionInputs) {
		in.Events = []core.Event{{Kind: "task.setup.changed", Payload: core.JSONPayload(map[string]any{"review_transition": map[string]any{"superseded_work_order_ids": []string{"task-review-2-seat-2"}}})}}
	}, store.ErrReviewRoundNotOpen)
	refusal("Overflow", func(in *store.ReviewSeatAdditionInputs) { in.Request.Add = store.MaxReviewSeatCount - 1 }, store.ErrReviewSeatAdditionInvalid)
	refusal("InvalidRequest", func(in *store.ReviewSeatAdditionInputs) { in.Request.Reason = " " }, store.ErrReviewSeatAdditionInvalid)
	t.Run("EarlierRoundSettlementDoesNotSettleLatest", func(t *testing.T) {
		earlier := in
		earlier.Events = []core.Event{{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 1})}}
		if _, err := store.PlanReviewSeatAddition(earlier); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("NonContiguousSeats", func(t *testing.T) {
		gap := in
		gap.Orders = []core.WorkOrder{seat(2, 1), seat(2, 3)}
		if _, err := store.PlanReviewSeatAddition(gap); err == nil || errors.Is(err, store.ErrReviewRoundNotOpen) {
			t.Fatalf("non-contiguous round error=%v", err)
		}
	})
}
