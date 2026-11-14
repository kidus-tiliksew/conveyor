package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

var waitingNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// requireWaitingForAgent proves a statement only waits for an agent: no stall,
// no health warning or operator act, no choice, and no deadline
// (req-task-lifecycle-and-queue AC-9.4; component-next-action invariants 5 and
// 14).
func requireWaitingForAgent(t *testing.T, name string, na NextAction) {
	t.Helper()
	if na.Kind != KindQueued || na.Actor != ActorAgent || na.Judgment || na.Details.StallReason != "" || len(na.Choices) != 0 || na.Deadline != nil {
		t.Fatalf("%s: next action %s/%s judgment=%t stall=%q choices=%+v deadline=%v, want an order waiting for an agent", name, na.Actor, na.Kind, na.Judgment, na.Details.StallReason, na.Choices, na.Deadline)
	}
	if na.CurrentOrder == nil || na.CurrentOrder.State != string(core.WorkOrderQueued) {
		t.Fatalf("%s: current order=%+v, want the queued order", name, na.CurrentOrder)
	}
}

// An order nobody has claimed, two days after it entered the queue and a day
// past the deadline the retired queue clock stored, reads as waiting for an
// agent on every stage, the risk stage included with no special case
// (req-task-lifecycle-and-queue AC-9.4, AC-9.5; DEC-117(2);
// feature-risk-based-review RR-15).
func TestOrderWaitingPastFormerQueueDeadlineReadsAsQueued(t *testing.T) {
	entered, formerDeadline := waitingNow.Add(-48*time.Hour), waitingNow.Add(-24*time.Hour)
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview} {
		order := core.WorkOrder{ID: "t-" + string(stage) + "-1", TaskID: "t", Stage: stage, State: core.WorkOrderQueued, Claimable: true, HeadSHA: "head",
			QueueEnteredAt: entered, QueueDeadline: formerDeadline}
		if stage == core.StageReview {
			order.ReviewRound, order.ReviewSeat = 1, 1
		}
		task := core.Task{ID: "t", Title: "Waiting", State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "head"}
		task.SetupContract.VerifyStage = stage == core.StageVerify
		requireWaitingForAgent(t, string(stage), Project(Input{Task: task, Orders: []core.WorkOrder{order}, Capabilities: allCaps(), Now: waitingNow}))
	}

	risk := riskOrder("t-risk-1", core.WorkOrderQueued)
	risk.QueueEnteredAt, risk.QueueDeadline = riskWaitNow.Add(-48*time.Hour), riskWaitNow.Add(-24*time.Hour)
	in := planRiskInput(riskpolicy.ModeEnforce, core.WorkOrderQueued)
	in.Orders, in.PlanRiskOrder.Order = []core.WorkOrder{risk}, risk
	na := Project(in)
	if na.Kind != KindQueued || na.Actor != ActorAgent || na.Details.StallReason != "" || len(na.Choices) != 0 || na.Deadline != nil {
		t.Fatalf("risk: next action %s/%s stall=%q choices=%+v deadline=%v, want the risk order waiting for an agent", na.Actor, na.Kind, na.Details.StallReason, na.Choices, na.Deadline)
	}
}

// A stored order that still holds the retired stale value reads as the queued
// order the clock returns it to. Age-only automatic recovery signals on it
// state no retry, exhaustion, or operator stop (component-work-orders
// invariant 5; req-task-lifecycle-and-queue AC-9.4).
func TestStoredStaleOrderReadsAsWaiting(t *testing.T) {
	scheduled := waitingNow.Add(time.Hour)
	for name, order := range map[string]core.WorkOrder{
		"plain": {ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderStale},
		"queue-only recovery signals": {ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderStale,
			RetrySuppressed: true, RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason, AutomaticRecoveryCount: 2,
			AutomaticRecoveryWindowStartedAt: waitingNow.Add(-time.Hour), NextAutomaticRecoveryAt: scheduled},
	} {
		order.QueueEnteredAt, order.QueueDeadline = waitingNow.Add(-48*time.Hour), waitingNow.Add(-24*time.Hour)
		task := core.Task{ID: "t", Title: "Legacy", State: core.TaskRunning, NextStage: core.StageImplement}
		requireWaitingForAgent(t, name, Project(Input{Task: task, Orders: []core.WorkOrder{order}, Capabilities: allCaps(), Now: waitingNow}))
		if hit := Stall(task, []core.WorkOrder{order}, nil, nil, waitingNow); hit != nil {
			t.Fatalf("%s: stall=%+v, want none", name, hit)
		}
	}
}

// Independent operator stops stay visible on a stored stale order: genuine
// attempt evidence keeps its stall, and an unanswered checkpoint keeps its
// agent question (DEC-117(3); component-next-action invariant 3).
func TestStoredStaleOrderKeepsIndependentStops(t *testing.T) {
	task := core.Task{ID: "t", Title: "Legacy", State: core.TaskRunning, NextStage: core.StageImplement}
	expired := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderStale,
		LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeExpired, RetrySuppressed: true, RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason}
	na := Project(Input{Task: task, Orders: []core.WorkOrder{expired}, Capabilities: allCaps(), Now: waitingNow})
	if na.Kind != KindStalled || na.Details.StallReason != StallRetrySuppressed {
		t.Fatalf("expired attempt: next action %s/%s stall=%q, want the retry-suppressed stall", na.Actor, na.Kind, na.Details.StallReason)
	}

	checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: "Which branch should the fix target?"}
	released := core.WorkOrder{ID: "t-implement-1", TaskID: "t", Stage: core.StageImplement, State: core.WorkOrderStale,
		LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased, RetrySuppressed: true, Checkpoint: checkpoint}
	na = Project(Input{Task: task, Orders: []core.WorkOrder{released}, Markers: Markers{CheckpointDecision: true}, Capabilities: allCaps(), Now: waitingNow})
	if na.Kind != KindAgentQuestion || na.Actor != ActorOperator {
		t.Fatalf("checkpoint: next action %s/%s, want the agent question", na.Actor, na.Kind)
	}
}
