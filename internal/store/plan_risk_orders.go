package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Plan risk orders (feature-risk-based-review RR-13; component-risk-review
// invariants 12 and 13; req-risk-based-review AC-4.7 to AC-4.9). A plan
// version that enters plan awaiting approval gets a risk order when the need
// decision calls for one. Every backend applies PlanRiskMoveFor in the
// transaction that commits the plan-state move, under the task's operation
// lock, so the plan version, the move, the order, and their events commit
// together or not at all. The risk policy never reads or sets the plan state:
// the move decides the order, never the reverse (DEC-125(5) to (7)).

// PlanRiskRatingOpenCode is the refusal code of a plan approval that waits
// for its plan version's risk order (req-risk-based-review AC-4.8;
// feature-risk-based-review RR-13).
const PlanRiskRatingOpenCode = "plan_risk_rating_open"

// ErrPlanRiskRatingOpen matches every *PlanRiskRatingOpenError.
var ErrPlanRiskRatingOpen = errors.New(PlanRiskRatingOpenCode)

// PlanRiskRatingOpenError refuses a plan approval of an enforcing task while
// the approved version's risk order is still open. The refused act changes
// nothing (req-risk-based-review AC-4.8; req-task-plan-state AC-1.3).
type PlanRiskRatingOpenError struct {
	TaskID      string
	PlanVersion int
	WorkOrderID string
	State       core.WorkOrderState
}

func (e *PlanRiskRatingOpenError) Error() string {
	return fmt.Sprintf("%s: plan version %d of task %s waits for its risk order %s (%s); approve the plan after the risk order completes or is cancelled",
		PlanRiskRatingOpenCode, e.PlanVersion, e.TaskID, e.WorkOrderID, e.State)
}

// Is makes errors.Is(err, ErrPlanRiskRatingOpen) match.
func (e *PlanRiskRatingOpenError) Is(target error) bool { return target == ErrPlanRiskRatingOpen }

// PlanApprovalRiskRefusal returns *PlanRiskRatingOpenError when the task's
// frozen risk mode is enforce and the risk order of plan version is open:
// queued, claimed, or stale or timed out and waiting for recovery to return it
// to the queue. Only completion or cancellation ends the wait. Under record or
// off it reads nothing and returns nil, so approval never waits
// (component-risk-review invariant 13; req-risk-based-review AC-1.6, AC-4.8).
func PlanApprovalRiskRefusal(task core.Task, orders []core.RiskOrder, version int) error {
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	for _, order := range orders {
		if order.Subject.Kind() == riskpolicy.RiskSubjectPlan && order.Subject.PlanVersion == version && order.Open() {
			return &PlanRiskRatingOpenError{TaskID: task.ID, PlanVersion: version, WorkOrderID: order.Order.ID, State: order.Order.State}
		}
	}
	return nil
}

// Cancellation reasons of plan risk orders (req-risk-based-review AC-4.9).
const (
	// PlanRiskReplacedReason cancels the open risk order of an earlier plan
	// version when a later plan version enters plan awaiting approval.
	PlanRiskReplacedReason = "a later plan version replaced the rated plan version"
	// PlanRiskRedirectedReason cancels the open risk order of a redirected
	// plan version.
	PlanRiskRedirectedReason = "the rated plan version was redirected"
	// PlanRiskOperatorExitReason cancels the open risk order of a plan
	// version that an operator act took out of plan awaiting approval.
	PlanRiskOperatorExitReason = "an operator act took the rated plan version out of plan awaiting approval"
)

// PlanRiskMove is what one plan-state move does to the task's plan risk
// orders. Create asks for a risk order for the latest plan version. Cancel
// names the reason that cancels every open plan risk order except the one of
// the latest plan version when Create is set; an empty Cancel cancels none.
type PlanRiskMove struct {
	Create bool
	Cancel string
}

// PlanRiskMoveFor decides the plan risk effects of a move from one plan state
// to another (req-risk-based-review AC-4.7, AC-4.9; feature-risk-based-review
// RR-13):
//
//   - A move into plan awaiting approval asks for the latest plan version's
//     risk order and cancels the open order of any earlier version, which
//     the later version replaces.
//   - A move out of plan awaiting approval to anything but planning completed
//     is a redirect or an operator exit and cancels the open order.
//   - Approval cancels nothing: under record the risk agent may finish after
//     approval, and under enforce approval waits for the order to end.
//
// A move out of planning completed cancels nothing, so the rating of an
// approved version still counts when its order completes later (AC-4.3).
func PlanRiskMoveFor(from, to core.PlanState, cause core.PlanStateCause) PlanRiskMove {
	switch {
	case from == to:
		return PlanRiskMove{}
	case to == core.PlanAwaitingApproval:
		return PlanRiskMove{Create: true, Cancel: PlanRiskReplacedReason}
	case from == core.PlanAwaitingApproval && to != core.PlanCompleted:
		if cause == core.PlanCausePlanRedirected {
			return PlanRiskMove{Cancel: PlanRiskRedirectedReason}
		}
		return PlanRiskMove{Cancel: PlanRiskOperatorExitReason}
	}
	return PlanRiskMove{}
}

// PlanRiskCancelIDs lists the open plan risk orders a move cancels: every
// open order whose subject is a plan version other than keep. Head risk
// orders are never touched here (RR-12 owns them). keep is zero when the move
// creates no order.
func PlanRiskCancelIDs(orders []core.RiskOrder, keep int) []string {
	ids := []string{}
	for _, order := range orders {
		if order.Subject.Kind() != riskpolicy.RiskSubjectPlan || !order.Open() || (keep > 0 && order.Subject.PlanVersion == keep) {
			continue
		}
		ids = append(ids, order.Order.ID)
	}
	return ids
}

// PlanRiskOrderRequest returns the creation request for plan version's risk
// order, and false when the need decision calls for none: the task's frozen
// mode is off, or its tier, counted as every read counts it, is high
// (component-risk-review invariant 12; req-risk-based-review AC-4.7,
// AC-12.3). counted are the task's ratings with PlanPending derived, so an
// unapproved plan version's rating never decides the answer (AC-4.3).
func PlanRiskOrderRequest(task core.Task, counted []core.RiskRating, version int, now time.Time) (RiskOrderCreateRequest, bool) {
	if version <= 0 || task.SetupContract.Risk == nil {
		return RiskOrderCreateRequest{}, false
	}
	tier := riskpolicy.Tier("")
	if setter, ok := core.RiskTierOf(counted); ok {
		tier = setter.Tier
	}
	if !riskpolicy.NeedsRiskOrder(riskpolicy.RiskOrderNeedInput{Mode: task.SetupContract.Risk.Mode, Tier: tier, SubjectKind: riskpolicy.RiskSubjectPlan}) {
		return RiskOrderCreateRequest{}, false
	}
	return RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{PlanVersion: version}, Now: now}, true
}

// PlanRiskWorkOrderID returns the ID of plan version's risk order, or empty
// when the version has none. A submission names it as risk_work_order_id
// (req-risk-based-review AC-12.6).
func PlanRiskWorkOrderID(orders []core.RiskOrder, version int) string {
	for _, order := range orders {
		if order.Subject.Kind() == riskpolicy.RiskSubjectPlan && order.Subject.PlanVersion == version {
			return order.Order.ID
		}
	}
	return ""
}

// ValidateTaskCommandPlanVersion refuses a lifecycle command that carries a
// plan version without a move into plan awaiting approval.
func ValidateTaskCommandPlanVersion(command taskops.Command) error {
	if command.PlanVersion == nil {
		return nil
	}
	if command.PlanState == nil || command.PlanState.To != core.PlanAwaitingApproval {
		return Refusef("a command that records a plan version must move the plan state to %s", core.PlanAwaitingApproval)
	}
	return nil
}

// Memory backend.

// planApprovalRiskRefusalLocked applies PlanApprovalRiskRefusal under m.mu.
func (m *memory) planApprovalRiskRefusalLocked(ctx context.Context, taskID string, version int) error {
	task, ok := m.tasks[taskID]
	if !ok || !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	workspace, _ := WorkspaceFromContext(ctx)
	return PlanApprovalRiskRefusal(task, m.taskRiskOrdersLocked(workspace, taskID), version)
}

// applyPlanRiskMoveLocked applies PlanRiskMoveFor to a plan-state move that
// task, as it now stands, has just taken from the state from. It runs under
// m.mu in the critical section of the move.
func (m *memory) applyPlanRiskMoveLocked(ctx context.Context, task core.Task, from core.PlanState, cause core.PlanStateCause, at time.Time) error {
	move := PlanRiskMoveFor(from, task.PlanState, cause)
	if !move.Create && move.Cancel == "" {
		return nil
	}
	workspace, _ := WorkspaceFromContext(ctx)
	latest := 0
	if move.Create {
		if versions := m.specs[task.ID]; len(versions) > 0 {
			latest = versions[len(versions)-1].Version
		}
	}
	if move.Cancel != "" {
		for _, id := range PlanRiskCancelIDs(m.taskRiskOrdersLocked(workspace, task.ID), latest) {
			m.cancelRiskOrdersLocked(ctx, task.ID, id, move.Cancel, at)
		}
	}
	if request, ok := PlanRiskOrderRequest(task, m.riskRatingsLocked(ctx, task), latest, at); ok && move.Create {
		if _, _, err := m.createRiskOrderLocked(ctx, task, request); err != nil {
			return err
		}
	}
	return nil
}
