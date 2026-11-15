package nextaction

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// PlanRiskRating is the catalogue variant and reason code of the plan risk
// wait row when it states a claimed or claimable risk order: an enforcing
// task at plan approval whose plan version's risk order an agent is rating or
// may claim (req-risk-based-review AC-11.3).
const PlanRiskRating = "plan_risk_rating"

// planRiskWait matches the row ahead of the gate row. The approval route
// refuses a plan approval while the version's risk order is open, so the
// gate row would ask a person for an approval the factory refuses. The row
// matches only an enforcing task waiting at plan approval whose current plan
// version's risk order is open: neither completed nor cancelled. Completion
// or cancellation ends the match, and the gate row applies again
// (component-next-action, The statement, row 3a; component-work-orders
// invariant 66; req-risk-based-review AC-4.8, AC-11.3).
func planRiskWait(in Input) bool { return planRiskWaitOrder(in) != nil }

// PlanRiskWaitOrder returns the open risk order that row 3a states, or nil
// when the row does not match. The assembler reads it to supply the inputs
// the row's statements need, such as the cited documents of the order's
// checkpoint.
func PlanRiskWaitOrder(in Input) *core.WorkOrder { return planRiskWaitOrder(in) }

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
	if order.State == core.WorkOrderCompleted || order.State == core.WorkOrderCancelled {
		return nil
	}
	// A stored stale order reads as the queued order the clock returns it
	// to, as every other row reads it (component-work-orders invariant 5).
	if order.State == core.WorkOrderStale {
		order = core.NormalizeLegacyStaleWorkOrder(order, in.Now)
	}
	return &order
}

// planRiskQuestion reports whether the risk order is queued and still
// released at a checkpoint with a decision request of its agent, the shape
// row 10 asks about (component-next-action invariant 10).
func planRiskQuestion(order core.WorkOrder) bool {
	return order.State == core.WorkOrderQueued && order.LastAttemptOutcome == core.WorkOrderOutcomeReleased &&
		order.Checkpoint != nil && order.Checkpoint.DecisionRequest != ""
}

// PlanRiskCheckpointCitations returns the citations of the decision request
// that row 3a states, or nil when the row states none. The assembler reads
// the cited documents for them, as it reads them for row 10.
func PlanRiskCheckpointCitations(in Input) []core.WorkOrderAuthorityConflictCitation {
	order := planRiskWaitOrder(in)
	if order == nil || !planRiskQuestion(*order) || order.Checkpoint.Class != core.WorkOrderCheckpointClassAuthorityConflict {
		return nil
	}
	return order.Checkpoint.Citations
}

// planRiskWaitBuild states the plan version's open risk order as rows 10,
// 15, 18, 19, and 20 would state it, in that order of precedence: a decision
// request of its agent, an order that row 15 finds unclaimable, a scheduled
// automatic recovery, a claimed order, and a claimable order. It reads only
// this order, never another order of the task. No statement carries a plan
// approval choice, because no kind it states offers one
// (component-next-action, The statement, row 3a; req-risk-based-review
// AC-11.3; req-operator-next-action AC-1.3, AC-1.6).
func planRiskWaitBuild(in Input) built {
	order := planRiskWaitOrder(in)
	if planRiskQuestion(*order) {
		return planRiskQuestionBuild(in, order)
	}
	if hit := stall(in.Task, []core.WorkOrder{*order}, in.Recovery, in.LiveClaimants, in.ClaimFailures, in.Now); hit != nil && hit.Reason != "" {
		return built{actor: ActorOperator, kind: KindStalled, stall: hit.Reason, reason: hit.Reason, order: &hit.Order}
	}
	if next := recoveryNext(in, order); next != nil && next.After(in.Now) {
		return built{actor: ActorFactory, kind: KindRetrying, reason: "automatic_recovery", order: order, deadline: next}
	}
	row := built{actor: ActorAgent, kind: KindQueued, reason: PlanRiskRating, variant: PlanRiskRating, order: order}
	if order.State == core.WorkOrderClaimed {
		row.kind, row.deadline = KindWorking, executionDeadline(*order)
	}
	return row
}

// planRiskQuestionBuild presents the risk order's own checkpoint as row 10
// presents the latest queued order's checkpoint.
func planRiskQuestionBuild(in Input, order *core.WorkOrder) built {
	checkpoint := order.Checkpoint
	row := built{
		actor: ActorOperator, kind: KindAgentQuestion, judgment: true, release: ReleaseCheckpoint,
		checkpoint: checkpoint.DecisionRequest, class: checkpoint.Class,
		citations: append([]core.WorkOrderAuthorityConflictCitation(nil), checkpoint.Citations...),
		order:     order,
	}
	if row.class != core.WorkOrderCheckpointClassAuthorityConflict {
		return row
	}
	row.proposals = CitedProposals(checkpoint.Citations, in.PendingProposals)
	switch {
	case len(row.proposals) > 0:
		row.variant = AgentQuestionCitedRevisions
	case CitedRevisionsConfirmed(row.citations, in.CitedDocuments):
		row.variant = AgentQuestionCitedConfirmed
	default:
		row.variant = AgentQuestionRevisionNeeded
	}
	return row
}

// SelectableOrders is the view of a task's orders that every next-action order
// selector reads. Under a frozen record mode, a risk order is never the order a
// next action names while another order of the task is queued or claimed, so
// the view drops the task's risk orders while any order of another stage is
// queued or claimed. Otherwise it returns orders unchanged. Under enforce the
// task waits for its risk order, so the view keeps it
// (component-next-action, The statement; req-risk-based-review AC-1.6).
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
