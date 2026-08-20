package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func proposalKey(taskID string, kind core.TaskContextProposalTargetKind, targetID string) string {
	return taskID + "\x00" + string(kind) + "\x00" + targetID
}

func (m *memory) ProposeTaskContext(ctx context.Context, input core.TaskContextProposalInput) (core.TaskContextProposal, bool, error) {
	return m.proposeTaskContext(ctx, input, false)
}

func (m *memory) proposeTaskContext(ctx context.Context, input core.TaskContextProposalInput, legacyCompatibility bool) (core.TaskContextProposal, bool, error) {
	input.TaskID, input.TargetID, input.Justification = strings.TrimSpace(input.TaskID), strings.TrimSpace(input.TargetID), strings.TrimSpace(input.Justification)
	if !input.Valid() {
		return core.TaskContextProposal{}, false, fmt.Errorf("invalid task context proposal")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[input.TaskID]
	if !ok || task.Workspace != workspaceOrDefault(ctx, "") {
		return core.TaskContextProposal{}, false, fmt.Errorf("task %s: %w", input.TaskID, ErrNotFound)
	}
	if core.TaskTerminal(task.State) && !legacyCompatibility {
		return core.TaskContextProposal{}, false, ErrTaskTerminal
	}
	activeRequirements, activeDesigns := ActiveTaskContextReferences(m.events[input.TaskID])
	if input.TargetKind == core.TaskContextProposalRequirement && activeRequirements[input.TargetID] ||
		input.TargetKind == core.TaskContextProposalSystemDesign && activeDesigns[input.TargetID] > 0 {
		return core.TaskContextProposal{}, true, nil
	}
	var title string
	if input.TargetKind == core.TaskContextProposalRequirement {
		document, exists := m.requirements[memoryScopedKey{workspace: task.Workspace, id: input.TargetID}]
		if !exists {
			return core.TaskContextProposal{}, false, &TaskContextReferenceError{Kind: "requirement", ID: input.TargetID, Reason: "was not found in this workspace"}
		}
		if document.CurrentVersion <= 0 && !legacyCompatibility {
			return core.TaskContextProposal{}, false, &TaskContextReferenceError{Kind: "requirement", ID: input.TargetID, Reason: "has no confirmed version"}
		}
		title = document.Title
	} else {
		document, exists := m.systemDesigns[memoryScopedKey{workspace: task.Workspace, id: input.TargetID}]
		if !exists {
			return core.TaskContextProposal{}, false, &TaskContextReferenceError{Kind: "system design", ID: input.TargetID, Reason: "was not found in this workspace"}
		}
		if document.CurrentVersion <= 0 && !legacyCompatibility {
			return core.TaskContextProposal{}, false, &TaskContextReferenceError{Kind: "system design", ID: input.TargetID, Reason: "has no confirmed version"}
		}
		title = document.Title
	}
	key := proposalKey(input.TaskID, input.TargetKind, input.TargetID)
	if existing, exists := m.taskContextProposals[key]; exists {
		if existing.State == core.TaskContextProposalProposed {
			return existing, true, nil
		}
		return core.TaskContextProposal{}, false, fmt.Errorf("%w: cannot repropose a %s proposal", ErrTaskContextProposalTransition, existing.State)
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	eventKind := "task.context_proposed"
	payload := map[string]any{"target_kind": input.TargetKind, "target_id": input.TargetID, "source": input.Source, "justification": input.Justification}
	if input.TargetKind == core.TaskContextProposalRequirement && legacyCompatibility {
		document := m.requirements[memoryScopedKey{workspace: task.Workspace, id: input.TargetID}]
		eventKind, payload["requirement_id"] = "task.requirement_suggested", input.TargetID
		payload["requirement_slug"], payload["requirement_title"] = document.Slug, document.Title
	}
	m.appendEventLocked(ctx, core.Event{TaskID: input.TaskID, Kind: eventKind, At: now, Payload: core.JSONPayload(payload)})
	proposal := core.TaskContextProposal{TaskID: input.TaskID, TargetKind: input.TargetKind, TargetID: input.TargetID, TargetTitle: title,
		State: core.TaskContextProposalProposed, Source: input.Source, Justification: input.Justification, CreatedByEventID: m.nextEventID,
		ProposedBy: actor.ID, Workspace: task.Workspace, CreatedAt: now, UpdatedAt: now}
	m.taskContextProposals[key] = proposal
	return proposal, false, nil
}

func (m *memory) ConfirmTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return m.transitionTaskContextProposal(ctx, taskID, kind, targetID, core.TaskContextProposalConfirmed, false)
}

func (m *memory) DismissTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string) (core.TaskContextProposal, error) {
	return m.transitionTaskContextProposal(ctx, taskID, kind, targetID, core.TaskContextProposalDismissed, false)
}

func (m *memory) transitionTaskContextProposal(ctx context.Context, taskID string, kind core.TaskContextProposalTargetKind, targetID string, target core.TaskContextProposalState, legacyCompatibility bool) (core.TaskContextProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.Workspace != workspaceOrDefault(ctx, "") {
		return core.TaskContextProposal{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if core.TaskTerminal(task.State) && !legacyCompatibility {
		return core.TaskContextProposal{}, ErrTaskTerminal
	}
	key := proposalKey(taskID, kind, strings.TrimSpace(targetID))
	proposal, ok := m.taskContextProposals[key]
	if !ok {
		return core.TaskContextProposal{}, fmt.Errorf("task context proposal: %w", ErrNotFound)
	}
	if proposal.State == target {
		return proposal, nil
	}
	if proposal.State != core.TaskContextProposalProposed {
		return core.TaskContextProposal{}, fmt.Errorf("%w: cannot transition %s proposal to %s", ErrTaskContextProposalTransition, proposal.State, target)
	}
	actor, now := ActorFromContext(ctx), time.Now().UTC()
	eventKind := "task.context_proposal_dismissed"
	if target == core.TaskContextProposalConfirmed {
		eventKind = "task.context_proposal_confirmed"
	}
	payload := map[string]any{"target_kind": kind, "target_id": targetID}
	if kind == core.TaskContextProposalRequirement && legacyCompatibility {
		payload["requirement_id"] = targetID
		if target == core.TaskContextProposalConfirmed {
			eventKind = "requirement.serves_confirmed"
		} else {
			eventKind = "requirement.serves_dismissed"
		}
	}
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: eventKind, At: now, Payload: core.JSONPayload(payload)})
	decisionEventID := m.nextEventID
	if target == core.TaskContextProposalConfirmed {
		activeRequirements, activeDesigns := ActiveTaskContextReferences(m.events[taskID])
		if kind == core.TaskContextProposalRequirement {
			if !activeRequirements[targetID] {
				m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextRequirementAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": targetID})})
			}
		} else {
			if activeDesigns[targetID] == 0 {
				document := m.systemDesigns[memoryScopedKey{workspace: task.Workspace, id: targetID}]
				m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": targetID, "version": document.CurrentVersion})})
			}
		}
	}
	proposal.State, proposal.DecisionEventID, proposal.DecidedBy, proposal.UpdatedAt = target, decisionEventID, actor.ID, now
	m.taskContextProposals[key] = proposal
	return proposal, nil
}

func (m *memory) ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := make([]core.TaskContextProposal, 0)
	for _, proposal := range m.taskContextProposals {
		if proposal.Workspace == workspace && (taskID == "" || proposal.TaskID == taskID) && (state == "" || proposal.State == state) {
			out = append(out, proposal)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		if out[i].TargetKind != out[j].TargetKind {
			return out[i].TargetKind < out[j].TargetKind
		}
		return out[i].TargetID < out[j].TargetID
	})
	return out, nil
}
