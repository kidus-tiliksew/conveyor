package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) rebuildLineage(w http.ResponseWriter, r *http.Request) {
	var request core.LineageRebuildRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.RequestID) == "" {
		http.Error(w, "lineage rebuild reason and request_id are required", http.StatusBadRequest)
		return
	}
	result, err := s.Store.RebuildLineage(r.Context(), request)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrLineageRebuildValidation) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, store.ErrLineageRebuildConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// getLineage exposes the same bounded, deterministic graph walk used for
// agent context. The graph remains a read-only projection of events; callers
// cannot volunteer edges through this surface (spec §4.2 item 4, §16).
func (s *Server) getLineage(w http.ResponseWriter, r *http.Request) {
	root := core.LineageNode{
		Type: core.LineageNodeType(strings.TrimSpace(chi.URLParam(r, "type"))),
		ID:   strings.TrimSpace(chi.URLParam(r, "id")),
	}
	if !root.Valid() {
		http.Error(w, "a valid lineage node type and id are required", http.StatusBadRequest)
		return
	}
	effective, err := s.effectiveLineageBudget(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	maxDepth, err := boundedLineageQueryValue(r, "max_depth", effective.Depth)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxNodes, err := boundedLineageQueryValue(r, "max_nodes", effective.Nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxLinks, err := boundedLineageQueryValue(r, "max_links", effective.Links)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if maxDepth > effective.Depth || maxNodes > effective.Nodes || maxLinks > effective.Links {
		http.Error(w, "lineage query exceeds the context traversal budget", http.StatusBadRequest)
		return
	}
	exists, err := s.lineageNodeExists(r, root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "lineage node not found", http.StatusNotFound)
		return
	}
	graph, err := s.lineageGraph(r, root, core.LineageTraversalBudget{MaxDepth: maxDepth, MaxNodes: maxNodes, MaxLinks: maxLinks})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

func boundedLineageQueryValue(r *http.Request, name string, fallback int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func (s *Server) lineageGraph(r *http.Request, root core.LineageNode, budget core.LineageTraversalBudget) (core.LineageTraversal, error) {
	workspace, _ := store.WorkspaceFromContext(r.Context())
	budget.Workspace = workspace
	fetchBudget := budget
	fetchBudget.MaxDepth++
	fetchBudget.MaxNodes++
	fetchBudget.MaxLinks++
	links, err := s.Store.ListLineageNeighborhood(r.Context(), []core.LineageNode{root}, fetchBudget)
	if err != nil {
		return core.LineageTraversal{}, err
	}
	graph, err := core.TraverseLineage(links, []core.LineageNode{root}, budget)
	if err != nil {
		return core.LineageTraversal{}, err
	}
	for index := range graph.Nodes {
		graph.Nodes[index].Label = s.lineageNodeLabel(r, graph.Nodes[index])
	}
	for index := range graph.Roots {
		graph.Roots[index].Label = s.lineageNodeLabel(r, graph.Roots[index])
	}
	return graph, nil
}

func (s *Server) effectiveLineageBudget(ctx context.Context) (lineagecontext.Budget, error) {
	if s.ConfigProvider == nil {
		return lineagecontext.BudgetFromConfig(nil), nil
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return lineagecontext.Budget{}, err
	}
	return lineagecontext.BudgetFromConfig(cfg), nil
}

func (s *Server) lineageNodeExists(r *http.Request, node core.LineageNode) (bool, error) {
	found := false
	switch node.Type {
	case core.LineageTask, core.LineageBlueprint:
		_, err := s.Store.GetTask(r.Context(), node.ID)
		found = err == nil
	case core.LineageRequirement:
		_, err := s.Store.GetRequirement(r.Context(), node.ID)
		found = err == nil
	case core.LineagePlanningSession:
		_, err := s.Store.GetPlanningSession(r.Context(), node.ID)
		found = err == nil
	case core.LineageWorkOrder:
		_, err := s.Store.GetWorkOrder(r.Context(), node.ID)
		found = err == nil
	case core.LineageEvidence:
		_, _, err := s.Store.GetArtifact(r.Context(), node.ID)
		found = err == nil
	case core.LineageBlueprintVersion:
		id, version, ok := versionNodeID(node.ID)
		if ok {
			_, exists, err := s.Store.GetSpecVersion(r.Context(), id, version)
			if err != nil {
				return false, err
			}
			found = exists
		}
	case core.LineageRequirementVersion:
		id, version, ok := versionNodeID(node.ID)
		if ok {
			_, err := s.Store.GetRequirementVersion(r.Context(), id, version)
			found = err == nil
		}
	}
	if found {
		return true, nil
	}
	links, err := s.Store.ListLineageLinks(r.Context())
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

func (s *Server) lineageNodeLabel(r *http.Request, node core.LineageNode) string {
	switch node.Type {
	case core.LineageTask, core.LineageBlueprint:
		if task, err := s.Store.GetTask(r.Context(), node.ID); err == nil {
			return firstLabel(task.Title, node.ID)
		}
	case core.LineageRequirement:
		if requirement, err := s.Store.GetRequirement(r.Context(), node.ID); err == nil {
			return firstLabel(requirement.Title, requirement.Slug, node.ID)
		}
	case core.LineagePlanningSession:
		if session, err := s.Store.GetPlanningSession(r.Context(), node.ID); err == nil {
			return firstLabel(session.Title, "Planning session "+node.ID)
		}
	case core.LineageWorkOrder:
		if order, err := s.Store.GetWorkOrder(r.Context(), node.ID); err == nil {
			return fmt.Sprintf("%s work order for %s", order.Stage, order.TaskID)
		}
	case core.LineageEvidence:
		if artifact, _, err := s.Store.GetArtifact(r.Context(), node.ID); err == nil {
			return firstLabel(artifact.Name, "Evidence "+node.ID)
		}
	case core.LineageBlueprintVersion:
		if id, version, ok := versionNodeID(node.ID); ok {
			if task, err := s.Store.GetTask(r.Context(), id); err == nil {
				return fmt.Sprintf("%s blueprint v%d", firstLabel(task.Title, id), version)
			}
		}
	case core.LineageRequirementVersion:
		if id, version, ok := versionNodeID(node.ID); ok {
			if requirement, err := s.Store.GetRequirement(r.Context(), id); err == nil {
				return fmt.Sprintf("%s requirement v%d", firstLabel(requirement.Title, id), version)
			}
		}
	case core.LineagePullRequest:
		return "Pull request " + node.ID
	case core.LineageCommitRange:
		return "Commit range " + node.ID
	case core.LineageVerdict:
		return "Review verdict " + strings.TrimPrefix(node.ID, "review:")
	}
	return strings.ReplaceAll(string(node.Type), "_", " ") + " " + node.ID
}

func versionNodeID(value string) (string, int, bool) {
	index := strings.LastIndex(value, ":v")
	if index <= 0 {
		return "", 0, false
	}
	version, err := strconv.Atoi(value[index+2:])
	return value[:index], version, err == nil && version > 0
}
func firstLabel(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Unknown lineage node"
}
