package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
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
// reads no workspace-wide order listing, so a poll costs one task read, one
// task order read, and the next-action projection
// (component-next-action; req-agent-skills REQ-4, AC-4.1 through AC-4.3).
//
// An unchanged poll answers 304 from the validation read alone, before the
// task read or the projection, exactly as the Board activity read does
// (component-http-api, Activity conditional admission).
func (s *Server) getTaskObservation(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	now := activityClock().UTC().Truncate(time.Microsecond)
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts.Now = now
	validation, err := s.Store.ReadActivityValidation(r.Context(), now)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	conditional := &activityConditional{now: now, opts: opts, scope: taskObservationValidatorScope(r.Context(), "observation", taskID, opts.Capabilities), validation: validation}
	if tag, admitted := conditional.admits(r.Header.Get("If-None-Match")); admitted {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", tag.raw)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	task, err := s.Store.GetTask(r.Context(), taskID)
	if err != nil {
		writeClassifiedStoreError(w, r, err, refuseSentinel(store.ErrNotFound, http.StatusNotFound, "task not found"))
		return
	}
	// One bounded read of this task's orders serves both the next action and
	// the claimable list, so the route never reads the workspace listing
	// (component-persistence, Board activity validation reads).
	orders, err := s.Store.ListTaskWorkOrders(r.Context(), taskID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	action, err := store.ReadNextActionForTask(r.Context(), s.Store, task, orders, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	observation := taskObservation{
		TaskID:          task.ID,
		State:           task.State,
		PlanState:       task.PlanState,
		MergeApproval:   task.MergeApproval,
		NextAction:      taskObservationAction{Compact: nextaction.CompactFrom(action)},
		ClaimableOrders: taskObservationClaimableOrders(orders),
	}
	if action.CurrentOrder != nil {
		observation.NextAction.CurrentOrder = &taskObservationOrder{ID: action.CurrentOrder.ID, Stage: core.Stage(action.CurrentOrder.Stage)}
	}
	w.Header().Set("ETag", conditional.etag(taskObservationValidUntil(now, action), ""))
	writeJSON(w, http.StatusOK, observation)
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

// taskObservationClaimableOrders lists the task's queued, claimable orders of
// a known stage. The list is never null: an observation with no claimable
// order reports an empty list (component-cli-onboarding invariant 22).
func taskObservationClaimableOrders(orders []core.WorkOrder) []taskObservationOrder {
	claimable := make([]taskObservationOrder, 0, len(orders))
	for _, order := range orders {
		if order.State != core.WorkOrderQueued || !order.Claimable || !core.ValidWorkOrderStage(order.Stage) {
			continue
		}
		claimable = append(claimable, taskObservationOrder{ID: order.ID, Stage: order.Stage, QueueEnteredAt: order.QueueEnteredAt})
	}
	return claimable
}

// taskObservationValidUntil is the next UTC minute boundary, pulled earlier to
// a next-action deadline inside it, because a crossed deadline changes the
// stated summary (component-http-api, Activity conditional admission).
func taskObservationValidUntil(now time.Time, action nextaction.NextAction) time.Time {
	until := now.Truncate(time.Minute).Add(time.Minute)
	if deadline := action.Deadline; deadline != nil && deadline.After(now) && deadline.Before(until) {
		until = *deadline
	}
	return until
}

// taskObservationValidatorScope binds a tag to its route, workspace, caller,
// capabilities, and task (component-http-api, Activity conditional admission).
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
