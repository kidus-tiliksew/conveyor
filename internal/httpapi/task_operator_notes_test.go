package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// A dismissal note stays on the dismissed version's history and never reaches
// task activity, for a live or terminal task (req-260810-23b69f REQ-5, AC-5.1).
func TestTaskActivityCarriesNoDismissalNotes(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
	if strings.Contains(response.Body.String(), "operator_notes") || strings.Contains(response.Body.String(), "demo reason") {
		t.Fatalf("task activity carries a dismissal note: %s", response.Body.String())
	}
	demo := store.WithWorkspace(ctx, "demo")
	requirement, err := st.GetRequirementVersion(demo, "req-note", 1)
	if err != nil || requirement.DismissalNote != "demo reason" {
		t.Fatalf("requirement history=%+v err=%v", requirement, err)
	}
	design, err := st.GetSystemDesignVersion(demo, "design-note", 1)
	if err != nil || design.DismissalNote != "demo reason" {
		t.Fatalf("design history=%+v err=%v", design, err)
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
