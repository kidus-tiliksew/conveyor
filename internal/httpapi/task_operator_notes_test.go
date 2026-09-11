package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// req-260810-23b69f AC-5.2: browser activity carries workspace-scoped notes
// only while the proposing task remains non-terminal.
func TestTaskActivityOperatorNotes(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	for _, id := range []string{"noted-task", "bare-task"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", State: core.TaskRunning}); err != nil {
			t.Fatal(err)
		}
	}
	for _, workspace := range []string{"demo", "other"} {
		scoped := store.WithWorkspace(ctx, workspace)
		noted, err := store.WithDocumentDismissalNote(scoped, workspace+" reason")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreateRequirement(scoped, core.Requirement{ID: "req-note", Title: "Intent"}, core.RequirementVersion{
			Content: "# Intent", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}},
			Origin: core.RequirementOriginImplementation, OriginTaskID: "noted-task",
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.DismissRequirementVersion(noted, "req-note", 1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreateSystemDesign(scoped, core.SystemDesign{ID: "design-note", Title: "Design", Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# Design\n\n```conveyor:governs\n- repo: conveyor\n  paths: [web/**]\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: "noted-task",
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.DismissSystemDesignVersion(noted, "design-note", 1); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
		ID: "session-operator", OwnerUserID: "local-operator", Kind: core.CredentialUser,
		Scope: core.CredentialScopeOperator, Method: core.CredentialMethodSession,
	}}
	handler := server.Handler()
	call := func(id, workspace string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+id+"/activity?workspace_id="+workspace, nil)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := call("noted-task", "demo", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var item reviewItem
	if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if len(item.OperatorNotes) != 2 {
		t.Fatalf("notes=%+v", item.OperatorNotes)
	}
	for _, note := range item.OperatorNotes {
		if note.Note != "demo reason" || note.Version != 1 || note.DismissedAt.IsZero() {
			t.Fatalf("note=%+v", note)
		}
	}
	if item.OperatorNotes[0].DocumentID != "req-note" || item.OperatorNotes[0].Tier != "requirement" || item.OperatorNotes[1].DocumentID != "design-note" || item.OperatorNotes[1].Tier != "system_design" {
		t.Fatalf("notes=%+v", item.OperatorNotes)
	}
	for _, tc := range []struct {
		id, workspace string
		auth          bool
		status        int
	}{
		{"noted-task", "demo", false, http.StatusUnauthorized},
		{"noted-task", "other", true, http.StatusNotFound},
		{"bare-task", "demo", true, http.StatusOK},
	} {
		w := call(tc.id, tc.workspace, tc.auth)
		if w.Code != tc.status {
			t.Fatalf("%+v: status=%d body=%s", tc, w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if _, exists := body["operator_notes"]; exists {
			t.Fatalf("unexpected notes: %s", w.Body.String())
		}
	}
	if _, err := storetest.For(st).CancelTask(ctx, core.Intervention{TaskID: "noted-task", Action: core.InterventionCancel, ReasonCode: "terminal-notes-test"}); err != nil {
		t.Fatal(err)
	}
	response = call("noted-task", "demo", true)
	var terminal map[string]json.RawMessage
	if response.Code != http.StatusOK {
		t.Fatalf("terminal status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &terminal); err != nil {
		t.Fatal(err)
	}
	if _, exists := terminal["operator_notes"]; exists {
		t.Fatalf("terminal notes: %s", response.Body.String())
	}
}
