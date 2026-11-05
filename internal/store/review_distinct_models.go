package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ErrReviewSeatModelNotDistinct refuses a review seat claim under a task's
// frozen review.distinct_models policy (req-review-gates-evidence AC-2.10;
// DEC-104(4); component-work-orders). The refusal leaves the seat queued and
// claimable and records no claim failure.
var ErrReviewSeatModelNotDistinct = errors.New("review_seat_model_not_distinct")

// ReviewSeatModelRefusal is the one comparison every backend runs inside its
// claim transaction, directly after the self-review guard, over the task's
// orders read in that transaction. Per-task claim serialization covers sibling
// claims, so the comparison takes no lock of its own.
//
// It refuses a seat of a numbered round whose reported model is blank or
// matches another seat of that round after trimming whitespace and ignoring
// letter case. Only a seat holding a live claim, a submitted verdict, or an
// accepted verdict counts. Conveyor compares reported names only: it verifies
// no name, chooses no model, and treats the literal "auto" like any other name.
func ReviewSeatModelRefusal(task core.Task, order core.WorkOrder, model string, orders []core.WorkOrder, now time.Time) error {
	if order.Stage != core.StageReview || order.ReviewRound <= 0 || !task.SetupContract.Review.DistinctModels {
		return nil
	}
	seats := make([]core.WorkOrder, 0, len(orders))
	for _, sibling := range orders {
		if sibling.ID != order.ID && sibling.TaskID == order.TaskID && sibling.Stage == core.StageReview &&
			sibling.ReviewRound == order.ReviewRound && reviewSeatHoldsModel(sibling, now) {
			seats = append(seats, sibling)
		}
	}
	sort.Slice(seats, func(i, j int) bool { return seats[i].ReviewSeat < seats[j].ReviewSeat })
	reported := strings.TrimSpace(model)
	held := make([]string, 0, len(seats))
	repeated := false
	for _, seat := range seats {
		name := strings.TrimSpace(seat.Model)
		if name == "" {
			continue
		}
		held = append(held, name)
		repeated = repeated || strings.EqualFold(name, reported)
	}
	holds := "holds no model names"
	if len(held) > 0 {
		holds = "holds model names " + strings.Join(held, ", ")
	}
	if reported == "" {
		return fmt.Errorf("%w: review round %d requires a nonblank reported model name and %s", ErrReviewSeatModelNotDistinct, order.ReviewRound, holds)
	}
	if repeated {
		return fmt.Errorf("%w: review round %d %s, so seat %d cannot claim with model %s", ErrReviewSeatModelNotDistinct, order.ReviewRound, holds, order.ReviewSeat, reported)
	}
	return nil
}

// reviewSeatHoldsModel excludes released, expired, and cancelled seats. A
// claimed seat whose lease has lapsed has expired even before the clock sweep
// moves its state.
func reviewSeatHoldsModel(seat core.WorkOrder, now time.Time) bool {
	switch seat.State {
	case core.WorkOrderClaimed:
		return seat.LeaseExpiresAt.After(now)
	case core.WorkOrderSubmitted, core.WorkOrderCompleted:
		return true
	}
	return false
}
