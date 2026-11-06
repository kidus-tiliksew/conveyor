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
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeWorkspaceConfigStore struct {
	record    config.VersionedDocument
	updates   int
	readErr   error
	updateErr error
}

func contextualWorkspaceDocument() config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeoutText: "24h",
		StageTimeouts: map[string]string{"spec": "30m", "implement": "2h", "review": "1h"},
		Review:        config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
		Execution:     config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 2},
		Repos:         []config.Repo{{Name: "conveyor", URL: "https://example.com/conveyor", Base: "main", InstallConveyor: config.InstallSwitch(false)}},
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
	if got.Document.ExecutionSettings != nil || strings.Contains(getResult.Body.String(), `"harnesses"`) || strings.Contains(getResult.Body.String(), `"setups"`) || len(got.Document.Routing.Stages) != 0 || got.Document.StageTimeouts["implement"] != "2h" || len(got.Document.Review.Seats) != 2 {
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
	if backend.record.Document.StageTimeouts["implement"] != "45m" || len(backend.record.Document.Review.Seats) != 2 || backend.record.Document.ExecutionSettings != nil || len(backend.record.Document.Routing.Stages) != 0 {
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

// A refused workspace configuration PUT answers 422 validation_failed naming the
// field, and neither the refusal nor anything stored holds the value. The repo
// rows are the only changed validator sites the route reaches. The hijack row
// holds another field's name in the value, which the former message handed to
// validationField. The control proves the same request succeeds with an accepted
// value (component-runtime invariant 17; req-security-boundaries REQ-2).
func TestWorkspaceConfigPUTRefusalNamesFieldAndHoldsNoValue(t *testing.T) {
	const mark = "FAKE0123456789"
	put := func(t *testing.T, document any) (*fakeWorkspaceConfigStore, *httptest.ResponseRecorder) {
		t.Helper()
		backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: contextualWorkspaceDocument(), Version: 1}}
		s := NewServer(store.NewMemory())
		s.BearerToken = "token"
		s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
		s.ConfigStore = backend
		body, err := json.Marshal(map[string]any{"document": document})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), mark) && !strings.Contains(string(body), "12345.678") && !strings.Contains(string(body), "accepted-name") {
			t.Fatal("request holds no misplaced value")
		}
		request := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("If-Match", "1")
		result := httptest.NewRecorder()
		s.Handler().ServeHTTP(result, request)
		return backend, result
	}
	withRepos := func(names ...string) config.WorkspaceDocument {
		document := contextualWorkspaceDocument()
		document.Repos = nil
		for _, name := range names {
			document.Repos = append(document.Repos, config.Repo{Name: name, URL: "https://example.com/fixture", Base: "main", InstallConveyor: config.InstallSwitch(false)})
		}
		return document
	}
	withRecovery := func(raw string) map[string]any {
		data, err := json.Marshal(contextualWorkspaceDocument())
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		document["recovery"] = json.RawMessage(raw)
		return document
	}
	for _, tc := range []struct {
		name         string
		document     any
		field, value string
		message      string
	}{
		{
			name: "invalid repo name", document: withRepos("fine", "sk-live/"+mark),
			field: "repos", value: mark,
			message: "repo 1: name must use only ASCII letters, digits, '.', '_', or '-' and must not be '.' or '..'",
		},
		{
			name: "duplicate repo name", document: withRepos("sk-live-"+mark, "fine", "sk-live-"+mark),
			field: "repos", value: mark,
			message: "repo 2: name duplicates repo 0",
		},
		{
			name: "invalid repo name holding another field", document: withRepos("max_bounces/" + mark),
			field: "repos", value: mark,
			message: "repo 0: name must use only ASCII letters, digits, '.', '_', or '-' and must not be '.' or '..'",
		},
		{
			// The route's JSON decode branch answers field document, as it does
			// for every decode failure, and the message names recovery.
			name: "recovery number encoding/json would print", document: withRecovery(`{"automatic_recoveries":12345.678}`),
			field: "document", value: "12345.678",
			message: "recovery must hold an integer automatic_recoveries and a string window",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, result := put(t, tc.document)
			if strings.Contains(result.Body.String(), tc.value) {
				t.Fatal("response holds the misplaced value")
			}
			var refusal struct {
				Error  string `json:"error"`
				Fields []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"fields"`
			}
			if err := json.Unmarshal(result.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if result.Code != http.StatusUnprocessableEntity || backend.updates != 0 || refusal.Error != "validation_failed" ||
				len(refusal.Fields) != 1 || refusal.Fields[0].Field != tc.field || refusal.Fields[0].Message != tc.message {
				t.Fatalf("status=%d updates=%d refusal=%+v", result.Code, backend.updates, refusal)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		backend, result := put(t, withRepos("accepted-name", "fine"))
		if result.Code != http.StatusOK || backend.updates != 1 {
			t.Fatalf("status=%d updates=%d", result.Code, backend.updates)
		}
	})
}

// Workspace creation shares the validators, so its refusal also names the field
// and holds no value (component-runtime invariant 18).
func TestCreateWorkspaceRefusalNamesFieldAndHoldsNoValue(t *testing.T) {
	const mark = "FAKE0123456789"
	for _, tc := range []struct {
		name         string
		document     map[string]any
		field, value string
		message      string
	}{
		{
			name: "invalid repo name",
			document: map[string]any{"repos": []any{
				map[string]any{"name": "sk-live/" + mark, "url": "https://example.test/repo"},
			}},
			field: "repos", value: mark,
			message: "repo 0: name must use only ASCII letters, digits, '.', '_', or '-' and must not be '.' or '..'",
		},
		{
			name: "duplicate repo name",
			document: map[string]any{"repos": []any{
				map[string]any{"name": "sk-live-" + mark, "url": "https://example.test/one"},
				map[string]any{"name": "sk-live-" + mark, "url": "https://example.test/two"},
			}},
			field: "repos", value: mark,
			message: "repo 1: name duplicates repo 0",
		},
		{
			name:     "recovery number encoding/json would print",
			document: map[string]any{"recovery": map[string]any{"automatic_recoveries": json.Number("12345.678")}},
			field:    "document", value: "12345.678",
			message: "recovery must hold an integer automatic_recoveries and a string window",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := &fakeWorkspaceControl{}
			create := NewServer(store.NewMemory())
			create.Workspaces, create.Deployment, create.BearerToken = control, legacyExecutorDeployment(), "token"
			create.Memberships = localOperatorInstanceAdmin()
			body, err := json.Marshal(map[string]any{"id": "engineering", "name": "Engineering", "document": tc.document})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer token")
			result := httptest.NewRecorder()
			create.Handler().ServeHTTP(result, request)
			if strings.Contains(result.Body.String(), tc.value) {
				t.Fatal("response holds the misplaced value")
			}
			var refusal struct {
				Error  string             `json:"error"`
				Fields []configFieldError `json:"fields"`
			}
			if err := json.Unmarshal(result.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if result.Code != http.StatusUnprocessableEntity || control.created != nil || refusal.Error != "validation_failed" ||
				len(refusal.Fields) != 1 || refusal.Fields[0].Field != tc.field || refusal.Fields[0].Message != tc.message {
				t.Fatalf("status=%d created=%v refusal=%+v", result.Code, control.created != nil, refusal)
			}
		})
	}
}

// validationField keeps naming the field for every changed validator family, read
// from the error the validator returns. Only the repo family reaches the route
// today; the other rows pin what validationField answers for the new text.
func TestValidationFieldNamesEachRedactedSiteFamily(t *testing.T) {
	const value = "sk-live-FAKE0123456789"
	const triage = "execution_settings: {control_plane: {triage: {model: fixture-model}}}\n"
	const database = "database: {backend: memory}\n"
	for _, tc := range []struct{ name, document, field string }{
		{"triage effort", "execution_settings: {control_plane: {triage: {model: fixture-model, effort: " + value + "}}}\n", "execution_settings.control_plane.triage.effort"},
		{"planning effort", "execution_settings: {control_plane: {triage: {model: fixture-model}, planning: {effort: " + value + "}}}\n", "execution_settings"},
		{"planning model", "execution_settings: {control_plane: {triage: {model: fixture-model}, planning: {model: " + value + "}}}\nplanning_models: [fixture-model]\n", "execution_settings"},
		{"planning_models duplicate", triage + "planning_models: [fixture-model, " + value + ", " + value + "]\n", "document"},
		{"repo name invalid", triage + "repos: [{name: 'a/" + value + "', url: https://example.test/a}]\n", "repos"},
		{"repo name duplicate", triage + "repos: [{name: " + value + ", url: https://example.test/a}, {name: " + value + ", url: https://example.test/b}]\n", "repos"},
		{"monitor unknown", triage + "repos: [{name: fine, url: https://example.test/a}]\nmonitor: {repositories: [" + value + "]}\n", "repos"},
		{"monitor duplicate", triage + "repos: [{name: " + value + ", url: https://example.test/a}]\nmonitor: {repositories: [" + value + ", " + value + "]}\n", "repos"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.ParseDeployment([]byte(database+tc.document), "deployment.yaml", nil)
			if err == nil {
				t.Fatal("document was accepted")
			}
			if strings.Contains(err.Error(), value) {
				t.Fatal("refusal holds the misplaced value")
			}
			if got := validationField(err); got != tc.field {
				t.Errorf("validationField = %q, want %q", got, tc.field)
			}
		})
	}
}

func TestWorkspaceConfigAPIRecoveryRoundTripAndRefusal(t *testing.T) {
	document := contextualWorkspaceDocument()
	document.Recovery = config.RecoveryPolicy{AutomaticRecoveries: 2, WindowText: "24h"}
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}
	s := NewServer(store.NewMemory())
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	h := s.Handler()

	get := httptest.NewRequest(http.MethodGet, "/v1/workspace/config", nil)
	get.Header.Set("Authorization", "Bearer token")
	getResult := httptest.NewRecorder()
	h.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK || !strings.Contains(getResult.Body.String(), `"automatic_recoveries":2`) || !strings.Contains(getResult.Body.String(), `"window":"24h"`) {
		t.Fatalf("GET status=%d body=%s", getResult.Code, getResult.Body)
	}

	document.Recovery = config.RecoveryPolicy{AutomaticRecoveries: 11, WindowText: "24h"}
	body, _ := json.Marshal(map[string]any{"document": document})
	refused := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	refused.Header.Set("Authorization", "Bearer token")
	refused.Header.Set("If-Match", "1")
	refusedResult := httptest.NewRecorder()
	h.ServeHTTP(refusedResult, refused)
	if refusedResult.Code != http.StatusUnprocessableEntity || backend.updates != 0 || !strings.Contains(refusedResult.Body.String(), `"field":"recovery.automatic_recoveries"`) {
		t.Fatalf("refusal status=%d updates=%d body=%s", refusedResult.Code, backend.updates, refusedResult.Body)
	}

	document.Recovery = config.RecoveryPolicy{AutomaticRecoveries: 0, WindowText: "1h"}
	body, _ = json.Marshal(map[string]any{"document": document})
	put := httptest.NewRequest(http.MethodPut, "/v1/workspace/config", strings.NewReader(string(body)))
	put.Header.Set("Authorization", "Bearer token")
	put.Header.Set("If-Match", "1")
	putResult := httptest.NewRecorder()
	h.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK || backend.record.Document.Recovery.AutomaticRecoveries != 0 || backend.record.Document.Recovery.WindowText != "1h" {
		t.Fatalf("PUT status=%d recovery=%+v body=%s", putResult.Code, backend.record.Document.Recovery, putResult.Body)
	}
}

// Every member who can view the configuration reads the issue publication
// setting, and only a manage_workspace caller changes it
// (req-delivery-and-forge AC-8.5).
func TestWorkspaceConfigIssuePublicationRequiresManageWorkspace(t *testing.T) {
	document := contextualWorkspaceDocument()
	document.Workspace = "alpha"
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 3}}
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"viewer": {"alpha": core.WorkspaceRoleViewer}, "contributor": {"alpha": core.WorkspaceRoleContributor},
			"maintainer": {"alpha": core.WorkspaceRoleMaintainer}, "operator": {"alpha": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = fixture, fixture
	server.Credentials = staticCredentialVerifier{}
	for user := range fixture.roles {
		server.Credentials.(staticCredentialVerifier)[user+"-token"] = core.AuthenticatedCredential{ID: "pat_" + user, OwnerUserID: user, Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	}
	server.Deployment = &config.Config{Workspace: "alpha", Database: config.Database{Backend: "memory"}}
	server.ConfigStore = backend
	handler := server.Handler()
	call := func(user, method, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/v1/workspace/config?workspace_id=alpha", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+user+"-token")
		request.Header.Set("If-Match", fmt.Sprintf("%d", backend.record.Version))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	turnOn := document
	turnOn.IssuePublication = config.IssuePublication{Enabled: true}
	body, err := json.Marshal(map[string]any{"document": turnOn})
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"viewer", "contributor", "maintainer"} {
		read := call(user, http.MethodGet, "")
		if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"issue_publication":{"enabled":false}`) {
			t.Fatalf("%s read status=%d body=%s", user, read.Code, read.Body)
		}
		write := call(user, http.MethodPut, string(body))
		if write.Code != http.StatusNotFound || write.Body.String() != canonicalWorkspaceNotFoundBody() || backend.updates != 0 {
			t.Fatalf("%s write status=%d updates=%d body=%q", user, write.Code, backend.updates, write.Body.String())
		}
	}
	if write := call("operator", http.MethodPut, string(body)); write.Code != http.StatusOK || backend.updates != 1 || !backend.record.Document.IssuePublication.Enabled {
		t.Fatalf("operator write status=%d updates=%d document=%+v body=%s", write.Code, backend.updates, backend.record.Document.IssuePublication, write.Body)
	}
	if read := call("viewer", http.MethodGet, ""); read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"issue_publication":{"enabled":true}`) {
		t.Fatalf("viewer read after turn-on status=%d body=%s", read.Code, read.Body)
	}
}
