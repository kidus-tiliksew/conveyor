package storetest

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const rs5Operator = "user:rs5-operator"

func rs5OperatorContext(x Fixture) Fixture {
	x.Context = store.WithActor(x.Context, store.Actor{ID: rs5Operator, Role: core.ActorUser})
	return x
}

func rs5Add(x Fixture, request store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	ctx := rs5OperatorContext(x).Context
	return taskops.ExecuteWorkOrder(ctx, x.Backend, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewSeatAdditionResult, error) {
		return x.Backend.AddReviewSeatsCommand(ctx, lease, request)
	})
}

func rs5Request(taskID, requestID string, add int) store.ReviewSeatAdditionRequest {
	return store.ReviewSeatAdditionRequest{TaskID: taskID, RequestID: requestID, Reason: "second opinion on the migration", Add: add, WorkspaceReviewSeats: 1}
}
