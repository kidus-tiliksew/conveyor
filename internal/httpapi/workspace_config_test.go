package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeWorkspaceConfigStore struct {
	record    config.VersionedDocument
	updates   int
	readErr   error
	updateErr error
}

func TestHarnessTemplatesAPIRequiresAuthAndIsRetired(t *testing.T) {
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
	if response.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "execution_configuration_retired") {
		t.Fatalf("unexpected retirement response: %s", response.Body)
	}
}

func contextualWorkspaceDocument() config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeoutText: "24h",
		StageTimeouts: map[string]string{"spec": "30m", "implement": "2h", "review": "1h"},
		Review:        config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
		Execution:     config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 2},
		Repos:         []config.Repo{{Name: "conveyor", URL: "https://example.com/conveyor", Base: "main"}},
	}
}

func (f *fakeWorkspaceConfigStore) WorkspaceConfig(context.Context) (config.VersionedDocument, error) {
	return f.record, f.readErr
}

func (f *fakeWorkspaceConfigStore) UpdateWorkspaceConfig(ctx context.Context, expected int64, next *config.Config) (config.UpdateReceipt, error) {
	if f.updateErr != nil {
		return config.UpdateReceipt{}, f.updateErr
	}
	if expected != f.record.Version {
		return config.UpdateReceipt{}, config.ErrVersionConflict
	}
	f.updates++
	f.record = config.VersionedDocument{Document: next.PolicyDocument(), Version: expected + 1}
	return config.UpdateReceipt{
		VersionedDocument: f.record, EventID: 41,
		ActorID: store.ActorFromContext(ctx).ID, Sections: []string{"routing"},
	}, nil
}

func TestWorkspaceConfigAPIMissingConfigReturnsNotFound(t *testing.T) {
	document := contextualWorkspaceDocument()
	for _, test := range []struct {
		name    string
		method  string
		backend *fakeWorkspaceConfigStore
	}{
		{name: "read", method: http.MethodGet, backend: &fakeWorkspaceConfigStore{readErr: fmt.Errorf("missing row: %w", store.ErrNotFound)}},
		{name: "update", method: http.MethodPut, backend: &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}, updateErr: fmt.Errorf("missing row: %w", store.ErrNotFound)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(store.NewMemory())
			server.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
			server.ConfigStore = test.backend
			body := ""
			if test.method == http.MethodPut {
				data, err := json.Marshal(map[string]any{"document": document})
				if err != nil {
					t.Fatal(err)
				}
				body = string(data)
			}
			request := httptest.NewRequest(test.method, "/v1/workspace/config", strings.NewReader(body))
			request = request.WithContext(store.WithWorkspace(request.Context(), "demo"))
			request.Header.Set("If-Match", "1")
			response := httptest.NewRecorder()
			if test.method == http.MethodGet {
				server.getWorkspaceConfig(response, request)
			} else {
				server.putWorkspaceConfig(response, request)
			}
			if response.Code != http.StatusNotFound || response.Body.String() != "workspace config unavailable\n" || strings.Contains(response.Body.String(), "missing row") {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestWorkspaceConfigAPIValidatesVersionsAndRecordsActor(t *testing.T) {
	document := contextualWorkspaceDocument()
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 3}}
	s := NewServer(store.NewMemory())
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	h := s.Handler()

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
	if got.Document.ExecutionSettings != nil || len(got.Document.Harnesses) != 0 || len(got.Document.Setups) != 0 || len(got.Document.Routing.Stages) != 0 || got.Document.StageTimeouts["implement"] != "2h" || len(got.Document.Review.Seats) != 2 {
		t.Fatalf("GET lost policy or exposed execution detail: %+v", got.Document)
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

	document.StageTimeouts["implement"] = "45m"
	body, _ := json.Marshal(map[string]any{"document": document})
	put := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	put.Header.Set("Authorization", "Bearer token")
	put.Header.Set("X-Conveyor-Actor", "alice")
	put.Header.Set("If-Match", "3")
	putResult := httptest.NewRecorder()
	h.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK || backend.updates != 1 || !strings.Contains(putResult.Body.String(), `"actor_id":"user:local-operator"`) {
		t.Fatalf("PUT status=%d updates=%d body=%s", putResult.Code, backend.updates, putResult.Body)
	}
	if backend.record.Document.StageTimeouts["implement"] != "45m" || len(backend.record.Document.Review.Seats) != 2 || backend.record.Document.ExecutionSettings != nil || len(backend.record.Document.Harnesses) != 0 {
		t.Fatalf("PUT lost policy or exposed execution detail: %+v", backend.record.Document)
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

func TestWorkspaceConfigAPIReportsInvalidRepositoryNameAsFieldError(t *testing.T) {
	document := contextualWorkspaceDocument()
	document.Repos[0].Name = "../outside"
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: contextualWorkspaceDocument(), Version: 1}}
	s := NewServer(store.NewMemory())
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	body, err := json.Marshal(map[string]any{"document": document})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("If-Match", "1")
	result := httptest.NewRecorder()
	s.Handler().ServeHTTP(result, request)

	if result.Code != http.StatusUnprocessableEntity || backend.updates != 0 ||
		!strings.Contains(result.Body.String(), `"error":"validation_failed"`) ||
		!strings.Contains(result.Body.String(), `"field":"repos"`) ||
		!strings.Contains(result.Body.String(), "repo 0: name") {
		t.Fatalf("status=%d updates=%d body=%s", result.Code, backend.updates, result.Body)
	}
}
