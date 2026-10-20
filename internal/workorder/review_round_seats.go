package workorder

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// AddReviewSeats is the operator review-seat addition (feature-review-seat-minimum
// RS-5; req-review-gates-evidence AC-2.5, AC-2.6). Only a user credential with
// a matching user actor may add seats; the store command decides the latest
// round, settlement, and replay inside its task-serialized transaction, under
// the task side-effect lock.
func (s *Service) AddReviewSeats(ctx context.Context, request store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	credential, ok := store.CredentialFromContext(ctx)
	actor := store.ActorFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser || actor.Role != core.ActorUser || actor.ID != store.UserActorID(credential.OwnerUserID) {
		return store.ReviewSeatAdditionResult{}, fmt.Errorf("%w: seat addition requires an authenticated user operator", store.ErrReviewSeatAdditionForbidden)
	}
	if err := request.Validate(); err != nil {
		return store.ReviewSeatAdditionResult{}, err
	}
	// The workspace seat-list length sizes a legacy round's carried minimum
	// and the queue clock times the added seats; only the configuration
	// provider knows them, never the request body.
	request.WorkspaceReviewSeats, request.QueueTimeout = 0, 0
	if s.ConfigProvider != nil {
		cfg, err := s.config(ctx)
		if err != nil {
			return store.ReviewSeatAdditionResult{}, err
		}
		if cfg != nil {
			request.WorkspaceReviewSeats, request.QueueTimeout = len(cfg.Review.Seats), cfg.WorkOrderQueueTimeout
		}
	}
	var result store.ReviewSeatAdditionResult
	err := s.Store.WithTaskSideEffectLock(ctx, request.TaskID, func(locked context.Context) error {
		var addErr error
		result, addErr = taskops.ExecuteWorkOrder(locked, s.Store, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
			return s.Store.AddReviewSeatsCommand(locked, lease, request)
		})
		return addErr
	})
	return result, err
}
