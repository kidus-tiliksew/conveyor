package workorder

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ChangeTaskPolicy admits only the operator's explicit frozen-policy exception
// (DEC-47, DEC-43; component-task-lifecycle). The store
// derives the new frozen policy and any verify/review handoff inside its task
// transaction. Execution-setup reassignment is retired: POST /tasks/{id}/setup
// answers 410, and harness, model, and effort stay in client-local execution
// setups (DEC-56; component-task-lifecycle).
func (s *Service) ChangeTaskPolicy(ctx context.Context, taskID, reason, requestID string, policy store.TaskPolicyChange) (store.SetupChangeResult, error) {
	request := store.SetupChangeRequest{TaskID: taskID, Reason: reason, RequestID: requestID, Policy: &policy}
	request, err := store.PrepareSetupChangeRequest(request)
	if err != nil {
		return store.SetupChangeResult{}, err
	}
	var result store.SetupChangeResult
	err = s.Store.WithTaskSideEffectLock(ctx, taskID, func(ctx context.Context) error {
		var changeErr error
		result, changeErr = taskops.ExecuteSetupChange(ctx, s.Store, taskID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
			return s.Store.ChangeTaskPolicyCommand(ctx, lease, request)
		})
		return changeErr
	})
	if err == nil && s.Dispatcher != nil {
		s.Dispatcher.Enqueue(ctx, taskID)
	}
	return result, err
}
