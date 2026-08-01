package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestRequirementsHTTPReplacesFeatureTreeAndConfirmsVersions(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-requirement", Title: "Define retry intent",
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-retries", Title: "Retry behavior",
	}, core.RequirementVersion{
		Content: "Retries stay bounded.",
		Statements: []core.RequirementStatement{{
			ID: "REQ-1", Statement: "A retry policy has a finite attempt limit.",
		}},
		Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "context.txt", ContentType: "text/plain",
		Role: core.ArtifactRoleTaskContext, RequirementID: requirement.ID,
	}, []byte("operator context"))
	if err != nil {
		t.Fatal(err)
	}
	// A sibling workspace may use the same requirement identity; its audit
	// events must not leak through the in-memory global event stream.
	sibling := store.WithWorkspace(t.Context(), "sibling")
	if _, err = st.CreatePlanningSession(sibling, core.PlanningSession{
		ID: "session-requirement", Title: "Sibling intent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateRequirement(sibling, core.Requirement{
		ID: requirement.ID, Title: "Sibling retries",
	}, core.RequirementVersion{
		Content: "Sibling prose.",
		Statements: []core.RequirementStatement{{
			ID: "REQ-1", Statement: "Sibling statement.",
		}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-requirement",
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var views []requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Requirement.ID != requirement.ID ||
		views[0].CurrentVersion != nil || len(views[0].PendingVersions) != 1 ||
		!views[0].Stale || len(views[0].Artifacts) != 1 ||
		views[0].Artifacts[0].ID != artifact.ID ||
		len(views[0].PlanningSessions) != 1 || len(views[0].Lineage) != 2 {
		t.Fatalf("requirement view=%+v", views)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost,
		"/v1/requirements/"+requirement.ID+"/versions/1/confirm", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized confirmation status=%d", unauthorized.Code)
	}

	confirm := httptest.NewRequest(http.MethodPost,
		"/v1/requirements/"+requirement.ID+"/versions/1/confirm", nil)
	confirm.Header.Set("Authorization", "Bearer token")
	confirm.Header.Set("X-Conveyor-Actor", "requirements-reviewer")
	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, confirm)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	current, getErr := st.GetRequirement(ctx, requirement.ID)
	if getErr != nil || current.CurrentVersion != proposed.Version {
		t.Fatalf("confirmed requirement=%+v err=%v", current, getErr)
	}

	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet,
		"/v1/requirements/"+requirement.ID, nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"current_version"`) ||
		strings.Contains(detail.Body.String(), `"pending_versions":[{`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func TestRequirementsHTTPDistinguishesMigratedSeedFromStaleConfirmableRevision(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	seed, _, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-migrated", Title: "Migrated feature",
	}, core.RequirementVersion{
		Content: "Legacy feature prose.", Statements: []core.RequirementStatement{},
		Origin: core.RequirementOriginFeatureMigration,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/v1/requirements/"+seed.ID, nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.MigratedSeed || view.ConfirmationEligible || view.Stale ||
		len(view.PendingVersions) != 1 || len(view.Lineage) != 2 {
		t.Fatalf("migrated seed view=%+v", view)
	}
}

func TestRequirementsHTTPSurfacesBlueprintSpecGateHandoffAndRemovesFeatureMutations(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirementSession, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-requirement", Title: "Capture intent",
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-planning", Title: "Planning",
	}, core.RequirementVersion{
		Content: "Plan in Conveyor.",
		Statements: []core.RequirementStatement{{
			ID: "REQ-1", Statement: "Blueprints enter the ordinary specification gate.",
		}},
		Origin: core.RequirementOriginChat, OriginSessionID: requirementSession.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1); err != nil {
		t.Fatal(err)
	}
	blueprintSession, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-blueprint", Title: "Plan work", RequirementContextID: requirement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := core.Task{
		ID: "blueprint-task", Workspace: "demo", Title: "Blueprint delivery",
		State: core.TaskAwaiting, NextStage: core.StageSpec, SpecApproval: true,
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: task.ID, Content: "# Blueprint\n\n## Acceptance Criteria\n\n- AC-1: ship it.",
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "planning.json", ContentType: "application/json",
		Role: core.ArtifactRoleGeneratedAudit, TaskID: task.ID,
	}, []byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
		SessionID: blueprintSession.ID, TaskID: task.ID,
		TranscriptArtifactID: transcript.ID,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/v1/requirements/"+requirement.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.ServingBlueprints) != 1 ||
		view.ServingBlueprints[0].Task.ID != task.ID ||
		view.ServingBlueprints[0].Spec == nil ||
		view.ServingBlueprints[0].Spec.Version != spec.Version ||
		view.ServingBlueprints[0].Spec.Approved {
		t.Fatalf("blueprint handoff=%+v", view.ServingBlueprints)
	}

	for _, legacy := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/features", `{"name":"retired"}`},
		{http.MethodPut, "/v1/tasks/" + task.ID + "/feature", `{"feature_id":"retired"}`},
	} {
		request := httptest.NewRequest(legacy.method, legacy.path, strings.NewReader(legacy.body))
		request.Header.Set("Authorization", "Bearer token")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != http.StatusNotFound && result.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status=%d body=%s", legacy.method, legacy.path, result.Code, result.Body.String())
		}
	}
}
