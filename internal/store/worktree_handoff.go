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

const WorktreeHandoffCommand core.WorkOrderCommand = "order.worktree_handoff"

// EvaluateWorktreeHandoff runs inside each store's task-serialized transaction.
// Events preserve provenance; current relational claim state grants authority
// (component-work-orders HO-1 through HO-3; DEC-35, DEC-36).
func EvaluateWorktreeHandoff(task core.Task, order core.WorkOrder, claim core.WorkOrderClaimIdentity, repository string, events []core.Event, request core.WorktreeHandoffRequest, now time.Time) (core.WorktreeHandoff, []core.Event, error) {
	var result core.WorktreeHandoff
	refuse := func(reason string) (core.WorktreeHandoff, []core.Event, error) {
		return result, nil, fmt.Errorf("worktree handoff refused: %s", reason)
	}
	if order.TaskID != task.ID || order.Stage != core.StageImplement || repository == "" || task.Branch == "" || request.SessionID == "" || request.SessionID != claim.SessionID || request.Generation == "" || len(request.Generation) > 128 {
		return refuse("missing or mismatched writer identity")
	}
	var producerClaim core.WorkOrder
	var currentClaimID int64
	var predecessor *core.WorktreeIdentity
	var latestWriter *core.WorktreeIdentity
	var lastStarted *core.WorktreeIdentity
	var admittedPredecessor *core.WorktreeIdentity
	previouslyAdmitted := false
	for _, event := range events {
		if event.TaskID != task.ID {
			continue
		}
		switch event.Kind {
		case "work_order.claimed":
			var c core.WorkOrder
			if json.Unmarshal(event.Payload, &c) != nil || c.Stage != core.StageImplement {
				continue
			}
			identity := core.WorktreeIdentity{Workspace: task.Workspace, TaskID: task.ID, Repository: repository, Branch: task.Branch, WorkOrderID: c.ID, AttemptID: c.AttemptID, SessionID: c.SessionID, ClaimEventID: event.ID}
			if c.ID == order.ID && c.SessionID == claim.SessionID && c.WorkerID == claim.WorkerID && c.ClaimantID == claim.ClaimantID {
				producerClaim = c
				currentClaimID = event.ID
			} else if currentClaimID == 0 {
				copy := identity
				predecessor = &copy
			}
		case "work_order.writer_started":
			var started core.WorktreeIdentity
			if json.Unmarshal(event.Payload, &started) != nil {
				return refuse("invalid producer evidence")
			}
			if started.AttemptID != producerClaim.AttemptID {
				lastStarted = &started
			}
		case "work_order.writer_admitted":
			var writer core.WorktreeIdentity
			if json.Unmarshal(event.Payload, &writer) != nil {
				return refuse("invalid writer evidence")
			}
			latestWriter = &writer
			var admission struct {
				Predecessor *core.WorktreeIdentity `json:"predecessor"`
			}
			if json.Unmarshal(event.Payload, &admission) != nil {
				return refuse("invalid predecessor evidence")
			}
			admittedPredecessor = admission.Predecessor
			if writer.WorkOrderID == order.ID && writer.SessionID == claim.SessionID {
				previouslyAdmitted = true
			}
		}
	}
	if currentClaimID == 0 || producerClaim.AttemptID == "" {
		return refuse("missing exact claim evidence")
	}
	writer := core.WorktreeIdentity{Workspace: task.Workspace, TaskID: task.ID, Repository: repository, Branch: task.Branch, WorkOrderID: order.ID, AttemptID: producerClaim.AttemptID, SessionID: claim.SessionID, Generation: request.Generation, ClaimEventID: currentClaimID}
	active := order.State == core.WorkOrderClaimed && order.AttemptID == writer.AttemptID && order.SessionID == claim.SessionID && order.WorkerID == claim.WorkerID && order.ClaimantID == claim.ClaimantID && order.LeaseExpiresAt.After(now)
	released := false
	for _, event := range events {
		if event.Kind != "work_order.released" {
			continue
		}
		var p struct {
			WorkOrderID string `json:"work_order_id"`
			AttemptID   string `json:"attempt_id"`
			Reason      string `json:"reason"`
		}
		if json.Unmarshal(event.Payload, &p) == nil && event.JobID == order.JobID && p.AttemptID == writer.AttemptID && (p.Reason == core.WorkOrderReleaseReasonOperatorCheckpointReached || p.Reason == core.WorkOrderReleaseReasonPlanRevisionRequested) {
			released = true
			writer.Reason = p.Reason
			writer.ReleaseEventID = event.ID
		}
	}
	enrich := func(identity *core.WorktreeIdentity) error {
		if identity == nil {
			return nil
		}
		for _, event := range events {
			var p struct {
				WorkOrderID string `json:"work_order_id"`
				AttemptID   string `json:"attempt_id"`
				Reason      string `json:"reason"`
				CommitSHA   string `json:"commit_sha"`
			}
			if json.Unmarshal(event.Payload, &p) != nil || (p.WorkOrderID != identity.WorkOrderID && event.JobID != identity.WorkOrderID) || p.AttemptID != identity.AttemptID {
				continue
			}
			if event.Kind == "work_order.released" {
				identity.Reason = p.Reason
				identity.ReleaseEventID = event.ID
			}
			if event.Kind == "work_order.attempt_checkpointed" {
				if identity.CommitSHA != "" && identity.CommitSHA != p.CommitSHA {
					return fmt.Errorf("conflicting checkpoint SHA evidence")
				}
				identity.CommitSHA = p.CommitSHA
				identity.CheckpointEventID = event.ID
			}
		}
		return nil
	}
	if lastStarted != nil {
		predecessor = lastStarted
	} else if latestWriter != nil && admittedPredecessor != nil {
		predecessor = admittedPredecessor
	}
	if predecessor != nil {
		if predecessor.Workspace != task.Workspace || predecessor.TaskID != task.ID || predecessor.Repository != repository || predecessor.Branch != task.Branch {
			return refuse("predecessor task or repository identity conflicts")
		}
		proven := false
		for _, e := range events {
			if e.Kind == "work_order.claimed" && e.ID == predecessor.ClaimEventID {
				var c core.WorkOrder
				if json.Unmarshal(e.Payload, &c) == nil && c.TaskID == task.ID && c.ID == predecessor.WorkOrderID && c.AttemptID == predecessor.AttemptID && c.SessionID == predecessor.SessionID {
					proven = true
				}
			}
		}
		if !proven {
			return refuse("predecessor lacks matching claim evidence")
		}
	}
	if err := enrich(predecessor); err != nil {
		return refuse(err.Error())
	}
	result = core.WorktreeHandoff{Writer: writer, Predecessor: predecessor}
	sameWriter := latestWriter != nil && latestWriter.WorkOrderID == writer.WorkOrderID && latestWriter.AttemptID == writer.AttemptID && latestWriter.SessionID == writer.SessionID && latestWriter.Generation == writer.Generation && latestWriter.Repository == repository && latestWriter.Branch == task.Branch
	event := func(kind string, payload any) core.Event {
		return core.Event{TaskID: task.ID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(payload), At: now}
	}
	switch request.Action {
	case "describe":
		if !active {
			return refuse("exact claim is not live")
		}
		return result, nil, nil
	case "admit":
		if !active {
			return refuse("exact claim is not live")
		}
		if sameWriter {
			return result, nil, nil
		}
		if previouslyAdmitted {
			return refuse("writer generation was superseded")
		}
		result.Created = true
		return result, []core.Event{event("work_order.writer_admitted", struct {
			core.WorktreeIdentity
			Predecessor *core.WorktreeIdentity `json:"predecessor,omitempty"`
		}{writer, predecessor})}, nil
	case "preserve", "audit", "ready":
		if !sameWriter || (!active && !released) {
			return refuse("writer no longer owns preservation authority")
		}
	default:
		return refuse("unknown action")
	}
	if request.Action == "ready" {
		if !active {
			return refuse("writer cannot start after release")
		}
		for _, e := range events {
			if e.Kind == "work_order.writer_started" {
				var p core.WorktreeIdentity
				if json.Unmarshal(e.Payload, &p) == nil && p.AttemptID == writer.AttemptID {
					return result, nil, nil
				}
			}
		}
		return result, []core.Event{event("work_order.writer_started", writer)}, nil
	}
	if request.Action == "preserve" {
		return result, nil, nil
	}
	if request.Producer == nil || len(request.CommitSHA) != 40 || strings.Trim(request.CommitSHA, "0123456789abcdef") != "" || strings.TrimSpace(request.OriginalReason) == "" || len(request.OriginalReason) > 4096 {
		return refuse("incomplete checkpoint evidence")
	}
	producer := *request.Producer
	expected := writer
	if producer.AttemptID != writer.AttemptID {
		if !active || predecessor == nil {
			return refuse("no predecessor chain")
		}
		expected = *predecessor
	}
	// Generation and checkpoint/release fields may have been enriched since the
	// descriptor was delivered. Immutable claim identity must still match.
	sameClaim := func(a, b core.WorktreeIdentity) bool {
		a.Generation = ""
		b.Generation = ""
		a.ReleaseEventID = 0
		b.ReleaseEventID = 0
		a.CheckpointEventID = 0
		b.CheckpointEventID = 0
		a.CommitSHA = ""
		b.CommitSHA = ""
		a.Reason = ""
		b.Reason = ""
		return reflect.DeepEqual(a, b)
	}
	if !sameClaim(producer, expected) {
		return refuse("producer differs from durable claim chain")
	}
	if err := enrich(&expected); err != nil {
		return refuse(err.Error())
	}
	if expected.CommitSHA != "" && expected.CommitSHA != request.CommitSHA {
		return refuse("checkpoint SHA conflicts with existing audit")
	}
	var writes []core.Event
	if expected.CheckpointEventID == 0 {
		writes = append(writes, event("work_order.attempt_checkpointed", map[string]any{"task_id": task.ID, "work_order_id": expected.WorkOrderID, "attempt_id": expected.AttemptID, "session_id": expected.SessionID, "commit_sha": request.CommitSHA, "termination_reason": request.OriginalReason, "push_result": "pushed", "producer": expected, "recovering_writer": writer}))
	}
	payload := map[string]any{"producer": expected, "recovering_writer": writer, "commit_sha": request.CommitSHA, "original_reason": request.OriginalReason, "observed_reason": expected.Reason}
	// Stable reconciliation identity excludes the audit ID which may become
	// known between an uncertain append and its retry.
	for _, e := range events {
		if e.Kind == "work_order.checkpoint_reconciled" {
			var p struct {
				Producer         core.WorktreeIdentity `json:"producer"`
				CommitSHA        string                `json:"commit_sha"`
				OriginalReason   string                `json:"original_reason"`
				RecoveringWriter core.WorktreeIdentity `json:"recovering_writer"`
				ObservedReason   string                `json:"observed_reason"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && sameClaim(p.Producer, expected) && p.CommitSHA == request.CommitSHA && p.OriginalReason == request.OriginalReason && sameClaim(p.RecoveringWriter, writer) && p.ObservedReason == expected.Reason {
				return result, writes, nil
			}
		}
	}
	writes = append(writes, event("work_order.checkpoint_reconciled", payload))
	result.Created = true
	return result, writes, nil
}

func (m *memory) WorktreeHandoffCommand(ctx context.Context, lease taskops.TaskLease, id string, claim core.WorkOrderClaimIdentity, repository string, request core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorktreeHandoff{}, ErrWorkOrderClaimLost
	}
	task := m.tasks[order.TaskID]
	if !lease.ValidForCommand(task.ID, string(WorktreeHandoffCommand)) {
		return core.WorktreeHandoff{}, fmt.Errorf("worktree handoff requires taskops lease")
	}
	if workspace, ok := WorkspaceFromContext(ctx); ok && workspace != task.Workspace {
		return core.WorktreeHandoff{}, ErrWorkOrderClaimLost
	}
	result, events, err := EvaluateWorktreeHandoff(task, order, claim, repository, m.events[task.ID], request, time.Now().UTC())
	if err != nil {
		return result, err
	}
	for _, event := range events {
		m.appendEventLocked(ctx, event)
	}
	return result, nil
}

// CheckpointReleaseForClaim recognizes a deliberate handoff even after the
// operator has cancelled its order to create a revised plan. This is outcome
// evidence only; writer admission independently fences preservation authority.
func CheckpointReleaseForClaim(events []core.Event, order core.WorkOrder, claim core.WorkOrderClaimIdentity) (string, bool) {
	attempt := ""
	for _, event := range events {
		if event.Kind == "work_order.claimed" {
			var c core.WorkOrder
			if json.Unmarshal(event.Payload, &c) == nil && c.ID == order.ID && c.SessionID == claim.SessionID && c.ClaimantID == claim.ClaimantID && c.WorkerID == claim.WorkerID {
				attempt = c.AttemptID
			}
		}
	}
	if attempt == "" {
		return "", false
	}
	for _, event := range events {
		if event.JobID == order.JobID && event.Kind == "work_order.released" {
			var p struct {
				AttemptID string `json:"attempt_id"`
				SessionID string `json:"session_id"`
				Reason    string `json:"reason"`
			}
			if json.Unmarshal(event.Payload, &p) == nil && p.AttemptID == attempt && p.SessionID == claim.SessionID && (p.Reason == core.WorkOrderReleaseReasonOperatorCheckpointReached || p.Reason == core.WorkOrderReleaseReasonPlanRevisionRequested) {
				return p.Reason, true
			}
		}
	}
	return "", false
}
