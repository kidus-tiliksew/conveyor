package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestMCPReadArtifactSupportsManualSessionsAndEnforcesWorkerOwnership(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "task-a", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()},
		{ID: "task-b", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		order, task, session, worker string
	}{
		{order: "order-a", task: "task-a", session: "session-a", worker: "worker-a"},
		{order: "order-b", task: "task-b", session: "session-b", worker: "worker-b"},
	} {
		if err := st.CreateJob(ctx, core.Job{ID: item.order, TaskID: item.task, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: item.order, TaskID: item.task, JobID: item.order, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimWorkOrder(ctx, item.order, core.WorkOrderClaim{SessionID: item.session, ClientToken: "secret", ClaimantID: item.worker, WorkerID: item.worker, Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "design.png", ContentType: "image/png", TaskID: "task-a"}, []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	args := map[string]any{"workspace_id": "demo", "work_order_id": "order-a", "session_id": "session-a", "artifact_id": artifact.ID}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err = server.callMCPTool(request, "read_artifact", args); err != nil {
		t.Fatalf("manual read: %v", err)
	}
	workerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-a", Workspace: "demo"}))
	if _, err = server.callMCPTool(workerRequest, "read_artifact", args); err != nil {
		t.Fatalf("owning worker read: %v", err)
	}
	otherWorkerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-b", Workspace: "demo"}))
	if _, err = server.callMCPTool(otherWorkerRequest, "read_artifact", args); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("other worker read error=%v", err)
	}
	wrongWorkspace := maps.Clone(args)
	wrongWorkspace["workspace_id"] = "other"
	if _, err = server.callMCPTool(request, "read_artifact", wrongWorkspace); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("wrong workspace read error=%v", err)
	}
}

func TestMCPToolsListRequiresAuthAndPublishesLifecycle(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	handler := server.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	want := []string{"create_task", "list_work_orders", "claim_work_order", "redispatch_work_order", "renew_work_order", "release_work_order", "get_work_order", "read_artifact", "report_progress", "report_usage", "upload_transcript", "submit_for_review", "await_review", "submit_review_verdict"}
	if len(envelope.Result.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(envelope.Result.Tools), len(want))
	}
	for i, name := range want {
		if envelope.Result.Tools[i].Name != name {
			t.Fatalf("tool[%d] = %q, want %q", i, envelope.Result.Tools[i].Name, name)
		}
	}
}

// A nil Go slice marshals to `"required": null`, which the official MCP
// SDK's validation rejects — taking every tool down with it as a
// "tools fetch failed" at connection time.
func TestMCPToolSchemasNeverEmitNullRequired(t *testing.T) {
	t.Parallel()
	for _, tool := range mcpTools() {
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"required":null`) {
			t.Fatalf("tool %v emits required:null", tool["name"])
		}
		if tool["name"] == "create_task" {
			schema := tool["inputSchema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			if _, present := properties["title"]; present {
				t.Fatalf("create_task still publishes a title field: %s", data)
			}
			bodyRequired := false
			for _, field := range schema["required"].([]string) {
				bodyRequired = bodyRequired || field == "body"
			}
			if !bodyRequired {
				t.Fatalf("create_task does not require body: %s", data)
			}
		}
	}
}

// Streamable-HTTP clients probe GET /mcp for an SSE stream after initialize;
// a server without one must answer 405, not fall through to the SPA catch-all
// as 200 HTML — that response wedges the client at "connecting".
func TestMCPNonPostReturnsMethodNotAllowedNotSPA(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	handler := server.Handler()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(method, "/mcp", nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		request.Header.Set("Accept", "text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp status = %d body=%s", method, response.Code, response.Body.String())
		}
		if allow := response.Header().Get("Allow"); allow != "POST" {
			t.Fatalf("%s /mcp Allow = %q", method, allow)
		}
	}
}

func TestMCPCreateTaskEnqueuesTriageIdempotently(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken = "operator-token"
	server.Workspace = "demo"
	server.Repos = []string{"api"}
	enqueued := 0
	generated := 0
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		generated++
		return "Triage this issue", nil
	}
	server.OnCreate = func(context.Context, string) { enqueued++ }
	handler := server.Handler()

	call := func(taskBody string) (core.Task, bool, bool) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "create_task", "arguments": map[string]any{"body": taskBody, "repo": "api", "source": "mcp:test-issue", "hold": true, "spec_approval": true, "merge_approval": true, "idempotency_key": "issue-42"}}})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
		request.Header.Set("Authorization", "Bearer operator-token")
		request.Header.Set("X-Conveyor-Actor", "issue-triage-agent")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.IsError {
			return core.Task{}, false, true
		}
		var result struct {
			Task    core.Task `json:"task"`
			Created bool      `json:"created"`
		}
		if len(envelope.Result.Content) != 1 {
			t.Fatalf("content = %+v", envelope.Result.Content)
		}
		if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &result); err != nil {
			t.Fatal(err)
		}
		return result.Task, result.Created, false
	}

	first, created, failed := call("from an MCP issue")
	if failed || !created || first.Title != "Triage this issue" || first.State != core.TaskQueued || first.NextStage != core.StageTriage {
		t.Fatalf("first task=%+v created=%t failed=%t", first, created, failed)
	}
	second, created, failed := call("from an MCP issue")
	if failed || created || second.ID != first.ID || enqueued != 1 || generated != 1 {
		t.Fatalf("retry task=%+v created=%t failed=%t enqueued=%d generated=%d", second, created, failed, enqueued, generated)
	}
	if _, _, failed = call("Different issue"); !failed {
		t.Fatal("reusing the idempotency key for different input succeeded")
	}
	tasks, err := st.ListTasks(t.Context())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	events, err := st.ListEvents(t.Context(), first.ID)
	if err != nil || len(events) != 1 || events[0].ActorID != "issue-triage-agent" || events[0].ActorRole != core.ActorAgent {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestMCPCreateTaskSelectsSetupAndRejectsUnknownName(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Workspace = "demo"
	setup := config.ExecutionSetup{Name: "backend", ExecutionSettings: config.ContextualExecutionSettings{
		ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
		Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "2h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionInProcess, TimeoutText: "1h", FallbackModel: "review"},
	}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "review"}}}}
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Execution: config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true}, Setups: []config.ExecutionSetup{setup}, DefaultSetup: setup.Name, Repos: []config.Repo{{Name: "api", Base: "main"}}}, nil
	}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "MCP setup", nil }
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	result, err := server.callMCPTool(request, "create_task", map[string]any{"workspace_id": "demo", "body": "work", "repo": "api", "setup": "backend", "idempotency_key": "setup-ok"})
	if err != nil {
		t.Fatal(err)
	}
	created := result.(map[string]any)["task"].(core.Task)
	if created.SetupName != "backend" || created.SetupContract.ExecutionSettings.Implementation.Harness != "codex" {
		t.Fatalf("created=%+v", created)
	}
	if _, err = server.callMCPTool(request, "create_task", map[string]any{"workspace_id": "demo", "body": "work", "repo": "api", "setup": "missing", "idempotency_key": "setup-missing"}); err == nil || !strings.Contains(err.Error(), "unknown setup") {
		t.Fatalf("unknown setup error=%v", err)
	}
}

func TestMCPCreateTaskRetryUsesPersistedPolicyBeforeLiveHealth(t *testing.T) {
	st := store.NewMemory()
	now := time.Now().UTC()
	cfg := &config.Config{
		Workspace: "demo",
		Execution: config.ExecutionPolicy{DefaultMode: "auto", SpecApproval: true, MergeApproval: true},
		Harnesses: []config.Harness{{Name: "codex"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Harness: "codex"},
			"review":    {Harness: "codex", Execution: config.ExecutionMCP},
		}},
		Repos: []config.Repo{{Name: "api", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	workers := &workerservice.Service{Store: st, ConfigProvider: provider, Now: func() time.Time { return now }}
	ctx := store.WithWorkspace(t.Context(), "demo")
	if err := st.CreateWorker(ctx, core.Worker{ID: "worker", Workspace: "demo", Name: "worker", CredentialHash: "hash", LeaseExpiresAt: now.Add(15 * time.Second), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.ConfigProvider, server.Workers = "demo", provider, workers
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Stable retry", nil }
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	arguments := map[string]any{"workspace_id": "demo", "body": "Keep retry policy stable", "repo": "api", "idempotency_key": "stable-policy"}
	firstResult, err := server.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatal(err)
	}
	first := firstResult.(map[string]any)["task"].(core.Task)
	if first.Mode != "" || first.Hold || !first.SpecApproval || !first.MergeApproval {
		t.Fatalf("first=%+v", first)
	}

	// Both live availability and omitted workspace gate defaults change after
	// intake. An exact retry must still return the persisted resolution.
	now = now.Add(time.Minute)
	cfg.Execution.SpecApproval = false
	cfg.Execution.MergeApproval = false
	secondResult, err := server.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatalf("exact retry after health/default change: %v", err)
	}
	second := secondResult.(map[string]any)["task"].(core.Task)
	if second.ID != first.ID || !second.SpecApproval || !second.MergeApproval {
		t.Fatalf("second=%+v first=%+v", second, first)
	}

	conflict := maps.Clone(arguments)
	conflict["spec_approval"] = false
	if _, err = server.callMCPTool(request, "create_task", conflict); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("explicit conflicting gate error=%v", err)
	}

	withTitle := maps.Clone(arguments)
	withTitle["title"] = "Caller title"
	if _, err = server.callMCPTool(request, "create_task", withTitle); err == nil || !strings.Contains(err.Error(), "must not be supplied") {
		t.Fatalf("title input error=%v", err)
	}
}

func TestResolveMCPWorkspaceFallbackFailsClosed(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	if _, err := server.resolveMCPWorkspace(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "workspace_unavailable") {
		t.Fatalf("zero-workspace omission error = %v", err)
	}
	if _, err := server.resolveMCPWorkspace(t.Context(), "unknown"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("zero-workspace explicit error = %v", err)
	}

	server.Workspace = "alpha"
	if got, err := server.resolveMCPWorkspace(t.Context(), ""); err != nil || got != "alpha" {
		t.Fatalf("singleton omission = %q, %v", got, err)
	}
	if got, err := server.resolveMCPWorkspace(t.Context(), "alpha"); err != nil || got != "alpha" {
		t.Fatalf("singleton explicit = %q, %v", got, err)
	}
	if _, err := server.resolveMCPWorkspace(t.Context(), "beta"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("unknown singleton explicit error = %v", err)
	}

	server.Deployment = &config.Config{Workspace: "beta"}
	if _, err := server.resolveMCPWorkspace(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "workspace_required") {
		t.Fatalf("ambiguous omission error = %v", err)
	}
}
