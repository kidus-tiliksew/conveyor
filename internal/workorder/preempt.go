package workorder

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Preempt is the operator command for revoking one active attempt. It is kept
// outside MCP deliberately: agents cannot preempt one another (spec §§13.2,
// 21.4).
func (s *Service) Preempt(ctx context.Context, id, reason, requestID string) (store.WorkOrderPreemptResult, error) {
	request, err := store.PrepareWorkOrderPreemptRequest(store.WorkOrderPreemptRequest{
		WorkOrderID: id, RequestID: requestID, Reason: reason,
	})
	if err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return store.WorkOrderPreemptResult{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
		return s.Store.PreemptWorkOrderCommand(ctx, lease, request)
	})
}
