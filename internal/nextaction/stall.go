package nextaction

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// StallHit is one deterministic reason a queued, stale, or timed-out order
// cannot be claimed. DependencyUnsatisfiable is row 13, not a stall reason.
type StallHit struct {
	Order                   core.WorkOrder
	Reason                  string
	DependencyUnsatisfiable bool
}

// Stall walks queued, stale, and timed-out orders newest-first. An order whose
// automatic recovery is still in the future is skipped. The first hit wins.
func Stall(task core.Task, orders []core.WorkOrder, recovery map[string]AutomaticRecovery, claimants *ClaimantView, now time.Time) *StallHit {
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		switch order.State {
		case core.WorkOrderQueued, core.WorkOrderStale, core.WorkOrderTimedOut:
		default:
			continue
		}
		rec := recovery[order.ID]
		if rec.NextAt != nil && rec.NextAt.After(now) {
			continue
		}
		reason := stallReason(task, order, claimants, rec)
		if len(order.UnsatisfiableTaskIDs) > 0 {
			return &StallHit{Order: order, Reason: reason, DependencyUnsatisfiable: true}
		}
		if reason != "" {
			return &StallHit{Order: order, Reason: reason}
		}
	}
	return nil
}

func stallReason(task core.Task, order core.WorkOrder, claimants *ClaimantView, rec AutomaticRecovery) string {
	switch {
	case order.RetrySuppressed:
		return StallRetrySuppressed
	case order.State == core.WorkOrderStale:
		return StallStale
	case order.State == core.WorkOrderTimedOut:
		return StallTimedOut
	case unclaimable(task, order, claimants):
		return StallUnclaimable
	case order.AutomaticRetryCount >= 2 && failureText(order) != "":
		return StallDispatchFailing
	case rec.Exhausted:
		return StallAutomaticRecoveryExhausted
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
