package nextaction

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var redispatchNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// stallFixtures reach each stall reason of component-next-action Invariants 5
// on one queued order.
var stallFixtures = map[string]func(*Input, *core.WorkOrder){
	StallAutomaticRecoveryExhausted: func(in *Input, o *core.WorkOrder) {
		in.Recovery = map[string]AutomaticRecovery{o.ID: {Exhausted: true}}
	},
	StallRetrySuppressed: func(_ *Input, o *core.WorkOrder) { o.RetrySuppressed = true },
	StallStale:           func(_ *Input, o *core.WorkOrder) { o.State = core.WorkOrderStale },
	StallTimedOut:        func(_ *Input, o *core.WorkOrder) { o.State = core.WorkOrderTimedOut },
	StallUnclaimable:     func(_ *Input, o *core.WorkOrder) { o.RecoveryRefusal = "frozen policy refuses recovery" },
	StallClaimFailing: func(in *Input, o *core.WorkOrder) {
		in.ClaimFailures = map[string]core.WorkOrderClaimFailure{o.ID: {WorkOrderID: o.ID, ConsecutiveFailures: ClaimFailureStallThreshold, ErrorClass: core.WorkOrderClaimFailureClassBackendOperation}}
	},
	StallDispatchFailing: func(_ *Input, o *core.WorkOrder) {
		o.AutomaticRetryCount, o.LastFailureMessage = 2, "dispatch failed"
	},
}

func stalledInput(task core.Task, reason string) Input {
	order := core.WorkOrder{ID: "t-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	in := Input{Task: task, Capabilities: allCaps(), Now: redispatchNow}
	stallFixtures[reason](&in, &order)
	in.Orders = []core.WorkOrder{order}
	return in
}

// requireWithheld proves the choice is absent and its refusal is recorded
// with the route's own text.
func requireWithheld(t *testing.T, got NextAction, id, text string) {
	t.Helper()
	if hasChoice(got, id) {
		t.Fatalf("%s offered: %+v", id, got.Choices)
	}
	if !hasRefusalText(got, id, text) {
		t.Fatalf("refusals = %+v, want %s refused with %q", got.Details.Refusals, id, text)
	}
}

func requireRedispatchOffered(t *testing.T, got NextAction, id string) {
	t.Helper()
	choice, ok := choiceByID(got, id)
	if !ok {
		t.Fatalf("%s withheld: choices %+v refusals %+v", id, got.Choices, got.Details.Refusals)
	}
	if choice.Method != "POST" || choice.Path != "/v1/tasks/t/redispatch" {
		t.Fatalf("%s = %+v, want POST /v1/tasks/t/redispatch", id, choice)
	}
	for _, refusal := range got.Details.Refusals {
		if refusal.Choice == id {
			t.Fatalf("%s offered and refused: %+v", id, refusal)
		}
	}
}

// A stalled statement withholds redispatch for every stall reason whenever the
// route would answer 409: a running task, or a queued task whose stage the
// order holds (req-operator-next-action AC-1.4; DEC-61; component-next-action,
// Invariants 7).
func TestStalledRedispatchWithheldWhenTheRouteRefuses(t *testing.T) {
	for reason := range stallFixtures {
		t.Run(reason, func(t *testing.T) {
			running := Project(stalledInput(core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement}, reason))
			if running.Kind != KindStalled || running.Details.StallReason != reason {
				t.Fatalf("running = %s/%s", running.Kind, running.Details.StallReason)
			}
			requireWithheld(t, running, "redispatch", core.TaskRedispatchActiveRefusal)

			queued := Project(stalledInput(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement}, reason))
			if queued.Kind != KindStalled || queued.Details.StallReason != reason {
				t.Fatalf("queued = %s/%s", queued.Kind, queued.Details.StallReason)
			}
			requireWithheld(t, queued, "redispatch", core.TaskHeldByWorkOrderRefusal+": task t waits on work order t-implement-1")
		})
	}
}

// A stalled statement offers redispatch for every stall reason the route
// accepts it on: a queued task whose orders hold no stage. claim_failing is the
// exception its design states (component-next-action, Invariants 6).
func TestStalledRedispatchOfferedWhenTheRouteAccepts(t *testing.T) {
	for reason := range stallFixtures {
		t.Run(reason, func(t *testing.T) {
			got := Project(stalledInput(core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageSpec}, reason))
			if got.Kind != KindStalled || got.Details.StallReason != reason {
				t.Fatalf("next action = %s/%s", got.Kind, got.Details.StallReason)
			}
			if reason == StallClaimFailing {
				requireWithheld(t, got, "redispatch", claimFailingRedispatchRefusal)
				return
			}
			requireRedispatchOffered(t, got, "redispatch")
		})
	}
}

// The newest order's stage and state decide the hold, as the route's stored
// predicate does, so a stalled order behind a finished one at the next stage
// keeps the offer and a stalled seat of the latest review round withholds it.
func TestStalledRedispatchFollowsTheStageHold(t *testing.T) {
	implement := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderStale}
	completed := core.WorkOrder{ID: "t-review-1-seat-1", TaskID: "t", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderCompleted}
	offered := Project(Input{Task: core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageReview}, Orders: []core.WorkOrder{implement, completed}, Capabilities: allCaps(), Now: redispatchNow})
	if offered.Details.StallReason != StallStale {
		t.Fatalf("stall = %q", offered.Details.StallReason)
	}
	requireRedispatchOffered(t, offered, "redispatch")

	seat := completed
	seat.ID, seat.ReviewSeat, seat.State = "t-review-1-seat-2", 2, core.WorkOrderTimedOut
	held := Project(Input{Task: core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageReview}, Orders: []core.WorkOrder{completed, seat}, Capabilities: allCaps(), Now: redispatchNow})
	if held.Details.StallReason != StallTimedOut {
		t.Fatalf("stall = %q", held.Details.StallReason)
	}
	requireWithheld(t, held, "redispatch", "waits on work order t-review-1-seat-2")
}

// A dead claim behind a later order still holds its stage until the order clock
// persists the expiry, because the route reads stored states. A persisted
// expiry of the same order no longer holds (component-durable-queue;
// core.WorkOrderHoldsStage).
func TestStalledRedispatchSeesAnUnpersistedClaimExpiry(t *testing.T) {
	expired := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderQueued, RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired}
	seat := core.WorkOrder{ID: "t-review-1-seat-1", TaskID: "t", Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, State: core.WorkOrderCompleted}
	task := core.Task{ID: "t", State: core.TaskQueued, NextStage: core.StageImplement}

	persisted := Project(Input{Task: task, Orders: []core.WorkOrder{expired, seat}, Capabilities: allCaps(), Now: redispatchNow})
	requireRedispatchOffered(t, persisted, "redispatch")

	expired.ClaimExpiryUnpersisted = true
	orders := []core.WorkOrder{expired, seat}
	held := Project(Input{Task: task, Orders: orders, Capabilities: allCaps(), Now: redispatchNow})
	requireWithheld(t, held, "redispatch", "waits on work order t-implement-1")
	if orders[0].State != core.WorkOrderQueued {
		t.Fatalf("the predicate mutated the projected order: %s", orders[0].State)
	}
	if !hasChoice(held, "recover") {
		t.Fatalf("recover withheld: %+v %+v", held.Choices, held.Details.Refusals)
	}
}

// dispatch_exhausted parks the task, so the route ignores the order's state:
// redispatch stays offered whatever order remains, and only a task with no
// stage to resume at withholds it (req-operator-next-action AC-1.4).
func TestDispatchExhaustedRedispatchFollowsTheRoute(t *testing.T) {
	markers := Markers{ParkCommand: "dispatch.fail_final"}
	task := core.Task{ID: "t", State: core.TaskParked, RecoveryStage: core.StageImplement}
	for _, state := range []core.WorkOrderState{"", core.WorkOrderQueued, core.WorkOrderStale, core.WorkOrderTimedOut, core.WorkOrderCancelled, core.WorkOrderClaimed, core.WorkOrderSubmitted} {
		name := string(state)
		if name == "" {
			name = "no order"
		}
		t.Run(name, func(t *testing.T) {
			var orders []core.WorkOrder
			if state != "" {
				orders = []core.WorkOrder{{ID: "o", TaskID: "t", Stage: core.StageImplement, State: state}}
			}
			got := Project(Input{Task: task, Markers: markers, Orders: orders, Capabilities: allCaps(), Now: redispatchNow})
			if got.Kind != KindDispatchExhausted {
				t.Fatalf("kind = %s", got.Kind)
			}
			requireRedispatchOffered(t, got, "redispatch")

			noStage := task
			noStage.RecoveryStage = ""
			refused := Project(Input{Task: noStage, Markers: markers, Orders: orders, Capabilities: allCaps(), Now: redispatchNow})
			requireWithheld(t, refused, "redispatch", core.TaskRedispatchNoStageRefusal)
		})
	}
}

// The parked recover names the task-scoped route, so it follows that route: a
// recorded recovery or next stage keeps the offer, and neither withholds it.
func TestParkedRecoverFollowsTheRoute(t *testing.T) {
	markers := Markers{ParkCommand: "triage.park"}
	for name, task := range map[string]core.Task{
		"recovery stage": {ID: "t", State: core.TaskParked, RecoveryStage: core.StageImplement},
		"next stage":     {ID: "t", State: core.TaskParked, NextStage: core.StageImplement},
	} {
		t.Run(name, func(t *testing.T) {
			requireRedispatchOffered(t, Project(Input{Task: task, Markers: markers, Capabilities: allCaps(), Now: redispatchNow}), "recover")
		})
	}
	got := Project(Input{Task: core.Task{ID: "t", State: core.TaskParked}, Markers: markers, Capabilities: allCaps(), Now: redispatchNow})
	if got.Kind != KindParked {
		t.Fatalf("kind = %s", got.Kind)
	}
	requireWithheld(t, got, "recover", core.TaskRedispatchNoStageRefusal)
	if !hasChoice(got, "close") || !hasChoice(got, "start_over") {
		t.Fatalf("choices = %+v, want close and start over kept", got.Choices)
	}
}

// claim_failing states its redispatch refusal and keeps the route's text first,
// and every other stalled act follows its own predicate.
func TestClaimFailingChoicesKeepOnlyTheRedispatchException(t *testing.T) {
	specs := claimFailingChoices()
	base := stalledChoices()
	if len(specs) != len(base) {
		t.Fatalf("specs = %+v", specs)
	}
	for i := range specs {
		if specs[i].ID != base[i].ID || (specs[i].ID != "redispatch" && specs[i].Refusal != "") {
			t.Fatalf("spec %d = %+v, want %+v", i, specs[i], base[i])
		}
	}
	running := Project(stalledInput(core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement}, StallClaimFailing))
	requireWithheld(t, running, "redispatch", core.TaskRedispatchActiveRefusal)
	if hasChoice(running, "change_policy") || !hasRefusal(running, "change_policy") {
		t.Fatalf("change_policy: choices %+v refusals %+v", running.Choices, running.Details.Refusals)
	}
}

// TestAwaitingGatesOfferNoRedispatch proves the statement at a pending
// plan-revision decision offers only the decision, and that no awaiting gate
// names the redispatch route, even with a stale order that would offer it
// outside a gate (req-operator-next-action AC-1.4; component-next-action
// invariants 3, 7, and 8; component-work-orders invariant 39).
func TestAwaitingGatesOfferNoRedispatch(t *testing.T) {
	for _, gate := range []core.GateKind{
		core.GateKindPlanRevision, core.GateKindPlanApproval, core.GateKindMergeApproval,
		core.GateKindReviewBounceLimit, core.GateKindJobFailure, "",
	} {
		t.Run("gate "+string(gate), func(t *testing.T) {
			got := Project(stalledInput(core.Task{ID: "t", State: core.TaskAwaiting, NextStage: core.StageImplement, GateKind: gate}, StallRetrySuppressed))
			for _, choice := range got.Choices {
				if choice.ID == "redispatch" || strings.HasSuffix(choice.Path, "/redispatch") {
					t.Fatalf("%s offers %+v", got.Kind, choice)
				}
			}
			if gate != core.GateKindPlanRevision {
				return
			}
			if got.Kind != KindPlanRevision {
				t.Fatalf("kind = %s, want %s", got.Kind, KindPlanRevision)
			}
			var ids []string
			for _, choice := range got.Choices {
				ids = append(ids, choice.ID)
			}
			if want := []string{"approve_revision", "decline", "reject"}; !slices.Equal(ids, want) {
				t.Fatalf("choices = %v, want %v", ids, want)
			}
		})
	}
}
