package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func cloneStartOverSlices[K comparable, V any](source map[K][]V) map[K][]V {
	result := make(map[K][]V, len(source))
	for key, value := range source {
		result[key] = slices.Clone(value)
	}
	return result
}

func (m *memory) StartOverTaskCommand(ctx context.Context, lease taskops.TaskLease, r core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	var result core.TaskStartOverResult
	if !lease.ValidForCommand(r.TaskID, string(core.TaskStartOver)) {
		return result, fmt.Errorf("task start over requires a valid taskops lease")
	}
	if err := r.Validate(); err != nil {
		return result, err
	}
	var err error
	ctx, err = WithDocumentDismissalNote(ctx, r.Note)
	if err != nil {
		return result, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.tasks[r.TaskID]
	if !ok || old.Workspace != workspaceOrDefault(ctx, "") {
		return result, fmt.Errorf("%w: task %s", ErrNotFound, r.TaskID)
	}
	for _, event := range m.events[old.ID] {
		if event.Kind == "task.started_over" {
			var p struct {
				RequestID   string `json:"request_id"`
				Reason      string `json:"reason"`
				Note        string `json:"note"`
				SuccessorID string `json:"successor_id"`
			}
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return result, err
			}
			if p.RequestID == r.RequestID {
				if p.Reason != r.Reason || p.Note != r.Note {
					return result, ErrStartOverRequestConflict
				}
				return core.TaskStartOverResult{Task: old, Successor: m.tasks[p.SuccessorID]}, nil
			}
		}
	}
	if core.TaskTerminal(old.State) {
		return result, ErrTaskTerminal
	}
	if _, err := core.TransitionTask(old.State, core.TaskStartOver); err != nil {
		return result, err
	}
	// Stage copies of every projection this command can mutate. Failed context,
	// dismissal, cancellation or artifact writes never publish a partial image.
	staged := memory{tasks: maps.Clone(m.tasks), dependencies: maps.Clone(m.dependencies), jobs: cloneStartOverSlices(m.jobs), events: cloneStartOverSlices(m.events), lineage: maps.Clone(m.lineage), interventions: cloneStartOverSlices(m.interventions), workOrders: maps.Clone(m.workOrders), requirements: maps.Clone(m.requirements), requirementVersions: cloneStartOverSlices(m.requirementVersions), systemDesigns: maps.Clone(m.systemDesigns), systemDesignVersions: cloneStartOverSlices(m.systemDesignVersions), taskContextProposals: maps.Clone(m.taskContextProposals), artifacts: maps.Clone(m.artifacts), nextEventID: m.nextEventID, nextReviewID: m.nextReviewID}
	type proposal struct {
		id      string
		version int
		design  bool
	}
	var proposals []proposal
	for key, versions := range staged.requirementVersions {
		if key.workspace != old.Workspace {
			continue
		}
		for _, v := range versions {
			if v.OriginTaskID == old.ID && v.Origin == core.RequirementOriginImplementation && !v.Confirmed && !v.Retired {
				proposals = append(proposals, proposal{key.id, v.Version, false})
			}
		}
	}
	for key, versions := range staged.systemDesignVersions {
		if key.workspace != old.Workspace {
			continue
		}
		for _, v := range versions {
			if v.OriginTaskID == old.ID && v.Origin == core.SystemDesignOriginImplementation && !v.Confirmed && !v.Dismissed {
				proposals = append(proposals, proposal{key.id, v.Version, true})
			}
		}
	}
	if len(proposals) > 0 && !r.CanConfirmDocuments {
		return result, ErrStartOverConfirmDocuments
	}
	actor := ActorFromContext(ctx)
	if strings.ContainsRune(actor.ID, 0) {
		return result, fmt.Errorf("audit actor contains NUL")
	}
	requirements, pinned := ActiveTaskContextReferences(staged.events[old.ID])
	attached := TaskContextInput{}
	for id := range requirements {
		attached.RequirementIDs = append(attached.RequirementIDs, id)
	}
	for id := range pinned {
		attached.DesignIDs = append(attached.DesignIDs, id)
	}
	var dependencies []string
	for id := range staged.dependencies[old.ID] {
		if !core.TaskTerminal(staged.tasks[id].State) {
			dependencies = append(dependencies, id)
		}
	}
	next := StartOverSuccessor(old, r)
	if _, err := staged.cancelTaskLocked(ctx, core.Intervention{TaskID: old.ID, Action: core.InterventionCancel, ReasonCode: r.Reason, Comment: r.Note, At: next.CreatedAt}); err != nil {
		return result, err
	}
	for _, p := range proposals {
		var err error
		if p.design {
			_, _, err = staged.dismissSystemDesignVersionLocked(ctx, p.id, p.version)
		} else {
			_, _, err = staged.dismissRequirementVersionLocked(ctx, p.id, p.version)
		}
		if err != nil && !StartOverDismissalDecided(err) {
			return result, err
		}
	}
	if err := staged.createTaskWithContextLocked(ctx, next, dependencies, attached, pinned); err != nil {
		return result, err
	}
	var plan core.SpecVersion
	for _, candidate := range m.specs[old.ID] {
		if candidate.Approved && candidate.Version > plan.Version {
			plan = candidate
		}
	}
	if plan.Version > 0 {
		if _, err := staged.createArtifactLocked(ctx, core.Artifact{Name: "previous-approved-plan.md", ContentType: "text/markdown", TaskID: next.ID, Role: core.ArtifactRoleTaskContext}, StartOverPlanContent(old.ID, plan.Version, plan.Content)); err != nil {
			return result, err
		}
	}
	old = staged.tasks[old.ID]
	old.SupersededBy = next.ID
	staged.tasks[old.ID] = old
	staged.appendEventLocked(ctx, core.Event{TaskID: old.ID, Kind: "task.started_over", At: next.CreatedAt, Payload: core.JSONPayload(map[string]any{"request_id": r.RequestID, "reason": r.Reason, "note": r.Note, "successor_id": next.ID})})
	m.tasks = staged.tasks
	m.dependencies = staged.dependencies
	m.jobs = staged.jobs
	m.events = staged.events
	m.lineage = staged.lineage
	m.interventions = staged.interventions
	m.workOrders = staged.workOrders
	m.requirements = staged.requirements
	m.requirementVersions = staged.requirementVersions
	m.systemDesigns = staged.systemDesigns
	m.systemDesignVersions = staged.systemDesignVersions
	m.taskContextProposals = staged.taskContextProposals
	m.artifacts = staged.artifacts
	m.nextEventID = staged.nextEventID
	m.nextReviewID = staged.nextReviewID
	return core.TaskStartOverResult{Task: old, Successor: next, Created: true}, nil
}
