package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestMCPListEnvironments proves component-environments invariant 17 and
// req-environments AC-1.10: the read returns each environment row without
// keys, archived ones included, in a stable order, and changes no record.
func TestMCPListEnvironments(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	production := h.create(`{"name":"production","tracked_branches":{"app":"main"},"github_environments":["prod"],"version_rule":{"kind":"tag_template","text":"v{version}"},"timing_window_minutes":90}`)
	key := h.issue(production.ID)
	h.create(`{"name":"Archived","archived":true}`)
	h.create(`{"name":"Staging"}`)
	before := h.recorded()
	viewer := h.tokens[core.WorkspaceRoleViewer]
	page := readAs(t, h.handler, viewer, "list_environments", map[string]any{"workspace_id": "alpha"})
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("page = %+v", page)
	}
	fields := slices.DeleteFunc(slices.Clone(environmentRowFields), func(field string) bool { return field == "keys" })
	var names []string
	for _, item := range page.Items {
		if got := environmentBodyKeys(t, item); !slices.Equal(got, fields) {
			t.Fatalf("MCP row fields = %v, want %v", got, fields)
		}
		var row struct {
			Name     string `json:"name"`
			Archived bool   `json:"archived"`
		}
		if err := json.Unmarshal(item, &row); err != nil {
			t.Fatal(err)
		}
		if row.Archived != (row.Name == "Archived") {
			t.Fatalf("archived state of %s = %v", row.Name, row.Archived)
		}
		names = append(names, row.Name)
		for _, forbidden := range []string{key.ID, key.Value, "deploy", "cv_env_", "envkey_"} {
			if strings.Contains(string(item), forbidden) {
				t.Fatalf("an MCP row holds key data: %s", item)
			}
		}
	}
	if !slices.Equal(names, []string{"Archived", "production", "Staging"}) {
		t.Fatalf("order = %v", names)
	}
	var row map[string]any
	if err := json.Unmarshal(page.Items[1], &row); err != nil {
		t.Fatal(err)
	}
	if row["id"] != production.ID || row["timing_window_minutes"] != float64(90) || fmt.Sprint(row["tracked_branches"]) != "map[app:main]" || fmt.Sprint(row["github_environments"]) != "[prod]" || fmt.Sprint(row["version_rule"]) != "map[kind:tag_template text:v{version}]" {
		t.Fatalf("MCP row = %v", row)
	}

	// Pages of one snapshot are bounded and stay stable across a later edit.
	first := readAs(t, h.handler, viewer, "list_environments", map[string]any{"workspace_id": "alpha", "limit": 1})
	if len(first.Items) != 1 || first.NextOffset == nil || first.Snapshot == "" {
		t.Fatalf("first page = %+v", first)
	}
	if after := h.recorded(); after != before {
		t.Fatalf("the read changed a record:\nbefore %s\nafter  %s", before, after)
	}
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+production.ID, `{"name":"Renamed"}`)
	second := readAs(t, h.handler, viewer, "list_environments", map[string]any{"workspace_id": "alpha", "limit": 1, "offset": 1, "snapshot": first.Snapshot})
	if len(second.Items) != 1 || !strings.Contains(string(second.Items[0]), `"production"`) {
		t.Fatalf("second page = %s", second.Items)
	}

	// A workspace the caller cannot view answers like a missing one, before
	// any read, and every page reauthorizes.
	for _, c := range []struct{ token, workspace string }{{h.foreign, "alpha"}, {viewer, "beta"}, {viewer, "missing"}} {
		if text := refusalAs(t, h.handler, c.token, "list_environments", map[string]any{"workspace_id": c.workspace}); !strings.Contains(text, "workspace_not_found") {
			t.Fatalf("refusal = %s", text)
		}
	}
	for _, args := range []map[string]any{{}, {"workspace_id": "alpha", "archived": true}, {"workspace_id": "alpha", "limit": 101}} {
		refusalAs(t, h.handler, viewer, "list_environments", args)
	}
	if _, _, _, err := mcpReadTry(h.handler, key.Value, "list_environments", map[string]any{"workspace_id": "alpha"}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("a key on /mcp: %v", err)
	}
	if beta := readAs(t, h.handler, h.foreign, "list_environments", map[string]any{"workspace_id": "beta"}); beta.Total != 0 {
		t.Fatalf("the other workspace lists %+v", beta)
	}

	// A failed read carries no partial row.
	h.server.Store = failingEnvironmentStore{Backend: h.st, err: errors.New("fixture environment failure")}
	refusalAs(t, h.server.Handler(), viewer, "list_environments", map[string]any{"workspace_id": "alpha"})
}

// TestMCPHasOneEnvironmentTool proves that no MCP tool creates or edits an
// environment or a key, and that the one read is declared read-only.
func TestMCPHasOneEnvironmentTool(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	response := h.do(h.manager(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Annotations map[string]bool `json:"annotations"`
				InputSchema struct {
					Required             []string       `json:"required"`
					Properties           map[string]any `json:"properties"`
					AdditionalProperties bool           `json:"additionalProperties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	decodeEnvironmentBody(t, response, &envelope)
	found := 0
	for _, tool := range envelope.Result.Tools {
		if !strings.Contains(tool.Name, "environment") {
			continue
		}
		found++
		if tool.Name != "list_environments" || !tool.Annotations["readOnlyHint"] || tool.Annotations["destructiveHint"] || !slices.Equal(tool.InputSchema.Required, []string{"workspace_id"}) || len(tool.InputSchema.Properties) != 4 || tool.InputSchema.AdditionalProperties {
			t.Fatalf("environment tool = %+v", tool)
		}
	}
	if found != 1 {
		t.Fatalf("%d environment tools, want one", found)
	}
}
