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
// the agent and the risk order ahead of the gate row. The other open states
// are covered by TestPlanRiskWaitStatesEveryOpenState. It asks no person for a
// judgment and offers no approval, because the approval route refuses it
// (req-risk-based-review AC-4.8, AC-11.3; component-next-action, The
// statement, row 3a).
func TestPlanRiskWaitWinsOverTheGateRow(t *testing.T) {
	for _, tc := range []struct {
		state            core.WorkOrderState
		kind, label, sum string
		deadline         time.Time
	}{
		{core.WorkOrderQueued, KindQueued, "Waiting for a risk rating", "Plan approval waits for a risk rating. The risk order is waiting for an agent.", time.Time{}},
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
			// A queued risk order waits with no deadline, as every queued order
			// does; a claimed one carries its execution deadline
			// (component-next-action invariant 14; DEC-117(2)).
			if tc.deadline.IsZero() && na.Deadline != nil || !tc.deadline.IsZero() && (na.Deadline == nil || !na.Deadline.Equal(tc.deadline)) {
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
// AC-1.6, AC-11.3; component-next-action, The statement, row 3a).
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
// (component-next-action, The statement; req-risk-based-review AC-1.6).
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

// The filter reaches every order selector, not only the queued row: the
// caller's orders stay whole, and a factory row whose task holds an
// unclaimable implementation order states that order, not a claimed
// record-mode risk order. The agent question and the disposition rows are
// reached from real store commands in the store conformance suite
// (RunRiskWaitNextActionConformance), because their markers come from the
// store assembler.
func TestRecordRiskOrderStaysOutOfEverySelector(t *testing.T) {
	task := riskTask(riskpolicy.ModeRecord, core.TaskRunning, "")
	notYet := core.WorkOrder{ID: "t-implement-2", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: riskWaitNow.Add(-2 * time.Hour)}
	orders := []core.WorkOrder{notYet, riskOrder("t-risk-1", core.WorkOrderClaimed)}
	if got := SelectableOrders(task, orders); len(got) != 1 || got[0].ID != "t-implement-2" {
		t.Fatalf("selectable orders=%+v", got)
	}
	na := Project(Input{Task: task, Orders: orders, Now: riskWaitNow})
	if len(orders) != 2 {
		t.Fatalf("Project changed the caller's orders: %+v", orders)
	}
	if na.Actor != ActorFactory || na.CurrentOrder == nil || na.CurrentOrder.ID != "t-implement-2" {
		t.Fatalf("next action %s/%s order %+v, want the factory naming the implementation order", na.Actor, na.Kind, na.CurrentOrder)
	}
}

// requireNoPlanApproval fails when a statement offers or refuses a plan
// approval choice: row 3a carries none in any kind it states.
func requireNoPlanApproval(t *testing.T, na NextAction) {
	t.Helper()
	for _, id := range []string{"approve", "redirect", "reject"} {
		if hasChoice(na, id) || hasRefusal(na, id) {
			t.Fatalf("row 3a carried the plan approval choice %s: choices=%+v refusals=%+v", id, na.Choices, na.Details.Refusals)
		}
	}
	if na.Details.GateKind != "" {
		t.Fatalf("gate kind=%q, want none", na.Details.GateKind)
	}
}

// requireRiskOrder fails unless the statement names the risk order and only
// that order.
func requireRiskOrder(t *testing.T, na NextAction, id, state string) {
	t.Helper()
	if na.CurrentOrder == nil || na.CurrentOrder.ID != id || na.CurrentOrder.Stage != string(core.StageRisk) || na.CurrentOrder.State != state {
		t.Fatalf("current order=%+v, want risk order %s in %s", na.CurrentOrder, id, state)
	}
	if len(na.Details.OrderIDs) != 1 || na.Details.OrderIDs[0] != id {
		t.Fatalf("order ids=%v, want [%s]", na.Details.OrderIDs, id)
	}
}

// withRiskOrder sets the plan risk order in the input and in its orders.
func withRiskOrder(in Input, order core.WorkOrder) Input {
	ref := *in.PlanRiskOrder
	ref.Order = order
	in.PlanRiskOrder = &ref
	in.Orders = []core.WorkOrder{order}
	return in
}

// Row 3a states the plan version's open risk order in every open state, as
// rows 10, 15, 18, 19, and 20 would state it. Only the working and queued
// statements carry the reason code plan_risk_rating, and no statement carries
// the plan approval choice (component-next-action, The statement, row 3a;
// req-risk-based-review AC-11.3; req-operator-next-action AC-1.3, AC-1.6).
func TestPlanRiskWaitStatesEveryOpenState(t *testing.T) {
	scheduled := riskWaitNow.Add(5 * time.Minute)
	base := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
	for _, tc := range []struct {
		name     string
		in       Input
		actor    string
		kind     string
		judgment bool
		reason   string
		stall    string
		state    core.WorkOrderState
		choices  []string
		deadline *time.Time
	}{
		{name: "agent question", in: func() Input {
			order := riskOrder("t-risk-1", core.WorkOrderQueued)
			order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
			order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Which plan step does the rating cover?"}
			return withRiskOrder(base, order)
		}(), actor: ActorOperator, kind: KindAgentQuestion, judgment: true, state: core.WorkOrderQueued, choices: []string{"answer"}},
		{name: "timed out", in: withRiskOrder(base, riskOrder("t-risk-1", core.WorkOrderTimedOut)),
			actor: ActorOperator, kind: KindStalled, reason: StallTimedOut, stall: StallTimedOut, state: core.WorkOrderTimedOut, choices: []string{"recover"}},
		{name: "automatic recovery exhausted", in: func() Input {
			order := riskOrder("t-risk-1", core.WorkOrderTimedOut)
			order.RetrySuppressed, order.RetrySuppressionReason = true, core.AutomaticRecoveryExhaustedReason
			in := withRiskOrder(base, order)
			in.Recovery = map[string]AutomaticRecovery{order.ID: {Exhausted: true}}
			return in
		}(), actor: ActorOperator, kind: KindStalled, reason: StallAutomaticRecoveryExhausted, stall: StallAutomaticRecoveryExhausted, state: core.WorkOrderTimedOut, choices: []string{"recover"}},
		{name: "retry suppressed", in: func() Input {
			order := riskOrder("t-risk-1", core.WorkOrderQueued)
			order.RetrySuppressed, order.RetrySuppressionReason = true, core.IdenticalFailureSuppressionReason
			return withRiskOrder(base, order)
		}(), actor: ActorOperator, kind: KindStalled, reason: StallRetrySuppressed, stall: StallRetrySuppressed, state: core.WorkOrderQueued, choices: []string{"recover"}},
		{name: "unclaimable", in: func() Input {
			in := withRiskOrder(base, riskOrder("t-risk-1", core.WorkOrderQueued))
			in.Task.Assignee = &core.TaskAssignee{UserID: "usr_away"}
			in.LiveClaimants = &ClaimantView{UserIDs: []string{"usr_other"}}
			return in
		}(), actor: ActorOperator, kind: KindStalled, reason: StallUnclaimable, stall: StallUnclaimable, state: core.WorkOrderQueued, choices: []string{"recover"}},
		{name: "retrying", in: func() Input {
			order := riskOrder("t-risk-1", core.WorkOrderTimedOut)
			order.NextAutomaticRecoveryAt = scheduled
			in := withRiskOrder(base, order)
			in.Recovery = map[string]AutomaticRecovery{order.ID: {NextAt: &scheduled, Attempt: 1}}
			return in
		}(), actor: ActorFactory, kind: KindRetrying, reason: "automatic_recovery", state: core.WorkOrderTimedOut, deadline: &scheduled},
		{name: "working", in: withRiskOrder(base, riskOrder("t-risk-1", core.WorkOrderClaimed)),
			actor: ActorAgent, kind: KindWorking, reason: PlanRiskRating, state: core.WorkOrderClaimed, deadline: timePtr(riskWaitNow.Add(time.Hour))},
		{name: "queued", in: withRiskOrder(base, riskOrder("t-risk-1", core.WorkOrderQueued)),
			actor: ActorAgent, kind: KindQueued, reason: PlanRiskRating, state: core.WorkOrderQueued},
		{name: "stored stale reads as queued", in: withRiskOrder(base, riskOrder("t-risk-1", core.WorkOrderStale)),
			actor: ActorAgent, kind: KindQueued, reason: PlanRiskRating, state: core.WorkOrderQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			na := Project(tc.in)
			if na.Actor != tc.actor || na.Kind != tc.kind || na.Judgment != tc.judgment {
				t.Fatalf("next action %s/%s judgment=%t, want %s/%s judgment=%t", na.Actor, na.Kind, na.Judgment, tc.actor, tc.kind, tc.judgment)
			}
			if na.Details.ReasonCode != tc.reason || na.Details.StallReason != tc.stall {
				t.Fatalf("reason=%q stall=%q, want %q %q", na.Details.ReasonCode, na.Details.StallReason, tc.reason, tc.stall)
			}
			requireRiskOrder(t, na, "t-risk-1", string(tc.state))
			requireNoPlanApproval(t, na)
			for _, id := range tc.choices {
				if !hasChoice(na, id) && !hasRefusal(na, id) {
					t.Fatalf("choices=%+v refusals=%+v, want %s", na.Choices, na.Details.Refusals, id)
				}
			}
			if tc.deadline == nil && na.Deadline != nil || tc.deadline != nil && (na.Deadline == nil || !na.Deadline.Equal(*tc.deadline)) {
				t.Fatalf("deadline=%v want %v", na.Deadline, tc.deadline)
			}
			if !hasSecondary(na, KindPlanApproval) {
				t.Fatalf("secondary=%+v, want plan_approval", na.Details.Secondary)
			}
		})
	}
}

// The statements keep the precedence of rows 10, 15, 18, 19, and 20: a
// decision request wins over a stall, a stall wins over a recovery whose time
// has passed, and a recovery scheduled in the future wins over the stall of
// the timeout it recovers (component-next-action invariant 5).
func TestPlanRiskWaitPrecedence(t *testing.T) {
	base := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
	question := riskOrder("t-risk-1", core.WorkOrderQueued)
	question.LastAttemptOutcome = core.WorkOrderOutcomeReleased
	question.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Which plan step does the rating cover?"}
	question.RetrySuppressed, question.RetrySuppressionReason = true, core.IdenticalFailureSuppressionReason
	if na := Project(withRiskOrder(base, question)); na.Kind != KindAgentQuestion || na.Details.Checkpoint != question.Checkpoint.DecisionRequest {
		t.Fatalf("question beside a suppression kind=%s checkpoint=%q, want the agent question", na.Kind, na.Details.Checkpoint)
	}
	passed := riskWaitNow.Add(-time.Minute)
	due := riskOrder("t-risk-1", core.WorkOrderTimedOut)
	in := withRiskOrder(base, due)
	in.Recovery = map[string]AutomaticRecovery{due.ID: {NextAt: &passed, Attempt: 1}}
	if na := Project(in); na.Kind != KindStalled || na.Details.StallReason != StallTimedOut {
		t.Fatalf("due recovery kind=%s stall=%s, want the timed-out stall", na.Kind, na.Details.StallReason)
	}
}

// Row 3a reads only its own order. A newer order of the task that is
// released at a checkpoint, scheduled for recovery, or claimed never takes
// the statement, and the risk order's own checkpoint is the one presented.
func TestPlanRiskWaitNamesOnlyItsOrder(t *testing.T) {
	scheduled := riskWaitNow.Add(5 * time.Minute)
	other := core.WorkOrder{ID: "t-spec-2", TaskID: "t", Stage: core.StageSpec, State: core.WorkOrderTimedOut,
		QueueEnteredAt: riskWaitNow.Add(-time.Minute), NextAutomaticRecoveryAt: scheduled}
	claimed := core.WorkOrder{ID: "t-spec-3", TaskID: "t", Stage: core.StageSpec, State: core.WorkOrderClaimed,
		QueueEnteredAt: riskWaitNow.Add(-time.Minute), LeaseExpiresAt: riskWaitNow.Add(time.Hour), ExecutionDeadline: riskWaitNow.Add(time.Hour)}
	for _, tc := range []struct {
		name  string
		risk  core.WorkOrder
		kind  string
		check string
	}{
		{"queued", riskOrder("t-risk-1", core.WorkOrderQueued), KindQueued, ""},
		{"claimed", riskOrder("t-risk-1", core.WorkOrderClaimed), KindWorking, ""},
		{"timed out", riskOrder("t-risk-1", core.WorkOrderTimedOut), KindStalled, ""},
		{"question", func() core.WorkOrder {
			order := riskOrder("t-risk-1", core.WorkOrderQueued)
			order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
			order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Rate the plan or the head?"}
			return order
		}(), KindAgentQuestion, "Rate the plan or the head?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
			in.PlanRiskOrder.Order = tc.risk
			in.Orders = []core.WorkOrder{tc.risk, other, claimed}
			in.Recovery = map[string]AutomaticRecovery{other.ID: {NextAt: &scheduled, Attempt: 1}}
			// A task-level checkpoint marker of another order never replaces
			// the risk order's own decision request.
			in.Markers = Markers{CheckpointDecision: true, Checkpoint: "Another order's question"}
			na := Project(in)
			if na.Kind != tc.kind {
				t.Fatalf("kind=%s want %s", na.Kind, tc.kind)
			}
			requireRiskOrder(t, na, "t-risk-1", string(tc.risk.State))
			requireNoPlanApproval(t, na)
			if na.Details.Checkpoint != tc.check {
				t.Fatalf("checkpoint=%q want %q", na.Details.Checkpoint, tc.check)
			}
		})
	}
}

// An authority-conflict decision request of the risk agent is presented with
// its own citations and the cited pending proposals, as row 10 presents one.
func TestPlanRiskWaitAuthorityQuestion(t *testing.T) {
	order := riskOrder("t-risk-1", core.WorkOrderQueued)
	order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
	citation := core.WorkOrderAuthorityConflictCitation{DocumentID: "component-risk-review", CitedVersion: 4, StatementOrSectionID: "s3"}
	order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "The tier table conflicts with the plan.", Class: core.WorkOrderCheckpointClassAuthorityConflict,
		Citations: []core.WorkOrderAuthorityConflictCitation{citation}}
	in := withRiskOrder(planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued), order)
	in.PendingProposals = []ProposalRef{{Tier: "system_design", ID: "component-risk-review", Version: 5}}
	if got := PlanRiskCheckpointCitations(in); len(got) != 1 || got[0] != citation {
		t.Fatalf("citations=%+v", got)
	}
	na := Project(in)
	if na.Kind != KindAgentQuestion || na.Details.CheckpointClass != core.WorkOrderCheckpointClassAuthorityConflict || len(na.Details.Citations) != 1 || len(na.Details.PendingProposals) != 1 {
		t.Fatalf("next action kind=%s details=%+v", na.Kind, na.Details)
	}
	requireRiskOrder(t, na, "t-risk-1", string(core.WorkOrderQueued))
	requireNoPlanApproval(t, na)
	// A plain question carries no citations to read.
	order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Which plan step?"}
	if got := PlanRiskCheckpointCitations(withRiskOrder(in, order)); got != nil {
		t.Fatalf("plain question citations=%+v", got)
	}
}
