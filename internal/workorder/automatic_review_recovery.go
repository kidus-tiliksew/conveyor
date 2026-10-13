package workorder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// SweepAutomaticReviewRecovery runs one AR-5 review-round sweep
// (feature-bounded-automatic-recovery, component-work-orders Review rounds).
// Each live task whose latest round cannot finish gets at most one automatic
// act per round: a round retry, or an in-place recovery of interrupted seats.
// Every act goes through the operator path's verification and its store
// command, which re-checks the rule inside its transaction. A conflict means
// the round changed underneath the sweep and is skipped; any other per-task
// failure is returned after the remaining tasks are swept. It returns the
// number of acts taken.
func (s *Service) SweepAutomaticReviewRecovery(ctx context.Context, now time.Time) (int, error) {
	cfg, err := s.config(ctx)
	if err != nil {
		return 0, err
	}
	if cfg.Recovery.AutomaticRecoveries == 0 {
		return 0, nil
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	acted := 0
	var failures []error
	for _, task := range tasks {
		if task.State != core.TaskQueued && task.State != core.TaskRunning {
			continue
		}
		orders, err := s.Store.ListTaskWorkOrders(ctx, task.ID)
		if err != nil {
			return acted, err
		}
		events, err := s.Store.ListEvents(ctx, task.ID)
		if err != nil {
			return acted, err
		}
		decision := store.DecideAutomaticReviewAct(cfg.Recovery, task, orders, events, now)
		if decision.Kind == store.AutomaticReviewNone {
			continue
		}
		actCtx := store.WithActor(ctx, store.AutomaticReviewActor(task.ID))
		requestID := store.AutomaticReviewRequestID(task.ID, decision.Round)
		if decision.Kind == store.AutomaticReviewRetry {
			_, err = s.retryReviewRound(actCtx, task.ID, requestID, store.AutomaticReviewReason, true)
		} else {
			_, err = s.recoverInterruptedReviewRound(actCtx, task.ID, requestID, true)
		}
		switch {
		case err == nil:
			acted++
		case errors.Is(err, store.ErrReviewRetryConflict):
			s.logf("[task %s] automatic review %s of round %d skipped: %v", task.ID, decision.Kind, decision.Round, err)
		default:
			failures = append(failures, fmt.Errorf("task %s automatic review %s of round %d: %w", task.ID, decision.Kind, decision.Round, err))
		}
	}
	return acted, errors.Join(failures...)
}
