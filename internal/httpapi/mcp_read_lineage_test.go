package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMCPReadLineageWalkBudgetAndAdmission proves get_lineage returns the
// HTTP walk's nodes with depth, edge path, and status, repeats the traversal
// summary on every snapshot page, requires a workspace, and refuses a budget
// above the operator read budget (component-mcp-investigation-reads;
// component-lineage; req-260802-72fc68 REQ-5).
func TestMCPReadLineageWalkBudgetAndAdmission(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	s.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 3, Nodes: 8, OperatorReadNodes: 12}}}}}, nil
	}
	task := core.Task{ID: "task-mcp-lineage", Workspace: "demo", Title: "MCP lineage", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-mcp-lineage", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := s.Store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{task.ID + "-implement-1", task.ID + "-review-1"} {
		if err := s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": id})}); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "direction": "out", "node_type": "work_order,blueprint", "limit": 2}
	page := mustRead(t, s, "get_lineage", args)
	// Out of the task are its two work orders; its blueprint plans the task,
	// an in-link, so the out walk leaves it behind.
	if page.Total != 3 || len(page.Items) != 2 || page.NextOffset == nil || page.Snapshot == "" {
		t.Fatalf("first page=%+v", page)
	}
	root := readItem(t, page, 0)
	if root["id"] != task.ID || root["depth"] != float64(0) || root["status"].(map[string]any)["state"] != string(core.TaskRunning) {
		t.Fatalf("root item=%v", root)
	}
	child := readItem(t, page, 1)
	if child["depth"] != float64(1) || len(child["edge_path"].([]any)) != 1 {
		t.Fatalf("child item=%v", child)
	}
	var summary mcpLineageSummary
	if err := json.Unmarshal(page.Traversal, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Budget.MaxNodes != 8 || summary.Budget.Filter == nil || summary.Budget.Filter.Direction != core.LineageDirectionOut || summary.Links != 2 || summary.Truncated {
		t.Fatalf("traversal summary=%+v", summary)
	}
	next := mustRead(t, s, "get_lineage", map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "direction": "out", "node_type": "work_order,blueprint", "limit": 2, "offset": 2, "snapshot": page.Snapshot})
	if len(next.Items) != 1 || next.NextOffset != nil || string(next.Traversal) != string(page.Traversal) {
		t.Fatalf("snapshot page=%+v", next)
	}
	wide := mustRead(t, s, "get_lineage", map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "max_nodes": 12})
	if err := json.Unmarshal(wide.Traversal, &summary); err != nil || summary.Budget.MaxNodes != 12 {
		t.Fatalf("operator budget summary=%+v err=%v", summary, err)
	}
	for name, refused := range map[string]struct {
		args map[string]any
		want string
	}{
		"workspace required": {map[string]any{"root_type": "task", "root_id": task.ID}, "workspace_id is required"},
		"over budget":        {map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "max_nodes": 13}, "operator read budget"},
		"over ceiling":       {map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "max_depth": config.MaxLineageOperatorReadDepth + 1}, "invalid max_depth"},
		"unknown kind":       {map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID, "edge_kind": "volunteered"}, "unknown lineage edge kind"},
		"unknown root type":  {map[string]any{"workspace_id": "demo", "root_type": "feature", "root_id": task.ID}, "valid lineage node type"},
		"absent root":        {map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": "absent"}, "lineage node not found"},
		"foreign workspace":  {map[string]any{"workspace_id": "private", "root_type": "task", "root_id": task.ID}, "workspace_not_found"},
	} {
		if _, err := mcpReadCall(t, s, "reader", "get_lineage", refused.args); !strings.Contains(err, refused.want) {
			t.Fatalf("%s: error=%q want %q", name, err, refused.want)
		}
	}
	for _, token := range []string{"agent", "child"} {
		if _, err := mcpReadCall(t, s, token, "get_lineage", map[string]any{"workspace_id": "demo", "root_type": "task", "root_id": task.ID}); !strings.Contains(err, "operator-scoped user credential") {
			t.Fatalf("%s credential error=%q", token, err)
		}
	}
	if links, err := s.Store.ListLineageLinks(store.WithWorkspace(ctx, "demo")); err != nil || len(links) != 3 {
		t.Fatalf("read changed lineage: links=%d err=%v", len(links), err)
	}
}
