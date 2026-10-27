package store

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// memoryLineage is the memory backend's link map with a per-endpoint index.
// Each endpoint keeps its incident links sorted in the walk's ranking, so a
// neighbor prefix is the head of one list and an existence check is one
// lookup; neither reads another node's links. Every write goes through put
// and remove, which keep the map and the index together (component-lineage).
type memoryLineage struct {
	links  map[string]core.LineageLink
	byNode map[memoryLineageEndpoint][]core.LineageLink
	// lookups and examined count endpoint lookups and index entries read by
	// prefix and existence reads, so tests can bound per-request cost.
	lookups, examined atomic.Int64
	// kindLinkPeak is the most candidates one kind-link read retained.
	kindLinkPeak atomic.Int64
}

type memoryLineageEndpoint struct {
	workspace string
	node      core.LineageNode
}

func newMemoryLineage() *memoryLineage {
	return &memoryLineage{links: map[string]core.LineageLink{}, byNode: map[memoryLineageEndpoint][]core.LineageLink{}}
}

func (l *memoryLineage) len() int { return len(l.links) }

func (l *memoryLineage) get(key string) (core.LineageLink, bool) {
	link, ok := l.links[key]
	return link, ok
}

// put stores link, replacing any link with the same key.
func (l *memoryLineage) put(link core.LineageLink) {
	key := lineageLinkKey(link)
	if _, exists := l.links[key]; exists {
		l.remove(key)
	}
	l.links[key] = link
	for _, endpoint := range l.endpoints(link) {
		list := l.byNode[endpoint]
		index := sort.Search(len(list), func(i int) bool { return !core.LineageLinkRanksBefore(endpoint.node, list[i], link) })
		list = append(list, core.LineageLink{})
		copy(list[index+1:], list[index:])
		list[index] = link
		l.byNode[endpoint] = list
	}
}

func (l *memoryLineage) remove(key string) {
	link, exists := l.links[key]
	if !exists {
		return
	}
	delete(l.links, key)
	for _, endpoint := range l.endpoints(link) {
		list := l.byNode[endpoint]
		index := sort.Search(len(list), func(i int) bool { return !core.LineageLinkRanksBefore(endpoint.node, list[i], link) })
		for ; index < len(list); index++ {
			if lineageLinkKey(list[index]) == key {
				list = append(list[:index], list[index+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(l.byNode, endpoint)
		} else {
			l.byNode[endpoint] = list
		}
	}
}

func (l *memoryLineage) endpoints(link core.LineageLink) []memoryLineageEndpoint {
	src := memoryLineageEndpoint{workspace: link.Workspace, node: core.LineageNode{Type: link.SrcType, ID: link.SrcID}}
	dst := memoryLineageEndpoint{workspace: link.Workspace, node: core.LineageNode{Type: link.DstType, ID: link.DstID}}
	if src == dst {
		return []memoryLineageEndpoint{src}
	}
	return []memoryLineageEndpoint{src, dst}
}

// clone deep-copies the map and the index for a staged transaction.
func (l *memoryLineage) clone() *memoryLineage {
	out := &memoryLineage{links: make(map[string]core.LineageLink, len(l.links)), byNode: make(map[memoryLineageEndpoint][]core.LineageLink, len(l.byNode))}
	for key, link := range l.links {
		out.links[key] = link
	}
	for endpoint, list := range l.byNode {
		out.byNode[endpoint] = append([]core.LineageLink(nil), list...)
	}
	return out
}

func (l *memoryLineage) exists(workspace string, node core.LineageNode) bool {
	l.lookups.Add(1)
	return len(l.byNode[memoryLineageEndpoint{workspace: workspace, node: node}]) > 0
}

// prefix returns node's first limit valid incident links in ranked order.
func (l *memoryLineage) prefix(workspace string, node core.LineageNode, limit int) []core.LineageLink {
	l.lookups.Add(1)
	list := l.byNode[memoryLineageEndpoint{workspace: workspace, node: node}]
	out := make([]core.LineageLink, 0, min(limit, len(list)))
	for _, link := range list {
		if len(out) == limit {
			break
		}
		l.examined.Add(1)
		if link.Validate() == nil {
			out = append(out, link)
		}
	}
	return out
}

// LineageNodeRecord is the minimal entity projection needed to label a
// lineage graph without loading unrelated workspace records.
type LineageNodeRecord struct {
	Title  string
	Slug   string
	TaskID string
	Stage  core.Stage
}

// LineageContextRecords is the bounded entity projection consumed after the
// lineage walk has selected its node set. ReviewPayload is the latest review
// completion event for a terminal task and remains raw until the context
// renderer applies its existing summary rules.
type LineageContextRecords struct {
	Tasks        map[string]LineageContextTaskRecord
	Requirements map[string]LineageContextRequirementRecord
}

type LineageContextTaskRecord struct {
	Title         string
	State         core.TaskState
	ParentTaskID  string
	ReviewPayload json.RawMessage
}

type LineageContextRequirementRecord struct {
	Title      string
	Version    int
	Content    string
	Statements []core.RequirementStatement
	// Archived flags the rendered label (component-lineage).
	Archived bool
}

func (m *memory) ListLineageNodeRecords(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]LineageNodeRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	records := make(map[core.LineageNode]LineageNodeRecord, len(nodes))
	for _, node := range nodes {
		baseID := lineageRecordBaseID(node)
		switch node.Type {
		case core.LineageTask, core.LineageBlueprint, core.LineageBlueprintVersion:
			if task, ok := m.tasks[baseID]; ok && task.Workspace == workspace {
				records[node] = LineageNodeRecord{Title: task.Title}
			}
		case core.LineageRequirement, core.LineageRequirementVersion:
			if requirement, ok := m.requirements[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: requirement.Title, Slug: requirement.Slug}
			}
		case core.LineageReferenceDocument, core.LineageReferenceDocumentVersion:
			if document, ok := m.referenceDocuments[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: document.Name}
			}
		case core.LineageSystemDesign, core.LineageSystemDesignVersion:
			if document, ok := m.systemDesigns[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: document.Title, Slug: document.Slug}
			}
		case core.LineageDecision:
			if decision, ok := m.decisions[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: decision.Statement}
			}
		case core.LineageRepositoryPath:
			records[node] = LineageNodeRecord{Title: node.ID}
		case core.LineagePlanningSession:
			if session, ok := m.planningSessions[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: session.Title}
			}
		case core.LineageWorkOrder:
			if order, ok := m.workOrders[baseID]; ok {
				if task, exists := m.tasks[order.TaskID]; exists && task.Workspace == workspace {
					records[node] = LineageNodeRecord{TaskID: order.TaskID, Stage: order.Stage}
				}
			}
		case core.LineagePlanningBundle:
			if bundle, ok := m.planningBundles[memoryScopedKey{workspace: workspace, id: baseID}]; ok {
				records[node] = LineageNodeRecord{Title: bundle.Title}
			}
		}
	}
	return records, nil
}

// ListLineageNodeStatuses reads each supplied node's record once
// (component-lineage).
func (m *memory) ListLineageNodeStatuses(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]core.LineageNodeStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	statuses := make(map[core.LineageNode]core.LineageNodeStatus, len(nodes))
	for _, node := range nodes {
		// Statuses are keyed by identity; a caller's label never splits a key.
		node = core.LineageNode{Type: node.Type, ID: node.ID}
		baseID, versionNumber := LineageVersionNodeParts(node)
		key := memoryScopedKey{workspace: workspace, id: baseID}
		switch node.Type {
		case core.LineageTask, core.LineageBlueprint:
			if task, ok := m.tasks[baseID]; ok && task.Workspace == workspace {
				statuses[node] = core.LineageNodeStatus{State: string(task.State)}
			}
		case core.LineageWorkOrder:
			if order, ok := m.workOrders[baseID]; ok {
				if task, exists := m.tasks[order.TaskID]; exists && task.Workspace == workspace {
					statuses[node] = core.LineageNodeStatus{State: string(order.State)}
				}
			}
		case core.LineageRequirement:
			if requirement, ok := m.requirements[key]; ok {
				statuses[node] = core.LineageNodeStatus{CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived}
			}
		case core.LineageRequirementVersion:
			requirement, ok := m.requirements[key]
			if !ok {
				continue
			}
			for _, candidate := range m.requirementVersions[key] {
				if candidate.Version == versionNumber {
					statuses[node] = core.LineageNodeStatus{
						VersionStatus:  core.LineageVersionStatus(candidate.Version, requirement.CurrentVersion, candidate.Confirmed, candidate.Retired),
						CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived,
					}
				}
			}
		case core.LineageSystemDesign:
			if document, ok := m.systemDesigns[key]; ok {
				statuses[node] = core.LineageNodeStatus{CurrentVersion: document.CurrentVersion, Archived: document.Archived}
			}
		case core.LineageSystemDesignVersion:
			document, ok := m.systemDesigns[key]
			if !ok {
				continue
			}
			for _, candidate := range m.systemDesignVersions[key] {
				if candidate.Version == versionNumber {
					statuses[node] = core.LineageNodeStatus{
						VersionStatus:  core.LineageVersionStatus(candidate.Version, document.CurrentVersion, candidate.Confirmed, candidate.Dismissed),
						CurrentVersion: document.CurrentVersion, Archived: document.Archived,
					}
				}
			}
		case core.LineageReferenceDocument:
			if document, ok := m.referenceDocuments[key]; ok {
				statuses[node] = core.LineageNodeStatus{CurrentVersion: document.CurrentVersion, Archived: !document.DeletedAt.IsZero()}
			}
		case core.LineageReferenceDocumentVersion:
			document, ok := m.referenceDocuments[key]
			if !ok {
				continue
			}
			for _, candidate := range m.referenceDocumentVersions[key] {
				if candidate.Version == versionNumber {
					statuses[node] = core.LineageNodeStatus{
						VersionStatus:  core.LineageVersionStatus(candidate.Version, document.CurrentVersion, true, false),
						CurrentVersion: document.CurrentVersion, Archived: !document.DeletedAt.IsZero(),
					}
				}
			}
		case core.LineageDecision:
			if decision, ok := m.decisions[key]; ok {
				statuses[node] = core.LineageNodeStatus{State: string(decision.Status)}
			}
		case core.LineagePlanningSession:
			if session, ok := m.planningSessions[key]; ok {
				statuses[node] = core.LineageNodeStatus{State: string(session.Status)}
			}
		case core.LineagePlanningBundle:
			if bundle, ok := m.planningBundles[key]; ok {
				statuses[node] = core.LineageNodeStatus{State: string(bundle.Status)}
			}
		case core.LineageVerdict:
			orderID := strings.TrimPrefix(node.ID, "review:")
			order, ok := m.workOrders[orderID]
			if !ok {
				continue
			}
			if task, exists := m.tasks[order.TaskID]; !exists || task.Workspace != workspace {
				continue
			}
			for _, event := range m.events[order.TaskID] {
				if event.Kind != "review.completed" {
					continue
				}
				if status, ok := LineageVerdictStatus(event.Payload, orderID); ok {
					statuses[node] = status
				}
			}
		}
	}
	return statuses, nil
}

// LineageVerdictStatus reads a verdict's outcome, round, and seat from one
// review.completed payload when it names orderID.
func LineageVerdictStatus(payload []byte, orderID string) (core.LineageNodeStatus, bool) {
	var recorded struct {
		ReviewWorkOrderID string `json:"review_work_order_id"`
		Verdict           string `json:"verdict"`
		ReviewRound       int    `json:"review_round"`
		ReviewSeat        int    `json:"review_seat"`
	}
	if json.Unmarshal(payload, &recorded) != nil || recorded.ReviewWorkOrderID != orderID {
		return core.LineageNodeStatus{}, false
	}
	return core.LineageNodeStatus{Outcome: recorded.Verdict, ReviewRound: recorded.ReviewRound, ReviewSeat: recorded.ReviewSeat}, true
}

// LineageVersionNodeParts splits a version node ID `<document>:v<n>` into
// its document ID and version. Other nodes return their ID and zero.
func LineageVersionNodeParts(node core.LineageNode) (string, int) {
	baseID := lineageRecordBaseID(node)
	if baseID == node.ID {
		return baseID, 0
	}
	version, err := strconv.Atoi(node.ID[len(baseID)+2:])
	if err != nil || version <= 0 {
		return baseID, 0
	}
	return baseID, version
}

// ListLineageKindLinks reads only the named nodes' index entries and
// retains at most query.Limit candidates while it scans, so its allocation
// follows the limit rather than a node's history (component-lineage).
func (m *memory) ListLineageKindLinks(ctx context.Context, query core.LineageKindLinkQuery) ([]core.LineageLink, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	seen := map[core.LineageNode]bool{}
	retained := &lineageKindLinkHeap{newest: query.Newest, links: make([]core.LineageLink, 0, min(query.Limit, 64))}
	for _, node := range query.Nodes {
		node = core.LineageNode{Type: node.Type, ID: node.ID}
		if seen[node] {
			continue
		}
		seen[node] = true
		m.lineage.lookups.Add(1)
		for _, link := range m.lineage.byNode[memoryLineageEndpoint{workspace: workspace, node: node}] {
			m.lineage.examined.Add(1)
			near, far := core.LineageNode{Type: link.SrcType, ID: link.SrcID}, link.DstType
			if query.Inbound {
				near, far = core.LineageNode{Type: link.DstType, ID: link.DstID}, link.SrcType
			}
			if link.Kind != query.Kind || near != node || far != query.FarType || link.Validate() != nil {
				continue
			}
			if retained.Len() < query.Limit {
				heap.Push(retained, link)
			} else if retained.keeps(link) {
				retained.links[0] = link
				heap.Fix(retained, 0)
			}
			if peak := int64(retained.Len()); peak > m.lineage.kindLinkPeak.Load() {
				m.lineage.kindLinkPeak.Store(peak)
			}
		}
	}
	out := retained.links
	core.SortLineageKindLinks(out)
	return out, nil
}

// lineageKindLinkHeap holds the retained kind-link candidates with the one
// to evict at the root: the latest in sort order when the read keeps the
// first links, the earliest when it keeps the newest.
type lineageKindLinkHeap struct {
	newest bool
	links  []core.LineageLink
}

func (h *lineageKindLinkHeap) Len() int { return len(h.links) }
func (h *lineageKindLinkHeap) Less(i, j int) bool {
	if h.newest {
		return core.LineageKindLinkBefore(h.links[i], h.links[j])
	}
	return core.LineageKindLinkBefore(h.links[j], h.links[i])
}
func (h *lineageKindLinkHeap) Swap(i, j int) { h.links[i], h.links[j] = h.links[j], h.links[i] }
func (h *lineageKindLinkHeap) Push(x any)    { h.links = append(h.links, x.(core.LineageLink)) }
func (h *lineageKindLinkHeap) Pop() any {
	last := h.links[len(h.links)-1]
	h.links = h.links[:len(h.links)-1]
	return last
}

// keeps reports whether link displaces the root of a full heap.
func (h *lineageKindLinkHeap) keeps(link core.LineageLink) bool {
	if h.newest {
		return core.LineageKindLinkBefore(h.links[0], link)
	}
	return core.LineageKindLinkBefore(link, h.links[0])
}

func (m *memory) ListLineageContextRecords(ctx context.Context, nodes []core.LineageNode) (LineageContextRecords, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	result := LineageContextRecords{
		Tasks:        map[string]LineageContextTaskRecord{},
		Requirements: map[string]LineageContextRequirementRecord{},
	}
	for _, node := range nodes {
		switch node.Type {
		case core.LineageTask:
			if _, exists := result.Tasks[node.ID]; exists {
				continue
			}
			task, ok := m.tasks[node.ID]
			if !ok || task.Workspace != workspace {
				continue
			}
			record := LineageContextTaskRecord{Title: task.Title, State: task.State, ParentTaskID: task.ParentTaskID}
			for index := len(m.events[task.ID]) - 1; core.TaskTerminal(task.State) && index >= 0; index-- {
				event := m.events[task.ID][index]
				if (event.Kind == "review.completed" || event.Kind == "review.round_completed") && validReviewSummaryPayload(event.Payload) {
					record.ReviewPayload = append(json.RawMessage(nil), event.Payload...)
					break
				}
			}
			result.Tasks[node.ID] = record
		case core.LineageRequirement:
			if _, exists := result.Requirements[node.ID]; exists {
				continue
			}
			key := memoryScopedKey{workspace: workspace, id: node.ID}
			requirement, ok := m.requirements[key]
			if !ok || requirement.CurrentVersion <= 0 {
				continue
			}
			versions := m.requirementVersions[key]
			if requirement.CurrentVersion > len(versions) {
				continue
			}
			version := versions[requirement.CurrentVersion-1]
			if !version.Confirmed {
				continue
			}
			result.Requirements[node.ID] = LineageContextRequirementRecord{
				Title: requirement.Title, Version: version.Version, Content: version.Content,
				Statements: append([]core.RequirementStatement(nil), version.Statements...), Archived: requirement.Archived,
			}
		}
	}
	return result, nil
}

func validReviewSummaryPayload(payload json.RawMessage) bool {
	var decoded struct {
		Verdict  string `json:"verdict"`
		Summary  string `json:"summary"`
		Feedback string `json:"feedback"`
	}
	return json.Unmarshal(payload, &decoded) == nil
}

func lineageRecordBaseID(node core.LineageNode) string {
	if node.Type != core.LineageBlueprintVersion && node.Type != core.LineageRequirementVersion && node.Type != core.LineageReferenceDocumentVersion && node.Type != core.LineageSystemDesignVersion {
		return node.ID
	}
	index := strings.LastIndex(node.ID, ":v")
	if index <= 0 {
		return node.ID
	}
	return node.ID[:index]
}

func (m *memory) ListRequirementDeliveryEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string][]core.Event, len(taskIDs))
	workspace, scoped := WorkspaceFromContext(ctx)
	for _, taskID := range taskIDs {
		task, exists := m.tasks[taskID]
		if !exists || scoped && task.Workspace != workspace {
			continue
		}
		events := make([]core.Event, 0)
		for _, event := range m.events[taskID] {
			switch event.Kind {
			case "merge.confirmed", "merge.reconciled", "review.round_completed", TaskContextRequirementAdded, TaskContextRequirementActive, TaskContextRequirementRemoved:
				events = append(events, event)
			}
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].At.Equal(events[j].At) {
				return events[i].ID < events[j].ID
			}
			return events[i].At.Before(events[j].At)
		})
		if len(events) > 0 {
			result[taskID] = events
		}
	}
	return result, nil
}

func (m *memory) ListLineageLinks(ctx context.Context) ([]core.LineageLink, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	links := make([]core.LineageLink, 0, m.lineage.len())
	for _, link := range m.lineage.links {
		if link.Workspace == workspace {
			links = append(links, link)
		}
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].CreatedByEventID != links[j].CreatedByEventID {
			return links[i].CreatedByEventID < links[j].CreatedByEventID
		}
		return lineageLinkKey(links[i]) < lineageLinkKey(links[j])
	})
	return links, nil
}

// LineageNodeExists is one endpoint-index lookup (component-lineage).
func (m *memory) LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error) {
	if !node.Valid() {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lineage.exists(workspaceOrDefault(ctx, ""), node), nil
}

// ListLineageNeighborhood runs the shared bounded walk, reading each depth's
// ranked link prefixes from the endpoint index (component-lineage).
func (m *memory) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	workspace := workspaceOrDefault(ctx, "")
	budget.Workspace = workspace
	return core.FetchLineageNeighborhood(ctx, roots, budget, func(_ context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
		for _, node := range nodes {
			out[node] = m.lineage.prefix(workspace, node, limit)
		}
		return out, nil
	})
}

func (m *memory) ListRequirementDeliveryLineage(ctx context.Context, requirementID string, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	if strings.TrimSpace(requirementID) == "" || budget.MaxDepth < 0 || budget.MaxNodes <= 0 || budget.MaxLinks <= 0 {
		return nil, fmt.Errorf("requirement delivery lineage requires a requirement id and bounded depth, nodes, and links")
	}
	links, err := m.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	return boundedRequirementDeliveryLinks(links, requirementID, budget), nil
}

func (m *memory) ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	out := make(map[string][]core.LineageLink, len(requirementIDs))
	for _, requirementID := range requirementIDs {
		links, err := m.ListRequirementDeliveryLineage(ctx, requirementID, budget)
		if err != nil {
			return nil, err
		}
		out[requirementID] = links
	}
	return out, nil
}

func boundedRequirementDeliveryLinks(links []core.LineageLink, requirementID string, budget core.LineageTraversalBudget) []core.LineageLink {
	workspace := budget.Workspace
	eligible := make(map[core.LineageNode][]core.LineageLink)
	for _, link := range links {
		if workspace != "" && link.Workspace != workspace || !requirementDeliveryLink(link) {
			continue
		}
		src := core.LineageNode{Type: link.SrcType, ID: link.SrcID}
		eligible[src] = append(eligible[src], link)
	}
	for src := range eligible {
		sort.Slice(eligible[src], func(i, j int) bool { return lineageLinkKey(eligible[src][i]) < lineageLinkKey(eligible[src][j]) })
	}

	type step struct {
		node  core.LineageNode
		depth int
	}
	queue := []step{{node: core.LineageNode{Type: core.LineageRequirement, ID: requirementID}}}
	seen := map[core.LineageNode]bool{queue[0].node: true}
	result := make([]core.LineageLink, 0, min(budget.MaxLinks+1, len(links)))
	for len(queue) > 0 && len(result) <= budget.MaxLinks {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= budget.MaxDepth {
			continue
		}
		for _, link := range eligible[current.node] {
			result = append(result, link)
			if len(result) > budget.MaxLinks {
				break
			}
			dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
			if !seen[dst] {
				seen[dst] = true
				queue = append(queue, step{node: dst, depth: current.depth + 1})
			}
		}
	}
	return result
}

func requirementDeliveryLink(link core.LineageLink) bool {
	return link.Kind == "serves" && link.SrcType == core.LineageRequirement &&
		(link.DstType == core.LineageTask || link.DstType == core.LineageBlueprint) ||
		link.Kind == "versions" && link.SrcType == core.LineageBlueprint && link.DstType == core.LineageBlueprintVersion ||
		link.Kind == "materializes" && link.SrcType == core.LineageBlueprintVersion && link.DstType == core.LineageTask
}

func (m *memory) RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.LineageRebuildResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.RequestID) == "" {
		return core.LineageRebuildResult{}, fmt.Errorf("%w: reason and request_id are required", ErrLineageRebuildValidation)
	}
	workspace := workspaceOrDefault(ctx, "")
	result := core.LineageRebuildResult{}
	for _, event := range m.events[""] {
		if event.Kind != "lineage.rebuilt" {
			continue
		}
		var payload struct {
			WorkspaceID string                    `json:"workspace_id"`
			RequestID   string                    `json:"request_id"`
			Reason      string                    `json:"reason"`
			Result      core.LineageRebuildResult `json:"result"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkspaceID == workspace && payload.RequestID == request.RequestID {
			if payload.Reason != request.Reason {
				return core.LineageRebuildResult{}, fmt.Errorf("%w: request_id %s was already used for a different reason", ErrLineageRebuildConflict, request.RequestID)
			}
			return payload.Result, nil
		}
	}
	replayable := map[string]core.LineageLink{}
	suppressed := map[string]struct{}{}
	candidateEvent := map[string]int64{}
	ambiguous := map[string]struct{}{}
	var workspaceEvents []core.Event
	for _, events := range m.events {
		for _, event := range events {
			if event.TaskID != "" {
				task, ok := m.tasks[event.TaskID]
				if !ok || task.Workspace != workspace {
					continue
				}
			} else {
				var payload struct {
					WorkspaceID string `json:"workspace_id"`
				}
				if json.Unmarshal(event.Payload, &payload) != nil || payload.WorkspaceID != workspace {
					continue
				}
			}
			workspaceEvents = append(workspaceEvents, event)
		}
	}
	sort.Slice(workspaceEvents, func(i, j int) bool { return workspaceEvents[i].ID < workspaceEvents[j].ID })
	replay := NewLineageReplay()
	for _, event := range workspaceEvents {
		projection := replay.Project(workspace, event)
		links := projection.Links
		if requirementID, version, historical := HistoricalRequirementConfirmation(event); historical {
			predecessor := 0
			for _, candidate := range m.requirementVersions[memoryScopedKey{workspace: workspace, id: requirementID}] {
				if candidate.Confirmed && candidate.Version < version && candidate.Version > predecessor {
					predecessor = candidate.Version
				}
			}
			links = LineageLinksForHistoricalConfirmation(workspace, event, predecessor)
		}
		result.Unsupported += projection.Unsupported
		for _, link := range projection.Suppresses {
			key := lineageLinkKey(link)
			delete(replayable, key)
			delete(candidateEvent, key)
			delete(ambiguous, key)
			suppressed[key] = struct{}{}
		}
		for _, link := range links {
			key := lineageLinkKey(link)
			delete(suppressed, key)
			if first, ok := candidateEvent[key]; ok && first != link.CreatedByEventID {
				ambiguous[key] = struct{}{}
			} else if !ok {
				candidateEvent[key] = link.CreatedByEventID
			}
			if prior, ok := replayable[key]; !ok || link.CreatedByEventID < prior.CreatedByEventID {
				replayable[key] = link
			}
		}
	}
	preserved := map[string]core.LineageLink{}
	stored := make(map[string]core.LineageLink, m.lineage.len())
	for key, link := range m.lineage.links {
		stored[key] = link
	}
	for key, link := range stored {
		if link.Workspace == workspace && link.CreatedByEventID != 0 && projectorOwnsLineageKind(link.Kind) {
			_, isSuppressed := suppressed[key]
			if _, regenerable := replayable[key]; !regenerable && !isSuppressed {
				preserved[key] = link
			}
			m.lineage.remove(key)
		} else if link.Workspace == workspace {
			if _, regenerated := replayable[key]; !regenerated {
				result.Existing++
			}
		}
	}
	for _, link := range replayable {
		m.lineage.put(link)
	}
	for _, link := range preserved {
		m.lineage.put(link)
	}
	result.Projected = len(replayable)
	result.PreservedUnregenerable = len(preserved)
	result.Ambiguous = len(ambiguous)
	m.appendEventLocked(ctx, core.Event{Kind: "lineage.rebuilt", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "reason": request.Reason, "request_id": request.RequestID, "result": result,
	})})
	return result, nil
}

// LineageStatusRows carries the batched record columns a SQL backend reads
// for ListLineageNodeStatuses, so every backend assembles statuses with the
// same rules (component-lineage).
type LineageStatusRows struct {
	TaskStates    map[string]string
	OrderStates   map[string]string
	DecisionState map[string]string
	SessionState  map[string]string
	BundleState   map[string]string
	// Documents and Versions are keyed by the document node type:
	// requirement, system_design, or reference_document.
	Documents map[core.LineageNodeType]map[string]LineageStatusDocument
	Versions  map[core.LineageNodeType]map[string]map[int]LineageStatusVersion
	// Verdicts is keyed by review work order ID.
	Verdicts map[string]core.LineageNodeStatus
}

type LineageStatusDocument struct {
	CurrentVersion int
	Archived       bool
}

type LineageStatusVersion struct {
	Confirmed bool
	// Dismissed is a dismissed design version or a retired requirement version.
	Dismissed bool
}

// LineageVersionPair names one document version a status read selects.
type LineageVersionPair struct {
	DocumentID string
	Version    int
}

// LineageStatusRequest is what a SQL status read selects. IDs groups the
// record IDs by node class: task, work_order, decision, planning_session,
// planning_bundle, requirement, system_design, reference_document, and
// verdict (review work order IDs). Versions lists, per document class, only
// the version pairs that version nodes name, so a document's other versions
// are never read (component-lineage).
type LineageStatusRequest struct {
	IDs      map[core.LineageNodeType][]string
	Versions map[core.LineageNodeType][]LineageVersionPair
}

// LineageStatusIDs builds the status request for nodes.
func LineageStatusIDs(nodes []core.LineageNode) LineageStatusRequest {
	seen := map[core.LineageNodeType]map[string]bool{}
	seenPair := map[core.LineageNodeType]map[LineageVersionPair]bool{}
	request := LineageStatusRequest{IDs: map[core.LineageNodeType][]string{}, Versions: map[core.LineageNodeType][]LineageVersionPair{}}
	add := func(class core.LineageNodeType, id string) {
		if seen[class] == nil {
			seen[class] = map[string]bool{}
		}
		if id != "" && !seen[class][id] {
			seen[class][id] = true
			request.IDs[class] = append(request.IDs[class], id)
		}
	}
	addVersion := func(class core.LineageNodeType, id string, version int) {
		add(class, id)
		pair := LineageVersionPair{DocumentID: id, Version: version}
		if seenPair[class] == nil {
			seenPair[class] = map[LineageVersionPair]bool{}
		}
		if id != "" && version > 0 && !seenPair[class][pair] {
			seenPair[class][pair] = true
			request.Versions[class] = append(request.Versions[class], pair)
		}
	}
	for _, node := range nodes {
		baseID, version := LineageVersionNodeParts(node)
		switch node.Type {
		case core.LineageTask, core.LineageBlueprint:
			add(core.LineageTask, baseID)
		case core.LineageWorkOrder, core.LineageDecision, core.LineagePlanningSession, core.LineagePlanningBundle:
			add(node.Type, baseID)
		case core.LineageRequirement, core.LineageSystemDesign, core.LineageReferenceDocument:
			add(node.Type, baseID)
		case core.LineageRequirementVersion:
			addVersion(core.LineageRequirement, baseID, version)
		case core.LineageSystemDesignVersion:
			addVersion(core.LineageSystemDesign, baseID, version)
		case core.LineageReferenceDocumentVersion:
			// Reference versions classify from the document row alone.
			add(core.LineageReferenceDocument, baseID)
		case core.LineageVerdict:
			add(core.LineageVerdict, strings.TrimPrefix(node.ID, "review:"))
		}
	}
	for class := range request.IDs {
		sort.Strings(request.IDs[class])
	}
	for class := range request.Versions {
		pairs := request.Versions[class]
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].DocumentID != pairs[j].DocumentID {
				return pairs[i].DocumentID < pairs[j].DocumentID
			}
			return pairs[i].Version < pairs[j].Version
		})
	}
	return request
}

// Statuses assembles each supplied node's status from the batched rows.
func (rows LineageStatusRows) Statuses(nodes []core.LineageNode) map[core.LineageNode]core.LineageNodeStatus {
	statuses := make(map[core.LineageNode]core.LineageNodeStatus, len(nodes))
	state := func(node core.LineageNode, states map[string]string, id string) {
		if value, ok := states[id]; ok {
			statuses[node] = core.LineageNodeStatus{State: value}
		}
	}
	documentType := map[core.LineageNodeType]core.LineageNodeType{
		core.LineageRequirementVersion: core.LineageRequirement, core.LineageSystemDesignVersion: core.LineageSystemDesign,
		core.LineageReferenceDocumentVersion: core.LineageReferenceDocument,
	}
	for _, node := range nodes {
		node = core.LineageNode{Type: node.Type, ID: node.ID}
		baseID, version := LineageVersionNodeParts(node)
		switch node.Type {
		case core.LineageTask, core.LineageBlueprint:
			state(node, rows.TaskStates, baseID)
		case core.LineageWorkOrder:
			state(node, rows.OrderStates, baseID)
		case core.LineageDecision:
			state(node, rows.DecisionState, baseID)
		case core.LineagePlanningSession:
			state(node, rows.SessionState, baseID)
		case core.LineagePlanningBundle:
			state(node, rows.BundleState, baseID)
		case core.LineageRequirement, core.LineageSystemDesign, core.LineageReferenceDocument:
			if document, ok := rows.Documents[node.Type][baseID]; ok {
				statuses[node] = core.LineageNodeStatus{CurrentVersion: document.CurrentVersion, Archived: document.Archived}
			}
		case core.LineageRequirementVersion, core.LineageSystemDesignVersion, core.LineageReferenceDocumentVersion:
			docType := documentType[node.Type]
			document, ok := rows.Documents[docType][baseID]
			if !ok || version == 0 {
				continue
			}
			recorded, ok := rows.Versions[docType][baseID][version]
			if docType == core.LineageReferenceDocument {
				// Reference versions are informative uploads; each is
				// current or superseded by a later upload.
				recorded, ok = LineageStatusVersion{Confirmed: true}, version <= document.CurrentVersion
			}
			if ok {
				statuses[node] = core.LineageNodeStatus{
					VersionStatus:  core.LineageVersionStatus(version, document.CurrentVersion, recorded.Confirmed, recorded.Dismissed),
					CurrentVersion: document.CurrentVersion, Archived: document.Archived,
				}
			}
		case core.LineageVerdict:
			if status, ok := rows.Verdicts[strings.TrimPrefix(node.ID, "review:")]; ok {
				statuses[node] = status
			}
		}
	}
	return statuses
}
