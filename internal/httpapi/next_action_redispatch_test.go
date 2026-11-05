package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// redispatchHarness reads the next action and posts to the redispatch route
// through the real handler over a memory store.
type redispatchHarness struct {
	t          *testing.T
	ctx        context.Context
	st         store.Store
	handler    http.Handler
	dispatched chan string
}

func newRedispatchHarness(t *testing.T) *redispatchHarness {
	t.Helper()
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	dispatched := make(chan string, 8)
	server.OnCreate = func(_ context.Context, id string) { dispatched <- id }
	return &redispatchHarness{t: t, ctx: store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo"), st: st, handler: server.Handler(), dispatched: dispatched}
}

func (h *redispatchHarness) do(method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	var body *bytes.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte(`{}`))
	} else {
		body = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Workspace-ID", "demo")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func (h *redispatchHarness) nextAction(id string) nextaction.NextAction {
	h.t.Helper()
	response := h.do(http.MethodGet, "/v1/tasks/"+id+"/next-action")
	var action nextaction.NextAction
	if err := json.Unmarshal(response.Body.Bytes(), &action); err != nil || response.Code != http.StatusOK {
		h.t.Fatalf("next action status=%d err=%v body=%s", response.Code, err, response.Body.String())
	}
	return action
}

func (h *redispatchHarness) task(task core.Task) {
	h.t.Helper()
	task.Workspace, task.Title = "demo", task.ID
	if err := h.st.CreateTask(h.ctx, task); err != nil {
		h.t.Fatal(err)
	}
}

func (h *redispatchHarness) order(order core.WorkOrder) {
	h.t.Helper()
	order.JobID = order.ID
	if err := h.st.CreateJob(h.ctx, core.Job{ID: order.ID, TaskID: order.TaskID, Stage: order.Stage, State: core.JobPending}); err != nil {
		h.t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(h.ctx, h.st, order); err != nil {
		h.t.Fatal(err)
	}
}

// redispatchChoice returns the one choice naming the task-scoped redispatch
// route, or false when the statement offers none.
func redispatchChoice(t *testing.T, action nextaction.NextAction) (nextaction.Choice, bool) {
	t.Helper()
	var found []nextaction.Choice
	for _, choice := range action.Choices {
		if strings.HasSuffix(choice.Path, "/redispatch") {
			found = append(found, choice)
		}
	}
	if len(found) > 1 {
		t.Fatalf("choices = %+v, want at most one on the redispatch route", action.Choices)
	}
	if len(found) == 0 {
		return nextaction.Choice{}, false
	}
	return found[0], true
}

// requireAccepted posts the statement's offered redispatch choice exactly as a
// client does, and proves the route accepts it and dispatches the task.
func (h *redispatchHarness) requireAccepted(id string, action nextaction.NextAction, choiceID string) {
	h.t.Helper()
	choice, ok := redispatchChoice(h.t, action)
	if !ok || choice.ID != choiceID {
		h.t.Fatalf("offered %+v, want %s (refusals %+v)", action.Choices, choiceID, action.Details.Refusals)
	}
	response := h.do(choice.Method, choice.Path)
	if response.Code != http.StatusAccepted {
		h.t.Fatalf("POST %s status=%d body=%s", choice.Path, response.Code, response.Body.String())
	}
	select {
	case got := <-h.dispatched:
		if got != id {
			h.t.Fatalf("dispatched %q, want %q", got, id)
		}
	default:
		h.t.Fatal("accepted redispatch dispatched nothing")
	}
	task, err := h.st.GetTask(h.ctx, id)
	if err != nil || task.State != core.TaskQueued {
		h.t.Fatalf("task after redispatch = %+v err=%v", task, err)
	}
}

// requireRefused proves the choice is withheld with a recorded refusal, and
// that the route it would have named answers 409 with that same text.
func (h *redispatchHarness) requireRefused(id string, action nextaction.NextAction, choiceID string) {
	h.t.Helper()
	if choice, ok := redispatchChoice(h.t, action); ok {
		h.t.Fatalf("offered %+v, want it withheld", choice)
	}
	text := ""
	for _, refusal := range action.Details.Refusals {
		if refusal.Choice == choiceID {
			text = refusal.Text
		}
	}
	if text == "" {
		h.t.Fatalf("refusals = %+v, want %s refused", action.Details.Refusals, choiceID)
	}
	response := h.do(http.MethodPost, "/v1/tasks/"+id+"/redispatch")
	if response.Code != http.StatusConflict {
		h.t.Fatalf("route status=%d body=%s, want 409 (refusal %q)", response.Code, response.Body.String(), text)
	}
	if got := strings.TrimSpace(response.Body.String()); got != text {
		h.t.Fatalf("route answered %q, refusal says %q", got, text)
	}
	select {
	case got := <-h.dispatched:
		h.t.Fatalf("refused redispatch dispatched %q", got)
	default:
	}
}

// stalledOrder seeds one stored order that reads as each stall reason.
func stalledOrder(t *testing.T, h *redispatchHarness, reason, taskID string, stage core.Stage) {
	t.Helper()
	now := time.Now().UTC()
	order := core.WorkOrder{ID: taskID + "-" + string(stage) + "-1", TaskID: taskID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(time.Hour)}
	switch reason {
	case nextaction.StallAutomaticRecoveryExhausted:
		order.RetrySuppressed, order.RetrySuppressionReason = true, core.AutomaticRecoveryExhaustedReason
	case nextaction.StallRetrySuppressed:
		order.RetrySuppressed = true
	case nextaction.StallStale:
		order.QueueDeadline = now.Add(-time.Minute)
	case nextaction.StallTimedOut:
		order.ExecutionStartedAt, order.ExecutionDeadline = now.Add(-2*time.Hour), now.Add(-time.Hour)
	case nextaction.StallDispatchFailing:
		order.AutomaticRetryCount, order.LastFailureMessage = 2, "dispatch failed"
	}
	h.order(order)
	if reason == nextaction.StallClaimFailing {
		for range nextaction.ClaimFailureStallThreshold {
			if _, err := h.st.RecordWorkOrderClaimFailure(h.ctx, order.ID, core.WorkOrderClaimFailureClassBackendOperation, now); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// stallReasons are the reasons this suite can seed on an order of any stage.
// unclaimable has its own verify-order fixtures below.
var stallReasons = []string{
	nextaction.StallAutomaticRecoveryExhausted, nextaction.StallRetrySuppressed, nextaction.StallStale,
	nextaction.StallTimedOut, nextaction.StallClaimFailing, nextaction.StallDispatchFailing,
}

func requireStall(t *testing.T, action nextaction.NextAction, reason string) {
	t.Helper()
	if action.Kind != nextaction.KindStalled || action.Details.StallReason != reason {
		t.Fatalf("next action = %s/%s, want stalled/%s", action.Kind, action.Details.StallReason, reason)
	}
}

// Every stalled statement agrees with the route: the offered redispatch posts
// and is accepted, and a withheld one is refused by the route with the stated
// text (req-operator-next-action AC-1.4; DEC-61; component-next-action,
// Invariants 7).
func TestStalledRedispatchChoiceAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	for _, reason := range stallReasons {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			t.Run("running task", func(t *testing.T) {
				h := newRedispatchHarness(t)
				h.task(core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement})
				stalledOrder(t, h, reason, "t", core.StageImplement)
				action := h.nextAction("t")
				requireStall(t, action, reason)
				h.requireRefused("t", action, "redispatch")
			})
			t.Run("queued task held by the order", func(t *testing.T) {
				h := newRedispatchHarness(t)
				h.task(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement})
				stalledOrder(t, h, reason, "t", core.StageImplement)
				action := h.nextAction("t")
				requireStall(t, action, reason)
				h.requireRefused("t", action, "redispatch")
			})
			t.Run("queued task the order does not hold", func(t *testing.T) {
				h := newRedispatchHarness(t)
				// An implement order left behind a return to the plan stage holds
				// no stage and has no recovery refusal.
				h.task(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageSpec})
				stalledOrder(t, h, reason, "t", core.StageImplement)
				action := h.nextAction("t")
				requireStall(t, action, reason)
				if reason == nextaction.StallClaimFailing {
					// The route would accept, and the design still offers no
					// redispatch: repairing the database is not a route
					// (component-next-action, Invariants 6).
					if choice, ok := redispatchChoice(t, action); ok {
						t.Fatalf("claim_failing offered %+v", choice)
					}
					return
				}
				h.requireAccepted("t", action, "redispatch")
			})
		})
	}
	t.Run(nextaction.StallUnclaimable, func(t *testing.T) {
		t.Parallel()
		// A queued verify order whose dispatch validation fails reads as
		// unclaimable on any task whose frozen policy has no verify stage.
		t.Run("queued task held by the order", func(t *testing.T) {
			h := newRedispatchHarness(t)
			h.task(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageVerify})
			stalledOrder(t, h, nextaction.StallUnclaimable, "t", core.StageVerify)
			action := h.nextAction("t")
			requireStall(t, action, nextaction.StallUnclaimable)
			h.requireRefused("t", action, "redispatch")
		})
		t.Run("queued task the order does not hold", func(t *testing.T) {
			h := newRedispatchHarness(t)
			h.task(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement})
			stalledOrder(t, h, nextaction.StallUnclaimable, "t", core.StageVerify)
			action := h.nextAction("t")
			requireStall(t, action, nextaction.StallUnclaimable)
			h.requireAccepted("t", action, "redispatch")
		})
	})
}

// A claim whose lease elapsed behind a later order reads as stalled, and the
// route still refuses while its stored claim holds the stage. Once the order
// clock persists the expiry, the route accepts (component-durable-queue;
// core.WorkOrderHoldsStage).
func TestRedispatchWithheldUntilAClaimExpiryIsPersisted(t *testing.T) {
	t.Parallel()
	h := newRedispatchHarness(t)
	h.task(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement})
	first := "t-implement-1"
	h.order(core.WorkOrder{ID: first, TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderQueued})
	if _, err := storetest.ClaimWorkOrder(h.ctx, h.st, first, core.WorkOrderClaim{SessionID: "dead-session", ClientToken: "dead-token", Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ops := taskops.New(h.st)
	if _, err := ops.Perform(h.ctx, "t", taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	h.order(core.WorkOrder{ID: "t-review-1-seat-1", TaskID: "t", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderQueued})
	if _, err := ops.Perform(h.ctx, "t", taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Perform(h.ctx, "t", taskops.Command{Kind: core.TaskStageBounce, NextStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	action := h.nextAction("t")
	if action.Kind != nextaction.KindStalled || action.CurrentOrder == nil || action.CurrentOrder.ID != first {
		t.Fatalf("next action = %+v, want the expired claim stalled", action)
	}
	h.requireRefused("t", action, "redispatch")

	if _, err := ops.TickOrderClock(h.ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The persisted expiry schedules an automatic recovery, which offers no
	// choice, and the hold is gone from the route.
	if after := h.nextAction("t"); after.Kind != nextaction.KindRetrying {
		t.Fatalf("after the clock next action = %s, want retrying", after.Kind)
	}
	if response := h.do(http.MethodPost, "/v1/tasks/t/redispatch"); response.Code != http.StatusAccepted {
		t.Fatalf("after the clock route status=%d body=%s", response.Code, response.Body.String())
	}
}

func parkedHarness(t *testing.T, command string, task core.Task) *redispatchHarness {
	t.Helper()
	h := newRedispatchHarness(t)
	task.ID, task.State = "t", core.TaskParked
	h.task(task)
	if err := h.st.AppendEvent(h.ctx, core.Event{TaskID: "t", Kind: "task.state_changed", ActorRole: core.ActorSystem, At: time.Now().UTC(), Payload: core.JSONPayload(map[string]any{"command": command})}); err != nil {
		t.Fatal(err)
	}
	return h
}

// A task parked after dispatch failed finally offers redispatch whatever order
// remains, because the route recovers the task and never reads its orders, and
// withholds it only when no stage is recorded (req-operator-next-action AC-1.4).
func TestDispatchExhaustedRedispatchChoiceAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for name, order := range map[string]*core.WorkOrder{
		"no order":  nil,
		"queued":    {State: core.WorkOrderQueued},
		"stale":     {State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute)},
		"timed out": {State: core.WorkOrderQueued, ExecutionStartedAt: now.Add(-2 * time.Hour), ExecutionDeadline: now.Add(-time.Hour)},
		"cancelled": {State: core.WorkOrderQueued},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := parkedHarness(t, "dispatch.fail_final", core.Task{RecoveryStage: core.StageImplement})
			if order != nil {
				seeded := *order
				seeded.ID, seeded.TaskID, seeded.Stage = "t-implement-1", "t", core.StageImplement
				h.order(seeded)
				if name == "cancelled" {
					if _, err := taskops.ExecuteWorkOrder(h.ctx, h.st, "t", core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
						return h.st.PreemptWorkOrderCommand(h.ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: seeded.ID, RequestID: "preempt-1", Reason: "fixture preempt"})
					}); err != nil {
						t.Fatal(err)
					}
					stored, err := h.st.GetWorkOrder(h.ctx, seeded.ID)
					if err != nil || stored.State != core.WorkOrderCancelled {
						t.Fatalf("preempted order = %+v err=%v", stored, err)
					}
				}
			}
			action := h.nextAction("t")
			if action.Kind != nextaction.KindDispatchExhausted {
				t.Fatalf("kind = %s", action.Kind)
			}
			h.requireAccepted("t", action, "redispatch")
		})
	}
	t.Run("no recorded stage", func(t *testing.T) {
		t.Parallel()
		h := parkedHarness(t, "dispatch.fail_final", core.Task{})
		action := h.nextAction("t")
		if action.Kind != nextaction.KindDispatchExhausted {
			t.Fatalf("kind = %s", action.Kind)
		}
		h.requireRefused("t", action, "redispatch")
	})
}

// The parked recover choice names the task-scoped route: it posts and is
// accepted with a recorded stage, and is withheld with the route's refusal
// without one (req-operator-next-action AC-1.4).
func TestParkedRecoverChoiceAgreesWithTheRoute(t *testing.T) {
	t.Parallel()
	t.Run("recorded recovery stage", func(t *testing.T) {
		t.Parallel()
		h := parkedHarness(t, "triage.park", core.Task{RecoveryStage: core.StageImplement})
		action := h.nextAction("t")
		if action.Kind != nextaction.KindParked {
			t.Fatalf("kind = %s", action.Kind)
		}
		h.requireAccepted("t", action, "recover")
	})
	t.Run("no recorded stage", func(t *testing.T) {
		t.Parallel()
		h := parkedHarness(t, "triage.park", core.Task{})
		action := h.nextAction("t")
		if action.Kind != nextaction.KindParked {
			t.Fatalf("kind = %s", action.Kind)
		}
		h.requireRefused("t", action, "recover")
	})
}
