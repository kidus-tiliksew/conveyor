package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// AutomaticReviewActKind is the AR-5 review-round decision for one task.
type AutomaticReviewActKind string

const (
	AutomaticReviewNone    AutomaticReviewActKind = "none"
	AutomaticReviewRetry   AutomaticReviewActKind = "retry"
	AutomaticReviewRecover AutomaticReviewActKind = "recover"
)

// AutomaticReviewDecision names the act and the latest round it applies to.
type AutomaticReviewDecision struct {
	Kind  AutomaticReviewActKind
	Round int
}

// AutomaticReviewReason is the reason recorded on an automatic round retry.
const AutomaticReviewReason = "automatic review-round retry"

// AutomaticReviewActor is the system actor of an automatic review-round act.
func AutomaticReviewActor(taskID string) Actor {
	return SystemActor("queue:auto-review:" + taskID)
}

// AutomaticReviewRequestID is deterministic per task and round, so the
// existing request idempotency makes a repeated act a replay.
func AutomaticReviewRequestID(taskID string, round int) string {
	return fmt.Sprintf("auto-review/%s/%d", taskID, round)
}

// AutomaticReviewRetryRefusal is the in-transaction recheck of an automatic
// round retry: the round must not have had its automatic act yet.
func AutomaticReviewRetryRefusal(request ReviewRoundRetryRequest, recovery *ReviewRecoveryState) error {
	if request.Automatic && recovery.AutomaticRetryExhausted {
		return fmt.Errorf("%w: review round %d already had its automatic act", ErrReviewRetryConflict, request.PriorRound)
	}
	return nil
}

// AutomaticInterruptedRecoveryRefusal is the in-transaction recheck of an
// automatic in-place recovery: once per round, and never re-queueing a seat
// whose bounded automatic recovery is exhausted.
func AutomaticInterruptedRecoveryRefusal(request InterruptedReviewRecoveryRequest, recovery *InterruptedReviewRecoveryState) error {
	if !request.Automatic {
		return nil
	}
	if recovery.AutomaticRetryExhausted {
		return fmt.Errorf("%w: review round %d already had its automatic act", ErrReviewRetryConflict, request.Round)
	}
	for _, order := range recovery.EligibleOrders {
		if ReviewSeatRecoveryExhausted(order) {
			return fmt.Errorf("%w: review seat %s has exhausted automatic recovery", ErrReviewRetryConflict, order.ID)
		}
	}
	return nil
}

// ReviewSeatRecoveryExhausted reports a queued, unclaimed review seat whose
// bounded automatic recovery is exhausted (AR-3). Such a seat is never
// re-queued; it no longer keeps its round active, so a round retry can
// replace it with a new round while the seat itself stays unchanged.
func ReviewSeatRecoveryExhausted(order core.WorkOrder) bool {
	return order.Stage == core.StageReview && order.State == core.WorkOrderQueued &&
		order.RetrySuppressed && order.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason &&
		order.SessionID == "" && order.WorkerID == ""
}

// AutomaticReviewAct reports whether round already had its automatic act: an
// automatic round retry from or to round, or an automatic recovery of round.
func AutomaticReviewAct(events []core.Event, round int) bool {
	for _, event := range events {
		if event.Kind != "review.round_retried" && event.Kind != "review.round_recovered" {
			continue
		}
		var payload struct {
			Automatic   bool `json:"automatic"`
			PriorRound  int  `json:"prior_round"`
			NewRound    int  `json:"new_round"`
			ReviewRound int  `json:"review_round"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || !payload.Automatic {
			continue
		}
		if event.Kind == "review.round_retried" && (payload.PriorRound == round || payload.NewRound == round) ||
			event.Kind == "review.round_recovered" && payload.ReviewRound == round {
			return true
		}
	}
	return false
}

// DecideAutomaticReviewAct is the AR-5 review-round rule. One automatic act
// is allowed per round, and none at all when the bound is 0 (AC-5.1). The
// rule defers while the AR-3 clock still owns a seat, never touches a seat
// stopped by an operator checkpoint or identical-failure suppression
// (AC-2.2), and never re-queues an AR-3-exhausted seat: a round holding one
// is replaced by a round retry instead of an in-place recovery.
func DecideAutomaticReviewAct(policy config.RecoveryPolicy, task core.Task, orders []core.WorkOrder, events []core.Event, now time.Time) AutomaticReviewDecision {
	none := AutomaticReviewDecision{Kind: AutomaticReviewNone}
	if policy.AutomaticRecoveries == 0 || task.State != core.TaskQueued && task.State != core.TaskRunning {
		return none
	}
	current := CurrentReviewOrders(orders, events)
	round := 0
	for _, order := range current {
		if order.Stage == core.StageReview && order.ReviewRound > round {
			round = order.ReviewRound
		}
	}
	if round == 0 || AutomaticReviewAct(events, round) {
		return none
	}
	for _, order := range current {
		if order.Stage != core.StageReview || order.ReviewRound != round {
			continue
		}
		if order.Checkpoint != nil || order.RetrySuppressionReason == core.IdenticalFailureSuppressionReason ||
			order.RetrySuppressionReason == core.WorkOrderReleaseReasonOperatorCheckpointReached ||
			order.LastFailureMessage == core.WorkOrderReleaseReasonOperatorCheckpointReached {
			return none
		}
		if automaticClockOwns(policy, task, order, orders, now) {
			return none
		}
	}
	decision := AutomaticReviewDecision{Round: round}
	if retry := ReviewRecoveryNeeded(current, events); retry != nil && retry.PriorRound == round {
		decision.Kind = AutomaticReviewRetry
		return decision
	}
	if recovery := InterruptedReviewRecoveryNeeded(task, current, events); recovery != nil && recovery.ReviewRound == round {
		for _, order := range recovery.EligibleOrders {
			if ReviewSeatRecoveryExhausted(order) {
				return none
			}
		}
		decision.Kind = AutomaticReviewRecover
		return decision
	}
	return none
}

// automaticClockOwns reports a seat the AR-3 clock will still act on: it is
// scheduled, due, or about to be marked exhausted.
func automaticClockOwns(policy config.RecoveryPolicy, task core.Task, order core.WorkOrder, orders []core.WorkOrder, now time.Time) bool {
	order.RecoveryRefusal = ""
	if refusal := WorkOrderRecoveryRefusal(task, order, orders); refusal != nil {
		order.RecoveryRefusal = refusal.Error()
	}
	if DecideAutomaticRecovery(policy, task, order, now).Kind != AutomaticRecoveryNone {
		return true
	}
	return order.RecoveryRefusal == "" && RecoverableStop(order, now) && order.NextAutomaticRecoveryAt.After(now)
}
