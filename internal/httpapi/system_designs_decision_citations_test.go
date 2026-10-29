package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// citationReadRecorder counts the decision citation read and records every
// per-document or decision read the handler might make instead, so the test
// proves one store call per request (component-document-corpus, Decision
// citations).
type citationReadRecorder struct {
	store.Store
	citationReads int
	otherReads    []string
	err           error
}

func (r *citationReadRecorder) ListDecisionCitations(ctx context.Context) ([]core.DecisionCitation, error) {
	r.citationReads++
	if r.err != nil {
		return nil, r.err
	}
	return r.Store.ListDecisionCitations(ctx)
}

func (r *citationReadRecorder) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	r.otherReads = append(r.otherReads, "ListDecisions")
	return r.Store.ListDecisions(ctx)
}

func (r *citationReadRecorder) GetDecision(ctx context.Context, id string) (core.Decision, error) {
	r.otherReads = append(r.otherReads, "GetDecision")
	return r.Store.GetDecision(ctx, id)
}

func (r *citationReadRecorder) ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error) {
	r.otherReads = append(r.otherReads, "ListRequirements")
	return r.Store.ListRequirements(ctx, includeArchived)
}

func (r *citationReadRecorder) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	r.otherReads = append(r.otherReads, "GetRequirement")
	return r.Store.GetRequirement(ctx, id)
}

func (r *citationReadRecorder) GetRequirementVersion(ctx context.Context, id string, version int) (core.RequirementVersion, error) {
	r.otherReads = append(r.otherReads, "GetRequirementVersion")
	return r.Store.GetRequirementVersion(ctx, id, version)
}

func (r *citationReadRecorder) ListRequirementVersions(ctx context.Context, id string) ([]core.RequirementVersion, error) {
	r.otherReads = append(r.otherReads, "ListRequirementVersions")
	return r.Store.ListRequirementVersions(ctx, id)
}

func (r *citationReadRecorder) ListRequirementVersionsByRequirement(ctx context.Context) (map[string][]core.RequirementVersion, error) {
	r.otherReads = append(r.otherReads, "ListRequirementVersionsByRequirement")
	return r.Store.ListRequirementVersionsByRequirement(ctx)
}

func (r *citationReadRecorder) ListSystemDesigns(ctx context.Context, includeArchived bool) ([]core.SystemDesign, error) {
	r.otherReads = append(r.otherReads, "ListSystemDesigns")
	return r.Store.ListSystemDesigns(ctx, includeArchived)
}

func (r *citationReadRecorder) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	r.otherReads = append(r.otherReads, "GetSystemDesign")
	return r.Store.GetSystemDesign(ctx, id)
}

func (r *citationReadRecorder) GetSystemDesignVersion(ctx context.Context, id string, version int) (core.SystemDesignVersion, error) {
	r.otherReads = append(r.otherReads, "GetSystemDesignVersion")
	return r.Store.GetSystemDesignVersion(ctx, id, version)
}

func (r *citationReadRecorder) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	r.otherReads = append(r.otherReads, "ListSystemDesignVersions")
	return r.Store.ListSystemDesignVersions(ctx, id)
}

func (r *citationReadRecorder) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	r.otherReads = append(r.otherReads, "ListSystemDesignVersionsByDocument")
	return r.Store.ListSystemDesignVersionsByDocument(ctx)
}

// seedDecisionCitations writes two confirmed decisions, two requirements, and
// one System Design into workspace. The requirement titled Alpha and the
// System Design titled Alpha share a title, so the order proves that tier
// sorts before title.
func seedDecisionCitations(t *testing.T, st store.Store, workspace string) {
	t.Helper()
	ctx := insightWorkspaceContext(t.Context(), workspace)
	for range 2 {
		decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Citation fixture.", Context: "Fixture context.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ConfirmDecision(ctx, decision.ID); err != nil {
			t.Fatal(err)
		}
	}
	requirement := func(id, title, body string) {
		t.Helper()
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title + "\n\n" + body, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}, Origin: core.RequirementOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	requirement("req-beta", "Beta", "Follows DEC-2 and DEC-1.")
	requirement("req-alpha", "Alpha", "Follows DEC-1; DEC-10 is not a decision here.")
	_, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-alpha", Title: "Alpha", Category: "Component design"}, core.SystemDesignVersion{Content: insightDesignContent("# Alpha\n\nFollows DEC-1."), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, "design-alpha", version.Version); err != nil {
		t.Fatal(err)
	}
}

// TestDecisionCitationsHTTP covers GET /v1/decision-citations: the response
// shape and order, the empty list, workspace scoping, the view_workspace
// capability, one store read per request, and a store failure
// (component-document-corpus, Decision citations;
// req-document-operating-surfaces AC-7.1, AC-8.5).
func TestDecisionCitationsHTTP(t *testing.T) {
	inner := store.NewMemory()
	seedDecisionCitations(t, inner, "demo")
	otherCtx := insightWorkspaceContext(t.Context(), "other")
	decision, err := inner.ProposeDecision(otherCtx, core.Decision{Statement: "Other fixture.", Context: "Other context.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	_, version, err := inner.CreateRequirement(otherCtx, core.Requirement{ID: "req-other", Title: "Other"}, core.RequirementVersion{Content: "# Other\n\nFollows " + decision.ID + ".", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = inner.ConfirmRequirementVersion(otherCtx, "req-other", version.Version); err != nil {
		t.Fatal(err)
	}

	recorder := &citationReadRecorder{Store: inner}
	server := NewServer(recorder)
	members := &membershipFixture{
		workspaces: []core.Workspace{{ID: "demo"}, {ID: "other"}, {ID: "empty"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"viewer":   {"demo": core.WorkspaceRoleViewer, "other": core.WorkspaceRoleViewer, "empty": core.WorkspaceRoleViewer},
			"outsider": {"other": core.WorkspaceRoleViewer},
		},
	}
	server.Workspaces, server.Memberships = members, members
	server.Credentials = staticCredentialVerifier{
		"viewer":   {ID: "pat_viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"outsider": {ID: "pat_outsider", OwnerUserID: "outsider", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	call := func(token, workspace string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/decision-citations?workspace_id="+workspace, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	decode := func(response *httptest.ResponseRecorder) []map[string]any {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %s: %v", response.Body.String(), err)
		}
		if len(body) != 1 || body["items"] == nil {
			t.Fatalf("body keys = %s, want only items", response.Body.String())
		}
		var items []map[string]any
		if err := json.Unmarshal(body["items"], &items); err != nil {
			t.Fatal(err)
		}
		return items
	}

	members.capabilityCalls = nil
	response := call("viewer", "demo")
	if response.Code != http.StatusOK {
		t.Fatalf("viewer status=%d body=%s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(members.capabilityCalls, []core.Capability{core.CapabilityViewWorkspace}) {
		t.Fatalf("capability calls = %v, want only view_workspace", members.capabilityCalls)
	}
	item := func(decisionID, tier, documentID, title string) map[string]any {
		return map[string]any{"decision_id": decisionID, "tier": tier, "document_id": documentID, "title": title, "version": float64(1)}
	}
	want := []map[string]any{
		item("DEC-1", "requirement", "req-alpha", "Alpha"),
		item("DEC-1", "requirement", "req-beta", "Beta"),
		item("DEC-1", "system_design", "design-alpha", "Alpha"),
		item("DEC-2", "requirement", "req-beta", "Beta"),
	}
	if got := decode(response); !reflect.DeepEqual(got, want) {
		t.Fatalf("demo items = %v\nwant %v", got, want)
	}
	if recorder.citationReads != 1 || len(recorder.otherReads) != 0 {
		t.Fatalf("store reads: citations=%d others=%v, want one citation read and no other", recorder.citationReads, recorder.otherReads)
	}

	// Workspace scoping: the other workspace sees only its own citation, and
	// a workspace with no document answers an empty list, never null.
	if got := decode(call("viewer", "other")); !reflect.DeepEqual(got, []map[string]any{item(decision.ID, "requirement", "req-other", "Other")}) {
		t.Fatalf("other items = %v", got)
	}
	empty := call("viewer", "empty")
	if empty.Code != http.StatusOK || empty.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty status=%d body=%q", empty.Code, empty.Body.String())
	}

	// Without view_workspace in the workspace, or without a credential, the
	// read is refused before any store read.
	recorder.citationReads = 0
	if refused := call("outsider", "demo"); refused.Code != http.StatusNotFound || refused.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("outsider status=%d body=%s", refused.Code, refused.Body.String())
	}
	if refused := call("", "demo"); refused.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", refused.Code, refused.Body.String())
	}
	if recorder.citationReads != 0 {
		t.Fatalf("refused requests read citations %d times", recorder.citationReads)
	}

	recorder.err = errors.New("database unavailable")
	if failed := call("viewer", "demo"); failed.Code != http.StatusInternalServerError || failed.Body.String() != "internal server error\n" {
		t.Fatalf("store failure status=%d body=%q", failed.Code, failed.Body.String())
	}
}
