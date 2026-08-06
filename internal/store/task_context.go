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
	TaskContextRequirementRemoved = "task.context_requirement_removed"
	TaskContextDesignAdded        = "task.context_design_added"
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
