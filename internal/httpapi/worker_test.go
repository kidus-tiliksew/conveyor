package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func workerEvidenceRequest(t *testing.T, credential, orderID, session, token, contentType string, content []byte, extra map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range extra {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="proof.bin"`)
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/worker/work-orders/"+orderID+"/verification-evidence", &body)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Conveyor-Work-Order-Session", session)
	request.Header.Set("X-Conveyor-Work-Order-Token", token)
	return request
}

func TestWorkerVerificationEvidenceUploadIsBoundToLiveClaim(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	orders := &workorder.Service{Store: st}
	workers := &workerservice.Service{Store: st, WorkOrders: orders}
	pairing, _, err := workers.IssuePairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := workers.Enroll(t.Context(), pairing, "evidence-worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"evidence-task", "other-task"} {
		if err = st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: "evidence-task-implement-1", TaskID: "evidence-task", Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: job.TaskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		WorkerID: enrollment.Worker.ID, ClaimantID: enrollment.Worker.ID,
		SessionID: "evidence-session", ClientToken: "evidence-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.WorkOrders, server.Workers = "demo", orders, workers
	handler := server.Handler()

	success := httptest.NewRecorder()
	handler.ServeHTTP(success, workerEvidenceRequest(t, enrollment.Credential, job.ID, "evidence-session", "evidence-token", "IMAGE/PNG; charset=binary", []byte("png evidence"), nil))
	if success.Code != http.StatusCreated {
		t.Fatalf("success status=%d body=%s", success.Code, success.Body.String())
	}
	var artifact core.Artifact
	if err = json.Unmarshal(success.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.TaskID != "evidence-task" || artifact.Workspace != "demo" || artifact.Role != core.ArtifactRoleVerificationEvidence || artifact.ContentType != "image/png" {
		t.Fatalf("artifact=%+v", artifact)
	}

	wrongToken := httptest.NewRecorder()
	handler.ServeHTTP(wrongToken, workerEvidenceRequest(t, enrollment.Credential, job.ID, "evidence-session", "wrong", "image/png", []byte("other"), nil))
	if wrongToken.Code != http.StatusConflict || wrongToken.Header().Get("X-Conveyor-Error-Code") != "verification_evidence_claim_conflict" {
		t.Fatalf("wrong token status=%d body=%s", wrongToken.Code, wrongToken.Body.String())
	}
	crossTask := httptest.NewRecorder()
	handler.ServeHTTP(crossTask, workerEvidenceRequest(t, enrollment.Credential, job.ID, "evidence-session", "evidence-token", "image/png", []byte("other"), map[string]string{"task_id": "other-task"}))
	if crossTask.Code != http.StatusBadRequest || !strings.Contains(crossTask.Body.String(), "only one file") {
		t.Fatalf("cross-task status=%d body=%s", crossTask.Code, crossTask.Body.String())
	}
	unsupported := httptest.NewRecorder()
	handler.ServeHTTP(unsupported, workerEvidenceRequest(t, enrollment.Credential, job.ID, "evidence-session", "evidence-token", "image/gif", []byte("gif"), nil))
	if unsupported.Code != http.StatusBadRequest || !strings.Contains(unsupported.Body.String(), "unsupported") {
		t.Fatalf("unsupported status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
}

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

func TestWorkerRenewHTTPReportsSameSessionCheckpointRelease(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "worker-checkpoint", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: "worker-checkpoint-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{WorkerID: "worker-a", ClaimantID: "worker-a", SessionID: "session-a", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, "worker-a", core.WorkOrderRelease{SessionID: "session-a", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workers = &workerservice.Service{Store: st}
	route := chi.NewRouteContext()
	route.URLParams.Add("id", job.ID)
	request := httptest.NewRequest(http.MethodPost, "/v1/worker/work-orders/"+job.ID+"/renew", strings.NewReader(`{"session_id":"session-a"}`))
	request = request.WithContext(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, route), workerContextKey{}, core.Worker{ID: "worker-a"}))
	response := httptest.NewRecorder()
	server.renewWorkerOrder(response, request)
	if response.Code != http.StatusConflict || response.Header().Get("X-Conveyor-Error-Code") != "work_order_released_checkpoint" || !strings.Contains(response.Body.String(), "released by this session") {
		t.Fatalf("status=%d code=%q body=%s", response.Code, response.Header().Get("X-Conveyor-Error-Code"), response.Body.String())
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
	if emptyList.Code != http.StatusOK || !strings.Contains(emptyList.Body.String(), `"workers":[]`) ||
		!strings.Contains(emptyList.Body.String(), `"worker_expected":false`) ||
		strings.Contains(emptyList.Body.String(), `"worker_unavailable_reason"`) {
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
	staleList := call(http.MethodGet, "/v1/workers", "", "operator")
	if staleList.Code != http.StatusOK || !strings.Contains(staleList.Body.String(), `"worker_expected":true`) ||
		!strings.Contains(staleList.Body.String(), `"worker_available":false`) ||
		!strings.Contains(staleList.Body.String(), `"worker_unavailable_reason":"enrolled worker \"laptop\": worker liveness lease expired"`) {
		t.Fatalf("stale list status=%d body=%s", staleList.Code, staleList.Body.String())
	}
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
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"worker_available":true`) || !strings.Contains(list.Body.String(), `"rate_limits":[{"work_order_id":"rate-health-implement-1"`) || !strings.Contains(list.Body.String(), `"status":"limited"`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	revoke := call(http.MethodDelete, "/v1/workers/"+enrollment.Worker.ID, "", "operator")
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	revokedList := call(http.MethodGet, "/v1/workers", "", "operator")
	if revokedList.Code != http.StatusOK || !strings.Contains(revokedList.Body.String(), `"worker_expected":false`) ||
		strings.Contains(revokedList.Body.String(), `"worker_unavailable_reason"`) {
		t.Fatalf("revoked list status=%d body=%s", revokedList.Code, revokedList.Body.String())
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
