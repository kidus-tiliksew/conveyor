package nextaction

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// PlanRiskRating is the catalogue variant and reason code of the plan risk
// wait row: an enforcing task at plan approval whose plan version's risk order
// is queued or claimed (req-risk-based-review AC-11.3).
const PlanRiskRating = "plan_risk_rating"

// planRiskWait matches the row ahead of the gate row. The approval route
// refuses a plan approval while the version's risk order is open, so the
// gate row would ask a person for an approval the factory refuses. The row
// matches only an enforcing task waiting at plan approval whose current plan
// version's risk order is queued or claimed. Completion or cancellation ends
// the match, and the gate row applies again (feature-risk-based-review RR-16;
// req-risk-based-review AC-4.8, AC-11.3).
func planRiskWait(in Input) bool { return planRiskWaitOrder(in) != nil }

func planRiskWaitOrder(in Input) *core.WorkOrder {
	if in.Task.State != core.TaskAwaiting || in.Task.GateKind != core.GateKindPlanApproval || !in.Task.SetupContract.Risk.Enforcing() {
		return nil
	}
	ref := in.PlanRiskOrder
	if ref == nil || in.GateSpecVersion <= 0 || ref.PlanVersion != in.GateSpecVersion {
		return nil
	}
	order := ref.Order
	if order.Stage != core.StageRisk || order.TaskID != in.Task.ID {
		return nil
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return nil
	}
	return &order
}

// planRiskWaitBuild names the agent and the risk order. It asks no person for
// a judgment and offers no approval.
func planRiskWaitBuild(in Input) built {
	order := planRiskWaitOrder(in)
	row := built{actor: ActorAgent, kind: KindQueued, reason: PlanRiskRating, variant: PlanRiskRating, order: order, deadline: queueDeadline(*order)}
	if order.State == core.WorkOrderClaimed {
		row.kind, row.deadline = KindWorking, executionDeadline(*order)
	}
	return row
}

// SelectableOrders is the view of a task's orders that every next-action order
// selector reads. Under a frozen record mode, a risk order is never the order a
// next action names while another order of the task is queued or claimed, so
// the view drops the task's risk orders while any order of another stage is
// queued or claimed. Otherwise it returns orders unchanged. Under enforce the
// task waits for its risk order, so the view keeps it
// (feature-risk-based-review RR-15; req-risk-based-review AC-1.6).
func SelectableOrders(task core.Task, orders []core.WorkOrder) []core.WorkOrder {
	if task.SetupContract.Risk == nil || task.SetupContract.Risk.Mode != riskpolicy.ModeRecord {
		return orders
	}
	other := false
	for _, order := range orders {
		if order.Stage != core.StageRisk && (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) {
			other = true
			break
		}
	}
	if !other {
		return orders
	}
	view := make([]core.WorkOrder, 0, len(orders))
	for _, order := range orders {
		if order.Stage != core.StageRisk {
			view = append(view, order)
		}
	}
	return view
}
