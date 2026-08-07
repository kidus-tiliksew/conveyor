package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// WorkOrderPreemptRequest identifies one operator command. RequestID makes
// retries safe; Reason is mandatory audit context, not agent-authored state.
type WorkOrderPreemptRequest struct {
	WorkOrderID string `json:"work_order_id"`
	RequestID   string `json:"request_id"`
	Reason      string `json:"reason"`
}

type WorkOrderPreemptResult struct {
	RequestID        string         `json:"request_id"`
	WorkOrder        core.WorkOrder `json:"work_order"`
	RevokedAttemptID string         `json:"revoked_attempt_id"`
	RevokedSessionID string         `json:"revoked_session_id"`
	RevokedWorkerID  string         `json:"revoked_worker_id"`
	GraceBound       string         `json:"grace_bound"`
}

type memoryWorkOrderPreemption struct {
	Workspace string
	Request   WorkOrderPreemptRequest
	Result    WorkOrderPreemptResult
}

func PrepareWorkOrderPreemptRequest(request WorkOrderPreemptRequest) (WorkOrderPreemptRequest, error) {
	request.WorkOrderID = strings.TrimSpace(request.WorkOrderID)
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.WorkOrderID == "" || request.RequestID == "" || request.Reason == "" {
		return request, fmt.Errorf("work_order_id, reason, and request_id are required")
	}
	return request, nil
}

func SameWorkOrderPreempt(left, right WorkOrderPreemptRequest) bool {
	return left.WorkOrderID == right.WorkOrderID && left.RequestID == right.RequestID && left.Reason == right.Reason
}

func (m *memory) PreemptWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, raw WorkOrderPreemptRequest) (WorkOrderPreemptResult, error) {
	request, err := PrepareWorkOrderPreemptRequest(raw)
	if err != nil {
		return WorkOrderPreemptResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, _ := WorkspaceFromContext(ctx)
	if prior, ok := m.preemptions[request.RequestID]; ok {
		if prior.Workspace != workspace || !SameWorkOrderPreempt(prior.Request, request) {
			return WorkOrderPreemptResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrWorkOrderPreemptConflict, request.RequestID)
		}
		return prior.Result, nil
	}
	order, ok := m.workOrders[request.WorkOrderID]
	if !ok {
		return WorkOrderPreemptResult{}, fmt.Errorf("work order %s not found", request.WorkOrderID)
	}
	if !lease.ValidForCommand(order.TaskID, string(core.WorkOrderCmdPreempt)) {
		return WorkOrderPreemptResult{}, fmt.Errorf("work-order preempt requires a valid taskops lease")
	}
	now := time.Now().UTC()
	order = m.refreshWorkOrderLocked(ctx, order, now)
	if order.State != core.WorkOrderClaimed || order.SessionID == "" || order.AttemptID == "" {
		return WorkOrderPreemptResult{}, fmt.Errorf("%w: work order %s does not have an active claimed attempt", ErrWorkOrderPreemptConflict, order.ID)
	}
	next, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdPreempt)
	if transitionErr != nil {
		return WorkOrderPreemptResult{}, transitionErr
	}
	result := WorkOrderPreemptResult{
		RequestID: request.RequestID, RevokedAttemptID: order.AttemptID,
		RevokedSessionID: order.SessionID, RevokedWorkerID: order.WorkerID,
		GraceBound: "one renewal interval",
	}
	order.LastAttemptID = order.AttemptID
	clearActiveAttempt(&order)
	order.State = next
	order.LastAttemptOutcome = core.WorkOrderOutcomePreempted
	order.NextRetryAt = time.Time{}
	order.RetrySuppressed = false
	order.RetrySuppressionReason = ""
	order.LastFailureCategory, order.LastFailureMessage, order.LastFailureDetail = "", "", ""
	order.LastFailureExitStatus, order.LastFailureAt = nil, time.Time{}
	order.UpdatedAt = now
	order.Claimable = order.ClaimableAt(now)
	m.workOrders[order.ID] = order
	if job, index, found := m.findJobLocked(order.JobID); found {
		job.State, job.StartedAt, job.EndedAt = core.JobPending, time.Time{}, time.Time{}
		m.jobs[job.TaskID][index] = job
	}
	result.WorkOrder = order
	actor := ActorFromContext(ctx)
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.preempted", ActorID: actor.ID, ActorRole: actor.Role,
		Payload: core.JSONPayload(map[string]any{
			"work_order_id": order.ID, "request_id": request.RequestID, "reason": request.Reason,
			"attempt_id": result.RevokedAttemptID, "session_id": result.RevokedSessionID, "worker_id": result.RevokedWorkerID,
			"prior_state": core.WorkOrderClaimed, "new_state": order.State, "command": core.WorkOrderCmdPreempt,
			"queue_entered_at": order.QueueEnteredAt, "queue_deadline": order.QueueDeadline, "grace_bound": result.GraceBound,
		}), At: now})
	m.preemptions[request.RequestID] = memoryWorkOrderPreemption{Workspace: workspace, Request: request, Result: result}
	return result, nil
}

func WorkOrderPreemptEventMatches(event core.Event, workOrderID, workerID, sessionID string) bool {
	if event.Kind != "work_order.preempted" {
		return false
	}
	var payload struct {
		WorkOrderID string `json:"work_order_id"`
		WorkerID    string `json:"worker_id"`
		SessionID   string `json:"session_id"`
	}
	return json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID == workOrderID &&
		payload.WorkerID == workerID && payload.SessionID == sessionID
}
