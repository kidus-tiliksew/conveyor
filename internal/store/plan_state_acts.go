package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// PlanStateChangedSuppressionReason is the retry suppression reason of a plan
// or implement order an operator plan act cancels (component-plan-state
// invariant 4).
const PlanStateChangedSuppressionReason = "plan_state_changed"

// PlanActs is the plan_acts read: each flag is true exactly when the act
// would be accepted, setting aside request-body validity and the caller's
// capability (component-plan-state invariant 6; DEC-61).
type PlanActs struct {
	SetNeeded      bool `json:"set_needed"`
	SetNotRequired bool `json:"set_not_required"`
	Attach         bool `json:"attach"`
}

// PlanActsFor computes plan_acts from a task and its orders as the task read
// already loaded them, with the predicates the two acts apply, so the read
// makes no store call of its own (component-plan-state invariant 6).
func PlanActsFor(task core.Task, orders []core.WorkOrder) PlanActs {
	_, needed := PlanPlanStateChange(task, orders, core.PlanNeeded)
	_, notRequired := PlanPlanStateChange(task, orders, core.PlanNotRequired)
	_, attach := PlanAttachPlan(task, orders)
	return PlanActs{SetNeeded: needed == nil, SetNotRequired: notRequired == nil, Attach: attach == nil}
}

// PlanStateEffects is the outcome of an accepted plan act inside the
// backend's task transaction: the orders it cancels, the plan-state move, and
// the lifecycle command it issues, if any (component-plan-state invariants 4
// and 5).
type PlanStateEffects struct {
	From core.PlanState `json:"from"`
	To   core.PlanState `json:"to"`
	// Cause is operator for a /policy change and plan_attached for an attach.
	Cause core.PlanStateCause `json:"cause"`
	// CancelWorkOrderIDs are the plan or implement orders the act cancels,
	// a claimed plan order as the terminal sweep cancels an order.
	CancelWorkOrderIDs []string `json:"cancel_work_order_ids"`
	// Command is plan.reroute, gate.spec, or empty for a move of the state
	// alone.
	Command       core.TaskCommand `json:"command,omitempty"`
	NextStage     core.Stage       `json:"next_stage,omitempty"`
	RecoveryStage core.Stage       `json:"recovery_stage,omitempty"`
}

// TaskCommand is the lifecycle command an act issues, carrying its plan-state
// move, or nil when the act moves the state alone.
func (e PlanStateEffects) TaskCommand() *taskops.Command {
	if e.Command == "" {
		return nil
	}
	return &taskops.Command{Kind: e.Command, NextStage: e.NextStage, RecoveryStage: e.RecoveryStage, ProjectStages: true,
		PlanState: &taskops.PlanStateMove{To: e.To, Cause: e.Cause}}
}

// MoveEvent is the task.plan_state_changed event of a move of the state
// alone; a lifecycle command appends its own.
func (e PlanStateEffects) MoveEvent(taskID string) *core.Event {
	return PlanStateMoveEvent(taskID, e.From, e.To, e.Cause)
}

func planActRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrSetupChangeConflict}, args...)...)
}

// atPlanApprovalGate reports a task waiting at the plan_approval gate.
func atPlanApprovalGate(task core.Task) bool {
	return task.State == core.TaskAwaiting && task.GateKind == core.GateKindPlanApproval
}

// claimedImplementOrder returns the ID of a claimed implement order. A read
// projection of a claim whose expiry the order clock has not persisted still
// holds the stage, so the plan_acts read counts it as the acts do
// (component-plan-state invariant 6).
func claimedImplementOrder(orders []core.WorkOrder) string {
	for _, order := range orders {
		if order.Stage == core.StageImplement && (order.State == core.WorkOrderClaimed || order.ClaimExpiryUnpersisted) {
			return order.ID
		}
	}
	return ""
}

// openStageOrderIDs lists the task's orders of stage that a plan act cancels:
// queued, stale, or timed out, and claimed when claimed is true.
func openStageOrderIDs(orders []core.WorkOrder, stage core.Stage, claimed bool) []string {
	ids := []string{}
	for _, order := range orders {
		if order.Stage != stage {
			continue
		}
		switch order.State {
		case core.WorkOrderQueued, core.WorkOrderStale, core.WorkOrderTimedOut:
			ids = append(ids, order.ID)
		case core.WorkOrderClaimed:
			if claimed {
				ids = append(ids, order.ID)
			}
		}
	}
	return ids
}

// PlanPlanStateChange applies component-plan-state invariant 4 to a
// /policy plan_state change. It returns the effects table's row for where
// the task stands, or an ErrSetupChangeConflict refusal, which records
// nothing. The shared refusals of component-task-lifecycle invariant 45
// apply, except that the change is accepted at the plan_approval gate and
// while a plan order is claimed; a claimed implement order still refuses it
// (req-task-plan-state AC-3.1; DEC-122(7)).
func PlanPlanStateChange(task core.Task, orders []core.WorkOrder, to core.PlanState) (PlanStateEffects, error) {
	effects := PlanStateEffects{From: task.PlanState, To: to, Cause: core.PlanCauseOperator, CancelWorkOrderIDs: []string{}}
	if to != core.PlanNeeded && to != core.PlanNotRequired {
		return effects, Refusef("plan_state must be %s or %s", core.PlanNeeded, core.PlanNotRequired)
	}
	switch {
	case core.TaskTerminal(task.State):
		return effects, planActRefusal("terminal task %s cannot change its plan state", task.ID)
	case task.State == core.TaskApproved:
		return effects, planActRefusal("approved task %s cannot change its plan state", task.ID)
	case task.State == core.TaskAwaiting && task.NextStage == "" && !atPlanApprovalGate(task):
		return effects, planActRefusal("task %s waits at the %s gate with no next stage", task.ID, task.GateKind)
	}
	if id := claimedImplementOrder(orders); id != "" {
		return effects, planActRefusal("task %s has a claimed implement order %s", task.ID, id)
	}
	for _, order := range orders {
		if order.Stage == core.StageReview && order.State == core.WorkOrderSubmitted {
			return effects, planActRefusal("task %s has an in-flight review verdict", task.ID)
		}
	}
	if task.PlanState == to {
		return effects, planActRefusal("task %s plan_state is already %s", task.ID, to)
	}
	reroute := func(next core.Stage) (PlanStateEffects, error) {
		if _, err := core.TransitionPlanReroute(task.State, task.GateKind); err != nil {
			return effects, planActRefusal("task %s cannot be rerouted from %s: %v", task.ID, task.State, err)
		}
		effects.Command, effects.NextStage = core.TaskPlanReroute, next
		return effects, nil
	}
	switch {
	case task.State == core.TaskParked || task.NextStage == core.StageTriage:
		return effects, nil
	case atPlanApprovalGate(task):
		// The plan version stays unapproved; a move to not_required routes to
		// implement without approving it.
		if to == core.PlanNeeded {
			return reroute(core.StageSpec)
		}
		return reroute(core.StageImplement)
	case task.NextStage == core.StageSpec:
		if to == core.PlanNeeded {
			return effects, nil
		}
		effects.CancelWorkOrderIDs = openStageOrderIDs(orders, core.StageSpec, true)
		return reroute(core.StageImplement)
	case task.NextStage == core.StageImplement:
		if to == core.PlanNotRequired {
			return effects, nil
		}
		effects.CancelWorkOrderIDs = openStageOrderIDs(orders, core.StageImplement, false)
		return reroute(core.StageSpec)
	case task.NextStage == core.StageVerify || task.NextStage == core.StageReview:
		if to == core.PlanNeeded {
			return effects, planActRefusal("task %s cannot need a plan while %s is next", task.ID, task.NextStage)
		}
		return effects, nil
	}
	return effects, nil
}

// PlanAttachPlan applies component-plan-state invariant 5's predicate and
// effects. The act is accepted only while the plan state is not_required or
// needed, the next stage is triage, spec, or implement, the task is not
// terminal, and no implement order is claimed. After triage it enters the
// plan_approval gate with gate.spec, so the task must stand where that
// command applies; before triage finishes, triage's proceed enters the gate
// (req-task-plan-state AC-3.2; DEC-122(6)).
func PlanAttachPlan(task core.Task, orders []core.WorkOrder) (PlanStateEffects, error) {
	effects := PlanStateEffects{From: task.PlanState, To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanAttached, CancelWorkOrderIDs: []string{}}
	if core.TaskTerminal(task.State) {
		return effects, planActRefusal("terminal task %s cannot take a plan", task.ID)
	}
	if task.PlanState != core.PlanNotRequired && task.PlanState != core.PlanNeeded {
		return effects, planActRefusal("task %s plan_state %s cannot take a plan; it must be %s or %s", task.ID, task.PlanState, core.PlanNotRequired, core.PlanNeeded)
	}
	if task.NextStage != core.StageTriage && task.NextStage != core.StageSpec && task.NextStage != core.StageImplement {
		return effects, planActRefusal("task %s next stage %q cannot take a plan; it must be triage, spec, or implement", task.ID, task.NextStage)
	}
	if id := claimedImplementOrder(orders); id != "" {
		return effects, planActRefusal("task %s has a claimed implement order %s", task.ID, id)
	}
	effects.CancelWorkOrderIDs = append(openStageOrderIDs(orders, core.StageImplement, false), openStageOrderIDs(orders, core.StageSpec, true)...)
	if task.NextStage == core.StageTriage {
		return effects, nil
	}
	if _, err := core.TransitionTask(task.State, core.TaskGateSpec); err != nil {
		return effects, planActRefusal("task %s cannot enter the plan approval gate from %s: %v", task.ID, task.State, err)
	}
	effects.Command, effects.RecoveryStage = core.TaskGateSpec, core.StageImplement
	return effects, nil
}

// PlanStateCancelPayload is the work_order.cancelled payload of an order a
// plan act cancels: the terminal sweep's fields with reason
// plan_state_changed and the act that cancelled it (component-plan-state
// invariant 4; component-task-lifecycle invariant 32).
func PlanStateCancelPayload(id, attemptID, sessionID string, from core.WorkOrderState, act string) json.RawMessage {
	return core.JSONPayload(map[string]any{
		"id": id, "attempt_id": attemptID, "session_id": sessionID, "state": core.WorkOrderCancelled,
		"from": from, "command": core.WorkOrderCmdCancel, "reason": PlanStateChangedSuppressionReason, "act": act,
	})
}

// Plan act names recorded on cancelled orders.
const (
	PlanActSetPlanState = "plan_state"
	PlanActAttachPlan   = "attach_plan"
)

// PlanStateSetupChangedPayload is the task.setup.changed payload of a
// plan_state change. The frozen setup contract is unchanged; the payload
// names the plan-state move and the orders the change cancelled.
func PlanStateSetupChangedPayload(workspace string, actor Actor, task core.Task, request SetupChangeRequest, effects PlanStateEffects) map[string]any {
	return map[string]any{
		"workspace_id": workspace, "task_id": task.ID, "actor": actor.ID,
		"request_id": request.RequestID, "reason": request.Reason,
		"previous_setup": task.SetupContract, "new_setup": task.SetupContract,
		"lifecycle_boundary": "future_work", "stage": task.NextStage,
		"plan_state": map[string]any{"from": effects.From, "to": effects.To, "command": effects.Command,
			"cancelled_work_order_ids": effects.CancelWorkOrderIDs},
		"updated_work_order_ids": []string{},
		"review_transition": map[string]any{"kind": request.ReviewTransition, "prior_round": 0, "resulting_round": 0,
			"retained_work_order_ids": []string{}, "superseded_work_order_ids": []string{}, "created_work_order_ids": []string{}},
	}
}

// AttachPlanRequest is one operator attach-plan act (component-plan-state
// invariant 5). The route has applied the intake plan rules; the backend
// checks the predicate and records the version in its task transaction.
type AttachPlanRequest struct {
	TaskID     string
	RequestID  string
	Reason     string
	Plan       string
	BaseCommit string
	// Actor is the authenticated actor, set by the backend from the context.
	Actor Actor
}

// AttachPlanResult is the persisted replay result of an attach.
type AttachPlanResult struct {
	RequestID           string    `json:"request_id"`
	Task                core.Task `json:"task"`
	PlanVersion         int       `json:"plan_version"`
	CancelledWorkOrders []string  `json:"cancelled_work_orders"`
}

// PrepareAttachPlanRequest normalizes an attach request and refuses a blank
// identity, an empty plan, or a base commit that is not a full commit ID.
func PrepareAttachPlanRequest(request AttachPlanRequest) (AttachPlanRequest, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.Plan = strings.TrimSpace(request.Plan)
	request.BaseCommit = strings.TrimSpace(request.BaseCommit)
	if request.TaskID == "" || request.RequestID == "" || request.Reason == "" {
		return request, Refusef("task, request_id, and nonblank reason are required")
	}
	if request.Plan == "" {
		return request, Refusef("plan is required")
	}
	if request.BaseCommit != "" && !core.ValidPlanBaseCommit(request.BaseCommit) {
		return request, Refusef("base_commit must be a full commit ID of 40 lowercase hex characters")
	}
	return request, nil
}

// AttachPlanIdentity is the replay identity an attach persists in
// task_setup_changes beside /policy replays. Its act field keeps a /policy
// request ID from replaying as an attach and the reverse.
func AttachPlanIdentity(request AttachPlanRequest) any {
	return struct {
		Act        string `json:"act"`
		TaskID     string `json:"task_id"`
		RequestID  string `json:"request_id"`
		Reason     string `json:"reason"`
		Plan       string `json:"plan"`
		BaseCommit string `json:"base_commit"`
		Actor      Actor  `json:"actor"`
	}{PlanActAttachPlan, request.TaskID, request.RequestID, request.Reason, request.Plan, request.BaseCommit, request.Actor}
}

// AttachedPlanSpecVersion is the plan version an attach records: the next
// version, origin operator, and the stated base commit, unapproved
// (component-plan-state invariant 5).
func AttachedPlanSpecVersion(request AttachPlanRequest, version int, at time.Time) core.SpecVersion {
	spec := core.SpecVersion{
		TaskID: request.TaskID, Version: version, Content: request.Plan,
		Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`),
		Origin: core.PlanOriginOperator, CreatedAt: at,
	}
	if request.BaseCommit != "" {
		commit := request.BaseCommit
		spec.BaseCommit = &commit
	}
	return spec
}

// AttachedPlanVersionCreatedEvent is the spec.version_created event of an
// attached plan, naming the operator's reason and request ID.
func AttachedPlanVersionCreatedEvent(spec core.SpecVersion, request AttachPlanRequest) core.Event {
	return core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", At: spec.CreatedAt,
		ActorID: request.Actor.ID, ActorRole: request.Actor.Role,
		Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount, "origin": spec.Origin,
			"reason": request.Reason, "request_id": request.RequestID})}
}

// ChangeTaskPlanState is the /policy plan_state branch of
// ChangeTaskPolicyCommand on the memory backend. The caller holds m.mu, has
// resolved replay, and has checked the task exists in the workspace.
func (m *memory) changeTaskPlanStateLocked(ctx context.Context, workspace string, task core.Task, request SetupChangeRequest) (SetupChangeResult, error) {
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID == task.ID {
			orders = append(orders, order)
		}
	}
	request, err := PlanTaskPolicyChange(task, orders, request, PolicyHandoffSeatInputs{})
	if err != nil {
		return SetupChangeResult{}, err
	}
	effects := *request.PlanStateEffects
	actor := ActorFromContext(ctx)
	now := time.Now().UTC()
	cancelled := m.cancelPlanActOrdersLocked(ctx, task.ID, effects.CancelWorkOrderIDs, PlanActSetPlanState, actor, now)
	if task, err = m.applyPlanStateEffectsLocked(ctx, task, effects); err != nil {
		return SetupChangeResult{}, err
	}
	result := SetupChangeResult{RequestID: request.RequestID, Task: task, ReviewTransition: request.ReviewTransition,
		UpdatedWorkOrders: []string{}, CreatedWorkOrders: []string{}, RetainedWorkOrders: []string{}, SupersededWorkOrders: []string{},
		CancelledWorkOrders: cancelled}
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.setup.changed", ActorID: actor.ID, ActorRole: actor.Role, At: now,
		Payload: core.JSONPayload(PlanStateSetupChangedPayload(workspace, actor, task, request, effects))})
	m.setupChanges[request.RequestID] = memorySetupChange{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

// cancelPlanActOrdersLocked cancels the named orders as the terminal sweep
// does, with retry suppressed for reason plan_state_changed.
func (m *memory) cancelPlanActOrdersLocked(ctx context.Context, taskID string, ids []string, act string, actor Actor, now time.Time) []string {
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	cancelled := m.cancelWorkOrdersLocked(ctx, taskID, func(order core.WorkOrder) bool { return selected[order.ID] }, PlanStateChangedSuppressionReason,
		func(id, attemptID, sessionID string, from core.WorkOrderState) json.RawMessage {
			return PlanStateCancelPayload(id, attemptID, sessionID, from, act)
		}, actor.ID, actor.Role, now)
	if cancelled == nil {
		cancelled = []string{}
	}
	return cancelled
}

// applyPlanStateEffectsLocked issues the act's lifecycle command with its
// plan-state move, or moves the state alone and appends its event.
func (m *memory) applyPlanStateEffectsLocked(ctx context.Context, task core.Task, effects PlanStateEffects) (core.Task, error) {
	if command := effects.TaskCommand(); command != nil {
		return m.applyTaskCommandLocked(ctx, task.ID, *command)
	}
	if move := effects.MoveEvent(task.ID); move != nil {
		task.PlanState = effects.To
		m.tasks[task.ID] = task
		m.appendEventLocked(ctx, *move)
	}
	return m.tasks[task.ID], nil
}

// AttachTaskPlanCommand records an operator-attached plan in one memory
// transaction: replay, the predicate, order cancellation, the next plan
// version, and the move to awaiting_approval (component-plan-state
// invariant 5; req-task-plan-state AC-3.2).
func (m *memory) AttachTaskPlanCommand(ctx context.Context, lease taskops.TaskLease, raw AttachPlanRequest) (AttachPlanResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return AttachPlanResult{}, err
	}
	request, err := PrepareAttachPlanRequest(raw)
	request.Actor = ActorFromContext(ctx)
	if !lease.ValidForCommand(request.TaskID, taskops.SetupChangeCommand) {
		return AttachPlanResult{}, fmt.Errorf("taskops lease does not authorize attaching a plan to task %s", request.TaskID)
	}
	if err != nil {
		return AttachPlanResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, _ := WorkspaceFromContext(ctx)
	identity := core.JSONPayload(AttachPlanIdentity(request))
	if prior, ok := m.setupChanges[request.RequestID]; ok {
		if prior.Workspace != workspace || prior.Attach == nil || !reflect.DeepEqual(prior.Identity, identity) {
			return AttachPlanResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrSetupChangeConflict, request.RequestID)
		}
		return *prior.Attach, nil
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || task.Workspace != workspace {
		return AttachPlanResult{}, fmt.Errorf("task %s: %w", request.TaskID, ErrNotFound)
	}
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID == task.ID {
			orders = append(orders, order)
		}
	}
	effects, err := PlanAttachPlan(task, orders)
	if err != nil {
		return AttachPlanResult{}, err
	}
	now := time.Now().UTC()
	version := 1
	for _, spec := range m.specs[task.ID] {
		if spec.Version >= version {
			version = spec.Version + 1
		}
	}
	cancelled := m.cancelPlanActOrdersLocked(ctx, task.ID, effects.CancelWorkOrderIDs, PlanActAttachPlan, request.Actor, now)
	spec := AttachedPlanSpecVersion(request, version, now)
	if m.specs == nil {
		m.specs = map[string][]core.SpecVersion{}
	}
	m.specs[task.ID] = append(m.specs[task.ID], spec)
	m.appendEventLocked(ctx, AttachedPlanVersionCreatedEvent(spec, request))
	if task, err = m.applyPlanStateEffectsLocked(ctx, task, effects); err != nil {
		return AttachPlanResult{}, err
	}
	result := AttachPlanResult{RequestID: request.RequestID, Task: task, PlanVersion: version, CancelledWorkOrders: cancelled}
	m.setupChanges[request.RequestID] = memorySetupChange{Workspace: workspace, Identity: identity, Attach: &result}
	return result, nil
}

// ResolveTriageProceed replaces a triage proceed command's step with the one
// the task's current plan state selects. Each backend calls it on the plan
// state it read under the task-operation lock that the plan acts commit
// under, so no act can commit between the read and the transition
// (component-plan-state invariant 5; component-triage invariant 29;
// req-task-plan-state AC-1.1, AC-3.2). Any other command passes unchanged.
func ResolveTriageProceed(command taskops.Command, current core.PlanState) (taskops.Command, error) {
	return PrepareTaskCommand(command, "", "", current)
}

// PrepareTaskCommand applies the plan-state preconditions of a lifecycle
// command to the task row the backend read under the task-operation lock:
// a triage proceed takes the step the plan state selects, and plan.reroute
// leaves awaiting_human only at the plan_approval gate
// (component-task-lifecycle invariant 2; component-plan-state invariant 5).
// An empty state skips the gate check.
func PrepareTaskCommand(command taskops.Command, state core.TaskState, gate core.GateKind, current core.PlanState) (taskops.Command, error) {
	if command.Kind == core.TaskPlanReroute && state != "" {
		if _, err := core.TransitionPlanReroute(state, gate); err != nil {
			return command, err
		}
	}
	return resolveTriageProceed(command, current)
}

func resolveTriageProceed(command taskops.Command, current core.PlanState) (taskops.Command, error) {
	if !command.TriageProceed {
		return command, nil
	}
	kind, next, recovery, err := core.TriageProceedStep(current)
	if err != nil {
		return command, err
	}
	command.Kind, command.NextStage, command.RecoveryStage, command.ProjectStages = kind, next, recovery, true
	return command, nil
}
