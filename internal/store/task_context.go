package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const (
	TaskContextRequirementAdded   = "task.context_requirement_added"
	TaskContextRequirementActive  = "task.context_requirement_activated"
	TaskContextRequirementRemoved = "task.context_requirement_removed"
	TaskContextDesignAdded        = "task.context_design_added"
	TaskContextDesignActive       = "task.context_design_activated"
	TaskContextDesignRemoved      = "task.context_design_removed"
)

type TaskContextInput struct {
	RequirementIDs []string `json:"requirement_ids,omitempty"`
	DesignIDs      []string `json:"system_design_ids,omitempty"`
}

type TaskContextChange struct {
	Add    TaskContextInput `json:"add"`
	Remove TaskContextInput `json:"remove"`
}

// CheckpointContextCandidate is the minimum read model needed to offer a
// confirmed requirement to an open task paused at an operator checkpoint.
type CheckpointContextCandidate struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	State core.TaskState `json:"state"`
}

type TaskContextReferenceError struct {
	Kind   string
	ID     string
	Reason string
}

func (e *TaskContextReferenceError) Error() string {
	return fmt.Sprintf("%s %s %s", e.Kind, e.ID, e.Reason)
}

func NormalizeTaskContextInput(input TaskContextInput) (TaskContextInput, error) {
	normalize := func(kind string, values []string) ([]string, error) {
		seen := map[string]bool{}
		result := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, &TaskContextReferenceError{Kind: kind, ID: "(empty)", Reason: "is invalid"}
			}
			if !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
		sort.Strings(result)
		return result, nil
	}
	var err error
	input.RequirementIDs, err = normalize("requirement", input.RequirementIDs)
	if err != nil {
		return TaskContextInput{}, err
	}
	input.DesignIDs, err = normalize("system design", input.DesignIDs)
	return input, err
}

// TaskContextForTask folds the append-only attachment audit stream into the
// active read model. Requirement versions remain live; designs retain the
// confirmed version pinned by the add event.
func TaskContextForTask(ctx context.Context, st Store, taskID string) (core.TaskContext, error) {
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		return core.TaskContext{}, err
	}
	return TaskContextFromEvents(ctx, st, events)
}

func TaskContextFromEvents(ctx context.Context, st Store, events []core.Event) (core.TaskContext, error) {
	requirements, designs := ActiveTaskContextReferences(events)
	result := core.TaskContext{Requirements: []core.TaskRequirementContext{}, Designs: []core.TaskDesignContext{}}
	for id := range requirements {
		document, getErr := st.GetRequirement(ctx, id)
		if getErr != nil || document.CurrentVersion <= 0 {
			continue
		}
		result.Requirements = append(result.Requirements, core.TaskRequirementContext{ID: id, Title: document.Title, Version: document.CurrentVersion})
	}
	for id, version := range designs {
		document, getErr := st.GetSystemDesign(ctx, id)
		if getErr != nil {
			continue
		}
		result.Designs = append(result.Designs, core.TaskDesignContext{ID: id, Title: document.Title, Version: version})
	}
	sort.Slice(result.Requirements, func(i, j int) bool { return result.Requirements[i].ID < result.Requirements[j].ID })
	sort.Slice(result.Designs, func(i, j int) bool { return result.Designs[i].ID < result.Designs[j].ID })
	return result, nil
}

// ActiveTaskContextReferences folds task context events without store reads so
// transactional writers can carry a design attachment's pinned version onto
// its removal event.
func ActiveTaskContextReferences(events []core.Event) (map[string]bool, map[string]int) {
	requirements := map[string]bool{}
	designs := map[string]int{}
	for _, event := range events {
		var payload struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		switch event.Kind {
		case TaskContextRequirementAdded:
			requirements[payload.ID] = true
		case TaskContextRequirementRemoved:
			delete(requirements, payload.ID)
		case TaskContextDesignAdded:
			designs[payload.ID] = payload.Version
		case TaskContextDesignRemoved:
			delete(designs, payload.ID)
		}
	}
	return requirements, designs
}

// AdvancedTaskContextDesignPins returns active design attachments whose
// current confirmed version is newer than the folded pin. Callers append a
// TaskContextDesignAdded event for each result at an implement queue-entry
// boundary; equal pins and pins ahead of confirmed authority are retained.
func AdvancedTaskContextDesignPins(pinned, confirmed map[string]int) []core.TaskDesignContext {
	advanced := make([]core.TaskDesignContext, 0)
	for id, pinnedVersion := range pinned {
		if confirmedVersion := confirmed[id]; confirmedVersion > pinnedVersion {
			advanced = append(advanced, core.TaskDesignContext{ID: id, Version: confirmedVersion})
		}
	}
	sort.Slice(advanced, func(i, j int) bool { return advanced[i].ID < advanced[j].ID })
	return advanced
}

// PendingTaskContextAttachment reports whether the latest add/remove state for
// one document is an unconfirmed attachment to the exact proposed version.
// Confirmation uses this fold before emitting the activation event that owns
// the eventual serves/governs edge.
func PendingTaskContextAttachment(events []core.Event, id string, version int, design bool) bool {
	active, pendingVersion, unconfirmed := false, 0, false
	for _, event := range events {
		var payload struct {
			ID             string `json:"id"`
			Version        int    `json:"version"`
			PendingVersion int    `json:"pending_version"`
			Unconfirmed    bool   `json:"unconfirmed"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.ID != id {
			continue
		}
		if design {
			switch event.Kind {
			case TaskContextDesignAdded:
				active, pendingVersion, unconfirmed = true, payload.Version, payload.Unconfirmed
			case TaskContextDesignRemoved:
				active, pendingVersion, unconfirmed = false, 0, false
			}
			continue
		}
		switch event.Kind {
		case TaskContextRequirementAdded:
			active, pendingVersion, unconfirmed = true, payload.PendingVersion, payload.Unconfirmed
		case TaskContextRequirementRemoved:
			active, pendingVersion, unconfirmed = false, 0, false
		}
	}
	return active && unconfirmed && pendingVersion == version
}

func (m *memory) activatePendingRequirementContextLocked(ctx context.Context, workspace, requirementID string, version int) {
	for taskID, task := range m.tasks {
		if task.Workspace != workspace || !PendingTaskContextAttachment(m.events[taskID], requirementID, version, false) {
			continue
		}
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextRequirementActive,
			Payload: core.JSONPayload(map[string]any{"id": requirementID, "version": version})})
	}
}

func (m *memory) activatePendingDesignContextLocked(ctx context.Context, workspace, documentID string, version int) {
	for taskID, task := range m.tasks {
		if task.Workspace != workspace || !PendingTaskContextAttachment(m.events[taskID], documentID, version, true) {
			continue
		}
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignActive,
			Payload: core.JSONPayload(map[string]any{"id": documentID, "version": version})})
	}
}
