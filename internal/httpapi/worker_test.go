package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type unauthorizedWorkerStore struct{ store.Store }

func (unauthorizedWorkerStore) IsDurable() bool { return true }
func (unauthorizedWorkerStore) AuthenticateWorker(context.Context, string) (core.Worker, error) {
	return core.Worker{}, store.ErrWorkerUnauthorized
}

type failingWorkerOrderStore struct{ store.Store }

func (failingWorkerOrderStore) ListWorkOrders(context.Context) ([]core.WorkOrder, error) {
	return nil, fmt.Errorf("raw worker order store failure")
}

func TestListWorkerOrdersMapsSentinelsAndRedactsInternalFailures(t *testing.T) {
	tests := []struct {
		name       string
		store      store.Store
		provider   func(context.Context) (*config.Config, error)
		wantStatus int
		wantBody   string
	}{
		{
			name:       "unauthorized",
			store:      unauthorizedWorkerStore{Store: store.NewMemory()},
			provider:   func(context.Context) (*config.Config, error) { return &config.Config{}, nil },
			wantStatus: http.StatusUnauthorized,
			wantBody:   "unauthorized\n",
		},
		{
			name:       "configuration unavailable",
			store:      store.NewMemory(),
			provider:   func(context.Context) (*config.Config, error) { return nil, fmt.Errorf("configuration unavailable") },
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "configuration unavailable\n",
		},
		{
			name:       "internal list failure",
			store:      failingWorkerOrderStore{Store: store.NewMemory()},
			provider:   func(context.Context) (*config.Config, error) { return &config.Config{}, nil },
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal server error\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orders := &workorder.Service{Store: test.store, ConfigProvider: test.provider}
			server := NewServer(test.store)
			server.ConfigProvider = test.provider
			server.Workers = &workerservice.Service{Store: test.store, WorkOrders: orders, ConfigProvider: test.provider}
			request := httptest.NewRequest(http.MethodGet, "/v1/worker/orders", nil)
			request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{CredentialHash: "hash"}))
			response := httptest.NewRecorder()
			server.listWorkerOrders(response, request)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody || strings.Contains(response.Body.String(), "raw worker order store failure") {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func workerConfigHTTPFixture(t *testing.T) (*Server, workerservice.Enrollment, *config.Config) {
	t.Helper()
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	pairing, _, err := workers.IssuePairing(store.WithWorkspace(t.Context(), "demo"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := workers.Enroll(t.Context(), pairing, "config-worker")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = orders
	server.Workers = workers
	return server, enrollment, cfg
}

func getWorkerConfigHTTP(server *Server, credential string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/worker/config", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestWorkerConfigHTTPRejectsMemoryBackendWithoutProvider(t *testing.T) {
	server, enrollment, _ := workerConfigHTTPFixture(t)

	response := getWorkerConfigHTTP(server, enrollment.Credential)
	if response.Code != http.StatusNotImplemented || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(response.Body.String(), "requires the Postgres database backend") {
		t.Fatalf("config status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
}

func TestWorkerConfigHTTPUsesConfiguredProvider(t *testing.T) {
	server, enrollment, cfg := workerConfigHTTPFixture(t)
	providerCalls := 0
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		providerCalls++
		return cfg, nil
	}

	response := getWorkerConfigHTTP(server, enrollment.Credential)
	if response.Code != http.StatusOK {
		t.Fatalf("config status=%d body=%s", response.Code, response.Body.String())
	}
	var workerConfig workerservice.WorkerConfig
	if err := json.Unmarshal(response.Body.Bytes(), &workerConfig); err != nil {
		t.Fatal(err)
	}
	if providerCalls != 1 || workerConfig.Workspace != "demo" || workerConfig.ActiveHarnesses == nil ||
		workerConfig.Execution.FirstActivityTimeoutText != config.DefaultFirstActivityTimeoutText {
		t.Fatalf("provider calls=%d config=%+v", providerCalls, workerConfig)
	}
}

func TestWorkerEnrollmentHeartbeatHealthAndRevocationHTTP(t *testing.T) {
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "codex"}, "review": {Harness: "codex", Execution: config.ExecutionMCP}}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "operator"
	server.ConfigProvider = provider
	server.WorkOrders = orders
	server.Workers = workers
	handler := server.Handler()
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	emptyList := call(http.MethodGet, "/v1/workers", "", "operator")
	if emptyList.Code != http.StatusOK || !strings.Contains(emptyList.Body.String(), `"workers":[]`) {
		t.Fatalf("empty list status=%d body=%s", emptyList.Code, emptyList.Body.String())
	}
	pairResponse := call(http.MethodPost, "/v1/workers/pairings", `{"ttl_seconds":60}`, "operator")
	if pairResponse.Code != http.StatusCreated {
		t.Fatalf("pair status=%d body=%s", pairResponse.Code, pairResponse.Body.String())
	}
	var pairing struct {
		PairingToken string `json:"pairing_token"`
	}
	_ = json.Unmarshal(pairResponse.Body.Bytes(), &pairing)
	enrollResponse := call(http.MethodPost, "/v1/worker/enroll", `{"pairing_token":"`+pairing.PairingToken+`","name":"laptop"}`, "")
	if enrollResponse.Code != http.StatusCreated {
		t.Fatalf("enroll status=%d body=%s", enrollResponse.Code, enrollResponse.Body.String())
	}
	var enrollment workerservice.Enrollment
	_ = json.Unmarshal(enrollResponse.Body.Bytes(), &enrollment)
	heartbeat := call(http.MethodPost, "/v1/worker/heartbeat", `{"probes":[{"harness":"codex","healthy":true,"checked_at":"`+time.Now().UTC().Format(time.RFC3339)+`"}]}`, enrollment.Credential)
	if heartbeat.Code != http.StatusOK {
		t.Fatalf("heartbeat status=%d body=%s", heartbeat.Code, heartbeat.Body.String())
	}
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "rate-health", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "rate-health-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, RequiredHarness: "codex", RequiredModel: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	rateOrder, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "rate-session", ClientToken: "secret", WorkerID: enrollment.Worker.ID, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	remaining := 7.0
	rateOrder.RateLimit = &core.RateLimitStatus{Status: "limited", Remaining: &remaining}
	rateOrder.RateLimitObservedAt = time.Now().UTC()
	if err = storetest.For(st).UpdateWorkOrder(ctx, rateOrder); err != nil {
		t.Fatal(err)
	}
	list := call(http.MethodGet, "/v1/workers", "", "operator")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"auto_available":true`) || !strings.Contains(list.Body.String(), `"rate_limits":[{"work_order_id":"rate-health-implement-1"`) || !strings.Contains(list.Body.String(), `"status":"limited"`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	revoke := call(http.MethodDelete, "/v1/workers/"+enrollment.Worker.ID, "", "operator")
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	if response := call(http.MethodPost, "/v1/worker/heartbeat", `{"probes":[]}`, enrollment.Credential); response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked heartbeat status=%d", response.Code)
	}
}

func TestWorkerClaimReconciliationIsReadOnlyAndServerAuthoritative(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo"}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	pairing, _, err := workers.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := workers.Enroll(t.Context(), pairing, "reconcile-worker")
	if err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "reconcile-task", Workspace: "demo", Mode: core.TaskModeAuto, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id      string
		session string
		lease   time.Duration
	}{
		{id: "active", session: "active-session", lease: time.Minute},
		{id: "expired", session: "expired-session", lease: time.Nanosecond},
	} {
		if err = st.CreateJob(ctx, core.Job{ID: fixture.id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: fixture.id, TaskID: task.ID, JobID: fixture.id, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err = storetest.For(st).ClaimWorkOrder(ctx, fixture.id, core.WorkOrderClaim{SessionID: fixture.session, ClientToken: fixture.id + "-token", WorkerID: enrollment.Worker.ID, ClaimantID: enrollment.Worker.ID, Lease: fixture.lease, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace, server.ConfigProvider, server.WorkOrders, server.Workers = "demo", provider, orders, workers
	call := func(id, session string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/worker/work-orders/"+id+"/reconcile?session_id="+session, nil)
		request.Header.Set("Authorization", "Bearer "+enrollment.Credential)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if active := call("active", "active-session"); active.Code != http.StatusOK || !strings.Contains(active.Body.String(), `"authorized":true`) {
		t.Fatalf("active status=%d body=%s", active.Code, active.Body.String())
	}
	if expired := call("expired", "expired-session"); expired.Code != http.StatusOK || !strings.Contains(expired.Body.String(), `"authorized":false`) || !strings.Contains(expired.Body.String(), `"state":"queued"`) {
		t.Fatalf("expired status=%d body=%s", expired.Code, expired.Body.String())
	}
}
