package core

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// Context traversal is deliberately small and deterministic. Six edges
	// reach from a work order or task through its parent blueprint to served
	// requirements, sibling outcomes, and adjacent evidence without allowing a
	// connected workspace graph to become an unbounded prompt (spec §4.2 item 4).
	ContextLineageMaxDepth = 6
	ContextLineageMaxNodes = 128
	ContextArtifactMaxRefs = 64
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
	LineageCommitRange        LineageNodeType = "commit_range"
	LineageEvidence           LineageNodeType = "evidence"
	LineageVerdict            LineageNodeType = "verdict"
)

func (nodeType LineageNodeType) Valid() bool {
	switch nodeType {
	case LineagePlanningSession, LineageRequirement, LineageRequirementVersion,
		LineageBlueprint, LineageBlueprintVersion, LineageTask, LineageWorkOrder,
		LineagePullRequest, LineageCommitRange, LineageEvidence, LineageVerdict:
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

// LineageNode identifies one endpoint without implying a persisted record of
// its own. Traversal is over event-provenanced links; nodes are only the
// bounded read model needed to assemble context (spec §4.2 item 4).
type LineageNode struct {
	Type LineageNodeType `json:"type"`
	ID   string          `json:"id"`
}

func (node LineageNode) Valid() bool {
	return node.Type.Valid() && strings.TrimSpace(node.ID) != ""
}

// LineageTraversalBudget makes graph reads fail closed against an unbounded
// workspace graph. MaxDepth counts edges from the root; MaxNodes includes the
// root and is a hard cap on returned nodes (spec §4.2 item 4, §15.1).
type LineageTraversalBudget struct {
	MaxDepth int
	MaxNodes int
}

// LineageTraversal is deterministic for the same roots, links, and budget.
// Truncated reports that a reachable node was omitted by either bound.
type LineageTraversal struct {
	Roots     []LineageNode `json:"roots"`
	Nodes     []LineageNode `json:"nodes"`
	Links     []LineageLink `json:"links"`
	Truncated bool          `json:"truncated"`
}

// ContextArtifactSelection is the common bounded artifact view used by both
// MCP work-order delivery and in-process pipeline input. Keeping selection in
// one pure domain function prevents those context paths from granting
// different reachability (spec §4.2 item 4).
type ContextArtifactSelection struct {
	Nodes     []LineageNode
	Artifacts []Artifact
	Truncated bool
}

func SelectContextArtifacts(links []LineageLink, roots []LineageNode, artifacts []Artifact) (ContextArtifactSelection, error) {
	traversal, err := TraverseLineage(links, roots, LineageTraversalBudget{
		MaxDepth: ContextLineageMaxDepth,
		MaxNodes: ContextLineageMaxNodes,
	})
	if err != nil {
		return ContextArtifactSelection{}, err
	}
	reachable := make(map[LineageNode]bool, len(traversal.Nodes))
	for _, node := range traversal.Nodes {
		reachable[node] = true
	}
	ordered := append([]Artifact(nil), artifacts...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		left := strings.Join([]string{ordered[i].ID, string(ordered[i].Role), ordered[i].TaskID, ordered[i].RequirementID, ordered[i].PlanningSessionID}, "\x00")
		right := strings.Join([]string{ordered[j].ID, string(ordered[j].Role), ordered[j].TaskID, ordered[j].RequirementID, ordered[j].PlanningSessionID}, "\x00")
		return left < right
	})
	selection := ContextArtifactSelection{
		Nodes:     append([]LineageNode(nil), traversal.Nodes...),
		Artifacts: make([]Artifact, 0, min(len(ordered), ContextArtifactMaxRefs)),
		Truncated: traversal.Truncated,
	}
	for _, artifact := range ordered {
		if !artifactReachableFromContext(artifact, reachable) {
			continue
		}
		if len(selection.Artifacts) == ContextArtifactMaxRefs {
			selection.Truncated = true
			continue
		}
		selection.Artifacts = append(selection.Artifacts, artifact)
	}
	return selection, nil
}

func artifactReachableFromContext(artifact Artifact, reachable map[LineageNode]bool) bool {
	for _, node := range []LineageNode{
		{Type: LineageTask, ID: artifact.TaskID},
		{Type: LineageRequirement, ID: artifact.RequirementID},
		{Type: LineagePlanningSession, ID: artifact.PlanningSessionID},
	} {
		if node.ID != "" && reachable[node] {
			return true
		}
	}
	return artifact.EligibleVerificationEvidence() && reachable[LineageNode{Type: LineageEvidence, ID: artifact.ID}]
}

// TraverseLineage walks links in both directions: lineage edges retain their
// semantic direction, while context assembly needs ancestors and descendants
// around the work-order root. Invalid links are ignored rather than granting
// reachability through malformed projection data.
func TraverseLineage(links []LineageLink, roots []LineageNode, budget LineageTraversalBudget) (LineageTraversal, error) {
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 {
		return LineageTraversal{}, fmt.Errorf("lineage traversal requires max depth >= 0 and max nodes > 0")
	}
	type neighbor struct {
		node LineageNode
		key  string
	}
	adjacent := map[LineageNode][]neighbor{}
	for _, link := range links {
		if link.Validate() != nil {
			continue
		}
		src := LineageNode{Type: link.SrcType, ID: link.SrcID}
		dst := LineageNode{Type: link.DstType, ID: link.DstID}
		key := lineageTraversalLinkKey(link)
		adjacent[src] = append(adjacent[src], neighbor{node: dst, key: key})
		adjacent[dst] = append(adjacent[dst], neighbor{node: src, key: key})
	}
	for node := range adjacent {
		sort.Slice(adjacent[node], func(i, j int) bool {
			if adjacent[node][i].key != adjacent[node][j].key {
				return adjacent[node][i].key < adjacent[node][j].key
			}
			return lineageTraversalNodeKey(adjacent[node][i].node) < lineageTraversalNodeKey(adjacent[node][j].node)
		})
	}

	sortedRoots := append([]LineageNode(nil), roots...)
	sort.Slice(sortedRoots, func(i, j int) bool {
		return lineageTraversalNodeKey(sortedRoots[i]) < lineageTraversalNodeKey(sortedRoots[j])
	})
	type queuedNode struct {
		node  LineageNode
		depth int
	}
	queue := make([]queuedNode, 0, budget.MaxNodes)
	seen := map[LineageNode]bool{}
	result := LineageTraversal{
		Roots: []LineageNode{},
		Nodes: make([]LineageNode, 0, budget.MaxNodes),
		Links: []LineageLink{},
	}
	for _, root := range sortedRoots {
		if !root.Valid() || seen[root] {
			continue
		}
		if len(result.Nodes) == budget.MaxNodes {
			result.Truncated = true
			break
		}
		seen[root] = true
		result.Roots = append(result.Roots, root)
		result.Nodes = append(result.Nodes, root)
		queue = append(queue, queuedNode{node: root})
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		neighbors := adjacent[current.node]
		if current.depth == budget.MaxDepth {
			for _, next := range neighbors {
				if !seen[next.node] {
					result.Truncated = true
					break
				}
			}
			continue
		}
		for _, next := range neighbors {
			if seen[next.node] {
				continue
			}
			if len(result.Nodes) == budget.MaxNodes {
				result.Truncated = true
				continue
			}
			seen[next.node] = true
			result.Nodes = append(result.Nodes, next.node)
			queue = append(queue, queuedNode{node: next.node, depth: current.depth + 1})
		}
	}
	reachable := make(map[LineageNode]bool, len(result.Nodes))
	for _, node := range result.Nodes {
		reachable[node] = true
	}
	for _, link := range links {
		if link.Validate() != nil || !reachable[LineageNode{Type: link.SrcType, ID: link.SrcID}] ||
			!reachable[LineageNode{Type: link.DstType, ID: link.DstID}] {
			continue
		}
		result.Links = append(result.Links, link)
	}
	sort.Slice(result.Links, func(i, j int) bool {
		left, right := lineageTraversalLinkKey(result.Links[i]), lineageTraversalLinkKey(result.Links[j])
		if left != right {
			return left < right
		}
		return result.Links[i].CreatedByEventID < result.Links[j].CreatedByEventID
	})
	return result, nil
}

func lineageTraversalNodeKey(node LineageNode) string {
	return string(node.Type) + "\x00" + node.ID
}

func lineageTraversalLinkKey(link LineageLink) string {
	return strings.Join([]string{string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind}, "\x00")
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

// PullRequestLineageID and CommitRangeLineageID document the stable forge/git
// identities written into lifecycle events. A range is asserted only when
// both immutable endpoints are known (spec §4.2 item 4).
func PullRequestLineageID(repository string, number int) string {
	return fmt.Sprintf("%s#%d", strings.TrimSpace(repository), number)
}

func CommitRangeLineageID(repository, baseSHA, headSHA string) string {
	return fmt.Sprintf("%s@%s..%s", strings.TrimSpace(repository), strings.TrimSpace(baseSHA), strings.TrimSpace(headSHA))
}

func VerdictLineageID(workOrderID string) string { return "review:" + workOrderID }
