package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const ServedRequirementAuthorityMaxNodes = 256

type ServedRequirementsResult struct {
	Requirements []core.ServedRequirementContext
	Truncated    bool
	Omitted      int
}

// ServedRequirementsForTask resolves current confirmed requirement authority
// through the bounded canonical graph used for model context (spec §4.2 item
// 4; REQ-4). Unconfirmed and merely proposed serves relations project no edge.
func ServedRequirementsForTask(ctx context.Context, st Store, taskID string) (ServedRequirementsResult, error) {
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return ServedRequirementsResult{}, err
	}
	roots := []core.LineageNode{{Type: core.LineageBlueprint, ID: task.ID}}
	if task.ParentTaskID != "" {
		roots = append(roots, core.LineageNode{Type: core.LineageBlueprint, ID: task.ParentTaskID})
	}
	workspace, _ := WorkspaceFromContext(ctx)
	budget := core.LineageTraversalBudget{MaxDepth: 1, MaxNodes: ServedRequirementAuthorityMaxNodes, MaxLinks: ServedRequirementAuthorityMaxNodes * 4, Workspace: workspace}
	fetchBudget := budget
	fetchBudget.MaxNodes++
	fetchBudget.MaxLinks++
	links, err := st.ListLineageNeighborhood(ctx, roots, fetchBudget)
	if err != nil {
		return ServedRequirementsResult{}, err
	}
	walk, err := core.TraverseLineage(links, roots, budget)
	if err != nil {
		return ServedRequirementsResult{}, err
	}
	authorityTruncated := false
	for _, reason := range walk.ExhaustionReasons {
		authorityTruncated = authorityTruncated || reason == "nodes" || reason == "links"
	}
	if authorityTruncated {
		return ServedRequirementsResult{Truncated: true, Omitted: walk.OmittedNodes + walk.OmittedLinks}, fmt.Errorf("served requirement authority for task %s is truncated", taskID)
	}
	governingBlueprints := map[string]bool{task.ID: true}
	if task.ParentTaskID != "" {
		governingBlueprints[task.ParentTaskID] = true
	}
	seen := map[string]bool{}
	result := []core.ServedRequirementContext{}
	for _, link := range links {
		if link.Kind != "serves" || link.SrcType != core.LineageRequirement ||
			link.DstType != core.LineageBlueprint || !governingBlueprints[link.DstID] || seen[link.SrcID] {
			continue
		}
		seen[link.SrcID] = true
		requirement, getErr := st.GetRequirement(ctx, link.SrcID)
		if getErr != nil {
			return ServedRequirementsResult{}, getErr
		}
		if requirement.CurrentVersion <= 0 {
			continue
		}
		version, getErr := st.GetRequirementVersion(ctx, requirement.ID, requirement.CurrentVersion)
		if getErr != nil {
			return ServedRequirementsResult{}, getErr
		}
		if !version.Confirmed {
			continue
		}
		result = append(result, core.ServedRequirementContext{ID: requirement.ID, Title: requirement.Title, Version: version.Version, Statements: append([]core.RequirementStatement(nil), version.Statements...)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return ServedRequirementsResult{Requirements: result}, nil
}
