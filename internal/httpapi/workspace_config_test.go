package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeWorkspaceConfigStore struct {
	record  config.VersionedDocument
	updates int
}

func TestHarnessTemplatesAPIRequiresAuthAndReturnsCatalog(t *testing.T) {
	s := NewServer(store.NewMemory())
	s.BearerToken = "token"
	h := s.Handler()

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/harness-templates", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/harness-templates", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	var body struct {
		Templates []config.HarnessTemplate `json:"templates"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Templates) != 3 || body.Templates[0].ID != "codex" || body.Templates[0].Harness.Command[0] != "codex" {
		t.Fatalf("unexpected templates response: %+v", body.Templates)
	}
}

func contextualWorkspaceDocument() config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeoutText: "24h",
		ExecutionSettings: &config.ContextualExecutionSettings{
			ControlPlane: config.ControlPlaneSettings{
				Triage: config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "20m"},
				Spec:   config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "30m"},
			},
			Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt-implement", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h", FallbackModel: "fallback", FallbackHarness: "codex"},
		},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", TimeoutText: "20m", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt", TimeoutText: "30m", Execution: config.ExecutionInProcess},
			"implement": {Model: "gpt-implement", ModelPolicy: config.ModelPolicyExplicit, Harness: "codex", TimeoutText: "2h", Execution: config.ExecutionMCP},
			"review":    {Model: "fallback", Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Harnesses: []config.Harness{
			{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
			{Name: "claude", Command: []string{"claude", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "5s"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{
			{Model: "gpt-review", Effort: "high"},
			{Model: "claude-review", Harness: "claude", Effort: "high"},
		}},
		Execution: config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 2},
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.com/conveyor", Base: "main"}},
	}
}

func (f *fakeWorkspaceConfigStore) WorkspaceConfig(context.Context) (config.VersionedDocument, error) {
	return f.record, nil
}

func (f *fakeWorkspaceConfigStore) UpdateWorkspaceConfig(ctx context.Context, expected int64, next *config.Config) (config.UpdateReceipt, error) {
	if expected != f.record.Version {
		return config.UpdateReceipt{}, config.ErrVersionConflict
	}
	f.updates++
	f.record = config.VersionedDocument{Document: next.WorkspaceDocument(), Version: expected + 1}
	return config.UpdateReceipt{
		VersionedDocument: f.record, EventID: 41,
		ActorID: store.ActorFromContext(ctx).ID, Sections: []string{"routing"},
	}, nil
}

func TestWorkspaceConfigAPIValidatesVersionsAndRecordsActor(t *testing.T) {
	document := contextualWorkspaceDocument()
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 3}}
	s := NewServer(store.NewMemory())
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	h := s.Handler()

	unauthorized := httptest.NewRequest(http.MethodGet, "/v1/workspace/config", nil)
	unauthorizedResult := httptest.NewRecorder()
	h.ServeHTTP(unauthorizedResult, unauthorized)
	if unauthorizedResult.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResult.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/workspace/config", nil)
	get.Header.Set("Authorization", "Bearer token")
	getResult := httptest.NewRecorder()
	h.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK || getResult.Header().Get("ETag") != `"3"` {
		t.Fatalf("GET status=%d etag=%q body=%s", getResult.Code, getResult.Header().Get("ETag"), getResult.Body)
	}
	if strings.Contains(strings.ToLower(getResult.Body.String()), "budget") {
		t.Fatalf("GET exposed removed budget surface: %s", getResult.Body)
	}
	var got config.VersionedDocument
	if err := json.Unmarshal(getResult.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Document.ExecutionSettings == nil || got.Document.ExecutionSettings.Implementation.Harness != "codex" || got.Document.Review.Seats[1].Effort != "high" || got.Document.Routing.Stages["review"].Harness != "codex" {
		t.Fatalf("GET lost contextual or compatibility config: %+v", got.Document)
	}
	if len(got.Document.Setups) != 1 || got.Document.Setups[0].Review.Seats == nil || len(got.Document.Setups[0].Review.Seats) != 2 {
		t.Fatalf("GET returned nullable or incomplete setup review seats: %+v", got.Document.Setups)
	}

	invalidDocument := document
	invalidDocument.MaxBounces = -1
	invalidBody, _ := json.Marshal(map[string]any{"document": invalidDocument})
	invalid := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(invalidBody)))
	invalid.Header.Set("Authorization", "Bearer token")
	invalid.Header.Set("If-Match", `"3"`)
	invalidResult := httptest.NewRecorder()
	h.ServeHTTP(invalidResult, invalid)
	if invalidResult.Code != http.StatusUnprocessableEntity || backend.updates != 0 || !strings.Contains(invalidResult.Body.String(), `"field":"max_bounces"`) {
		t.Fatalf("invalid status=%d updates=%d body=%s", invalidResult.Code, backend.updates, invalidResult.Body)
	}

	document.ExecutionSettings.Implementation.Effort = "medium"
	unsupportedBody, _ := json.Marshal(map[string]any{"document": document})
	unsupported := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(unsupportedBody)))
	unsupported.Header.Set("Authorization", "Bearer token")
	unsupported.Header.Set("If-Match", `"3"`)
	unsupportedResult := httptest.NewRecorder()
	h.ServeHTTP(unsupportedResult, unsupported)
	if unsupportedResult.Code != http.StatusUnprocessableEntity || backend.updates != 0 || !strings.Contains(unsupportedResult.Body.String(), `"field":"execution_settings.implementation.effort"`) || !strings.Contains(unsupportedResult.Body.String(), `effort \"medium\" is not supported by harness \"codex\"`) {
		t.Fatalf("unsupported effort status=%d updates=%d body=%s", unsupportedResult.Code, backend.updates, unsupportedResult.Body)
	}
	document.ExecutionSettings.Implementation.Effort = ""

	document.ExecutionSettings.Implementation.TimeoutText = "45m"
	document.Routing.Stages["implement"] = config.StageRoute{Model: "gpt-implement", ModelPolicy: config.ModelPolicyExplicit, Harness: "codex", TimeoutText: "45m", Execution: config.ExecutionMCP}
	body, _ := json.Marshal(map[string]any{"document": document})
	put := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	put.Header.Set("Authorization", "Bearer token")
	put.Header.Set("X-Conveyor-Actor", "alice")
	put.Header.Set("If-Match", "3")
	putResult := httptest.NewRecorder()
	h.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK || backend.updates != 1 || !strings.Contains(putResult.Body.String(), `"actor_id":"alice"`) {
		t.Fatalf("PUT status=%d updates=%d body=%s", putResult.Code, backend.updates, putResult.Body)
	}
	if backend.record.Document.ExecutionSettings == nil || backend.record.Document.ExecutionSettings.Implementation.TimeoutText != "45m" || backend.record.Document.Review.Seats[1].Effort != "high" || backend.record.Document.Routing.Stages["implement"].Harness != "codex" {
		t.Fatalf("PUT lost contextual or compatibility config: %+v", backend.record.Document)
	}

	stale := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	stale.Header.Set("Authorization", "Bearer token")
	stale.Header.Set("If-Match", "3")
	staleResult := httptest.NewRecorder()
	h.ServeHTTP(staleResult, stale)
	if staleResult.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d body=%s", staleResult.Code, staleResult.Body)
	}
}

func TestWorkspaceConfigAPIValidatesControlPlaneEffort(t *testing.T) {
	for _, stage := range []string{"triage", "spec"} {
		for _, effort := range []string{"", "minimal", "low", "medium", "high"} {
			t.Run(stage+"/accept/"+effort, func(t *testing.T) {
				document := contextualWorkspaceDocument()
				if stage == "triage" {
					document.ExecutionSettings.ControlPlane.Triage.Effort = effort
				} else {
					document.ExecutionSettings.ControlPlane.Spec.Effort = effort
				}
				backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}
				s := NewServer(store.NewMemory())
				s.BearerToken = "token"
				s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
				s.ConfigStore = backend
				body, _ := json.Marshal(map[string]any{"document": document})
				request := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
				request.Header.Set("Authorization", "Bearer token")
				request.Header.Set("If-Match", "1")
				result := httptest.NewRecorder()
				s.Handler().ServeHTTP(result, request)
				if result.Code != http.StatusOK || backend.updates != 1 {
					t.Fatalf("status=%d updates=%d body=%s", result.Code, backend.updates, result.Body)
				}
			})
		}

		t.Run(stage+"/reject", func(t *testing.T) {
			document := contextualWorkspaceDocument()
			if stage == "triage" {
				document.ExecutionSettings.ControlPlane.Triage.Effort = "maximum"
			} else {
				document.ExecutionSettings.ControlPlane.Spec.Effort = "maximum"
			}
			backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}
			s := NewServer(store.NewMemory())
			s.BearerToken = "token"
			s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
			s.ConfigStore = backend
			body, _ := json.Marshal(map[string]any{"document": document})
			request := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer token")
			request.Header.Set("If-Match", "1")
			result := httptest.NewRecorder()
			s.Handler().ServeHTTP(result, request)
			wantField := `"field":"execution_settings.control_plane.` + stage + `.effort"`
			if result.Code != http.StatusUnprocessableEntity || backend.updates != 0 || !strings.Contains(result.Body.String(), wantField) || !strings.Contains(result.Body.String(), "maximum") {
				t.Fatalf("status=%d updates=%d body=%s", result.Code, backend.updates, result.Body)
			}
		})
	}
}
