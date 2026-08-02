package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
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
		!views[0].ConfirmationEligible || views[0].ShippedPastIntent != "" || len(views[0].Artifacts) != 1 ||
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

func TestRequirementStalenessFollowsLineageToChildMerge(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-linked-stale", Title: "Linked intent"}, core.RequirementVersion{
		Content: "Delivery follows confirmed intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery remains traceable."}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-linked-stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	blueprint := core.Task{ID: "blueprint-linked-stale", Workspace: "demo", Title: "Linked blueprint", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": requirement.ID})}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})}); err != nil {
		t.Fatal(err)
	}
	child := core.Task{ID: "child-linked-stale", Workspace: "demo", Title: "Linked child", Repo: "conveyor", ParentTaskID: blueprint.ID, OriginSpecVersion: 1, State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	mergeAt := time.Now().UTC().Add(time.Minute)
	if err = st.AppendEvent(ctx, core.Event{TaskID: child.ID, Kind: "merge.confirmed", At: mergeAt, Payload: core.JSONPayload(map[string]any{
		"repository": "kidus/conveyor", "base_sha": "base", "head_sha": "head", "task_title": child.Title,
	})}); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{ID: "direct_push:conveyor:linked", WorkspaceID: "demo", Repository: "conveyor", Kind: monitor.DirectPush,
		SourceURL: "https://example.test/commit/head", CommitSHA: "head", RequirementID: requirement.ID, TaskID: "monitor-linked-stale", DetectedAt: mergeAt.Add(time.Minute)}
	if _, fresh, recordErr := st.(monitor.Store).RecordDrift(ctx, drift); recordErr != nil || !fresh {
		t.Fatalf("record drift fresh=%t err=%v", fresh, recordErr)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	server.Monitor = &monitor.Service{Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Staleness.Stale || view.Staleness.LatestDelivery != child.Title || !view.Staleness.LatestDeliveryAt.Equal(mergeAt) || view.ShippedPastIntent != child.Title ||
		len(view.Staleness.ActiveDrift) != 1 || view.Staleness.ActiveDrift[0].ID != drift.ID {
		t.Fatalf("link-aware staleness=%+v shipped=%q", view.Staleness, view.ShippedPastIntent)
	}
	foundMaterialization := false
	for _, link := range view.LineageGraph.Links {
		foundMaterialization = foundMaterialization || link.Kind == "materializes"
	}
	if !foundMaterialization {
		t.Fatalf("requirement graph does not reach child: %+v", view.LineageGraph)
	}
}

func TestRequirementConfirmationRejectsStaleExpectedAndSupersededVersions(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-confirm-race", Title: "Confirm race"}, core.RequirementVersion{
		Content: "First intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "First intent is explicit."}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "Second intent.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Second intent is explicit."}},
		Origin:     core.RequirementOriginChat, OriginSessionID: "session-second",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()
	confirm := func(version int, expected string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/requirements/%s/versions/%d/confirm", requirement.ID, version), nil)
		request.Header.Set("Authorization", "Bearer token")
		if expected != "" {
			request.Header.Set("If-Match", expected)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := confirm(1, `"1"`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "requirement_current_version_mismatch") {
		t.Fatalf("stale initial confirm status=%d body=%s", response.Code, response.Body.String())
	}
	if response := confirm(1, `"0"`); response.Code != http.StatusOK {
		t.Fatalf("confirm v1 status=%d body=%s", response.Code, response.Body.String())
	}
	if response := confirm(2, `W/"0"`); response.Code != http.StatusConflict {
		t.Fatalf("stale v2 status=%d body=%s", response.Code, response.Body.String())
	}
	if response := confirm(2, `"1"`); response.Code != http.StatusOK {
		t.Fatalf("confirm v2 status=%d body=%s", response.Code, response.Body.String())
	}
	if response := confirm(1, ""); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "requirement_version_superseded") || !strings.Contains(response.Body.String(), `"current_version":2`) {
		t.Fatalf("superseded v1 status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRequirementsHTTPDistinguishesMigratedSeedFromStaleConfirmableRevision(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	migrationCtx := store.WithActor(ctx, store.Actor{ID: "migration-050", Role: core.ActorSystem})
	seed, _, err := st.CreateRequirement(migrationCtx, core.Requirement{
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
	if !view.MigratedSeed || view.ConfirmationEligible || view.ShippedPastIntent != "" ||
		len(view.PendingVersions) != 1 || len(view.Lineage) != 2 {
		t.Fatalf("migrated seed view=%+v", view)
	}
	var backfilled bool
	for _, event := range view.Lineage {
		var payload map[string]any
		_ = json.Unmarshal(event.Payload, &payload)
		backfilled = backfilled || payload["backfilled"] == true
	}
	if !backfilled {
		t.Fatalf("migrated seed lineage lacks backfilled annotation: %+v", view.Lineage)
	}
	if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: seed.ID,
		Content:       "Deliberately revised intent.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The migrated behavior is now explicit.\n```",
		Statements:    []core.RequirementStatement{{ID: "REQ-1", Statement: "The migrated behavior is now explicit."}},
		Origin:        core.RequirementOriginChat, OriginSessionID: "session-deliberate-revision",
	}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+seed.ID, nil))
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.MigratedSeed || !view.ConfirmationEligible || len(view.PendingVersions) != 2 {
		t.Fatalf("revised seed view=%+v", view)
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
	if _, err = st.ConfirmRequirementServes(ctx, task.ID, requirement.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{
		TaskID: task.ID, Kind: "merge.confirmed", At: time.Now().UTC().Add(time.Minute),
		Payload: core.JSONPayload(map[string]any{"title": "Blueprint delivery"}),
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
		view.ServingBlueprints[0].Spec.Approved ||
		view.ShippedPastIntent != "Blueprint delivery" {
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
