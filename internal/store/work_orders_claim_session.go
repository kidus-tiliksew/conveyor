package store

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ClaimSessionReader is the read surface of the claim session check. Neither
// read selects an authority snapshot or another order's row
// (component-work-orders, Snapshot-free fold reads).
type ClaimSessionReader interface {
	GetWorkOrderWithoutSnapshots(ctx context.Context, id string) (core.WorkOrder, error)
	OtherWorkOrderHoldsSession(ctx context.Context, workOrderID, sessionID string) (bool, error)
}

// ResolveClaimSession binds a presented session to one order of the bound
// workspace. It returns the order, without snapshots, when the order's
// projected session is the presented non-empty session. Otherwise it answers
// ErrWorkOrderClaimUnauthorized when another order of the workspace holds the
// session and ErrWorkOrderClaimLost when none does or the session is empty;
// an empty session answers without a read. These are the answers the former
// scan of ListWorkOrders gave (component-mcp-protocol, Claim-bound
// authorization; component-work-orders, Snapshot-free fold reads).
func ResolveClaimSession(ctx context.Context, st ClaimSessionReader, workOrderID, sessionID string) (core.WorkOrder, error) {
	if sessionID == "" {
		return core.WorkOrder{}, ErrWorkOrderClaimLost
	}
	order, err := st.GetWorkOrderWithoutSnapshots(ctx, workOrderID)
	if err == nil && order.SessionID == sessionID {
		return order, nil
	}
	held, err := st.OtherWorkOrderHoldsSession(ctx, workOrderID, sessionID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if held {
		return core.WorkOrder{}, ErrWorkOrderClaimUnauthorized
	}
	return core.WorkOrder{}, ErrWorkOrderClaimLost
}

// GetWorkOrderWithoutSnapshots reads one order of the selected workspace as
// GetWorkOrder does, with both authority snapshots and the assignee cleared,
// as the durable backends' activityWorkOrderColumns read returns it.
func (m *memory) GetWorkOrderWithoutSnapshots(ctx context.Context, id string) (core.WorkOrder, error) {
	order, err := m.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	order.Assignee = nil
	return withoutAuthoritySnapshots(order), nil
}

// OtherWorkOrderHoldsSession scans the selected workspace's orders with the
// read-time projection, as the durable backends' predicate does in SQL.
func (m *memory) OtherWorkOrderHoldsSession(ctx context.Context, workOrderID, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	selected, scoped := WorkspaceFromContext(ctx)
	scoped = scoped && selected != ""
	now := time.Now().UTC()
	for id, order := range m.workOrders {
		if id == workOrderID || order.SessionID != sessionID {
			continue
		}
		if scoped && m.tasks[order.TaskID].Workspace != selected {
			continue
		}
		if ProjectWorkOrderAt(order, now).SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}
