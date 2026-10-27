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

// Impact read bounds: at most lineageImpactMaxVersions version links and
// lineageImpactMaxEntries version-to-task links are read, and pages of at most
// 100 entries select from them. They are variables only so tests can drive
// truncation with small histories (component-lineage).
var (
	lineageImpactMaxVersions = core.LineageKindLinkMaxNodes
	lineageImpactMaxEntries  = 1000
)

const (
	lineageImpactDefaultLimit = 25
	lineageImpactMaxLimit     = 100
	lineageImpactMaxOffset    = 1000
)

// Version status of a task served through the requirement edge alone. It is
// unrecorded only when a complete inbound read finds no version edge from the
// requirement; when that read is truncated the version is unknown, never
// unrecorded.
const (
	lineageVersionUnrecorded = "unrecorded"
	lineageVersionUnknown    = "unknown"
)

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
		if err != nil || parsed < 0 || parsed > lineageImpactMaxOffset {
			return request, lineageRequestError{fmt.Sprintf("offset must be an integer from 0 to %d", lineageImpactMaxOffset)}
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

func (s *Server) lineageImpact(ctx context.Context, request lineageImpactRequest) (lineageImpactView, error) {
	root := core.LineageNode{Type: request.Root.Type, ID: request.Root.ID}
	view := lineageImpactView{Root: root, OutdatedOnly: request.OutdatedOnly, Limit: request.Limit, Offset: request.Offset, Items: []lineageImpactEntry{}}
	truncate := func(reason string) {
		view.Truncated = true
		for _, existing := range view.ExhaustionReasons {
			if existing == reason {
				return
			}
		}
		view.ExhaustionReasons = append(view.ExhaustionReasons, reason)
	}
	var versionType core.LineageNodeType
	var kind string
	switch root.Type {
	case core.LineageRequirement:
		versionType, kind = core.LineageRequirementVersion, "serves"
	case core.LineageSystemDesign:
		versionType, kind = core.LineageSystemDesignVersion, "governs"
	default:
		return view, lineageRequestError{"lineage impact reads a requirement or system_design root"}
	}
	// The document row alone gives the current version and archive state;
	// no version history is read (component-lineage).
	rootStatus, err := s.Store.ListLineageNodeStatuses(ctx, []core.LineageNode{root})
	if err != nil {
		return view, err
	}
	status, exists := rootStatus[root]
	if !exists {
		return view, errLineageRootNotFound
	}
	view.CurrentVersion, view.Archived = status.CurrentVersion, status.Archived
	// The document -versions-> version links name every confirmed version,
	// read with a cap and a sentinel. The newest versions stay in scope.
	versionLinks, err := s.Store.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: []core.LineageNode{root}, Kind: "versions", FarType: versionType, Limit: lineageImpactMaxVersions + 1})
	if err != nil {
		return view, err
	}
	if len(versionLinks) > lineageImpactMaxVersions {
		versionLinks = versionLinks[len(versionLinks)-lineageImpactMaxVersions:]
		truncate("versions")
	}
	sources := make([]core.LineageNode, 0, len(versionLinks))
	for _, link := range versionLinks {
		sources = append(sources, core.LineageNode{Type: link.DstType, ID: link.DstID})
	}
	readTasks := func(nodes []core.LineageNode, kind string, inbound bool, farType core.LineageNodeType) ([]core.LineageLink, bool, error) {
		if len(nodes) == 0 {
			return nil, false, nil
		}
		links, err := s.Store.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: nodes, Kind: kind, Inbound: inbound, FarType: farType, Limit: lineageImpactMaxEntries + 1})
		if err != nil || len(links) <= lineageImpactMaxEntries {
			return links, false, err
		}
		return links[:lineageImpactMaxEntries], true, nil
	}
	links, cut, err := readTasks(sources, kind, false, core.LineageTask)
	if err != nil {
		return view, err
	}
	if cut {
		truncate("tasks")
	}
	unknown := map[string]bool{}
	if root.Type == core.LineageRequirement {
		// Every requirement attachment projects requirement -serves-> task,
		// so the direct edges name every served task. A task the version
		// read did not reach is checked from the task side: its own inbound
		// version edges from this requirement give the recorded version
		// even when the version read was truncated (AC-5.2).
		direct, cut, err := readTasks([]core.LineageNode{root}, "serves", false, core.LineageTask)
		if err != nil {
			return view, err
		}
		if cut {
			truncate("tasks")
		}
		versioned := map[string]bool{}
		for _, link := range links {
			versioned[link.DstID] = true
		}
		missing := []core.LineageNode{}
		for _, link := range direct {
			if !versioned[link.DstID] {
				missing = append(missing, core.LineageNode{Type: core.LineageTask, ID: link.DstID})
			}
		}
		inbound, inboundCut, err := readTasks(missing, "serves", true, core.LineageRequirementVersion)
		if err != nil {
			return view, err
		}
		if inboundCut {
			truncate("version_edges")
		}
		recovered := map[string]bool{}
		for _, link := range inbound {
			if baseID, _ := store.LineageVersionNodeParts(core.LineageNode{Type: link.SrcType, ID: link.SrcID}); baseID == root.ID {
				links = append(links, link)
				recovered[link.DstID] = true
			}
		}
		for _, link := range direct {
			if versioned[link.DstID] || recovered[link.DstID] {
				continue
			}
			links = append(links, link)
			unknown[link.DstID] = inboundCut
		}
		core.SortLineageKindLinks(links)
	}
	taskNodes := []core.LineageNode{}
	statusNodes := []core.LineageNode{}
	seen := map[core.LineageNode]bool{}
	for _, link := range links {
		for _, node := range []core.LineageNode{{Type: core.LineageTask, ID: link.DstID}, {Type: link.SrcType, ID: link.SrcID}} {
			if node.Type == root.Type || seen[node] {
				continue
			}
			seen[node] = true
			statusNodes = append(statusNodes, node)
			if node.Type == core.LineageTask {
				taskNodes = append(taskNodes, node)
			}
		}
	}
	statuses := map[core.LineageNode]core.LineageNodeStatus{}
	labels := map[string]string{}
	ranges := map[string][]string{}
	if len(taskNodes) > 0 {
		if statuses, err = s.Store.ListLineageNodeStatuses(ctx, statusNodes); err != nil {
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
		merged, err := s.Store.ListLineageKindLinks(ctx, core.LineageKindLinkQuery{Nodes: taskNodes, Kind: "merged_range", FarType: core.LineageCommitRange, Limit: rangeLimit + 1})
		if err != nil {
			return view, err
		}
		if len(merged) > rangeLimit {
			merged = merged[:rangeLimit]
			truncate("merged_ranges")
		}
		for _, link := range merged {
			ranges[link.SrcID] = append(ranges[link.SrcID], link.DstID)
		}
	}
	entries := make([]lineageImpactEntry, 0, len(links))
	for _, link := range links {
		task := core.LineageNode{Type: core.LineageTask, ID: link.DstID}
		entry := lineageImpactEntry{Relation: kind, Task: core.LineageNode{Type: task.Type, ID: task.ID, Label: labels[task.ID]}, TaskState: statuses[task].State, MergedRanges: ranges[link.DstID], EventID: link.CreatedByEventID}
		switch {
		case link.SrcType == versionType:
			version := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
			_, entry.Version = store.LineageVersionNodeParts(version)
			entry.VersionNode = version
			entry.VersionStatus = lineageVersionUnknown
			if recorded, ok := statuses[version]; ok && recorded.VersionStatus != "" {
				entry.VersionStatus = recorded.VersionStatus
			}
		case unknown[link.DstID]:
			entry.VersionStatus = lineageVersionUnknown
		default:
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
// that is no longer current. An unrecorded or unknown version is never
// asserted.
func lineageImpactOutdated(entry lineageImpactEntry) bool {
	return entry.TaskState == string(core.TaskMerged) && entry.VersionStatus == core.LineageVersionSuperseded
}
