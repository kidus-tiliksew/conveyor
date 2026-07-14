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
	document := config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", BudgetUSD: 1, TimeoutText: "20m", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt", BudgetUSD: 1, TimeoutText: "30m", Execution: config.ExecutionInProcess},
			"implement": {Model: "operator", BudgetUSD: 3, TimeoutText: "2h", Execution: config.ExecutionMCP},
			"review":    {Model: "operator", BudgetUSD: 1, TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Repos: []config.Repo{{Name: "conveyor", URL: "https://example.com/conveyor", Base: "main"}},
	}
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

	document.Routing.Stages["implement"] = config.StageRoute{Model: "operator", BudgetUSD: 2, TimeoutText: "45m", Execution: config.ExecutionMCP}
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

	stale := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	stale.Header.Set("Authorization", "Bearer token")
	stale.Header.Set("If-Match", "3")
	staleResult := httptest.NewRecorder()
	h.ServeHTTP(staleResult, stale)
	if staleResult.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d body=%s", staleResult.Code, staleResult.Body)
	}
}
