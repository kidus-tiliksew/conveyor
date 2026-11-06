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
	if _, err := RequireActor(ctx); err != nil {
		return core.TaskStartOverResult{}, err
	}
	return m.startOverTaskCommand(ctx, lease, r, TaskTitleSecrets{})
}

// StartOverTaskCommand reads the stored secrets before the store lock, as
// applyVerification does.
func (m *volatileMemory) StartOverTaskCommand(ctx context.Context, lease taskops.TaskLease, r core.TaskStartOverRequest) (core.TaskStartOverResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.TaskStartOverResult{}, err
	}
	return m.memory.startOverTaskCommand(ctx, lease, r, ReadTaskTitleSecrets(ctx, m))
}

func (m *memory) startOverTaskCommand(ctx context.Context, lease taskops.TaskLease, r core.TaskStartOverRequest, secrets TaskTitleSecrets) (core.TaskStartOverResult, error) {
	var result core.TaskStartOverResult
	if !lease.ValidForCommand(r.TaskID, string(core.TaskStartOver)) {
		return result, fmt.Errorf("task start over requires a valid taskops lease")
	}
	if err := r.Validate(); err != nil {
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
				return core.TaskStartOverResult{Task: old, Successor: m.tasks[p.SuccessorID], DroppedContext: StartOverDroppedContextFromEvents(m.events[p.SuccessorID])}, nil
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
	// cancellation or artifact writes never publish a partial image.
	staged := memory{tasks: maps.Clone(m.tasks), dependencies: maps.Clone(m.dependencies), jobs: cloneStartOverSlices(m.jobs), events: cloneStartOverSlices(m.events), lineage: m.lineage.clone(), interventions: cloneStartOverSlices(m.interventions), workOrders: maps.Clone(m.workOrders), governanceSnapshots: maps.Clone(m.governanceSnapshots), requirements: maps.Clone(m.requirements), requirementVersions: cloneStartOverSlices(m.requirementVersions), systemDesigns: maps.Clone(m.systemDesigns), systemDesignVersions: cloneStartOverSlices(m.systemDesignVersions), taskContextProposals: maps.Clone(m.taskContextProposals), artifacts: maps.Clone(m.artifacts), nextEventID: m.nextEventID, nextReviewID: m.nextReviewID}
	stageNotices(&staged)
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
	attached, pinned, drops, err := PartitionStartOverContext(attached, pinned, func(input TaskContextInput) error {
		_, validateErr := staged.validateTaskContextLocked(old.Workspace, input)
		return validateErr
	})
	if err != nil {
		return result, err
	}
	var dependencies []string
	for id := range staged.dependencies[old.ID] {
		if !core.TaskTerminal(staged.tasks[id].State) {
			dependencies = append(dependencies, id)
		}
	}
	next, titleStats, err := StartOverSuccessor(old, r, secrets)
	if err != nil {
		return result, err
	}
	if _, err := staged.cancelTaskLocked(ctx, core.Intervention{TaskID: old.ID, Action: core.InterventionCancel, ReasonCode: r.Reason, Comment: r.Note, At: next.CreatedAt}); err != nil {
		return result, err
	}
	if err := TaskStartOverFault(ctx, "successor"); err != nil {
		return result, err
	}
	if err := staged.createTaskWithContextLocked(WithTaskTitleRedactions(ctx, titleStats), next, dependencies, attached, pinned); err != nil {
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
	if len(drops) > 0 {
		staged.appendEventLocked(ctx, core.Event{TaskID: next.ID, Kind: TaskStartOverContextDropped, At: next.CreatedAt, Payload: StartOverDroppedContextPayload(drops)})
	}
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
	m.publishStaged(&staged)
	return core.TaskStartOverResult{Task: old, Successor: next, Created: true, DroppedContext: drops}, nil
}
