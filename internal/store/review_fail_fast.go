package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ErrReviewHeadStale refuses a fail-fast changes-requested verdict whose
// reviewed pull-request head differs from the head its review round opened
// for. Such a verdict cannot settle the round.
var ErrReviewHeadStale = errors.New("review_head_stale")

// ReviewFailFastSettlement is the review.round_completed settlement value a
// fail-fast settlement records. All-seat settlement records none.
const ReviewFailFastSettlement = "fail_fast"

// FailFastReviewRound reports whether a numbered review round of task settles
// under the opt-in fail-fast policy. The value is read from the task's frozen
// pipeline policy, never from current workspace settings, so every round of
// one task settles the same way. Legacy round 0 reviews never fail fast.
func FailFastReviewRound(task core.Task, round int) bool {
	return round > 0 && task.SetupContract.Review.FailFast
}

// ValidateFailFastReviewDecision refuses a not-yet-accepted verdict for a
// fail-fast round that has already settled, and a changes-requested verdict
// whose reviewed head is not the round's head. Every backend calls it inside
// the verdict transaction after the duplicate-delivery check, so a replay of
// an already accepted verdict stays an idempotent no-op.
func ValidateFailFastReviewDecision(task core.Task, decision core.ReviewDecision, roundSettled bool) error {
	if !FailFastReviewRound(task, decision.ReviewRound) {
		return nil
	}
	if roundSettled {
		return fmt.Errorf("%w: review round %d settled on an earlier changes-requested verdict", ErrReviewRoundSettled, decision.ReviewRound)
	}
	if decision.Verdict == "changes_requested" && decision.HeadSHA != "" && decision.ReviewedCommitSHA != "" && decision.ReviewedCommitSHA != decision.HeadSHA {
		return fmt.Errorf("%w: verdict reviewed %s but review round %d is for %s", ErrReviewHeadStale, decision.ReviewedCommitSHA, decision.ReviewRound, decision.HeadSHA)
	}
	return nil
}

// FailFastSettles reports whether an accepted verdict settles its round
// before every seat has a verdict: only a changes-requested verdict in a
// fail-fast round does. Approval still waits for every required seat.
func FailFastSettles(task core.Task, decision core.ReviewDecision) bool {
	return FailFastReviewRound(task, decision.ReviewRound) && decision.Verdict == "changes_requested"
}

// SettledReviewSeatPayload is the work_order.cancelled payload for an
// unfinished seat retired by a fail-fast settlement of its round. The seat is
// cancelled, never given a verdict.
func SettledReviewSeatPayload(id, attemptID, sessionID string, from core.WorkOrderState, round int, settledBy string) json.RawMessage {
	return core.JSONPayload(map[string]any{
		"id": id, "attempt_id": attemptID, "session_id": sessionID, "state": core.WorkOrderCancelled,
		"from": from, "command": core.WorkOrderCmdCancel, "reason": core.ReviewRoundSettledSuppressionReason,
		"review_round": round, "settled_by_work_order_id": settledBy,
	})
}

// ReviewRoundActorID is the agent actor a review round's settlement records.
func ReviewRoundActorID(round int) string {
	return fmt.Sprintf("review:round:%d", round)
}
