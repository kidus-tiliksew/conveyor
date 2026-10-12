package core

import "fmt"

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
		return ""
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

// NextActionRedispatchRefusal is the redispatch route's order-state check.
func NextActionRedispatchRefusal(order WorkOrder) string {
	switch order.State {
	case WorkOrderQueued, WorkOrderStale, WorkOrderTimedOut, WorkOrderCancelled:
		return ""
	default:
		return fmt.Sprintf("work order %s cannot be redispatched from %s", order.ID, order.State)
	}
}

// NextActionChangePolicyRefusal offers policy change only when a recovery
// refusal is set.
func NextActionChangePolicyRefusal(recoveryRefusal string) string {
	if recoveryRefusal == "" {
		return "change policy is refused without a recovery refusal"
	}
	return ""
}
