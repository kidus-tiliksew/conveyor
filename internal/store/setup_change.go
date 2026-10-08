package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ErrSetupChangeConflict reports a frozen-policy change refused by task or
// order state, or a replayed request_id with different inputs. The name is
// kept for the task_setup_changes replay record the command shares with
// retired setup changes.
var ErrSetupChangeConflict = fmt.Errorf("setup change conflict")

// SetupChangeRequest is one operator frozen-policy change. Callers supply only
// TaskID, RequestID, Reason, and Policy; the backend derives every other
// field with PlanTaskPolicyChange inside its task transaction, after replay
// resolution and claim exclusion, and never accepts a caller-supplied order
// plan or execution setup (DEC-7, DEC-43, DEC-56). The derived fields keep
// their historical names because persisted replay results and audit events
// use them.
type SetupChangeRequest struct {
	PolicyActor Actor `json:"policy_actor,omitempty"`
	Policy      *TaskPolicyChange
	TaskID      string
	RequestID   string
	Reason      string
	// Derived by PlanTaskPolicyChange.
	NextStage             core.Stage
	Setup                 config.ExecutionSetup
	WorkOrderUpdates      []core.WorkOrder
	NewJobs               []core.Job
	NewWorkOrders         []core.WorkOrder
	SupersedeWorkOrderIDs []string
	ReviewTransition      string
	PriorReviewRound      int
	ResultingReviewRound  int
}

// SetupChangeResult is the persisted replay result of a frozen-policy change.
// RetainedWorkOrders stays in the wire and replay shape; only retired setup
// changes populated it.
type SetupChangeResult struct {
	RequestID            string    `json:"request_id"`
	Task                 core.Task `json:"task"`
	ReviewTransition     string    `json:"review_transition"`
	UpdatedWorkOrders    []string  `json:"updated_work_orders"`
	CreatedWorkOrders    []string  `json:"created_work_orders"`
	RetainedWorkOrders   []string  `json:"retained_work_orders"`
	SupersededWorkOrders []string  `json:"superseded_work_orders"`
}

type memorySetupChange struct {
	Workspace string
	Request   SetupChangeRequest
	Result    SetupChangeResult
}

func normalizeSetupChangeRequest(request SetupChangeRequest) SetupChangeRequest {
	if request.Policy != nil {
		policy := *request.Policy
		if policy.VerifyStage != nil {
			enabled := *policy.VerifyStage
			policy.VerifyStage = &enabled
		}
		if policy.StageTimeouts != nil {
			policy.StageTimeouts = map[string]string{}
			for k, v := range request.Policy.StageTimeouts {
				policy.StageTimeouts[k] = v
			}
		}
		request.Policy = &policy
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Reason = strings.TrimSpace(request.Reason)
	return request
}

func validateSetupChangeRequest(request SetupChangeRequest) error {
	if request.Policy == nil {
		return fmt.Errorf("a frozen-policy change is required")
	}
	if request.TaskID == "" || request.RequestID == "" || request.Reason == "" {
		return fmt.Errorf("task, request_id, and nonblank reason are required")
	}
	return request.Policy.Validate()
}

// PrepareSetupChangeRequest normalizes the operator's policy request and
// validates the command identity shared by every persistence implementation.
func PrepareSetupChangeRequest(request SetupChangeRequest) (SetupChangeRequest, error) {
	request = normalizeSetupChangeRequest(request)
	return request, validateSetupChangeRequest(request)
}

// SameSetupChange reports whether a replay carries the same command identity.
func SameSetupChange(left, right SetupChangeRequest) bool {
	return reflect.DeepEqual(SetupChangeIdentity(left), SetupChangeIdentity(right))
}

// SetupChangeIdentity is the replay identity persisted in task_setup_changes:
// task, request ID, reason, policy, and the authenticated actor. Its JSON shape
// is unchanged so stored policy replays still match.
func SetupChangeIdentity(request SetupChangeRequest) any {
	return struct {
		TaskID, RequestID, Reason string
		Policy                    *TaskPolicyChange
		Actor                     Actor
	}{request.TaskID, request.RequestID, request.Reason, request.Policy, request.PolicyActor}
}

func setupChangePayload(workspace string, actor Actor, prior config.ExecutionSetup, request SetupChangeRequest, result SetupChangeResult) map[string]any {
	return map[string]any{
		"workspace_id": workspace, "task_id": request.TaskID, "actor": actor.ID,
		"request_id": request.RequestID, "reason": request.Reason,
		"previous_setup": prior, "new_setup": request.Setup,
		"lifecycle_boundary": "future_work", "stage": result.Task.NextStage,
		"review_transition": map[string]any{
			"kind": request.ReviewTransition, "prior_round": request.PriorReviewRound,
			"resulting_round":           request.ResultingReviewRound,
			"retained_work_order_ids":   result.RetainedWorkOrders,
			"superseded_work_order_ids": result.SupersededWorkOrders,
			"created_work_order_ids":    result.CreatedWorkOrders,
		},
		"updated_work_order_ids": result.UpdatedWorkOrders,
	}
}

func (m *memory) ChangeTaskPolicyCommand(ctx context.Context, lease taskops.TaskLease, raw SetupChangeRequest) (SetupChangeResult, error) {
	request, err := PrepareSetupChangeRequest(raw)
	request.PolicyActor = ActorFromContext(ctx)
	if !lease.ValidForCommand(request.TaskID, taskops.SetupChangeCommand) {
		return SetupChangeResult{}, fmt.Errorf("taskops lease does not authorize policy change for task %s", request.TaskID)
	}
	if err != nil {
		return SetupChangeResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, _ := WorkspaceFromContext(ctx)
	if prior, ok := m.setupChanges[request.RequestID]; ok {
		if prior.Workspace != workspace || !SameSetupChange(prior.Request, request) {
			return SetupChangeResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrSetupChangeConflict, request.RequestID)
		}
		return prior.Result, nil
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || task.Workspace != workspace {
		return SetupChangeResult{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	if core.TaskTerminal(task.State) {
		return SetupChangeResult{}, fmt.Errorf("%w: terminal task %s cannot change policy", ErrSetupChangeConflict, request.TaskID)
	}
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID != task.ID {
			continue
		}
		// Submitted spec/implement attempts are delivered, not executing; only
		// claimed attempts and in-flight review verdicts block.
		if order.State == core.WorkOrderClaimed {
			return SetupChangeResult{}, fmt.Errorf("%w: task %s has a claimed attempt", ErrSetupChangeConflict, request.TaskID)
		}
		if order.Stage == core.StageReview && order.State == core.WorkOrderSubmitted {
			return SetupChangeResult{}, fmt.Errorf("%w: task %s has an in-flight review verdict", ErrSetupChangeConflict, request.TaskID)
		}
		orders = append(orders, order)
	}
	request, err = PlanTaskPolicyChange(task, orders, request)
	if err != nil {
		return SetupChangeResult{}, err
	}
	for _, desired := range request.WorkOrderUpdates {
		current, exists := m.workOrders[desired.ID]
		if !exists || current.TaskID != task.ID || current.State != core.WorkOrderQueued || current.SessionID != "" || current.WorkerID != "" {
			return SetupChangeResult{}, fmt.Errorf("%w: work order %s is not an unclaimed queued order", ErrSetupChangeConflict, desired.ID)
		}
	}
	for i, job := range request.NewJobs {
		order := request.NewWorkOrders[i]
		if job.TaskID != task.ID || order.TaskID != task.ID || order.JobID != job.ID || order.Stage != job.Stage {
			return SetupChangeResult{}, fmt.Errorf("invalid policy-change handoff member %d", i)
		}
		if _, _, exists := m.findJobLocked(job.ID); exists {
			return SetupChangeResult{}, fmt.Errorf("%w: %w: job %s already exists", ErrSetupChangeConflict, ErrDispatchJobConflict, job.ID)
		}
		if _, exists := m.workOrders[order.ID]; exists {
			return SetupChangeResult{}, fmt.Errorf("%w: work order %s already exists", ErrSetupChangeConflict, order.ID)
		}
	}
	for _, id := range request.SupersedeWorkOrderIDs {
		order, exists := m.workOrders[id]
		if !exists || order.TaskID != task.ID || (order.State != core.WorkOrderQueued && order.State != core.WorkOrderCompleted) {
			return SetupChangeResult{}, fmt.Errorf("%w: superseded work order %s is invalid", ErrSetupChangeConflict, id)
		}
	}
	now := time.Now().UTC()
	fromStage := task.NextStage
	priorSetup := task.SetupContract
	// The frozen policy changes; the task's legacy SetupName is left as is.
	task.SetupContract = request.Setup
	task.NextStage = request.NextStage
	m.tasks[task.ID] = task
	result := SetupChangeResult{RequestID: request.RequestID, Task: task, ReviewTransition: request.ReviewTransition,
		UpdatedWorkOrders: make([]string, 0, len(request.WorkOrderUpdates)), CreatedWorkOrders: make([]string, 0, len(request.NewWorkOrders)),
		RetainedWorkOrders: []string{}, SupersededWorkOrders: append([]string(nil), request.SupersedeWorkOrderIDs...)}
	actor := ActorFromContext(ctx)
	if fromStage != task.NextStage {
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.transition_decided", ActorID: actor.ID, ActorRole: actor.Role, At: now, Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": task.NextStage, "recovery_stage": task.RecoveryStage, "state": task.State})})
	}
	for _, desired := range request.WorkOrderUpdates {
		current := m.workOrders[desired.ID]
		applyFutureRouting(&current, desired, now)
		m.workOrders[current.ID] = current
		result.UpdatedWorkOrders = append(result.UpdatedWorkOrders, current.ID)
		if current.Stage == core.StageReview {
			m.appendEventLocked(ctx, setupSeatEvent(task.ID, current, actor, workspace, priorSetup, request, "rebuilt_future_work", now))
		}
	}
	for _, id := range request.SupersedeWorkOrderIDs {
		order, exists := m.workOrders[id]
		if !exists || order.TaskID != task.ID {
			return SetupChangeResult{}, fmt.Errorf("%w: superseded work order %s is invalid", ErrSetupChangeConflict, id)
		}
		priorState := order.State
		if order.State == core.WorkOrderQueued {
			state, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdCancel)
			if transitionErr != nil {
				return SetupChangeResult{}, transitionErr
			}
			order.State, order.Claimable, order.UpdatedAt = state, false, now
			m.workOrders[id] = order
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role,
				Payload: core.JSONPayload(order), At: now})
			if job, index, found := m.findJobLocked(order.JobID); found {
				job.State, job.EndedAt = core.JobFailed, now
				m.jobs[job.TaskID][index] = job
			}
		}
		if order.Stage != core.StageReview {
			continue
		}
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "review.seat.setup_superseded", ActorID: actor.ID, ActorRole: actor.Role,
			Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "request_id": request.RequestID, "work_order_id": id,
				"prior_state": priorState, "resulting_state": order.State, "outcome": "historical_only", "previous_setup": priorSetup, "new_setup": request.Setup}), At: now})
	}
	for i, job := range request.NewJobs {
		order := request.NewWorkOrders[i]
		if job.State == "" {
			job.State = core.JobPending
		}
		m.jobs[task.ID] = append(m.jobs[task.ID], job)
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now})
		if order.CreatedAt.IsZero() {
			order.CreatedAt = now
		}
		if order.QueueEnteredAt.IsZero() {
			order.QueueEnteredAt = now
		}
		if order.QueueDeadline.IsZero() {
			order.QueueDeadline = now.Add(config.DefaultWorkOrderQueueTimeout)
		}
		state, transitionErr := core.TransitionWorkOrder("", core.WorkOrderCmdCreate)
		if transitionErr != nil {
			return SetupChangeResult{}, transitionErr
		}
		order.State, order.Claimable, order.UpdatedAt = state, true, now
		if order.Stage == core.StageVerify && m.taskBlockedLocked(task.ID) {
			order.QueueBlockedAt, order.Claimable = now, false
		}
		m.workOrders[order.ID] = order
		result.CreatedWorkOrders = append(result.CreatedWorkOrders, order.ID)
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order), At: now})
		if order.Stage == core.StageReview {
			m.appendEventLocked(ctx, setupSeatEvent(task.ID, order, actor, workspace, priorSetup, request, "created_under_new_setup", now))
		}
	}
	payload := setupChangePayload(workspace, actor, priorSetup, request, result)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(payload), At: now})
	m.setupChanges[request.RequestID] = memorySetupChange{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func applyFutureRouting(current *core.WorkOrder, desired core.WorkOrder, now time.Time) {
	current.RequiredModel, current.RequiredHarness, current.RequiredEffort = desired.RequiredModel, desired.RequiredHarness, desired.RequiredEffort
	current.RequiredHarnessConfig, current.ExecutionTimeoutText = desired.RequiredHarnessConfig, desired.ExecutionTimeoutText
	current.LastAttemptOutcome, current.LastFailureMessage, current.LastFailureDetail = "", "", ""
	current.LastFailureExitStatus, current.LastFailureAt = nil, time.Time{}
	current.AutomaticRetryCount, current.NextRetryAt = 0, time.Time{}
	current.RetrySuppressed, current.RetrySuppressionReason = false, ""
	current.QueueEnteredAt, current.QueueDeadline = desired.QueueEnteredAt, desired.QueueDeadline
	if current.QueueEnteredAt.IsZero() {
		current.QueueEnteredAt = now
	}
	if current.QueueDeadline.IsZero() {
		current.QueueDeadline = now.Add(config.DefaultWorkOrderQueueTimeout)
	}
	current.RedispatchCount++
	current.Claimable, current.UpdatedAt = true, now
}

func setupSeatEvent(taskID string, order core.WorkOrder, actor Actor, workspace string, previous config.ExecutionSetup, request SetupChangeRequest, outcome string, now time.Time) core.Event {
	return core.Event{TaskID: taskID, JobID: order.JobID, Kind: "review.seat.setup_rebuilt", ActorID: actor.ID, ActorRole: actor.Role,
		Payload: core.JSONPayload(map[string]any{"workspace_id": workspace, "request_id": request.RequestID, "review_round": order.ReviewRound,
			"review_seat": order.ReviewSeat, "work_order_id": order.ID, "outcome": outcome, "previous_setup": previous, "new_setup": request.Setup}), At: now}
}

// SupersededReviewWorkOrders derives immutable historical-only review evidence
// from the append-only setup-change audit trail.
func SupersededReviewWorkOrders(events []core.Event) map[string]bool {
	result := map[string]bool{}
	for _, event := range events {
		if event.Kind != "task.setup.changed" {
			continue
		}
		var payload struct {
			ReviewTransition struct {
				Superseded []string `json:"superseded_work_order_ids"`
			} `json:"review_transition"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		for _, id := range payload.ReviewTransition.Superseded {
			result[id] = true
		}
	}
	return result
}

func CurrentReviewOrders(orders []core.WorkOrder, events []core.Event) []core.WorkOrder {
	superseded := SupersededReviewWorkOrders(events)
	result := make([]core.WorkOrder, 0, len(orders))
	for _, order := range orders {
		if !superseded[order.ID] {
			result = append(result, order)
		}
	}
	return result
}
