package store

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// AutomaticRecoveryKind is the pure decision for one order at one clock tick.
type AutomaticRecoveryKind string

const (
	AutomaticRecoveryNone       AutomaticRecoveryKind = "none"
	AutomaticRecoverySchedule   AutomaticRecoveryKind = "schedule"
	AutomaticRecoveryRecover    AutomaticRecoveryKind = "recover"
	AutomaticRecoveryRedispatch AutomaticRecoveryKind = "redispatch"
	AutomaticRecoveryExhausted  AutomaticRecoveryKind = "exhausted"
)

// AutomaticRecoveryDecision is the first matching AR-3 row. Number is the
// window-local recovery about to be scheduled or performed, or the count that
// hit the bound when Kind is exhausted. At is the schedule time, or now for
// an act. WindowStartedAt is the window the act will stamp.
type AutomaticRecoveryDecision struct {
	Kind            AutomaticRecoveryKind
	At              time.Time
	Number          int
	WindowStartedAt time.Time
}

// DecideAutomaticRecovery is the shared clock decision. The first matching
// row wins. Callers fill order.RecoveryRefusal from WorkOrderRecoveryRefusal
// before calling, and apply deadline transitions before that.
func DecideAutomaticRecovery(policy config.RecoveryPolicy, task core.Task, order core.WorkOrder, now time.Time) AutomaticRecoveryDecision {
	if task.State != core.TaskQueued && task.State != core.TaskRunning || core.TaskTerminal(task.State) {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	if order.State == core.WorkOrderClaimed && liveClaim(order, now) {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	if order.RetrySuppressionReason != "" && order.RetrySuppressionReason != core.AttemptRetriesExhaustedReason {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	if order.Checkpoint != nil {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	if order.RecoveryRefusal != "" {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	if policy.AutomaticRecoveries == 0 {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	recoverable, redispatch := recoverableOrder(order, now)
	if !recoverable {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	count := order.AutomaticRecoveryCount
	windowStart := order.AutomaticRecoveryWindowStartedAt
	if !windowStart.IsZero() && !now.Before(windowStart.Add(policy.Window)) {
		count = 0
		windowStart = time.Time{}
	}
	if count >= policy.AutomaticRecoveries {
		return AutomaticRecoveryDecision{
			Kind:            AutomaticRecoveryExhausted,
			Number:          count,
			WindowStartedAt: order.AutomaticRecoveryWindowStartedAt,
		}
	}
	number := count + 1
	if order.NextAutomaticRecoveryAt.IsZero() {
		return AutomaticRecoveryDecision{
			Kind:            AutomaticRecoverySchedule,
			At:              now.Add(AutomaticRecoveryDelay(number)),
			Number:          number,
			WindowStartedAt: windowStart,
		}
	}
	if order.NextAutomaticRecoveryAt.After(now) {
		return AutomaticRecoveryDecision{Kind: AutomaticRecoveryNone}
	}
	kind := AutomaticRecoveryRecover
	if redispatch {
		kind = AutomaticRecoveryRedispatch
	}
	if windowStart.IsZero() {
		windowStart = now
	}
	return AutomaticRecoveryDecision{Kind: kind, At: now, Number: number, WindowStartedAt: windowStart}
}

// AutomaticRecoveryDelay is one minute times five to the power of n-1, capped
// at one hour. The loop refuses to multiply past the cap.
func AutomaticRecoveryDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	delay := time.Minute
	cap := time.Hour
	for i := 1; i < n; i++ {
		if delay > cap/5 {
			return cap
		}
		delay *= 5
		if delay > cap {
			return cap
		}
	}
	if delay > cap {
		return cap
	}
	return delay
}

// RecoverableStop is the allowlist ListElapsedWorkOrderTaskIDs mirrors in SQL.
// A set checkpoint or any suppression reason other than attempt-retry
// exhaustion is not recoverable. Claimed orders are recoverable only after
// the claim lease has expired; the clock's deadline pass turns those into
// queued orders with outcome expired before it decides.
func RecoverableStop(order core.WorkOrder, now time.Time) bool {
	ok, _ := recoverableOrder(order, now)
	return ok
}

func recoverableOrder(order core.WorkOrder, now time.Time) (bool, bool) {
	if order.Checkpoint != nil {
		return false, false
	}
	if order.RetrySuppressionReason != "" && order.RetrySuppressionReason != core.AttemptRetriesExhaustedReason {
		return false, false
	}
	switch order.State {
	case core.WorkOrderTimedOut:
		return true, false
	case core.WorkOrderStale:
		return true, true
	case core.WorkOrderQueued:
		if order.LastAttemptOutcome == core.WorkOrderOutcomeExpired {
			return true, false
		}
		if order.RetrySuppressed && order.RetrySuppressionReason == core.AttemptRetriesExhaustedReason {
			return true, false
		}
		return false, false
	case core.WorkOrderClaimed:
		if !liveClaim(order, now) && !order.LeaseExpiresAt.IsZero() {
			return true, false
		}
		return false, false
	default:
		return false, false
	}
}

func liveClaim(order core.WorkOrder, now time.Time) bool {
	return order.LeaseExpiresAt.IsZero() || order.LeaseExpiresAt.After(now)
}

// StampAutomaticRecovery applies the ledger of one automatic act. The window
// starts on the first recovery of the window. next_automatic_recovery_at is
// cleared. count is the stored count before this act; a rolled window counts
// as zero.
func StampAutomaticRecovery(order *core.WorkOrder, policy config.RecoveryPolicy, now time.Time) (number int, windowStart time.Time) {
	count := order.AutomaticRecoveryCount
	windowStart = order.AutomaticRecoveryWindowStartedAt
	if !windowStart.IsZero() && !now.Before(windowStart.Add(policy.Window)) {
		count = 0
		windowStart = time.Time{}
	}
	count++
	if windowStart.IsZero() {
		windowStart = now
	}
	order.AutomaticRecoveryCount = count
	order.AutomaticRecoveryWindowStartedAt = windowStart
	order.NextAutomaticRecoveryAt = time.Time{}
	return count, windowStart
}

// ResetAutomaticRecoveryLedger clears the three order columns. An operator
// recovery or redispatch calls it, which starts a new window.
func ResetAutomaticRecoveryLedger(order *core.WorkOrder) {
	order.AutomaticRecoveryCount = 0
	order.AutomaticRecoveryWindowStartedAt = time.Time{}
	order.NextAutomaticRecoveryAt = time.Time{}
}

// ClearAutomaticRecoveryExhaustion clears the exhaustion suppression an
// operator redispatch must not keep. Recover already clears every suppression.
func ClearAutomaticRecoveryExhaustion(order *core.WorkOrder) {
	if order.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason {
		order.RetrySuppressed = false
		order.RetrySuppressionReason = ""
	}
}

// AutomaticRecoveryRequestID is unique across operator resets because
// redispatch_count is monotonic and never reset. recoveryNumber is the
// window-local count and is not part of the id.
func AutomaticRecoveryRequestID(orderID string, redispatchCount int) string {
	return "auto-recovery/" + orderID + "/" + itoa(redispatchCount+1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
