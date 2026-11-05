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
// the route's own failing condition (req-operator-next-action AC-1.4).
const (
	TaskRedispatchActiveRefusal  = "task is already active"
	TaskRedispatchNoStageRefusal = "task has no decided next stage; record an explicit review redirect instead"
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
