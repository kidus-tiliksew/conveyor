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

const ContextObservationCommand core.WorkOrderCommand = "order.context_observe"

type ContextObservationStore interface {
	RecordInProcessContextObservation(context.Context, taskops.TaskLease, core.ContextObservation) (core.ContextObservation, error)
	RecordContextObservation(context.Context, taskops.TaskLease, core.ContextObservation) (core.ContextObservation, error)
}

// ValidateContextClaim repeats exact scope and authenticated owner checks under
// the store serialization boundary. No client assertion grants delivery state.
func ValidateContextClaim(ctx context.Context, task core.Task, order core.WorkOrder, attempt, session, workerOwner string, now time.Time) error {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || task.Workspace != ws || task.ID != order.TaskID || order.AttemptID != attempt || session == "" || order.SessionID != session || order.State != core.WorkOrderClaimed || !order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return ErrWorkOrderClaimUnauthorized
	}
	actor := ActorFromContext(ctx)
	switch actor.Role {
	case core.ActorWorker:
		if order.WorkerID != "" && actor.ID == WorkerActorID(order.WorkerID) {
			return nil
		}
	case core.ActorUser:
		if order.WorkerID == "" && order.ClaimantID == core.TaskRunClaimantID(strings.TrimPrefix(actor.ID, "user:")) {
			return nil
		}
	case core.ActorAgent:
		c, ok := CredentialFromContext(ctx)
		if !ok || c.Kind != core.CredentialAgent || actor.ID != AgentActorID(c.ID) {
			break
		}
		if c.RunWorkOrderID != "" && (c.RunWorkspaceID != ws || c.RunWorkOrderID != order.ID || c.RunSessionID != session) {
			break
		}
		if order.WorkerID == "" && order.ClaimantID == core.TaskRunClaimantID(c.OwnerUserID) {
			return nil
		}
		if order.WorkerID != "" && workerOwner != "" && workerOwner == c.OwnerUserID {
			return nil
		}
	}
	return ErrWorkOrderClaimUnauthorized
}

func EvaluateContextObservation(ctx context.Context, task core.Task, order core.WorkOrder, workerOwner string, events []core.Event, o core.ContextObservation, now time.Time) (core.ContextObservation, *core.Event, error) {
	if err := ValidateContextClaim(ctx, task, order, o.AttemptID, o.SessionID, workerOwner, now); err != nil {
		return core.ContextObservation{}, nil, err
	}
	return evaluateContextObservation(ctx, task, order, events, o, now)
}
func evaluateContextObservation(ctx context.Context, task core.Task, order core.WorkOrder, events []core.Event, o core.ContextObservation, now time.Time) (core.ContextObservation, *core.Event, error) {
	if o.Workspace != task.Workspace || o.TaskID != task.ID || o.WorkOrderID != order.ID || o.Schema != 1 || !core.ValidContextRevision(o.SelectionRevision) {
		return core.ContextObservation{}, nil, ErrWorkOrderClaimUnauthorized
	}
	switch o.Kind {
	case "context.baseline_observed", "context.refresh_observed", "context.verdict_refresh_observed":
		if o.Outcome != "available" || o.Snapshot == nil || o.Snapshot.Revision != o.SelectionRevision {
			return core.ContextObservation{}, nil, fmt.Errorf("invalid context observation")
		}
	case "context.artifact_fetch_observed":
		switch o.Outcome {
		case "fetched":
			if o.ArtifactID == "" || !core.ValidContextRevision(o.ContentDigest) || o.ReturnedBytes < 0 {
				return core.ContextObservation{}, nil, fmt.Errorf("invalid fetch receipt")
			}
		case "fetch_failed", "omitted", "truncated":
			if o.ArtifactID == "" || o.ContentDigest != "" || o.ReturnedBytes != 0 {
				return core.ContextObservation{}, nil, fmt.Errorf("invalid fetch receipt")
			}
		default:
			return core.ContextObservation{}, nil, fmt.Errorf("invalid context outcome")
		}
	default:
		return core.ContextObservation{}, nil, fmt.Errorf("invalid context operation")
	}
	actor := ActorFromContext(ctx)
	o.ActorID = actor.ID
	o.ActorRole = actor.Role
	o.ObservedAt = now
	key := o.ReplayKey()
	if o.ID != "" && o.ID != key {
		return core.ContextObservation{}, nil, fmt.Errorf("context replay conflict")
	}
	o.ID = key
	if len(core.JSONPayload(o)) > core.ContextEnvelopeBytes+4096 {
		return core.ContextObservation{}, nil, fmt.Errorf("context observation too large")
	}
	for _, e := range events {
		if e.Kind != o.Kind {
			continue
		}
		var prior core.ContextObservation
		if json.Unmarshal(e.Payload, &prior) == nil && prior.ID == key {
			return prior, nil, nil
		}
	}
	e := core.Event{TaskID: task.ID, JobID: order.JobID, Kind: o.Kind, Payload: core.JSONPayload(o), At: now}
	return o, &e, nil
}
func (m *memory) RecordContextObservation(ctx context.Context, lease taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[o.WorkOrderID]
	if !ok {
		return core.ContextObservation{}, ErrWorkOrderClaimUnauthorized
	}
	if !lease.ValidForCommand(order.TaskID, string(ContextObservationCommand)) {
		return core.ContextObservation{}, fmt.Errorf("context observation requires taskops lease")
	}
	task := m.tasks[order.TaskID]
	owner := ""
	if order.WorkerID != "" {
		worker, exists := m.workers[order.WorkerID]
		if !exists || !worker.RevokedAt.IsZero() {
			return core.ContextObservation{}, ErrWorkOrderClaimUnauthorized
		}
		owner = worker.OwnerUserID
	}
	result, event, err := EvaluateContextObservation(ctx, task, order, owner, m.events[task.ID], o, time.Now().UTC())
	if err == nil && event != nil {
		m.appendEventLocked(ctx, *event)
	}
	return result, err
}

// ContextHistory ignores every other attempt, even when content IDs match.
func ContextHistory(events []core.Event, order core.WorkOrder) []core.ContextObservation {
	var result []core.ContextObservation
	for _, e := range events {
		if !strings.HasPrefix(e.Kind, "context.") {
			continue
		}
		var o core.ContextObservation
		if json.Unmarshal(e.Payload, &o) == nil && o.WorkOrderID == order.ID && o.TaskID == order.TaskID && o.AttemptID == order.AttemptID && o.SessionID == order.SessionID {
			result = append(result, o)
		}
	}
	return result
}
func ProjectContext(snapshot core.ContextSnapshot, history []core.ContextObservation, priorRevision string) core.ContextFreshness {
	var prior *core.ContextSnapshot
	for _, o := range history {
		if o.Snapshot != nil && (priorRevision == "" || o.SelectionRevision == priorRevision) {
			copy := *o.Snapshot
			prior = &copy
			if priorRevision != "" {
				break
			}
		}
	}
	f := core.CompareContext(snapshot, prior)
	for _, d := range snapshot.Artifacts.Items {
		status := core.ContextDelivery{ArtifactID: d.ArtifactID, State: "not_fetched_in_this_attempt", Acknowledgement: "not_recorded"}
		for _, o := range history {
			if o.Kind != "context.artifact_fetch_observed" || o.ArtifactID != d.ArtifactID {
				continue
			}
			switch o.Outcome {
			case "fetched":
				status.Fetched = true
				status.State = "fetched"
			case "fetch_failed":
				status.Failed = true
			case "omitted":
				status.Omitted = true
			case "truncated":
				status.Truncated = true
			}
		}
		f.Deliveries = append(f.Deliveries, status)
		if !status.Fetched {
			for _, added := range f.Additions.Items {
				if added.ArtifactID == d.ArtifactID && added.OmissionReason == "" {
					f.UnfetchedAdditions++
					break
				}
			}
		}
	}
	f.ObservationRevision = core.ContextDigest(f.Deliveries)
	return core.BoundContextFreshness(f)
}

// In-process jobs have no external work-order claim. Their existing running
// job and system actor form a separate explicit input boundary, never a
// fabricated external session (component-harness-execution CF-H1).
func EvaluateInProcessContextObservation(ctx context.Context, task core.Task, job core.Job, events []core.Event, o core.ContextObservation, now time.Time) (core.ContextObservation, *core.Event, error) {
	ws, ok := WorkspaceFromContext(ctx)
	actor := ActorFromContext(ctx)
	if !ok || task.Workspace != ws || actor.Role != core.ActorSystem || actor.ID != "dispatcher" || job.TaskID != task.ID || job.ID != o.JobID || job.Runner != "in-process" || job.State != core.JobRunning || o.WorkOrderID != "" || o.SessionID != "" || o.AttemptID != job.ID || o.DeliveryBoundary != "provider_input" {
		return core.ContextObservation{}, nil, ErrWorkOrderClaimUnauthorized
	}
	order := core.WorkOrder{TaskID: task.ID, JobID: job.ID}
	return evaluateContextObservation(ctx, task, order, events, o, now)
}
func (m *memory) RecordInProcessContextObservation(ctx context.Context, lease taskops.TaskLease, o core.ContextObservation) (core.ContextObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !lease.ValidForCommand(o.TaskID, string(ContextObservationCommand)) {
		return core.ContextObservation{}, ErrWorkOrderClaimUnauthorized
	}
	job, _, ok := m.findJobLocked(o.JobID)
	if !ok {
		return core.ContextObservation{}, ErrWorkOrderClaimUnauthorized
	}
	result, event, err := EvaluateInProcessContextObservation(ctx, m.tasks[o.TaskID], job, m.events[o.TaskID], o, time.Now().UTC())
	if err == nil && event != nil {
		m.appendEventLocked(ctx, *event)
	}
	return result, err
}
