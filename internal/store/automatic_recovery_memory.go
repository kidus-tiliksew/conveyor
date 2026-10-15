package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) recoveryPolicyLocked(workspace string) (config.RecoveryPolicy, time.Duration) {
	if m.readClockPolicy != nil {
		policy, timeout, err := m.readClockPolicy(workspace)
		if err == nil && timeout > 0 {
			return policy, timeout
		}
	}
	return config.DefaultRecoveryPolicy(), config.DefaultWorkOrderQueueTimeout
}

func (m *memory) automaticRecoveryCandidateLocked(order core.WorkOrder, now time.Time) bool {
	task := m.tasks[order.TaskID]
	policy, _ := m.recoveryPolicyLocked(task.Workspace)
	if policy.AutomaticRecoveries == 0 {
		return false
	}
	if task.State != core.TaskQueued && task.State != core.TaskRunning || core.TaskTerminal(task.State) {
		return false
	}
	if order.RetrySuppressionReason != "" && order.RetrySuppressionReason != core.AttemptRetriesExhaustedReason {
		return false
	}
	if order.Checkpoint != nil {
		return false
	}
	if !RecoverableStop(order, now) {
		return false
	}
	return order.NextAutomaticRecoveryAt.IsZero() || !order.NextAutomaticRecoveryAt.After(now)
}

func (m *memory) applyAutomaticRecoveryLocked(ctx context.Context, taskID string, now time.Time) (int, error) {
	task, ok := m.tasks[taskID]
	if !ok {
		return 0, nil
	}
	policy, queueTimeout := m.recoveryPolicyLocked(task.Workspace)
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID == taskID {
			orders = append(orders, order)
		}
	}
	sort.Slice(orders, func(i, j int) bool {
		if !orders[i].QueueEnteredAt.Equal(orders[j].QueueEnteredAt) {
			return orders[i].QueueEnteredAt.Before(orders[j].QueueEnteredAt)
		}
		return orders[i].ID < orders[j].ID
	})
	acted := 0
	for _, order := range orders {
		current, exists := m.workOrders[order.ID]
		if !exists {
			continue
		}
		current.RecoveryRefusal = ""
		if err := WorkOrderRecoveryRefusal(task, current, orders); err != nil {
			current.RecoveryRefusal = err.Error()
		}
		if AnsweredAuthorityCandidate(task, current) {
			recovered, err := m.recoverAnsweredAuthorityLocked(ctx, task, current, queueTimeout, now)
			if err != nil {
				return acted, err
			}
			if recovered {
				acted++
			}
			continue
		}
		decision := DecideAutomaticRecovery(policy, task, current, now)
		switch decision.Kind {
		case AutomaticRecoverySchedule:
			ScheduleAutomaticRecovery(&current, decision)
			current.UpdatedAt = now
			m.workOrders[current.ID] = current
		case AutomaticRecoveryExhausted:
			current.RetrySuppressed = true
			current.RetrySuppressionReason = core.AutomaticRecoveryExhaustedReason
			current.UpdatedAt = now
			m.workOrders[current.ID] = current
			eventCtx := WithActor(ctx, SystemActor("queue:"+current.JobID))
			m.appendEventLocked(eventCtx, core.Event{TaskID: current.TaskID, JobID: current.JobID, Kind: "work_order.recovery_exhausted", Payload: core.JSONPayload(map[string]any{
				"work_order_id": current.ID, "recovery_count": decision.Number, "window_started_at": decision.WindowStartedAt, "reason": core.AutomaticRecoveryExhaustedReason, "last_failure_message": current.LastFailureMessage,
			}), At: now})
		case AutomaticRecoveryRecover, AutomaticRecoveryRedispatch:
			requestID := AutomaticRecoveryRequestID(current.ID, current.RedispatchCount)
			var err error
			if decision.Kind == AutomaticRecoveryRedispatch {
				current, err = m.redispatchOrderLocked(ctx, current, queueTimeout, now, true, policy, requestID)
			} else {
				current, err = m.recoverOrderLocked(ctx, current, requestID, "", queueTimeout, now, nil, recoveryMechanical, policy, nil)
			}
			if err != nil {
				return acted, err
			}
			if current.State == core.WorkOrderQueued {
				acted++
			}
		}
	}
	return acted, nil
}

// recoveryMode selects the ledger and audit of one recovery transition: an
// operator recovery resets the ledger, a mechanical recovery stamps it (AR-3),
// and an authority-confirmed recovery leaves it unchanged and clears the
// answered checkpoint (feature-bounded-automatic-recovery AR-3a).
type recoveryMode int

const (
	recoveryOperator recoveryMode = iota
	recoveryMechanical
	recoveryAuthorityConfirmed
)

// recoverAnsweredAuthorityLocked reads the cited documents under the mutex,
// decides, and recovers the order once for its release (DEC-73).
func (m *memory) recoverAnsweredAuthorityLocked(ctx context.Context, task core.Task, order core.WorkOrder, queueTimeout time.Duration, now time.Time) (bool, error) {
	workspace := workspaceOrDefault(ctx, "")
	documents := m.authorityDocumentsLocked(workspace, AnsweredAuthorityDocumentIDs(order))
	decision := DecideAnsweredAuthorityRecovery(task, order, documents)
	if !decision.Recover {
		return false, nil
	}
	var releaseEventID int64
	for _, event := range m.events[order.TaskID] {
		if event.Kind == "work_order.released" && ReleaseEventAttemptID(event) == order.LastAttemptID {
			releaseEventID = event.ID
		}
	}
	if releaseEventID == 0 {
		return false, nil
	}
	payload := AuthorityConfirmedEventPayload(workspace, order, decision, releaseEventID)
	recovered, err := m.recoverOrderLocked(ctx, order, decision.RequestID, decision.Direction, queueTimeout, now, nil, recoveryAuthorityConfirmed, config.RecoveryPolicy{}, payload)
	if err != nil {
		return false, err
	}
	return recovered.Checkpoint == nil && recovered.LastAttemptOutcome == "", nil
}

// authorityDocumentsLocked snapshots each cited document: a requirement first,
// then a System Design, as the checkpoint card resolves it. Pending versions
// follow listPendingProposalsLocked for every origin.
func (m *memory) authorityDocumentsLocked(workspace string, ids []string) map[string]AuthorityDocument {
	out := make(map[string]AuthorityDocument, len(ids))
	for _, id := range ids {
		key := memoryScopedKey{workspace: workspace, id: id}
		if requirement, ok := m.requirements[key]; ok {
			document := AuthorityDocument{ID: id, Kind: "requirement", Found: !requirement.Archived, CurrentVersion: requirement.CurrentVersion}
			for _, version := range m.requirementVersions[key] {
				if !version.Confirmed && !version.Retired && version.Version > requirement.CurrentVersion {
					document.PendingVersions++
				}
			}
			out[id] = document
			continue
		}
		if design, ok := m.systemDesigns[key]; ok {
			document := AuthorityDocument{ID: id, Kind: "system_design", Found: !design.Archived, CurrentVersion: design.CurrentVersion}
			for _, version := range m.systemDesignVersions[key] {
				if !version.Confirmed && !version.Dismissed {
					document.PendingVersions++
				}
			}
			out[id] = document
		}
	}
	return out
}

func (m *memory) recoverOrderLocked(ctx context.Context, order core.WorkOrder, requestID, direction string, queueTimeout time.Duration, now time.Time, refreeze *RecoveryRefreeze, mode recoveryMode, policy config.RecoveryPolicy, authorityPayload map[string]any) (core.WorkOrder, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	key := workspace + "/" + order.ID + "/" + requestID
	if _, exists := m.recoveries[key]; exists {
		return m.workOrders[order.ID], nil
	}
	prior := order.LastAttemptOutcome
	priorAttemptID := order.LastAttemptID
	priorState := order.State
	priorFailureCategory := order.LastFailureCategory
	priorTransientFailures := m.previousTransientFailuresLocked(order)
	priorNextRetryAt := order.NextRetryAt
	clearActiveAttempt(&order)
	lifecycleCommand := core.WorkOrderCmdRecover
	eventKind := "work_order.recovered"
	if priorState == core.WorkOrderQueued {
		lifecycleCommand = ""
		if mode != recoveryAuthorityConfirmed {
			eventKind = "work_order.redispatched"
		}
	}
	next := priorState
	if lifecycleCommand != "" {
		transitioned, err := core.TransitionWorkOrder(priorState, lifecycleCommand)
		if err != nil {
			return core.WorkOrder{}, err
		}
		next = transitioned
	}
	order.State = next
	order.LastAttemptOutcome = ""
	order.RetrySuppressed = false
	order.RetrySuppressionReason = ""
	order.AutomaticRetryCount = 0
	order.NextRetryAt = time.Time{}
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.RedispatchCount++
	order.OperatorDirection = direction
	order.UpdatedAt = now
	order.Claimable = true
	order.ClearExecutionPins()
	number := 0
	windowStart := time.Time{}
	switch mode {
	case recoveryMechanical:
		number, windowStart = StampAutomaticRecovery(&order, policy, now)
	case recoveryAuthorityConfirmed:
		order.Checkpoint = nil
	default:
		ResetAutomaticRecoveryLedger(&order)
	}
	if order.Stage == core.StageImplement {
		m.repinTaskDesignContextLocked(ctx, order.TaskID, now)
	}
	if refreeze != nil {
		order.ExecutionTimeoutText = refreeze.ExecutionTimeoutText
	}
	m.workOrders[order.ID] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	m.recoveries[key] = struct{}{}
	payload := map[string]any{"attempt_id": priorAttemptID, "workspace_id": workspace, "work_order_id": order.ID, "request_id": requestID, "prior_state": priorState, "prior_outcome": prior, "new_state": order.State, "command": lifecycleCommand, "reason": "operator recovery", "direction": direction, "failure_category": priorFailureCategory, "consecutive_transient_failures": priorTransientFailures, "next_retry_at": priorNextRetryAt}
	eventCtx := ctx
	switch mode {
	case recoveryMechanical:
		payload["automatic"] = true
		payload["recovery_number"] = number
		payload["window_started_at"] = windowStart
		payload["next_automatic_recovery_at"] = nil
		payload["reason"] = core.AutomaticRecoveryReason
		eventCtx = WithActor(ctx, SystemActor("queue:"+order.JobID))
	case recoveryAuthorityConfirmed:
		payload = authorityPayload
		eventCtx = WithActor(ctx, SystemActor("queue:"+order.JobID))
	}
	m.appendEventLocked(eventCtx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: eventKind, Payload: core.JSONPayload(payload), At: now})
	return order, nil
}

func (m *memory) redispatchOrderLocked(ctx context.Context, order core.WorkOrder, queueTimeout time.Duration, now time.Time, automatic bool, policy config.RecoveryPolicy, requestID string) (core.WorkOrder, error) {
	if order.State != core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not stale and cannot be redispatched", order.ID)
	}
	next, err := core.TransitionWorkOrder(order.State, core.WorkOrderCmdRedispatch)
	if err != nil {
		return core.WorkOrder{}, err
	}
	order.State, order.Claimable = next, true
	order.ClaimantID, order.SessionID, order.ClientTokenHash = "", "", ""
	order.Agent, order.Model, order.WorkerID, order.Progress = "", "", "", ""
	order.ModelEnforcement = ""
	order.LeaseExpiresAt = time.Time{}
	order.ClearExecutionPins()
	order.QueueEnteredAt, order.QueueDeadline = now, now.Add(queueTimeout)
	order.ExecutionStartedAt, order.ExecutionDeadline = time.Time{}, time.Time{}
	order.RedispatchCount++
	order.UpdatedAt = now
	number := 0
	windowStart := time.Time{}
	if automatic {
		number, windowStart = StampAutomaticRecovery(&order, policy, now)
	} else {
		ResetAutomaticRecoveryLedger(&order)
		ClearAutomaticRecoveryExhaustion(&order)
	}
	if order.Stage == core.StageImplement {
		m.repinTaskDesignContextLocked(ctx, order.TaskID, now)
	}
	m.workOrders[order.ID] = order
	if job, index, exists := m.findJobLocked(order.JobID); exists {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	payload := map[string]any{"work_order_id": order.ID, "prior_state": core.WorkOrderStale, "new_state": order.State, "command": core.WorkOrderCmdRedispatch, "reason": "stale never-claimed queue redispatch"}
	eventCtx := ctx
	if automatic {
		payload["automatic"] = true
		payload["recovery_number"] = number
		payload["window_started_at"] = windowStart
		payload["next_automatic_recovery_at"] = nil
		payload["request_id"] = requestID
		payload["reason"] = core.AutomaticRecoveryReason
		eventCtx = WithActor(ctx, SystemActor("queue:"+order.JobID))
	}
	m.appendEventLocked(eventCtx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", Payload: core.JSONPayload(payload), At: now})
	m.retireWorkOrderSiblingsLocked(ctx, order, "superseded by stale redispatch", now, false)
	return order, nil
}
