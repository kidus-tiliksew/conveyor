package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
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
		http.Error(w, err.Error(), http.StatusConflict)
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
	maxDepth, err := boundedLineageQueryValue(r, "max_depth", core.ContextLineageMaxDepth)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxNodes, err := boundedLineageQueryValue(r, "max_nodes", core.ContextLineageMaxNodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if maxDepth > core.ContextLineageMaxDepth || maxNodes > core.ContextLineageMaxNodes {
		http.Error(w, "lineage query exceeds the context traversal budget", http.StatusBadRequest)
		return
	}
	graph, err := s.lineageGraph(r, root, core.LineageTraversalBudget{MaxDepth: maxDepth, MaxNodes: maxNodes})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(graph.Links) == 0 {
		http.Error(w, "lineage node not found", http.StatusNotFound)
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
	links, err := s.Store.ListLineageNeighborhood(r.Context(), []core.LineageNode{root}, budget)
	if err != nil {
		return core.LineageTraversal{}, err
	}
	return core.TraverseLineage(links, []core.LineageNode{root}, budget)
}
