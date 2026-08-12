package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type failingLineageStore struct {
	store.Store
	err error
}

type recordingLineageStore struct {
	store.Store
	nodes []core.LineageNode
}

func (s *recordingLineageStore) ListLineageNodeRecords(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]store.LineageNodeRecord, error) {
	s.nodes = append([]core.LineageNode(nil), nodes...)
	return s.Store.ListLineageNodeRecords(ctx, nodes)
}

func TestLineageLabelsUseBoundedNodesAndReusePlanningSessions(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	memory := store.NewMemory()
	if err := memory.CreateTask(ctx, core.Task{ID: "task-bounded", Workspace: "demo", Title: "Bounded task", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	recording := &recordingLineageStore{Store: memory}
	server := NewServer(recording)
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	taskNode := core.LineageNode{Type: core.LineageTask, ID: "task-bounded"}
	sessionNode := core.LineageNode{Type: core.LineagePlanningSession, ID: "session-bounded"}
	labels, err := server.lineageNodeLabels(request, []core.LineageNode{taskNode, sessionNode}, []core.Artifact{}, []core.PlanningSession{{ID: sessionNode.ID, Title: "Preloaded session"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recording.nodes) != 1 || recording.nodes[0] != taskNode {
		t.Fatalf("bounded record nodes=%+v", recording.nodes)
	}
	if labels[taskNode] != "Bounded task" || labels[sessionNode] != "Preloaded session" {
		t.Fatalf("labels=%+v", labels)
	}
}

func TestLineageSystemDesignDecisionAndRepositoryNodesResolveDirectlyWithLabels(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-labelled", Title: "Labelled architecture", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Labelled\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version, 0); err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use direct graph labels.", Context: "Opaque IDs are hard to scan.", AlternativesRejected: "Linear edge scans.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	nodes := []core.LineageNode{
		{Type: core.LineageSystemDesign, ID: document.ID},
		{Type: core.LineageSystemDesignVersion, ID: core.SystemDesignVersionLineageID(document.ID, version.Version)},
		{Type: core.LineageDecision, ID: decision.ID},
		{Type: core.LineageRepositoryPath, ID: core.RepoPathComponentLineageID("conveyor", "internal/httpapi/**")},
	}
	server := NewServer(st)
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	for _, node := range nodes {
		exists, existsErr := server.lineageNodeExists(request, node)
		if existsErr != nil || !exists {
			t.Fatalf("node=%+v exists=%t err=%v", node, exists, existsErr)
		}
	}
	labels, err := server.lineageNodeLabels(request, nodes, []core.Artifact{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if labels[nodes[0]] != document.Title || !strings.Contains(labels[nodes[1]], document.Title) || labels[nodes[2]] != decision.Statement || labels[nodes[3]] != nodes[3].ID {
		t.Fatalf("labels=%+v", labels)
	}
}

func (s failingLineageStore) RebuildLineage(context.Context, core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	return core.LineageRebuildResult{}, s.err
}

func TestLineageHTTPReturnsBoundedTaskGraphAndTaskDetailProjection(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "task-lineage-api", Workspace: "demo", Title: "Trace delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-lineage-api", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": "task-lineage-api-implement-1"})}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 2, Nodes: 8, RenderableBytes: 4096, ArtifactRefs: 4}}}}}, nil
	}
	handler := server.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/lineage/task/"+task.ID+"?max_depth=2&max_nodes=8", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("lineage status=%d body=%s", response.Code, response.Body.String())
	}
	var graph core.LineageTraversal
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if len(graph.Roots) != 1 || graph.Roots[0].ID != task.ID || len(graph.Nodes) != 2 || len(graph.Links) != 1 || graph.Links[0].Kind != "dispatches" || graph.Truncated {
		t.Fatalf("lineage graph=%+v", graph)
	}

	activity := httptest.NewRecorder()
	handler.ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity", nil))
	if activity.Code != http.StatusOK || !json.Valid(activity.Body.Bytes()) || containsJSONField(activity.Body.Bytes(), "lineage_graph") {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}

	overBudget := httptest.NewRecorder()
	handler.ServeHTTP(overBudget, httptest.NewRequest(http.MethodGet, "/v1/lineage/task/"+task.ID+"?max_nodes=129", nil))
	if overBudget.Code != http.StatusBadRequest {
		t.Fatalf("over-budget status=%d body=%s", overBudget.Code, overBudget.Body.String())
	}
	overDepth := httptest.NewRecorder()
	handler.ServeHTTP(overDepth, httptest.NewRequest(http.MethodGet, "/v1/lineage/task/"+task.ID+"?max_depth=3", nil))
	if overDepth.Code != http.StatusBadRequest {
		t.Fatalf("configured over-depth status=%d body=%s", overDepth.Code, overDepth.Body.String())
	}
}

func TestLineageHTTPHidesConfigurationProviderDetails(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace = "demo"
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return nil, errors.New("secret provider address and credentials")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/lineage/task/anything", nil))
	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "lineage configuration is unavailable" ||
		strings.Contains(response.Body.String(), "credentials") {
		t.Fatalf("configuration failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestLineageHTTPDistinguishesUnlinkedAndAbsentRootsAndBoundsLargeGraphs(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	root := core.Task{ID: "large-root", Workspace: "demo", Title: "Human root title", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, root); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	call := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	unlinked := call("/v1/lineage/task/" + root.ID)
	if unlinked.Code != http.StatusOK {
		t.Fatalf("unlinked status=%d body=%s", unlinked.Code, unlinked.Body.String())
	}
	var empty core.LineageTraversal
	if json.Unmarshal(unlinked.Body.Bytes(), &empty) != nil || len(empty.Links) != 0 || empty.Nodes[0].Label != root.Title {
		t.Fatalf("unlinked graph=%+v", empty)
	}
	if absent := call("/v1/lineage/task/absent"); absent.Code != http.StatusNotFound {
		t.Fatalf("absent status=%d", absent.Code)
	}
	for index := 0; index < 140; index++ {
		if err := st.AppendEvent(ctx, core.Event{TaskID: root.ID, Kind: "task.dependency_added", Payload: core.JSONPayload(map[string]any{"task_id": root.ID, "depends_on_task_id": fmt.Sprintf("neighbor-%03d", index)})}); err != nil {
			t.Fatal(err)
		}
	}
	bounded := call("/v1/lineage/task/" + root.ID)
	var graph core.LineageTraversal
	if bounded.Code != http.StatusOK || json.Unmarshal(bounded.Body.Bytes(), &graph) != nil {
		t.Fatalf("large status=%d body=%s", bounded.Code, bounded.Body.String())
	}
	if len(graph.Nodes) > config.DefaultLineageContextNodes || len(graph.Links) > config.DefaultLineageContextNodes*config.DefaultLineageContextLinksPerNode || !graph.Truncated || graph.OmittedNodes == 0 || graph.Budget.MaxNodes != config.DefaultLineageContextNodes {
		t.Fatalf("large graph not honestly bounded: %+v", graph)
	}
}

func TestLineageRebuildRequiresOperatorAndAuditInput(t *testing.T) {
	st := store.NewMemoryWithConfig(&config.Config{Workspace: "demo"})
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "operator-token"
	call := func(token, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/lineage/rebuild", strings.NewReader(body))
		request.Header.Set("X-Workspace-ID", "demo")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if got := call("", `{}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got)
	}
	if got := call("operator-token", `{}`).Code; got != http.StatusBadRequest {
		t.Fatalf("missing input status=%d", got)
	}
	response := call("operator-token", `{"reason":"repair","request_id":"lineage-1"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result core.LineageRebuildResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	conflict := call("operator-token", `{"reason":"different","request_id":"lineage-1"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	broken := NewServer(failingLineageStore{Store: st, err: errors.New("database unavailable")})
	broken.Workspace, broken.BearerToken = "demo", "operator-token"
	internal := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/lineage/rebuild", strings.NewReader(`{"reason":"repair","request_id":"lineage-2"}`))
	request.Header.Set("X-Workspace-ID", "demo")
	request.Header.Set("Authorization", "Bearer operator-token")
	broken.Handler().ServeHTTP(internal, request)
	if internal.Code != http.StatusInternalServerError {
		t.Fatalf("internal status=%d body=%s", internal.Code, internal.Body.String())
	}
}

func TestLineageHTTPQueriesPlanningThroughDeliveryEvidenceEndToEnd(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	blueprint := core.Task{ID: "blueprint-e2e", Workspace: "demo", Title: "Blueprint", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	child := core.Task{ID: "child-e2e", Workspace: "demo", Title: "Child", Repo: "conveyor", ParentTaskID: blueprint.ID, OriginSpecVersion: 1, State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: blueprint.ID, Kind: "planning_session.finalized", Payload: core.JSONPayload(map[string]any{"session_id": "planning-e2e", "produced_requirement_id": "requirement-e2e", "produced_task_id": blueprint.ID})},
		{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": "requirement-e2e"})},
		{TaskID: blueprint.ID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: child.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": "review-e2e"})},
		{TaskID: child.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "kidus/conveyor", "number": 42, "base_sha": "base", "head_sha": "head"})},
		{TaskID: child.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus/conveyor", "base_sha": "base", "head_sha": "head"})},
		{TaskID: child.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-e2e", "evidence_ids": []string{"evidence-e2e"}})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	server := NewServer(st)
	server.Workspace = "demo"
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 8, Nodes: 32, RenderableBytes: 256 << 10}}}}}, nil
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/lineage/requirement/requirement-e2e", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("lineage status=%d body=%s", response.Code, response.Body.String())
	}
	var graph core.LineageTraversal
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	wantTypes := map[core.LineageNodeType]bool{
		core.LineagePlanningSession: false, core.LineageRequirement: false, core.LineageBlueprint: false,
		core.LineageBlueprintVersion: false, core.LineageTask: false, core.LineageWorkOrder: false,
		core.LineagePullRequest: false, core.LineageCommitRange: false, core.LineageEvidence: false, core.LineageVerdict: false,
	}
	for _, node := range graph.Nodes {
		if _, ok := wantTypes[node.Type]; ok {
			wantTypes[node.Type] = true
		}
	}
	for nodeType, found := range wantTypes {
		if !found {
			t.Errorf("full lineage graph omitted %s: %+v", nodeType, graph.Nodes)
		}
	}
	if graph.Truncated {
		t.Fatalf("small end-to-end graph was truncated: %+v", graph)
	}
}

func containsJSONField(data []byte, field string) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return false
	}
	_, ok := payload[field]
	return ok
}
