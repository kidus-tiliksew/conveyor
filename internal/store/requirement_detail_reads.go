package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RequirementDetailReadStore serves the requirement detail read model with a
// fixed number of set reads, whatever the number of tasks it names, instead
// of one full task or plan read per task (component-persistence;
// component-document-corpus). Each method reads the workspace from the
// context, deduplicates the requested IDs, and returns an empty map without
// a statement for an empty selection. A requested ID outside the workspace,
// or one that names no task, is absent from the result.
type RequirementDetailReadStore interface {
	// ListRequirementDetailTasks returns each requested task's ID, title,
	// state, and whether another task of the workspace names it as its
	// parent. It reads no body, relation, assignee, or GitHub lifecycle.
	ListRequirementDetailTasks(ctx context.Context, taskIDs []string) (map[string]RequirementDetailTask, error)
	// ListLatestPlanSummaries returns the version number and approval flag
	// of each requested task's latest plan version, the version
	// GetLatestSpecVersion returns. A task without a plan is absent. It
	// reads no plan content.
	ListLatestPlanSummaries(ctx context.Context, taskIDs []string) (map[string]PlanSummary, error)
}

// RequirementDetailTask is the compact task fact the requirement detail
// labels and filters by. HasChildren is core.BlueprintAnchor of the full
// task.
type RequirementDetailTask struct {
	ID          string
	Title       string
	State       core.TaskState
	HasChildren bool
}

// PlanSummary is the compact fact of one plan version.
type PlanSummary struct {
	Version  int
	Approved bool
}

// UniqueTaskIDs returns ids without repeats, in first-seen order. Durable
// backends bind the result as one set argument.
func UniqueTaskIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (m *memory) ListRequirementDetailTasks(ctx context.Context, taskIDs []string) (map[string]RequirementDetailTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string]RequirementDetailTask{}
	for _, id := range UniqueTaskIDs(taskIDs) {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		hasChildren := false
		for _, child := range m.tasks {
			if child.ParentTaskID == task.ID && child.Workspace == task.Workspace {
				hasChildren = true
				break
			}
		}
		out[id] = RequirementDetailTask{ID: task.ID, Title: task.Title, State: task.State, HasChildren: hasChildren}
	}
	return out, nil
}

func (m *memory) ListLatestPlanSummaries(ctx context.Context, taskIDs []string) (map[string]PlanSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string]PlanSummary{}
	for _, id := range UniqueTaskIDs(taskIDs) {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		if versions := m.specs[id]; len(versions) > 0 {
			latest := versions[len(versions)-1]
			out[id] = PlanSummary{Version: latest.Version, Approved: latest.Approved}
		}
	}
	return out, nil
}
