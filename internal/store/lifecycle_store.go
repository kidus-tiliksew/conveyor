package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// PlanRevisionRequestResult is the atomic projection committed when an
// implementer contests its approved execution plan (REQ-1, AC-1.1–AC-1.3).
type PlanRevisionRequestResult struct {
	WorkOrder   core.WorkOrder `json:"work_order"`
	Task        core.Task      `json:"task"`
	PlanVersion int            `json:"plan_version"`
	Rationale   string         `json:"rationale"`
}

func (m *memory) RequestPlanRevisionCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID string, claim core.WorkOrderClaimIdentity, rationale string) (PlanRevisionRequestResult, error) {
	// A worker claim records its own worker actor; any other claim records the
	// caller's actor. Either must be complete before the order, job, session,
	// or task changes (component-persistence, Actor context).
	if _, err := RequireActor(workerClaimActorContext(ctx, claim.WorkerID)); err != nil {
		return PlanRevisionRequestResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return PlanRevisionRequestResult{}, fmt.Errorf("rationale is required")
	}
	order, ok := m.workOrders[workOrderID]
	if !ok || !taskLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdRequestPlanRevision)) {
		return PlanRevisionRequestResult{}, ErrWorkOrderClaimLost
	}
	now := time.Now().UTC()
	if order.SessionID == "" || order.SessionID != claim.SessionID || order.State != core.WorkOrderClaimed ||
		!order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return PlanRevisionRequestResult{}, ErrWorkOrderClaimLost
	}
	if order.WorkerID != claim.WorkerID || order.ClaimantID != claim.ClaimantID {
		return PlanRevisionRequestResult{}, ErrWorkOrderClaimUnauthorized
	}
	if order.Stage != core.StageImplement {
		return PlanRevisionRequestResult{}, fmt.Errorf("request_plan_revision requires an implement-stage work order")
	}
	task, ok := m.tasks[order.TaskID]
	if !ok {
		return PlanRevisionRequestResult{}, fmt.Errorf("task %s not found", order.TaskID)
	}
	plan, ok := m.approvedExecutionDocumentLocked(task)
	if !ok {
		return PlanRevisionRequestResult{}, fmt.Errorf("request_plan_revision requires an approved execution plan")
	}
	nextOrder, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRequestPlanRevision)
	if err != nil {
		return PlanRevisionRequestResult{}, err
	}
	nextTask, err := core.TransitionTask(task.State, core.TaskGatePlanRevision)
	if err != nil {
		return PlanRevisionRequestResult{}, err
	}
	queueTimeout := order.QueueDeadline.Sub(order.QueueEnteredAt)
	if queueTimeout <= 0 {
		queueTimeout = config.DefaultWorkOrderQueueTimeout
	}
	attemptID := order.AttemptID
	order.LastAttemptID = attemptID
	clearActiveAttempt(&order)
	order.ClearExecutionPins()
	order.OperatorDirection = ""
	order.State = nextOrder
	order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
	order.LastFailureCategory = ""
	order.LastFailureMessage = core.WorkOrderReleaseReasonPlanRevisionRequested
	order.LastFailureDetail = ""
	order.LastFailureExitStatus = nil
	order.LastFailureAt = now
	order.NextRetryAt = time.Time{}
	order.RetrySuppressed = true
	order.RetrySuppressionReason = ""
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.UpdatedAt = now
	order.Claimable = false
	m.workOrders[workOrderID] = order
	for taskID, jobs := range m.jobs {
		for i := range jobs {
			if jobs[i].ID == order.JobID {
				jobs[i].State = core.JobPending
				jobs[i].StartedAt = time.Time{}
				jobs[i].EndedAt = time.Time{}
				m.jobs[taskID] = jobs
			}
		}
	}
	task.State = nextTask
	m.tasks[task.ID] = task
	actor := workerClaimActorContext(ctx, claim.WorkerID)
	m.appendEventLocked(actor, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "work_order.plan_revision_requested", Payload: core.JSONPayload(map[string]any{"work_order_id": order.ID, "attempt_id": attemptID, "session_id": claim.SessionID, "rationale": rationale, "plan_version": plan.Version}), At: now})
	m.appendEventLocked(actor, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]any{"attempt_id": attemptID, "session_id": claim.SessionID, "reason": core.WorkOrderReleaseReasonPlanRevisionRequested, "release_cause": core.WorkOrderReleaseCauseOperatorAction, "outcome": core.WorkOrderOutcomeReleased, "automatic_retry_count": order.AutomaticRetryCount, "retry_suppressed": true}), At: now})
	m.appendEventLocked(actor, core.Event{TaskID: task.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": core.TaskRunning, "to": nextTask, "command": core.TaskGatePlanRevision}), At: now})
	return PlanRevisionRequestResult{WorkOrder: order, Task: task, PlanVersion: plan.Version, Rationale: rationale}, nil
}

func (m *memory) CancelPlanRevisionWorkOrderCommand(ctx context.Context, taskLease taskops.TaskLease, workOrderID, attemptID string) (core.WorkOrder, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[workOrderID]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", workOrderID)
	}
	if !taskLease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdCancel)) {
		return core.WorkOrder{}, fmt.Errorf("plan-revision cancellation requires a valid taskops lease")
	}
	if order.Stage != core.StageImplement || order.LastAttemptID != attemptID || order.LastFailureMessage != core.WorkOrderReleaseReasonPlanRevisionRequested {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not the contested plan-revision attempt", workOrderID)
	}
	if order.State == core.WorkOrderCancelled {
		return order, nil
	}
	next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdCancel)
	if err != nil {
		return core.WorkOrder{}, err
	}
	now := time.Now().UTC()
	priorState := order.State
	order.State, order.Claimable, order.UpdatedAt = next, false, now
	m.workOrders[workOrderID] = order
	for taskID, jobs := range m.jobs {
		for i := range jobs {
			if jobs[i].ID != order.JobID || jobs[i].State == core.JobDone {
				continue
			}
			jobs[i].State, jobs[i].EndedAt = core.JobFailed, now
			m.jobs[taskID] = jobs
		}
	}
	actor := ActorFromContext(ctx)
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.cancelled", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID, "attempt_id": attemptID, "prior_state": priorState, "state": next,
		"command": core.WorkOrderCmdCancel, "reason": "plan revision approved",
	}), At: now})
	return order, nil
}

func (m *memory) retireWorkOrderSiblingsLocked(ctx context.Context, authoritative core.WorkOrder, reason string, now time.Time, olderOnly bool) {
	// Review seats are intentionally parallel siblings, not superseded retries.
	if authoritative.Stage == core.StageReview {
		return
	}
	for id, sibling := range m.workOrders {
		if id == authoritative.ID || sibling.TaskID != authoritative.TaskID || sibling.Stage != authoritative.Stage ||
			(sibling.State != core.WorkOrderQueued && sibling.State != core.WorkOrderStale) {
			continue
		}
		if olderOnly && !(sibling.CreatedAt.Before(authoritative.CreatedAt) || sibling.CreatedAt.Equal(authoritative.CreatedAt) && sibling.ID < authoritative.ID) {
			continue
		}
		priorState := sibling.State
		sibling.State, sibling.Claimable = core.WorkOrderCancelled, false
		sibling.LastAttemptOutcome = core.WorkOrderOutcomeCancelled
		sibling.RetrySuppressed, sibling.RetrySuppressionReason = true, "superseded"
		sibling.NextRetryAt, sibling.LeaseExpiresAt = time.Time{}, time.Time{}
		sibling.UpdatedAt = now
		m.workOrders[id] = sibling
		if job, index, found := m.findJobLocked(sibling.JobID); found {
			job.State, job.EndedAt = core.JobFailed, now
			m.jobs[job.TaskID][index] = job
		}
		m.appendEventLocked(ctx, core.Event{TaskID: sibling.TaskID, JobID: sibling.JobID, Kind: "work_order.retired", Payload: core.JSONPayload(map[string]any{
			"work_order_id": sibling.ID, "authoritative_work_order_id": authoritative.ID, "stage": sibling.Stage,
			"prior_state": priorState, "new_state": sibling.State, "reason": reason,
		}), At: now})
	}
}

// WorkOrderRecoverySupersessionError rejects recovery once another order or
// task progress has made the target's stage historical. Callers hold the
// task-operation serialization boundary while evaluating it.
func WorkOrderRecoverySupersessionError(task core.Task, order core.WorkOrder, taskOrders []core.WorkOrder) error {
	laterThanTarget := func(candidate core.WorkOrder) bool {
		return candidate.CreatedAt.After(order.CreatedAt) || candidate.CreatedAt.Equal(order.CreatedAt) && candidate.ID > order.ID
	}
	for _, candidate := range taskOrders {
		if candidate.ID == order.ID || candidate.Stage != order.Stage || candidate.State == core.WorkOrderCancelled {
			continue
		}
		if laterThanTarget(candidate) {
			return fmt.Errorf("work order %s was superseded by same-stage successor %s", order.ID, candidate.ID)
		}
	}
	pastStage := func(stage core.Stage) bool {
		// A bounce makes the target stage current again. Historical orders from
		// later stages cannot contradict the task-stage projection
		// (component-persistence).
		if task.NextStage == stage {
			return false
		}
		for _, candidate := range taskOrders {
			if candidate.State == core.WorkOrderCancelled || !laterThanTarget(candidate) {
				continue
			}
			if stage == core.StageSpec && (candidate.Stage == core.StageImplement || candidate.Stage == core.StageVerify || candidate.Stage == core.StageReview) {
				return true
			}
			if (stage == core.StageImplement || stage == core.StageVerify) && candidate.Stage == core.StageReview {
				return true
			}
		}
		return false
	}
	if task.State == core.TaskApproved || task.State == core.TaskMerged || task.State == core.TaskClosed {
		gate := "terminal task state"
		if order.Stage == core.StageReview {
			gate = "merge gate"
		}
		return fmt.Errorf("work order %s cannot be recovered after its task reached the %s", order.ID, gate)
	}
	switch order.Stage {
	case core.StageSpec:
		if pastStage(core.StageSpec) || task.NextStage == core.StageImplement || task.NextStage == core.StageVerify || task.NextStage == core.StageReview ||
			task.RecoveryStage == core.StageImplement || task.RecoveryStage == core.StageVerify || task.RecoveryStage == core.StageReview {
			return fmt.Errorf("work order %s cannot be recovered because task %s has passed the plan gate", order.ID, task.ID)
		}
	case core.StageVerify:
		if !task.SetupContract.VerifyStage || order.HeadSHA != core.VerifyStageHead(task) || pastStage(core.StageVerify) || task.NextStage == core.StageReview || task.RecoveryStage == core.StageReview {
			return fmt.Errorf("verify work order %s is superseded", order.ID)
		}
	case core.StageImplement:
		if pastStage(core.StageImplement) || task.NextStage == core.StageVerify || task.NextStage == core.StageReview || task.RecoveryStage == core.StageVerify || task.RecoveryStage == core.StageReview {
			return fmt.Errorf("work order %s cannot be recovered because task %s has advanced to review", order.ID, task.ID)
		}
	}
	return nil
}

// ValidateJobTransition keeps whole-record metadata updates compatible while
// rejecting every state change absent from the canonical job machine.
func ValidateJobTransition(from, to core.JobState) error {
	if from == to {
		return nil
	}
	command := ""
	switch to {
	case core.JobRunning:
		command = "job.start"
	case core.JobDone:
		command = "job.complete"
	case core.JobFailed:
		command = "job.fail"
	case core.JobPending:
		command = "job.retry"
	default:
		command = "job.invalid"
	}
	expected, err := core.TransitionJob(from, command)
	if err != nil {
		return err
	}
	if expected != to {
		return fmt.Errorf("job command %q resolves to %q, not %q", command, expected, to)
	}
	return nil
}

func (m *memory) recordDependencyOutcomeLocked(ctx context.Context, dependencyID string, state core.TaskState, at time.Time) {
	if state == core.TaskMerged || !core.TaskTerminal(state) {
		for dependentID, dependencies := range m.dependencies {
			if _, exists := dependencies[dependencyID]; exists {
				m.resumeDependencyQueueClocksLocked(dependentID, at)
			}
		}
		return
	}
	for dependentID, dependencies := range m.dependencies {
		if _, exists := dependencies[dependencyID]; !exists {
			continue
		}
		duplicate := false
		for _, event := range m.events[dependentID] {
			if event.Kind != "task.dependency_unsatisfiable" {
				continue
			}
			var payload struct {
				DependsOnTaskID string         `json:"depends_on_task_id"`
				DependencyState core.TaskState `json:"dependency_state"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil &&
				payload.DependsOnTaskID == dependencyID && payload.DependencyState == state {
				duplicate = true
				break
			}
		}
		if !duplicate {
			m.appendEventLocked(ctx, core.Event{
				TaskID: dependentID, Kind: "task.dependency_unsatisfiable", At: at,
				Payload: core.JSONPayload(map[string]any{
					"task_id": dependentID, "depends_on_task_id": dependencyID, "dependency_state": state,
				}),
			})
		}
	}
}

func (m *memory) resumeDependencyQueueClocksLocked(taskID string, now time.Time) {
	for dependencyID := range m.dependencies[taskID] {
		if dependency, exists := m.tasks[dependencyID]; exists && dependency.State != core.TaskMerged {
			return
		}
	}
	for id, order := range m.workOrders {
		if order.TaskID != taskID || (order.Stage != core.StageImplement && order.Stage != core.StageVerify) ||
			order.State != core.WorkOrderQueued || order.QueueBlockedAt.IsZero() {
			continue
		}
		order.QueueDeadline = order.QueueDeadline.Add(now.Sub(order.QueueBlockedAt))
		order.QueueBlockedAt = time.Time{}
		order.Claimable = order.ClaimableAt(now)
		order.UpdatedAt = now
		m.workOrders[id] = order
	}
}

func (m *memory) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	if command.Kind == core.TaskStartOver {
		return core.Task{}, fmt.Errorf("task start over requires StartOverTaskCommand")
	}
	if !lease.ValidFor(id) {
		return core.Task{}, fmt.Errorf("task lifecycle mutation requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	fromState, fromStage := task.State, task.NextStage
	state, err := core.TransitionTask(fromState, command.Kind)
	if err != nil {
		return core.Task{}, err
	}
	if !m.verifyReviewReadyLocked(task) {
		if state == core.TaskApproved || state == core.TaskMerged {
			return core.Task{}, fmt.Errorf("verification is required before approval or merge")
		}
		if command.NextStage == core.StageReview {
			command.NextStage = core.StageVerify
		}
		if command.RecoveryStage == core.StageReview {
			command.RecoveryStage = core.StageVerify
		}
	}
	task.State = state
	if command.ProjectStages {
		task.NextStage = command.NextStage
		task.RecoveryStage = command.RecoveryStage
	}
	m.tasks[id] = task
	if core.TaskTerminal(state) {
		m.deleteProposedTaskContextLocked(task.ID)
	}
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command.Kind})})
	if command.FailureMessage != "" {
		m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "dispatch.failed", Payload: core.JSONPayload(map[string]any{
			"attempt": command.Attempt, "max_attempts": command.MaxAttempts, "error": command.FailureMessage,
		})})
	}
	m.recordDependencyOutcomeLocked(ctx, id, state, time.Now().UTC())
	if command.ProjectStages {
		m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": command.NextStage, "recovery_stage": command.RecoveryStage, "state": state})})
	}
	if task.ParentTaskID != "" && core.TaskTerminal(state) {
		m.closeBlueprintParentLocked(ctx, task.ParentTaskID)
	}
	if command.Kind == core.TaskRecover && m.closeBlueprintParentLocked(ctx, task.ID) {
		task = m.tasks[task.ID]
	}
	return task, nil
}

func (m *memory) closeBlueprintParentLocked(ctx context.Context, parentID string) bool {
	parent, ok := m.tasks[parentID]
	if !ok {
		return false
	}
	from := parent.State
	closed, err := core.TransitionTask(from, core.TaskBlueprintClose)
	if err != nil {
		return false
	}
	childCount := 0
	for _, child := range m.tasks {
		if child.ParentTaskID != parentID {
			continue
		}
		childCount++
		if !core.TaskTerminal(child.State) {
			return false
		}
	}
	if childCount == 0 {
		return false
	}
	parent.State = closed
	m.tasks[parent.ID] = parent
	m.deleteProposedTaskContextLocked(parent.ID)
	m.appendEventLocked(ctx, core.Event{TaskID: parent.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": closed, "command": core.TaskBlueprintClose})})
	m.appendEventLocked(ctx, core.Event{TaskID: parent.ID, Kind: "blueprint.closed", Payload: core.JSONPayload(map[string]any{"children": childCount, "terminal_states": []core.TaskState{core.TaskMerged, core.TaskClosed}})})
	m.recordDependencyOutcomeLocked(ctx, parent.ID, closed, time.Now().UTC())
	return true
}
