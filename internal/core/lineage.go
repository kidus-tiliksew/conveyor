package core

import (
	"context"
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

// LineageNodeType names a durable node class in the knowledge graph.
// IDs remain opaque to the graph (component-lineage).
type LineageNodeType string

const (
	LineagePlanningSession          LineageNodeType = "planning_session"
	LineagePlanningBundle           LineageNodeType = "planning_bundle"
	LineageRequirement              LineageNodeType = "requirement"
	LineageRequirementVersion       LineageNodeType = "requirement_version"
	LineageReferenceDocument        LineageNodeType = "reference_document"
	LineageReferenceDocumentVersion LineageNodeType = "reference_document_version"
	LineageSystemDesign             LineageNodeType = "system_design"
	LineageSystemDesignVersion      LineageNodeType = "system_design_version"
	LineageDecision                 LineageNodeType = "decision"
	LineageRepositoryPath           LineageNodeType = "repository_path"
	LineageBlueprint                LineageNodeType = "blueprint"
	LineageBlueprintVersion         LineageNodeType = "blueprint_version"
	LineageTask                     LineageNodeType = "task"
	LineageWorkOrder                LineageNodeType = "work_order"
	LineagePullRequest              LineageNodeType = "pull_request"
	LineageCommitRange              LineageNodeType = "commit_range"
	LineageEvidence                 LineageNodeType = "evidence"
	LineageVerdict                  LineageNodeType = "verdict"
)

func (nodeType LineageNodeType) Valid() bool {
	switch nodeType {
	case LineagePlanningSession, LineagePlanningBundle, LineageRequirement, LineageRequirementVersion, LineageReferenceDocument, LineageReferenceDocumentVersion,
		LineageSystemDesign, LineageSystemDesignVersion, LineageDecision, LineageRepositoryPath,
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
// bounded read model needed to assemble context (component-lineage).
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
// root and is a hard cap on returned nodes (component-lineage).
type LineageTraversalBudget struct {
	MaxDepth int `json:"max_depth"`
	MaxNodes int `json:"max_nodes"`
	MaxLinks int `json:"max_links,omitempty"`
	// MaxFanout caps the ranked links each non-root node examines; a root
	// examines up to the larger of MaxNodes and MaxFanout. Zero takes the
	// Normalized default.
	MaxFanout int `json:"max_fanout,omitempty"`
	// Workspace makes the trust boundary explicit even when a caller supplies
	// mixed link input. Empty preserves compatibility for pure in-memory walks.
	Workspace string `json:"-"`
	// Filter narrows which examined links the walk follows. Nil walks every
	// link in both directions, as before (component-lineage).
	Filter *LineageTraversalFilter `json:"filter,omitempty"`
}

// LineageDirection names which side of a link the walk leaves a node by.
type LineageDirection string

const (
	LineageDirectionBoth LineageDirection = "both"
	LineageDirectionOut  LineageDirection = "out"
	LineageDirectionIn   LineageDirection = "in"
)

func (direction LineageDirection) Valid() bool {
	switch direction {
	case "", LineageDirectionBoth, LineageDirectionOut, LineageDirectionIn:
		return true
	default:
		return false
	}
}

// LineageTraversalFilter selects among the ranked link prefix each node
// examines. A link the filter refuses still counts against that node's
// fan-out, so a filter narrows a walk without widening its store reads or
// changing its ranking (component-lineage).
type LineageTraversalFilter struct {
	// Direction out follows a link only from its source to its destination,
	// in only from its destination to its source. Empty means both.
	Direction LineageDirection `json:"direction,omitempty"`
	// Kinds, when set, admits only links of these kinds.
	Kinds []string `json:"kinds,omitempty"`
	// NodeTypes, when set, admits only neighbors of these types. Roots are
	// always admitted.
	NodeTypes []LineageNodeType `json:"node_types,omitempty"`
}

// admitsKind reports whether the filter keeps a link of kind.
func (filter *LineageTraversalFilter) admitsKind(kind string) bool {
	if filter == nil || len(filter.Kinds) == 0 {
		return true
	}
	for _, candidate := range filter.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// admits reports whether the walk may leave node along link to far.
func (filter *LineageTraversalFilter) admits(node, far LineageNode, link LineageLink) bool {
	if filter == nil {
		return true
	}
	switch filter.Direction {
	case LineageDirectionOut:
		if link.SrcType != node.Type || link.SrcID != node.ID {
			return false
		}
	case LineageDirectionIn:
		if link.DstType != node.Type || link.DstID != node.ID {
			return false
		}
	}
	if !filter.admitsKind(link.Kind) {
		return false
	}
	if len(filter.NodeTypes) == 0 {
		return true
	}
	for _, nodeType := range filter.NodeTypes {
		if nodeType == far.Type {
			return true
		}
	}
	return false
}

// LineageFanoutFloor is the smallest default per-node fan-out.
const LineageFanoutFloor = 4

// Normalized fills the default fan-out: a quarter of MaxNodes, never less
// than LineageFanoutFloor. A hub therefore admits at most its fan-out instead
// of the whole node budget (component-lineage).
func (budget LineageTraversalBudget) Normalized() LineageTraversalBudget {
	if budget.MaxFanout <= 0 {
		budget.MaxFanout = budget.MaxNodes / 4
		if budget.MaxFanout < LineageFanoutFloor {
			budget.MaxFanout = LineageFanoutFloor
		}
	}
	return budget
}

// LineageOmissions reports each traversal budget axis on its own. Node counts
// cover the links the walk examined, so they are lower bounds on what lies
// beyond the budget (component-lineage).
type LineageOmissions struct {
	// DepthNodes counts distinct unseen nodes examined at the depth cap.
	DepthNodes int `json:"depth_nodes"`
	// NodeCapNodes counts distinct unseen nodes examined after the node cap filled.
	NodeCapNodes int `json:"node_cap_nodes"`
	// FanoutNodes counts nodes, at any depth, whose ranked links exceeded
	// their examined prefix.
	FanoutNodes int `json:"fanout_nodes"`
	// Links counts reached links dropped by the link cap.
	Links int `json:"links"`
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
	Omissions                    LineageOmissions              `json:"omissions"`
	ForeignWorkspaceLinksIgnored int                           `json:"foreign_workspace_links_ignored,omitempty"`
	Depths                       map[LineageNode]int           `json:"-"`
	Paths                        map[LineageNode][]LineageLink `json:"-"`
}

// LineageNodeStatus is the bounded status projection of one lineage node.
// Each field applies only to the node types that carry it (component-lineage).
type LineageNodeStatus struct {
	// State is a task, blueprint, or work order state, a decision status, a
	// planning session status, or a planning bundle status.
	State string `json:"state,omitempty"`
	// VersionStatus is current, superseded, proposed, or dismissed for a
	// document version node.
	VersionStatus string `json:"version_status,omitempty"`
	// CurrentVersion is the document's current confirmed or current version.
	CurrentVersion int  `json:"current_version,omitempty"`
	Archived       bool `json:"archived,omitempty"`
	// Outcome, ReviewRound, and ReviewSeat describe a verdict from its
	// review.completed record.
	Outcome     string `json:"outcome,omitempty"`
	ReviewRound int    `json:"review_round,omitempty"`
	ReviewSeat  int    `json:"review_seat,omitempty"`
}

// Version status vocabulary for document version nodes.
const (
	LineageVersionCurrent    = "current"
	LineageVersionSuperseded = "superseded"
	LineageVersionProposed   = "proposed"
	LineageVersionDismissed  = "dismissed"
)

// LineageVersionStatus classifies one document version. A dismissed or
// retired unconfirmed version is dismissed; an unconfirmed live one is
// proposed; a confirmed one is current when it is the document's current
// version and superseded otherwise.
func LineageVersionStatus(version, current int, confirmed, dismissed bool) string {
	switch {
	case !confirmed && dismissed:
		return LineageVersionDismissed
	case !confirmed:
		return LineageVersionProposed
	case version == current:
		return LineageVersionCurrent
	default:
		return LineageVersionSuperseded
	}
}

// LineageOutLinkQuery reads one link kind leaving a bounded set of source
// nodes, for reads such as impact that need every such link rather than a
// walk's ranked prefix (component-lineage).
type LineageOutLinkQuery struct {
	Sources []LineageNode
	Kind    string
	DstType LineageNodeType
	// Limit caps returned links; stores return at most Limit links ordered by
	// event ID, then source and destination keys.
	Limit int
}

// LineageOutLinkMaxSources bounds the sources one out-link read names.
const LineageOutLinkMaxSources = 1024

// Validate refuses an unbounded or malformed out-link query.
func (query LineageOutLinkQuery) Validate() error {
	if len(query.Sources) == 0 || len(query.Sources) > LineageOutLinkMaxSources {
		return fmt.Errorf("lineage out-link query needs 1-%d sources", LineageOutLinkMaxSources)
	}
	for _, source := range query.Sources {
		if !source.Valid() {
			return fmt.Errorf("lineage out-link query names an invalid source")
		}
	}
	if strings.TrimSpace(query.Kind) == "" || !query.DstType.Valid() || query.Limit <= 0 {
		return fmt.Errorf("lineage out-link query needs a kind, a destination type, and a positive limit")
	}
	return nil
}

// SortLineageOutLinks orders out-link results as every store returns them.
func SortLineageOutLinks(links []LineageLink) {
	sort.Slice(links, func(i, j int) bool {
		left, right := links[i], links[j]
		if left.CreatedByEventID != right.CreatedByEventID {
			return left.CreatedByEventID < right.CreatedByEventID
		}
		return lineageTraversalLinkKey(left) < lineageTraversalLinkKey(right)
	})
}

// ContextArtifactSelection is the common bounded artifact view used by both
// MCP work-order delivery and in-process pipeline input. Keeping selection in
// one pure domain function prevents those context paths from granting
// different reachability (component-lineage).
type ContextArtifactSelection struct {
	OmittedArtifacts []Artifact
	Nodes            []LineageNode
	Artifacts        []Artifact
	Truncated        bool
	Omitted          int
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

// SelectContextArtifacts walks links once and selects from that walk. Callers
// that already hold the traversal use SelectContextArtifactsFromTraversal.
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
	return SelectContextArtifactsFromTraversal(traversal, artifacts, opts), nil
}

// SelectContextArtifactsFromTraversal selects artifacts reachable in an
// already computed traversal, so one context assembly walks the graph once
// (component-lineage).
func SelectContextArtifactsFromTraversal(traversal LineageTraversal, artifacts []Artifact, opts ContextArtifactSelectionOptions) ContextArtifactSelection {
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
	for _, item := range ordered {
		if !selected[item.artifact.ID] {
			selection.OmittedArtifacts = append(selection.OmittedArtifacts, item.artifact)
		}
	}
	selection.Omitted = len(ordered) - len(selection.Artifacts)
	selection.Truncated = selection.Truncated || selection.Omitted > 0
	return selection
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
	adjacent := map[LineageNode][]lineageNeighbor{}
	for _, link := range links {
		if budget.Workspace != "" && link.Workspace != budget.Workspace {
			continue
		}
		if link.Validate() != nil {
			continue
		}
		src := LineageNode{Type: link.SrcType, ID: link.SrcID}
		dst := LineageNode{Type: link.DstType, ID: link.DstID}
		adjacent[src] = append(adjacent[src], lineageNeighbor{node: dst, link: link})
		adjacent[dst] = append(adjacent[dst], lineageNeighbor{node: src, link: link})
	}
	for node := range adjacent {
		sortLineageNeighbors(adjacent[node])
	}
	result, err := walkLineage(roots, budget, func(level []LineageNode, limit int) (map[LineageNode][]lineageNeighbor, error) {
		out := make(map[LineageNode][]lineageNeighbor, len(level))
		for _, node := range level {
			list := adjacent[node]
			if len(list) > limit {
				list = list[:limit]
			}
			out[node] = list
		}
		return out, nil
	})
	if err != nil {
		return LineageTraversal{}, err
	}
	if budget.Workspace != "" {
		for _, link := range links {
			if link.Workspace != budget.Workspace {
				result.ForeignWorkspaceLinksIgnored++
			}
		}
	}
	return result, nil
}

// LineageNeighborFetch returns, for each requested node, its first limit
// incident links of the workspace in LineageRelationPriority order, then link
// time, event ID, and the far endpoint's node key in byte order. A node with
// fewer links returns all of them. Rows LineageLink.Validate refuses are never
// returned (component-lineage).
type LineageNeighborFetch func(ctx context.Context, nodes []LineageNode, limit int) (map[LineageNode][]LineageLink, error)

// FetchLineageNeighborhood runs the TraverseLineage walk against a store,
// asking fetch once per depth for the ranked link prefixes the walk examines.
// The returned links traverse exactly as every workspace link would under the
// same normalized budget, so store cost follows the budget rather than the
// workspace graph or a hub's degree (component-lineage;
// req-260802-72fc68 AC-2.5).
func FetchLineageNeighborhood(ctx context.Context, roots []LineageNode, budget LineageTraversalBudget, fetch LineageNeighborFetch) ([]LineageLink, error) {
	if budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks < 0 {
		return nil, fmt.Errorf("lineage neighborhood requires bounded depth and nodes")
	}
	fetched := map[string]LineageLink{}
	_, err := walkLineage(roots, budget, func(level []LineageNode, limit int) (map[LineageNode][]lineageNeighbor, error) {
		rows, err := fetch(ctx, level, limit)
		if err != nil {
			return nil, err
		}
		out := make(map[LineageNode][]lineageNeighbor, len(level))
		for _, node := range level {
			var list []lineageNeighbor
			for _, link := range rows[node] {
				if (budget.Workspace != "" && link.Workspace != budget.Workspace) || link.Validate() != nil {
					continue
				}
				src := LineageNode{Type: link.SrcType, ID: link.SrcID}
				dst := LineageNode{Type: link.DstType, ID: link.DstID}
				far := dst
				if dst == node {
					far = src
				} else if src != node {
					continue
				}
				list = append(list, lineageNeighbor{node: far, link: link})
				fetched[lineageTraversalLinkKey(link)] = link
			}
			sortLineageNeighbors(list)
			if len(list) > limit {
				list = list[:limit]
			}
			out[node] = list
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	links := make([]LineageLink, 0, len(fetched))
	for _, link := range fetched {
		links = append(links, link)
	}
	sortLineageLinks(links)
	return links, nil
}

// RankLineageLinks orders node's incident links as the walk ranks them and
// keeps the first limit. Stores without a SQL ranking use it to answer a
// LineageNeighborFetch.
func RankLineageLinks(node LineageNode, links []LineageLink, limit int) []LineageLink {
	neighbors := make([]lineageNeighbor, 0, len(links))
	for _, link := range links {
		neighbors = append(neighbors, lineageNeighbor{node: lineageFarEndpoint(node, link), link: link})
	}
	sortLineageNeighbors(neighbors)
	if limit >= 0 && len(neighbors) > limit {
		neighbors = neighbors[:limit]
	}
	out := make([]LineageLink, len(neighbors))
	for index, neighbor := range neighbors {
		out[index] = neighbor.link
	}
	return out
}

type lineageNeighbor struct {
	node LineageNode
	link LineageLink
}

func lineageNeighborLess(left, right lineageNeighbor) bool {
	if LineageRelationPriority(left.link.Kind) != LineageRelationPriority(right.link.Kind) {
		return LineageRelationPriority(left.link.Kind) < LineageRelationPriority(right.link.Kind)
	}
	if !left.link.CreatedAt.Equal(right.link.CreatedAt) {
		return left.link.CreatedAt.Before(right.link.CreatedAt)
	}
	if left.link.CreatedByEventID != right.link.CreatedByEventID {
		return left.link.CreatedByEventID < right.link.CreatedByEventID
	}
	if leftKey, rightKey := lineageTraversalNodeKey(left.node), lineageTraversalNodeKey(right.node); leftKey != rightKey {
		return leftKey < rightKey
	}
	return left.link.Kind < right.link.Kind
}

// LineageLinkRanksBefore reports whether left ranks before right among
// node's incident links, in the order the walk examines them. Stores that
// keep ranked per-node lists use it to insert in place.
func LineageLinkRanksBefore(node LineageNode, left, right LineageLink) bool {
	return lineageNeighborLess(lineageNeighbor{node: lineageFarEndpoint(node, left), link: left}, lineageNeighbor{node: lineageFarEndpoint(node, right), link: right})
}

func lineageFarEndpoint(node LineageNode, link LineageLink) LineageNode {
	if far := (LineageNode{Type: link.DstType, ID: link.DstID}); far != node {
		return far
	}
	return LineageNode{Type: link.SrcType, ID: link.SrcID}
}

func sortLineageNeighbors(neighbors []lineageNeighbor) {
	sort.Slice(neighbors, func(i, j int) bool { return lineageNeighborLess(neighbors[i], neighbors[j]) })
}

func sortLineageLinks(links []LineageLink) {
	sort.Slice(links, func(i, j int) bool {
		left, right := links[i], links[j]
		if left.CreatedByEventID != right.CreatedByEventID {
			return left.CreatedByEventID < right.CreatedByEventID
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return lineageTraversalLinkKey(left) < lineageTraversalLinkKey(right)
	})
}

// walkLineage is the one bounded walk behind TraverseLineage and
// FetchLineageNeighborhood. neighbors returns, for every node at one depth,
// at most limit ranked neighbors; the walk asks for one beyond each node's
// prefix to detect fan-out truncation. A root examines up to the larger of
// MaxNodes and MaxFanout links and every other node up to MaxFanout. Each depth admits in rounds: round r
// examines the r-th ranked link of every node at that depth in the order the
// walk reached them, so a hub cannot spend the node budget before its peers
// admit their first neighbors (component-lineage).
func walkLineage(roots []LineageNode, budget LineageTraversalBudget, neighbors func(level []LineageNode, limit int) (map[LineageNode][]lineageNeighbor, error)) (LineageTraversal, error) {
	budget = budget.Normalized()
	sortedRoots := append([]LineageNode(nil), roots...)
	sort.Slice(sortedRoots, func(i, j int) bool {
		return lineageTraversalNodeKey(sortedRoots[i]) < lineageTraversalNodeKey(sortedRoots[j])
	})
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
	omittedNodes, omittedByDepth, omittedByCap := map[LineageNode]bool{}, map[LineageNode]bool{}, map[LineageNode]bool{}
	reasons := map[string]bool{}
	level := []LineageNode{}
	for _, root := range sortedRoots {
		if !root.Valid() || seen[root] {
			continue
		}
		if len(result.Nodes) == budget.MaxNodes {
			result.Truncated = true
			omittedNodes[root] = true
			omittedByCap[root] = true
			reasons["nodes"] = true
			break
		}
		seen[root] = true
		result.Roots = append(result.Roots, root)
		result.Nodes = append(result.Nodes, root)
		result.Depths[root] = 0
		level = append(level, root)
	}
	examined := map[string]LineageLink{}
	for depth := 0; len(level) > 0; depth++ {
		limit := budget.MaxFanout
		if depth == 0 && budget.MaxNodes > limit {
			limit = budget.MaxNodes
		}
		adjacent, err := neighbors(level, limit+1)
		if err != nil {
			return LineageTraversal{}, err
		}
		prefixes := make([][]lineageNeighbor, len(level))
		rounds := 0
		for index, node := range level {
			prefix := adjacent[node]
			if len(prefix) > limit {
				// A cut prefix may hide a neighbor the walk never examined,
				// at the depth cap as anywhere else, so it always reports
				// fan-out rather than claim a complete neighborhood
				// (component-lineage).
				prefix = prefix[:limit]
				result.Truncated = true
				result.Omissions.FanoutNodes++
				reasons["fanout"] = true
			}
			for _, candidate := range prefix {
				examined[lineageTraversalLinkKey(candidate.link)] = candidate.link
			}
			prefixes[index] = prefix
			if len(prefix) > rounds {
				rounds = len(prefix)
			}
		}
		next := []LineageNode{}
		for round := 0; round < rounds; round++ {
			for index, node := range level {
				if round >= len(prefixes[index]) {
					continue
				}
				neighbor := prefixes[index][round]
				if seen[neighbor.node] || !budget.Filter.admits(node, neighbor.node, neighbor.link) {
					continue
				}
				if depth == budget.MaxDepth {
					result.Truncated = true
					omittedNodes[neighbor.node] = true
					omittedByDepth[neighbor.node] = true
					reasons["depth"] = true
					continue
				}
				if len(result.Nodes) == budget.MaxNodes {
					result.Truncated = true
					omittedNodes[neighbor.node] = true
					omittedByCap[neighbor.node] = true
					reasons["nodes"] = true
					continue
				}
				seen[neighbor.node] = true
				result.Nodes = append(result.Nodes, neighbor.node)
				result.Depths[neighbor.node] = depth + 1
				result.Paths[neighbor.node] = append(append([]LineageLink(nil), result.Paths[node]...), neighbor.link)
				next = append(next, neighbor.node)
			}
		}
		level = next
	}
	for _, link := range examined {
		if !budget.Filter.admitsKind(link.Kind) {
			continue
		}
		if seen[LineageNode{Type: link.SrcType, ID: link.SrcID}] && seen[LineageNode{Type: link.DstType, ID: link.DstID}] {
			result.Links = append(result.Links, link)
		}
	}
	sortLineageLinks(result.Links)
	if budget.MaxLinks > 0 && len(result.Links) > budget.MaxLinks {
		result.OmittedLinks = len(result.Links) - budget.MaxLinks
		result.Links = result.Links[:budget.MaxLinks]
		result.Truncated = true
		reasons["links"] = true
	}
	result.OmittedNodes = len(omittedNodes)
	result.Omissions.DepthNodes = len(omittedByDepth)
	result.Omissions.NodeCapNodes = len(omittedByCap)
	result.Omissions.Links = result.OmittedLinks
	for _, reason := range []string{"depth", "nodes", "fanout", "links"} {
		if reasons[reason] {
			result.ExhaustionReasons = append(result.ExhaustionReasons, reason)
		}
	}
	return result, nil
}

// LineageRelationDefaultPriority ranks every kind lineageRelationPriorities
// does not name.
const LineageRelationDefaultPriority = 5

// lineageRelationPriorities is the one relation ranking. Traversal, the
// SingleStore neighborhood walk, and the context-assembly relation tie-break
// call LineageRelationPriority; the PostgreSQL neighborhood CTE mirrors it in
// SQL, and a parity test holds the two together (component-lineage). Only
// kinds a projector case produces are ranked.
var lineageRelationPriorities = map[string]int{
	"serves":       0,
	"materializes": 1, "supersedes": 1,
	"depends_on":       2,
	"produced_verdict": 3, "merged_range": 3, "submitted_range": 3, "submitted_as": 3,
	"supports": 4,
}

// LineageRelationPriority is the graph-walk half of the context priority
// contract. Item-type ranking is applied again after content is rendered.
func LineageRelationPriority(kind string) int {
	if priority, ok := lineageRelationPriorities[kind]; ok {
		return priority
	}
	return LineageRelationDefaultPriority
}

// LineageRankedRelationKinds returns the kinds ranked ahead of the default,
// sorted, so callers can compare another copy of the ranking against it.
func LineageRankedRelationKinds() []string {
	kinds := make([]string, 0, len(lineageRelationPriorities))
	for kind := range lineageRelationPriorities {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// LineageRelationRankSQL renders LineageRelationPriority as a SQL CASE over
// column for the store neighbor queries, which cannot call Go. Kinds are
// fixed projector identifiers, never caller input (component-lineage).
func LineageRelationRankSQL(column string) string {
	var out strings.Builder
	out.WriteString("CASE " + column)
	for _, kind := range LineageRankedRelationKinds() {
		fmt.Fprintf(&out, " WHEN '%s' THEN %d", kind, lineageRelationPriorities[kind])
	}
	fmt.Fprintf(&out, " ELSE %d END", LineageRelationDefaultPriority)
	return out.String()
}

// LineageNodeTypes lists every valid node type, sorted, for store queries
// that mirror LineageLink.Validate.
func LineageNodeTypes() []LineageNodeType {
	return []LineageNodeType{
		LineageBlueprint, LineageBlueprintVersion, LineageCommitRange, LineageDecision, LineageEvidence,
		LineagePlanningBundle, LineagePlanningSession, LineagePullRequest, LineageReferenceDocument,
		LineageReferenceDocumentVersion, LineageRepositoryPath, LineageRequirement, LineageRequirementVersion,
		LineageSystemDesign, LineageSystemDesignVersion, LineageTask, LineageVerdict, LineageWorkOrder,
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
// both immutable endpoints are known (component-lineage).
func PullRequestLineageID(repository string, number int) string {
	return fmt.Sprintf("%s#%d", strings.TrimSpace(repository), number)
}

func CommitRangeLineageID(repository, baseSHA, headSHA string) string {
	return fmt.Sprintf("%s@%s..%s", strings.TrimSpace(repository), strings.TrimSpace(baseSHA), strings.TrimSpace(headSHA))
}

func VerdictLineageID(workOrderID string) string { return "review:" + workOrderID }
