package dispatch

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ResolveReviewRoundSeats is the RS-2 resolution of one new review round's
// seat count (feature-review-seat-minimum RS-2; DEC-77, DEC-78). It forwards
// to the shared pure store implementation, which the policy-change planner
// also calls inside every backend transaction, because package store cannot
// import dispatch. There is no second copy of the resolution table.
func ResolveReviewRoundSeats(in store.ReviewRoundSeatInputs) store.ReviewRoundSeatResolution {
	return store.ResolveReviewRoundSeats(in)
}

// ResolveTaskReviewRoundSeats resolves round's seats for task from the task's
// minimum under cfg and the inputs read through the public store surface.
func ResolveTaskReviewRoundSeats(ctx context.Context, reader store.ReviewRoundSeatReader, cfg *config.Config, task core.Task, round int) (store.ReviewRoundSeatResolution, error) {
	workspaceSeats := 0
	if cfg != nil {
		workspaceSeats = len(cfg.Review.Seats)
	}
	in, err := store.LoadReviewRoundSeatInputs(ctx, reader, task.ID, store.ReviewSeatMinimum(task.SetupContract, workspaceSeats), round)
	if err != nil {
		return store.ReviewRoundSeatResolution{}, err
	}
	return ResolveReviewRoundSeats(in), nil
}
