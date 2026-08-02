package core

import (
	"fmt"
	"strings"
	"time"
)

// LineageNodeType names a durable node class in the Phase 6 knowledge graph
// (spec §4.2 item 4, §16). IDs remain opaque to the graph.
type LineageNodeType string

const (
	LineagePlanningSession    LineageNodeType = "planning_session"
	LineageRequirement        LineageNodeType = "requirement"
	LineageRequirementVersion LineageNodeType = "requirement_version"
	LineageBlueprint          LineageNodeType = "blueprint"
	LineageBlueprintVersion   LineageNodeType = "blueprint_version"
	LineageTask               LineageNodeType = "task"
	LineageWorkOrder          LineageNodeType = "work_order"
	LineagePullRequest        LineageNodeType = "pull_request"
	LineageCommit             LineageNodeType = "commit"
	LineageEvidence           LineageNodeType = "evidence"
	LineageVerdict            LineageNodeType = "verdict"
)

func (nodeType LineageNodeType) Valid() bool {
	switch nodeType {
	case LineagePlanningSession, LineageRequirement, LineageRequirementVersion,
		LineageBlueprint, LineageBlueprintVersion, LineageTask, LineageWorkOrder,
		LineagePullRequest, LineageCommit, LineageEvidence, LineageVerdict:
		return true
	default:
		return false
	}
}

// LineageLink is an immutable, event-provenanced edge. The links table is a
// rebuildable projection; CreatedByEventID identifies the asserting event.
type LineageLink struct {
	Workspace        string          `json:"workspace"`
	SrcType          LineageNodeType `json:"src_type"`
	SrcID            string          `json:"src_id"`
	DstType          LineageNodeType `json:"dst_type"`
	DstID            string          `json:"dst_id"`
	Kind             string          `json:"kind"`
	CreatedByEventID int64           `json:"created_by_event_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

func (link LineageLink) Validate() error {
	if strings.TrimSpace(link.Workspace) == "" || strings.TrimSpace(string(link.SrcType)) == "" ||
		strings.TrimSpace(link.SrcID) == "" || strings.TrimSpace(string(link.DstType)) == "" ||
		strings.TrimSpace(link.DstID) == "" || strings.TrimSpace(link.Kind) == "" {
		return fmt.Errorf("lineage workspace, endpoints, and kind are required")
	}
	if link.CreatedByEventID <= 0 {
		return fmt.Errorf("lineage event provenance is required")
	}
	if !link.SrcType.Valid() || !link.DstType.Valid() {
		return fmt.Errorf("invalid lineage node type %q -> %q", link.SrcType, link.DstType)
	}
	if link.SrcType == link.DstType && link.SrcID == link.DstID {
		return fmt.Errorf("lineage self-links are invalid")
	}
	return nil
}

func BlueprintVersionLineageID(taskID string, version int) string {
	return fmt.Sprintf("%s:v%d", taskID, version)
}

func RequirementVersionLineageID(requirementID string, version int) string {
	return fmt.Sprintf("%s:v%d", requirementID, version)
}

func VerdictLineageID(workOrderID string) string { return "review:" + workOrderID }
