package workorder

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ReviewSeatsBelowFloorCode is the refusal code for a declared review seat
// count below the task's seat floor (req-review-gates-evidence AC-2.3; DEC-78;
// feature-review-seat-minimum RS-4).
const ReviewSeatsBelowFloorCode = "review_seats_below_floor"

// ReviewSeatsBelowFloorError refuses a submission whose declared review seat
// count is below the larger of the task's minimum and its newest approved
// plan count. PlannedSeatCount is 0 when no approved plan states a count. The
// refusal precedes every submission side effect, so the order stays claimed.
type ReviewSeatsBelowFloorError struct {
	ReviewSeats      int
	SeatFloor        int
	MinimumSeatCount int
	PlannedSeatCount int
}

func (e *ReviewSeatsBelowFloorError) Error() string {
	return fmt.Sprintf("%s: review_seats %d is below the task's review seat floor %d (minimum_seat_count=%d, planned_seat_count=%d)",
		ReviewSeatsBelowFloorCode, e.ReviewSeats, e.SeatFloor, e.MinimumSeatCount, e.PlannedSeatCount)
}

// admitReviewSeats is the read-only floor admission of a declared count. It
// reads nothing when no count is declared. The minimum follows RS-2's
// ReviewSeatMinimum under the effective configuration, the planned count is
// the newest approved plan version's, and a previous round's count is a
// default rather than part of the floor.
func (s *Service) admitReviewSeats(ctx context.Context, cfg *config.Config, task core.Task, reviewSeats *int) error {
	if reviewSeats == nil {
		return nil
	}
	if *reviewSeats < 1 {
		return fmt.Errorf("review_seats must be at least 1")
	}
	workspaceSeats := 0
	if cfg != nil {
		workspaceSeats = len(cfg.Review.Seats)
	}
	minimum := store.ReviewSeatMinimum(task.SetupContract, workspaceSeats)
	var planned *int
	plan, approved, err := s.Store.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil {
		return err
	}
	if approved && plan.ReviewSeats != nil {
		count := *plan.ReviewSeats
		planned = &count
	}
	floor := store.ReviewSeatFloor(minimum, planned)
	if *reviewSeats >= floor {
		return nil
	}
	refusal := &ReviewSeatsBelowFloorError{ReviewSeats: *reviewSeats, SeatFloor: floor, MinimumSeatCount: minimum}
	if planned != nil {
		refusal.PlannedSeatCount = *planned
	}
	return refusal
}

// withReviewSeatsResult adds the declared count and whether it sizes the
// next round to a submission result. Without a count the result keeps its
// existing shape.
func withReviewSeatsResult(result map[string]any, count *int, applied bool) map[string]any {
	if count == nil || result == nil {
		return result
	}
	result["review_seats"] = *count
	result["review_seats_applied"] = applied
	return result
}

// recordedUnusedReviewSeats reads the count a refresh-completing submission
// recorded unused from that submission's own work_order.updated payload.
func (s *Service) recordedUnusedReviewSeats(ctx context.Context, order core.WorkOrder) (*int, error) {
	events, err := s.Store.ListEvents(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Kind != "work_order.updated" || event.JobID != order.JobID {
			continue
		}
		var recorded struct {
			ID                 string              `json:"id"`
			State              core.WorkOrderState `json:"state"`
			HeadSHA            string              `json:"head_sha"`
			ReviewSeats        *int                `json:"review_seats"`
			ReviewSeatsApplied *bool               `json:"review_seats_applied"`
		}
		if json.Unmarshal(event.Payload, &recorded) != nil || recorded.ID != order.ID || recorded.State != core.WorkOrderSubmitted || recorded.HeadSHA != order.HeadSHA {
			continue
		}
		if recorded.ReviewSeats != nil && recorded.ReviewSeatsApplied != nil && !*recorded.ReviewSeatsApplied {
			return recorded.ReviewSeats, nil
		}
		return nil, nil
	}
	return nil, nil
}
