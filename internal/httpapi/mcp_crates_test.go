package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateMCPRow is the MCP crate row as a session decodes it.
type crateMCPRow struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	TargetDate      *string  `json:"target_date"`
	State           string   `json:"state"`
	Shipped         bool     `json:"shipped"`
	CreatedBy       string   `json:"created_by"`
	OpenTaskCount   int      `json:"open_task_count"`
	MergedTaskCount int      `json:"merged_task_count"`
	TotalTaskCount  int      `json:"total_task_count"`
	RequirementIDs  []string `json:"requirement_ids"`
	TaskIDs         []string `json:"task_ids"`
}

func crateMCPRows(t *testing.T, page mcpReadPage) []crateMCPRow {
	t.Helper()
	rows := make([]crateMCPRow, len(page.Items))
	for i, item := range page.Items {
		if err := json.Unmarshal(item, &rows[i]); err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(item, &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"id", "name", "target_date", "state", "shipped", "created_by", "created_at", "closed_at", "open_task_count", "merged_task_count", "total_task_count", "requirement_ids", "task_ids"} {
			if _, found := keys[key]; !found {
				t.Fatalf("crate row lacks %s: %s", key, item)
			}
		}
	}
	return rows
}

func crateRowIDs(rows []crateMCPRow) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}

// TestMCPCrateReads: list_crates and get_crate return crate rows with name,
// target date, state, declared requirement IDs, and member task IDs to a
// person who may view the workspace, keep each workspace's crates apart, read
// closed crates, and change no record (component-crates invariant 14;
// req-crates AC-6.2).
func TestMCPCrateReads(t *testing.T) {
	f := newCrateFixture(t)
	declared := f.requirement(t, f.ctx, true)
	soon := f.createCrate(t, f.editor, map[string]any{"name": "Soon", "target_date": "2026-12-01", "requirement_ids": []string{declared}})
	later := f.createCrate(t, f.editor, map[string]any{"name": "Later"})
	closed := f.createCrate(t, f.editor, map[string]any{"name": "Shut"})
	foreign := f.createCrate(t, f.foreign, map[string]any{"name": "Beta"})
	second := f.task(t, soon.ID)
	first := f.task(t, "")
	// The second member joins by a recorded act after it is created.
	if status, raw := f.do(t, f.editor, http.MethodPut, "/v1/tasks/"+first.ID+"/crate", map[string]any{"crate_id": soon.ID}); status != http.StatusOK {
		t.Fatalf("join status=%d body=%s", status, raw)
	}
	closedMember := f.task(t, closed.ID)
	if status, raw := f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+closed.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	f.task(t, "")
	// Member task IDs are ordered by creation time and then ID, not by the
	// order in which the tasks joined.
	members := []core.Task{second, first}
	slices.SortFunc(members, func(a, b core.Task) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	firstThenSecond := []string{members[0].ID, members[1].ID}
	before := f.recorded(t)

	page := readAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha"})
	rows := crateMCPRows(t, page)
	if got := crateRowIDs(rows); !slices.Equal(got, []string{soon.ID, later.ID}) {
		t.Fatalf("open crates = %v, want %s then %s", got, soon.ID, later.ID)
	}
	if row := rows[0]; row.Name != "Soon" || row.TargetDate == nil || *row.TargetDate != "2026-12-01" || row.State != "open" || !slices.Equal(row.RequirementIDs, []string{declared}) || !slices.Equal(row.TaskIDs, firstThenSecond) || row.TotalTaskCount != 2 || row.CreatedBy == "" {
		t.Fatalf("soon row = %+v, want declared %s and tasks %v", row, declared, firstThenSecond)
	}
	if row := rows[1]; row.TargetDate != nil || len(row.RequirementIDs) != 0 || row.RequirementIDs == nil || len(row.TaskIDs) != 0 || row.TaskIDs == nil {
		t.Fatalf("later row = %+v, want empty lists", row)
	}
	closedRows := crateMCPRows(t, readAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha", "state": "closed"}))
	if len(closedRows) != 1 || closedRows[0].ID != closed.ID || closedRows[0].State != "closed" || !slices.Equal(closedRows[0].TaskIDs, []string{closedMember.ID}) {
		t.Fatalf("closed crates = %+v", closedRows)
	}
	all := crateMCPRows(t, readAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha", "state": "all"}))
	if got := crateRowIDs(all); !slices.Equal(got, []string{soon.ID, later.ID, closed.ID}) {
		t.Fatalf("all crates = %v", got)
	}

	one := readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": soon.ID})
	if got := crateMCPRows(t, one); len(got) != 1 || got[0].ID != soon.ID || !slices.Equal(got[0].TaskIDs, firstThenSecond) || !slices.Equal(got[0].RequirementIDs, []string{declared}) {
		t.Fatalf("get_crate = %+v", got)
	}
	if one.Snapshot != "" || one.NextOffset != nil {
		t.Fatalf("get_crate kept a snapshot: %+v", one)
	}
	if got := crateMCPRows(t, readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": closed.ID})); len(got) != 1 || got[0].State != "closed" {
		t.Fatalf("closed get_crate = %+v", got)
	}

	// Workspace isolation: another workspace's crate is not found, and that
	// workspace lists only its own crate.
	for _, id := range []string{foreign.ID, "crate-000000-000000"} {
		if refusal := refusalAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": id}); !strings.Contains(refusal, "crate_not_found") || !strings.Contains(refusal, id) {
			t.Fatalf("get_crate %s refusal = %q", id, refusal)
		}
	}
	if got := crateRowIDs(crateMCPRows(t, readAs(t, f.handler, f.foreign, "list_crates", map[string]any{"workspace_id": "beta", "state": "all"}))); !slices.Equal(got, []string{foreign.ID}) {
		t.Fatalf("beta crates = %v", got)
	}

	// Authorization and schema: a caller with no binding, another
	// workspace's member, a missing workspace, a missing crate_id, and an
	// unknown state are refused.
	for _, tc := range []struct {
		token, tool string
		args        map[string]any
		want        string
	}{
		{"pat-unbound", "list_crates", map[string]any{"workspace_id": "alpha"}, "workspace_not_found"},
		{"pat-unbound", "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": soon.ID}, "workspace_not_found"},
		{f.foreign, "list_crates", map[string]any{"workspace_id": "alpha"}, "workspace_not_found"},
		{f.viewer, "list_crates", map[string]any{}, "workspace_id is required"},
		{f.viewer, "get_crate", map[string]any{"workspace_id": "alpha"}, "crate_id is required"},
		{f.viewer, "list_crates", map[string]any{"workspace_id": "alpha", "state": "shipped"}, "invalid state"},
	} {
		if refusal := refusalAs(t, f.handler, tc.token, tc.tool, tc.args); !strings.Contains(refusal, tc.want) {
			t.Fatalf("%s as %s with %v: refusal %q, want %q", tc.tool, tc.token, tc.args, refusal, tc.want)
		}
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("crate reads changed records:\nbefore %s\nafter  %s", before, after)
	}
}

// crateFailingStore fails the member read that list_crates uses.
type crateFailingStore struct {
	crateBackend
}

func (crateFailingStore) ListCrateMembers(context.Context, []string) (map[string][]store.CrateMemberTask, error) {
	return nil, fmt.Errorf("read crate members: %w", store.ErrBackendOperation)
}

// TestMCPCrateReadFailure: a failed store read fails the MCP read in band
// and returns no partial rows.
func TestMCPCrateReadFailure(t *testing.T) {
	f := newCrateFixture(t)
	f.createCrate(t, f.editor, map[string]any{"name": "Unreadable"})
	f.server.Store = &crateFailingStore{crateBackend: f.store.(crateBackend)}
	refusal := refusalAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha"})
	if !strings.Contains(refusal, "request_id") {
		t.Fatalf("list_crates failure = %q, want the backend failure with its request ID", refusal)
	}
}

// TestMCPCreateTaskCrate: create_task passes crate_id to the shared intake,
// which applies the crate rules of every intake surface: the task joins an
// open crate and takes only the requirements the call names, a closed,
// missing, or foreign crate records no task, and a retry naming another
// crate is refused (component-crates invariant 7; req-crates AC-6.1, AC-2.1,
// AC-3.1).
func TestMCPCreateTaskCrate(t *testing.T) {
	f := newCrateFixture(t)
	declared := f.requirement(t, f.ctx, true)
	named := f.requirement(t, f.ctx, true)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Agent intake", "requirement_ids": []string{declared}})
	other := f.createCrate(t, f.editor, map[string]any{"name": "Elsewhere"})
	closed := f.createCrate(t, f.editor, map[string]any{"name": "Shut"})
	if status, raw := f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+closed.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	foreign := f.createCrate(t, f.foreign, map[string]any{"name": "Beta"})

	args := map[string]any{"workspace_id": "alpha", "body": "Crate intake over MCP.", "repo": "api", "crate_id": crate.ID, "requirement_ids": []any{named}, "idempotency_key": "mcp-crate-1"}
	created := f.callMCP(t, f.editor, "create_task", args)
	if created.errText != "" || !created.created || created.task.Crate == nil || created.task.Crate.ID != crate.ID || created.task.Crate.Name != "Agent intake" {
		t.Fatalf("create_task with crate = %+v", created)
	}
	if stored, err := f.store.GetTask(f.ctx, created.task.ID); err != nil || stored.TaskCrateID() != crate.ID {
		t.Fatalf("stored task crate = %v, %v", stored.Crate, err)
	}
	active, err := store.TaskContextForTask(f.ctx, f.store, created.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Requirements) != 1 || active.Requirements[0].ID != named {
		t.Fatalf("context = %+v, want only %s and not the declared %s", active.Requirements, named, declared)
	}
	if again := f.callMCP(t, f.editor, "create_task", args); again.errText != "" || again.created || again.task.ID != created.task.ID {
		t.Fatalf("equal retry = %+v", again)
	}
	moved := map[string]any{}
	for key, value := range args {
		moved[key] = value
	}
	moved["crate_id"] = other.ID
	if again := f.callMCP(t, f.editor, "create_task", moved); again.errText == "" {
		t.Fatalf("retry naming another crate was accepted: %+v", again)
	}
	plain := f.callMCP(t, f.editor, "create_task", map[string]any{"workspace_id": "alpha", "body": "No crate over MCP.", "repo": "api", "idempotency_key": "mcp-crate-plain"})
	if plain.errText != "" || plain.task.Crate != nil {
		t.Fatalf("create_task without crate = %+v", plain)
	}

	before := f.recorded(t)
	for _, tc := range []struct {
		name    string
		crateID any
		want    string
	}{
		{"closed", closed.ID, "crate_closed"},
		{"missing", "crate-000000-000000", "crate_not_found"},
		{"other workspace", foreign.ID, "crate_not_found"},
		{"not a string", 7, "invalid_crate"},
	} {
		refused := f.callMCP(t, f.editor, "create_task", map[string]any{"workspace_id": "alpha", "body": "Refused " + tc.name + ".", "repo": "api", "crate_id": tc.crateID, "idempotency_key": "mcp-crate-refused-" + strings.ReplaceAll(tc.name, " ", "-")})
		if !strings.Contains(refused.errText, tc.want) {
			t.Fatalf("%s crate: error %q, want %q", tc.name, refused.errText, tc.want)
		}
		if id, ok := tc.crateID.(string); ok && !strings.Contains(refused.errText, id) {
			t.Fatalf("%s crate: error %q does not name %s", tc.name, refused.errText, id)
		}
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("refused create_task recorded state:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMCPCrateToolsAreReadOnly: the only crate tools are the two reads, both
// marked read-only, and create_task states crate_id. No tool creates or
// edits a crate or changes membership (component-crates invariant 14).
func TestMCPCrateToolsAreReadOnly(t *testing.T) {
	var crateTools []string
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		if name == "create_task" {
			properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if _, found := properties["crate_id"]; !found {
				t.Fatal("create_task schema has no crate_id")
			}
			continue
		}
		if strings.Contains(name, "crate") {
			crateTools = append(crateTools, name)
			annotations, _ := tool["annotations"].(map[string]any)
			if annotations["readOnlyHint"] != true || annotations["destructiveHint"] != false {
				t.Fatalf("%s annotations = %v", name, annotations)
			}
			if mcpCapability(name) != core.CapabilityViewWorkspace {
				t.Fatalf("%s capability = %q", name, mcpCapability(name))
			}
		}
	}
	if !slices.Equal(crateTools, []string{"list_crates", "get_crate"}) {
		t.Fatalf("crate tools = %v, want only list_crates and get_crate", crateTools)
	}
}
