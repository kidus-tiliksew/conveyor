package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunPlanGateRiskRecoveryConformance proves on every backend, from a real
// plan submission, claims, releases, and order clock ticks only, that while
// an enforcing task waits at the plan approval gate the clock recovers the
// plan version's timed-out risk order within the recovery bound, stops at the
// bound with one exhaustion record, and never applies a task command,
// records an approval, or moves the gate. Under record, and for a head risk
// order, a live claim, or an unanswered checkpoint, the clock leaves the
// order alone. Each step also reads the public next action through the
// store assembler: row 3a states the risk order as queued, working,
// retrying, stalled, or an agent question, and never offers the plan
// approval (component-work-orders invariants 45 to 48 and its State and
// failure entry "Stopped risk order of a task at a gate"; component-next-action,
// The statement, row 3a; component-plan-state invariant 10;
// req-bounded-automatic-recovery AC-1.1 to AC-1.4, AC-2.2, AC-2.3;
// req-risk-based-review AC-11.3; req-operator-next-action AC-1.3, AC-1.6;
// DEC-117(4)).
func RunPlanGateRiskRecoveryConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	repo := "conveyor"
	if x.Config != nil && len(x.Config.Repos) > 0 {
		repo = x.Config.Repos[0].Name
	}
	plane := taskops.New(st)
	bound := config.DefaultRecoveryPolicy().AutomaticRecoveries
	planningTask := func(t *testing.T, mode riskpolicy.Mode) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Source: "api", Repo: repo, Title: id, Body: "Plan gate risk recovery " + id, BaseBranch: "main",
			Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, PolicyVersion: 1,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	// submit is the plan submission's own command. It records plan version 1
	// and takes the task to the plan approval gate, which creates the plan
	// version's risk order in the same act.
	submit := func(t *testing.T, taskID string) core.RiskOrder {
		t.Helper()
		outcome, err := plane.Perform(ctx, taskID, taskops.Command{
			Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
			PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
			PlanVersion: &core.SpecVersion{Content: planRiskPlan("", ""), Acceptance: []byte(`[]`), Decomposition: []byte(`[]`), Agent: "codex", Model: "planner"},
		})
		requireOK(t, err)
		if outcome.Task.State != core.TaskAwaiting || outcome.Task.GateKind != core.GateKindPlanApproval {
			t.Fatalf("submitted task=%s/%s", outcome.Task.State, outcome.Task.GateKind)
		}
		orders, err := st.ListTaskRiskOrders(ctx, taskID)
		requireOK(t, err)
		if len(orders) != 1 || orders[0].Subject.PlanVersion != 1 || orders[0].Order.State != core.WorkOrderQueued {
			t.Fatalf("plan risk orders=%+v, want one queued order for version 1", orders)
		}
		return orders[0]
	}
	claim := func(t *testing.T, orderID, session string, lease time.Duration) {
		t.Helper()
		_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{ClaimantID: "run:plan-gate-risk", SessionID: session, ClientToken: session + "-token",
			Agent: "claude-code", Model: "risk-model", Lease: lease, ExecutionTimeout: lease})
		requireOK(t, err)
	}
	tick := func(t *testing.T, at time.Time) {
		t.Helper()
		_, err := plane.TickOrderClock(ctx, at)
		requireOK(t, err)
	}
	order := func(t *testing.T, id string) core.WorkOrder {
		t.Helper()
		current, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return current
	}
	// timeOut claims the order with a one-minute lease and execution timeout
	// and ticks the clock past both, as a risk agent that stopped mid-rating
	// leaves it.
	timeOut := func(t *testing.T, id, session string) time.Time {
		t.Helper()
		claim(t, id, session, time.Minute)
		at := time.Now().UTC().Add(2 * time.Minute)
		tick(t, at)
		return at
	}
	read := func(t *testing.T, taskID string, now time.Time) nextaction.NextAction {
		t.Helper()
		opts := store.NextActionOptions{Now: now, Capabilities: map[core.Capability]bool{
			core.CapabilityOperateGates: true, core.CapabilityRecoverWork: true,
		}}
		na, err := store.ReadNextAction(ctx, st, taskID, opts)
		requireOK(t, err)
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		opts.CompactOnly = true
		compact, err := store.ReadNextActions(ctx, st, []core.Task{task}, opts)
		requireOK(t, err)
		if got, want := nextaction.CompactFrom(compact[taskID]), nextaction.CompactFrom(na); !reflect.DeepEqual(got, want) {
			t.Fatalf("compact=%+v, full=%+v", got, want)
		}
		return na
	}
	// requireRow3a asserts a statement of row 3a: it names the risk order,
	// states the kind, and carries no plan approval choice or refusal.
	requireRow3a := func(t *testing.T, na nextaction.NextAction, actor, kind, orderID string) {
		t.Helper()
		if na.Actor != actor || na.Kind != kind {
			t.Fatalf("next action %s/%s reason=%q stall=%q, want %s/%s", na.Actor, na.Kind, na.Details.ReasonCode, na.Details.StallReason, actor, kind)
		}
		if na.CurrentOrder == nil || na.CurrentOrder.ID != orderID || na.CurrentOrder.Stage != string(core.StageRisk) {
			t.Fatalf("current order=%+v, want risk order %s", na.CurrentOrder, orderID)
		}
		for _, choice := range na.Choices {
			if choice.ID == "approve" || choice.ID == "redirect" || choice.ID == "reject" {
				t.Fatalf("row 3a offered the plan approval choice %s: %+v", choice.ID, na.Choices)
			}
		}
		for _, refusal := range na.Details.Refusals {
			if refusal.Choice == "approve" {
				t.Fatalf("row 3a listed the plan approval refusal: %+v", na.Details.Refusals)
			}
		}
		if na.Details.GateKind != "" {
			t.Fatalf("row 3a gate kind=%q", na.Details.GateKind)
		}
		wantReason := kind == nextaction.KindQueued || kind == nextaction.KindWorking
		if (na.Details.ReasonCode == nextaction.PlanRiskRating) != wantReason {
			t.Fatalf("%s reason=%q", kind, na.Details.ReasonCode)
		}
	}
	// requireGated asserts the task still waits at the plan approval gate
	// with plan version 1 unapproved, and that the approval route still
	// refuses while the order is open (component-plan-state invariant 10).
	requireGated := func(t *testing.T, taskID, orderID string) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		if task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanApproval || task.PlanState != core.PlanAwaitingApproval {
			t.Fatalf("task=%s/%s plan %s, want the plan approval gate", task.State, task.GateKind, task.PlanState)
		}
		spec, found, err := st.GetLatestSpecVersion(ctx, taskID)
		requireOK(t, err)
		if !found || spec.Version != 1 || spec.Approved {
			t.Fatalf("latest plan=%+v found=%t, want version 1 unapproved", spec, found)
		}
		var refusal *store.PlanRiskRatingOpenError
		if err := st.ApproveSpecVersion(ctx, taskID, 1); !errors.As(err, &refusal) || refusal.WorkOrderID != orderID || !errors.Is(err, store.ErrPlanRiskRatingOpen) {
			t.Fatalf("approval err=%v, want %s naming %s", err, store.PlanRiskRatingOpenCode, orderID)
		}
	}
	events := func(t *testing.T, taskID string) []core.Event {
		t.Helper()
		list, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		return list
	}
	countKind := func(list []core.Event, kind string) int {
		n := 0
		for _, event := range list {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	// requireNoTaskAct asserts that nothing after the submission applied a
	// task command or recorded an approval: no task.state_changed event and
	// no spec.version_approved event follows the ones the submission wrote.
	requireNoTaskAct := func(t *testing.T, taskID string, stateChanges int) {
		t.Helper()
		list := events(t, taskID)
		if got := countKind(list, "task.state_changed"); got != stateChanges {
			t.Fatalf("task.state_changed events=%d, want %d after submission", got, stateChanges)
		}
		if got := countKind(list, "spec.version_approved"); got != 0 {
			t.Fatalf("spec.version_approved events=%d, want none", got)
		}
	}
	// recoveredEvent returns the automatic recovery event with the number.
	recoveredEvent := func(t *testing.T, taskID, orderID string, number int) {
		t.Helper()
		found := 0
		for _, event := range events(t, taskID) {
			if event.Kind != "work_order.recovered" {
				continue
			}
			var payload struct {
				WorkOrderID    string `json:"work_order_id"`
				Automatic      bool   `json:"automatic"`
				RecoveryNumber int    `json:"recovery_number"`
				WindowStarted  string `json:"window_started_at"`
				RequestID      string `json:"request_id"`
			}
			requireOK(t, json.Unmarshal(event.Payload, &payload))
			if payload.WorkOrderID != orderID || payload.RecoveryNumber != number {
				continue
			}
			if event.ActorID != "queue:"+orderID || !payload.Automatic || payload.WindowStarted == "" || payload.RequestID == "" {
				t.Fatalf("recovery event actor=%s payload=%+v", event.ActorID, payload)
			}
			found++
		}
		if found != 1 {
			t.Fatalf("automatic recovery %d events=%d, want one", number, found)
		}
	}

	t.Run("EnforceRecoversWithinTheBoundAndStops", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		risk := submit(t, task.ID)
		id := risk.Order.ID
		stateChanges := countKind(events(t, task.ID), "task.state_changed")
		requireRow3a(t, read(t, task.ID, time.Now().UTC()), nextaction.ActorAgent, nextaction.KindQueued, id)
		for number := 1; number <= bound; number++ {
			at := timeOut(t, id, "plan-gate-risk-"+string(rune('a'+number)))
			scheduled := order(t, id)
			if scheduled.State != core.WorkOrderTimedOut || scheduled.NextAutomaticRecoveryAt.IsZero() || !scheduled.NextAutomaticRecoveryAt.After(at) {
				t.Fatalf("recovery %d scheduled=%+v", number, scheduled)
			}
			na := read(t, task.ID, at)
			requireRow3a(t, na, nextaction.ActorFactory, nextaction.KindRetrying, id)
			if na.Deadline == nil || !na.Deadline.Equal(scheduled.NextAutomaticRecoveryAt) {
				t.Fatalf("retrying deadline=%v want %s", na.Deadline, scheduled.NextAutomaticRecoveryAt)
			}
			requireGated(t, task.ID, id)
			tick(t, scheduled.NextAutomaticRecoveryAt)
			recovered := order(t, id)
			if recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != number || recovered.RetrySuppressed || !recovered.NextAutomaticRecoveryAt.IsZero() {
				t.Fatalf("recovery %d recovered=%+v", number, recovered)
			}
			recoveredEvent(t, task.ID, id, number)
			requireRow3a(t, read(t, task.ID, scheduled.NextAutomaticRecoveryAt), nextaction.ActorAgent, nextaction.KindQueued, id)
			requireGated(t, task.ID, id)
		}
		// The bound is reached: the next stop is not recovered. The clock
		// suppresses the order with one exhaustion record and leaves it timed
		// out, and later ticks add nothing.
		at := timeOut(t, id, "plan-gate-risk-last")
		exhausted := order(t, id)
		if exhausted.State != core.WorkOrderTimedOut || !exhausted.RetrySuppressed || exhausted.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason || !exhausted.NextAutomaticRecoveryAt.IsZero() {
			t.Fatalf("exhausted=%+v", exhausted)
		}
		tick(t, at.Add(3*time.Hour))
		if got := countKind(events(t, task.ID), "work_order.recovery_exhausted"); got != 1 {
			t.Fatalf("recovery_exhausted events=%d, want one", got)
		}
		if got := countKind(events(t, task.ID), "work_order.recovered"); got != bound {
			t.Fatalf("recovered events=%d, want %d", got, bound)
		}
		na := read(t, task.ID, at)
		requireRow3a(t, na, nextaction.ActorOperator, nextaction.KindStalled, id)
		if na.Details.StallReason != nextaction.StallAutomaticRecoveryExhausted {
			t.Fatalf("stall=%q", na.Details.StallReason)
		}
		recover := false
		for _, choice := range na.Choices {
			recover = recover || choice.ID == "recover"
		}
		if !recover {
			t.Fatalf("stalled choices=%+v refusals=%+v, want recover", na.Choices, na.Details.Refusals)
		}
		requireGated(t, task.ID, id)
		// The offered recover is the act the route accepts. It starts a new
		// window and still leaves the gate as it is.
		reset, err := RecoverWorkOrder(ctx, st, id, "plan-gate-risk-operator", time.Hour)
		requireOK(t, err)
		if reset.State != core.WorkOrderQueued || reset.AutomaticRecoveryCount != 0 || reset.RetrySuppressed {
			t.Fatalf("operator recovery=%+v", reset)
		}
		requireRow3a(t, read(t, task.ID, time.Now().UTC()), nextaction.ActorAgent, nextaction.KindQueued, id)
		requireGated(t, task.ID, id)
		requireNoTaskAct(t, task.ID, stateChanges)
	})

	t.Run("RecordLeavesTheTimedOutOrderAlone", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeRecord)
		risk := submit(t, task.ID)
		id := risk.Order.ID
		stateChanges := countKind(events(t, task.ID), "task.state_changed")
		at := timeOut(t, id, "plan-gate-risk-record")
		for _, later := range []time.Time{at, at.Add(time.Hour), at.Add(72 * time.Hour)} {
			tick(t, later)
			current := order(t, id)
			if current.State != core.WorkOrderTimedOut || !current.NextAutomaticRecoveryAt.IsZero() || current.AutomaticRecoveryCount != 0 || current.RetrySuppressed {
				t.Fatalf("record order after a tick at %s=%+v", later, current)
			}
		}
		list := events(t, task.ID)
		for _, kind := range []string{"work_order.recovered", "work_order.redispatched", "work_order.recovery_exhausted"} {
			if got := countKind(list, kind); got != 0 {
				t.Fatalf("record task recorded %d %s events", got, kind)
			}
		}
		// Under record no risk order decides the approval, so the gate row
		// states the operator's plan approval.
		na := read(t, task.ID, at)
		if na.Actor != nextaction.ActorOperator || na.Kind != nextaction.KindPlanApproval {
			t.Fatalf("record next action %s/%s, want the plan approval", na.Actor, na.Kind)
		}
		requireNoTaskAct(t, task.ID, stateChanges)
	})

	t.Run("EnforceLeavesAHeadRiskOrderAlone", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		risk := submit(t, task.ID)
		head, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			created, made, createErr := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{HeadSHA: "plan-gate-head", BaseSHA: "plan-gate-base"}})
			if createErr == nil && !made {
				t.Fatalf("no risk order was created for the head")
			}
			return created, createErr
		})
		requireOK(t, err)
		at := timeOut(t, head.ID, "plan-gate-risk-head")
		tick(t, at.Add(3*time.Hour))
		current := order(t, head.ID)
		if current.State != core.WorkOrderTimedOut || !current.NextAutomaticRecoveryAt.IsZero() || current.AutomaticRecoveryCount != 0 {
			t.Fatalf("head risk order=%+v, want it left timed out", current)
		}
		// The plan version's order, still queued, is the one row 3a states.
		requireRow3a(t, read(t, task.ID, at), nextaction.ActorAgent, nextaction.KindQueued, risk.Order.ID)
	})

	t.Run("EnforceLeavesALiveClaimAndAnUnansweredCheckpointAlone", func(t *testing.T) {
		task := planningTask(t, riskpolicy.ModeEnforce)
		risk := submit(t, task.ID)
		id := risk.Order.ID
		session := "plan-gate-risk-checkpoint"
		claim(t, id, session, time.Hour)
		requireRow3a(t, read(t, task.ID, time.Now().UTC()), nextaction.ActorAgent, nextaction.KindWorking, id)
		tick(t, time.Now().UTC().Add(2*time.Minute))
		if live := order(t, id); live.State != core.WorkOrderClaimed || !live.NextAutomaticRecoveryAt.IsZero() {
			t.Fatalf("live claim=%+v, want it left claimed", live)
		}
		request := "Does the rating cover the migration step of the plan?"
		_, err := ReleaseWorkerClaim(ctx, st, id, "run:plan-gate-risk", core.WorkOrderRelease{
			SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
			Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: request},
		})
		requireOK(t, err)
		later := time.Now().UTC().Add(3 * time.Hour)
		tick(t, later)
		released := order(t, id)
		if released.State != core.WorkOrderQueued || released.Checkpoint == nil || !released.NextAutomaticRecoveryAt.IsZero() || released.AutomaticRecoveryCount != 0 {
			t.Fatalf("released order=%+v, want it left at its checkpoint", released)
		}
		na := read(t, task.ID, later)
		requireRow3a(t, na, nextaction.ActorOperator, nextaction.KindAgentQuestion, id)
		if !na.Judgment || na.Details.Checkpoint != request {
			t.Fatalf("agent question judgment=%t checkpoint=%q", na.Judgment, na.Details.Checkpoint)
		}
		answer := false
		for _, choice := range na.Choices {
			answer = answer || choice.ID == "answer"
		}
		if !answer {
			t.Fatalf("agent question choices=%+v, want answer", na.Choices)
		}
		requireGated(t, task.ID, id)
		// The answer recovers the order with direction, and the row returns
		// to the queued order.
		_, err = RecoverWorkOrderWithDirection(ctx, st, id, "plan-gate-risk-answer", "Rate the plan as written.", time.Hour)
		requireOK(t, err)
		requireRow3a(t, read(t, task.ID, time.Now().UTC()), nextaction.ActorAgent, nextaction.KindQueued, id)
		requireGated(t, task.ID, id)
	})
}
