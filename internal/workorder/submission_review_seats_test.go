package workorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// reviewSeatsFixture is one claimed implement order on a repository without a
// forge, served by the real submission service and dispatcher
// (feature-review-seat-minimum RS-4).
type reviewSeatsFixture struct {
	ctx        context.Context
	st         store.Backend
	task       core.Task
	cfg        *config.Config
	dispatcher *dispatch.Dispatcher
	service    *Service
	implement  string
}

const reviewSeatsSession = "rs4-implementer"

var reviewSeatsHead, reviewSeatsBase = strings.Repeat("a", 40), strings.Repeat("b", 40)

func intPtr(value int) *int { return &value }

// newReviewSeatsFixture freezes setup on the task (an absent seat list stays
// absent) and gives the workspace workspaceSeats configured review seats.
func newReviewSeatsFixture(t *testing.T, name string, setup config.ExecutionSetup, workspaceSeats int) *reviewSeatsFixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	task := core.Task{ID: "rs4-" + name, Workspace: "demo", Repo: "app", Title: "Review seats", Branch: "conveyor/task-rs4-" + name, BaseBranch: "main",
		PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: setup, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	// The forge-backed submission records this comparison; the fixture's
	// repository has no forge, so it supplies the trusted pair directly.
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": reviewSeatsBase, "head_sha": reviewSeatsHead})}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 5, WorkOrderQueueTimeout: time.Hour,
		Repos:  []config.Repo{{Name: "app", Base: "main", URL: "https://github.com/acme/app"}},
		Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, workspaceSeats)},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, TimeoutText: "1h"},
			"verify":    {Execution: config.ExecutionMCP, TimeoutText: "1h"},
		}}}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	f := &reviewSeatsFixture{ctx: ctx, st: st, task: task, cfg: cfg, dispatcher: d,
		service: &Service{Store: st, Dispatcher: d, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	f.claimImplement(t, job.ID)
	return f
}

func (f *reviewSeatsFixture) claimImplement(t *testing.T, id string) {
	t.Helper()
	if _, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, id, core.WorkOrderClaim{SessionID: reviewSeatsSession, ClientToken: "rs4-token", ClaimantID: core.TaskRunClaimantID("usr-rs4"), OwnerUserID: "usr-rs4", Agent: "codex", Model: "implementer", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.implement = id
}

// plan stores a plan version with count and approves it when approve is set.
func (f *reviewSeatsFixture) plan(t *testing.T, count *int, approve bool) {
	t.Helper()
	version, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: f.task.ID, Content: "plan", ReviewSeats: count})
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if err = f.st.ApproveSpecVersion(f.ctx, f.task.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *reviewSeatsFixture) submit(seats *int) (map[string]any, error) {
	return f.service.SubmitForReviewWithReviewSeats(f.ctx, f.implement, reviewSeatsSession, reviewSeatsHead, seats)
}

// reviewSeatsState is every durable surface a refused submission must leave
// unchanged.
type reviewSeatsState struct {
	Order   core.WorkOrder
	Jobs    []core.Job
	Task    core.Task
	Events  int
	Orders  int
	Records []store.ReviewRoundSeats
}

func (f *reviewSeatsFixture) state(t *testing.T) reviewSeatsState {
	t.Helper()
	order, err := f.st.GetWorkOrder(f.ctx, f.implement)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := f.st.ListJobs(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return reviewSeatsState{Order: order, Jobs: jobs, Task: task, Events: len(events), Orders: len(orders), Records: records}
}

// round returns the round's review seat orders in seat order and its record.
func (f *reviewSeatsFixture) round(t *testing.T, round int) ([]core.WorkOrder, store.ReviewRoundSeats) {
	t.Helper()
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	seats := []core.WorkOrder{}
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == round {
			seats = append(seats, order)
		}
	}
	for i, order := range seats {
		if order.ReviewSeat != i+1 || order.RequiredModel != "" || order.RequiredHarness != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
			t.Fatalf("round %d seat %d is not a contiguous content-free seat: %+v", round, i+1, order)
		}
	}
	records, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.ReviewRound == round {
			return seats, record
		}
	}
	t.Fatalf("round %d has no seat record: %+v", round, records)
	return nil, store.ReviewRoundSeats{}
}

// bounce requests changes on every seat of round and claims the follow-up
// implement order the settled round dispatches.
func (f *reviewSeatsFixture) bounce(t *testing.T, round int) {
	t.Helper()
	seats, _ := f.round(t, round)
	applicable, citable := false, false
	for _, seat := range seats {
		session := "rs4-review-" + seat.ID
		if _, err := f.service.Claim(f.ctx, seat.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "token-" + seat.ID, ClaimantID: "reviewer-" + seat.ID, OwnerUserID: "usr-reviewer", Agent: "codex", Model: "reviewer", Lease: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.SubmitVerdict(f.ctx, seat.ID, session, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "revise", Feedback: "add a test", GovernanceAssessment: &core.GovernanceAssessment{
			DesignApplicable: &applicable, DecisionCitable: &citable, CitedIDs: []string{}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders {
		if order.Stage == core.StageImplement && order.State == core.WorkOrderQueued {
			f.claimImplement(t, order.ID)
			return
		}
	}
	t.Fatalf("round %d bounce dispatched no implement order: %+v", round, orders)
}

// submissionPayload is the implement order's submitted work_order.updated
// payload.
func (f *reviewSeatsFixture) submissionPayload(t *testing.T, id string) map[string]json.RawMessage {
	t.Helper()
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != "work_order.updated" {
			continue
		}
		var payload map[string]json.RawMessage
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if string(payload["id"]) == `"`+id+`"` && string(payload["state"]) == `"submitted"` {
			return payload
		}
	}
	t.Fatalf("no submission event for %s", id)
	return nil
}

// TestSubmitForReviewReviewSeatsFloor proves floor admission: the floor is the
// larger of the RS-2 minimum and the newest approved plan count, equality is
// accepted, and a refusal is typed, carries both counts, and leaves every
// submission surface unchanged with the order still claimed
// (req-review-gates-evidence AC-2.3; DEC-78; feature-review-seat-minimum RS-4).
func TestSubmitForReviewReviewSeatsFloor(t *testing.T) {
	frozen := func(seats int) config.ExecutionSetup {
		return config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats)}}
	}
	for _, test := range []struct {
		name           string
		setup          config.ExecutionSetup
		workspaceSeats int
		plans          func(*testing.T, *reviewSeatsFixture)
		refused        int
		floor, minimum int
		planned        int
	}{
		{name: "frozen-minimum", setup: frozen(2), workspaceSeats: 4, refused: 1, floor: 2, minimum: 2},
		{name: "approved-plan-above-minimum", setup: frozen(1), plans: func(t *testing.T, f *reviewSeatsFixture) { f.plan(t, intPtr(3), true) }, refused: 2, floor: 3, minimum: 1, planned: 3},
		{name: "unapproved-draft-ignored", setup: frozen(1), plans: func(t *testing.T, f *reviewSeatsFixture) {
			f.plan(t, intPtr(3), true)
			f.plan(t, intPtr(5), false)
		}, refused: 2, floor: 3, minimum: 1, planned: 3},
		{name: "unapproved-lower-draft-ignored", setup: frozen(1), plans: func(t *testing.T, f *reviewSeatsFixture) {
			f.plan(t, intPtr(3), true)
			f.plan(t, intPtr(1), false)
		}, refused: 2, floor: 3, minimum: 1, planned: 3},
		{name: "approved-without-count-clears", setup: frozen(2), plans: func(t *testing.T, f *reviewSeatsFixture) {
			f.plan(t, intPtr(4), true)
			f.plan(t, nil, true)
		}, refused: 1, floor: 2, minimum: 2},
		{name: "absent-seat-list-uses-workspace", setup: config.ExecutionSetup{MaxBounces: 3}, workspaceSeats: 3, refused: 2, floor: 3, minimum: 3},
		{name: "frozen-empty-seat-list", setup: frozen(0), workspaceSeats: 3, plans: func(t *testing.T, f *reviewSeatsFixture) { f.plan(t, intPtr(2), true) }, refused: 1, floor: 2, minimum: 1, planned: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReviewSeatsFixture(t, test.name, test.setup, test.workspaceSeats)
			if test.plans != nil {
				test.plans(t, f)
			}
			before := f.state(t)
			_, err := f.submit(intPtr(test.refused))
			var refusal *ReviewSeatsBelowFloorError
			if !errors.As(err, &refusal) || refusal.ReviewSeats != test.refused || refusal.SeatFloor != test.floor || refusal.MinimumSeatCount != test.minimum || refusal.PlannedSeatCount != test.planned {
				t.Fatalf("refusal=%+v err=%v", refusal, err)
			}
			want := fmt.Sprintf("review_seats_below_floor: review_seats %d is below the task's review seat floor %d (minimum_seat_count=%d, planned_seat_count=%d)", test.refused, test.floor, test.minimum, test.planned)
			if err.Error() != want {
				t.Fatalf("refusal text=%q want %q", err.Error(), want)
			}
			if after := f.state(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed state:\nbefore=%+v\nafter=%+v", before, after)
			}
			if before.Order.State != core.WorkOrderClaimed || before.Order.SessionID != reviewSeatsSession {
				t.Fatalf("order is not claimed: %+v", before.Order)
			}
			// The same claim corrects the count: equality with the floor is
			// accepted and persisted.
			result, err := f.submit(intPtr(test.floor))
			if err != nil || result["review_seats"] != test.floor || result["review_seats_applied"] != true {
				t.Fatalf("floor submission=%+v err=%v", result, err)
			}
			order, err := f.st.GetWorkOrder(f.ctx, f.implement)
			if err != nil || order.State != core.WorkOrderSubmitted || order.DeclaredReviewSeats == nil || *order.DeclaredReviewSeats != test.floor {
				t.Fatalf("submitted order=%+v err=%v", order, err)
			}
		})
	}
}

// TestSubmitForReviewReviewSeatsOpenAndCarry proves a declared count opens
// exactly that many seats in the next round, later rounds without a new
// declaration carry it, and an explicit declaration may choose the floor
// below a larger previous round (feature-review-seat-minimum RS-4, RS-2).
func TestSubmitForReviewReviewSeatsOpenAndCarry(t *testing.T) {
	f := newReviewSeatsFixture(t, "open-carry", config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}, 0)
	first := f.implement
	result, err := f.submit(intPtr(3))
	if err != nil || result["review_seats"] != 3 || result["review_seats_applied"] != true || result["await_review"] != true {
		t.Fatalf("declared submission=%+v err=%v", result, err)
	}
	if payload := f.submissionPayload(t, first); string(payload["declared_review_seats"]) != "3" || payload["review_seats_applied"] != nil {
		t.Fatalf("submission payload=%s", core.JSONPayload(payload))
	}
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	seats, record := f.round(t, 1)
	if len(seats) != 3 || record.SeatCount != 3 || record.SeatCountSource != store.ReviewSeatSourceDeclared || record.ImplementationWorkOrderID != first || record.MinimumSeatCount != 1 {
		t.Fatalf("round 1 seats=%d record=%+v", len(seats), record)
	}

	// A follow-up submission without a declaration carries round 1's count.
	f.bounce(t, 1)
	second := f.implement
	result, err = f.submit(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["review_seats"]; ok {
		t.Fatalf("omitted declaration changed the result shape: %+v", result)
	}
	if payload := f.submissionPayload(t, second); payload["declared_review_seats"] != nil || payload["review_seats"] != nil {
		t.Fatalf("omitted declaration changed the payload: %s", core.JSONPayload(payload))
	}
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	seats, record = f.round(t, 2)
	if len(seats) != 3 || record.SeatCount != 3 || record.SeatCountSource != store.ReviewSeatSourceCarried || record.ImplementationWorkOrderID != second {
		t.Fatalf("round 2 seats=%d record=%+v", len(seats), record)
	}

	// The previous round's larger count is a default, not a floor: an
	// explicit declaration at the floor opens one seat.
	f.bounce(t, 2)
	third := f.implement
	if result, err = f.submit(intPtr(1)); err != nil || result["review_seats"] != 1 {
		t.Fatalf("floor declaration=%+v err=%v", result, err)
	}
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	seats, record = f.round(t, 3)
	if len(seats) != 1 || record.SeatCount != 1 || record.SeatCountSource != store.ReviewSeatSourceDeclared || record.ImplementationWorkOrderID != third {
		t.Fatalf("round 3 seats=%d record=%+v", len(seats), record)
	}
}

// TestSubmitForReviewReviewSeatsOmittedKeepsDefaults proves an omitted count
// keeps RS-2 default resolution and today's result and event shapes.
func TestSubmitForReviewReviewSeatsOmittedKeepsDefaults(t *testing.T) {
	f := newReviewSeatsFixture(t, "omitted", config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}, 0)
	result, err := f.service.SubmitForReview(f.ctx, f.implement, reviewSeatsSession, reviewSeatsHead)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["review_seats"]; ok {
		t.Fatalf("result=%+v", result)
	}
	if _, ok := result["review_seats_applied"]; ok {
		t.Fatalf("result=%+v", result)
	}
	order, err := f.st.GetWorkOrder(f.ctx, f.implement)
	if err != nil || order.DeclaredReviewSeats != nil {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	seats, record := f.round(t, 1)
	if len(seats) != 2 || record.SeatCountSource != store.ReviewSeatSourceMinimum {
		t.Fatalf("seats=%d record=%+v", len(seats), record)
	}
}

// TestSubmitForReviewReviewSeatsAfterVerify proves that with verify_stage on a
// declaration opens no round at submission; after successful sealed
// verification the dispatcher opens the declared seats from the submitting
// implement order, not the verify order.
func TestSubmitForReviewReviewSeatsAfterVerify(t *testing.T) {
	f := newReviewSeatsFixture(t, "after-verify", config.ExecutionSetup{VerifyStage: true, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}, 0)
	implement := f.implement
	if _, err := f.st.BootstrapWorkspaceConfig(f.ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	result, err := f.submit(intPtr(2))
	if err != nil || result["next_stage"] != core.StageVerify || result["review_seats"] != 2 || result["review_seats_applied"] != true {
		t.Fatalf("submission=%+v err=%v", result, err)
	}
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var verify core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview {
			t.Fatalf("review opened before verification: %+v", order)
		}
		if order.Stage == core.StageVerify {
			verify = order
		}
	}
	if verify.ID == "" {
		t.Fatalf("verify order missing: %+v", orders)
	}
	if records, _ := f.st.ListReviewRoundSeats(f.ctx, f.task.ID); len(records) != 0 {
		t.Fatalf("records before verification: %+v", records)
	}
	claim := core.WorkOrderClaim{WorkerID: "verifier", ClaimantID: "verifier", SessionID: "rs4-verify", ClientToken: "rs4-verify-token", Lease: time.Hour, ExecutionTimeout: time.Hour,
		Requirements: []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Observed"}}}}}}}
	verify, err = storetest.ClaimWorkOrder(f.ctx, f.st, verify.ID, claim)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx := store.WithActor(f.ctx, store.Actor{ID: "worker:verifier", Role: core.ActorWorker})
	raw, err := json.Marshal(VerificationPrepareRequest{RequestKey: "rs4-prepare"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.service.Verification(workerCtx, verify.ID, claim.SessionID, claim.ClientToken, "prepare_verification", raw)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := prepared.(store.VerificationSnapshot)
	access := store.VerificationAccess{TaskID: f.task.ID, WorkOrderID: verify.ID, WorkOrderAttemptID: verify.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}}
	storetest.SealEmptyVerificationFixture(t, workerCtx, f.st, access, snapshot.Contexts[0], "AC-1.1")
	if err = f.dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	seats, record := f.round(t, 1)
	if len(seats) != 2 || record.SeatCount != 2 || record.SeatCountSource != store.ReviewSeatSourceDeclared || record.ImplementationWorkOrderID != implement {
		t.Fatalf("post-verify seats=%d record=%+v", len(seats), record)
	}
}

// TestSubmitForReviewReviewSeatsRefreshUnused proves a submission completing a
// dispatched refresh admits its count against the floor, keeps the existing
// round and seats, records the count unused, returns review_seats_applied
// false on submission and replay, and leaves no declaration a later round can
// consume.
func TestSubmitForReviewReviewSeatsRefreshUnused(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{RefreshReview: config.RefreshReviewDelta})
	f.dispatchExistingRefresh(t, "fix-head", config.RefreshReviewDelta)
	plan, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: f.task.ID, Content: "plan", ReviewSeats: intPtr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.ApproveSpecVersion(f.ctx, f.task.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	refreshOrders, _ := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	refreshSeats := 0
	for _, order := range refreshOrders {
		if order.Stage == core.StageReview && order.ReviewKind == "refresh" {
			refreshSeats++
		}
	}
	recordsBefore, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := f.eventCount(t)
	var refusal *ReviewSeatsBelowFloorError
	if _, err = f.service.SubmitForReviewWithReviewSeats(f.ctx, f.order.ID, "fixer", "fix-head", intPtr(2)); !errors.As(err, &refusal) || refusal.SeatFloor != 3 || refusal.PlannedSeatCount != 3 {
		t.Fatalf("below-floor refresh completion err=%v", err)
	}
	f.assertUntouched(t, events, core.TaskQueued)
	result, err := f.service.SubmitForReviewWithReviewSeats(f.ctx, f.order.ID, "fixer", "fix-head", intPtr(4))
	if err != nil || result["refresh_already_dispatched"] != true || result["review_seats"] != 4 || result["review_seats_applied"] != false {
		t.Fatalf("refresh completion=%+v err=%v", result, err)
	}
	order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
	if err != nil || order.State != core.WorkOrderSubmitted || order.DeclaredReviewSeats != nil {
		t.Fatalf("refresh order=%+v err=%v", order, err)
	}
	eventsAfter, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range eventsAfter {
		var payload map[string]json.RawMessage
		if event.Kind != "work_order.updated" || json.Unmarshal(event.Payload, &payload) != nil || string(payload["id"]) != `"`+f.order.ID+`"` || string(payload["state"]) != `"submitted"` {
			continue
		}
		found = string(payload["review_seats"]) == "4" && string(payload["review_seats_applied"]) == "false" && payload["declared_review_seats"] == nil
	}
	if !found {
		t.Fatal("refresh completion did not record its count unused")
	}
	orders, _ := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	after := 0
	for _, candidate := range orders {
		if candidate.Stage == core.StageReview && candidate.ReviewKind == "refresh" {
			after++
		}
	}
	recordsAfter, err := f.st.ListReviewRoundSeats(f.ctx, f.task.ID)
	if err != nil || after != refreshSeats || !reflect.DeepEqual(recordsBefore, recordsAfter) {
		t.Fatalf("refresh seats %d -> %d records %+v -> %+v err=%v", refreshSeats, after, recordsBefore, recordsAfter, err)
	}
	// RS-2 resolution sees no declaration for this order.
	inputs, err := store.LoadReviewRoundSeatInputs(f.ctx, f.st, f.task.ID, 1, 99)
	if err != nil || inputs.ImplementationWorkOrderID != f.order.ID || inputs.DeclaredSeatCount != nil {
		t.Fatalf("resolver inputs=%+v err=%v", inputs, err)
	}
	// The replay reports the recorded unused count, ignores a new count,
	// and writes nothing.
	events = f.eventCount(t)
	replayed, err := f.service.SubmitForReviewWithReviewSeats(f.ctx, f.order.ID, "fixer", "fix-head", intPtr(9))
	if err != nil || replayed["replayed"] != true || replayed["review_seats"] != 4 || replayed["review_seats_applied"] != false || f.eventCount(t) != events {
		t.Fatalf("refresh replay=%+v err=%v", replayed, err)
	}
}

// TestSubmitForReviewReviewSeatsReplay proves a same-session, same-head
// replay is read-only: it reports the recorded declaration, ignores a
// different supplied count, and never rewrites it; changed-head and
// foreign-session replays stay refused.
func TestSubmitForReviewReviewSeatsReplay(t *testing.T) {
	f := newReviewSeatsFixture(t, "replay", config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}, 0)
	if _, err := f.submit(intPtr(2)); err != nil {
		t.Fatal(err)
	}
	before := f.state(t)
	for _, supplied := range []*int{nil, intPtr(2), intPtr(5)} {
		result, err := f.submit(supplied)
		if err != nil || result["replayed"] != true || result["review_seats"] != 2 || result["review_seats_applied"] != true {
			t.Fatalf("replay with %v=%+v err=%v", supplied, result, err)
		}
	}
	if after := f.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed state:\nbefore=%+v\nafter=%+v", before, after)
	}
	if _, err := f.service.SubmitForReviewWithReviewSeats(f.ctx, f.implement, reviewSeatsSession, "other-head", intPtr(5)); err == nil || !strings.Contains(err.Error(), "only that head replays") {
		t.Fatalf("changed-head replay err=%v", err)
	}
	if _, err := f.service.SubmitForReviewWithReviewSeats(f.ctx, f.implement, "foreign-session", reviewSeatsHead, intPtr(5)); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("foreign-session replay err=%v", err)
	}
	if after := f.state(t); !reflect.DeepEqual(before, after) {
		t.Fatal("refused replays changed state")
	}
}
