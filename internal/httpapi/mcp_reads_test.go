package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func newMCPReadFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	s := NewServer(st)
	membership := &membershipFixture{workspaces: []core.Workspace{{ID: "demo", Name: "Demo"}, {ID: "private", Name: "Private"}}, roles: map[string]map[string]core.WorkspaceRole{"reader": {"demo": core.WorkspaceRoleViewer}, "other": {"demo": core.WorkspaceRoleViewer}}}
	s.Workspaces, s.Memberships = membership, membership
	s.Credentials = staticCredentialVerifier{
		"reader": {ID: "reader-pat", OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"other":  {ID: "other-pat", OwnerUserID: "other", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"denied": {ID: "denied-pat", OwnerUserID: "denied", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent":  {ID: "agent-credential", OwnerUserID: "reader", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
		"child":  {ID: "child-credential", OwnerUserID: "reader", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: "demo", RunWorkOrderID: "claimed-order", RunSessionID: "claimed-session"},
	}
	s.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
		workspace, _ := store.WorkspaceFromContext(ctx)
		if workspace != "demo" {
			t.Fatalf("unscoped config lookup: %s", workspace)
		}
		return &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main", URL: "https://user:private-password@example.test/repo", Checkout: "/private/local/path"}}}, nil
	}
	s.OnCreate = func(context.Context, string) { t.Fatal("read enqueued a task") }
	return s, ctx
}
func mcpReadCall(t *testing.T, s *Server, token, name string, args map[string]any) (mcpReadPage, string) {
	return mcpReadCallWithMeta(t, s, token, name, args, nil)
}
func mcpReadCallWithMeta(t *testing.T, s *Server, token, name string, args map[string]any, meta map[string]any) (mcpReadPage, string) {
	t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	wire, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(wire)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("MCP HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if e := json.Unmarshal(rec.Body.Bytes(), &envelope); e != nil {
		t.Fatal(e)
	}
	if len(envelope.Result.Content) != 1 {
		t.Fatalf("unexpected response %s", rec.Body.String())
	}
	text := envelope.Result.Content[0].Text
	if envelope.Result.IsError {
		return mcpReadPage{}, text
	}
	if len(text) > mcpReadMaxBytes {
		t.Fatalf("unbounded response: %d", len(text))
	}
	var page mcpReadPage
	if e := json.Unmarshal([]byte(text), &page); e != nil {
		t.Fatal(e)
	}
	return page, ""
}

func TestMCPReadCallsAcceptEnvelopeMetadataWithoutRelaxingArguments(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "metadata-task", Workspace: "demo", Title: "Metadata", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	document, version, err := s.Store.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-metadata", Title: "Metadata", Category: "Component"}, core.SystemDesignVersion{Content: "# Metadata\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Store.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"progressToken": float64(1), "trace": "native-fixture"}
	listed, toolErr := mcpReadCallWithMeta(t, s, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "state": "active", "limit": float64(2)}, meta)
	if toolErr != "" || listed.Total != 1 || readItem(t, listed, 0)["id"] != "metadata-task" {
		t.Fatalf("list_tasks with metadata: page=%+v error=%q", listed, toolErr)
	}
	got, toolErr := mcpReadCallWithMeta(t, s, "reader", "get_document", map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": document.ID}, meta)
	if toolErr != "" || got.Total != 1 || readItem(t, got, 0)["id"] != document.ID {
		t.Fatalf("get_document with metadata: page=%+v error=%q", got, toolErr)
	}
	for name, args := range map[string]map[string]any{
		"list_tasks":   {"workspace_id": "demo", "limit": "2"},
		"get_document": {"workspace_id": "demo", "kind": "system_design", "document_id": document.ID, "unexpected": true},
	} {
		if _, toolErr = mcpReadCallWithMeta(t, s, "reader", name, args, meta); toolErr == "" {
			t.Fatalf("%s accepted malformed tool arguments", name)
		}
	}
	if _, toolErr = mcpReadCallWithMeta(t, s, "denied", "list_tasks", map[string]any{"workspace_id": "demo"}, meta); !strings.Contains(toolErr, "workspace_not_found") {
		t.Fatalf("metadata changed authorization: %q", toolErr)
	}
}
func readItem(t *testing.T, p mcpReadPage, index int) map[string]any {
	t.Helper()
	if len(p.Items) <= index {
		t.Fatalf("missing item %d: %+v", index, p)
	}
	var value map[string]any
	if e := json.Unmarshal(p.Items[index], &value); e != nil {
		t.Fatal(e)
	}
	return value
}
func mustRead(t *testing.T, s *Server, name string, a map[string]any) mcpReadPage {
	t.Helper()
	p, e := mcpReadCall(t, s, "reader", name, a)
	if e != "" {
		t.Fatalf("%s: %s", name, e)
	}
	return p
}

// The fixture drives native tools/call from a viewer PAT with no work order.
// No production credential or real operator gate act participates.
func TestMCPReadTerminalContextArchivedVersionEndToEnd(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	st := s.Store
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	_, v, e := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-integrations-sync", Title: "Integration sync", Category: "Component"}, core.SystemDesignVersion{Content: "# Original design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/integrations/**\n```", Origin: core.SystemDesignOriginOperator})
	must(e)
	_, _, e = st.ConfirmSystemDesignVersion(ctx, v.DocumentID, v.Version)
	must(e)
	task := core.Task{ID: "terminal-investigation", Workspace: "demo", Repo: "conveyor", Title: "Archived context investigation", State: core.TaskQueued, CreatedAt: time.Now()}
	must(st.CreateTask(ctx, task))
	proposer := store.WithActor(ctx, store.Actor{ID: "agent:triage", Role: core.ActorAgent})
	_, _, e = st.ProposeTaskContext(proposer, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: v.DocumentID, Source: core.TaskContextProposalTriage, Justification: "integration contract"})
	must(e)
	confirmer := store.WithActor(ctx, store.Actor{ID: "user:operator", Role: core.ActorUser})
	_, e = st.ConfirmTaskContextProposal(confirmer, task.ID, core.TaskContextProposalSystemDesign, v.DocumentID)
	must(e)
	next, e := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: v.DocumentID, Content: strings.Replace(v.Content, "Original", "Current", 1), Origin: core.SystemDesignOriginOperator})
	must(e)
	_, _, e = st.ConfirmSystemDesignVersion(ctx, v.DocumentID, next.Version)
	must(e)
	must(st.ArchiveSystemDesign(confirmer, v.DocumentID, "user:operator", nil))
	_, e = taskops.New(st).Cancel(confirmer, core.Intervention{TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "cancel", Comment: "fixture complete"})
	must(e)
	// A foreign task proves entity lookup cannot rely on the memory GetTask scope.
	must(st.CreateTask(store.WithWorkspace(ctx, "private"), core.Task{ID: "foreign", Workspace: "private", State: core.TaskClosed, Title: "private title"}))
	must(st.CreateTask(ctx, core.Task{ID: "leased-task", Workspace: "demo", State: core.TaskRunning}))
	must(st.CreateJob(ctx, core.Job{ID: "leased-order", TaskID: "leased-task", Stage: core.StageImplement, State: core.JobPending}))
	must(storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "leased-order", TaskID: "leased-task", JobID: "leased-order", Stage: core.StageImplement, State: core.WorkOrderQueued}))
	_, e = storetest.For(st).ClaimWorkOrder(ctx, "leased-order", core.WorkOrderClaim{SessionID: "fixture-session", ClientToken: "fixture-secret", ClaimantID: "run:fixture", Lease: time.Minute})
	must(e)
	beforeTask, _ := st.GetTask(ctx, task.ID)
	beforeEvents, _ := st.ListEvents(ctx, task.ID)
	beforeOrders, _ := st.ListWorkOrders(ctx)
	beforeJobs, _ := st.ListJobs(ctx, task.ID)
	beforeProposals, _ := st.ListTaskContextProposals(ctx, task.ID, "")
	beforeDocs, _ := st.ListSystemDesignEvents(ctx, v.DocumentID)
	page := mustRead(t, s, "list_tasks", map[string]any{"workspace_id": "demo", "state": "terminal", "query": "investigation"})
	if page.Total != 1 || readItem(t, page, 0)["id"] != task.ID {
		t.Fatalf("terminal task discovery: %+v", page)
	}
	_ = mustRead(t, s, "get_task", map[string]any{"workspace_id": "demo", "task_id": task.ID})
	contextPage := mustRead(t, s, "get_task_context", map[string]any{"workspace_id": "demo", "task_id": task.ID})
	pin := readItem(t, contextPage, 0)
	if pin["archived"] != true || pin["active_authority"] != false || pin["pinned_version"] != float64(1) || pin["current_version"] != float64(2) {
		t.Fatalf("pin=%v", pin)
	}
	proposal := readItem(t, contextPage, 1)["proposal"].(map[string]any)
	if proposal["state"] != "confirmed" || proposal["source"] != "triage" || proposal["proposed_by"] != "agent:triage" || proposal["decided_by"] != "user:operator" {
		t.Fatalf("proposal=%v", proposal)
	}
	events := mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": task.ID, "limit": 100})
	kinds := map[string]bool{}
	for i := range events.Items {
		event := readItem(t, events, i)
		kinds[event["kind"].(string)] = true
		if event["kind"] == "task.context_proposed" && event["actor_id"] != "agent:triage" {
			t.Fatalf("attribution=%v", event)
		}
	}
	for _, kind := range []string{"task.context_proposed", "task.context_proposal_confirmed", store.TaskContextDesignAdded} {
		if !kinds[kind] {
			t.Fatalf("missing %s", kind)
		}
	}
	listed := mustRead(t, s, "list_documents", map[string]any{"workspace_id": "demo", "kind": "system_design"})
	if listed.Total != 0 {
		t.Fatal("archived design in active listing")
	}
	a := map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": v.DocumentID, "version": 1}
	if _, err := mcpReadCall(t, s, "reader", "get_document", a); !strings.Contains(err, "archived") {
		t.Fatalf("implicit archive read: %s", err)
	}
	a["include_archived"] = true
	doc := readItem(t, mustRead(t, s, "get_document", a), 0)
	if doc["content"] != v.Content || doc["version_status"] != "confirmed" || doc["historical"] != true || doc["active_authority"] != false {
		t.Fatalf("archived version=%v", doc)
	}
	history := mustRead(t, s, "list_document_events", map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": v.DocumentID, "include_archived": true})
	if history.Total == 0 {
		t.Fatal("missing document history")
	}
	if _, err := mcpReadCall(t, s, "reader", "get_task", map[string]any{"workspace_id": "demo", "task_id": "foreign"}); !strings.Contains(err, "not found") {
		t.Fatalf("cross-workspace task: %s", err)
	}
	afterTask, _ := st.GetTask(ctx, task.ID)
	afterEvents, _ := st.ListEvents(ctx, task.ID)
	afterOrders, _ := st.ListWorkOrders(ctx)
	afterJobs, _ := st.ListJobs(ctx, task.ID)
	afterProposals, _ := st.ListTaskContextProposals(ctx, task.ID, "")
	afterDocs, _ := st.ListSystemDesignEvents(ctx, v.DocumentID)
	if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeEvents, afterEvents) || !reflect.DeepEqual(beforeOrders, afterOrders) || !reflect.DeepEqual(beforeJobs, afterJobs) || !reflect.DeepEqual(beforeProposals, afterProposals) || !reflect.DeepEqual(beforeDocs, afterDocs) {
		t.Fatal("operator reads mutated durable state")
	}
}

func TestMCPReadAuthorizationAndSchemas(t *testing.T) {
	s, _ := newMCPReadFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var response struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if e := json.Unmarshal(rec.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range response.Result.Tools {
		schemas[tool["name"].(string)] = tool["inputSchema"].(map[string]any)
	}
	for _, d := range mcpReadDefinitions() {
		t.Run(d.name, func(t *testing.T) {
			schema := schemas[d.name]
			if schema == nil || schema["additionalProperties"] != false || mcpCapability(d.name) != core.CapabilityViewWorkspace {
				t.Fatalf("schema/capability missing for %s", d.name)
			}
			a := map[string]any{"workspace_id": "demo"}
			for _, field := range d.required {
				a[field] = "missing"
			}
			if _, ok := d.fields["kind"]; ok {
				a["kind"] = "system_design"
			}
			for _, token := range []string{"agent", "child", "denied"} {
				if _, e := mcpReadCall(t, s, token, d.name, a); e == "" {
					t.Fatalf("%s accepted %s", d.name, token)
				}
			}
			a["workspace_id"] = "private"
			_, foreign := mcpReadCall(t, s, "reader", d.name, a)
			a["workspace_id"] = "absent"
			_, absent := mcpReadCall(t, s, "reader", d.name, a)
			if foreign != absent || !strings.Contains(foreign, "workspace_not_found") {
				t.Fatalf("scope leaks existence: %s / %s", foreign, absent)
			}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(context.WithValue(t.Context(), workerContextKey{}, core.Worker{ID: "worker", Workspace: "demo"}))
			if _, e := s.callMCPTool(request, d.name, a); e == nil {
				t.Fatal("worker read accepted")
			}
		})
	}
	page := mustRead(t, s, "list_workspaces", map[string]any{"workspace_id": "demo"})
	if page.Total != 1 || readItem(t, page, 0)["id"] != "demo" {
		t.Fatal("workspace enumeration leaked")
	}
	repos := mustRead(t, s, "list_repositories", map[string]any{"workspace_id": "demo"})
	b, _ := json.Marshal(repos)
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "url") || strings.Contains(string(b), "checkout") {
		t.Fatalf("config disclosure: %s", b)
	}
}

func TestMCPReadBoundsSnapshotsAndOrdering(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	st := s.Store
	at := time.Date(2026, 9, 15, 1, 2, 3, 0, time.UTC)
	if e := st.CreateTask(ctx, core.Task{ID: "events", Workspace: "demo", State: core.TaskClosed}); e != nil {
		t.Fatal(e)
	}
	for _, e := range []core.Event{{ID: 9, TaskID: "events", Kind: "task.context_proposed", At: at, Payload: core.JSONPayload(map[string]any{"target_id": "design", "secret": "PRIVATE", "source": "triage", "setup_contract": map[string]any{"token": "PRIVATE"}})}, {ID: 8, TaskID: "events", Kind: "task.context_proposed", At: at}, {ID: 10, TaskID: "events", Kind: "task.context_proposed", At: at.Add(time.Second)}} {
		if err := st.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	a := map[string]any{"workspace_id": "demo", "task_id": "events", "event_kind": "task.context_proposed", "limit": 1}
	first := mustRead(t, s, "list_task_events", a)
	if first.Total != 3 {
		t.Fatalf("first=%+v", first)
	}
	// Append an earlier timestamp after snapshot capture: the frozen page must not shift.
	if e := st.AppendEvent(ctx, core.Event{TaskID: "events", Kind: "task.context_proposed", At: at.Add(-time.Hour)}); e != nil {
		t.Fatal(e)
	}
	a["snapshot"] = first.Snapshot
	a["offset"] = 1
	second := mustRead(t, s, "list_task_events", a)
	again := mustRead(t, s, "list_task_events", a)
	if !reflect.DeepEqual(second, again) || second.Total != 3 {
		t.Fatal("snapshot changed")
	}
	if readItem(t, first, 0)["id"].(float64) >= readItem(t, second, 0)["id"].(float64) {
		t.Fatal("timestamp tie lacks ID ordering")
	}
	b, _ := json.Marshal(second)
	if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "setup_contract") {
		t.Fatalf("event payload leaked: %s", b)
	}
	if _, e := mcpReadCall(t, s, "other", "list_task_events", a); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("foreign snapshot: %s", e)
	}
	changed := maps.Clone(a)
	changed["event_kind"] = "different"
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", changed); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("snapshot query changed: %s", e)
	}
	membership := s.Memberships.(*membershipFixture)
	delete(membership.roles["reader"], "demo")
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", a); !strings.Contains(e, "workspace_not_found") {
		t.Fatalf("revoked membership used cache: %s", e)
	}
	membership.roles["reader"]["demo"] = core.WorkspaceRoleViewer
	s.mcpReads.mu.Lock()
	snap := s.mcpReads.entries[first.Snapshot]
	snap.expires = time.Now().Add(-time.Second)
	s.mcpReads.entries[first.Snapshot] = snap
	s.mcpReads.mu.Unlock()
	if _, e := mcpReadCall(t, s, "reader", "list_task_events", a); !strings.Contains(e, "snapshot unavailable") {
		t.Fatalf("expired snapshot: %s", e)
	}
	for _, bad := range []map[string]any{{}, {"workspace_id": "demo", "limit": 0}, {"workspace_id": "demo", "limit": 101}, {"workspace_id": "demo", "limit": 1.5}, {"workspace_id": "demo", "offset": 1}, {"workspace_id": "demo", "offset": -1}, {"workspace_id": "demo", "offset": 1001}, {"workspace_id": "demo", "query": strings.Repeat("x", 257)}, {"workspace_id": "demo", "state": "bogus"}, {"workspace_id": "demo", "include_archived": true}, {"workspace_id": "demo", "limit": "1"}, {"workspace_id": "demo", "snapshot": "junk"}} {
		if _, e := mcpReadCall(t, s, "reader", "list_tasks", bad); e == "" {
			t.Fatalf("accepted bad args %v", bad)
		}
	}
	if e := st.CreateTask(ctx, core.Task{ID: "huge", Workspace: "demo", Body: strings.Repeat("x", mcpReadMaxBytes), State: core.TaskClosed}); e != nil {
		t.Fatal(e)
	}
	if _, e := mcpReadCall(t, s, "reader", "get_task", map[string]any{"workspace_id": "demo", "task_id": "huge"}); !strings.Contains(e, "output budget") {
		t.Fatalf("oversized response: %s", e)
	}
	// Exhaustion fails closed; no caller can allocate beyond the global snapshot bound.
	s.mcpReads.mu.Lock()
	s.mcpReads.entries = map[string]mcpReadSnapshot{}
	for i := 0; i < mcpReadSnapshotCount; i++ {
		s.mcpReads.entries[fmt.Sprint(i)] = mcpReadSnapshot{expires: time.Now().Add(time.Minute)}
	}
	s.mcpReads.mu.Unlock()
	if _, e := mcpReadCall(t, s, "reader", "list_tasks", map[string]any{"workspace_id": "demo"}); !strings.Contains(e, "capacity") {
		t.Fatalf("unbounded cache: %s", e)
	}
}

func TestMCPReadDocumentStatesAndFilters(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	st := s.Store
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	_, v, e := st.CreateRequirement(ctx, core.Requirement{ID: "req-read", Title: "Read access"}, core.RequirementVersion{Content: "# Read access", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Allow reads."}}})
	must(e)
	absent := readItem(t, mustRead(t, s, "get_document", map[string]any{"workspace_id": "demo", "kind": "requirement", "document_id": "req-read"}), 0)
	if absent["authority_absent"] != true || absent["active_authority"] != false {
		t.Fatalf("missing authority=%v", absent)
	}
	proposed := readItem(t, mustRead(t, s, "get_document", map[string]any{"workspace_id": "demo", "kind": "requirement", "document_id": "req-read", "version": v.Version}), 0)
	if proposed["version_status"] != "proposed" || proposed["active_authority"] != false {
		t.Fatalf("proposal=%v", proposed)
	}
	_, _, e = st.ConfirmRequirementVersion(ctx, "req-read", v.Version)
	must(e)
	p := mustRead(t, s, "list_documents", map[string]any{"workspace_id": "demo", "kind": "requirement", "query": "no match"})
	if p.Total != 0 {
		t.Fatal("ignored document query")
	}
	must(st.ArchiveRequirement(ctx, "req-read", "user:operator", nil))
	p = mustRead(t, s, "list_documents", map[string]any{"workspace_id": "demo", "kind": "requirement", "include_archived": true})
	if p.Total != 1 || readItem(t, p, 0)["archived"] != true {
		t.Fatal("missing archived requirement")
	}
	must(st.RestoreRequirement(ctx, "req-read", "user:operator"))
	restored := readItem(t, mustRead(t, s, "get_document", map[string]any{"workspace_id": "demo", "kind": "requirement", "document_id": "req-read"}), 0)
	if restored["active_authority"] != true || restored["archived"] != false {
		t.Fatalf("restore=%v", restored)
	}
	events := mustRead(t, s, "list_document_events", map[string]any{"workspace_id": "demo", "kind": "requirement", "document_id": "req-read", "event_kind": "requirement.restored"})
	if events.Total != 1 {
		t.Fatalf("restore event missing: %+v", events)
	}
	_, _, e = st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-read", Name: "Read notes"}, core.ReferenceDocumentVersion{Content: "# Informative notes", Filename: "notes.md", ContentType: "text/markdown"})
	must(e)
	ref := readItem(t, mustRead(t, s, "get_document", map[string]any{"workspace_id": "demo", "kind": "reference", "document_id": "ref-read"}), 0)
	if ref["informative"] != true || ref["active_authority"] != false || ref["version_status"] != "informative" {
		t.Fatalf("reference authority=%v", ref)
	}
	decision, e := st.ProposeDecision(ctx, core.Decision{Statement: "Read protocol", Context: "Test", AlternativesRejected: "None", Origin: core.DecisionOriginOperator})
	must(e)
	_, e = st.ConfirmDecision(ctx, decision.ID)
	must(e)
	successor, e := st.ProposeDecision(ctx, core.Decision{Statement: "New read protocol", Context: "Test", AlternativesRejected: "None", Origin: core.DecisionOriginOperator, Supersedes: decision.ID})
	must(e)
	_, e = st.ConfirmDecision(ctx, successor.ID)
	must(e)
	p = mustRead(t, s, "list_decisions", map[string]any{"workspace_id": "demo"})
	if p.Total != 1 || readItem(t, p, 0)["id"] != successor.ID {
		t.Fatalf("decision discovery=%+v", p)
	}
	if _, err := mcpReadCall(t, s, "reader", "get_decision", map[string]any{"workspace_id": "demo", "decision_id": decision.ID}); !strings.Contains(err, "include_history") {
		t.Fatalf("implicit history=%s", err)
	}
	historical := readItem(t, mustRead(t, s, "get_decision", map[string]any{"workspace_id": "demo", "decision_id": decision.ID, "include_history": true}), 0)
	if historical["active_authority"] != false {
		t.Fatal("superseded decision authority")
	}
	// Structured types and unrecognized arguments are checked in dispatch, not just tools/list.
	for _, a := range []map[string]any{{"workspace_id": "demo", "kind": "system_design", "document_id": "id", "include_archived": "true"}, {"workspace_id": "demo", "kind": "system_design", "document_id": "id", "version": 0}, {"workspace_id": "demo", "kind": "system_design", "document_id": "id", "version": 1.5}, {"workspace_id": "demo", "kind": "unsupported", "document_id": "id"}} {
		if _, err := mcpReadCall(t, s, "reader", "get_document", a); err == "" {
			t.Fatalf("accepted %v", a)
		}
	}
}

type mcpReadSecretFixture struct{ fail bool }

func (f mcpReadSecretFixture) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	if f.fail {
		return nil, fmt.Errorf("private source failed")
	}
	return []string{"private-value-for-test"}, nil
}
func TestMCPReadRedactionAndExactEventIDs(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	s.WorkOrders = &workorder.Service{Store: s.Store, RedactionSecrets: mcpReadSecretFixture{}}
	if e := s.Store.CreateTask(ctx, core.Task{ID: "redaction", Workspace: "demo", State: core.TaskClosed, Body: "Contains private-value-for-test"}); e != nil {
		t.Fatal(e)
	}
	page := mustRead(t, s, "get_task", map[string]any{"workspace_id": "demo", "task_id": "redaction"})
	if body := readItem(t, page, 0)["body"].(string); strings.Contains(body, "private-value-for-test") || !strings.Contains(body, "REDACTED") {
		t.Fatalf("body=%s", body)
	}
	// JSON projection must preserve int64 values beyond floating-point precision.
	const eventID int64 = 9007199254740993
	s.Store = mcpReadEventStore{Store: s.Store, events: []core.Event{{ID: eventID, TaskID: "redaction", Kind: "test.precision", At: time.Now()}}}
	page = mustRead(t, s, "list_task_events", map[string]any{"workspace_id": "demo", "task_id": "redaction", "event_kind": "test.precision"})
	if len(page.Items) != 1 || !strings.Contains(string(page.Items[0]), fmt.Sprint(eventID)) {
		t.Fatalf("event ID lost precision: %s", page.Items)
	}
	s.WorkOrders.RedactionSecrets = mcpReadSecretFixture{fail: true}
	if _, e := mcpReadCall(t, s, "reader", "get_task", map[string]any{"workspace_id": "demo", "task_id": "redaction"}); e != "read redaction unavailable" {
		t.Fatalf("redaction did not fail closed: %s", e)
	}
}

type mcpReadEventStore struct {
	store.Store
	events []core.Event
}

func (s mcpReadEventStore) ReadTaskEventWindow(_ context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	events := []core.Event{}
	for _, event := range s.events {
		if q.Kind == "" || event.Kind == q.Kind {
			events = append(events, event)
		}
	}
	return store.TaskEventWindow{Boundary: store.TaskEventBoundary{MaxID: s.events[len(s.events)-1].ID, Count: len(events)}, Events: events}, nil
}
func TestMCPReadChronologicalTieBreakAndUnknownActor(t *testing.T) {
	at := time.Now()
	events := mcpReadEvents([]core.Event{{ID: 3, At: at.Add(time.Second)}, {ID: 2, At: at}, {ID: 1, At: at}}, "")
	for i, item := range events {
		p := item.(map[string]any)
		if p["id"] != int64(i+1) || p["actor_id"] != "" || p["actor_role"] != core.ActorRole("") {
			t.Fatalf("event=%v", p)
		}
	}
}

func TestMCPReadDismissalArchive(t *testing.T) {
	for _, tier := range []string{"requirement", "system_design"} {
		t.Run(tier, func(t *testing.T) {
			s, ctx := newMCPReadFixture(t)
			st := s.Store
			id := "dismissed-only"
			var err error
			ctx, err = store.WithDocumentDismissalNote(ctx, "Not needed")
			if err != nil {
				t.Fatal(err)
			}
			if tier == "requirement" {
				_, _, err = st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "# Proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep history."}}})
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = st.DismissRequirementVersion(ctx, id, 1)
			} else {
				_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, core.SystemDesignVersion{Content: "# Proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths: [internal/**]\n```", Origin: core.SystemDesignOriginOperator})
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = st.DismissSystemDesignVersion(ctx, id, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			a := map[string]any{"workspace_id": "demo", "kind": tier, "query": id}
			if p := mustRead(t, s, "list_documents", a); p.Total != 0 {
				t.Fatalf("live list=%+v", p)
			}
			a["include_archived"] = true
			p := mustRead(t, s, "list_documents", a)
			if p.Total != 1 {
				t.Fatalf("archive list=%+v", p)
			}
			d := readItem(t, p, 0)
			if d["archived"] != true || d["archive_reason"] != core.ArchiveReasonOnlyProposalDismissed || d["archive_note"] != "Not needed" || d["active_authority"] != false {
				t.Fatalf("archive=%v", d)
			}
			a = map[string]any{"workspace_id": "demo", "kind": tier, "document_id": id, "include_archived": true}
			d = readItem(t, mustRead(t, s, "get_document", a), 0)
			if d["authority_absent"] != true || d["archive_reason"] != core.ArchiveReasonOnlyProposalDismissed || d["active_authority"] != false {
				t.Fatalf("read=%v", d)
			}
			a["version"] = 1
			d = readItem(t, mustRead(t, s, "get_document", a), 0)
			if d["active_authority"] != false || !strings.Contains(fmt.Sprint(d["content"]), "# Proposal") {
				t.Fatalf("history=%v", d)
			}
		})
	}
}

// mcpReadTestBase is the injected cache clock's start; capacity tests never sleep.
var mcpReadTestBase = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type mcpReadTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mcpReadTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *mcpReadTestClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
func (c *mcpReadTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lockedMembership serializes the shared membership fixture so parallel reads
// are race-free; role changes go through the same lock.
type lockedMembership struct {
	mu sync.Mutex
	*membershipFixture
}

func (m *lockedMembership) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.membershipFixture.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
}
func (m *lockedMembership) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.membershipFixture.AuthorizeDeployment(ctx, userID, capability)
}
func (m *lockedMembership) ListWorkspacesForUser(ctx context.Context, userID string) ([]core.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.membershipFixture.ListWorkspacesForUser(ctx, userID)
}
func (m *lockedMembership) setRole(userID, workspaceID string, role core.WorkspaceRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if role == "" {
		delete(m.roles[userID], workspaceID)
		return
	}
	m.roles[userID][workspaceID] = role
}

// newMCPCapacityFixture adds credentials for cache-identity tests: reader-2 is
// a second credential of user reader; c1..c6 belong to distinct users. reader
// can also read the private workspace. The cache clock is injected.
func newMCPCapacityFixture(t *testing.T) (*Server, context.Context, *mcpReadTestClock, *lockedMembership) {
	t.Helper()
	s, ctx := newMCPReadFixture(t)
	clock := &mcpReadTestClock{now: mcpReadTestBase}
	s.mcpReads.clock = clock.Now
	membership := s.Memberships.(*membershipFixture)
	credentials := s.Credentials.(staticCredentialVerifier)
	credentials["reader-2"] = core.AuthenticatedCredential{ID: "reader-pat-2", OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	membership.roles["reader"]["private"] = core.WorkspaceRoleViewer
	for i := 1; i <= 6; i++ {
		token, user := fmt.Sprintf("c%d", i), fmt.Sprintf("user-%d", i)
		credentials[token] = core.AuthenticatedCredential{ID: token + "-pat", OwnerUserID: user, Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
		membership.roles[user] = map[string]core.WorkspaceRole{"demo": core.WorkspaceRoleViewer}
	}
	locked := &lockedMembership{membershipFixture: membership}
	s.Memberships = locked
	return s, ctx, clock, locked
}

// mcpReadTry is a goroutine-safe tools/call: it reports instead of failing.
func mcpReadTry(h http.Handler, token, name string, args map[string]any) (mcpReadPage, string, string, error) {
	wire, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(wire)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return mcpReadPage{}, "", "", fmt.Errorf("MCP HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || len(envelope.Result.Content) != 1 {
		return mcpReadPage{}, "", "", fmt.Errorf("unexpected response %s", rec.Body.String())
	}
	text := envelope.Result.Content[0].Text
	if envelope.Result.IsError {
		return mcpReadPage{}, "", text, nil
	}
	if len(text) > mcpReadMaxBytes {
		return mcpReadPage{}, "", "", fmt.Errorf("unbounded response: %d", len(text))
	}
	var page mcpReadPage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		return mcpReadPage{}, "", "", err
	}
	return page, text, "", nil
}

func readAs(t *testing.T, h http.Handler, token, name string, args map[string]any) mcpReadPage {
	t.Helper()
	page, _, toolErr, err := mcpReadTry(h, token, name, args)
	if err != nil || toolErr != "" {
		t.Fatalf("%s as %s: %v %s", name, token, err, toolErr)
	}
	return page
}

func refusalAs(t *testing.T, h http.Handler, token, name string, args map[string]any) string {
	t.Helper()
	_, _, toolErr, err := mcpReadTry(h, token, name, args)
	if err != nil || toolErr == "" {
		t.Fatalf("%s as %s was not refused: %v", name, token, err)
	}
	return toolErr
}

func withArgs(base map[string]any, kv ...any) map[string]any {
	args := maps.Clone(base)
	for i := 0; i+1 < len(kv); i += 2 {
		args[kv[i].(string)] = kv[i+1]
	}
	return args
}

type mcpCacheOccupancy struct{ snapshots, cursors int }

// mcpCacheView copies the cache state, including access sequence and expiry.
func mcpCacheView(s *Server) (map[string]mcpReadSnapshot, map[string]mcpEventCursor) {
	s.mcpReads.mu.Lock()
	defer s.mcpReads.mu.Unlock()
	entries, cursors := map[string]mcpReadSnapshot{}, map[string]mcpEventCursor{}
	maps.Copy(entries, s.mcpReads.entries)
	maps.Copy(cursors, s.mcpReads.cursors)
	return entries, cursors
}

func mcpCacheOwners(s *Server) map[string]mcpCacheOccupancy {
	entries, cursors := mcpCacheView(s)
	owners := map[string]mcpCacheOccupancy{}
	for _, v := range entries {
		o := owners[v.owner]
		o.snapshots++
		owners[v.owner] = o
	}
	for _, v := range cursors {
		o := owners[v.owner]
		o.cursors++
		owners[v.owner] = o
	}
	return owners
}

func assertCacheUnchanged(t *testing.T, s *Server, step string, entries map[string]mcpReadSnapshot, cursors map[string]mcpEventCursor) {
	t.Helper()
	afterEntries, afterCursors := mcpCacheView(s)
	if !reflect.DeepEqual(entries, afterEntries) || !reflect.DeepEqual(cursors, afterCursors) {
		t.Fatalf("%s changed the cache: %d/%d snapshots, %d/%d cursors", step, len(entries), len(afterEntries), len(cursors), len(afterCursors))
	}
}

// TestMCPReadCredentialQuotaParallelIsolation starts many simultaneous first
// reads under one credential. Its quota stays at eight in every admission while
// a second credential (same user, a distinct user, or with A spread over two
// workspaces) keeps every snapshot it admitted.
func TestMCPReadCredentialQuotaParallelIsolation(t *testing.T) {
	for name, tc := range map[string]struct {
		other, otherID string
		workspaces     []string
	}{
		"same owner user":     {"reader-2", "reader-pat-2", []string{"demo"}},
		"multiple workspaces": {"c1", "c1-pat", []string{"demo", "private"}},
		"distinct owners":     {"other", "other-pat", []string{"demo"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := newMCPCapacityFixture(t)
			h := s.Handler()
			s.mcpReads.beforeAdmit = func() {
				for owner, o := range mcpCacheOwners(s) {
					if o.snapshots > mcpReadCredentialSnapshots || o.cursors > mcpReadCredentialCursors {
						t.Errorf("%s holds %d snapshots and %d cursors", owner, o.snapshots, o.cursors)
					}
				}
			}
			type result struct{ token, snapshot, failure string }
			const readsA, readsB = 3 * mcpReadCredentialSnapshots, 4
			results := make(chan result, readsA+readsB)
			start := make(chan struct{})
			var ready, done sync.WaitGroup
			launch := func(token, workspace string) {
				ready.Add(1)
				done.Add(1)
				go func() {
					defer done.Done()
					ready.Done()
					<-start
					page, _, toolErr, err := mcpReadTry(h, token, "list_tasks", map[string]any{"workspace_id": workspace})
					failure := toolErr
					if err != nil {
						failure = err.Error()
					}
					results <- result{token, page.Snapshot, failure}
				}()
			}
			for i := range readsA {
				launch("reader", tc.workspaces[i%len(tc.workspaces)])
			}
			for range readsB {
				launch(tc.other, "demo")
			}
			ready.Wait()
			close(start)
			done.Wait()
			close(results)
			var tokensA, tokensB []string
			for r := range results {
				if r.failure != "" || r.snapshot == "" {
					t.Fatalf("%s first read failed: %q", r.token, r.failure)
				}
				if r.token == "reader" {
					tokensA = append(tokensA, r.snapshot)
				} else {
					tokensB = append(tokensB, r.snapshot)
				}
			}
			owners := mcpCacheOwners(s)
			if owners["reader-pat"].snapshots != mcpReadCredentialSnapshots || owners[tc.otherID].snapshots != readsB || len(owners) != 2 {
				t.Fatalf("occupancy=%+v", owners)
			}
			entries, _ := mcpCacheView(s)
			retainedA := 0
			for _, token := range tokensA {
				if _, ok := entries[token]; ok {
					retainedA++
				}
			}
			if retainedA != mcpReadCredentialSnapshots {
				t.Fatalf("credential A retained %d of its own tokens", retainedA)
			}
			// B never lost a slot to A, and neither credential can use the other's
			// tokens even when both belong to one user.
			for _, token := range tokensB {
				readAs(t, h, tc.other, "list_tasks", map[string]any{"workspace_id": "demo", "snapshot": token})
				if e := refusalAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "snapshot": token}); e != "snapshot unavailable: restart read" {
					t.Fatalf("A used B's token: %q", e)
				}
			}
			for _, token := range tokensA {
				if entry, ok := entries[token]; ok {
					if e := refusalAs(t, h, tc.other, "list_tasks", map[string]any{"workspace_id": entry.workspace, "snapshot": token}); !strings.Contains(e, "snapshot unavailable") && !strings.Contains(e, "workspace_not_found") {
						t.Fatalf("B used A's token: %q", e)
					}
				}
			}
			readAs(t, h, tc.other, "list_tasks", map[string]any{"workspace_id": "demo"})
			// The quota spans workspaces: switching workspace evicts A's own
			// oldest snapshots instead of opening a second allowance.
			for range mcpReadCredentialSnapshots {
				readAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "private"})
			}
			entries, _ = mcpCacheView(s)
			for _, entry := range entries {
				if entry.owner == "reader-pat" && entry.workspace != "private" {
					t.Fatal("a second workspace widened credential A's quota")
				}
			}
			if owners = mcpCacheOwners(s); owners["reader-pat"].snapshots != mcpReadCredentialSnapshots || owners[tc.otherID].snapshots != readsB+1 {
				t.Fatalf("occupancy after workspace switch=%+v", owners)
			}
		})
	}
}

// TestMCPReadSingleItemDoesNotRetainSnapshot proves complete single-item reads
// stay available at a full process cache because they retain nothing, while
// get_task_context still retains and pages. Not-found identities are the
// zero-result case: a refusal that also retains nothing.
func TestMCPReadSingleItemDoesNotRetainSnapshot(t *testing.T) {
	s, ctx, clock, _ := newMCPCapacityFixture(t)
	st := s.Store
	h := s.Handler()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateTask(ctx, core.Task{ID: "single", Workspace: "demo", Title: "Single", State: core.TaskQueued}))
	_, design, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-single", Title: "Single", Category: "Component"}, core.SystemDesignVersion{Content: "# Single\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
	must(err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.DocumentID, design.Version)
	must(err)
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Single read", Context: "Test", AlternativesRejected: "None", Origin: core.DecisionOriginOperator})
	must(err)
	_, err = st.ConfirmDecision(ctx, decision.ID)
	must(err)
	_, _, err = st.ProposeTaskContext(store.WithActor(ctx, store.Actor{ID: "agent:triage", Role: core.ActorAgent}), core.TaskContextProposalInput{TaskID: "single", TargetKind: core.TaskContextProposalSystemDesign, TargetID: design.DocumentID, Source: core.TaskContextProposalTriage, Justification: "context"})
	must(err)
	_, err = st.ConfirmTaskContextProposal(store.WithActor(ctx, store.Actor{ID: "user:operator", Role: core.ActorUser}), "single", core.TaskContextProposalSystemDesign, design.DocumentID)
	must(err)

	// Fill the process ceiling from four other credentials.
	for _, token := range []string{"c1", "c2", "c3", "c4"} {
		for range mcpReadCredentialSnapshots {
			readAs(t, h, token, "list_tasks", map[string]any{"workspace_id": "demo"})
		}
	}
	entries, cursors := mcpCacheView(s)
	if len(entries) != mcpReadSnapshotCount {
		t.Fatalf("fixture holds %d snapshots", len(entries))
	}
	for name, tc := range map[string]struct{ found, missing map[string]any }{
		"get_task":     {map[string]any{"workspace_id": "demo", "task_id": "single"}, map[string]any{"workspace_id": "demo", "task_id": "absent"}},
		"get_document": {map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": design.DocumentID}, map[string]any{"workspace_id": "demo", "kind": "system_design", "document_id": "component-absent"}},
		"get_decision": {map[string]any{"workspace_id": "demo", "decision_id": decision.ID}, map[string]any{"workspace_id": "demo", "decision_id": "DEC-999999"}},
	} {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < mcpReadSnapshotCount+8; i++ {
				page, raw, toolErr, err := mcpReadTry(h, "reader", name, tc.found)
				if err != nil || toolErr != "" {
					t.Fatalf("read %d at a full cache: %v %s", i, err, toolErr)
				}
				var fields map[string]json.RawMessage
				must(json.Unmarshal([]byte(raw), &fields))
				_, snapshot := fields["snapshot"]
				_, expires := fields["expires_at"]
				_, next := fields["next_offset"]
				if page.Total != 1 || len(page.Items) != 1 || page.Offset != 0 || snapshot || expires || next || !strings.Contains(page.Evidence, "restart the read") {
					t.Fatalf("single-item envelope=%s", raw)
				}
			}
			if e := refusalAs(t, h, "reader", name, tc.missing); strings.Contains(e, "capacity") {
				t.Fatalf("zero-result read reached admission: %q", e)
			}
			// Explicit snapshot and offset arguments keep their validation.
			if e := refusalAs(t, h, "reader", name, withArgs(tc.found, "snapshot", strings.Repeat("ab", 16))); e != "snapshot unavailable: restart read" {
				t.Fatalf("explicit snapshot=%q", e)
			}
			if e := refusalAs(t, h, "reader", name, withArgs(tc.found, "offset", 1)); e != "offset requires snapshot" {
				t.Fatalf("offset without snapshot=%q", e)
			}
		})
	}
	assertCacheUnchanged(t, s, "single-item reads", entries, cursors)

	// get_task_context has several entries, so it still retains and pages.
	contextArgs := map[string]any{"workspace_id": "demo", "task_id": "single", "limit": 1}
	if e := refusalAs(t, h, "reader", "get_task_context", contextArgs); !strings.HasPrefix(e, "snapshot capacity reached: process limit 32") {
		t.Fatalf("get_task_context did not need a snapshot: %q", e)
	}
	clock.Advance(mcpReadSnapshotTTL)
	first := readAs(t, h, "reader", "get_task_context", contextArgs)
	if first.Snapshot == "" || first.NextOffset == nil || *first.NextOffset != 1 || first.Total < 2 || first.ExpiresAt.IsZero() {
		t.Fatalf("get_task_context page=%+v", first)
	}
	second := readAs(t, h, "reader", "get_task_context", withArgs(contextArgs, "snapshot", first.Snapshot, "offset", 1))
	if len(second.Items) != 1 || second.Snapshot != first.Snapshot {
		t.Fatalf("get_task_context second page=%+v", second)
	}
}

// TestMCPReadCredentialLRUEviction admits a ninth snapshot at the caller's
// quota and proves the caller's untouched least-recently-used snapshot is the
// victim, accesses never extend expiry, and equal access sequences break ties
// by token.
func TestMCPReadCredentialLRUEviction(t *testing.T) {
	s, _, clock, _ := newMCPCapacityFixture(t)
	h := s.Handler()
	args := map[string]any{"workspace_id": "demo", "limit": 1}
	sameUser := readAs(t, h, "reader-2", "list_tasks", args).Snapshot
	distinct := readAs(t, h, "other", "list_tasks", args).Snapshot
	tokens, expires := []string{}, []time.Time{}
	for range mcpReadCredentialSnapshots {
		page := readAs(t, h, "reader", "list_tasks", args)
		tokens, expires = append(tokens, page.Snapshot), append(expires, page.ExpiresAt)
		clock.Advance(time.Second)
	}
	if !expires[0].Equal(mcpReadTestBase.Add(mcpReadSnapshotTTL)) {
		t.Fatalf("expiry=%s", expires[0])
	}
	// A successful page read refreshes the oldest; a failed one does not.
	if touched := readAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", tokens[0])); !touched.ExpiresAt.Equal(expires[0]) {
		t.Fatalf("access extended expiry: %s", touched.ExpiresAt)
	}
	if e := refusalAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", tokens[1], "offset", 5)); e != "offset exceeds snapshot" {
		t.Fatalf("failed page=%q", e)
	}
	clock.Advance(time.Minute)
	ninth := readAs(t, h, "reader", "list_tasks", args)
	if e := refusalAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", tokens[1])); e != "snapshot unavailable: restart read" {
		t.Fatalf("untouched LRU snapshot survived: %q", e)
	}
	for i, token := range append(append([]string{tokens[0]}, tokens[2:]...), ninth.Snapshot) {
		page := readAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", token))
		if i == 0 && !page.ExpiresAt.Equal(expires[0]) {
			t.Fatalf("touched snapshot expiry moved: %s", page.ExpiresAt)
		}
	}
	readAs(t, h, "reader-2", "list_tasks", withArgs(args, "snapshot", sameUser))
	readAs(t, h, "other", "list_tasks", withArgs(args, "snapshot", distinct))
	if owners := mcpCacheOwners(s); owners["reader-pat"].snapshots != mcpReadCredentialSnapshots || owners["reader-pat-2"].snapshots != 1 || owners["other-pat"].snapshots != 1 {
		t.Fatalf("occupancy=%+v", owners)
	}

	// Equal access sequences evict the lexicographically smaller token.
	s.mcpReads.mu.Lock()
	retained := []string{}
	for token, entry := range s.mcpReads.entries {
		if entry.owner == "reader-pat" {
			retained = append(retained, token)
		}
	}
	slices.Sort(retained)
	low, high := retained[2], retained[5]
	for _, token := range []string{high, low} {
		entry := s.mcpReads.entries[token]
		entry.access = 0
		s.mcpReads.entries[token] = entry
	}
	s.mcpReads.mu.Unlock()
	readAs(t, h, "reader", "list_tasks", args)
	entries, _ := mcpCacheView(s)
	if _, ok := entries[low]; ok {
		t.Fatal("tie-break kept the lower token")
	}
	if _, ok := entries[high]; !ok {
		t.Fatal("tie-break evicted the higher token")
	}
	// Expiry stays fixed at capture regardless of later accesses.
	clock.Set(expires[0])
	if e := refusalAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", tokens[0])); e != "snapshot unavailable: restart read" {
		t.Fatalf("touched snapshot outlived its expiry: %q", e)
	}
}

// TestMCPReadProcessCeilingRefusal fills 32 slots from credentials below their
// quotas. Another credential is refused with the resource, count and earliest
// expiry; existing pages stay usable; the expiry sweep admits it.
func TestMCPReadProcessCeilingRefusal(t *testing.T) {
	s, ctx, clock, _ := newMCPCapacityFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "quiet", Workspace: "demo", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	args := map[string]any{"workspace_id": "demo"}
	type held struct{ credential, token string }
	var all []held
	for _, plan := range []struct {
		credential string
		reads      int
	}{{"reader", 7}, {"reader-2", 7}, {"c1", 6}, {"c2", 6}, {"c3", 6}} {
		for range plan.reads {
			all = append(all, held{plan.credential, readAs(t, h, plan.credential, "list_tasks", args).Snapshot})
			clock.Advance(time.Second)
		}
	}
	if len(all) != mcpReadSnapshotCount {
		t.Fatalf("fixture holds %d", len(all))
	}
	entries, cursors := mcpCacheView(s)
	want := "snapshot capacity reached: process limit 32 retained snapshots; retry at 2026-10-08T12:05:00Z after expiry (within 5 minutes)"
	for _, credential := range []string{"c4", "reader", "c1"} {
		for _, name := range []string{"list_tasks", "list_task_events"} {
			a := args
			if name == "list_task_events" {
				a = map[string]any{"workspace_id": "demo", "task_id": "quiet"}
			}
			e := refusalAs(t, h, credential, name, a)
			if e != want {
				t.Fatalf("%s refusal=%q", credential, e)
			}
			for _, secret := range []string{"pat", "workspace_id", "list_tasks", all[0].token, "reader"} {
				if strings.Contains(e, secret) {
					t.Fatalf("refusal disclosed %q: %q", secret, e)
				}
			}
		}
	}
	assertCacheUnchanged(t, s, "process refusal", entries, cursors)
	for _, h2 := range all {
		readAs(t, h, h2.credential, "list_tasks", withArgs(args, "snapshot", h2.token))
	}
	// The earliest expiry frees exactly one slot.
	clock.Set(mcpReadTestBase.Add(mcpReadSnapshotTTL))
	admitted := readAs(t, h, "c4", "list_tasks", args)
	if e := refusalAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", all[0].token)); e != "snapshot unavailable: restart read" {
		t.Fatalf("expired snapshot=%q", e)
	}
	readAs(t, h, "c4", "list_tasks", withArgs(args, "snapshot", admitted.Snapshot))
	readAs(t, h, all[1].credential, "list_tasks", withArgs(args, "snapshot", all[1].token))
	if e := refusalAs(t, h, "c5", "list_tasks", args); e != "snapshot capacity reached: process limit 32 retained snapshots; retry at 2026-10-08T12:05:01Z after expiry (within 5 minutes)" {
		t.Fatalf("next earliest expiry=%q", e)
	}
	// Sub-second expiries round up so the stated time follows the expiry.
	if got := mcpReadRetryAt(mcpReadTestBase.Add(1500 * time.Millisecond)); got != "2026-10-08T12:00:02Z" {
		t.Fatalf("rounding=%s", got)
	}
}

// mcpCountingStore counts the store reads behind list_tasks and get_task.
type mcpCountingStore struct {
	store.Store
	reads *atomic.Int64
}

func (s mcpCountingStore) ListTaskPage(ctx context.Context, q store.TaskOperationsQuery) (store.TaskPage, error) {
	s.reads.Add(1)
	return s.Store.ListTaskPage(ctx, q)
}
func (s mcpCountingStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	s.reads.Add(1)
	return s.Store.GetTask(ctx, id)
}

// TestMCPReadAdmissionFailureLeavesCacheUnchanged runs every pre-admission
// failure while the caller is at its quota: nothing is retained, evicted or
// touched. Revoked membership refuses before any store or cache access.
func TestMCPReadAdmissionFailureLeavesCacheUnchanged(t *testing.T) {
	s, ctx, _, membership := newMCPCapacityFixture(t)
	reads := &atomic.Int64{}
	for i := range 300 {
		if err := s.Store.CreateTask(ctx, core.Task{ID: fmt.Sprintf("bulky-%03d", i), Workspace: "demo", Title: fmt.Sprintf("bulky-%03d %s", i, strings.Repeat("b", 4000)), State: core.TaskQueued}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.CreateTask(ctx, core.Task{ID: "huge", Workspace: "demo", Body: strings.Repeat("x", mcpReadMaxBytes), State: core.TaskClosed}); err != nil {
		t.Fatal(err)
	}
	s.Store = mcpCountingStore{Store: s.Store, reads: reads}
	// A user credential without an ID never shares an anonymous cache owner.
	s.Credentials.(staticCredentialVerifier)["anonymous"] = core.AuthenticatedCredential{OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	s.Credentials.(staticCredentialVerifier)["blank"] = core.AuthenticatedCredential{ID: " ", OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	h := s.Handler()
	args := map[string]any{"workspace_id": "demo", "query": "absent", "limit": 1}
	var held []string
	for range mcpReadCredentialSnapshots {
		held = append(held, readAs(t, h, "reader", "list_tasks", args).Snapshot)
	}
	readAs(t, h, "other", "list_tasks", args)
	entries, cursors := mcpCacheView(s)
	for name, tc := range map[string]struct {
		call  func() string
		want  string
		setup func() func()
	}{
		"snapshot byte budget": {call: func() string {
			return refusalAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "query": "bulky"})
		}, want: "snapshot byte budget"},
		"output budget": {call: func() string {
			return refusalAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "query": "bulky-0", "limit": 25})
		}, want: "65536-byte output budget"},
		"oversized single item": {call: func() string {
			return refusalAs(t, h, "reader", "get_task", map[string]any{"workspace_id": "demo", "task_id": "huge"})
		}, want: "65536-byte output budget"},
		"missing credential ID": {call: func() string {
			return refusalAs(t, h, "anonymous", "list_tasks", map[string]any{"workspace_id": "demo"})
		}, want: "list_tasks requires an operator-scoped user credential"},
		"blank credential ID": {call: func() string {
			return refusalAs(t, h, "blank", "list_tasks", map[string]any{"workspace_id": "demo"})
		}, want: "list_tasks requires an identified credential"},
		"invalid arguments": {call: func() string {
			return refusalAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "demo", "limit": 101})
		}, want: "invalid limit"},
		"redaction failure": {call: func() string {
			return refusalAs(t, h, "reader", "list_tasks", map[string]any{"workspace_id": "demo"})
		}, want: "read redaction unavailable", setup: func() func() {
			original := s.WorkOrders
			s.WorkOrders = &workorder.Service{Store: s.Store, RedactionSecrets: mcpReadSecretFixture{fail: true}}
			return func() { s.WorkOrders = original }
		}},
	} {
		if tc.setup != nil {
			restore := tc.setup()
			if e := tc.call(); !strings.Contains(e, tc.want) {
				t.Fatalf("%s: %q", name, e)
			}
			restore()
		} else if e := tc.call(); !strings.Contains(e, tc.want) {
			t.Fatalf("%s: %q", name, e)
		}
		assertCacheUnchanged(t, s, name, entries, cursors)
	}
	// Revocation refuses snapshot follow-ups and first reads before the store
	// or cache is consulted; restoring access shows nothing was consumed.
	membership.setRole("reader", "demo", "")
	before := reads.Load()
	for _, a := range []map[string]any{withArgs(args, "snapshot", held[0]), args} {
		if e := refusalAs(t, h, "reader", "list_tasks", a); !strings.Contains(e, "workspace_not_found") {
			t.Fatalf("revoked read=%q", e)
		}
	}
	if reads.Load() != before {
		t.Fatal("revoked read reached the store")
	}
	assertCacheUnchanged(t, s, "revoked membership", entries, cursors)
	membership.setRole("reader", "demo", core.WorkspaceRoleViewer)
	for _, token := range held {
		readAs(t, h, "reader", "list_tasks", withArgs(args, "snapshot", token))
	}
}
