package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runRedispatchOfferParity proves on every backend that the statement's
// redispatch choice agrees with EnsureTaskEnqueued, the backend check behind
// the redispatch route on a queued task: a stage the backend holds is withheld
// with a refusal naming the holder, and an unheld queued task keeps the offer.
// A claim whose lease elapsed behind a later order holds until the order clock
// persists its expiry, though every read projects it as expired
// (req-operator-next-action AC-1.4; component-next-action, Invariants 7;
// component-durable-queue).
func runRedispatchOfferParity(t *testing.T, x Fixture) {
	if x.LineageHTTP == nil {
		t.Fatal("conformance fixture must provide LineageHTTP for the redispatch offer")
	}
	st, ctx := x.Backend, x.Context
	handler := x.LineageHTTP(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	newTask := func(next core.Stage) string {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: "Redispatch " + id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: next, CreatedAt: now}))
		return id
	}
	addOrder := func(order core.WorkOrder) {
		t.Helper()
		order.JobID = order.ID
		requireOK(t, st.CreateJob(ctx, core.Job{ID: order.ID, TaskID: order.TaskID, Stage: order.Stage, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, order))
	}
	addStale := func(taskID string) string {
		t.Helper()
		id := taskID + "-implement-1"
		addOrder(core.WorkOrder{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-48 * time.Hour), QueueDeadline: now.Add(-time.Minute)})
		return id
	}
	// requireParity compares the shared predicate over the orders a read returns
	// with the backend's own stored check.
	requireParity := func(taskID, holder string) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		refusal := core.NextActionRedispatchRefusal(task, orders)
		ensureErr := st.EnsureTaskEnqueued(ctx, taskID)
		if holder == "" {
			if refusal != "" || ensureErr != nil {
				t.Fatalf("task %s refusal=%q ensure=%v, want both to accept", taskID, refusal, ensureErr)
			}
			return
		}
		if !errors.Is(ensureErr, store.ErrTaskHeldByWorkOrder) || !strings.Contains(ensureErr.Error(), holder) {
			t.Fatalf("task %s ensure=%v, want held by %s", taskID, ensureErr, holder)
		}
		if !strings.Contains(refusal, core.TaskHeldByWorkOrderRefusal) || !strings.Contains(refusal, holder) {
			t.Fatalf("task %s refusal=%q, want held by %s", taskID, refusal, holder)
		}
	}
	redispatchRefusal := func(action nextaction.NextAction) string {
		for _, refusal := range action.Details.Refusals {
			if refusal.Choice == "redispatch" {
				return refusal.Text
			}
		}
		return ""
	}
	offered := func(action nextaction.NextAction) bool {
		for _, choice := range action.Choices {
			if choice.ID == "redispatch" {
				return true
			}
		}
		return false
	}

	// A stale order at the task's next stage holds it.
	held := newTask(core.StageImplement)
	heldOrder := addStale(held)
	action := claimFailureNextAction(t, handler, x.Workspace, held)
	if action.Details.StallReason != nextaction.StallStale || offered(action) || !strings.Contains(redispatchRefusal(action), heldOrder) {
		t.Fatalf("held statement stall=%q choices=%+v refusals=%+v", action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(held, heldOrder)

	// The same order behind a return to the plan stage holds nothing.
	moved := newTask(core.StageSpec)
	addStale(moved)
	action = claimFailureNextAction(t, handler, x.Workspace, moved)
	if action.Details.StallReason != nextaction.StallStale || !offered(action) || redispatchRefusal(action) != "" {
		t.Fatalf("unheld statement stall=%q choices=%+v refusals=%+v", action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(moved, "")

	// A claim behind a later review seat holds its stage, live or expired.
	lease := 100 * time.Millisecond
	claimed := newTask(core.StageImplement)
	claimedOrder := claimed + "-implement-1"
	addOrder(core.WorkOrder{ID: claimedOrder, TaskID: claimed, Stage: core.StageImplement, State: core.WorkOrderQueued})
	_, err := ClaimWorkOrder(ctx, st, claimedOrder, core.WorkOrderClaim{SessionID: "redispatch-offer-" + claimed, ClientToken: "redispatch-offer-token-" + claimed, Lease: lease, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	ops := taskops.New(st)
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	requireOK(t, err)
	addOrder(core.WorkOrder{ID: claimed + "-review-1-seat-1", TaskID: claimed, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderQueued})
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskDispatchStart})
	requireOK(t, err)
	_, err = ops.Perform(ctx, claimed, taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true})
	requireOK(t, err)
	time.Sleep(2 * lease)
	action = claimFailureNextAction(t, handler, x.Workspace, claimed)
	if action.Kind != nextaction.KindStalled || offered(action) || !strings.Contains(redispatchRefusal(action), claimedOrder) {
		t.Fatalf("expired claim statement %s/%s choices=%+v refusals=%+v", action.Kind, action.Details.StallReason, action.Choices, action.Details.Refusals)
	}
	requireParity(claimed, claimedOrder)

	// The persisted expiry ends the hold on the backend and in the predicate.
	_, err = ops.TickOrderClock(ctx, time.Now().UTC())
	requireOK(t, err)
	requireParity(claimed, "")
}

// redispatchGateEventCanary sits in each corrupt gate event, so a response that
// echoes the event is caught by value.
const redispatchGateEventCanary = "fake-redispatch-event-canary"

// redispatchGateLedger is everything the redispatch route could write for one
// task, as JSON so two reads compare by value.
func redispatchGateLedger(t *testing.T, x Fixture, taskID string) string {
	t.Helper()
	st, ctx := x.Backend, x.Context
	task, err := st.GetTask(ctx, taskID)
	requireOK(t, err)
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	jobs, err := st.ListJobs(ctx, taskID)
	requireOK(t, err)
	orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
	requireOK(t, err)
	interventions, err := st.ListInterventions(ctx, taskID)
	requireOK(t, err)
	raw, err := json.Marshal(map[string]any{"task": task, "events": events, "jobs": jobs, "orders": orders, "interventions": interventions})
	requireOK(t, err)
	return string(raw)
}

func redispatchGatePost(handler http.Handler, workspace, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
	request.Header.Set("X-Workspace-ID", workspace)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// requireNoRedispatchOffer proves the statement names neither the redispatch
// choice nor any choice on the redispatch route.
func requireNoRedispatchOffer(t *testing.T, action nextaction.NextAction) {
	t.Helper()
	for _, choice := range action.Choices {
		if choice.ID == "redispatch" || strings.HasSuffix(choice.Path, "/redispatch") {
			t.Fatalf("statement %s offers %+v at a pending plan-revision gate", action.Kind, choice)
		}
	}
}

// runRedispatchPlanRevisionGate proves through the real handler, on every
// backend, that POST /v1/tasks/{id}/redispatch refuses at a pending or
// unreadable plan-revision decision before any write, requests no dispatch,
// and accepts the same task once the decision is recorded. The statement offers
// no redispatch at those gates (component-work-orders invariant 39 and State
// and failure; req-operator-next-action AC-1.4; component-next-action
// invariant 7).
func runRedispatchPlanRevisionGate(t *testing.T, x Fixture, gate PlanRevisionGateFunc) {
	if x.RedispatchHTTP == nil {
		t.Fatal("conformance fixture must provide RedispatchHTTP for the plan-revision gate refusal")
	}
	if gate == nil {
		t.Fatal("factory must wire dispatch.PendingPlanRevisionGate")
	}
	st, ctx := x.Backend, x.Context
	var dispatched []string
	handler := x.RedispatchHTTP(t, func(_ context.Context, id string) { dispatched = append(dispatched, id) })
	redispatch := func(taskID string) *httptest.ResponseRecorder {
		return redispatchGatePost(handler, x.Workspace, "/v1/tasks/"+taskID+"/redispatch", `{}`)
	}
	// pendingGate parks one claimed order's task at the gate through the real
	// request command.
	pendingGate := func(t *testing.T) string {
		t.Helper()
		fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, true)
		_, err := requestPlanRevision(ctx, st, fixture.order.TaskID, fixture.order.ID, fixture.workerID, fixture.sessionID, "the API moved")
		requireOK(t, err)
		return fixture.order.TaskID
	}
	// requireRefused proves one refusal: its exact text, an unchanged ledger, and
	// no dispatch. The exact text is why no event or decode text can be present.
	requireRefused := func(t *testing.T, taskID, text string) {
		t.Helper()
		before := redispatchGateLedger(t, x, taskID)
		dispatchedBefore := len(dispatched)
		response := redispatch(taskID)
		if response.Code != http.StatusConflict || strings.TrimSpace(response.Body.String()) != text {
			t.Fatalf("redispatch status=%d body=%q, want 409 %q", response.Code, response.Body.String(), text)
		}
		if strings.Contains(response.Body.String(), redispatchGateEventCanary) {
			t.Fatalf("refusal echoes event text: %q", response.Body.String())
		}
		if after := redispatchGateLedger(t, x, taskID); after != before {
			t.Fatalf("refused redispatch wrote:\nbefore %s\nafter  %s", before, after)
		}
		if len(dispatched) != dispatchedBefore {
			t.Fatalf("refused redispatch requested dispatch: %v", dispatched[dispatchedBefore:])
		}
	}

	t.Run("pending gate refuses until the decision is recorded", func(t *testing.T) {
		taskID := pendingGate(t)
		action := claimFailureNextAction(t, handler, x.Workspace, taskID)
		if action.Kind != nextaction.KindPlanRevision {
			t.Fatalf("statement kind = %s, want %s", action.Kind, nextaction.KindPlanRevision)
		}
		requireNoRedispatchOffer(t, action)
		if pending, err := gate(ctx, st, taskID); !pending || err != nil {
			t.Fatalf("gate pending=%t err=%v, want pending", pending, err)
		}
		requireRefused(t, taskID, core.TaskRedispatchPlanRevisionRefusal)
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		if task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanRevision {
			t.Fatalf("refused task state=%s gate_kind=%s", task.State, task.GateKind)
		}

		// The control: the same task, with its decision recorded through the
		// real review route, is accepted and dispatched exactly once.
		decision := redispatchGatePost(handler, x.Workspace, "/v1/tasks/"+taskID+"/review", `{"action":"redirect","reason_code":"plan-revision-approved"}`)
		if decision.Code != http.StatusAccepted {
			t.Fatalf("decision status=%d body=%s", decision.Code, decision.Body.String())
		}
		task, err = st.GetTask(ctx, taskID)
		requireOK(t, err)
		if task.State != core.TaskQueued || task.NextStage != core.StageSpec || task.GateKind != "" {
			t.Fatalf("decided task state=%s next=%s gate_kind=%s", task.State, task.NextStage, task.GateKind)
		}
		dispatchedBefore := len(dispatched)
		response := redispatch(taskID)
		if response.Code != http.StatusAccepted {
			t.Fatalf("redispatch after the decision status=%d body=%s", response.Code, response.Body.String())
		}
		if got := dispatched[dispatchedBefore:]; len(got) != 1 || got[0] != taskID {
			t.Fatalf("redispatch after the decision dispatched %v, want %s once", got, taskID)
		}
	})

	t.Run("declined decision leaves the held answer", func(t *testing.T) {
		taskID := pendingGate(t)
		decision := redispatchGatePost(handler, x.Workspace, "/v1/tasks/"+taskID+"/review", `{"action":"redirect","reason_code":"plan-revision-declined","comment":"keep the approved plan"}`)
		if decision.Code != http.StatusAccepted {
			t.Fatalf("decision status=%d body=%s", decision.Code, decision.Body.String())
		}
		dispatchedBefore := len(dispatched)
		response := redispatch(taskID)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), core.TaskHeldByWorkOrderRefusal) {
			t.Fatalf("redispatch after a decline status=%d body=%q, want 409 held", response.Code, response.Body.String())
		}
		if len(dispatched) != dispatchedBefore {
			t.Fatalf("held redispatch requested dispatch: %v", dispatched[dispatchedBefore:])
		}
	})

	t.Run("task at no plan-revision gate stays accepted", func(t *testing.T) {
		now := time.Now().UTC()
		taskID := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: taskID, Workspace: x.Workspace, Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskAwaiting, NextStage: core.StageImplement, CreatedAt: now}))
		dispatchedBefore := len(dispatched)
		if response := redispatch(taskID); response.Code != http.StatusAccepted {
			t.Fatalf("redispatch status=%d body=%s", response.Code, response.Body.String())
		}
		if got := dispatched[dispatchedBefore:]; len(got) != 1 || got[0] != taskID {
			t.Fatalf("accepted redispatch dispatched %v, want %s once", got, taskID)
		}
	})

	// requireUnreadable proves the seeded gate is the unreadable kind before it
	// proves the refusal, so a fixture that stopped corrupting the gate fails.
	requireUnreadable := func(t *testing.T, taskID string, wantGate core.GateKind) {
		t.Helper()
		if pending, err := gate(ctx, st, taskID); !pending || !errors.Is(err, store.ErrPlanRevisionGateUnreadable) {
			t.Fatalf("gate pending=%t err=%v, want pending and ErrPlanRevisionGateUnreadable", pending, err)
		}
		requireNoRedispatchOffer(t, claimFailureNextAction(t, handler, x.Workspace, taskID))
		requireRefused(t, taskID, core.TaskRedispatchPlanRevisionUnreadableRefusal)
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		if task.State != core.TaskAwaiting || task.GateKind != wantGate {
			t.Fatalf("refused task state=%s gate_kind=%s", task.State, task.GateKind)
		}
	}

	t.Run("unreadable gate/undecodable transition", func(t *testing.T) {
		taskID := pendingGate(t)
		requireOK(t, st.AppendEvent(ctx, core.Event{
			TaskID: taskID, Kind: "task.state_changed", At: time.Now().UTC().Add(time.Second),
			Payload: json.RawMessage(`{"command":7,"detail":"` + redispatchGateEventCanary + `"}`),
		}))
		requireUnreadable(t, taskID, core.GateKindPlanRevision)
	})

	t.Run("unreadable gate/undecodable request", func(t *testing.T) {
		taskID := pendingGate(t)
		at := time.Now().UTC().Add(time.Second)
		requireOK(t, st.AppendEvent(ctx, core.Event{
			TaskID: taskID, Kind: "work_order.plan_revision_requested", At: at,
			Payload: json.RawMessage(`{"work_order_id":1,"rationale":"` + redispatchGateEventCanary + `"}`),
		}))
		requireOK(t, st.AppendEvent(ctx, core.Event{
			TaskID: taskID, Kind: "task.state_changed", At: at.Add(time.Millisecond),
			Payload: core.JSONPayload(map[string]any{"command": core.TaskGatePlanRevision, "from": core.TaskRunning, "to": core.TaskAwaiting}),
		}))
		requireUnreadable(t, taskID, core.GateKindPlanRevision)
	})

	t.Run("unreadable gate/missing request", func(t *testing.T) {
		now := time.Now().UTC()
		taskID := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{ID: taskID, Workspace: x.Workspace, Repo: "app", Branch: "conveyor/task-" + taskID, State: core.TaskAwaiting, NextStage: core.StageImplement, CreatedAt: now}))
		requireOK(t, st.AppendEvent(ctx, core.Event{
			TaskID: taskID, Kind: "task.state_changed", At: now,
			Payload: core.JSONPayload(map[string]any{"command": core.TaskGatePlanRevision, "from": core.TaskRunning, "to": core.TaskAwaiting}),
		}))
		requireUnreadable(t, taskID, "")
	})
}
