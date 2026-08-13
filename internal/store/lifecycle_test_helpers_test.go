package store

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type testLifecycleAdapter struct{ Store }

func storetestFor(st Store) testLifecycleAdapter { return testLifecycleAdapter{Store: st} }

func (a testLifecycleAdapter) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, a.CreateWorkOrderCommand(ctx, lease, order)
	})
	return err
}
func (a testLifecycleAdapter) CreateReviewRound(ctx context.Context, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(ctx, a.Store, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, a.CreateReviewRoundCommand(ctx, lease, taskID, jobs, orders)
	})
	return err
}
func (a testLifecycleAdapter) RetryReviewRound(ctx context.Context, request ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (ReviewRoundRetryResult, error) {
	return taskops.ExecuteWorkOrder(ctx, a.Store, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (ReviewRoundRetryResult, error) {
		return a.RetryReviewRoundCommand(ctx, lease, request, jobs, orders)
	})
}
func (a testLifecycleAdapter) RecoverInterruptedReviewRound(ctx context.Context, request InterruptedReviewRecoveryRequest, timeout time.Duration) (InterruptedReviewRecoveryResult, error) {
	return taskops.ExecuteWorkOrder(ctx, a.Store, request.TaskID, core.WorkOrderCmdRecover, func(lease taskops.TaskLease) (InterruptedReviewRecoveryResult, error) {
		return a.RecoverInterruptedReviewRoundCommand(ctx, lease, request, timeout)
	})
}
func (a testLifecycleAdapter) ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	order, err := a.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.New(a.Store).ClaimWorkOrder(ctx, order.TaskID, id, claim)
}
func (a testLifecycleAdapter) RedispatchWorkOrder(ctx context.Context, id string, timeout time.Duration) (core.WorkOrder, error) {
	order, err := a.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, core.WorkOrderCmdRedispatch, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return a.RedispatchWorkOrderCommand(ctx, lease, id, timeout)
	})
}
func (a testLifecycleAdapter) RecoverWorkOrder(ctx context.Context, id, requestID string, timeout time.Duration, refreeze ...*RecoveryRefreeze) (core.WorkOrder, error) {
	order, err := a.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, core.WorkOrderCmdRecover, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return a.RecoverWorkOrderCommand(ctx, lease, id, requestID, "", timeout, refreeze...)
	})
}
func (a testLifecycleAdapter) UpdateWorkOrder(ctx context.Context, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	command := taskops.WorkOrderMetadataCommand
	if len(commands) == 1 {
		command = commands[0]
	} else if current, err := a.GetWorkOrder(ctx, order.ID); err == nil && current.State != order.State {
		if inferred, ok := InferWorkOrderUpdateCommand(current, order); ok {
			command = inferred
		}
	}
	_, err := taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, command, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, a.UpdateWorkOrderCommand(ctx, lease, order, commands...)
	})
	return err
}
func (a testLifecycleAdapter) RenewWorkerClaim(ctx context.Context, id, workerID, sessionID string, duration time.Duration) (core.WorkOrder, error) {
	order, err := a.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, core.WorkOrderCmdRenew, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return a.RenewWorkerClaimCommand(ctx, lease, id, workerID, sessionID, duration)
	})
}
func (a testLifecycleAdapter) ReleaseWorkerClaim(ctx context.Context, id, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	order, err := a.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, a.Store, order.TaskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		claim := core.WorkOrderClaimIdentity{WorkerID: workerID, ClaimantID: order.ClaimantID, SessionID: release.SessionID}
		return a.ReleaseWorkerClaimCommand(ctx, lease, id, claim, release)
	})
}
func (a testLifecycleAdapter) CancelTask(ctx context.Context, intervention core.Intervention) (core.Task, error) {
	return taskops.New(a.Store).Cancel(ctx, intervention)
}
func (a testLifecycleAdapter) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	return taskops.New(a.Store).AcceptReviewDecision(ctx, decision)
}
