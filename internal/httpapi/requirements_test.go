package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestOperatorRequirementProposalRESTLifecycle(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	_, source, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: "ref-api", Name: "API.md"},
		core.ReferenceDocumentVersion{Filename: "API.md", ContentType: "text/markdown", Content: "# API contract\n\nPinned source."})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("X-Conveyor-Actor", "headless-planner")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	content := "Operator-authored intent.\n\n```conveyor:requirements\n- id: REQ-2\n  statement: The API remains authenticated.\n  user_story:\n    as_a: operator\n    i_want: to propose intent headlessly\n    so_that: confirmation stays explicit\n  acceptance_criteria:\n    - id: AC-2.1\n      statement: The proposal remains pending.\n```"
	body, _ := json.Marshal(map[string]any{
		"id": "req-api", "title": "Requirement proposal API", "content": content,
		"derived_from": map[string]any{"document_id": "ref-api", "version": source.Version, "section_anchor": "api-contract", "target_id": "AC-2.1"},
	})
	created := call(http.MethodPost, "/v1/requirements", string(body))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result struct {
		Requirement core.Requirement        `json:"requirement"`
		Version     core.RequirementVersion `json:"version"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Requirement.CurrentVersion != 0 || result.Version.Version != 1 || result.Version.Confirmed ||
		result.Version.Origin != core.RequirementOriginOperator || result.Version.OriginSessionID != "" ||
		result.Version.OriginDriftID != "" || result.Version.DerivedFrom == nil ||
		result.Version.DerivedFrom.SectionAnchor != "#api-contract" || len(result.Version.Statements) != 1 ||
		result.Version.Statements[0].UserStory == nil || len(result.Version.Statements[0].AcceptanceCriteria) != 1 {
		t.Fatalf("created proposal=%+v", result)
	}
	assertNoLineage := func(want int) {
		links, listErr := st.ListLineageLinks(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		got := 0
		for _, link := range links {
			if link.Kind == "derived_from" && link.SrcID == core.RequirementVersionLineageID("req-api", 1) {
				got++
			}
		}
		if got != want {
			t.Fatalf("derived_from links=%d want=%d", got, want)
		}
	}
	assertNoLineage(0)
	list := call(http.MethodGet, "/v1/requirements", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"pending_version_count":1`) {
		t.Fatalf("pending list status=%d body=%s", list.Code, list.Body.String())
	}
	confirmed := call(http.MethodPost, "/v1/requirements/req-api/versions/1/confirm", "")
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	assertNoLineage(1)

	revisionJSON, _ := json.Marshal(map[string]string{"content": "Revised intent.\n\n```conveyor:requirements\n- id: REQ-3\n  statement: Revisions preserve high-water discipline.\n```"})
	revision := string(revisionJSON)
	revised := call(http.MethodPost, "/v1/requirements/req-api/versions", revision)
	if revised.Code != http.StatusCreated || !strings.Contains(revised.Body.String(), `"version":2`) || !strings.Contains(revised.Body.String(), `"origin":"operator"`) {
		t.Fatalf("revision status=%d body=%s", revised.Code, revised.Body.String())
	}
	recycledJSON, _ := json.Marshal(map[string]string{"content": "Bad reuse.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Recycled identity.\n```"})
	recycled := call(http.MethodPost, "/v1/requirements/req-api/versions", string(recycledJSON))
	if recycled.Code != http.StatusBadRequest || !strings.Contains(recycled.Body.String(), "reuses a retired identifier") {
		t.Fatalf("recycled status=%d body=%s", recycled.Code, recycled.Body.String())
	}
	invalidFence := call(http.MethodPost, "/v1/requirements/req-api/versions", `{"content":"Missing machine block."}`)
	if invalidFence.Code != http.StatusBadRequest || !strings.Contains(invalidFence.Body.String(), "requires one conveyor:requirements block") {
		t.Fatalf("invalid fence status=%d body=%s", invalidFence.Code, invalidFence.Body.String())
	}
	multipleJSON, _ := json.Marshal(map[string]string{"content": content + "\n\n" + content})
	multipleFences := call(http.MethodPost, "/v1/requirements/req-api/versions", string(multipleJSON))
	if multipleFences.Code != http.StatusBadRequest || !strings.Contains(multipleFences.Body.String(), "exactly one conveyor:requirements block; found 2") {
		t.Fatalf("multiple fences status=%d body=%s", multipleFences.Code, multipleFences.Body.String())
	}
	spoofJSON, _ := json.Marshal(map[string]string{"content": "Spoof.\n\n```conveyor:requirements\n- id: REQ-3\n  statement: No spoofing.\n```", "origin": "chat", "origin_session_id": "forged"})
	spoof := call(http.MethodPost, "/v1/requirements/req-api/versions", string(spoofJSON))
	if spoof.Code != http.StatusBadRequest || !strings.Contains(spoof.Body.String(), "only operator origin") {
		t.Fatalf("spoof status=%d body=%s", spoof.Code, spoof.Body.String())
	}
	taskSpoofJSON, _ := json.Marshal(map[string]string{"content": "Task spoof.\n\n```conveyor:requirements\n- id: REQ-3\n  statement: No task spoofing.\n```", "origin": "operator", "origin_task_id": "forged-task"})
	taskSpoof := call(http.MethodPost, "/v1/requirements/req-api/versions", string(taskSpoofJSON))
	if taskSpoof.Code != http.StatusBadRequest || !strings.Contains(taskSpoof.Body.String(), "session, task, or drift") {
		t.Fatalf("task spoof status=%d body=%s", taskSpoof.Code, taskSpoof.Body.String())
	}
	badDerivation, _ := json.Marshal(map[string]any{
		"id": "req-bad-derivation", "title": "Bad derivation", "content": content,
		"derived_from": map[string]any{"document_id": "ref-api", "version": source.Version, "section_anchor": "api-contract", "target_id": "AC-9.9"},
	})
	invalidDerivation := call(http.MethodPost, "/v1/requirements", string(badDerivation))
	if invalidDerivation.Code != http.StatusBadRequest || !strings.Contains(invalidDerivation.Body.String(), "is not present in the proposed requirement version") {
		t.Fatalf("invalid derivation status=%d body=%s", invalidDerivation.Code, invalidDerivation.Body.String())
	}
	if _, getErr := st.GetRequirement(ctx, "req-bad-derivation"); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("invalid derivation persisted document: %v", getErr)
	}
	missing := call(http.MethodPost, "/v1/requirements/missing/versions", revision)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestRequirementReadsExposeImplementationOriginTask(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-task-origin", Title: "Task origin"}, core.RequirementVersion{
		Content:    "# Task origin\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Reads expose provenance.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Reads expose provenance."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version); err != nil {
		t.Fatal(err)
	}
	proposal, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "# Task origin\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Reads expose task provenance.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Reads expose task provenance."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: "task-proposer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposal.Version, initial.Version); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	for _, path := range []string{"/v1/requirements", "/v1/requirements/" + requirement.ID, "/v1/requirements/" + requirement.ID + "/versions"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"origin":"implementation"`) || !strings.Contains(response.Body.String(), `"origin_task_id":"task-proposer"`) {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestRequirementConfirmationDistinguishesSupersededFromIfMatchConflict(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-confirm-errors", Title: "Confirmation errors"}, core.RequirementVersion{
		Content: "First.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Errors are honest."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"Second.", "Third."} {
		if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: content, Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Errors remain honest."}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 3); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	call := func(version int, expected string) (int, map[string]any) {
		request := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/v1/requirements/%s/versions/%d/confirm", requirement.ID, version), nil)
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("X-Conveyor-Actor", "operator")
		request.Header.Set("If-Match", expected)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		var body map[string]any
		if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response %q: %v", response.Body.String(), err)
		}
		return response.Code, body
	}
	status, superseded := call(2, `"3"`)
	if status != http.StatusConflict || superseded["error"] != "requirement_version_superseded" || superseded["superseded_by_version"] != float64(3) {
		t.Fatalf("superseded status=%d body=%+v", status, superseded)
	}
	status, mismatch := call(3, `"1"`)
	if status != http.StatusConflict || mismatch["error"] != "requirement_current_version_mismatch" {
		t.Fatalf("mismatch status=%d body=%+v", status, mismatch)
	}
}

func TestCheckpointContextCandidatesREST(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx,
		core.Requirement{ID: "req-confirmed", Title: "Confirmed intent"},
		core.RequirementVersion{
			Content:    "Confirmed intent.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Paused tasks receive confirmed context.\n```",
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Paused tasks receive confirmed context."}},
			Origin:     core.RequirementOriginOperator,
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := core.Task{ID: "checkpoint-task", Workspace: "demo", Title: "Checkpoint task", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "checkpoint-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued,
		LastAttemptOutcome: core.WorkOrderOutcomeReleased,
		LastFailureMessage: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		QueueEnteredAt:     now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	request := httptest.NewRequest(http.MethodGet, "/v1/requirements/req-confirmed/checkpoint-context-candidates", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"checkpoint-task"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDeliveryReachabilityDoesNotCrossPlanningSessionBridge(t *testing.T) {
	links := []core.LineageLink{
		{Workspace: "demo", SrcType: core.LineagePlanningSession, SrcID: "session", DstType: core.LineageRequirement, DstID: "req", Kind: "produced_requirement", CreatedByEventID: 1},
		{Workspace: "demo", SrcType: core.LineagePlanningSession, SrcID: "session", DstType: core.LineageBlueprint, DstID: "unrelated", Kind: "produced_blueprint", CreatedByEventID: 2},
	}
	if got := deliveryReachableTasks(links, "req"); len(got) != 0 {
		t.Fatalf("planning bridge reached unrelated delivery: %v", got)
	}
}

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

	guard := &requirementsScopedStore{Store: st}
	server := NewServer(guard)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var summaries []requirementSummary
	if err = json.Unmarshal(response.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Requirement.ID != requirement.ID ||
		summaries[0].CurrentVersion != nil || summaries[0].PendingVersionCount != 1 ||
		!summaries[0].ConfirmationEligible || summaries[0].Staleness.DeliveryAfterIntent {
		t.Fatalf("requirement summary=%+v", summaries)
	}
	for _, detailOnly := range []string{"pending_versions", "serving_blueprints", "planning_sessions", "artifacts", "lineage", "lineage_graph"} {
		if strings.Contains(response.Body.String(), `"`+detailOnly+`"`) {
			t.Fatalf("list includes detail-only field %q: %s", detailOnly, response.Body.String())
		}
	}
	if guard.fullLineage != 0 || guard.fullArtifacts != 0 || guard.neighborhood != 0 || guard.scopedArtifacts != 0 {
		t.Fatalf("requirements queries full_lineage=%d full_artifacts=%d neighborhood=%d scoped_artifacts=%d", guard.fullLineage, guard.fullArtifacts, guard.neighborhood, guard.scopedArtifacts)
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
	confirmedList := httptest.NewRecorder()
	handler.ServeHTTP(confirmedList, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)))
	if confirmedList.Code != http.StatusOK || !strings.Contains(confirmedList.Body.String(), `"current_version":{"requirement_id":"req-retries","version":1`) ||
		strings.Contains(confirmedList.Body.String(), `"content"`) || confirmedList.Body.Len() > 2048 {
		t.Fatalf("compact confirmed list status=%d bytes=%d body=%s", confirmedList.Code, confirmedList.Body.Len(), confirmedList.Body.String())
	}
	if guard.neighborhood != 0 || guard.scopedArtifacts != 0 {
		t.Fatalf("confirmed list queries neighborhood=%d scoped_artifacts=%d", guard.neighborhood, guard.scopedArtifacts)
	}
	if guard.batchVersions != 2 || guard.batchEvents != 2 || guard.batchDelivery != 2 || guard.batchDeliveryEvents != 2 || guard.taskLists != 2 || guard.singleVersions != 0 || guard.singleEvents != 0 || guard.singleDelivery != 0 {
		t.Fatalf("confirmed list requirement reads batch_versions=%d batch_events=%d batch_delivery=%d batch_delivery_events=%d task_lists=%d single_versions=%d single_events=%d single_delivery=%d",
			guard.batchVersions, guard.batchEvents, guard.batchDelivery, guard.batchDeliveryEvents, guard.taskLists, guard.singleVersions, guard.singleEvents, guard.singleDelivery)
	}

	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet,
		"/v1/requirements/"+requirement.ID, nil)))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"current_version"`) ||
		strings.Contains(detail.Body.String(), `"pending_versions":[{`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(detail.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Artifacts) != 1 || view.Artifacts[0].ID != artifact.ID ||
		len(view.PlanningSessions) != 1 || len(view.Lineage) < 2 || len(view.LineageGraph.Nodes) == 0 {
		t.Fatalf("detail requirement view=%+v", view)
	}
	if guard.neighborhood != 1 || guard.scopedArtifacts != 1 {
		t.Fatalf("detail queries neighborhood=%d scoped_artifacts=%d", guard.neighborhood, guard.scopedArtifacts)
	}
	if guard.singleVersions != 1 || guard.singleEvents != 1 || guard.singleDelivery != 1 {
		t.Fatalf("detail requirement reads versions=%d events=%d delivery=%d", guard.singleVersions, guard.singleEvents, guard.singleDelivery)
	}
}

type requirementsScopedStore struct {
	store.Store
	fullLineage, fullArtifacts, neighborhood, scopedArtifacts                 int
	batchVersions, batchEvents, batchDelivery, batchDeliveryEvents, taskLists int
	singleVersions, singleEvents, singleDelivery                              int
}

func (st *requirementsScopedStore) ListRequirementVersionsByRequirement(ctx context.Context) (map[string][]core.RequirementVersion, error) {
	st.batchVersions++
	return st.Store.ListRequirementVersionsByRequirement(ctx)
}

func (st *requirementsScopedStore) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	st.batchEvents++
	return st.Store.ListRequirementEventsByRequirement(ctx)
}

func (st *requirementsScopedStore) ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error) {
	st.singleVersions++
	return st.Store.ListRequirementVersions(ctx, requirementID)
}

func (st *requirementsScopedStore) ListRequirementEvents(ctx context.Context, requirementID string) ([]core.Event, error) {
	st.singleEvents++
	return st.Store.ListRequirementEvents(ctx, requirementID)
}

func (st *requirementsScopedStore) ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	st.batchDelivery++
	return st.Store.ListRequirementDeliveryLineageByRequirement(ctx, requirementIDs, budget)
}

func (st *requirementsScopedStore) ListRequirementDeliveryEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	st.batchDeliveryEvents++
	return st.Store.ListRequirementDeliveryEventsForTasks(ctx, taskIDs)
}

func (st *requirementsScopedStore) ListTasks(ctx context.Context) ([]core.Task, error) {
	st.taskLists++
	return st.Store.ListTasks(ctx)
}

func (st *requirementsScopedStore) ListRequirementDeliveryLineage(ctx context.Context, requirementID string, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	st.singleDelivery++
	return st.Store.ListRequirementDeliveryLineage(ctx, requirementID, budget)
}

func (st *requirementsScopedStore) ListLineageLinks(context.Context) ([]core.LineageLink, error) {
	st.fullLineage++
	return nil, fmt.Errorf("whole-workspace lineage scan forbidden")
}

func (st *requirementsScopedStore) ListArtifacts(context.Context) ([]core.Artifact, error) {
	st.fullArtifacts++
	return nil, fmt.Errorf("whole-workspace artifact scan forbidden")
}

func (st *requirementsScopedStore) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	st.neighborhood++
	return st.Store.ListLineageNeighborhood(ctx, roots, budget)
}

func (st *requirementsScopedStore) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	st.scopedArtifacts++
	return st.Store.ListArtifactsForLineage(ctx, nodes)
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
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 1, Nodes: 4, RenderableBytes: 1024, ArtifactRefs: 2}}}}}, nil
	}
	server.Monitor = &monitor.Service{Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 ||
		view.Staleness.Deliveries[0].TaskID != child.ID || view.Staleness.Deliveries[0].Label != child.Title ||
		!view.Staleness.Deliveries[0].At.Equal(mergeAt) || !view.Staleness.Deliveries[0].NeedsAttention ||
		!slices.Contains(view.Staleness.Deliveries[0].Reasons, "delivered through related work without serving this requirement") ||
		len(view.Staleness.ActiveDrift) != 1 || view.Staleness.ActiveDrift[0].ID != drift.ID {
		t.Fatalf("link-aware staleness=%+v", view.Staleness)
	}
	foundMaterialization := false
	for _, link := range view.LineageGraph.Links {
		foundMaterialization = foundMaterialization || link.Kind == "materializes"
	}
	if !foundMaterialization {
		t.Fatalf("requirement graph does not reach child: %+v", view.LineageGraph)
	}
	labels := map[string]string{}
	for _, node := range view.LineageGraph.Nodes {
		labels[string(node.Type)+":"+node.ID] = node.Label
	}
	if labels["requirement:"+requirement.ID] != requirement.Title || labels["blueprint:"+blueprint.ID] != blueprint.Title || labels["task:"+child.ID] != child.Title {
		t.Fatalf("requirement graph labels=%v", labels)
	}
}

func TestDesignDriftCrossPostsPreserveSubjectAndResolveEverywhere(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	for _, id := range []string{"req-deployment", "req-lifecycle"} {
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{
			Content: "Confirmed intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery stays aligned."}},
			Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	task := core.Task{ID: "shared-delivery", Workspace: "demo", Title: "Shared delivery", Repo: "conveyor", State: core.TaskMerged}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"req-deployment", "req-lifecycle"} {
		if _, err := st.ProposeRequirementServes(ctx, task.ID, id, core.RequirementServesOperator, false); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ConfirmRequirementServes(ctx, task.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	document, version, err := st.CreateSystemDesign(ctx,
		core.SystemDesign{ID: "design-system-architecture", Title: "System architecture", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# System architecture\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{
		ID: "shared-design-drift", WorkspaceID: "demo", Repository: "conveyor", Kind: monitor.LineagedMerge,
		SourceURL: "https://example.test/pr/488", TaskID: task.ID, SystemDesignID: document.ID,
		SystemDesignVersion: version.Version, MatchingPaths: []string{"internal/store/storetest/task_operations.go"}, DetectedAt: time.Now().UTC(),
	}
	if _, fresh, err := st.(monitor.Store).RecordDrift(ctx, drift); err != nil || !fresh {
		t.Fatalf("record drift fresh=%t err=%v", fresh, err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Monitor = &monitor.Service{Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	handler := server.Handler()
	list := func() []requirementView {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
		}
		var views []requirementView
		if err := json.Unmarshal(response.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		return views
	}
	views := list()
	if len(views) != 2 {
		t.Fatalf("views=%+v", views)
	}
	for _, view := range views {
		if len(view.Staleness.ActiveDrift) != 1 || view.Staleness.ActiveDrift[0].ID != drift.ID ||
			view.Staleness.ActiveDrift[0].SystemDesignID != document.ID ||
			view.Staleness.ActiveDrift[0].SystemDesignVersion != version.Version {
			t.Fatalf("cross-posted drift for %s=%+v", view.Requirement.ID, view.Staleness.ActiveDrift)
		}
	}
	resolve := httptest.NewRequest(http.MethodPost, "/v1/monitor/drift/"+drift.ID+"/resolve", strings.NewReader(`{"outcome":"conflict_resolved"}`))
	resolve.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, resolve)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve status=%d body=%s", response.Code, response.Body.String())
	}
	for _, view := range list() {
		if len(view.Staleness.ActiveDrift) != 0 {
			t.Fatalf("resolved drift remained on %s: %+v", view.Requirement.ID, view.Staleness.ActiveDrift)
		}
	}
}

// Staleness walks delivery edges at task level.
// Task-centric delivery attaches the requirement to the task itself (change 3),
// so a merge on that task is the requirement's delivery — with no blueprint
// anywhere in the walk, and none invented to stand in for one.
func TestRequirementStalenessFollowsTaskLevelServesChain(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-task-chain", Title: "Task-chain intent"}, core.RequirementVersion{
		Content:    "Delivery follows the task that serves the requirement.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Task-level service is delivery authority."}},
		Origin:     core.RequirementOriginChat, OriginSessionID: "session-task-chain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "task-chain-delivery", Workspace: "demo", Title: "Task-chain delivery", Repo: "conveyor", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{
		"id": requirement.ID, "version": proposed.Version,
	})}); err != nil {
		t.Fatal(err)
	}
	mergeAt := time.Now().UTC().Add(time.Minute)
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", At: mergeAt, Payload: core.JSONPayload(map[string]any{
		"repository": "kidus/conveyor", "base_sha": "base", "head_sha": "head", "task_title": task.Title,
	})}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 ||
		view.Staleness.Deliveries[0].TaskID != task.ID || view.Staleness.Deliveries[0].Label != task.Title ||
		!view.Staleness.Deliveries[0].At.Equal(mergeAt) || view.Staleness.Deliveries[0].NeedsAttention ||
		len(view.Staleness.Deliveries[0].Reasons) != 0 {
		t.Fatalf("task-chain staleness=%+v", view.Staleness)
	}
	if len(view.ServingBlueprints) != 0 {
		t.Fatalf("task-centric delivery invented a blueprint record: %+v", view.ServingBlueprints)
	}
	if len(view.ServingTasks) != 1 || view.ServingTasks[0].ID != task.ID {
		t.Fatalf("task-centric serving tasks = %+v", view.ServingTasks)
	}
}

func TestRequirementDeliveryClassificationNamesSuspectConditions(t *testing.T) {
	confirmedV1 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	confirmedV2 := confirmedV1.Add(time.Hour)
	mergeAt := confirmedV2.Add(time.Hour)
	versions := []core.RequirementVersion{
		{RequirementID: "req-versioned", Version: 1, Confirmed: true, ConfirmedAt: confirmedV1},
		{RequirementID: "req-versioned", Version: 2, Confirmed: true, ConfirmedAt: confirmedV2},
	}
	contextEvent := core.Event{TaskID: "delivery", Kind: store.TaskContextRequirementAdded, At: confirmedV1.Add(time.Minute), Payload: core.JSONPayload(map[string]any{
		"id": "req-versioned", "version": 1,
	})}

	t.Run("version skew", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			TaskID: "delivery", Kind: "merge.confirmed", At: mergeAt, Payload: core.JSONPayload(map[string]any{"task_title": "Versioned delivery"}),
		}}, versions, "req-versioned", requirementDeliveryWatermark{At: confirmedV2}, true)
		if len(deliveries) != 1 || !deliveries[0].NeedsAttention ||
			!slices.Contains(deliveries[0].Reasons, "planned against v1; v2 was current at merge") {
			t.Fatalf("version-skew delivery = %+v", deliveries)
		}
	})

	t.Run("explicit reviewed reconciliation", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{
				"head_sha": "reviewed-head", "approved_head_sha": "reviewed-head", "factory_review_validated": true,
			}),
		}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
		if len(deliveries) != 1 || deliveries[0].NeedsAttention || len(deliveries[0].Reasons) != 0 {
			t.Fatalf("reviewed reconciliation = %+v", deliveries)
		}
	})

	t.Run("explicit outside reconciliation", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{
				"head_sha": "outside-head", "approved_head_sha": "reviewed-head", "factory_review_validated": false,
			}),
		}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
		if len(deliveries) != 1 || !deliveries[0].NeedsAttention || !slices.Contains(deliveries[0].Reasons, "merged outside factory review") {
			t.Fatalf("outside reconciliation = %+v", deliveries)
		}
	})

	t.Run("legacy approved round matching reconciled head", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			ID: 10, TaskID: "delivery", Kind: "review.round_completed", At: mergeAt, Payload: core.JSONPayload(map[string]any{
				"verdict": "approve", "approved_head_sha": "reviewed-head",
			}),
		}, {
			ID: 20, TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{"head_sha": "reviewed-head"}),
		}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
		if len(deliveries) != 1 || deliveries[0].NeedsAttention || len(deliveries[0].Reasons) != 0 {
			t.Fatalf("legacy reviewed reconciliation = %+v", deliveries)
		}
	})

	t.Run("legacy evidence must precede and match merge", func(t *testing.T) {
		for _, review := range []core.Event{
			{ID: 10, TaskID: "delivery", Kind: "review.round_completed", At: mergeAt.Add(-time.Minute), Payload: core.JSONPayload(map[string]any{"verdict": "approve", "approved_head_sha": "other-head"})},
			{ID: 30, TaskID: "delivery", Kind: "review.round_completed", At: mergeAt, Payload: core.JSONPayload(map[string]any{"verdict": "approve", "approved_head_sha": "reviewed-head"})},
			{ID: 30, TaskID: "delivery", Kind: "review.round_completed", At: mergeAt.Add(time.Minute), Payload: core.JSONPayload(map[string]any{"verdict": "approve", "approved_head_sha": "reviewed-head"})},
		} {
			deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, review, {
				ID: 20, TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{"head_sha": "reviewed-head"}),
			}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
			if len(deliveries) != 1 || !deliveries[0].NeedsAttention || !slices.Contains(deliveries[0].Reasons, "merged outside factory review") {
				t.Fatalf("ambiguous reconciliation = %+v", deliveries)
			}
		}
	})

	t.Run("legacy per-seat approval remains ambiguous", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			ID: 10, TaskID: "delivery", Kind: "review.completed", At: mergeAt.Add(-time.Minute), Payload: core.JSONPayload(map[string]any{
				"verdict": "approve", "review_round": 0, "reviewed_commit_sha": "reviewed-head",
			}),
		}, {
			ID: 20, TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{"head_sha": "reviewed-head"}),
		}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
		if len(deliveries) != 1 || !deliveries[0].NeedsAttention ||
			!slices.Contains(deliveries[0].Reasons, "merged outside factory review") {
			t.Fatalf("ambiguous per-seat reconciliation = %+v", deliveries)
		}
	})

	t.Run("legacy reconciliation preserves other reasons", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			ID: 20, TaskID: "delivery", Kind: "merge.reconciled", At: mergeAt, Payload: core.JSONPayload(map[string]any{"head_sha": "unknown-head"}),
		}}, versions, "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, false)
		if len(deliveries) != 1 || !deliveries[0].NeedsAttention ||
			!slices.Contains(deliveries[0].Reasons, "merged outside factory review") ||
			!slices.Contains(deliveries[0].Reasons, "planned against v1; v2 was current at merge") ||
			!slices.Contains(deliveries[0].Reasons, "delivered through related work without serving this requirement") {
			t.Fatalf("reconciled delivery = %+v", deliveries)
		}
	})

	t.Run("routine", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{contextEvent, {
			TaskID: "delivery", Kind: "merge.confirmed", At: confirmedV1.Add(2 * time.Minute),
		}}, versions[:1], "req-versioned", requirementDeliveryWatermark{At: confirmedV1}, true)
		if len(deliveries) != 1 || deliveries[0].NeedsAttention || len(deliveries[0].Reasons) != 0 {
			t.Fatalf("routine delivery = %+v", deliveries)
		}
	})

	t.Run("missing direct context", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{{
			ID: 44, TaskID: "delivery", Kind: "merge.confirmed", At: mergeAt,
		}}, versions, "req-versioned", requirementDeliveryWatermark{At: confirmedV2}, true)
		if len(deliveries) != 1 || !deliveries[0].NeedsAttention ||
			!slices.Contains(deliveries[0].Reasons, "planned requirement version unavailable") {
			t.Fatalf("missing-context delivery = %+v", deliveries)
		}
	})

	t.Run("same timestamp context is event bounded", func(t *testing.T) {
		deliveries := classifyRequirementDeliveries("delivery", []core.Event{{
			ID: 101, TaskID: "delivery", Kind: "merge.confirmed", At: mergeAt,
		}, {
			ID: 102, TaskID: "delivery", Kind: store.TaskContextRequirementActive, At: mergeAt,
			Payload: core.JSONPayload(map[string]any{"id": "req-versioned", "version": 2}),
		}}, versions, "req-versioned", requirementDeliveryWatermark{At: confirmedV2}, true)
		if len(deliveries) != 1 || deliveries[0].PinnedVersion != 0 ||
			!slices.Contains(deliveries[0].Reasons, "planned requirement version unavailable") {
			t.Fatalf("event-bounded delivery = %+v", deliveries)
		}
	})
}

func TestRequirementDeliveryReevaluatesReportedReconciliationSignal(t *testing.T) {
	confirmedAt := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	mergeAt := time.Date(2026, 8, 19, 7, 26, 26, 739471000, time.UTC)
	versions := []core.RequirementVersion{{
		RequirementID: "req-260811-0ee057", Version: 17, Confirmed: true, ConfirmedAt: confirmedAt,
	}}
	events := []core.Event{{
		ID: 281500, TaskID: "260819-1630df", Kind: store.TaskContextRequirementAdded, At: mergeAt.Add(-time.Hour),
		Payload: core.JSONPayload(map[string]any{"id": "req-260811-0ee057", "version": 17}),
	}, {
		ID: 282079, TaskID: "260819-1630df", Kind: "review.round_completed", At: time.Date(2026, 8, 19, 7, 26, 9, 52663000, time.UTC),
		Payload: core.JSONPayload(map[string]any{
			"review_round": 1, "verdict": "approve", "approved_head_sha": "b309532aa8c2ec66ce0e4f42fdf7dc5bdd1298b7",
		}),
	}, {
		ID: 282087, TaskID: "260819-1630df", Kind: "merge.reconciled", At: mergeAt,
		Payload: core.JSONPayload(map[string]any{
			"url":      "https://github.com/kidus-tiliksew/conveyor/pull/642",
			"head_sha": "b309532aa8c2ec66ce0e4f42fdf7dc5bdd1298b7",
		}),
	}}

	deliveries := classifyRequirementDeliveries("260819-1630df", events, versions, "req-260811-0ee057", requirementDeliveryWatermark{At: confirmedAt}, true)
	if len(deliveries) != 1 || deliveries[0].DeliveryEventID != 282087 || deliveries[0].URL != "https://github.com/kidus-tiliksew/conveyor/pull/642" || deliveries[0].NeedsAttention || len(deliveries[0].Reasons) != 0 {
		t.Fatalf("reported reconciliation re-evaluation = %+v", deliveries)
	}
}

func TestRequirementDeliveryReproducesSuppliedVersionSignal(t *testing.T) {
	confirmedV2 := time.Date(2026, 8, 8, 16, 48, 10, 669012000, time.UTC)
	contextAt := time.Date(2026, 8, 13, 16, 25, 11, 181114000, time.UTC)
	confirmedV3 := time.Date(2026, 8, 13, 16, 36, 59, 381747000, time.UTC)
	mergeAt := time.Date(2026, 8, 13, 16, 53, 15, 905326000, time.UTC)
	versions := []core.RequirementVersion{
		{RequirementID: "req-security-boundaries", Version: 2, Confirmed: true, ConfirmedAt: confirmedV2},
		{RequirementID: "req-security-boundaries", Version: 3, Confirmed: true, ConfirmedAt: confirmedV3},
	}
	events := []core.Event{{
		ID: 265646, TaskID: "260813-21d15e", Kind: store.TaskContextRequirementAdded, At: contextAt,
		Payload: core.JSONPayload(map[string]any{"id": "req-security-boundaries"}),
	}, {
		ID: 266166, TaskID: "260813-21d15e", Kind: "merge.confirmed", At: mergeAt,
		Payload: core.JSONPayload(map[string]any{"url": "https://github.com/kidus-tiliksew/conveyor/pull/529"}),
	}}
	deliveries := classifyRequirementDeliveries("260813-21d15e", events, versions, "req-security-boundaries", requirementDeliveryWatermark{At: confirmedV2}, true)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	delivery := deliveries[0]
	if delivery.SignalID != "efae210dca1db05af670a28445b532694f59c00c086966e9df595e9a8053f2cc" ||
		delivery.PinnedVersion != 2 || delivery.CurrentVersion != 3 || delivery.DeliveryEventID != 266166 ||
		delivery.URL != "https://github.com/kidus-tiliksew/conveyor/pull/529" ||
		!slices.Equal(delivery.Reasons, []string{"planned against v2; v3 was current at merge"}) {
		t.Fatalf("supplied delivery = %+v", delivery)
	}
}

func TestRequirementStalenessReproducesExecutionConfigurationV3V4Signal(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-execution-configuration", Title: "Execution configuration: harnesses and setups",
	}, core.RequirementVersion{
		Content: "Initial execution configuration.",
		Statements: []core.RequirementStatement{{
			ID: "REQ-6", Statement: "Serviceability remains advisory.",
		}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	confirm := func(version core.RequirementVersion) core.RequirementVersion {
		t.Helper()
		_, confirmed, confirmErr := st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version)
		if confirmErr != nil {
			t.Fatal(confirmErr)
		}
		return confirmed
	}
	propose := func(content string, statements []core.RequirementStatement) core.RequirementVersion {
		t.Helper()
		version, proposeErr := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: content, Statements: statements, Origin: core.RequirementOriginOperator,
		})
		if proposeErr != nil {
			t.Fatal(proposeErr)
		}
		return version
	}
	statementsV3 := []core.RequirementStatement{{ID: "REQ-6", Statement: "Serviceability remains advisory and claimant-aware."}}
	confirm(proposed)
	confirm(propose("Execution configuration v2.", statementsV3))
	v3 := confirm(propose("Execution configuration v3.", statementsV3))

	delivery := core.Task{
		ID: "260815-4804be", Workspace: "demo", Title: "Add interactive local execution setup wizard",
		Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-260815-4804be", State: core.TaskMerged,
	}
	if err = st.CreateTask(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	// The historical event omitted an explicit version. Classification must
	// resolve that pin at the event instant rather than from today's task view.
	if err = st.AppendEvent(ctx, core.Event{
		TaskID: delivery.ID, Kind: store.TaskContextRequirementAdded, At: v3.ConfirmedAt,
		Payload: core.JSONPayload(map[string]any{"id": requirement.ID}),
	}); err != nil {
		t.Fatal(err)
	}
	statementsV4 := append(slices.Clone(statementsV3), core.RequirementStatement{
		ID: "REQ-7", Statement: "Workspace policy contains no execution detail.",
	})
	v4 := confirm(propose("Execution configuration v4 retires server execution detail.", statementsV4))
	mergeAt := v4.ConfirmedAt.Add(time.Second)
	if err = st.AppendEvent(ctx, core.Event{
		TaskID: delivery.ID, Kind: "merge.confirmed", At: mergeAt,
		Payload: core.JSONPayload(map[string]any{
			"url": "https://github.com/kidus-tiliksew/conveyor/pull/569", "pull_request": 569,
		}),
	}); err != nil {
		t.Fatal(err)
	}

	// A merge in the same workspace is not a delivery candidate without the
	// requirement's bounded serving/materialization lineage.
	unrelated := core.Task{ID: "unrelated-merge", Workspace: "demo", Title: "Unrelated merge", Repo: "conveyor", State: core.TaskMerged}
	if err = st.CreateTask(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: unrelated.ID, Kind: "merge.confirmed", At: mergeAt}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	getView := func() requirementView {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
		}
		var view requirementView
		if unmarshalErr := json.Unmarshal(response.Body.Bytes(), &view); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return view
	}

	view := getView()
	if !view.Staleness.DeliveryAfterIntent || view.Staleness.PartialEvaluation || len(view.Staleness.Deliveries) != 1 {
		t.Fatalf("reported delivery staleness=%+v", view.Staleness)
	}
	signal := view.Staleness.Deliveries[0]
	if signal.TaskID != delivery.ID || signal.EventKind != "merge.confirmed" || !signal.At.Equal(mergeAt) ||
		signal.PinnedVersion != 3 || signal.CurrentVersion != 4 || !signal.NeedsAttention ||
		!slices.Equal(signal.Reasons, []string{"planned against v3; v4 was current at merge"}) ||
		signal.URL != "https://github.com/kidus-tiliksew/conveyor/pull/569" || signal.SignalID == "" {
		t.Fatalf("reported v3/v4 signal=%+v", signal)
	}
	if repeated := getView().Staleness.Deliveries[0]; repeated.SignalID != signal.SignalID {
		t.Fatalf("signal identity changed: first=%s repeated=%s", signal.SignalID, repeated.SignalID)
	}
}

func TestRequirementStalenessAcknowledgmentAndFollowUpLifecycle(t *testing.T) {
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "alice", Role: core.ActorHuman})
	st := store.NewMemory()
	requirement, v1, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-actionable-staleness", Title: "Actionable staleness"}, core.RequirementVersion{
		Content: "First intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery stays aligned."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v1.Version); err != nil {
		t.Fatal(err)
	}
	deliveryTask := core.Task{ID: "stale-delivery", Workspace: "demo", Title: "Stale delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-stale-delivery", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, deliveryTask); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: deliveryTask.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": v1.Version})}); err != nil {
		t.Fatal(err)
	}
	v2, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "Second intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery stays aligned with revisions."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v2.Version); err != nil {
		t.Fatal(err)
	}
	firstMerge := time.Now().UTC().Add(time.Minute)
	if err = st.AppendEvent(ctx, core.Event{TaskID: deliveryTask.ID, Kind: "merge.confirmed", At: firstMerge, Payload: core.JSONPayload(map[string]any{
		"url": "https://example.test/pull/1", "task_title": deliveryTask.Title,
	})}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Investigate stale delivery", nil }
	handler := server.Handler()
	getView := func() requirementView {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("requirement status=%d body=%s", response.Code, response.Body.String())
		}
		var view requirementView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	mutate := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("X-Conveyor-Actor", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	view := getView()
	if !view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].SignalID == "" || view.Staleness.Deliveries[0].URL == "" {
		t.Fatalf("initial signal=%+v", view.Staleness)
	}
	firstSignal := view.Staleness.Deliveries[0]
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)))
	var summaries []requirementSummary
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &summaries) != nil ||
		len(summaries) != 1 || len(summaries[0].Staleness.Deliveries) != 1 ||
		summaries[0].Staleness.Deliveries[0].SignalID != firstSignal.SignalID {
		t.Fatalf("list/detail causal delivery mismatch: status=%d summaries=%+v", listResponse.Code, summaries)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/requirements/%s/staleness/%s/acknowledge", requirement.ID, firstSignal.SignalID), nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized acknowledgment status=%d", unauthorized.Code)
	}
	acknowledged := mutate(fmt.Sprintf("/v1/requirements/%s/staleness/%s/acknowledge", requirement.ID, firstSignal.SignalID))
	if acknowledged.Code != http.StatusCreated {
		t.Fatalf("acknowledge status=%d body=%s", acknowledged.Code, acknowledged.Body.String())
	}
	view = getView()
	if view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 0 {
		t.Fatalf("acknowledged signal remained raised: %+v", view.Staleness)
	}
	staleRetry := mutate(fmt.Sprintf("/v1/requirements/%s/staleness/%s/acknowledge", requirement.ID, firstSignal.SignalID))
	if staleRetry.Code != http.StatusConflict {
		t.Fatalf("stale acknowledgment retry status=%d body=%s", staleRetry.Code, staleRetry.Body.String())
	}
	ackEvents, err := st.ListRequirementEvents(ctx, requirement.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundAck := false
	for _, event := range ackEvents {
		foundAck = foundAck || (event.Kind == "requirement.staleness_acknowledged" && event.ActorID == "user:local-operator" && event.ActorRole == core.ActorUser)
	}
	if !foundAck {
		t.Fatalf("audited acknowledgment missing from requirement timeline: %+v", ackEvents)
	}

	// Event ID breaks a timestamp tie, so a later append at the same stored
	// instant still sits beyond the acknowledged-through watermark.
	secondMerge := firstMerge
	if err = st.AppendEvent(ctx, core.Event{TaskID: deliveryTask.ID, Kind: "merge.confirmed", At: secondMerge, Payload: core.JSONPayload(map[string]any{
		"url": "https://example.test/pull/2", "task_title": deliveryTask.Title,
	})}); err != nil {
		t.Fatal(err)
	}
	view = getView()
	if !view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].SignalID == firstSignal.SignalID {
		t.Fatalf("later delivery did not raise a fresh signal: %+v", view.Staleness)
	}
	secondSignal := view.Staleness.Deliveries[0]
	followUpPath := fmt.Sprintf("/v1/requirements/%s/staleness/%s/follow-up", requirement.ID, secondSignal.SignalID)
	created := mutate(followUpPath)
	if created.Code != http.StatusCreated {
		t.Fatalf("follow-up create status=%d body=%s", created.Code, created.Body.String())
	}
	var firstResult struct {
		Task    core.Task `json:"task"`
		Created bool      `json:"created"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &firstResult); err != nil {
		t.Fatal(err)
	}
	retried := mutate(followUpPath)
	if retried.Code != http.StatusOK {
		t.Fatalf("follow-up retry status=%d body=%s", retried.Code, retried.Body.String())
	}
	var retryResult struct {
		Task    core.Task `json:"task"`
		Created bool      `json:"created"`
	}
	if err = json.Unmarshal(retried.Body.Bytes(), &retryResult); err != nil {
		t.Fatal(err)
	}
	if firstResult.Task.ID == "" || firstResult.Task.ID != retryResult.Task.ID || !firstResult.Created || retryResult.Created {
		t.Fatalf("follow-up dedup first=%+v retry=%+v", firstResult, retryResult)
	}
	context, err := store.TaskContextForTask(ctx, st, firstResult.Task.ID)
	if err != nil || len(context.Requirements) != 1 || context.Requirements[0].ID != requirement.ID {
		t.Fatalf("follow-up requirement context=%+v err=%v", context, err)
	}
	view = getView()
	if view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].FollowUp == nil || view.Staleness.Deliveries[0].FollowUp.TaskID != firstResult.Task.ID {
		t.Fatalf("open follow-up did not replace attention: %+v", view.Staleness)
	}
	listResponse = httptest.NewRecorder()
	handler.ServeHTTP(listResponse, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)))
	summaries = nil
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &summaries) != nil ||
		len(summaries) != 1 || len(summaries[0].Staleness.Deliveries) != 1 ||
		summaries[0].Staleness.Deliveries[0].FollowUp == nil ||
		summaries[0].Staleness.Deliveries[0].FollowUp.TaskID != firstResult.Task.ID {
		t.Fatalf("linked causal delivery missing from list: status=%d summaries=%+v", listResponse.Code, summaries)
	}
}

type truncatedDisplayLineageStore struct{ store.Store }

func (st *truncatedDisplayLineageStore) ListLineageNeighborhood(_ context.Context, roots []core.LineageNode, _ core.LineageTraversalBudget) ([]core.LineageLink, error) {
	links := make([]core.LineageLink, 0, 300)
	root := roots[0]
	for index := 0; index < 300; index++ {
		links = append(links, core.LineageLink{
			Workspace: "demo", SrcType: root.Type, SrcID: root.ID,
			DstType: core.LineageEvidence, DstID: fmt.Sprintf("artifact-%03d", index), Kind: "supports", CreatedByEventID: int64(index + 1),
		})
	}
	return links, nil
}

func TestRequirementStalenessIgnoresDisplayTruncation(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	requirement, proposed, err := base.CreateRequirement(ctx, core.Requirement{ID: "req-display-truncated", Title: "Bounded intent"}, core.RequirementVersion{
		Content: "Bounded intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Display fan-out does not suppress staleness."}},
		Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = base.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "display-truncated-delivery", Workspace: "demo", Title: "Display-independent delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-display-truncated-delivery", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = base.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{
		"id": requirement.ID, "version": proposed.Version,
	})}); err != nil {
		t.Fatal(err)
	}
	mergeAt := time.Now().UTC().Add(time.Minute)
	if err = base.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", At: mergeAt, Payload: core.JSONPayload(map[string]any{
		"repository": "kidus/conveyor", "base_sha": "base", "head_sha": "head", "task_title": task.Title,
	})}); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{ID: "direct_push:conveyor:display-independent", WorkspaceID: "demo", Repository: "conveyor", Kind: monitor.DirectPush,
		SourceURL: "https://example.test/commit/head", CommitSHA: "head", TaskID: task.ID, DetectedAt: mergeAt.Add(time.Minute)}
	if _, fresh, recordErr := base.(monitor.Store).RecordDrift(ctx, drift); recordErr != nil || !fresh {
		t.Fatalf("record drift fresh=%t err=%v", fresh, recordErr)
	}
	server := NewServer(&truncatedDisplayLineageStore{Store: base})
	server.Workspace = "demo"
	server.Monitor = &monitor.Service{Store: base.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.LineageGraph.Truncated || view.Staleness.PartialEvaluation || view.Staleness.DeliveryAfterIntent ||
		len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].TaskID != task.ID ||
		view.Staleness.Deliveries[0].NeedsAttention || len(view.Staleness.ActiveDrift) != 1 || view.Staleness.ActiveDrift[0].ID != drift.ID {
		t.Fatalf("display-independent staleness=%+v graph=%+v", view.Staleness, view.LineageGraph)
	}
}

type truncatedDeliveryLineageStore struct{ store.Store }

func (st *truncatedDeliveryLineageStore) ListRequirementDeliveryLineage(_ context.Context, requirementID string, _ core.LineageTraversalBudget) ([]core.LineageLink, error) {
	links := make([]core.LineageLink, 0, 300)
	for index := 0; index < 300; index++ {
		links = append(links, core.LineageLink{
			Workspace: "demo", SrcType: core.LineageRequirement, SrcID: requirementID,
			DstType: core.LineageTask, DstID: fmt.Sprintf("delivery-%03d", index), Kind: "serves", CreatedByEventID: int64(index + 1),
		})
	}
	return links, nil
}

func TestRequirementStalenessSurfacesTruncatedDeliveryEvaluation(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	requirement, proposed, err := base.CreateRequirement(ctx, core.Requirement{ID: "req-partial", Title: "Bounded delivery"}, core.RequirementVersion{
		Content: "Bounded delivery.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Partial delivery evaluation is visible."}},
		Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = base.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	server := NewServer(&truncatedDeliveryLineageStore{Store: base})
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Staleness.PartialEvaluation || view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 0 {
		t.Fatalf("partial staleness = %+v", view.Staleness)
	}
}

func TestRequirementStalenessIgnoresDismissedServesProjection(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-dismissed-stale", Title: "Dismissed intent"}, core.RequirementVersion{
		Content: "Dismissed service must not create staleness.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Dismissed service is not authority."}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-dismissed-stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	blueprint := core.Task{ID: "blueprint-dismissed-stale", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-blueprint-dismissed-stale", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementServes(ctx, blueprint.ID, requirement.ID, core.RequirementServesPlanning, false); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": requirement.ID})}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DismissRequirementServes(ctx, blueprint.ID, requirement.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: blueprint.ID, Kind: "merge.confirmed", At: time.Now().UTC().Add(time.Minute), Payload: core.JSONPayload(map[string]any{
		"repository": "kidus/conveyor", "base_sha": "base", "head_sha": "head", "task_title": blueprint.Title,
	})}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 0 {
		t.Fatalf("dismissed service created staleness: %+v", view.Staleness)
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
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(
		http.MethodGet, "/v1/requirements/"+seed.ID, nil,
	)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view requirementView
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.MigratedSeed || view.ConfirmationEligible || view.Staleness.DeliveryAfterIntent ||
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
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+seed.ID, nil)))
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
	handler.ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet,
		"/v1/requirements/"+requirement.ID, nil)))
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
		view.ServingBlueprints[0].Spec.Approved || view.Staleness.DeliveryAfterIntent ||
		len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].Label != "Blueprint delivery" ||
		view.Staleness.Deliveries[0].NeedsAttention ||
		slices.Contains(view.Staleness.Deliveries[0].Reasons, "delivered through related work without serving this requirement") {
		t.Fatalf("blueprint handoff=%+v staleness=%+v", view.ServingBlueprints, view.Staleness)
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
