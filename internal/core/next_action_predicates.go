package core

import "fmt"

// RecoverableQueuedWorkOrder is the recover route's queued-order eligibility:
// a queued order is recoverable only after an attempt ended, retry was
// suppressed, or a retry was scheduled. An order an operator already
// recovered has none of these (component-work-orders, Recovery).
func RecoverableQueuedWorkOrder(order WorkOrder) bool {
	return order.State == WorkOrderQueued && (order.LastAttemptOutcome != "" || order.RetrySuppressed || !order.NextRetryAt.IsZero())
}

// WorkOrderRecoverStateRefusal is the recover route's state refusal on a
// clock-projected order. The memory, PostgreSQL, and SingleStore recover
// paths and NextActionRecoverRefusal all call it, so an offered recover-route
// choice is one the route accepts (req-operator-next-action AC-1.4).
func WorkOrderRecoverStateRefusal(order WorkOrder) string {
	if RecoverableQueuedWorkOrder(order) || order.State == WorkOrderStale || order.State == WorkOrderTimedOut {
		return ""
	}
	return fmt.Sprintf("work order %s is not released, expired, or retry-suppressed", order.ID)
}

// NextActionRecoverRefusal is the recover route's state check. A claimed
// order is refused. Empty means the route would accept the order's state;
// a stamped RecoveryRefusal is returned as the refusal text.
func NextActionRecoverRefusal(order WorkOrder) string {
	if order.State == WorkOrderClaimed {
		return fmt.Sprintf("work order %s cannot be recovered while it is claimed", order.ID)
	}
	if order.RecoveryRefusal != "" {
		return order.RecoveryRefusal
	}
	switch order.State {
	case WorkOrderQueued, WorkOrderStale, WorkOrderTimedOut:
		return WorkOrderRecoverStateRefusal(order)
	default:
		return fmt.Sprintf("work order %s cannot be recovered from %s", order.ID, order.State)
	}
}

// NextActionPlainApproveRefusal refuses a plain approve while a plan revision
// is the gate. Plan-revision reason codes are the route's own choices.
func NextActionPlainApproveRefusal(gate GateKind) string {
	if gate == GateKindPlanRevision {
		return "plain approve is refused while a plan revision is pending"
	}
	return ""
}

// ApproveWithoutReviewCode is the refusal code of an approve at a gate that
// no review reached. The review route answers 409 with it, and the next
// action states it in details.refusals beside the plain sentence, which
// carries no identifier (component-next-action invariants 7, 18;
// component-task-lifecycle, Extending this component).
const ApproveWithoutReviewCode = "approve_without_review"

// ApproveWithoutReviewRefusal is the plain sentence of ApproveWithoutReviewCode.
const ApproveWithoutReviewRefusal = "Approve is refused because no review approved the task's current state."

// ReviewOutputInvalidSource is the recorded source of a bounce limit that
// review reached with invalid output.
const ReviewOutputInvalidSource = "review.output_invalid"

// ReviewSourcedBounceLimit reports whether a review_bounce_limit gate's
// recorded source names review: review's invalid output, or no source,
// which the review-round bounce limit records. Any other value, including
// one a history read cut to its first characters, is not review's. The
// comparison is exact, so a cut value is never read as review's.
func ReviewSourcedBounceLimit(source string) bool {
	return source == "" || source == ReviewOutputInvalidSource
}

// NextActionUnreviewedApproveRefusal is the one predicate that refuses an
// approve at a gate no review reached. The review route and the next action
// both call it with the awaiting task and the recorded gate source, so the
// offer and the route agree (component-next-action invariant 7;
// req-operator-next-action AC-1.4). It returns ApproveWithoutReviewRefusal,
// or empty when the predicate admits the approve:
//
//   - At review_bounce_limit, an approve is admitted only when the recorded
//     source is review's (ReviewSourcedBounceLimit). Another stage's limit
//     would otherwise bind a head from an earlier review round that review
//     rejected.
//   - At job_failure, an approve is never admitted. The failure entered the
//     gate, so any reviewed head the task holds was recorded before the
//     failure, or at the implementation submission before any review: no
//     review approved the current state. An awaiting task without a recorded
//     gate kind states job_failure, so it is refused the same way.
//
// Plan approval, plan revision, and merge approval are untouched.
func NextActionUnreviewedApproveRefusal(task Task, gateSource string) string {
	if task.State != TaskAwaiting {
		return ""
	}
	switch task.GateKind {
	case GateKindReviewBounceLimit:
		if ReviewSourcedBounceLimit(gateSource) {
			return ""
		}
		return ApproveWithoutReviewRefusal
	case GateKindJobFailure, "":
		return ApproveWithoutReviewRefusal
	}
	return ""
}

// NextActionSupersessionRefusal refuses an approve when the gate was entered
// against a spec version that is no longer current.
func NextActionSupersessionRefusal(gateSpecVersion, latest int) string {
	if latest > 0 && gateSpecVersion != latest {
		return fmt.Sprintf("approve is refused because spec v%d superseded the gate's v%d", latest, gateSpecVersion)
	}
	return ""
}

// Refusal texts of POST /v1/tasks/{id}/redispatch. The route answers 409 with
// them and NextActionRedispatchRefusal states them, so a withheld choice names
// the route's own failing condition (req-operator-next-action AC-1.4). The two
// plan-revision texts are the route's alone, as NextActionRedispatchRefusal
// says.
const (
	TaskRedispatchActiveRefusal  = "task is already active"
	TaskRedispatchNoStageRefusal = "task has no decided next stage; record an explicit review redirect instead"
	// TaskRedispatchPlanRevisionRefusal answers a pending plan-revision
	// decision (component-work-orders invariant 39).
	TaskRedispatchPlanRevisionRefusal = "task is waiting at the plan-revision decision gate; record the plan-revision decision instead"
	// TaskRedispatchPlanRevisionUnreadableRefusal answers a gate whose events
	// cannot be read. It is fixed so that no event, driver, or decode text
	// reaches the response (component-work-orders, State and failure).
	TaskRedispatchPlanRevisionUnreadableRefusal = "task has an unreadable plan-revision request; use the plan-revision decision gate"
	// TaskHeldByWorkOrderRefusal is the text of store.ErrTaskHeldByWorkOrder.
	TaskHeldByWorkOrderRefusal = "task stage is held by a work order; redispatch or recover the work order instead"
)

// TaskRedispatchStage is the stage POST /v1/tasks/{id}/redispatch resumes at
// for a task that is neither running nor queued. A triage park records the
// stage in RecoveryStage and clears NextStage (component-http-api).
func TaskRedispatchStage(task Task) Stage {
	if task.State == TaskParked && task.NextStage == "" {
		return task.RecoveryStage
	}
	return task.NextStage
}

// TaskRedispatchCommand is the transition POST /v1/tasks/{id}/redispatch
// applies to a task that is neither running nor queued.
func TaskRedispatchCommand(state TaskState) TaskCommand {
	switch state {
	case TaskAwaiting:
		return TaskInterventionRedirect
	case TaskApproved:
		return TaskRefreshReview
	case TaskClaiming:
		return TaskIntakeFinalize
	default:
		return TaskRecover
	}
}

// NextActionRedispatchRefusal states why POST /v1/tasks/{id}/redispatch would
// answer 409 for this task and its orders, and is empty when the route would
// accept. It tests the route's own conditions and no others: a running task,
// a queued task whose stage a work order holds, a task with no stage to resume
// at, and a refused transition. orders are the task's clock-projected orders
// sorted by (created_at, id) (req-operator-next-action AC-1.4; DEC-61;
// component-next-action, Invariants 7; component-durable-queue).
//
// The route's plan-revision gate refusals are not tested here, because the
// route reads that gate from the task's events, which this predicate does not
// hold. No statement needs them: every awaiting task states its gate, and no
// gate's choices name the redispatch route.
func NextActionRedispatchRefusal(task Task, orders []WorkOrder) string {
	switch task.State {
	case TaskRunning:
		return TaskRedispatchActiveRefusal
	case TaskQueued:
		if holding, held := WorkOrderHoldsStage(withStoredClaims(orders), task.NextStage); held {
			return fmt.Sprintf("%s: task %s waits on work order %s", TaskHeldByWorkOrderRefusal, task.ID, holding.ID)
		}
		return ""
	}
	if TaskRedispatchStage(task) == "" {
		return TaskRedispatchNoStageRefusal
	}
	if _, err := TransitionTask(task.State, TaskRedispatchCommand(task.State)); err != nil {
		return err.Error()
	}
	return ""
}

// withStoredClaims returns orders with each claim whose elapsed expiry only a
// read has applied restored to claimed. The route's hold check reads stored
// states, where such a claim still holds its stage until the order clock
// persists the expiry (WorkOrderHoldsStage).
func withStoredClaims(orders []WorkOrder) []WorkOrder {
	out := append([]WorkOrder(nil), orders...)
	for i := range out {
		if out[i].ClaimExpiryUnpersisted {
			out[i].State = WorkOrderClaimed
		}
	}
	return out
}

// NextActionChangePolicyRefusal offers policy change only when a recovery
// refusal is set.
func NextActionChangePolicyRefusal(recoveryRefusal string) string {
	if recoveryRefusal == "" {
		return "change policy is refused without a recovery refusal"
	}
	return ""
}
