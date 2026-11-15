package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// planGateTask is task t awaiting_human at gate under the frozen risk mode.
func planGateTask(mode riskpolicy.Mode, state core.TaskState, gate core.GateKind) core.Task {
	task := core.Task{ID: "t", State: state, GateKind: gate}
	if mode != "" {
		task.SetupContract = config.PolicyContract{Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
	}
	return task
}

func planGateRiskOrder(id string, state core.WorkOrderState, version int, head string) core.RiskOrder {
	order := core.RiskOrder{Order: core.WorkOrder{ID: id, TaskID: "t", Stage: core.StageRisk, State: state}}
	if head != "" {
		order.Subject = core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base-" + head}
	} else {
		order.Subject = core.RiskOrderSubject{PlanVersion: version}
	}
	return order
}

// PlanGateRiskOrderID names only the open risk order of the plan version the
// gate holds, of an enforcing task awaiting_human at plan_approval
// (component-work-orders invariant 46).
func TestPlanGateRiskOrderID(t *testing.T) {
	enforce := planGateTask(riskpolicy.ModeEnforce, core.TaskAwaiting, core.GateKindPlanApproval)
	current := planGateRiskOrder("t-risk-2", core.WorkOrderTimedOut, 2, "")
	for _, tc := range []struct {
		name   string
		task   core.Task
		orders []core.RiskOrder
		gate   int
		want   string
	}{
		{"timed-out order of the gate version", enforce, []core.RiskOrder{current}, 2, "t-risk-2"},
		{"queued order of the gate version", enforce, []core.RiskOrder{planGateRiskOrder("t-risk-2", core.WorkOrderQueued, 2, "")}, 2, "t-risk-2"},
		{"completed order", enforce, []core.RiskOrder{planGateRiskOrder("t-risk-2", core.WorkOrderCompleted, 2, "")}, 2, ""},
		{"cancelled order", enforce, []core.RiskOrder{planGateRiskOrder("t-risk-2", core.WorkOrderCancelled, 2, "")}, 2, ""},
		{"earlier plan version", enforce, []core.RiskOrder{planGateRiskOrder("t-risk-1", core.WorkOrderTimedOut, 1, "")}, 2, ""},
		{"head subject", enforce, []core.RiskOrder{planGateRiskOrder("t-risk-3", core.WorkOrderTimedOut, 0, "abc")}, 2, ""},
		{"no gate version", enforce, []core.RiskOrder{current}, 0, ""},
		{"record", planGateTask(riskpolicy.ModeRecord, core.TaskAwaiting, core.GateKindPlanApproval), []core.RiskOrder{current}, 2, ""},
		{"off", planGateTask(riskpolicy.ModeOff, core.TaskAwaiting, core.GateKindPlanApproval), []core.RiskOrder{current}, 2, ""},
		{"no policy", planGateTask("", core.TaskAwaiting, core.GateKindPlanApproval), []core.RiskOrder{current}, 2, ""},
		{"merge approval gate", planGateTask(riskpolicy.ModeEnforce, core.TaskAwaiting, core.GateKindMergeApproval), []core.RiskOrder{current}, 2, ""},
		{"plan revision gate", planGateTask(riskpolicy.ModeEnforce, core.TaskAwaiting, core.GateKindPlanRevision), []core.RiskOrder{current}, 2, ""},
		{"approved task", planGateTask(riskpolicy.ModeEnforce, core.TaskApproved, ""), []core.RiskOrder{current}, 2, ""},
		{"another task's order", enforce, []core.RiskOrder{func() core.RiskOrder {
			order := current
			order.Order.TaskID = "other"
			return order
		}()}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanGateRiskOrderID(tc.task, tc.orders, tc.gate); got != tc.want {
				t.Fatalf("PlanGateRiskOrderID=%q want %q", got, tc.want)
			}
		})
	}
}

// The decision applies to the plan gate's risk order under the bound of
// invariant 45, and every other exclusion of invariant 46 still applies. An
// order the exception does not name stays alone while the task is gated
// (req-bounded-automatic-recovery AC-1.1, AC-1.2, AC-1.4, AC-2.1 to AC-2.3;
// DEC-117(4)).
func TestDecideAutomaticRecoveryAtPlanGate(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	policy := recoveryPolicy(2, 24*time.Hour)
	due := now.Add(-time.Minute)
	gated := planGateTask(riskpolicy.ModeEnforce, core.TaskAwaiting, core.GateKindPlanApproval)
	risk := func(mutate func(*core.WorkOrder)) core.WorkOrder {
		order := core.WorkOrder{ID: "t-risk-1", TaskID: "t", Stage: core.StageRisk, State: core.WorkOrderTimedOut, QueueEnteredAt: now.Add(-time.Hour)}
		if mutate != nil {
			mutate(&order)
		}
		return order
	}
	for _, tc := range []struct {
		name   string
		task   core.Task
		order  core.WorkOrder
		id     string
		policy config.RecoveryPolicy
		want   AutomaticRecoveryKind
		number int
	}{
		{"schedules the first recovery", gated, risk(nil), "t-risk-1", policy, AutomaticRecoverySchedule, 1},
		{"recovers when due", gated, risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "t-risk-1", policy, AutomaticRecoveryRecover, 1},
		{"recovers an expired lease", gated, risk(func(o *core.WorkOrder) {
			o.State, o.LastAttemptOutcome, o.NextAutomaticRecoveryAt = core.WorkOrderQueued, core.WorkOrderOutcomeExpired, due
		}), "t-risk-1", policy, AutomaticRecoveryRecover, 1},
		{"stops at the bound", gated, risk(func(o *core.WorkOrder) {
			o.AutomaticRecoveryCount, o.AutomaticRecoveryWindowStartedAt, o.NextAutomaticRecoveryAt = 2, now.Add(-time.Hour), due
		}), "t-risk-1", policy, AutomaticRecoveryExhausted, 2},
		{"bound zero disables", gated, risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "t-risk-1", recoveryPolicy(0, 24*time.Hour), AutomaticRecoveryNone, 0},
		{"not the named order", gated, risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "t-risk-2", policy, AutomaticRecoveryNone, 0},
		{"no named order", gated, risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "", policy, AutomaticRecoveryNone, 0},
		{"another stage under the named ID", gated, risk(func(o *core.WorkOrder) { o.Stage, o.NextAutomaticRecoveryAt = core.StageSpec, due }), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"record", planGateTask(riskpolicy.ModeRecord, core.TaskAwaiting, core.GateKindPlanApproval), risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"merge approval gate", planGateTask(riskpolicy.ModeEnforce, core.TaskAwaiting, core.GateKindMergeApproval), risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"live claim", gated, risk(func(o *core.WorkOrder) {
			o.State, o.LeaseExpiresAt, o.NextAutomaticRecoveryAt = core.WorkOrderClaimed, now.Add(time.Minute), due
		}), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"unanswered checkpoint", gated, risk(func(o *core.WorkOrder) {
			o.State, o.LastAttemptOutcome, o.NextAutomaticRecoveryAt = core.WorkOrderQueued, core.WorkOrderOutcomeReleased, due
			o.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Which plan step does the rating cover?"}
		}), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"identical failure suppression", gated, risk(func(o *core.WorkOrder) {
			o.RetrySuppressed, o.RetrySuppressionReason, o.NextAutomaticRecoveryAt = true, core.IdenticalFailureSuppressionReason, due
		}), "t-risk-1", policy, AutomaticRecoveryNone, 0},
		{"recovery refusal", gated, risk(func(o *core.WorkOrder) {
			o.RecoveryRefusal, o.NextAutomaticRecoveryAt = "refused", due
		}), "t-risk-1", policy, AutomaticRecoveryNone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideAutomaticRecoveryAtPlanGate(tc.policy, tc.task, tc.order, tc.id, now)
			if got.Kind != tc.want || got.Number != tc.number {
				t.Fatalf("kind=%s number=%d want %s %d", got.Kind, got.Number, tc.want, tc.number)
			}
		})
	}
	// DecideAutomaticRecovery names no order, so it leaves every order of a
	// gated task alone, as before.
	if got := DecideAutomaticRecovery(policy, gated, risk(func(o *core.WorkOrder) { o.NextAutomaticRecoveryAt = due }), now); got.Kind != AutomaticRecoveryNone {
		t.Fatalf("DecideAutomaticRecovery on a gated task kind=%s", got.Kind)
	}
}
