package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestConfirmedHeadingRenamesDocumentAcrossFreshReads drives both document
// tiers through the operator REST routes and checks every fresh read surface
// before proposal, after proposal, and after confirmation
// (req-document-operating-surfaces AC-6.1; component-document-corpus).
func TestConfirmedHeadingRenamesDocumentAcrossFreshReads(t *testing.T) {
	mcp, ctx := newMCPReadFixture(t)
	st := mcp.Store
	rest := NewServer(st)
	rest.Workspace, rest.BearerToken = "demo", "token"
	handler := rest.Handler()
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload string
		if body != nil {
			wire, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(wire)
		}
		request := httptest.NewRequest(method, path, strings.NewReader(payload))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("X-Conveyor-Actor", "user:title-operator")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	task := core.Task{ID: "title-task", Workspace: "demo", Repo: "conveyor", Title: "Title reader", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	governs := "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"
	requirementFence := "\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Titles follow confirmed headings.\n```"
	for _, tier := range []struct {
		name, route, mcpKind, id, eventKind, idKey string
		contextKind                                core.TaskContextProposalTargetKind
		create                                     map[string]any
		suffix                                     string
	}{
		{
			name: "requirement", route: "/v1/requirements", mcpKind: "requirement", id: "req-title-read",
			eventKind: "requirement.title_changed", idKey: "requirement_id", contextKind: core.TaskContextProposalRequirement,
			create: map[string]any{"id": "req-title-read", "title": "Listed requirement", "content": "# Listed requirement\n\nIntent prose." + requirementFence},
			suffix: requirementFence,
		},
		{
			name: "system design", route: "/v1/system-designs", mcpKind: "system_design", id: "design-title-read",
			eventKind: "system_design.title_changed", idKey: "document_id", contextKind: core.TaskContextProposalSystemDesign,
			create: map[string]any{"id": "design-title-read", "title": "Listed design", "category": "Architecture", "content": "# Listed design\n\nMechanism prose." + governs},
			suffix: governs,
		},
	} {
		t.Run(tier.name, func(t *testing.T) {
			oldTitle := tier.create["title"].(string)
			newTitle := "Renamed " + tier.name
			if response := call(http.MethodPost, tier.route, tier.create); response.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
			}
			if response := call(http.MethodPost, tier.route+"/"+tier.id+"/versions/1/confirm", nil); response.Code != http.StatusOK {
				t.Fatalf("confirm v1 status=%d body=%s", response.Code, response.Body.String())
			}
			proposer := store.WithActor(ctx, store.Actor{ID: "agent:triage", Role: core.ActorAgent})
			if _, _, err := st.ProposeTaskContext(proposer, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: tier.contextKind, TargetID: tier.id, Source: core.TaskContextProposalTriage, Justification: "title reads"}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ConfirmTaskContextProposal(store.WithActor(ctx, store.Actor{ID: "user:operator", Role: core.ActorUser}), task.ID, tier.contextKind, tier.id); err != nil {
				t.Fatal(err)
			}
			slug := assertFreshDocumentTitle(t, call, mcp, tier.route, tier.mcpKind, tier.id, task.ID, oldTitle, "")

			if response := call(http.MethodPost, tier.route+"/"+tier.id+"/versions", map[string]any{"content": "# " + newTitle + "\n\nRevised prose." + tier.suffix}); response.Code != http.StatusCreated {
				t.Fatalf("propose status=%d body=%s", response.Code, response.Body.String())
			}
			assertFreshDocumentTitle(t, call, mcp, tier.route, tier.mcpKind, tier.id, task.ID, oldTitle, slug)

			confirmed := call(http.MethodPost, tier.route+"/"+tier.id+"/versions/2/confirm", nil)
			if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), `"title":"`+newTitle+`"`) {
				t.Fatalf("confirm v2 status=%d body=%s", confirmed.Code, confirmed.Body.String())
			}
			assertFreshDocumentTitle(t, call, mcp, tier.route, tier.mcpKind, tier.id, task.ID, newTitle, slug)

			// Direct and paginated REST history both carry the complete record.
			readPage := func(query string) store.DocumentEventPage {
				t.Helper()
				response := call(http.MethodGet, tier.route+"/"+tier.id+"/events"+query, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("events%s status=%d body=%s", query, response.Code, response.Body.String())
				}
				var page store.DocumentEventPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				return page
			}
			direct := readPage("")
			paged := readPage("?limit=1")
			for offset := 1; offset < paged.Total; offset++ {
				next := readPage("?limit=1&offset=" + strconv.Itoa(offset) + "&snapshot_id=" + strconv.FormatInt(paged.SnapshotID, 10))
				paged.Events = append(paged.Events, next.Events...)
			}
			for _, events := range [][]core.Event{direct.Events, paged.Events} {
				var renames []map[string]any
				for _, event := range events {
					if event.Kind != tier.eventKind {
						continue
					}
					var payload map[string]any
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					renames = append(renames, payload)
				}
				if len(renames) != 1 || renames[0]["old_title"] != oldTitle || renames[0]["new_title"] != newTitle ||
					renames[0]["version"] != float64(2) || renames[0]["confirmed_by"] != "user:local-operator" || renames[0][tier.idKey] != tier.id || renames[0]["workspace_id"] != "demo" {
					t.Fatalf("rename history=%v", renames)
				}
			}
			events := mustRead(t, mcp, "list_document_events", map[string]any{"workspace_id": "demo", "kind": tier.mcpKind, "document_id": tier.id, "event_kind": tier.eventKind})
			if events.Total != 1 {
				t.Fatalf("MCP rename history=%+v", events)
			}
		})
	}
}

// assertFreshDocumentTitle checks the REST list and detail, MCP list_documents
// and get_document, and MCP get_task_context for the expected listed title,
// and returns the unchanged slug.
func assertFreshDocumentTitle(t *testing.T, call func(string, string, any) *httptest.ResponseRecorder, mcp *Server, route, kind, id, taskID, title, slug string) string {
	t.Helper()
	list := call(http.MethodGet, route, nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"title":"`+title+`"`) {
		t.Fatalf("REST list status=%d lacks title %q: %s", list.Code, title, list.Body.String())
	}
	detail := call(http.MethodGet, route+"/"+id, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("REST detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(detail.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	identity := body["requirement"]
	if identity == nil {
		identity = body["document"]
	}
	var document struct {
		ID, Slug, Title string
	}
	if err := json.Unmarshal(identity, &document); err != nil {
		t.Fatal(err)
	}
	if document.ID != id || document.Title != title || (slug != "" && document.Slug != slug) {
		t.Fatalf("REST detail identity=%+v, want id=%q title=%q slug=%q", document, id, title, slug)
	}
	listed := mustRead(t, mcp, "list_documents", map[string]any{"workspace_id": "demo", "kind": kind})
	found := false
	for index := range listed.Items {
		item := readItem(t, listed, index)
		if item["id"] == id {
			found = true
			if item["title"] != title {
				t.Fatalf("MCP list_documents item=%v, want title %q", item, title)
			}
		}
	}
	if !found {
		t.Fatalf("MCP list_documents omitted %s: %+v", id, listed)
	}
	read := readItem(t, mustRead(t, mcp, "get_document", map[string]any{"workspace_id": "demo", "kind": kind, "document_id": id}), 0)
	if !strings.Contains(mustJSON(t, read), `"title":"`+title+`"`) {
		t.Fatalf("MCP get_document=%v, want title %q", read, title)
	}
	taskContext := mustRead(t, mcp, "get_task_context", map[string]any{"workspace_id": "demo", "task_id": taskID})
	for index := range taskContext.Items {
		item := readItem(t, taskContext, index)
		if item["relation"] == "attached" && item["id"] == id {
			if item["title"] != title {
				t.Fatalf("MCP get_task_context pin=%v, want title %q", item, title)
			}
			return document.Slug
		}
	}
	t.Fatalf("MCP get_task_context omitted %s: %+v", id, taskContext)
	return ""
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(wire)
}
