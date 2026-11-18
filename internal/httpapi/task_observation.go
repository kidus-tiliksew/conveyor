package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// getTaskObservation answers GET /v1/tasks/{id}/observation: the narrow read a
// wait loop polls, holding only the task state, its plan state, its frozen
// merge approval, the compact next action, and the task's own claimable
// orders. It composes no task context, no event fold, and no plan text, and
// reads no workspace-wide order listing. A poll costs two validation reads,
// one task read, one task order read, one marker read, the next-action
// projection, and the work-order list's claim projections over this task's
// orders: the dependency blocker read for a queued implementation or
// verification order, the recovery task and gate reads for an order that
// recovery can refuse, and the risk subject read for a risk order
// (component-next-action invariant 57; req-agent-skills REQ-4, AC-4.1
// through AC-4.3).
//
// The route answers under the conditional statement read: it projects
// first and answers 304 only when the tag of the body projected now matches
// and its inputs held still (component-next-action invariant 58). The tag
// covers every input above, the claimability projections included, through
// the validation read and the encoded body. One read of the task's markers
// states the review lease horizon.
func (s *Server) getTaskObservation(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	s.answerStatement(w, r, "observation", taskID, func(ctx context.Context, opts store.NextActionOptions) (any, error) {
		task, err := s.Store.GetTask(ctx, taskID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, statementNotFound{err}
			}
			return nil, err
		}
		// One bounded read of this task's orders serves both the next action
		// and the claimable list, so the route never reads the workspace
		// listing (component-next-action invariant 57).
		orders, err := s.Store.ListTaskWorkOrders(ctx, taskID)
		if err != nil {
			return nil, err
		}
		inputs, err := s.Store.ReadActivityInputs(ctx, []string{task.ID}, opts.Now)
		if err != nil {
			return nil, err
		}
		memoHorizon(ctx, inputs.ValidUntil)
		orderHorizon(ctx, orders)
		opts.MarkersLoaded, opts.Markers = true, inputs.Markers
		action, err := store.ReadNextActionForTask(ctx, s.Store, task, orders, opts)
		if err != nil {
			return nil, err
		}
		if action.Deadline != nil {
			memoHorizon(ctx, *action.Deadline)
		}
		claimable, err := s.projectTaskClaimability(ctx, task, orders)
		if err != nil {
			return nil, err
		}
		observation := taskObservation{
			TaskID:          task.ID,
			State:           task.State,
			PlanState:       task.PlanState,
			MergeApproval:   task.MergeApproval,
			NextAction:      taskObservationAction{Compact: nextaction.CompactFrom(action)},
			ClaimableOrders: taskObservationClaimableOrders(claimable),
		}
		if action.CurrentOrder != nil {
			observation.NextAction.CurrentOrder = &taskObservationOrder{ID: action.CurrentOrder.ID, Stage: core.Stage(action.CurrentOrder.Stage)}
		}
		return observation, nil
	})
}

// taskObservation is the narrow wait observation of one task. It carries only
// the fields a wait branches on: the state and merge approval that select the
// gate, the plan state, the next action, and the claimable orders
// (component-cli-onboarding invariant 22).
type taskObservation struct {
	TaskID          string                 `json:"task_id"`
	State           core.TaskState         `json:"state"`
	PlanState       core.PlanState         `json:"plan_state"`
	MergeApproval   bool                   `json:"merge_approval"`
	NextAction      taskObservationAction  `json:"next_action"`
	ClaimableOrders []taskObservationOrder `json:"claimable_orders"`
}

// taskObservationAction is the compact next action plus the order it works, so
// a --until stage wait compares the same stage as the full task read served.
type taskObservationAction struct {
	nextaction.Compact
	CurrentOrder *taskObservationOrder `json:"current_order,omitempty"`
}

// taskObservationOrder is one order reference. QueueEnteredAt lets the caller
// rank two orders of the same stage without reading the order rows.
type taskObservationOrder struct {
	ID             string     `json:"id"`
	Stage          core.Stage `json:"stage"`
	QueueEnteredAt time.Time  `json:"queue_entered_at,omitzero"`
}

// projectTaskClaimability applies to one task's orders the claim projections
// that GET /v1/work-orders applies for the same caller: the work-order
// service's dependency, recovery, and risk projections, then the assignee
// projection. Without a configured service the list route serves the stored
// orders, and so does this read. The task order read does not carry the
// assignee on every backend, so the assignee comes from the task read the
// route already made. A projection failure fails the read, so an order never
// reads as claimable because a projection input was unreadable
// (component-next-action invariant 57; component-work-orders invariant 59).
func (s *Server) projectTaskClaimability(ctx context.Context, task core.Task, orders []core.WorkOrder) ([]core.WorkOrder, error) {
	projected := slices.Clone(orders)
	if s.WorkOrders != nil {
		var err error
		if projected, err = s.WorkOrders.ProjectTaskOrders(ctx, orders); err != nil {
			return nil, err
		}
	}
	for i := range projected {
		projected[i].Assignee = task.Assignee
	}
	return projectAssigneeClaimability(ctx, projected), nil
}

// taskObservationStageRank orders claimable stages the way a session works
// them. A risk order ranks last, so ordinary work beside it comes first and a
// lone risk order is still reported (component-cli-onboarding invariant 22).
var taskObservationStageRank = map[core.Stage]int{core.StageSpec: 0, core.StageImplement: 1, core.StageVerify: 2, core.StageReview: 3, core.StageRisk: 4}

// taskObservationClaimableOrders lists the queued, claimable orders of a known
// stage among projected orders, ranked spec, implement, verify, review, then
// risk, then by the oldest queue entry, then by order ID. A wait takes the
// first entry as its next order. The list is never null: an observation with
// no claimable order reports an empty list (component-cli-onboarding
// invariant 22).
func taskObservationClaimableOrders(orders []core.WorkOrder) []taskObservationOrder {
	claimable := make([]taskObservationOrder, 0, len(orders))
	for _, order := range orders {
		if order.State != core.WorkOrderQueued || !order.Claimable || !core.ValidWorkOrderStage(order.Stage) {
			continue
		}
		claimable = append(claimable, taskObservationOrder{ID: order.ID, Stage: order.Stage, QueueEnteredAt: order.QueueEnteredAt})
	}
	slices.SortFunc(claimable, func(a, b taskObservationOrder) int {
		if rank := cmp.Compare(taskObservationStageRank[a.Stage], taskObservationStageRank[b.Stage]); rank != 0 {
			return rank
		}
		if entered := a.QueueEnteredAt.Compare(b.QueueEnteredAt); entered != 0 {
			return entered
		}
		return strings.Compare(a.ID, b.ID)
	})
	return claimable
}

// taskObservationValidatorScope binds a tag to its route, workspace, caller,
// capabilities, and task (component-next-action invariant 58).
func taskObservationValidatorScope(ctx context.Context, route, taskID string, capabilities map[core.Capability]bool) []byte {
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	credential, _ := store.CredentialFromContext(ctx)
	granted := make([]string, 0, len(nextActionCapabilities))
	for _, capability := range nextActionCapabilities {
		if capabilities[capability] {
			granted = append(granted, string(capability))
		}
	}
	scope, _ := json.Marshal(struct {
		Version      string   `json:"version"`
		Route        string   `json:"route"`
		Workspace    string   `json:"workspace"`
		User         string   `json:"user"`
		Capabilities []string `json:"capabilities"`
		Task         string   `json:"task"`
	}{"task-observation-v1", route, workspaceID, credential.OwnerUserID, granted, taskID})
	return scope
}
