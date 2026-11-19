package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateMCPRow is the MCP crate row as a session decodes it. TaskIDs and
// TaskIDsTruncated are the get_crate fields.
type crateMCPRow struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	TargetDate       *string  `json:"target_date"`
	State            string   `json:"state"`
	Shipped          bool     `json:"shipped"`
	CreatedBy        string   `json:"created_by"`
	OpenTaskCount    int      `json:"open_task_count"`
	MergedTaskCount  int      `json:"merged_task_count"`
	TotalTaskCount   int      `json:"total_task_count"`
	RequirementIDs   []string `json:"requirement_ids"`
	TaskIDs          []string `json:"task_ids"`
	TaskIDsTruncated bool     `json:"task_ids_truncated"`
}

// crateRowKeys are the fields of every MCP crate row (component-crates
// Public surfaces, crate row, plus requirement_ids).
var crateRowKeys = []string{"id", "name", "target_date", "state", "shipped", "created_by", "created_at", "closed_at", "open_task_count", "merged_task_count", "total_task_count", "requirement_ids"}

// crateMCPRows decodes list_crates rows. It checks the keys of each raw row,
// so a list row that carries task_ids or task_ids_truncated fails even when
// the value is empty (component-crates invariant 14).
func crateMCPRows(t *testing.T, page mcpReadPage) []crateMCPRow {
	t.Helper()
	return decodeCrateMCPRows(t, page, false)
}

// crateDetailRows decodes get_crate rows, which also carry task_ids, never
// null, and task_ids_truncated.
func crateDetailRows(t *testing.T, page mcpReadPage) []crateMCPRow {
	t.Helper()
	return decodeCrateMCPRows(t, page, true)
}

func decodeCrateMCPRows(t *testing.T, page mcpReadPage, detail bool) []crateMCPRow {
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
		for _, key := range crateRowKeys {
			if _, found := keys[key]; !found {
				t.Fatalf("crate row lacks %s: %s", key, item)
			}
		}
		for _, key := range []string{"task_ids", "task_ids_truncated"} {
			_, found := keys[key]
			if found != detail {
				t.Fatalf("crate row (detail %t) has %s = %t: %s", detail, key, found, item)
			}
		}
		if detail && string(keys["task_ids"]) == "null" {
			t.Fatalf("get_crate task_ids is null: %s", item)
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

// TestMCPCrateReads: list_crates returns crate rows with name, target date,
// state, declared requirement IDs, and counts and no member task IDs, and
// get_crate adds the member task IDs and task_ids_truncated, to a person who
// may view the workspace. Both reads keep each workspace's crates apart, read
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
	if row := rows[0]; row.Name != "Soon" || row.TargetDate == nil || *row.TargetDate != "2026-12-01" || row.State != "open" || !slices.Equal(row.RequirementIDs, []string{declared}) || row.OpenTaskCount != 2 || row.TotalTaskCount != 2 || row.CreatedBy == "" {
		t.Fatalf("soon row = %+v, want declared %s and two member tasks", row, declared)
	}
	if row := rows[1]; row.TargetDate != nil || len(row.RequirementIDs) != 0 || row.RequirementIDs == nil || row.TotalTaskCount != 0 {
		t.Fatalf("later row = %+v, want an empty requirement list and no members", row)
	}
	closedRows := crateMCPRows(t, readAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha", "state": "closed"}))
	if len(closedRows) != 1 || closedRows[0].ID != closed.ID || closedRows[0].State != "closed" || closedRows[0].TotalTaskCount != 1 {
		t.Fatalf("closed crates = %+v", closedRows)
	}
	all := crateMCPRows(t, readAs(t, f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha", "state": "all"}))
	if got := crateRowIDs(all); !slices.Equal(got, []string{soon.ID, later.ID, closed.ID}) {
		t.Fatalf("all crates = %v", got)
	}

	one := readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": soon.ID})
	if got := crateDetailRows(t, one); len(got) != 1 || got[0].ID != soon.ID || !slices.Equal(got[0].TaskIDs, firstThenSecond) || got[0].TaskIDsTruncated || !slices.Equal(got[0].RequirementIDs, []string{declared}) || got[0].TotalTaskCount != 2 || got[0].Name != "Soon" {
		t.Fatalf("get_crate = %+v, want tasks %v and no truncation", got, firstThenSecond)
	}
	if one.Snapshot != "" || one.NextOffset != nil {
		t.Fatalf("get_crate kept a snapshot: %+v", one)
	}
	if got := crateDetailRows(t, readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": closed.ID})); len(got) != 1 || got[0].State != "closed" || !slices.Equal(got[0].TaskIDs, []string{closedMember.ID}) || got[0].TaskIDsTruncated {
		t.Fatalf("closed get_crate = %+v", got)
	}
	// The crate ID is trimmed, as the REST crate routes trim it.
	if got := crateDetailRows(t, readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": " " + later.ID + " "})); len(got) != 1 || got[0].ID != later.ID || len(got[0].TaskIDs) != 0 || got[0].TaskIDsTruncated {
		t.Fatalf("empty get_crate = %+v", got)
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
		{f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": " "}, "invalid crate_id"},
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

// crateMemberAt records a member task of crateID with the given ID and
// creation time, so a test can control the member order.
func (f *crateFixture) crateMemberAt(t *testing.T, crateID, id string, createdAt time.Time) {
	t.Helper()
	task := core.Task{ID: id, Workspace: "alpha", Repo: "api", Title: "Member " + id, Body: "Crate member.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: createdAt, PlanState: core.PlanNotRequired, Branch: "conveyor/task-" + id, Crate: &core.TaskCrate{ID: crateID}}
	if err := f.store.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
}

// crateReadSpy wraps the crate store. It records each crate and task page
// read the MCP tools make and fails the read that fail names.
type crateReadSpy struct {
	crateBackend
	mu      sync.Mutex
	calls   []string
	limited []string
	pages   []store.TaskFilter
	fail    string
}

func (s *crateReadSpy) record(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if s.fail == name {
		return fmt.Errorf("read %s: %w", name, store.ErrBackendOperation)
	}
	return nil
}

func (s *crateReadSpy) called(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if call == name {
			n++
		}
	}
	return n
}

func (s *crateReadSpy) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls, s.limited, s.pages = nil, nil, nil
}

func (s *crateReadSpy) GetCrate(ctx context.Context, id string) (store.CrateRecord, error) {
	if err := s.record("GetCrate"); err != nil {
		return store.CrateRecord{}, err
	}
	return s.crateBackend.GetCrate(ctx, id)
}

func (s *crateReadSpy) ListCrates(ctx context.Context, query store.CrateListQuery) ([]store.Crate, error) {
	if err := s.record("ListCrates"); err != nil {
		return nil, err
	}
	return s.crateBackend.ListCrates(ctx, query)
}

func (s *crateReadSpy) ListCrateMembers(ctx context.Context, ids []string) (map[string][]store.CrateMemberTask, error) {
	if err := s.record("ListCrateMembers"); err != nil {
		return nil, err
	}
	return s.crateBackend.ListCrateMembers(ctx, ids)
}

func (s *crateReadSpy) ListCrateMembersLimited(ctx context.Context, crateID, excludeTaskID string, limit int) ([]store.CrateMemberTask, bool, error) {
	if err := s.record("ListCrateMembersLimited"); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	s.limited = append(s.limited, fmt.Sprintf("%s|%s|%d", crateID, excludeTaskID, limit))
	s.mu.Unlock()
	return s.crateBackend.ListCrateMembersLimited(ctx, crateID, excludeTaskID, limit)
}

func (s *crateReadSpy) ListTaskPage(ctx context.Context, query store.TaskOperationsQuery) (store.TaskPage, error) {
	if err := s.record("ListTaskPage"); err != nil {
		return store.TaskPage{}, err
	}
	s.mu.Lock()
	s.pages = append(s.pages, query.TaskFilter)
	s.mu.Unlock()
	return s.crateBackend.ListTaskPage(ctx, query)
}

func newCrateReadSpy(f *crateFixture) *crateReadSpy {
	spy := &crateReadSpy{crateBackend: f.store.(crateBackend)}
	f.server.Store = spy
	return spy
}

// TestMCPGetCrateTaskIDBound: get_crate returns the IDs of at most the 500
// oldest member tasks by creation time and then ID, with task_ids_truncated
// true only when the crate has more. The IDs come from one bounded member
// read with no task left out, and neither crate read reads every member ID
// (req-crates AC-6.2; component-crates invariant 14).
func TestMCPGetCrateTaskIDBound(t *testing.T) {
	f := newCrateFixture(t)
	spy := newCrateReadSpy(f)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		members int
		want    int
		more    bool
	}{
		{"no members", 0, 0, false},
		{"500 members", 500, 500, false},
		{"501 members", 501, 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crate := f.createCrate(t, f.editor, map[string]any{"name": "Bound " + tc.name})
			var created []core.Task
			// The last five members share one creation time and are recorded
			// with descending IDs, so the IDs at the 500 cutoff are ordered
			// by ID and not by insertion.
			tied := max(tc.members-5, 0)
			for i := range tc.members {
				id := fmt.Sprintf("member-%s-%04d", strings.ReplaceAll(tc.name, " ", "-"), i)
				at := base.Add(time.Duration(i) * time.Second)
				if i >= tied {
					id = fmt.Sprintf("member-%s-%04d", strings.ReplaceAll(tc.name, " ", "-"), tc.members-1-(i-tied))
					at = base.Add(time.Duration(tied) * time.Second)
				}
				f.crateMemberAt(t, crate.ID, id, at)
				created = append(created, core.Task{ID: id, CreatedAt: at})
			}
			slices.SortFunc(created, func(a, b core.Task) int {
				if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
					return n
				}
				return strings.Compare(a.ID, b.ID)
			})
			want := make([]string, 0, tc.want)
			for _, task := range created[:tc.want] {
				want = append(want, task.ID)
			}
			before := f.recorded(t)
			spy.reset()

			rows := crateDetailRows(t, readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID}))
			if len(rows) != 1 || rows[0].ID != crate.ID || rows[0].TotalTaskCount != tc.members {
				t.Fatalf("get_crate = %+v, want crate %s with %d members", rows, crate.ID, tc.members)
			}
			if got := rows[0].TaskIDs; !slices.Equal(got, want) {
				t.Fatalf("task_ids = %d IDs, want %d oldest IDs by creation time then ID\ngot  %v\nwant %v", len(got), len(want), tail(got), tail(want))
			}
			if rows[0].TaskIDsTruncated != tc.more {
				t.Fatalf("task_ids_truncated = %t, want %t", rows[0].TaskIDsTruncated, tc.more)
			}
			if !slices.Equal(spy.limited, []string{crate.ID + "||500"}) || spy.called("GetCrate") != 0 || spy.called("ListCrateMembers") != 0 {
				t.Fatalf("get_crate reads = %v bounded %v, want one bounded read of 500 with no task left out and no full member read", spy.calls, spy.limited)
			}
			if after := f.recorded(t); after != before {
				t.Fatalf("get_crate changed records:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// tail shows the last IDs of a long list in a failure message.
func tail(ids []string) []string {
	return ids[max(len(ids)-3, 0):]
}

// TestMCPCrateReadsLargeCrateFitBudget: a crate of 3,000 member tasks reads
// over MCP within the 65,536-byte response limit. get_crate succeeds with 500
// IDs and task_ids_truncated, and list_crates succeeds with no task IDs
// (req-crates AC-6.2; component-crates invariant 14; component-mcp-protocol
// invariant 19).
func TestMCPCrateReadsLargeCrateFitBudget(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Large"})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3000 {
		f.crateMemberAt(t, crate.ID, core.NewTaskID(), base.Add(time.Duration(i)*time.Millisecond))
	}
	before := f.recorded(t)
	spy := newCrateReadSpy(f)

	page, text, toolErr, err := mcpReadTry(f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})
	if err != nil || toolErr != "" {
		t.Fatalf("get_crate on 3000 members: %v %s", err, toolErr)
	}
	if len(text) > mcpReadMaxBytes {
		t.Fatalf("get_crate result is %d bytes, want at most %d", len(text), mcpReadMaxBytes)
	}
	rows := crateDetailRows(t, page)
	if len(rows) != 1 || len(rows[0].TaskIDs) != 500 || !rows[0].TaskIDsTruncated || rows[0].TotalTaskCount != 3000 {
		t.Fatalf("get_crate = %d rows, %d IDs, truncated %t, total %d", len(rows), len(rows[0].TaskIDs), rows[0].TaskIDsTruncated, rows[0].TotalTaskCount)
	}
	t.Logf("get_crate on 3000 members: %d bytes", len(text))

	page, text, toolErr, err = mcpReadTry(f.handler, f.viewer, "list_crates", map[string]any{"workspace_id": "alpha"})
	if err != nil || toolErr != "" {
		t.Fatalf("list_crates with a 3000-member crate: %v %s", err, toolErr)
	}
	if len(text) > mcpReadMaxBytes {
		t.Fatalf("list_crates result is %d bytes, want at most %d", len(text), mcpReadMaxBytes)
	}
	if listed := crateMCPRows(t, page); len(listed) != 1 || listed[0].TotalTaskCount != 3000 {
		t.Fatalf("list_crates = %+v", listed)
	}
	if spy.called("ListCrateMembers") != 0 || spy.called("GetCrate") != 0 || spy.called("ListCrateMembersLimited") != 1 {
		t.Fatalf("crate reads = %v, want one bounded member read and no full member read", spy.calls)
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("crate reads changed records")
	}
}

// TestMCPCrateReadFailure: a failed store read fails the MCP read in band
// with its request ID, returns no partial rows, and changes no record. Each
// store read that list_crates, get_crate, and list_tasks with crate_id use is
// failed in turn (DEC-105(4)).
func TestMCPCrateReadFailure(t *testing.T) {
	for _, tc := range []struct {
		fail, tool string
	}{
		{"ListCrates", "list_crates"},
		{"ListCrates", "get_crate"},
		{"ListCrateMembersLimited", "get_crate"},
		{"ListTaskPage", "list_tasks"},
	} {
		t.Run(tc.tool+" "+tc.fail, func(t *testing.T) {
			f := newCrateFixture(t)
			crate := f.createCrate(t, f.editor, map[string]any{"name": "Unreadable"})
			f.task(t, crate.ID)
			before := f.recorded(t)
			spy := newCrateReadSpy(f)
			spy.fail = tc.fail
			args := map[string]any{"workspace_id": "alpha"}
			if tc.tool != "list_crates" {
				args["crate_id"] = crate.ID
			}
			refusal := refusalAs(t, f.handler, f.viewer, tc.tool, args)
			if !strings.Contains(refusal, "request_id") || strings.Contains(refusal, crate.ID) {
				t.Fatalf("%s failure = %q, want the backend failure with its request ID and no row", tc.tool, refusal)
			}
			if spy.called(tc.fail) != 1 {
				t.Fatalf("%s reads = %v, want one failed %s", tc.tool, spy.calls, tc.fail)
			}
			if after := f.recorded(t); after != before {
				t.Fatalf("failed read changed records")
			}
		})
	}
}

// TestMCPListTasksCrateFilter: list_tasks with crate_id lists only that
// crate's member tasks. The filter combines with state, repository, and query
// in the store's task page, keeps snapshot paging, and lists no task for a
// crate that is missing, empty, or in another workspace. The read changes no
// record (req-crates AC-6.5; component-crates invariant 14;
// component-mcp-protocol read arguments).
func TestMCPListTasksCrateFilter(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Filtered"})
	other := f.createCrate(t, f.editor, map[string]any{"name": "Other"})
	empty := f.createCrate(t, f.editor, map[string]any{"name": "Empty"})
	foreign := f.createCrate(t, f.foreign, map[string]any{"name": "Beta"})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	add := func(ctx context.Context, workspace, id, crateID, repo string, state core.TaskState, offset int) {
		task := core.Task{ID: id, Workspace: workspace, Repo: repo, Title: "Title " + id, Body: "Body.", State: state, NextStage: core.StageImplement, CreatedAt: base.Add(time.Duration(offset) * time.Minute), PlanState: core.PlanNotRequired, Branch: "conveyor/task-" + id}
		if crateID != "" {
			task.Crate = &core.TaskCrate{ID: crateID}
		}
		if err := f.store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	add(f.ctx, "alpha", "member-queued-api", crate.ID, "api", core.TaskQueued, 1)
	add(f.ctx, "alpha", "member-running-web", crate.ID, "web", core.TaskRunning, 2)
	add(f.ctx, "alpha", "member-merged-api", crate.ID, "api", core.TaskMerged, 3)
	add(f.ctx, "alpha", "member-closed-api", crate.ID, "api", core.TaskClosed, 4)
	add(f.ctx, "alpha", "other-queued-api", other.ID, "api", core.TaskQueued, 5)
	add(f.ctx, "alpha", "loose-queued-api", "", "api", core.TaskQueued, 6)
	add(f.ctx, "alpha", "loose-merged-api", "", "api", core.TaskMerged, 7)
	add(f.betaCtx, "beta", "foreign-queued-api", foreign.ID, "api", core.TaskQueued, 8)
	before := f.recorded(t)
	spy := newCrateReadSpy(f)

	list := func(args map[string]any) []string {
		t.Helper()
		page := readAs(t, f.handler, f.viewer, "list_tasks", withArgs(map[string]any{"workspace_id": "alpha"}, flatten(args)...))
		ids := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(item, &row); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		return ids
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"crate alone lists active members", map[string]any{"crate_id": crate.ID}, []string{"member-queued-api", "member-running-web"}},
		{"crate with state all", map[string]any{"crate_id": crate.ID, "state": "all"}, []string{"member-closed-api", "member-merged-api", "member-queued-api", "member-running-web"}},
		{"crate with state terminal", map[string]any{"crate_id": crate.ID, "state": "terminal"}, []string{"member-closed-api", "member-merged-api"}},
		{"crate with repository", map[string]any{"crate_id": crate.ID, "state": "all", "repository": "web"}, []string{"member-running-web"}},
		{"crate with query", map[string]any{"crate_id": crate.ID, "state": "all", "query": "merged"}, []string{"member-merged-api"}},
		{"crate ID is trimmed", map[string]any{"crate_id": " " + other.ID + " "}, []string{"other-queued-api"}},
		{"crate with no members", map[string]any{"crate_id": empty.ID, "state": "all"}, []string{}},
		{"missing crate", map[string]any{"crate_id": "crate-000000-000000", "state": "all"}, []string{}},
		{"crate of another workspace", map[string]any{"crate_id": foreign.ID, "state": "all"}, []string{}},
		{"no crate lists every task", map[string]any{"state": "all"}, []string{"loose-merged-api", "loose-queued-api", "member-closed-api", "member-merged-api", "member-queued-api", "member-running-web", "other-queued-api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy.reset()
			if got := list(tc.args); !slices.Equal(got, tc.want) {
				t.Fatalf("list_tasks %v = %v, want %v", tc.args, got, tc.want)
			}
			// The crate filter reaches the store's task page, where it
			// intersects the other filters before the candidate count.
			wantCrate := strings.TrimSpace(readString(tc.args, "crate_id"))
			if len(spy.pages) == 0 || spy.pages[0].Crate != wantCrate || spy.called("GetCrate")+spy.called("ListCrates")+spy.called("ListCrateMembers")+spy.called("ListCrateMembersLimited") != 0 {
				t.Fatalf("list_tasks reads = %v filters %+v, want the task page with crate %q and no crate read", spy.calls, spy.pages, wantCrate)
			}
		})
	}

	// Snapshot paging stays the same: a page of one continues through the
	// crate's members under the snapshot the first page returned.
	first := readAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID, "state": "all", "limit": 1})
	if first.Total != 4 || len(first.Items) != 1 || first.NextOffset == nil || first.Snapshot == "" {
		t.Fatalf("first page = %+v", first)
	}
	seen := []string{string(first.Items[0])}
	for next := first.NextOffset; next != nil; {
		page := readAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID, "state": "all", "limit": 1, "offset": *next, "snapshot": first.Snapshot})
		seen = append(seen, string(page.Items[0]))
		next = page.NextOffset
	}
	if len(seen) != 4 {
		t.Fatalf("paged %d members, want 4", len(seen))
	}
	// A snapshot belongs to its arguments: another crate_id cannot reuse it.
	if refusal := refusalAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": other.ID, "state": "all", "limit": 1, "offset": 1, "snapshot": first.Snapshot}); !strings.Contains(refusal, "snapshot unavailable") {
		t.Fatalf("snapshot reuse with another crate = %q", refusal)
	}
	if refusal := refusalAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": " "}); !strings.Contains(refusal, "invalid crate_id") {
		t.Fatalf("blank crate_id refusal = %q", refusal)
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("list_tasks with crate_id changed records:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMCPListTasksCrateFilterCandidateBound: crate_id narrows the candidates
// before the 1,000-candidate refusal of list_tasks, and the refusal still
// applies inside one crate (req-crates AC-6.5).
func TestMCPListTasksCrateFilterCandidateBound(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Bounded"})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range mcpReadMaxItems + 1 {
		task := core.Task{ID: fmt.Sprintf("loose-%04d", i), Workspace: "alpha", Repo: "api", Title: "Loose", Body: "Body.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: base.Add(time.Duration(i) * time.Second), PlanState: core.PlanNotRequired}
		if err := f.store.CreateTask(f.ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	f.crateMemberAt(t, crate.ID, "member-0000", base)
	f.crateMemberAt(t, crate.ID, "member-0001", base)
	if refusal := refusalAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha"}); !strings.Contains(refusal, "exceeds 1000 candidates") {
		t.Fatalf("unfiltered list_tasks = %q, want the candidate refusal", refusal)
	}
	if page := readAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID}); page.Total != 2 {
		t.Fatalf("crate list_tasks total = %d, want 2", page.Total)
	}
	for i := range mcpReadMaxItems - 1 {
		f.crateMemberAt(t, crate.ID, fmt.Sprintf("member-%04d", i+2), base.Add(time.Duration(i)*time.Second))
	}
	if refusal := refusalAs(t, f.handler, f.viewer, "list_tasks", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID}); !strings.Contains(refusal, "exceeds 1000 candidates") {
		t.Fatalf("list_tasks on a crate of 1001 members = %q, want the candidate refusal", refusal)
	}
}

// flatten turns a map of arguments into withArgs pairs.
func flatten(args map[string]any) []any {
	out := make([]any, 0, 2*len(args))
	for key, value := range args {
		out = append(out, key, value)
	}
	return out
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
