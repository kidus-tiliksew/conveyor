package nextaction

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// StallHit is one deterministic reason a queued or timed-out order cannot be
// claimed. DependencyUnsatisfiable is row 13, not a stall reason. Waiting
// unclaimed is never a stall reason: an order nobody has claimed reads as
// waiting for an agent, and a stored stale order reads as queued until the
// clock returns it (req-task-lifecycle-and-queue AC-9.4, AC-9.5;
// component-next-action invariant 5).
type StallHit struct {
	Order                   core.WorkOrder
	Reason                  string
	DependencyUnsatisfiable bool
}

// Stall walks queued and timed-out orders newest-first. An order whose
// automatic recovery is still in the future is skipped. The first hit wins.
func Stall(task core.Task, orders []core.WorkOrder, recovery map[string]AutomaticRecovery, claimants *ClaimantView, now time.Time) *StallHit {
	return stall(task, orders, recovery, claimants, nil, now)
}

// stallIn is Stall over the projection input, including its claim-failure
// records (feature-operator-next-action NA-3, Claim-failure stall).
func stallIn(in Input) *StallHit {
	return stall(in.Task, in.Orders, in.Recovery, in.LiveClaimants, in.ClaimFailures, in.Now)
}

func stall(task core.Task, orders []core.WorkOrder, recovery map[string]AutomaticRecovery, claimants *ClaimantView, failures map[string]core.WorkOrderClaimFailure, now time.Time) *StallHit {
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		if order.State == core.WorkOrderStale {
			order = core.NormalizeLegacyStaleWorkOrder(order, now)
		}
		switch order.State {
		case core.WorkOrderQueued, core.WorkOrderTimedOut:
		default:
			continue
		}
		rec := recovery[order.ID]
		if rec.NextAt != nil && rec.NextAt.After(now) {
			continue
		}
		reason := stallReason(task, order, claimants, rec, failures[order.ID])
		if len(order.UnsatisfiableTaskIDs) > 0 {
			return &StallHit{Order: order, Reason: reason, DependencyUnsatisfiable: true}
		}
		if reason != "" {
			return &StallHit{Order: order, Reason: reason}
		}
	}
	return nil
}

func stallReason(task core.Task, order core.WorkOrder, claimants *ClaimantView, rec AutomaticRecovery, failure core.WorkOrderClaimFailure) string {
	switch {
	case rec.Exhausted:
		return StallAutomaticRecoveryExhausted
	case order.RetrySuppressed:
		return StallRetrySuppressed
	case order.State == core.WorkOrderTimedOut:
		return StallTimedOut
	case unclaimable(task, order, claimants):
		return StallUnclaimable
	case claimFailing(order, failure):
		return StallClaimFailing
	case order.AutomaticRetryCount >= 2 && failureText(order) != "":
		return StallDispatchFailing
	default:
		return ""
	}
}

func unclaimable(task core.Task, order core.WorkOrder, claimants *ClaimantView) bool {
	if order.RecoveryRefusal != "" {
		return true
	}
	if order.State == core.WorkOrderQueued && order.Stage == core.StageVerify && order.RecoveryRefusal == "" {
		if err := core.ValidateVerifyDispatch(task, order); err != nil {
			return true
		}
	}
	if claimants == nil || task.Assignee == nil || task.Assignee.UserID == "" {
		return false
	}
	for _, id := range claimants.UserIDs {
		if id == task.Assignee.UserID {
			return false
		}
	}
	return true
}

func failureText(order core.WorkOrder) string {
	if text := trim(order.LastFailureMessage); text != "" {
		return text
	}
	return trim(order.LastFailureDetail)
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// claimFailing reports an observed stall: a queued order whose consecutive
// claims failed in the backend reached the threshold. Deterministic reasons
// are checked first and keep winning (feature-operator-next-action NA-3,
// Claim-failure stall; req-operator-next-action AC-1.8).
func claimFailing(order core.WorkOrder, failure core.WorkOrderClaimFailure) bool {
	return order.State == core.WorkOrderQueued && failure.WorkOrderID == order.ID && failure.ConsecutiveFailures >= ClaimFailureStallThreshold
}
