package core

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	DefaultContextArtifactMaxRefs        = 64
	ContextArtifactProtectedTierMaxRefs  = 16
	DefaultContextArtifactTraversalDepth = 5
	DefaultContextArtifactTraversalNodes = 32
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
	// LegacyCreatedByEvent records the pre-event-ledger provenance retained by
	// feature migration. Only historical_feature_assignment may use it.
	LegacyCreatedByEvent string    `json:"legacy_created_by_event,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// LineageRebuildResult reports projection reconciliation without treating
// retained, non-derived legacy links as event-derived graph state.
type LineageRebuildResult struct {
	Projected int `json:"projected"` // distinct canonical keys regenerated from eligible events
	Existing  int `json:"existing"`  // retained non-projector rows whose keys were not regenerated
	// PreservedUnregenerable counts projector-owned event-provenanced rows for
	// which the current replay cannot derive a replacement.
	PreservedUnregenerable int `json:"preserved_unregenerable"`
	Unsupported            int `json:"unsupported"` // structurally invalid event-derived candidates
	Ambiguous              int `json:"ambiguous"`   // keys with multiple eligible candidate events
}

type LineageRebuildRequest struct {
	Reason    string `json:"reason"`
	RequestID string `json:"request_id"`
}

// LineageNode identifies one endpoint without implying a persisted record of
// its own. Traversal is over event-provenanced links; nodes are only the
// bounded read model needed to assemble context (spec §4.2 item 4).
type LineageNode struct {
	Type  LineageNodeType `json:"type"`
	ID    string          `json:"id"`
	Label string          `json:"label,omitempty"`
}

func (node LineageNode) Valid() bool {
	return node.Type.Valid() && strings.TrimSpace(node.ID) != ""
}

// LineageTraversalBudget makes graph reads fail closed against an unbounded
// workspace graph. MaxDepth counts edges from the root; MaxNodes includes the
// root and is a hard cap on returned nodes (spec §4.2 item 4, §15.1).
type LineageTraversalBudget struct {
	MaxDepth int `json:"max_depth"`
	MaxNodes int `json:"max_nodes"`
	MaxLinks int `json:"max_links,omitempty"`
	// Workspace makes the trust boundary explicit even when a caller supplies
	// mixed link input. Empty preserves compatibility for pure in-memory walks.
	Workspace string `json:"-"`
}

// LineageTraversal is deterministic for the same roots, links, and budget.
// Truncated reports that a reachable node was omitted by either bound.
type LineageTraversal struct {
	Roots                        []LineageNode                 `json:"roots"`
	Nodes                        []LineageNode                 `json:"nodes"`
	Links                        []LineageLink                 `json:"links"`
	Truncated                    bool                          `json:"truncated"`
	Budget                       LineageTraversalBudget        `json:"budget"`
	OmittedNodes                 int                           `json:"omitted_nodes,omitempty"`
	OmittedLinks                 int                           `json:"omitted_links,omitempty"`
	ExhaustionReasons            []string                      `json:"exhaustion_reasons,omitempty"`
	ForeignWorkspaceLinksIgnored int                           `json:"foreign_workspace_links_ignored,omitempty"`
	Depths                       map[LineageNode]int           `json:"-"`
	Paths                        map[LineageNode][]LineageLink `json:"-"`
}

// ContextArtifactSelection is the common bounded artifact view used by both
// MCP work-order delivery and in-process pipeline input. Keeping selection in
// one pure domain function prevents those context paths from granting
// different reachability (spec §4.2 item 4).
type ContextArtifactSelection struct {
	Nodes     []LineageNode
	Artifacts []Artifact
	Truncated bool
	Omitted   int
}

type ContextArtifactSelectionOptions struct {
	Workspace                        string
	LocalTaskID                      string
	IncludeLocalVerificationEvidence bool
	Budget                           LineageTraversalBudget
	MaxArtifactRefs                  int
}

type contextArtifactCandidate struct {
	artifact Artifact
	local    bool
	evidence bool
	depth    int
}

func SelectContextArtifacts(links []LineageLink, roots []LineageNode, artifacts []Artifact, options ...ContextArtifactSelectionOptions) (ContextArtifactSelection, error) {
	var opts ContextArtifactSelectionOptions
	if len(options) > 0 {
		opts = options[0]
	}
	budget := opts.Budget
	if budget.MaxDepth == 0 && budget.MaxNodes == 0 {
		budget.MaxDepth, budget.MaxNodes = DefaultContextArtifactTraversalDepth, DefaultContextArtifactTraversalNodes
	}
	budget.Workspace = opts.Workspace
	traversal, err := TraverseLineage(links, roots, budget)
	if err != nil {
		return ContextArtifactSelection{}, err
	}
	reachable := make(map[LineageNode]bool, len(traversal.Nodes))
	for _, node := range traversal.Nodes {
		reachable[node] = true
	}
	byID := map[string]contextArtifactCandidate{}
	for _, artifact := range artifacts {
		local := opts.LocalTaskID != "" && artifact.TaskID == opts.LocalTaskID
		if (!artifact.Role.ModelInputEligible() && !(opts.IncludeLocalVerificationEvidence && local && artifact.EligibleVerificationEvidence())) || !artifactReachableFromContext(artifact, reachable) {
			continue
		}
		depth := artifactContextDepth(artifact, traversal.Depths)
		item := contextArtifactCandidate{artifact: artifact, local: local, evidence: local && artifact.EligibleVerificationEvidence(), depth: depth}
		prior, exists := byID[artifact.ID]
		if !exists || contextArtifactCandidateLess(item, prior) {
			byID[artifact.ID] = item
		}
	}
	ordered := make([]contextArtifactCandidate, 0, len(byID))
	for _, item := range byID {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool { return contextArtifactCandidateLess(ordered[i], ordered[j]) })
	selection := ContextArtifactSelection{
		Nodes:     append([]LineageNode(nil), traversal.Nodes...),
		Artifacts: make([]Artifact, 0, len(ordered)),
		Truncated: traversal.Truncated,
	}
	maxRefs := opts.MaxArtifactRefs
	if maxRefs <= 0 {
		maxRefs = DefaultContextArtifactMaxRefs
	}
	// Reserve a bounded share for each protected local tier before filling the
	// remaining global budget. This prevents high evidence fan-out from evicting
	// ordinary task-local context (and vice versa) without making either tier
	// an unbounded authorization escape hatch.
	protectedAllowance := maxRefs / 4
	if protectedAllowance < 1 {
		protectedAllowance = 1
	}
	if protectedAllowance > ContextArtifactProtectedTierMaxRefs {
		protectedAllowance = ContextArtifactProtectedTierMaxRefs
	}
	selected := make(map[string]bool, maxRefs)
	appendTier := func(match func(contextArtifactCandidate) bool) {
		added := 0
		for _, item := range ordered {
			if len(selection.Artifacts) >= maxRefs || added >= protectedAllowance || selected[item.artifact.ID] || !match(item) {
				continue
			}
			selection.Artifacts = append(selection.Artifacts, item.artifact)
			selected[item.artifact.ID] = true
			added++
		}
	}
	appendTier(func(item contextArtifactCandidate) bool { return item.evidence })
	appendTier(func(item contextArtifactCandidate) bool { return item.local && !item.evidence })
	for _, item := range ordered {
		if selected[item.artifact.ID] {
			continue
		}
		if len(selection.Artifacts) < maxRefs {
			selection.Artifacts = append(selection.Artifacts, item.artifact)
			selected[item.artifact.ID] = true
		}
	}
	selection.Omitted = len(ordered) - len(selection.Artifacts)
	selection.Truncated = selection.Truncated || selection.Omitted > 0
	return selection, nil
}

func contextArtifactCandidateLess(left, right contextArtifactCandidate) bool {
	if left.evidence != right.evidence {
		return left.evidence
	}
	if left.local != right.local {
		return left.local
	}
	if left.depth != right.depth {
		return left.depth < right.depth
	}
	if !left.artifact.CreatedAt.Equal(right.artifact.CreatedAt) {
		return left.artifact.CreatedAt.After(right.artifact.CreatedAt)
	}
	return strings.Join([]string{left.artifact.ID, string(left.artifact.Role), left.artifact.TaskID, left.artifact.RequirementID, left.artifact.PlanningSessionID}, "\x00") <
		strings.Join([]string{right.artifact.ID, string(right.artifact.Role), right.artifact.TaskID, right.artifact.RequirementID, right.artifact.PlanningSessionID}, "\x00")
}

func artifactContextDepth(artifact Artifact, depths map[LineageNode]int) int {
	best := int(^uint(0) >> 1)
	for _, node := range []LineageNode{{Type: LineageTask, ID: artifact.TaskID}, {Type: LineageRequirement, ID: artifact.RequirementID}, {Type: LineagePlanningSession, ID: artifact.PlanningSessionID}, {Type: LineageEvidence, ID: artifact.ID}} {
		if node.ID != "" {
			if depth, ok := depths[node]; ok && depth < best {
				best = depth
			}
		}
	}
	return best
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
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks < 0 {
		return LineageTraversal{}, fmt.Errorf("lineage traversal requires max depth >= 0 and max nodes > 0")
	}
	type neighbor struct {
		node LineageNode
		link LineageLink
	}
	adjacent := map[LineageNode][]neighbor{}
	for _, link := range links {
		if budget.Workspace != "" && link.Workspace != budget.Workspace {
			continue
		}
		if link.Validate() != nil {
			continue
		}
		src := LineageNode{Type: link.SrcType, ID: link.SrcID}
		dst := LineageNode{Type: link.DstType, ID: link.DstID}
		adjacent[src] = append(adjacent[src], neighbor{node: dst, link: link})
		adjacent[dst] = append(adjacent[dst], neighbor{node: src, link: link})
	}
	for node := range adjacent {
		sort.Slice(adjacent[node], func(i, j int) bool {
			left, right := adjacent[node][i], adjacent[node][j]
			if lineageRelationPriority(left.link.Kind) != lineageRelationPriority(right.link.Kind) {
				return lineageRelationPriority(left.link.Kind) < lineageRelationPriority(right.link.Kind)
			}
			if !left.link.CreatedAt.Equal(right.link.CreatedAt) {
				return left.link.CreatedAt.Before(right.link.CreatedAt)
			}
			if left.link.CreatedByEventID != right.link.CreatedByEventID {
				return left.link.CreatedByEventID < right.link.CreatedByEventID
			}
			return lineageTraversalNodeKey(left.node) < lineageTraversalNodeKey(right.node)
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
		Roots:  []LineageNode{},
		Nodes:  make([]LineageNode, 0, budget.MaxNodes),
		Links:  []LineageLink{},
		Depths: map[LineageNode]int{},
		Paths:  map[LineageNode][]LineageLink{},
		Budget: budget,
	}
	result.Budget.Workspace = ""
	omittedNodes := map[LineageNode]bool{}
	reasons := map[string]bool{}
	if budget.Workspace != "" {
		for _, link := range links {
			if link.Workspace != budget.Workspace {
				result.ForeignWorkspaceLinksIgnored++
			}
		}
	}
	for _, root := range sortedRoots {
		if !root.Valid() || seen[root] {
			continue
		}
		if len(result.Nodes) == budget.MaxNodes {
			result.Truncated = true
			omittedNodes[root] = true
			reasons["nodes"] = true
			break
		}
		seen[root] = true
		result.Roots = append(result.Roots, root)
		result.Nodes = append(result.Nodes, root)
		result.Depths[root] = 0
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
					omittedNodes[next.node] = true
					reasons["depth"] = true
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
				omittedNodes[next.node] = true
				reasons["nodes"] = true
				continue
			}
			seen[next.node] = true
			result.Nodes = append(result.Nodes, next.node)
			result.Depths[next.node] = current.depth + 1
			result.Paths[next.node] = append(append([]LineageLink(nil), result.Paths[current.node]...), next.link)
			queue = append(queue, queuedNode{node: next.node, depth: current.depth + 1})
		}
	}
	reachable := make(map[LineageNode]bool, len(result.Nodes))
	for _, node := range result.Nodes {
		reachable[node] = true
	}
	for _, link := range links {
		if (budget.Workspace != "" && link.Workspace != budget.Workspace) || link.Validate() != nil || !reachable[LineageNode{Type: link.SrcType, ID: link.SrcID}] ||
			!reachable[LineageNode{Type: link.DstType, ID: link.DstID}] {
			continue
		}
		result.Links = append(result.Links, link)
	}
	sort.Slice(result.Links, func(i, j int) bool {
		left, right := result.Links[i], result.Links[j]
		if left.CreatedByEventID != right.CreatedByEventID {
			return left.CreatedByEventID < right.CreatedByEventID
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return lineageTraversalLinkKey(left) < lineageTraversalLinkKey(right)
	})
	if budget.MaxLinks > 0 && len(result.Links) > budget.MaxLinks {
		result.OmittedLinks = len(result.Links) - budget.MaxLinks
		result.Links = result.Links[:budget.MaxLinks]
		result.Truncated = true
		reasons["links"] = true
	}
	result.OmittedNodes = len(omittedNodes)
	for _, reason := range []string{"depth", "nodes", "links"} {
		if reasons[reason] {
			result.ExhaustionReasons = append(result.ExhaustionReasons, reason)
		}
	}
	return result, nil
}

// lineageRelationPriority is the graph-walk half of the context priority
// contract. Item-type ranking is applied again after content is rendered.
func lineageRelationPriority(kind string) int {
	switch kind {
	case "serves":
		return 0
	case "materializes", "supersedes":
		return 1
	case "depends_on":
		return 2
	case "produced_verdict", "merged_range", "submitted_range", "submitted_as":
		return 3
	case "supports", "proved_by":
		return 4
	default:
		return 5
	}
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
	if link.CreatedByEventID <= 0 && !(link.Kind == "historical_feature_assignment" && strings.TrimSpace(link.LegacyCreatedByEvent) != "") {
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
