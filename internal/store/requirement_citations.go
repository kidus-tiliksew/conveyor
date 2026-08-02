package store

import (
	"context"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ServedRequirementsForTask resolves current confirmed requirement authority
// through the bounded canonical graph used for model context (spec §4.2 item
// 4; REQ-4). Unconfirmed and merely proposed serves relations project no edge.
func ServedRequirementsForTask(ctx context.Context, st Store, taskID string) ([]core.ServedRequirementContext, error) {
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
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
			return nil, getErr
		}
		if requirement.CurrentVersion <= 0 {
			continue
		}
		version, getErr := st.GetRequirementVersion(ctx, requirement.ID, requirement.CurrentVersion)
		if getErr != nil {
			return nil, getErr
		}
		if !version.Confirmed {
			continue
		}
		result = append(result, core.ServedRequirementContext{ID: requirement.ID, Title: requirement.Title, Version: version.Version, Statements: append([]core.RequirementStatement(nil), version.Statements...)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
