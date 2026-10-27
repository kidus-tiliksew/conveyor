package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// mcpLineageSummary is the get_lineage traversal record without its node and
// link lists: the budget, truncation, and per-axis omissions every page
// repeats (component-mcp-investigation-reads; component-lineage).
type mcpLineageSummary struct {
	Roots                        []core.LineageNode          `json:"roots"`
	Budget                       core.LineageTraversalBudget `json:"budget"`
	Truncated                    bool                        `json:"truncated"`
	OmittedNodes                 int                         `json:"omitted_nodes,omitempty"`
	OmittedLinks                 int                         `json:"omitted_links,omitempty"`
	ExhaustionReasons            []string                    `json:"exhaustion_reasons,omitempty"`
	Omissions                    core.LineageOmissions       `json:"omissions"`
	ForeignWorkspaceLinksIgnored int                         `json:"foreign_workspace_links_ignored,omitempty"`
	Links                        int                         `json:"links"`
}

// mcpLineageRead runs the HTTP walk with depth, path, and status included.
// Admission, membership, and snapshot rules are callMCPRead's; this reads
// only the walk the arguments name (component-mcp-investigation-reads).
func (s *Server) mcpLineageRead(r *http.Request, args map[string]any) ([]any, json.RawMessage, error) {
	request := lineageWalkRequest{
		Root:         core.LineageNode{Type: core.LineageNodeType(readString(args, "root_type")), ID: strings.TrimSpace(readString(args, "root_id"))},
		IncludeDepth: true, IncludePath: true, IncludeStatus: true,
	}
	for name, target := range map[string]*int{"max_depth": &request.MaxDepth, "max_nodes": &request.MaxNodes, "max_links": &request.MaxLinks} {
		if value, ok := args[name]; ok {
			raw, _ := json.Marshal(value)
			if json.Unmarshal(raw, target) != nil {
				return nil, nil, fmt.Errorf("invalid %s", name)
			}
		}
	}
	request.Filter.Direction = core.LineageDirection(readString(args, "direction"))
	for _, kind := range strings.Split(readString(args, "edge_kind"), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			request.Filter.Kinds = append(request.Filter.Kinds, kind)
		}
	}
	for _, nodeType := range strings.Split(readString(args, "node_type"), ",") {
		if nodeType = strings.TrimSpace(nodeType); nodeType != "" {
			request.Filter.NodeTypes = append(request.Filter.NodeTypes, core.LineageNodeType(nodeType))
		}
	}
	if err := request.validateFilter(); err != nil {
		return nil, nil, err
	}
	result, err := s.walkLineage(r, request)
	var refused lineageRequestError
	switch {
	case errors.As(err, &refused):
		return nil, nil, refused
	case errors.Is(err, errLineageRootNotFound), errors.Is(err, errLineageConfigurationUnusable):
		return nil, nil, err
	case err != nil:
		return nil, nil, fmt.Errorf("lineage read failed")
	}
	view := result.(lineageGraphView)
	summary, err := json.Marshal(mcpLineageSummary{
		Roots: view.Roots, Budget: view.Budget, Truncated: view.Truncated, OmittedNodes: view.OmittedNodes, OmittedLinks: view.OmittedLinks,
		ExhaustionReasons: view.ExhaustionReasons, Omissions: view.Omissions, ForeignWorkspaceLinksIgnored: view.ForeignWorkspaceLinksIgnored, Links: len(view.Links),
	})
	if err != nil {
		return nil, nil, err
	}
	items := make([]any, 0, len(view.Nodes))
	for _, node := range view.Nodes {
		items = append(items, node)
	}
	return items, summary, nil
}
