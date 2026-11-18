package store

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RiskProjectionStore is the store surface one request-scoped risk
// projection needs. Store already satisfies it; the named contract is the
// mandatory subset parent embeds if a backend is split.
type RiskProjectionStore interface {
	ListRiskRatingsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.RiskRating, error)
	ListRiskOrdersForTasks(ctx context.Context, workspaceID string, taskIDs []string) (map[string][]core.RiskOrder, error)
	ListClaimEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
	ListTaskApprovalsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.TaskApproval, error)
	ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error)
}

// RiskProjectionOptions are live workspace inputs read once per batch.
// OrdersLoaded distinguishes not-loaded from loaded-empty so a caller that
// already selected work orders does not select them again.
type RiskProjectionOptions struct {
	WorkspaceSeats int
	TrustedAuthors map[string][]string
	Orders         []core.WorkOrder
	OrdersLoaded   bool
}

// ProjectTaskRisks projects risk fields for the supplied tasks. Off-mode
// tasks are present as nil and cause no store read. Enabled tasks share one
// ratings selection, one risk-order selection, and EvaluateTaskApprovalsBatch
// with preloaded ratings and optional work orders
// (req-risk-based-review AC-1.1, AC-11.2).
func ProjectTaskRisks(ctx context.Context, st Store, tasks []core.Task, opts RiskProjectionOptions) (map[string]*core.TaskRisk, error) {
	out := make(map[string]*core.TaskRisk, len(tasks))
	var enabled []core.Task
	var enforcing []core.Task
	var workspace string
	for _, task := range tasks {
		if workspace == "" {
			workspace = task.Workspace
		} else if task.Workspace != workspace {
			return nil, fmt.Errorf("risk projection spans workspaces")
		}
		if !task.SetupContract.Risk.Enabled() {
			out[task.ID] = nil
			continue
		}
		enabled = append(enabled, task)
		if task.SetupContract.Risk.Enforcing() {
			enforcing = append(enforcing, task)
		}
	}
	if len(enabled) == 0 {
		return out, nil
	}
	if workspace != "" {
		ctx = WithWorkspace(ctx, workspace)
	}
	ids := make([]string, 0, len(enabled))
	for _, task := range enabled {
		ids = append(ids, task.ID)
	}
	ratings, err := st.ListRiskRatingsForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	ordersByTask, err := st.ListRiskOrdersForTasks(ctx, workspace, ids)
	if err != nil {
		return nil, err
	}
	evaluations := map[string]ApprovalEvaluation{}
	if len(enforcing) > 0 {
		evaluations, err = EvaluateTaskApprovalsBatchInputs(ctx, st, enforcing, ApprovalBatchInputs{
			Orders:        opts.Orders,
			OrdersLoaded:  opts.OrdersLoaded,
			Ratings:       ratings,
			RatingsLoaded: true,
		}, opts.TrustedAuthors, "")
		if err != nil {
			return nil, err
		}
	}
	for _, task := range enabled {
		projection := ProjectTaskRisk(task, ratings[task.ID], ReviewSeatMinimum(task.SetupContract, opts.WorkspaceSeats))
		if projection == nil {
			out[task.ID] = nil
			continue
		}
		projection.Orders = core.TaskRiskOrders(ordersByTask[task.ID])
		ProjectTaskApprovals(projection, evaluations[task.ID])
		out[task.ID] = projection
	}
	return out, nil
}
