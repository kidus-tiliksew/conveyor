package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func (s mcpReadEventStore) ListEvents(context.Context, string) ([]core.Event, error) {
	return append([]core.Event(nil), s.events...), nil
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
