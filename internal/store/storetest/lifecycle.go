// Package storetest provides lifecycle setup helpers for tests. Production
// code must use the lease-taking Store command surface through taskops.
package storetest

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type Adapter struct{ store.Store }

func For(st store.Store) Adapter { return Adapter{Store: st} }

func (a Adapter) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	return CreateWorkOrder(ctx, a.Store, order)
}
func (a Adapter) CreateStageWorkOrder(ctx context.Context, job core.Job, order core.WorkOrder) (bool, error) {
	return CreateStageWorkOrder(ctx, a.Store, job, order)
}
func (a Adapter) CreateReviewRound(ctx context.Context, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	return CreateReviewRound(ctx, a.Store, taskID, jobs, orders)
}
func (a Adapter) RetryReviewRound(ctx context.Context, request store.ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	return RetryReviewRound(ctx, a.Store, request, jobs, orders)
}
func (a Adapter) RecoverInterruptedReviewRound(ctx context.Context, request store.InterruptedReviewRecoveryRequest, timeout time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	return RecoverInterruptedReviewRound(ctx, a.Store, request, timeout)
}
func (a Adapter) ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	return ClaimWorkOrder(ctx, a.Store, id, claim)
}
func (a Adapter) RedispatchWorkOrder(ctx context.Context, id string, timeout time.Duration) (core.WorkOrder, error) {
	return RedispatchWorkOrder(ctx, a.Store, id, timeout)
}
func (a Adapter) RecoverWorkOrder(ctx context.Context, id, requestID string, timeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	return RecoverWorkOrder(ctx, a.Store, id, requestID, timeout, refreeze...)
}
func (a Adapter) RecoverWorkOrderWithDirection(ctx context.Context, id, requestID, direction string, timeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	return RecoverWorkOrderWithDirection(ctx, a.Store, id, requestID, direction, timeout, refreeze...)
}
func (a Adapter) UpdateWorkOrder(ctx context.Context, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	return UpdateWorkOrder(ctx, a.Store, order, commands...)
}
func (a Adapter) RenewWorkerClaim(ctx context.Context, id, workerID, sessionID string, duration time.Duration) (core.WorkOrder, error) {
	return RenewWorkerClaim(ctx, a.Store, id, workerID, sessionID, duration)
}
func (a Adapter) ReleaseWorkerClaim(ctx context.Context, id, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	return ReleaseWorkerClaim(ctx, a.Store, id, workerID, release)
}
func (a Adapter) CancelTask(ctx context.Context, intervention core.Intervention) (core.Task, error) {
	return taskops.New(a.Store).Cancel(ctx, intervention)
}
func (a Adapter) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	return taskops.New(a.Store).AcceptReviewDecision(ctx, decision)
}

func CreateWorkOrder(ctx context.Context, st store.Store, order core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateWorkOrderCommand(ctx, lease, order)
	})
	return err
}

func CreateStageWorkOrder(ctx context.Context, st store.Store, job core.Job, order core.WorkOrder) (bool, error) {
	return taskops.ExecuteWorkOrder(ctx, st, job.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (bool, error) {
		return st.CreateStageWorkOrderCommand(ctx, lease, job, order)
	})
}

func CreateReviewRound(ctx context.Context, st store.Store, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	_, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.CreateReviewRoundCommand(ctx, lease, taskID, jobs, orders)
	})
	return err
}

func RetryReviewRound(ctx context.Context, st store.Store, request store.ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	return taskops.ExecuteWorkOrder(ctx, st, request.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.ReviewRoundRetryResult, error) {
		return st.RetryReviewRoundCommand(ctx, lease, request, jobs, orders)
	})
}

func RecoverInterruptedReviewRound(ctx context.Context, st store.Store, request store.InterruptedReviewRecoveryRequest, timeout time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	return taskops.ExecuteWorkOrder(ctx, st, request.TaskID, core.WorkOrderCmdRecover, func(lease taskops.TaskLease) (store.InterruptedReviewRecoveryResult, error) {
		return st.RecoverInterruptedReviewRoundCommand(ctx, lease, request, timeout)
	})
}

func ClaimWorkOrder(ctx context.Context, st store.Store, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.New(st).ClaimWorkOrder(ctx, order.TaskID, id, claim)
}

func RedispatchWorkOrder(ctx context.Context, st store.Store, id string, timeout time.Duration) (core.WorkOrder, error) {
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRedispatch, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return st.RedispatchWorkOrderCommand(ctx, lease, id, timeout)
	})
}

func RecoverWorkOrder(ctx context.Context, st store.Store, id, requestID string, timeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	return RecoverWorkOrderWithDirection(ctx, st, id, requestID, "", timeout, refreeze...)
}

func RecoverWorkOrderWithDirection(ctx context.Context, st store.Store, id, requestID, direction string, timeout time.Duration, refreeze ...*store.RecoveryRefreeze) (core.WorkOrder, error) {
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRecover, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return st.RecoverWorkOrderCommand(ctx, lease, id, requestID, direction, timeout, refreeze...)
	})
}

func UpdateWorkOrder(ctx context.Context, st store.Store, order core.WorkOrder, commands ...core.WorkOrderCommand) error {
	command := taskops.WorkOrderMetadataCommand
	if len(commands) == 1 {
		command = commands[0]
	} else if current, err := st.GetWorkOrder(ctx, order.ID); err == nil && current.State != order.State {
		if inferred, ok := store.InferWorkOrderUpdateCommand(current, order); ok {
			command = inferred
		}
	}
	_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, command, func(lease taskops.TaskLease) (struct{}, error) {
		return struct{}{}, st.UpdateWorkOrderCommand(ctx, lease, order, commands...)
	})
	return err
}

func RenewWorkerClaim(ctx context.Context, st store.Store, id, workerID, sessionID string, duration time.Duration) (core.WorkOrder, error) {
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRenew, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return st.RenewWorkerClaimCommand(ctx, lease, id, workerID, sessionID, duration)
	})
}

func ReleaseWorkerClaim(ctx context.Context, st store.Store, id, workerID string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		claim := core.WorkOrderClaimIdentity{WorkerID: workerID, ClaimantID: order.ClaimantID, SessionID: release.SessionID}
		return st.ReleaseWorkerClaimCommand(ctx, lease, id, claim, release)
	})
}
