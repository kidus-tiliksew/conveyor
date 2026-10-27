package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// lineageWalkRequest is one operator lineage walk as HTTP, CLI, and MCP
// state it. Zero caps take the context budget, so a request that names no
// new option walks exactly as before (component-lineage;
// req-260802-72fc68 REQ-5).
type lineageWalkRequest struct {
	Root                         core.LineageNode
	MaxDepth, MaxNodes, MaxLinks int
	Filter                       core.LineageTraversalFilter
	IncludeDepth                 bool
	IncludePath                  bool
	IncludeStatus                bool
}

// lineageRequestError is a refused request: HTTP answers 400 and MCP returns
// the message.
type lineageRequestError struct{ message string }

func (e lineageRequestError) Error() string { return e.message }

var (
	errLineageRootNotFound          = errors.New("lineage node not found")
	errLineageConfigurationUnusable = errors.New("lineage configuration is unavailable")
)

// lineageNodeView carries the per-node fields a walk includes on request.
type lineageNodeView struct {
	Type     core.LineageNodeType    `json:"type"`
	ID       string                  `json:"id"`
	Label    string                  `json:"label,omitempty"`
	Depth    *int                    `json:"depth,omitempty"`
	EdgePath []core.LineageLink      `json:"edge_path,omitempty"`
	Status   *core.LineageNodeStatus `json:"status,omitempty"`
}

// lineageGraphView replaces nodes with views when the request includes depth,
// path, or status; every other field is the traversal's own.
type lineageGraphView struct {
	core.LineageTraversal
	Nodes []lineageNodeView `json:"nodes"`
}

func (request lineageWalkRequest) includesDetail() bool {
	return request.IncludeDepth || request.IncludePath || request.IncludeStatus
}

// parseLineageWalkQuery reads the additive walk parameters. Every list
// parameter accepts repeated or comma-separated values.
func parseLineageWalkQuery(r *http.Request, root core.LineageNode) (lineageWalkRequest, error) {
	request := lineageWalkRequest{Root: root}
	query := r.URL.Query()
	for name, target := range map[string]*int{"max_depth": &request.MaxDepth, "max_nodes": &request.MaxNodes, "max_links": &request.MaxLinks} {
		value, err := boundedLineageQueryValue(r, name, 0)
		if err != nil {
			return request, lineageRequestError{err.Error()}
		}
		*target = value
	}
	values := func(name string) []string {
		var out []string
		for _, raw := range query[name] {
			for _, value := range strings.Split(raw, ",") {
				if value = strings.TrimSpace(value); value != "" {
					out = append(out, value)
				}
			}
		}
		return out
	}
	direction := values("direction")
	if len(direction) > 1 {
		return request, lineageRequestError{"direction takes one value: out, in, or both"}
	}
	if len(direction) == 1 {
		request.Filter.Direction = core.LineageDirection(direction[0])
	}
	request.Filter.Kinds = values("kind")
	for _, nodeType := range values("node_type") {
		request.Filter.NodeTypes = append(request.Filter.NodeTypes, core.LineageNodeType(nodeType))
	}
	for _, include := range values("include") {
		switch include {
		case "depth":
			request.IncludeDepth = true
		case "path":
			request.IncludePath = true
		case "status":
			request.IncludeStatus = true
		default:
			return request, lineageRequestError{fmt.Sprintf("unknown include %q: use depth, path, or status", include)}
		}
	}
	return request, request.validateFilter()
}

// validateFilter refuses an unknown direction, edge kind, or node type.
func (request *lineageWalkRequest) validateFilter() error {
	filter := &request.Filter
	if !filter.Direction.Valid() {
		return lineageRequestError{fmt.Sprintf("unknown direction %q: use out, in, or both", filter.Direction)}
	}
	if filter.Direction == core.LineageDirectionBoth {
		filter.Direction = ""
	}
	known := lineageLinkKinds()
	for _, kind := range filter.Kinds {
		if _, ok := known[kind]; !ok {
			return lineageRequestError{fmt.Sprintf("unknown lineage edge kind %q", kind)}
		}
	}
	for _, nodeType := range filter.NodeTypes {
		if !nodeType.Valid() {
			return lineageRequestError{fmt.Sprintf("unknown lineage node type %q", nodeType)}
		}
	}
	sort.Strings(filter.Kinds)
	filter.Kinds = compactStrings(filter.Kinds)
	sort.Slice(filter.NodeTypes, func(i, j int) bool { return filter.NodeTypes[i] < filter.NodeTypes[j] })
	filter.NodeTypes = compactNodeTypes(filter.NodeTypes)
	return nil
}

// lineageLinkKinds is every kind a link can carry: the projector's kinds and
// the retained historical feature assignment.
func lineageLinkKinds() map[string]struct{} {
	kinds := store.CanonicalLineageKinds()
	kinds["historical_feature_assignment"] = struct{}{}
	return kinds
}

func compactStrings(values []string) []string {
	out := values[:0]
	for index, value := range values {
		if index == 0 || value != values[index-1] {
			out = append(out, value)
		}
	}
	return out
}

func compactNodeTypes(values []core.LineageNodeType) []core.LineageNodeType {
	out := values[:0]
	for index, value := range values {
		if index == 0 || value != values[index-1] {
			out = append(out, value)
		}
	}
	return out
}

// lineageReadBudget is the effective walk budget: the context budget for
// defaults and the operator read budget as the cap (component-lineage).
type lineageReadBudget struct {
	defaultDepth, defaultNodes, defaultLinks int
	maxDepth, maxNodes, maxLinks             int
}

func (s *Server) lineageReadBudget(ctx context.Context) (lineageReadBudget, error) {
	effective, err := s.effectiveLineageBudget(ctx)
	if err != nil {
		return lineageReadBudget{}, err
	}
	var cfg *config.Config
	if s.ConfigProvider != nil {
		if cfg, err = s.ConfigProvider(ctx); err != nil {
			return lineageReadBudget{}, err
		}
	}
	depth, nodes, links := config.LineageOperatorReadBudget(cfg)
	return lineageReadBudget{
		defaultDepth: effective.Depth, defaultNodes: effective.Nodes, defaultLinks: effective.Links,
		maxDepth: max(depth, effective.Depth), maxNodes: max(nodes, effective.Nodes), maxLinks: max(links, effective.Links),
	}, nil
}

// walkLineage validates the root and budget, walks, and renders the result.
// A request without depth, path, or status renders core.LineageTraversal
// unchanged.
func (s *Server) walkLineage(r *http.Request, request lineageWalkRequest) (any, error) {
	if !request.Root.Valid() {
		return nil, lineageRequestError{"a valid lineage node type and id are required"}
	}
	budget, err := s.lineageReadBudget(r.Context())
	if err != nil {
		log.Printf("resolve lineage traversal budget: %v", err)
		return nil, errLineageConfigurationUnusable
	}
	pick := func(value, fallback int) int {
		if value == 0 {
			return fallback
		}
		return value
	}
	maxDepth, maxNodes, maxLinks := pick(request.MaxDepth, budget.defaultDepth), pick(request.MaxNodes, budget.defaultNodes), pick(request.MaxLinks, budget.defaultLinks)
	if maxDepth < 0 || maxNodes < 0 || maxLinks < 0 {
		return nil, lineageRequestError{"lineage budget values must be positive integers"}
	}
	if maxDepth > budget.maxDepth || maxNodes > budget.maxNodes || maxLinks > budget.maxLinks {
		return nil, lineageRequestError{fmt.Sprintf("lineage query exceeds the operator read budget (depth %d, nodes %d, links %d)", budget.maxDepth, budget.maxNodes, budget.maxLinks)}
	}
	exists, err := s.lineageNodeExists(r, request.Root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errLineageRootNotFound
	}
	traversalBudget := core.LineageTraversalBudget{MaxDepth: maxDepth, MaxNodes: maxNodes, MaxLinks: maxLinks}
	filter := request.Filter
	if filter.Direction != "" || len(filter.Kinds) > 0 || len(filter.NodeTypes) > 0 {
		traversalBudget.Filter = &filter
	}
	graph, err := s.lineageGraph(r, request.Root, traversalBudget)
	if err != nil {
		return nil, err
	}
	if !request.includesDetail() {
		return graph, nil
	}
	var statuses map[core.LineageNode]core.LineageNodeStatus
	if request.IncludeStatus {
		if statuses, err = s.Store.ListLineageNodeStatuses(r.Context(), graph.Nodes); err != nil {
			return nil, err
		}
	}
	view := lineageGraphView{LineageTraversal: graph, Nodes: make([]lineageNodeView, 0, len(graph.Nodes))}
	for _, node := range graph.Nodes {
		key := core.LineageNode{Type: node.Type, ID: node.ID}
		item := lineageNodeView{Type: node.Type, ID: node.ID, Label: node.Label}
		if request.IncludeDepth {
			depth := graph.Depths[key]
			item.Depth = &depth
		}
		if request.IncludePath {
			item.EdgePath = graph.Paths[key]
		}
		if status, ok := statuses[key]; ok {
			item.Status = &status
		}
		view.Nodes = append(view.Nodes, item)
	}
	return view, nil
}

// writeLineageError maps a walk or impact error to its HTTP status.
func writeLineageError(w http.ResponseWriter, err error) {
	var refused lineageRequestError
	switch {
	case errors.As(err, &refused):
		http.Error(w, refused.message, http.StatusBadRequest)
	case errors.Is(err, errLineageRootNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errLineageConfigurationUnusable):
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		log.Printf("handle lineage request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// Impact read bounds: at most lineageImpactMaxEntries version-to-task links
// are read, and pages of at most 100 entries select from them
// (component-lineage).
const (
	lineageImpactMaxEntries   = 1000
	lineageImpactDefaultLimit = 25
	lineageImpactMaxLimit     = 100
)

// Version status of a task served through the requirement edge alone, whose
// attachment recorded no version.
const lineageVersionUnrecorded = "unrecorded"

type lineageImpactRequest struct {
	Root          core.LineageNode
	OutdatedOnly  bool
	Limit, Offset int
}

// lineageImpactEntry is one version-to-task edge with the task's state, the
// version's status, and the task's merged commit ranges.
type lineageImpactEntry struct {
	Version       int              `json:"version,omitempty"`
	VersionNode   core.LineageNode `json:"version_node,omitzero"`
	VersionStatus string           `json:"version_status"`
	Relation      string           `json:"relation"`
	Task          core.LineageNode `json:"task"`
	TaskState     string           `json:"task_state,omitempty"`
	MergedRanges  []string         `json:"merged_ranges,omitempty"`
	EventID       int64            `json:"created_by_event_id"`
}

type lineageImpactView struct {
	Root              core.LineageNode     `json:"root"`
	CurrentVersion    int                  `json:"current_version"`
	Archived          bool                 `json:"archived,omitempty"`
	OutdatedOnly      bool                 `json:"outdated_only"`
	Items             []lineageImpactEntry `json:"items"`
	Total             int                  `json:"total"`
	Limit             int                  `json:"limit"`
	Offset            int                  `json:"offset"`
	NextOffset        *int                 `json:"next_offset,omitempty"`
	Truncated         bool                 `json:"truncated"`
	ExhaustionReasons []string             `json:"exhaustion_reasons,omitempty"`
}

func parseLineageImpactQuery(r *http.Request, root core.LineageNode) (lineageImpactRequest, error) {
	request := lineageImpactRequest{Root: root, Limit: lineageImpactDefaultLimit}
	query := r.URL.Query()
	if raw := strings.TrimSpace(query.Get("outdated")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return request, lineageRequestError{"outdated must be true or false"}
		}
		request.OutdatedOnly = parsed
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > lineageImpactMaxLimit {
			return request, lineageRequestError{fmt.Sprintf("limit must be an integer from 1 to %d", lineageImpactMaxLimit)}
		}
		request.Limit = parsed
	}
	if raw := strings.TrimSpace(query.Get("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > lineageImpactMaxEntries {
			return request, lineageRequestError{fmt.Sprintf("offset must be an integer from 0 to %d", lineageImpactMaxEntries)}
		}
		request.Offset = parsed
	}
	return request, nil
}

// getLineageImpact answers "which work was built on which version" for a
// requirement or System Design: each version's serves or governs edges to
// tasks, with task state, version status, and merged commit ranges
// (component-lineage; req-260802-72fc68 AC-5.2).
func (s *Server) getLineageImpact(w http.ResponseWriter, r *http.Request) {
	root := core.LineageNode{
		Type: core.LineageNodeType(strings.TrimSpace(chi.URLParam(r, "type"))),
		ID:   strings.TrimSpace(chi.URLParam(r, "id")),
	}
	request, err := parseLineageImpactQuery(r, root)
	if err != nil {
		writeLineageError(w, err)
		return
	}
	view, err := s.lineageImpact(r.Context(), request)
	if err != nil {
		writeLineageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type lineageImpactVersion struct {
	confirmed, dismissed bool
}

func (s *Server) lineageImpact(ctx context.Context, request lineageImpactRequest) (lineageImpactView, error) {
	root := request.Root
	view := lineageImpactView{Root: root, OutdatedOnly: request.OutdatedOnly, Limit: request.Limit, Offset: request.Offset, Items: []lineageImpactEntry{}}
	versions := map[int]lineageImpactVersion{}
	var versionType core.LineageNodeType
	var kind string
	switch root.Type {
	case core.LineageRequirement:
		requirement, err := s.Store.GetRequirement(ctx, root.ID)
		if err != nil {
			return view, errLineageRootNotFound
		}
		recorded, err := s.Store.ListRequirementVersions(ctx, root.ID)
		if err != nil {
			return view, err
		}
		for _, version := range recorded {
			versions[version.Version] = lineageImpactVersion{confirmed: version.Confirmed, dismissed: version.Retired}
		}
		view.CurrentVersion, view.Archived = requirement.CurrentVersion, requirement.Archived
		versionType, kind = core.LineageRequirementVersion, "serves"
	case core.LineageSystemDesign:
		design, err := s.Store.GetSystemDesign(ctx, root.ID)
		if err != nil {
			return view, errLineageRootNotFound
		}
		recorded, err := s.Store.ListSystemDesignVersions(ctx, root.ID)
		if err != nil {
			return view, err
		}
		for _, version := range recorded {
			versions[version.Version] = lineageImpactVersion{confirmed: version.Confirmed, dismissed: version.Dismissed}
		}
		view.CurrentVersion, view.Archived = design.CurrentVersion, design.Archived
		versionType, kind = core.LineageSystemDesignVersion, "governs"
	default:
		return view, lineageRequestError{"lineage impact reads a requirement or system_design root"}
	}
	numbers := make([]int, 0, len(versions))
	for number := range versions {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	if len(numbers) > core.LineageOutLinkMaxSources {
		// Newest versions first stay within the source bound.
		numbers = numbers[len(numbers)-core.LineageOutLinkMaxSources:]
		view.Truncated = true
		view.ExhaustionReasons = append(view.ExhaustionReasons, "versions")
	}
	sources := make([]core.LineageNode, 0, len(numbers))
	for _, number := range numbers {
		id := core.RequirementVersionLineageID(root.ID, number)
		if versionType == core.LineageSystemDesignVersion {
			id = core.SystemDesignVersionLineageID(root.ID, number)
		}
		sources = append(sources, core.LineageNode{Type: versionType, ID: id})
	}
	var links []core.LineageLink
	if len(sources) > 0 {
		var err error
		links, err = s.Store.ListLineageOutLinks(ctx, core.LineageOutLinkQuery{Sources: sources, Kind: kind, DstType: core.LineageTask, Limit: lineageImpactMaxEntries + 1})
		if err != nil {
			return view, err
		}
	}
	if root.Type == core.LineageRequirement {
		// An attachment that recorded no version projects only the
		// requirement edge; its task is listed with an unrecorded version.
		direct, err := s.Store.ListLineageOutLinks(ctx, core.LineageOutLinkQuery{Sources: []core.LineageNode{root}, Kind: "serves", DstType: core.LineageTask, Limit: lineageImpactMaxEntries + 1})
		if err != nil {
			return view, err
		}
		versioned := map[string]bool{}
		for _, link := range links {
			versioned[link.DstID] = true
		}
		for _, link := range direct {
			if !versioned[link.DstID] {
				links = append(links, link)
			}
		}
		core.SortLineageOutLinks(links)
	}
	if len(links) > lineageImpactMaxEntries {
		links = links[:lineageImpactMaxEntries]
		view.Truncated = true
		view.ExhaustionReasons = append(view.ExhaustionReasons, "tasks")
	}
	taskNodes := []core.LineageNode{}
	seenTasks := map[string]bool{}
	for _, link := range links {
		if !seenTasks[link.DstID] {
			seenTasks[link.DstID] = true
			taskNodes = append(taskNodes, core.LineageNode{Type: core.LineageTask, ID: link.DstID})
		}
	}
	statuses := map[core.LineageNode]core.LineageNodeStatus{}
	labels := map[string]string{}
	ranges := map[string][]string{}
	if len(taskNodes) > 0 {
		var err error
		if statuses, err = s.Store.ListLineageNodeStatuses(ctx, taskNodes); err != nil {
			return view, err
		}
		records, err := s.Store.ListLineageNodeRecords(ctx, taskNodes)
		if err != nil {
			return view, err
		}
		for _, node := range taskNodes {
			labels[node.ID] = firstLabel(records[node].Title, node.ID)
		}
		rangeLimit := 4 * lineageImpactMaxEntries
		merged, err := s.Store.ListLineageOutLinks(ctx, core.LineageOutLinkQuery{Sources: taskNodes, Kind: "merged_range", DstType: core.LineageCommitRange, Limit: rangeLimit + 1})
		if err != nil {
			return view, err
		}
		if len(merged) > rangeLimit {
			merged = merged[:rangeLimit]
			view.Truncated = true
			view.ExhaustionReasons = append(view.ExhaustionReasons, "merged_ranges")
		}
		for _, link := range merged {
			ranges[link.SrcID] = append(ranges[link.SrcID], link.DstID)
		}
	}
	entries := make([]lineageImpactEntry, 0, len(links))
	for _, link := range links {
		task := core.LineageNode{Type: core.LineageTask, ID: link.DstID, Label: labels[link.DstID]}
		entry := lineageImpactEntry{Relation: kind, Task: task, TaskState: statuses[core.LineageNode{Type: core.LineageTask, ID: link.DstID}].State, MergedRanges: ranges[link.DstID], EventID: link.CreatedByEventID}
		if link.SrcType == versionType {
			_, number := store.LineageVersionNodeParts(core.LineageNode{Type: link.SrcType, ID: link.SrcID})
			recorded, known := versions[number]
			entry.Version, entry.VersionNode = number, core.LineageNode{Type: link.SrcType, ID: link.SrcID}
			entry.VersionStatus = lineageVersionUnrecorded
			if known {
				entry.VersionStatus = core.LineageVersionStatus(number, view.CurrentVersion, recorded.confirmed, recorded.dismissed)
			}
		} else {
			entry.VersionStatus = lineageVersionUnrecorded
		}
		if request.OutdatedOnly && !lineageImpactOutdated(entry) {
			continue
		}
		entries = append(entries, entry)
	}
	view.Total = len(entries)
	if request.Offset > len(entries) {
		return view, lineageRequestError{"offset exceeds the impact result"}
	}
	end := min(request.Offset+request.Limit, len(entries))
	view.Items = append(view.Items, entries[request.Offset:end]...)
	if end < len(entries) {
		view.NextOffset = &end
	}
	return view, nil
}

// lineageImpactOutdated holds for merged work built on a confirmed version
// that is no longer current. An unrecorded version is never asserted.
func lineageImpactOutdated(entry lineageImpactEntry) bool {
	return entry.TaskState == string(core.TaskMerged) && entry.VersionStatus == core.LineageVersionSuperseded
}
