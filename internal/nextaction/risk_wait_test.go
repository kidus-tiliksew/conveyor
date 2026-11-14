package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

var riskWaitNow = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func riskTask(mode riskpolicy.Mode, state core.TaskState, gate core.GateKind) core.Task {
	task := core.Task{ID: "t", Title: "Risk wait", State: state, GateKind: gate, NextStage: core.StageImplement}
	if mode != "" {
		task.SetupContract = config.PolicyContract{Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
	}
	return task
}

func riskOrder(id string, state core.WorkOrderState) core.WorkOrder {
	order := core.WorkOrder{ID: id, TaskID: "t", JobID: id, Stage: core.StageRisk, State: state, Claimable: state == core.WorkOrderQueued,
		QueueEnteredAt: riskWaitNow.Add(-time.Hour), QueueDeadline: riskWaitNow.Add(23 * time.Hour)}
	if state == core.WorkOrderClaimed {
		order.ExecutionStartedAt, order.ExecutionDeadline = riskWaitNow.Add(-time.Minute), riskWaitNow.Add(time.Hour)
		order.LeaseExpiresAt = riskWaitNow.Add(time.Hour)
	}
	return order
}

// planRiskInput is an enforcing task waiting at plan approval on version 2
// whose risk order is in state.
func planRiskInput(mode riskpolicy.Mode, state core.WorkOrderState) Input {
	order := riskOrder("t-risk-1", state)
	return Input{
		Task: riskTask(mode, core.TaskAwaiting, core.GateKindPlanApproval), Orders: []core.WorkOrder{order},
		LatestSpec: &SpecRef{Version: 2}, GateSpecVersion: 2,
		PlanRiskOrder: &PlanRiskOrderRef{PlanVersion: 2, Order: order},
		Capabilities:  map[core.Capability]bool{core.CapabilityOperateGates: true}, Now: riskWaitNow,
	}
}

func requireGateRow(t *testing.T, na NextAction) {
	t.Helper()
	if na.Actor != ActorOperator || na.Kind != KindPlanApproval || !na.Judgment || !hasChoice(na, "approve") {
		t.Fatalf("next action %s/%s judgment=%t choices=%+v, want the operator's plan approval", na.Actor, na.Kind, na.Judgment, na.Choices)
	}
}

// An enforcing task whose plan version's risk order is queued or claimed names
// the agent and the risk order ahead of the gate row. It asks no person for a
// judgment and offers no approval, because the approval route refuses it
// (req-risk-based-review AC-4.8, AC-11.3; feature-risk-based-review RR-16).
func TestPlanRiskWaitWinsOverTheGateRow(t *testing.T) {
	for _, tc := range []struct {
		state            core.WorkOrderState
		kind, label, sum string
		deadline         time.Time
	}{
		{core.WorkOrderQueued, KindQueued, "Waiting for a risk rating", "Plan approval waits for a risk rating. The risk order is waiting for an agent.", riskWaitNow.Add(23 * time.Hour)},
		{core.WorkOrderClaimed, KindWorking, "Rating plan risk", "An agent is rating the risk of the plan. Plan approval waits for this rating.", riskWaitNow.Add(time.Hour)},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			na := Project(planRiskInput(riskpolicy.ModeEnforce, tc.state))
			assertSummary(t, na, ActorAgent, tc.kind, tc.label, tc.sum)
			if na.Judgment || len(na.Choices) != 0 || na.Question != "" {
				t.Fatalf("judgment=%t choices=%+v question=%q, want no person's act", na.Judgment, na.Choices, na.Question)
			}
			if na.CurrentOrder == nil || na.CurrentOrder.ID != "t-risk-1" || na.CurrentOrder.Stage != string(core.StageRisk) || na.CurrentOrder.State != string(tc.state) {
				t.Fatalf("current order=%+v, want the risk order", na.CurrentOrder)
			}
			if len(na.Details.OrderIDs) != 1 || na.Details.OrderIDs[0] != "t-risk-1" || na.Details.ReasonCode != PlanRiskRating || na.Details.GateKind != "" {
				t.Fatalf("details=%+v", na.Details)
			}
			if na.Deadline == nil || !na.Deadline.Equal(tc.deadline) {
				t.Fatalf("deadline=%v want %v", na.Deadline, tc.deadline)
			}
			// The gate row still matches and is recorded as secondary.
			if !hasSecondary(na, KindPlanApproval) {
				t.Fatalf("secondary=%+v, want plan_approval", na.Details.Secondary)
			}
		})
	}
}

// Completion and cancellation end the wait, and the gate row applies again.
// Every other shape keeps the gate row: modes other than enforce, a missing
// order, an order of an earlier plan version, an order of another task or
// stage, and a head risk order that is not the plan's (req-risk-based-review
// AC-1.6, AC-11.3; feature-risk-based-review RR-15, RR-16).
func TestPlanRiskWaitLeavesTheGateRowOtherwise(t *testing.T) {
	cases := map[string]func() Input{
		"completed": func() Input { return planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderCompleted) },
		"cancelled": func() Input { return planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderCancelled) },
		"record":    func() Input { return planRiskInput(riskpolicy.ModeRecord, core.WorkOrderQueued) },
		"off":       func() Input { return planRiskInput(riskpolicy.ModeOff, core.WorkOrderQueued) },
		"no policy": func() Input { return planRiskInput("", core.WorkOrderQueued) },
		"missing order": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder = nil
			return in
		},
		"earlier version": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder.PlanVersion = 1
			return in
		},
		"no gate version": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.GateSpecVersion, in.LatestSpec = 0, nil
			return in
		},
		"another task": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder.Order.TaskID = "other"
			return in
		},
		"not a risk order": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder.Order.Stage = core.StageReview
			return in
		},
		"head risk order only": func() Input {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder = nil
			in.Orders = []core.WorkOrder{riskOrder("t-risk-2", core.WorkOrderQueued)}
			return in
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			requireGateRow(t, Project(build()))
		})
	}
}

// The row matches plan approval only: another gate kind keeps its own row
// beside an open plan risk order.
func TestPlanRiskWaitLeavesOtherGates(t *testing.T) {
	for _, gate := range []core.GateKind{core.GateKindPlanRevision, core.GateKindMergeApproval, core.GateKindReviewBounceLimit, core.GateKindJobFailure} {
		t.Run(string(gate), func(t *testing.T) {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.Task.GateKind = gate
			na := Project(in)
			if na.Actor != ActorOperator || na.Kind != string(gate) {
				t.Fatalf("next action %s/%s, want the operator's %s gate", na.Actor, na.Kind, gate)
			}
		})
	}
}

// Rows above the new row keep winning over it: a parked or terminal task never
// states the plan risk wait.
func TestPlanRiskWaitKeepsEarlierRows(t *testing.T) {
	in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
	in.Task.State = core.TaskClosed
	if na := Project(in); na.Kind != KindTerminal {
		t.Fatalf("closed task kind=%s, want terminal", na.Kind)
	}
}

// Under record, a risk order is never the order a next action names while
// another order of the task is queued or claimed. A lone risk order is named
// with risk wording, and enforce keeps its risk order selectable
// (feature-risk-based-review RR-15; req-risk-based-review AC-1.6).
func TestRecordRiskOrderNeverDisplacesAnotherOpenOrder(t *testing.T) {
	implement := func(state core.WorkOrderState) core.WorkOrder {
		return core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: state, Claimable: state == core.WorkOrderQueued,
			QueueEnteredAt: riskWaitNow.Add(-2 * time.Hour), QueueDeadline: riskWaitNow.Add(22 * time.Hour)}
	}
	running := func(mode riskpolicy.Mode) core.Task { return riskTask(mode, core.TaskRunning, "") }
	for _, tc := range []struct {
		name        string
		mode        riskpolicy.Mode
		orders      []core.WorkOrder
		kind, order string
		summary     string
	}{
		{"claimed risk beside queued implementation", riskpolicy.ModeRecord,
			[]core.WorkOrder{implement(core.WorkOrderQueued), riskOrder("t-risk-1", core.WorkOrderClaimed)},
			KindQueued, "t-implement-1", "The implementation order is waiting for an agent."},
		{"queued risk beside claimed implementation", riskpolicy.ModeRecord,
			[]core.WorkOrder{implement(core.WorkOrderClaimed), riskOrder("t-risk-1", core.WorkOrderQueued)},
			KindWorking, "t-implement-1", "An agent is implementing this task."},
		{"newer queued risk beside queued implementation", riskpolicy.ModeRecord,
			[]core.WorkOrder{implement(core.WorkOrderQueued), riskOrder("t-risk-1", core.WorkOrderQueued)},
			KindQueued, "t-implement-1", "The implementation order is waiting for an agent."},
		{"lone queued record risk order", riskpolicy.ModeRecord,
			[]core.WorkOrder{implement(core.WorkOrderCompleted), riskOrder("t-risk-1", core.WorkOrderQueued)},
			KindQueued, "t-risk-1", "The risk order is waiting for an agent to rate the risk."},
		{"lone claimed record risk order", riskpolicy.ModeRecord,
			[]core.WorkOrder{implement(core.WorkOrderCompleted), riskOrder("t-risk-1", core.WorkOrderClaimed)},
			KindWorking, "t-risk-1", "An agent is rating the risk of this task."},
		{"enforce head wait names the queued risk order", riskpolicy.ModeEnforce,
			[]core.WorkOrder{implement(core.WorkOrderCompleted), riskOrder("t-risk-1", core.WorkOrderQueued)},
			KindQueued, "t-risk-1", "The risk order is waiting for an agent to rate the risk."},
		{"enforce head wait names the claimed risk order", riskpolicy.ModeEnforce,
			[]core.WorkOrder{implement(core.WorkOrderCompleted), riskOrder("t-risk-1", core.WorkOrderClaimed)},
			KindWorking, "t-risk-1", "An agent is rating the risk of this task."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			na := Project(Input{Task: running(tc.mode), Orders: tc.orders, Now: riskWaitNow})
			if na.Kind != tc.kind || na.Actor != ActorAgent || na.CurrentOrder == nil || na.CurrentOrder.ID != tc.order {
				t.Fatalf("next action %s/%s order %+v, want %s naming %s", na.Actor, na.Kind, na.CurrentOrder, tc.kind, tc.order)
			}
			if na.Summary != tc.summary {
				t.Fatalf("summary=%q want %q", na.Summary, tc.summary)
			}
		})
	}
}

// The filter reaches every order selector, not only the queued row: a
// record-mode risk order queued after an implementation order released at its
// checkpoint never hides the agent question, and the risk order is never the
// stated order of a factory row.
func TestRecordRiskOrderStaysOutOfEverySelector(t *testing.T) {
	released := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderQueued,
		LastAttemptOutcome: core.WorkOrderOutcomeReleased, QueueEnteredAt: riskWaitNow.Add(-2 * time.Hour)}
	in := Input{Task: riskTask(riskpolicy.ModeRecord, core.TaskRunning, ""), Orders: []core.WorkOrder{released, riskOrder("t-risk-1", core.WorkOrderQueued)},
		Markers: Markers{CheckpointDecision: true, Checkpoint: "Which way?"}, Now: riskWaitNow}
	na := Project(in)
	if na.Kind != KindAgentQuestion || na.CurrentOrder == nil || na.CurrentOrder.ID != "t-implement-1" {
		t.Fatalf("next action %s order %+v, want the agent question on the implementation order", na.Kind, na.CurrentOrder)
	}
	if got := SelectableOrders(in.Task, in.Orders); len(got) != 1 || got[0].ID != "t-implement-1" {
		t.Fatalf("selectable orders=%+v", got)
	}
	// The caller's slice keeps every order.
	if len(in.Orders) != 2 {
		t.Fatalf("Project changed the caller's orders: %+v", in.Orders)
	}
	// A factory row whose task holds an unclaimable implementation order
	// states that order, not the queued risk order.
	notYet := core.WorkOrder{ID: "t-implement-2", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: riskWaitNow.Add(-2 * time.Hour)}
	na = Project(Input{Task: riskTask(riskpolicy.ModeRecord, core.TaskRunning, ""), Orders: []core.WorkOrder{notYet, riskOrder("t-risk-1", core.WorkOrderClaimed)}, Now: riskWaitNow})
	if na.Actor != ActorFactory || na.CurrentOrder == nil || na.CurrentOrder.ID != "t-implement-2" {
		t.Fatalf("next action %s/%s order %+v, want the factory naming the implementation order", na.Actor, na.Kind, na.CurrentOrder)
	}
}
