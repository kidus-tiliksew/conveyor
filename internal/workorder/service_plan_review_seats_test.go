package workorder

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// These tests prove RS-3 of feature-review-seat-minimum at the submit_plan
// service boundary: a planned review seat count below the task's minimum is
// refused with the plan order left claimed and nothing written, an accepted
// count is stored on the plan version through approval, and the latest
// approved plan's count is the plan's contribution to the seat floor
// (req-review-gates-evidence AC-2.8, AC-2.3, AC-2.4; DEC-78).

const planSeatsMarkdown = "## Approach\nPlan the panel.\n\n## Files touched\n- internal/workorder/service.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The planned count persists."

type planSeatsEnv struct {
	ctx     context.Context
	st      store.Store
	cfg     *config.Config
	d       *dispatch.Dispatcher
	service *Service
	taskID  string
	claims  int
}

// newPlanSeatsEnv creates a queued plan-stage task with the given frozen
// setup and workspace seat list, and dispatches its first MCP plan order.
func newPlanSeatsEnv(t *testing.T, id string, gate bool, setup config.ExecutionSetup, workspaceSeats int) *planSeatsEnv {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, PolicyVersion: 1, SpecApproval: gate,
		State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now().UTC(), SetupContract: setup}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, workspaceSeats)},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Model: "planner", Execution: config.ExecutionMCP, Timeout: time.Hour},
			"implement": {Model: "implementer", Execution: config.ExecutionMCP, Timeout: time.Hour},
		}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}}}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	env := &planSeatsEnv{ctx: ctx, st: st, cfg: cfg, d: d, taskID: id,
		service: &Service{Store: st, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}}
	if err := d.DispatchNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	return env
}

// claimPlan claims the task's queued plan order under a fresh session.
func (e *planSeatsEnv) claimPlan(t *testing.T) core.WorkOrder {
	t.Helper()
	return e.claimStage(t, core.StageSpec)
}

func (e *planSeatsEnv) claimStage(t *testing.T, stage core.Stage) core.WorkOrder {
	t.Helper()
	orders, err := e.st.ListTaskWorkOrders(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].Stage != stage || orders[i].State != core.WorkOrderQueued {
			continue
		}
		e.claims++
		session := fmt.Sprintf("%s-session-%d", stage, e.claims)
		claimed, claimErr := storetest.For(e.st).ClaimWorkOrder(e.ctx, orders[i].ID, core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", WorkerID: fmt.Sprintf("%s-worker-%d", e.taskID, e.claims), Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		return claimed
	}
	t.Fatalf("no queued %s order in %+v", stage, orders)
	return core.WorkOrder{}
}

func (e *planSeatsEnv) submit(order core.WorkOrder, seats *int) (map[string]any, error) {
	return e.service.SubmitPlan(e.ctx, order.ID, order.SessionID, pipeline.StructuredPlan{Markdown: planSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}, ReviewSeats: seats})
}

// approve records the operator's plan approval through the real gate path.
func (e *planSeatsEnv) approve(t *testing.T) {
	t.Helper()
	e.intervene(t, core.Intervention{Action: core.InterventionApprove, ReasonCode: "plan-approved"})
}

func (e *planSeatsEnv) intervene(t *testing.T, decision core.Intervention) {
	t.Helper()
	current, err := e.st.GetTask(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	latest, _, err := e.st.GetLatestJob(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	decision.TaskID, decision.JobID = e.taskID, latest.ID
	if err = e.d.HandleIntervention(e.ctx, current, latest, decision); err != nil {
		t.Fatal(err)
	}
	if err = e.st.CreateIntervention(e.ctx, decision); err != nil {
		t.Fatal(err)
	}
	if err = e.d.DispatchNow(e.ctx, e.taskID); err != nil {
		t.Fatal(err)
	}
}

// reenterPlan takes an approved plan back to the plan stage through the
// unchanged plan-revision lifecycle: the implementation order requests a
// revision and the operator approves it.
func (e *planSeatsEnv) reenterPlan(t *testing.T) core.WorkOrder {
	t.Helper()
	// Automatic approval advances to implementation without dispatching;
	// redelivery reuses an order the gate approval already dispatched.
	if err := e.d.DispatchNow(e.ctx, e.taskID); err != nil {
		t.Fatal(err)
	}
	implement := e.claimStage(t, core.StageImplement)
	if _, err := taskops.ExecuteWorkOrder(e.ctx, e.st, e.taskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		return e.st.RequestPlanRevisionCommand(e.ctx, lease, implement.ID, core.WorkOrderClaimIdentity{WorkerID: implement.WorkerID, ClaimantID: implement.ClaimantID, SessionID: implement.SessionID}, "the panel needs revising")
	}); err != nil {
		t.Fatal(err)
	}
	e.intervene(t, core.Intervention{Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionApprovedReasonCode, Comment: "Revise the plan."})
	return e.claimPlan(t)
}

type planSeatsSnapshot struct {
	task    core.Task
	order   core.WorkOrder
	jobs    []core.Job
	events  int
	latest  int
	hasPlan bool
}

func (e *planSeatsEnv) snapshot(t *testing.T, orderID string) planSeatsSnapshot {
	t.Helper()
	task, err := e.st.GetTask(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	order, err := e.st.GetWorkOrder(e.ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := e.st.ListJobs(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := e.st.ListEvents(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := e.st.GetLatestSpecVersion(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	return planSeatsSnapshot{task: task, order: order, jobs: jobs, events: len(events), latest: latest.Version, hasPlan: ok}
}

// requireRefusedUnchanged asserts a refused submission wrote nothing and left
// the plan order claimed by the same session.
func (e *planSeatsEnv) requireRefusedUnchanged(t *testing.T, before planSeatsSnapshot, order core.WorkOrder) {
	t.Helper()
	after := e.snapshot(t, order.ID)
	if after.order.State != core.WorkOrderClaimed || after.order.SessionID != order.SessionID || after.order.UpdatedAt != before.order.UpdatedAt {
		t.Fatalf("refusal changed the claimed order: before=%+v after=%+v", before.order, after.order)
	}
	if !reflect.DeepEqual(after.task, before.task) {
		t.Fatalf("refusal changed the task: before=%+v after=%+v", before.task, after.task)
	}
	if after.events != before.events || after.latest != before.latest || after.hasPlan != before.hasPlan || len(after.jobs) != len(before.jobs) {
		t.Fatalf("refusal wrote lifecycle state: before=%+v after=%+v", before, after)
	}
	for i := range after.jobs {
		if after.jobs[i].State != before.jobs[i].State {
			t.Fatalf("refusal changed job %s: %s -> %s", after.jobs[i].ID, before.jobs[i].State, after.jobs[i].State)
		}
	}
}

func requireBelowMinimum(t *testing.T, err error, seats, minimum int) {
	t.Helper()
	var below *dispatch.PlanReviewSeatsBelowMinimumError
	if !errors.As(err, &below) || below.ReviewSeats != seats || below.MinimumSeatCount != minimum {
		t.Fatalf("review_seats %d error=%v, want below minimum %d", seats, err, minimum)
	}
	for _, want := range []string{"review_seats_below_minimum", fmt.Sprintf("minimum_seat_count=%d", minimum)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q lacks %q", err.Error(), want)
		}
	}
}

func (e *planSeatsEnv) requireStored(t *testing.T, result map[string]any, version int, seats *int, approved bool) {
	t.Helper()
	if result["version"] != version || result["approved"] != approved {
		t.Fatalf("submit_plan result=%+v, want version %d approved %t", result, version, approved)
	}
	stored, ok, err := e.st.GetSpecVersion(e.ctx, e.taskID, version)
	if err != nil || !ok || !sameSeats(stored.ReviewSeats, seats) || stored.Approved != approved {
		t.Fatalf("plan version %d=%+v ok=%t err=%v, want review_seats %v approved %t", version, stored, ok, err, describeSeats(seats), approved)
	}
}

// requireFloor asserts the approved plan contribution through the RS-2
// resolver for the first review round.
func (e *planSeatsEnv) requireFloor(t *testing.T, approvedVersion int, planned *int, floor int, source string) {
	t.Helper()
	approved, ok, err := e.st.GetApprovedSpecVersion(e.ctx, e.taskID)
	if err != nil || !ok || approved.Version != approvedVersion || !sameSeats(approved.ReviewSeats, planned) {
		t.Fatalf("approved plan=%+v ok=%t err=%v, want version %d review_seats %v", approved, ok, err, approvedVersion, describeSeats(planned))
	}
	task, err := e.st.GetTask(e.ctx, e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	seats, err := dispatch.ResolveTaskReviewRoundSeats(e.ctx, e.st, e.cfg, task, 1)
	if err != nil || seats.FloorSeatCount != floor || seats.SeatCount != floor || seats.Source != source {
		t.Fatalf("resolution=%+v err=%v, want floor %d source %s", seats, err, floor, source)
	}
}

func sameSeats(got, want *int) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

func describeSeats(value *int) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprint(*value)
}

func planSeats(value int) *int { return &value }

func frozenSeats(count int) config.ExecutionSetup {
	return config.ExecutionSetup{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, count)}}
}

// TestSubmitPlanReviewSeatsMinimum covers the minimum for a frozen, frozen
// empty, and absent seat list with a nontrivial workspace fallback: a count
// below it is refused with the minimum in the error and no mutation; equal,
// larger, and omitted counts are accepted and stored as stated.
func TestSubmitPlanReviewSeatsMinimum(t *testing.T) {
	for _, tc := range []struct {
		name           string
		setup          config.ExecutionSetup
		workspaceSeats int
		minimum        int
	}{
		{name: "frozen", setup: frozenSeats(3), workspaceSeats: 1, minimum: 3},
		{name: "frozen empty", setup: config.ExecutionSetup{Review: config.ReviewPanel{Seats: []config.ReviewSeat{}}}, workspaceSeats: 4, minimum: 1},
		{name: "absent uses workspace", workspaceSeats: 3, minimum: 3},
		{name: "absent zero workspace", workspaceSeats: 0, minimum: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			below := []int{0}
			if tc.minimum > 1 {
				below = append(below, tc.minimum-1)
			}
			for _, count := range below {
				env := newPlanSeatsEnv(t, "plan-seats-below", true, tc.setup, tc.workspaceSeats)
				order := env.claimPlan(t)
				before := env.snapshot(t, order.ID)
				_, err := env.submit(order, planSeats(count))
				if count < 1 {
					if err == nil || !strings.Contains(err.Error(), "review_seats must be at least 1") {
						t.Fatalf("review_seats %d error=%v", count, err)
					}
				} else {
					requireBelowMinimum(t, err, count, tc.minimum)
				}
				env.requireRefusedUnchanged(t, before, order)
			}
			for _, seats := range []*int{planSeats(tc.minimum), planSeats(tc.minimum + 2), nil} {
				env := newPlanSeatsEnv(t, "plan-seats-accepted", true, tc.setup, tc.workspaceSeats)
				order := env.claimPlan(t)
				result, err := env.submit(order, seats)
				if err != nil {
					t.Fatalf("review_seats %s: %v", describeSeats(seats), err)
				}
				env.requireStored(t, result, 1, seats, false)
				current, _ := env.st.GetTask(env.ctx, env.taskID)
				completed, _ := env.st.GetWorkOrder(env.ctx, order.ID)
				if current.State != core.TaskAwaiting || completed.State != core.WorkOrderCompleted {
					t.Fatalf("accepted plan task=%+v order=%+v", current, completed)
				}
			}
		})
	}
}

// TestSubmitPlanReviewSeatsCorrectionRetainsClaim proves a refused count
// leaves the same claim usable for a corrected submission, and that a stated
// count requires the workspace configuration while an omitted one does not.
func TestSubmitPlanReviewSeatsCorrectionRetainsClaim(t *testing.T) {
	env := newPlanSeatsEnv(t, "plan-seats-correction", true, config.ExecutionSetup{}, 2)
	order := env.claimPlan(t)
	before := env.snapshot(t, order.ID)
	_, err := env.submit(order, planSeats(1))
	requireBelowMinimum(t, err, 1, 2)
	env.requireRefusedUnchanged(t, before, order)

	provider := env.service.ConfigProvider
	env.service.ConfigProvider = nil
	if _, err = env.submit(order, planSeats(3)); err == nil || !strings.Contains(err.Error(), "work-order config unavailable") {
		t.Fatalf("count without configuration error=%v", err)
	}
	env.requireRefusedUnchanged(t, before, order)
	env.service.ConfigProvider = provider

	result, err := env.submit(order, planSeats(3))
	if err != nil {
		t.Fatal(err)
	}
	env.requireStored(t, result, 1, planSeats(3), false)

	omitted := newPlanSeatsEnv(t, "plan-seats-no-config", true, config.ExecutionSetup{}, 2)
	omitted.service.ConfigProvider = nil
	result, err = omitted.submit(omitted.claimPlan(t), nil)
	if err != nil {
		t.Fatalf("omitted count needed configuration: %v", err)
	}
	omitted.requireStored(t, result, 1, nil, false)
}

// TestSubmitPlanReviewSeatsRevision proves approval keeps the stated count, a
// newer unapproved version never displaces the approved contribution, a later
// plan may lower the count to another valid value or clear it by omission
// once approved, and validation compares with the minimum rather than the
// previous plan's count, with the plan gate on and off.
func TestSubmitPlanReviewSeatsRevision(t *testing.T) {
	t.Run("gate on", func(t *testing.T) {
		env := newPlanSeatsEnv(t, "plan-seats-revision-gated", true, frozenSeats(2), 1)
		result, err := env.submit(env.claimPlan(t), planSeats(4))
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 1, planSeats(4), false)
		if _, ok, _ := env.st.GetApprovedSpecVersion(env.ctx, env.taskID); ok {
			t.Fatal("an unapproved plan became the approved version")
		}
		env.approve(t)
		env.requireStored(t, map[string]any{"version": 1, "approved": true}, 1, planSeats(4), true)
		env.requireFloor(t, 1, planSeats(4), 4, store.ReviewSeatSourcePlanned)

		result, err = env.submit(env.reenterPlan(t), planSeats(3))
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 2, planSeats(3), false)
		env.requireFloor(t, 1, planSeats(4), 4, store.ReviewSeatSourcePlanned)
		env.approve(t)
		env.requireFloor(t, 2, planSeats(3), 3, store.ReviewSeatSourcePlanned)

		result, err = env.submit(env.reenterPlan(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 3, nil, false)
		env.requireFloor(t, 2, planSeats(3), 3, store.ReviewSeatSourcePlanned)
		env.approve(t)
		env.requireFloor(t, 3, nil, 2, store.ReviewSeatSourceMinimum)
	})
	t.Run("gate off", func(t *testing.T) {
		env := newPlanSeatsEnv(t, "plan-seats-revision-automatic", false, frozenSeats(2), 1)
		result, err := env.submit(env.claimPlan(t), planSeats(4))
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 1, planSeats(4), true)
		env.requireFloor(t, 1, planSeats(4), 4, store.ReviewSeatSourcePlanned)

		result, err = env.submit(env.reenterPlan(t), planSeats(2))
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 2, planSeats(2), true)
		env.requireFloor(t, 2, planSeats(2), 2, store.ReviewSeatSourceMinimum)

		result, err = env.submit(env.reenterPlan(t), planSeats(5))
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 3, planSeats(5), true)
		env.requireFloor(t, 3, planSeats(5), 5, store.ReviewSeatSourcePlanned)

		order := env.reenterPlan(t)
		before := env.snapshot(t, order.ID)
		_, err = env.submit(order, planSeats(1))
		requireBelowMinimum(t, err, 1, 2)
		env.requireRefusedUnchanged(t, before, order)
		result, err = env.submit(order, nil)
		if err != nil {
			t.Fatal(err)
		}
		env.requireStored(t, result, 4, nil, true)
		env.requireFloor(t, 4, nil, 2, store.ReviewSeatSourceMinimum)
	})
}
