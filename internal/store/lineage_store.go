package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

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
		}
	}
	return records, nil
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
				Statements: append([]core.RequirementStatement(nil), version.Statements...),
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
	links := make([]core.LineageLink, 0, len(m.lineage))
	for _, link := range m.lineage {
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

func (m *memory) LineageNodeExists(ctx context.Context, node core.LineageNode) (bool, error) {
	if !node.Valid() {
		return false, nil
	}
	links, err := m.ListLineageLinks(ctx)
	if err != nil {
		return false, err
	}
	for _, link := range links {
		if (link.SrcType == node.Type && link.SrcID == node.ID) || (link.DstType == node.Type && link.DstID == node.ID) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memory) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	links, err := m.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	selected := map[string]core.LineageLink{}
	for _, root := range roots {
		walk, walkErr := core.TraverseLineage(links, []core.LineageNode{root}, core.LineageTraversalBudget{MaxDepth: budget.MaxDepth, MaxNodes: budget.MaxNodes, Workspace: workspaceOrDefault(ctx, "")})
		if walkErr != nil {
			return nil, walkErr
		}
		nodes := make(map[core.LineageNode]bool, len(walk.Nodes))
		for _, node := range walk.Nodes {
			nodes[node] = true
		}
		for _, link := range links {
			if nodes[core.LineageNode{Type: link.SrcType, ID: link.SrcID}] || nodes[core.LineageNode{Type: link.DstType, ID: link.DstID}] {
				selected[lineageLinkKey(link)] = link
			}
		}
	}
	result := make([]core.LineageLink, 0, len(selected))
	for _, link := range selected {
		result = append(result, link)
	}
	sort.Slice(result, func(i, j int) bool { return lineageLinkKey(result[i]) < lineageLinkKey(result[j]) })
	return result, nil
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
	for _, event := range workspaceEvents {
		projection := projectLineageEvent(workspace, event)
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
	for key, link := range m.lineage {
		if link.Workspace == workspace && link.CreatedByEventID != 0 && projectorOwnsLineageKind(link.Kind) {
			_, isSuppressed := suppressed[key]
			if _, regenerable := replayable[key]; !regenerable && !isSuppressed {
				preserved[key] = link
			}
			delete(m.lineage, key)
		} else if link.Workspace == workspace {
			if _, regenerated := replayable[key]; !regenerated {
				result.Existing++
			}
		}
	}
	for key, link := range replayable {
		m.lineage[key] = link
	}
	for key, link := range preserved {
		m.lineage[key] = link
	}
	result.Projected = len(replayable)
	result.PreservedUnregenerable = len(preserved)
	result.Ambiguous = len(ambiguous)
	m.appendEventLocked(ctx, core.Event{Kind: "lineage.rebuilt", Payload: core.JSONPayload(map[string]any{
		"workspace_id": workspace, "reason": request.Reason, "request_id": request.RequestID, "result": result,
	})})
	return result, nil
}
