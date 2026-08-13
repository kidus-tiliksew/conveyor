package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func createMemoryWorkOrderInState(t *testing.T, st store.Store, ctx context.Context, order core.WorkOrder) core.WorkOrder {
	t.Helper()
	target := order.State
	order.State = core.WorkOrderQueued
	if target == core.WorkOrderTimedOut && order.ExecutionDeadline.IsZero() {
		order.ExecutionDeadline = time.Now().Add(-time.Minute)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	switch target {
	case core.WorkOrderQueued:
		return order
	case core.WorkOrderClaimed, core.WorkOrderCompleted:
		lease := time.Minute
		if target == core.WorkOrderClaimed && order.LeaseExpiresAt.After(time.Now()) {
			lease = time.Until(order.LeaseExpiresAt)
		}
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: order.ID + "-session", ClientToken: "test-token", ClaimantID: "worker", WorkerID: "worker", Lease: lease})
		if err != nil {
			t.Fatal(err)
		}
		if target == core.WorkOrderClaimed {
			return claimed
		}
		claimed.State = core.WorkOrderCompleted
		if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
			t.Fatal(err)
		}
		return claimed
	case core.WorkOrderTimedOut:
		persisted, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil || persisted.State != core.WorkOrderTimedOut {
			t.Fatalf("timed-out order=%+v err=%v", persisted, err)
		}
		return persisted
	default:
		t.Fatalf("unsupported work-order fixture state %q", target)
		return core.WorkOrder{}
	}
}

type failOnceArtifactStore struct {
	store.Store
	calls  int
	failAt int
}

type notFoundRaceStore struct {
	store.Store
	failEnsure       bool
	failIntervention bool
}

func (s notFoundRaceStore) EnsureTaskEnqueued(context.Context, string) error {
	if s.failEnsure {
		return fmt.Errorf("enqueue race: %w", store.ErrNotFound)
	}
	return nil
}

func (s notFoundRaceStore) CreateIntervention(context.Context, core.Intervention) error {
	if s.failIntervention {
		return fmt.Errorf("intervention race: %w", store.ErrNotFound)
	}
	return nil
}

type staticCredentialVerifier map[string]core.AuthenticatedCredential

func (v staticCredentialVerifier) VerifyCredential(_ context.Context, token string) (core.AuthenticatedCredential, error) {
	credential, ok := v[token]
	if !ok {
		return core.AuthenticatedCredential{}, core.ErrInvalidCredential
	}
	return credential, nil
}

type failingCredentialVerifier struct{}

func (failingCredentialVerifier) VerifyCredential(context.Context, string) (core.AuthenticatedCredential, error) {
	return core.AuthenticatedCredential{}, errors.New("credential database unavailable")
}

func TestCredentialAuthDerivesTypedActorsAndIgnoresAssertedHeader(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Credentials = staticCredentialVerifier{
		"human-secret": {ID: "pat_1", OwnerUserID: "usr_1", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"agent-secret": {ID: "agt_1", OwnerUserID: "usr_1", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.ActorFromContext(r.Context()))
	})
	call := func(handler http.Handler, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Conveyor-Actor", "attacker")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	for _, test := range []struct {
		name, token, actor string
		role               core.ActorRole
	}{
		{name: "user", token: "human-secret", actor: "user:usr_1", role: core.ActorUser},
		{name: "agent", token: "agent-secret", actor: "agent:agt_1", role: core.ActorAgent},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := call(server.requireMCPAuth(capture), test.token)
			var actor store.Actor
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &actor) != nil || actor.ID != test.actor || actor.Role != test.role {
				t.Fatalf("status=%d actor=%+v body=%s", response.Code, actor, response.Body.String())
			}
		})
	}
	if response := call(server.requireMutationCapability(core.CapabilityOperateGates)(capture), "agent-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("execution credential reached human mutation: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCredentialStoreFailureReturnsInternalServerError(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Credentials = failingCredentialVerifier{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer secret-that-must-not-leak")
	response := httptest.NewRecorder()
	server.requireMCPAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request reached handler after credential-store failure")
	})).ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret-that-must-not-leak") || strings.Contains(response.Body.String(), "database unavailable") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func (st *failOnceArtifactStore) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	st.calls++
	if st.calls == st.failAt {
		return core.Artifact{}, fmt.Errorf("artifact store unavailable")
	}
	return st.Store.CreateArtifact(ctx, artifact, content)
}

func attachmentTaskRequest(t *testing.T, intakeKey string, files map[string][]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("task", `{"body":"Use every file","repo":"api","mode":"manual","spec_approval":true,"merge_approval":true,"source":"dashboard"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("idempotency_key", intakeKey); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		part, err := writer.CreateFormFile("attachments", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", &body)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func artifactUploadRequest(t *testing.T, taskID string, role core.ArtifactRole, filename, contentType string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("task_id", taskID); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("role", string(role)); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
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
	request := httptest.NewRequest(http.MethodPost, "/v1/artifacts?workspace_id=demo", &body)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestVerificationEvidenceUploadAndTaskActivityUseExplicitRole(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "evidence-api", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken = "token"
	server.Workspace = "demo"

	valid := httptest.NewRecorder()
	server.Handler().ServeHTTP(valid, artifactUploadRequest(t, task.ID, core.ArtifactRoleVerificationEvidence, "proof.png", "IMAGE/PNG; charset=binary", []byte("png evidence")))
	if valid.Code != http.StatusCreated ||
		!strings.Contains(valid.Body.String(), `"role":"verification_evidence"`) ||
		!strings.Contains(valid.Body.String(), `"content_type":"image/png"`) {
		t.Fatalf("valid upload status=%d body=%s", valid.Code, valid.Body)
	}

	unsupported := httptest.NewRecorder()
	server.Handler().ServeHTTP(unsupported, artifactUploadRequest(t, task.ID, core.ArtifactRoleVerificationEvidence, "proof.gif", "image/gif", []byte("gif evidence")))
	if unsupported.Code != http.StatusBadRequest || !strings.Contains(unsupported.Body.String(), "unsupported") {
		t.Fatalf("unsupported status=%d body=%s", unsupported.Code, unsupported.Body)
	}

	internalRole := httptest.NewRecorder()
	server.Handler().ServeHTTP(internalRole, artifactUploadRequest(t, task.ID, core.ArtifactRoleGeneratedAudit, "audit.txt", "text/plain", []byte("not operator-generated")))
	if internalRole.Code != http.StatusBadRequest || !strings.Contains(internalRole.Body.String(), "task_context or verification_evidence") {
		t.Fatalf("internal role status=%d body=%s", internalRole.Code, internalRole.Body)
	}

	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	if activity.Code != http.StatusOK ||
		!strings.Contains(activity.Body.String(), `"verification_evidence":[`) ||
		!strings.Contains(activity.Body.String(), `"role":"verification_evidence"`) ||
		!strings.Contains(activity.Body.String(), `"attachments":[]`) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body)
	}
}

func TestAttachmentTaskCreationStoresEveryFileBeforeEnqueueAndRetriesDraft(t *testing.T) {
	t.Parallel()
	base := store.NewMemory()
	flaky := &failOnceArtifactStore{Store: base, failAt: 2}
	server := NewServer(flaky)
	server.BearerToken, server.Workspace, server.Repos = "token", "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Attachment task", nil }
	enqueued := 0
	server.OnCreate = func(ctx context.Context, taskID string) {
		enqueued++
		task, err := flaky.GetTask(ctx, taskID)
		if err != nil || task.State != core.TaskQueued {
			t.Errorf("task at enqueue=%+v err=%v", task, err)
		}
		artifacts, err := flaky.ListArtifacts(ctx)
		if err != nil || len(artifacts) != 2 {
			t.Errorf("artifacts at enqueue=%+v err=%v", artifacts, err)
		}
	}
	files := map[string][]byte{"brief.txt": []byte("brief"), "design.png": append([]byte("\x89PNG\r\n\x1a\n"), []byte("design")...)}
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, attachmentTaskRequest(t, "attachment-retry", files))
	if first.Code != http.StatusUnprocessableEntity || !strings.Contains(first.Body.String(), "remains unqueued") || enqueued != 0 {
		t.Fatalf("first status=%d body=%s enqueued=%d", first.Code, first.Body.String(), enqueued)
	}
	tasks, err := flaky.ListTasks(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(tasks) != 1 || tasks[0].State != core.TaskClaiming {
		t.Fatalf("draft tasks=%+v err=%v", tasks, err)
	}

	retry := httptest.NewRecorder()
	server.Handler().ServeHTTP(retry, attachmentTaskRequest(t, "attachment-retry", files))
	if retry.Code != http.StatusCreated || enqueued != 1 {
		t.Fatalf("retry status=%d body=%s enqueued=%d", retry.Code, retry.Body.String(), enqueued)
	}
	tasks, err = flaky.ListTasks(store.WithWorkspace(t.Context(), "demo"))
	artifacts, artifactErr := flaky.ListArtifacts(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || artifactErr != nil || len(tasks) != 1 || tasks[0].State != core.TaskQueued || len(artifacts) != 2 {
		t.Fatalf("tasks=%+v artifacts=%+v errors=%v/%v", tasks, artifacts, err, artifactErr)
	}
}

func TestWorkOrderRecoveryHTTPIsAuthorizedFailClosedAndIdempotent(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "recover-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "recover-order", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "child", ClientToken: "secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: "child", Outcome: core.WorkOrderOutcomeCancelled, Reason: "worker shutting down"}); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	call := func(token, requestID, direction string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"request_id": requestID, "direction": direction})
		request := httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+job.ID+"/recover?workspace_id=demo", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("", "recover-1", "Proceed"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("token", "", "Proceed"); response.Code != http.StatusConflict {
		t.Fatalf("missing id status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("token", "too-long", strings.Repeat("x", core.MaxWorkOrderOperatorDirectionRunes+1)); response.Code != http.StatusBadRequest {
		t.Fatalf("over-limit status=%d body=%s", response.Code, response.Body.String())
	}
	first := call("token", "recover-1", "  Proceed with the amendment.  ")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"claimable":true`) || !strings.Contains(first.Body.String(), `"operator_direction":"Proceed with the amendment."`) {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := call("token", "recover-1", "replacement")
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"operator_direction":"Proceed with the amendment."`) {
		t.Fatalf("duplicate status=%d body=%s", second.Code, second.Body.String())
	}
	if response := call("token", "recover-2", "Another direction"); response.Code != http.StatusConflict {
		t.Fatalf("new request after recovery status=%d body=%s", response.Code, response.Body.String())
	}
	count, _ := st.CountEvents(ctx, task.ID, "work_order.redispatched")
	if count != 1 {
		t.Fatalf("redispatch events=%d", count)
	}
}

func TestWorkOrderRecoveryHTTPConflictsWithPendingPlanRevisionDecision(t *testing.T) {
	ctx, st, server, _, orderID := newPlanRevisionReviewServer(t)
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
	}
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}

	request := httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+orderID+"/recover?workspace_id=demo", bytes.NewBufferString(`{"request_id":"recover-pending-plan"}`))
	request = request.WithContext(ctx)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "pending plan-revision decision") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || order.State != core.WorkOrderQueued || !order.RetrySuppressed || order.Claimable {
		t.Fatalf("pending order changed: %+v err=%v", order, err)
	}
}

func TestTaskCloseRequiresReasonAndCancelsOutsideHumanGate(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "close-running", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "close-running-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	handler := server.Handler()
	call := func(taskID, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/close?workspace_id=demo", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Conveyor-Actor", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(task.ID, `{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(task.ID, `{"reason":"obsolete"}`); response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"state":"closed"`) {
		t.Fatalf("close status=%d body=%s", response.Code, response.Body.String())
	}
	order, _ := st.GetWorkOrder(ctx, job.ID)
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if order.State != core.WorkOrderCancelled || len(interventions) != 1 || interventions[0].ActorID != "user:local-operator" || interventions[0].Action != core.InterventionCancel {
		t.Fatalf("order=%+v interventions=%+v", order, interventions)
	}
	if response := call(task.ID, `{"reason":"again"}`); response.Code != http.StatusConflict {
		t.Fatalf("terminal close status=%d body=%s", response.Code, response.Body.String())
	}
	for _, kind := range []string{"intervention.cancel", "task.cancelled"} {
		events, _ := st.CountEvents(ctx, task.ID, kind)
		if events != 1 {
			t.Fatalf("%s events=%d", kind, events)
		}
	}

	for _, state := range []core.TaskState{core.TaskQueued, core.TaskAwaiting, core.TaskParked} {
		stateTask := core.Task{ID: "close-" + string(state), Workspace: "demo", State: state, CreatedAt: time.Now().UTC()}
		if err := st.CreateTask(ctx, stateTask); err != nil {
			t.Fatal(err)
		}
		response := call(stateTask.ID, `{"reason":"state coverage"}`)
		if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"state":"closed"`) {
			t.Fatalf("close %s status=%d body=%s", state, response.Code, response.Body.String())
		}
	}

	for _, state := range []core.TaskState{core.TaskMerged, core.TaskClosed} {
		terminalTask := core.Task{ID: "close-terminal-" + string(state), Workspace: "demo", State: state, CreatedAt: time.Now().UTC()}
		if err := st.CreateTask(ctx, terminalTask); err != nil {
			t.Fatal(err)
		}
		response := call(terminalTask.ID, `{"reason":"must conflict"}`)
		if response.Code != http.StatusConflict {
			t.Fatalf("close %s status=%d body=%s", state, response.Code, response.Body.String())
		}
		terminalInterventions, _ := st.ListInterventions(ctx, terminalTask.ID)
		if len(terminalInterventions) != 0 {
			t.Fatalf("terminal %s cancellation mutated interventions=%+v", state, terminalInterventions)
		}
	}
}

func TestReviewEndpointRejectsLifecycleCancelAction(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "review-cannot-cancel", Workspace: "demo", State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review?workspace_id=demo", strings.NewReader(`{"action":"cancel","reason_code":"obsolete"}`))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("review cancel status=%d body=%s", response.Code, response.Body.String())
	}
	unchanged, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if unchanged.State != core.TaskAwaiting || len(interventions) != 0 {
		t.Fatalf("task=%+v interventions=%+v", unchanged, interventions)
	}
}

func TestReviewRoundRetryHTTPIsAuthorizedActionableAndIdempotent(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "retry-review-http", Workspace: "demo", Repo: "api", Branch: "conveyor/task-retry-review-http", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for seat, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderTimedOut} {
		id := fmt.Sprintf("%s-review-1-seat-%d", task.ID, seat+1)
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Time{}
		if state == core.WorkOrderTimedOut {
			deadline = time.Now().Add(-time.Minute)
		}
		createMemoryWorkOrderInState(t, st, ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview, State: state, ExecutionDeadline: deadline, ReviewRound: 1, ReviewSeat: seat + 1, LastFailureMessage: "worker retries exhausted"})
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 9, "head_sha": "head-9"})}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", WorkOrderQueueTimeout: time.Hour,
		Repos:     []config.Repo{{Name: "api", GitHub: "acme/api", Base: "main"}},
		Routing:   config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Harness: "codex", TimeoutText: "1h"}}},
		Harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}},
		Review:    config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-a", Harness: "codex"}, {Model: "gpt-b", Harness: "codex"}}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	service := &workorder.Service{Store: st, ConfigProvider: provider, ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
		return githubtrigger.ReviewTarget{Number: 9, HeadSHA: "head-9"}, nil
	}}
	server := NewServer(st)
	server.BearerToken, server.Workspace, server.ConfigProvider, server.WorkOrders = "token", "demo", provider, service

	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"needs_attention":true`) || !strings.Contains(activity.Body.String(), `"review_recovery"`) || !strings.Contains(activity.Body.String(), `"prior_round":1`) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	reviews := httptest.NewRecorder()
	server.Handler().ServeHTTP(reviews, httptest.NewRequest(http.MethodGet, "/v1/reviews?workspace_id=demo", nil))
	if reviews.Code != http.StatusOK || !strings.Contains(reviews.Body.String(), task.ID) || !strings.Contains(reviews.Body.String(), `"needs_attention":true`) {
		t.Fatalf("reviews status=%d body=%s", reviews.Code, reviews.Body.String())
	}
	call := func(token, requestID, reason string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review-round/retry?workspace_id=demo", strings.NewReader(fmt.Sprintf(`{"request_id":%q,"reason":%q}`, requestID, reason)))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("", "retry-1", "review worker timed out"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("token", "retry-1", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status=%d body=%s", response.Code, response.Body.String())
	}
	first := call("token", "retry-1", "review worker timed out")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"new_round":2`) || strings.Count(first.Body.String(), `"review_round":2`) != 2 {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if duplicate := call("token", "retry-1", "review worker timed out"); duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"new_round":2`) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if conflict := call("token", "retry-1", "different reason"); conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestInterruptedReviewRecoveryHTTPIsAuthorizedAtomicAndIdempotent(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{ID: "interrupted-review-http", Workspace: "demo", Repo: "api", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for seat, order := range []core.WorkOrder{
		{State: core.WorkOrderCompleted},
		{State: core.WorkOrderQueued, RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired},
	} {
		id := fmt.Sprintf("%s-review-1-seat-%d", task.ID, seat+1)
		order.ID, order.TaskID, order.JobID, order.Stage, order.ReviewRound, order.ReviewSeat = id, task.ID, id, core.StageReview, 1, seat+1
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		createMemoryWorkOrderInState(t, st, ctx, order)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace, server.ConfigProvider = "token", "demo", provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	call := func(token, requestID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review-round/recover?workspace_id=demo", strings.NewReader(fmt.Sprintf(`{"request_id":%q}`, requestID)))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("", "recover-1"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", response.Code)
	}
	first := call("token", "recover-1")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"review_round":1`) || !strings.Contains(first.Body.String(), `"recovered_orders"`) || !strings.Contains(first.Body.String(), `"retained_orders"`) {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if duplicate := call("token", "recover-1"); duplicate.Code != http.StatusOK || duplicate.Body.String() != first.Body.String() {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if conflict := call("token", "recover-2"); conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	if activity.Code != http.StatusOK || strings.Contains(activity.Body.String(), `"interrupted_review_recovery"`) {
		t.Fatalf("recovery action remained visible status=%d body=%s", activity.Code, activity.Body.String())
	}
}

func TestSetTaskHoldTogglesReservationWithAudit(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	ctx := store.WithWorkspace(t.Context(), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "hold-task", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/tasks/hold-task/hold", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if response := request(`{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing hold status=%d", response.Code)
	}
	response := request(`{"hold":true}`)
	var task core.Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); response.Code != http.StatusOK || err != nil || !task.Hold {
		t.Fatalf("set status=%d task=%+v err=%v", response.Code, task, err)
	}
	// Setting the current value is idempotent: no duplicate audit event.
	if response = request(`{"hold":true}`); response.Code != http.StatusOK {
		t.Fatalf("idempotent set status=%d", response.Code)
	}
	if response = request(`{"hold":false}`); response.Code != http.StatusOK {
		t.Fatalf("clear status=%d", response.Code)
	}
	events, err := st.ListEvents(ctx, "hold-task")
	if err != nil {
		t.Fatal(err)
	}
	sets, clears := 0, 0
	for _, event := range events {
		if event.Kind == "task.hold.set" {
			sets++
		}
		if event.Kind == "task.hold.cleared" {
			clears++
		}
	}
	if sets != 1 || clears != 1 {
		t.Fatalf("sets=%d clears=%d events=%+v", sets, clears, events)
	}
}

func TestTaskIntakeIsNotHealthGatedAndPersistsHold(t *testing.T) {
	st := store.NewMemory()
	now := time.Now().UTC()
	cfg := &config.Config{Workspace: "demo", Execution: config.ExecutionPolicy{DefaultMode: "auto", SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 1}, Harnesses: []config.Harness{{Name: "codex"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "codex"}, "review": {Harness: "codex", Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "api", Base: "main"}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider, Now: func() time.Time { return now }}
	server := NewServer(st)
	server.BearerToken, server.Workspace, server.ConfigProvider, server.Workers = "token", "demo", provider, workers
	server.GenerateTaskTitle = func(_ context.Context, task core.Task) (string, error) { return task.Body, nil }
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	// §21.31: no worker is enrolled, yet nothing is rejected or resolved to
	// another mode at intake — serviceability is advisory, orders queue openly.
	response := request(`{"body":"deprecated auto","repo":"api","mode":"auto"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("deprecated auto status=%d body=%s", response.Code, response.Body.String())
	}
	var task core.Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Hold || task.Mode != "" || task.PolicyVersion != 1 || !task.SpecApproval || !task.MergeApproval {
		t.Fatalf("deprecated auto task=%+v", task)
	}
	deprecatedEvents := func(id string) int {
		events, _ := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), id)
		count := 0
		for _, event := range events {
			if event.Kind == "task.mode.deprecated" {
				count++
			}
		}
		return count
	}
	if deprecatedEvents(task.ID) != 1 {
		t.Fatalf("expected deprecated-mode event for %s", task.ID)
	}

	response = request(`{"body":"deprecated manual","repo":"api","mode":"manual"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("deprecated manual status=%d body=%s", response.Code, response.Body.String())
	}
	var held core.Task
	_ = json.Unmarshal(response.Body.Bytes(), &held)
	if !held.Hold || held.Mode != "" || deprecatedEvents(held.ID) != 1 {
		t.Fatalf("deprecated manual task=%+v", held)
	}

	response = request(`{"body":"held with gates off","repo":"api","hold":true,"spec_approval":false,"merge_approval":false}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("hold status=%d body=%s", response.Code, response.Body.String())
	}
	var explicit core.Task
	_ = json.Unmarshal(response.Body.Bytes(), &explicit)
	if !explicit.Hold || explicit.SpecApproval || explicit.MergeApproval || deprecatedEvents(explicit.ID) != 0 {
		t.Fatalf("hold task=%+v", explicit)
	}

	if response := request(`{"body":"bad","repo":"api","mode":"turbo"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskIntakeSelectsAndFreezesExecutionSetup(t *testing.T) {
	st := store.NewMemory()
	settings := func(harness, model string) config.ContextualExecutionSettings {
		return config.ContextualExecutionSettings{
			ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
			Implementation: config.ImplementationSettings{Harness: harness, Model: model, ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h"},
		}
	}
	backend := config.ExecutionSetup{Name: "backend", ExecutionSettings: settings("codex", "gpt-backend"), Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Harness: "codex"}}}}
	frontend := config.ExecutionSetup{Name: "frontend", ExecutionSettings: settings("claude", "claude-ui"), Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "claude-review", Harness: "claude"}}}}
	cfg := &config.Config{Workspace: "demo", Execution: config.ExecutionPolicy{DefaultMode: "manual", SpecApproval: true, MergeApproval: true}, Setups: []config.ExecutionSetup{backend, frontend}, DefaultSetup: backend.Name, Repos: []config.Repo{{Name: "api", Base: "main"}}}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	server.GenerateTaskTitle = func(_ context.Context, task core.Task) (string, error) {
		if task.SetupContract.Name != frontend.Name {
			t.Fatalf("title generation setup=%+v", task.SetupContract)
		}
		return "Frozen frontend", nil
	}
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	response := request(`{"body":"ui","repo":"api","setup":"frontend","mode":"manual"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var task core.Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.SetupName != "frontend" || task.SetupContract.ExecutionSettings.Implementation.Harness != "claude" {
		t.Fatalf("task setup=%+v", task)
	}
	cfg.Setups[1].ExecutionSettings.Implementation.Harness = "changed"
	persisted, err := st.GetTask(store.WithWorkspace(t.Context(), "demo"), task.ID)
	if err != nil || persisted.SetupContract.ExecutionSettings.Implementation.Harness != "claude" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "unused", nil }
	if unknown := request(`{"body":"bad","repo":"api","setup":"missing","mode":"manual"}`); unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func TestCreateTaskRequiresBearerToken(t *testing.T) {
	st := store.NewMemory()
	created := make(chan string, 1)
	s := NewServer(st)
	s.Repos = []string{"api"}
	s.Workspace = "test"
	s.BearerToken = "secret-token"
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Fix it", nil }
	s.OnCreate = func(_ context.Context, id string) { created <- id }
	h := s.Handler()
	body := []byte(`{"body":"fix it","repo":"api"}`)

	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "wrong", header: "Bearer wrong-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
			req.Header.Set("Authorization", tc.header)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusCreated, res.Body.String())
	}
	select {
	case <-created:
	default:
		t.Fatal("authorized task was not dispatched")
	}

	tasks, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(tasks))
	}
}

func TestCreateTaskDependencyValidationReturnsStructuredError(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.Repos = []string{"api"}
	titleCalls := 0
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		titleCalls++
		return "unused", nil
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/tasks?workspace_id=demo",
		strings.NewReader(`{"body":"dependent work","repo":"api","depends_on":["missing"]}`),
	)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type=%q", contentType)
	}
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, response.Body.String())
	}
	if envelope.Error != "invalid_dependencies" || !strings.Contains(envelope.Message, "invalid depends_on") {
		t.Fatalf("error envelope=%+v", envelope)
	}
	if titleCalls != 0 {
		t.Fatalf("dependency validation called title generation %d times", titleCalls)
	}
}

func TestRemoveTaskDependencyRESTIsAuditedAndIdempotent(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	dependency := core.Task{ID: "unlink-dependency", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	dependent := core.Task{ID: "unlink-dependent", Workspace: "demo", Repo: "ui", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTaskWithDependencies(ctx, dependent, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	call := func(token, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete,
			"/v1/tasks/"+dependent.ID+"/dependencies/"+dependency.ID+"?workspace_id=demo",
			strings.NewReader(body))
		request.Header.Set("Authorization", token)
		request.Header.Set("X-Conveyor-Actor", "operator-1")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("", `{"reason":"manual resolution","request_id":"unlink-rest"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("Bearer token", `{"request_id":"unlink-rest"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status=%d body=%s", response.Code, response.Body.String())
	}
	response := call("Bearer token", `{"reason":"manual resolution","request_id":"unlink-rest"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("unlink status=%d body=%s", response.Code, response.Body.String())
	}
	var result store.DependencyRemovalResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Removed {
		t.Fatalf("unlink result=%+v err=%v", result, err)
	}
	retry := call("Bearer token", `{"reason":"manual resolution","request_id":"unlink-rest"}`)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"removed":false`) {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	events, err := st.ListEvents(ctx, dependent.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task.dependency_removed" {
			found = event.ActorID == "user:local-operator" && strings.Contains(string(event.Payload), `"reason":"manual resolution"`)
		}
	}
	if !found {
		t.Fatalf("audited removal event missing: %+v", events)
	}
}

func TestCreateTaskAlwaysGeneratesTitleBeforePersistenceAndRejectsTitleInput(t *testing.T) {
	st := store.NewMemory()
	s := NewServer(st)
	s.Repos = []string{"api"}
	s.Workspace = "demo"
	s.BearerToken = "token"
	generated := 0
	s.GenerateTaskTitle = func(_ context.Context, task core.Task) (string, error) {
		generated++
		if task.Body != "Describe the requested change" || task.Repo != "api" || task.Source != "dashboard" {
			t.Fatalf("title input = %+v", task)
		}
		return "Generated task title", nil
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"body":"Describe the requested change","repo":"api","source":"dashboard","hold":true,"spec_approval":false,"merge_approval":true}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var task core.Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if generated != 1 || task.Title != "Generated task title" || task.Body != "Describe the requested change" || task.Mode != "" || !task.Hold || task.SpecApproval || !task.MergeApproval {
		t.Fatalf("generated=%d task=%+v", generated, task)
	}
	persisted, err := st.GetTask(store.WithWorkspace(t.Context(), "demo"), task.ID)
	if err != nil || persisted.Title != "Generated task title" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}

	explicit := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"title":"Keep this title","body":"Other context","repo":"api"}`))
	explicit.Header.Set("Authorization", "Bearer token")
	explicitResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(explicitResponse, explicit)
	if explicitResponse.Code != http.StatusBadRequest || generated != 1 || !strings.Contains(explicitResponse.Body.String(), "must not be supplied") {
		t.Fatalf("explicit status=%d body=%s generated=%d", explicitResponse.Code, explicitResponse.Body.String(), generated)
	}
}

func TestCreateTaskTitleGenerationFailsClosed(t *testing.T) {
	st := store.NewMemory()
	s := NewServer(st)
	s.Repos, s.Workspace, s.BearerToken = []string{"api"}, "demo", "token"
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "", fmt.Errorf("AI unavailable") }
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"body":"Needs a generated title","repo":"api"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "generate task title") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	tasks, err := st.ListTasks(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

func TestGitHubLifecycleAppearsInTaskReadsButNotRetiredFeatureTree(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "github-visible", Workspace: "demo", Repo: "api", Title: "Visible lifecycle", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	feature := core.Feature{ID: "feature-visible", Workspace: "demo", Name: "Lifecycle"}
	if err := st.CreateFeature(ctx, feature); err != nil {
		t.Fatal(err)
	}
	if err := st.AssignTaskFeature(ctx, task.ID, feature.ID); err != nil {
		t.Fatal(err)
	}
	lifecycle := core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/api", SpecVersion: 1}
	if err := st.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	lifecycle.State = core.GitHubPublicationPublished
	lifecycle.IssueNumber = 42
	lifecycle.IssueURL = "https://github.com/acme/api/issues/42"
	if err := st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	for _, path := range []string{"/v1/tasks/github-visible", "/v1/tasks/github-visible/activity"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), lifecycle.IssueURL) {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	// The Requirements read model no longer leaks task.feature_id or the
	// retired feature hierarchy. Requirement/blueprint lineage is deposited by
	// planning and rendered from living documents instead.
	request := httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("requirements status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRedispatchRequiresInactiveTaskAndAuth(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	task := core.Task{ID: "task-1", State: core.TaskAwaiting, NextStage: core.StageImplement}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan string, 1)
	s := NewServer(st)
	s.BearerToken = "secret-token"
	s.OnCreate = func(_ context.Context, id string) { dispatched <- id }
	h := s.Handler()

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/tasks/task-1/redispatch", bytes.NewReader([]byte(`{}`))))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/task-1/redispatch", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer secret-token")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}
	updated, err := st.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != core.TaskQueued {
		t.Fatalf("state = %s", updated.State)
	}
	select {
	case id := <-dispatched:
		if id != "task-1" {
			t.Fatalf("dispatched %q", id)
		}
	default:
		t.Fatal("task was not enqueued")
	}
}

func TestRedispatchRepairsAlreadyQueuedTask(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "queued-task", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan string, 1)
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnCreate = func(_ context.Context, id string) { dispatched <- id }
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/queued-task/redispatch", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case id := <-dispatched:
		if id != "queued-task" {
			t.Fatalf("dispatched %q", id)
		}
	default:
		t.Fatal("queued task was not re-enqueued")
	}
}

func TestRedispatchMapsEnqueueRaceToNotFound(t *testing.T) {
	t.Parallel()
	base := store.NewMemory()
	if err := base.CreateTask(t.Context(), core.Task{ID: "raced-task", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(notFoundRaceStore{Store: base, failEnsure: true})
	server.BearerToken = "token"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/raced-task/redispatch", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRedispatchRecoversParkedTaskAtRecordedStage(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "halted", State: core.TaskParked, RecoveryStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan string, 1)
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnCreate = func(_ context.Context, id string) { dispatched <- id }
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/halted/redispatch", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	persisted, _ := st.GetTask(context.Background(), "halted")
	if persisted.State != core.TaskQueued || persisted.NextStage != core.StageImplement || persisted.RecoveryStage != "" {
		t.Fatalf("halted task was not recovered: %+v", persisted)
	}
	select {
	case id := <-dispatched:
		if id != "halted" {
			t.Fatalf("dispatched %q", id)
		}
	default:
		t.Fatal("recovered task was not enqueued")
	}
}

func TestRedispatchReturnsConflictForTerminalTaskTransition(t *testing.T) {
	t.Parallel()
	for _, state := range []core.TaskState{core.TaskMerged, core.TaskClosed} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			st := store.NewMemory()
			id := "terminal-" + string(state)
			if err := st.CreateTask(context.Background(), core.Task{ID: id, State: state, NextStage: core.StageImplement}); err != nil {
				t.Fatal(err)
			}
			s := NewServer(st)
			s.BearerToken = "token"
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+id+"/redispatch", bytes.NewReader([]byte(`{}`)))
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestReadEndpointsDoNotRequireToken(t *testing.T) {
	s := NewServer(store.NewMemory())
	s.BearerToken = "secret-token"
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
}

func TestPendingProposalsProjectionAttentionAndTaskWarning(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "proposal-origin", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	attentionTask := core.Task{ID: "attention-task", Workspace: "demo", State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
	for _, candidate := range []core.Task{task, attentionTask} {
		if err := st.CreateTask(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	siblingCtx := store.WithWorkspace(t.Context(), "sibling")
	if err := st.CreateTask(siblingCtx, core.Task{ID: "sibling-attention", Workspace: "sibling", State: core.TaskParked, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-pending", Title: "Pending design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-pending", Title: "Pending requirement"}, core.RequirementVersion{
		Content: "Pending requirement", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Surface pending authority."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Pending decision", Context: "Operator attention", AlternativesRejected: "Hidden proposals", Origin: core.DecisionOriginImplementation, OriginTaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: task.ID + "-implement-1", TaskID: task.ID, JobID: task.ID + "-implement-1", Stage: core.StageImplement, State: core.WorkOrderQueued}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "proposal-session", ClientToken: "token", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var projection pendingProposalsResponse
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Items) != 3 || projection.Attention.TaskCount != 2 || projection.Attention.PendingProposalCount != 3 || projection.Attention.Total != 5 {
		t.Fatalf("projection=%+v", projection)
	}
	for _, item := range projection.Items {
		if item.ID == "" || item.Title == "" || item.Tier == "" || item.OriginType == "" || item.ProposedAt.IsZero() || item.AgeSeconds < 0 {
			t.Fatalf("incomplete projection item=%+v", item)
		}
	}
	for _, forbidden := range []string{`"href"`, `"route"`, `"hash"`} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("projection leaked UI navigation field %s: %s", forbidden, response.Body.String())
		}
	}
	detail := httptest.NewRecorder()
	server.Handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"pending_authority":true`) || !strings.Contains(detail.Body.String(), `"needs_attention":true`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/activity?workspace_id=demo", nil))
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"id":"`+task.ID+`"`) || !strings.Contains(activity.Body.String(), `"needs_attention":true`) || !strings.Contains(activity.Body.String(), `"pending_authority":true`) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	reviews := httptest.NewRecorder()
	server.Handler().ServeHTTP(reviews, httptest.NewRequest(http.MethodGet, "/v1/reviews?workspace_id=demo", nil))
	if reviews.Code != http.StatusOK || !strings.Contains(reviews.Body.String(), `"id":"`+task.ID+`"`) || !strings.Contains(reviews.Body.String(), `"needs_attention":true`) || !strings.Contains(reviews.Body.String(), `"pending_authority":true`) {
		t.Fatalf("reviews status=%d body=%s", reviews.Code, reviews.Body.String())
	}
	operations := httptest.NewRecorder()
	server.Handler().ServeHTTP(operations, httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id=demo", nil))
	if operations.Code != http.StatusOK || !strings.Contains(operations.Body.String(), `"id":"`+task.ID+`"`) || !strings.Contains(operations.Body.String(), `"needs_attention":true`) {
		t.Fatalf("task operations status=%d body=%s", operations.Code, operations.Body.String())
	}

	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DismissDecision(store.WithActor(ctx, store.Actor{ID: "operator", Role: "operator"}), decision.ID); err != nil {
		t.Fatal(err)
	}
	detail = httptest.NewRecorder()
	server.Handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?workspace_id=demo", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"pending_authority":false`) {
		t.Fatalf("resolved detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	activity = httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/activity?workspace_id=demo", nil))
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"id":"`+task.ID+`"`) || !strings.Contains(activity.Body.String(), `"pending_authority":false`) {
		t.Fatalf("resolved activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	reviews = httptest.NewRecorder()
	server.Handler().ServeHTTP(reviews, httptest.NewRequest(http.MethodGet, "/v1/reviews?workspace_id=demo", nil))
	if reviews.Code != http.StatusOK || strings.Contains(reviews.Body.String(), `"id":"`+task.ID+`"`) {
		t.Fatalf("resolved reviews status=%d body=%s", reviews.Code, reviews.Body.String())
	}
	remaining, err := st.ListPendingProposals(ctx)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Items) != 0 || projection.Attention.TaskCount != 1 || projection.Attention.PendingProposalCount != 0 || projection.Attention.Total != 1 {
		t.Fatalf("zero-proposal attention changed task count: %+v", projection)
	}
}

func TestActivityIncludesAllTasksWhileReviewsStayFiltered(t *testing.T) {
	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "running", State: core.TaskRunning, CreatedAt: time.Now()},
		{ID: "awaiting", State: core.TaskAwaiting, CreatedAt: time.Now()},
	} {
		if err := st.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewServer(st).Handler()
	activity := httptest.NewRecorder()
	handler.ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/v1/activity", nil))
	if activity.Code != http.StatusOK || !bytes.Contains(activity.Body.Bytes(), []byte(`"id":"running"`)) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	reviews := httptest.NewRecorder()
	handler.ServeHTTP(reviews, httptest.NewRequest(http.MethodGet, "/v1/reviews", nil))
	if reviews.Code != http.StatusOK || bytes.Contains(reviews.Body.Bytes(), []byte(`"id":"running"`)) {
		t.Fatalf("reviews status=%d body=%s", reviews.Code, reviews.Body.String())
	}
}

func TestActivityUsesHumanGateAfterSubmittedOrderRecoversFromRetries(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "submitted-after-retries", Workspace: "demo", State: core.TaskAwaiting,
		NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	job := core.Job{
		ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement,
		State: core.JobDone,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	const failure = "harness exited before completing work order"
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement,
		AutomaticRetryCount: 3, LastFailureMessage: failure,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "recovered-session", ClientToken: "test-token", WorkerID: "worker",
		Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{
		TaskID: task.ID, JobID: job.ID, Kind: "work_order.child_failed",
		Payload: core.JSONPayload(map[string]any{"reason": failure, "automatic_retry_count": 3}),
		At:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	handler := NewServer(st).Handler()
	for _, path := range []string{
		"/v1/activity",
		"/v1/tasks/" + task.ID + "/activity",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, `"state":"awaiting_human"`) || !strings.Contains(body, `"needs_attention":true`) {
			t.Fatalf("%s omitted current human gate: %s", path, body)
		}
		if strings.Contains(body, `"stalled"`) {
			t.Fatalf("%s retained stale stalled projection: %s", path, body)
		}
		if strings.Contains(path, "/tasks/") && (!strings.Contains(body, `"kind":"work_order.child_failed"`) || !strings.Contains(body, failure)) {
			t.Fatalf("%s omitted historical failure event: %s", path, body)
		}
	}
}

func TestTaskActivityExposesLatestAgentProgressWithLabelAndTimestamp(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "latest-agent-activity", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "activity-session", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st}
	if _, err := service.Progress(ctx, job.ID, "activity-session", "Running focused worker tests"); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer(st).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/activity", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"last_agent_activity_label":"Progress: Running focused worker tests"`) ||
		!strings.Contains(body, `"last_agent_activity_at":`) ||
		!strings.Contains(body, `"kind":"work_order.progress_reported"`) {
		t.Fatalf("latest agent activity missing: %s", body)
	}
}

func TestAgentActivityLabelTruncatesProgressOnRuneBoundary(t *testing.T) {
	message := strings.Repeat("a", 119) + "é" + " after the boundary"
	label := agentActivityLabel(core.Event{
		Kind:    "work_order.progress_reported",
		Payload: core.JSONPayload(map[string]any{"message": message}),
	})
	if !utf8.ValidString(label) {
		t.Fatalf("label is invalid UTF-8: %q", label)
	}
	want := "Progress: " + strings.Repeat("a", 119) + "…"
	if label != want {
		t.Fatalf("label=%q want=%q", label, want)
	}
}

func TestActivitySurfacesReviewClaimsWithoutTerminalVerdicts(t *testing.T) {
	now := time.Now().UTC()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "missing-verdicts", State: core.TaskRunning, CreatedAt: now.Add(-time.Hour)}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{
		{ID: "review-current-job", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning},
		{ID: "review-expired-job", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	current := core.WorkOrder{
		ID: "review-current", TaskID: task.ID, JobID: "review-current-job", Stage: core.StageReview,
		State: core.WorkOrderClaimed, ReviewRound: 1, ReviewSeat: 1,
		ExecutionStartedAt: now.Add(-2 * time.Minute), LeaseExpiresAt: now.Add(5 * time.Minute),
	}
	expiredClaim := core.WorkOrder{
		ID: "review-expired", TaskID: task.ID, JobID: "review-expired-job", Stage: core.StageReview,
		State: core.WorkOrderClaimed, ReviewRound: 1, ReviewSeat: 2,
		ExecutionStartedAt: now.Add(-10 * time.Minute), LeaseExpiresAt: now.Add(-5 * time.Minute),
	}
	expired := expiredClaim
	expired.State, expired.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	createMemoryWorkOrderInState(t, st, ctx, current)
	createMemoryWorkOrderInState(t, st, ctx, expired)
	if err := st.AppendEvent(ctx, core.Event{
		TaskID: task.ID, JobID: expired.JobID, Kind: "work_order.claimed",
		Payload: core.JSONPayload(expiredClaim), At: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(st).Handler()
	for _, path := range []string{"/v1/activity", "/v1/tasks/missing-verdicts/activity"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		for _, expected := range []string{
			`"status":"claimed_without_verdict"`, `"work_order_id":"review-current"`, `"review_seat":1`,
			`"status":"expired_without_verdict"`, `"work_order_id":"review-expired"`, `"review_seat":2`,
		} {
			if !strings.Contains(response.Body.String(), expected) {
				t.Fatalf("%s missing %s: %s", path, expected, response.Body.String())
			}
		}
	}
}

func TestListJobsOmitsInProcessCostAndKeepsWorkerCost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "job-cost-wire", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	reportedCost := 1.25
	for _, job := range []core.Job{
		{ID: "in-process", TaskID: task.ID, Runner: "in-process", TokensIn: 17, TokensOut: 3, State: core.JobDone},
		{ID: "worker", TaskID: task.ID, Runner: "external", CostUSD: &reportedCost, State: core.JobDone},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	NewServer(st).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/jobs", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var jobs []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0]["id"] != "in-process" || jobs[0]["cost_usd"] != nil || jobs[0]["tokens_in"] != float64(17) || jobs[0]["tokens_out"] != float64(3) {
		t.Fatalf("in-process wire job = %+v", jobs)
	}
	if jobs[1]["id"] != "worker" || jobs[1]["cost_usd"] != reportedCost {
		t.Fatalf("worker wire job = %+v", jobs)
	}
}

func TestDashboardIsEmbedded(t *testing.T) {
	response := httptest.NewRecorder()
	NewServer(store.NewMemory()).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tasks/example", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("Conveyor")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskActivityIncludesLatestSpecForHumanGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "spec-gate", State: core.TaskAwaiting, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: task.ID, Content: "# Proposed change\n\nReview this exact text.", AcceptanceCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer(st).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/spec-gate/activity", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, expected := range [][]byte{
		[]byte(`"needs_attention":true`),
		[]byte(`"work_orders":[]`),
		[]byte(`"checkout_available":true`),
		[]byte(`"checkout_command":"conveyor checkout spec-gate"`),
		[]byte(`"checkout_guidance":"Creates or reuses the clean, task-dedicated worktree without switching the primary checkout."`),
		[]byte(`"spec":{"task_id":"spec-gate"`),
		[]byte(`"version":` + fmt.Sprint(created.Version)),
		[]byte(`"content":"# Proposed change\n\nReview this exact text."`),
		[]byte(`"approved":false`),
	} {
		if !bytes.Contains(response.Body.Bytes(), expected) {
			t.Fatalf("activity body does not contain %s: %s", expected, response.Body.String())
		}
	}
}

func TestTaskActivitySurfacesAttachmentsExcludingAuditTranscripts(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "attach-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	// Operator-supplied attachment (task_context role, defaulted from empty).
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "design.png", ContentType: "image/png", TaskID: task.ID}, []byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	// Conveyor-generated audit transcript must never appear as an attachment.
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "triage-transcript.json", ContentType: "application/json", TaskID: task.ID, Role: core.ArtifactRoleGeneratedAudit}, []byte("{}")); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/attach-task/activity?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, expected := range [][]byte{
		[]byte(`"attachments":[`),
		[]byte(`"name":"design.png"`),
		[]byte(`"content_type":"image/png"`),
		[]byte(`"download_url":"/v1/artifacts/`),
	} {
		if !bytes.Contains(response.Body.Bytes(), expected) {
			t.Fatalf("activity body does not contain %s: %s", expected, response.Body.String())
		}
	}
	if bytes.Contains(response.Body.Bytes(), []byte("triage-transcript.json")) {
		t.Fatalf("audit transcript leaked into attachments: %s", response.Body.String())
	}
}

func TestTaskActivityOmitsAttachmentsWhenNone(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "bare-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/bare-task/activity?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"attachments":[]`)) {
		t.Fatalf("expected empty attachments array: %s", response.Body.String())
	}
}

func TestTaskActivityFailsClosedWhenMergeReadinessCannotBeResolved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "readiness-error", State: core.TaskApproved, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		return dispatch.MergeReadiness{}, fmt.Errorf("github unavailable")
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/readiness-error/activity", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "resolve merge readiness") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskActivityEnablesCheckoutAfterPushedBranchEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "pushed-task", State: core.TaskAwaiting, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{
		TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"url": "https://example.test/pr/1"}),
	}); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer(st).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/pushed-task/activity", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, expected := range [][]byte{
		[]byte(`"checkout_available":true`),
		[]byte(`"checkout_command":"conveyor checkout pushed-task"`),
	} {
		if !bytes.Contains(response.Body.Bytes(), expected) {
			t.Fatalf("activity body does not contain %s: %s", expected, response.Body.String())
		}
	}
}

func TestPullToLocalProvidesDedicatedWorktreeCommandBeforePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "assigned-only", State: core.TaskAwaiting, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken = "token"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/assigned-only/review", bytes.NewReader([]byte(`{"action":"pull_to_local","reason_code":"needs-human"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !bytes.Contains(response.Body.Bytes(), []byte(`"checkout_command":"conveyor checkout assigned-only"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	interventions, err := st.ListInterventions(ctx, "assigned-only")
	if err != nil {
		t.Fatal(err)
	}
	if len(interventions) != 1 || interventions[0].Action != core.InterventionPull {
		t.Fatalf("pull-to-local interventions = %+v", interventions)
	}
}

func TestReviewMapsInterventionRacesToNotFound(t *testing.T) {
	t.Run("ordinary intervention", func(t *testing.T) {
		base := store.NewMemory()
		if err := base.CreateTask(t.Context(), core.Task{ID: "raced-review", State: core.TaskAwaiting, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		server := NewServer(notFoundRaceStore{Store: base, failIntervention: true})
		server.BearerToken = "token"
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/raced-review/review", bytes.NewBufferString(`{"action":"pull_to_local","reason_code":"race"}`))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("plan revision intervention", func(t *testing.T) {
		_, base, server, taskID, _ := newPlanRevisionReviewServer(t)
		server.Store = notFoundRaceStore{Store: base, failIntervention: true}
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"redirect","reason_code":"plan-revision-approved","comment":"Proceed."}`))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

func TestReviewRedirectRecordsReasonAndRequeues(t *testing.T) {
	st := store.NewMemory()
	task := core.Task{ID: "task-review", Workspace: "test", Repo: "api", State: core.TaskAwaiting, RecoveryStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	requeued := make(chan string, 1)
	s := NewServer(st)
	s.BearerToken = "secret-token"
	s.OnIntervention = func(ctx context.Context, task core.Task, _ core.Job, intervention core.Intervention) error {
		if intervention.Action != core.InterventionRedirect {
			return nil
		}
		if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: task.RecoveryStage, ProjectStages: true}); err != nil {
			return err
		}
		requeued <- task.ID
		return nil
	}
	h := s.Handler()

	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/task-review/review", bytes.NewReader([]byte(`{
  "action": "redirect",
  "reason_code": "spec-wrong",
  "comment": "Clarify the boundary"
}`)))
	request.Header.Set("Authorization", "Bearer secret-token")
	request.Header.Set("X-Conveyor-Actor", "kidus")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case id := <-requeued:
		if id != task.ID {
			t.Fatalf("requeued = %q", id)
		}
	default:
		t.Fatal("redirect did not requeue task")
	}
	updated, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != core.TaskQueued {
		t.Fatalf("state = %s", updated.State)
	}
	interventions, err := st.ListInterventions(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(interventions) != 1 || interventions[0].ReasonCode != "spec-wrong" || interventions[0].ActorID != "user:local-operator" {
		t.Fatalf("interventions = %+v", interventions)
	}
	events, err := st.ListEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[1].Kind != "task.state_changed" || events[2].Kind != "pipeline.transition_decided" || events[3].Kind != "intervention.redirect" {
		t.Fatalf("events = %+v", events)
	}
}

func TestPlanRevisionReviewDecisionsUseFixedAuditedContract(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
		wantTask   core.TaskState
		wantOrder  core.WorkOrderState
		wantReason string
		wantDirect string
	}{
		{name: "approve revision", body: `{"action":"redirect","reason_code":"plan-revision-approved","comment":"Keep compatibility."}`, wantStatus: http.StatusAccepted, wantTask: core.TaskQueued, wantOrder: core.WorkOrderCancelled, wantReason: dispatch.PlanRevisionApprovedReasonCode},
		{name: "decline with direction", body: `{"action":"redirect","reason_code":"plan-revision-declined","comment":"Continue with plan v1."}`, wantStatus: http.StatusAccepted, wantTask: core.TaskQueued, wantOrder: core.WorkOrderQueued, wantReason: dispatch.PlanRevisionDeclinedReasonCode, wantDirect: "Continue with plan v1."},
		{name: "reject", body: `{"action":"reject","reason_code":"plan-revision-rejected"}`, wantStatus: http.StatusAccepted, wantTask: core.TaskClosed, wantOrder: core.WorkOrderQueued, wantReason: dispatch.PlanRevisionRejectedReasonCode},
		{name: "decline requires direction", body: `{"action":"redirect","reason_code":"plan-revision-declined","comment":" "}`, wantStatus: http.StatusBadRequest, wantTask: core.TaskAwaiting, wantOrder: core.WorkOrderQueued},
		{name: "generic redirect is rejected", body: `{"action":"redirect","reason_code":"spec-wrong"}`, wantStatus: http.StatusBadRequest, wantTask: core.TaskAwaiting, wantOrder: core.WorkOrderQueued},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, server, taskID, orderID := newPlanRevisionReviewServer(t)
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review", bytes.NewBufferString(test.body))
			request.Header.Set("Authorization", "Bearer token")
			request.Header.Set("X-Conveyor-Actor", "operator")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			task, _ := st.GetTask(ctx, taskID)
			order, _ := st.GetWorkOrder(ctx, orderID)
			if task.State != test.wantTask || order.State != test.wantOrder || order.OperatorDirection != test.wantDirect {
				t.Fatalf("task=%+v order=%+v", task, order)
			}
			interventions, _ := st.ListInterventions(ctx, taskID)
			if test.wantStatus == http.StatusBadRequest {
				if len(interventions) != 0 {
					t.Fatalf("rejected decision recorded interventions=%+v", interventions)
				}
				return
			}
			if len(interventions) != 1 || interventions[0].ReasonCode != test.wantReason || interventions[0].ActorID != "user:local-operator" {
				t.Fatalf("interventions=%+v", interventions)
			}
		})
	}
}

func TestPlanRevisionApprovalIsDurableBeforeImmediatePlanClaim(t *testing.T) {
	_, st, server, taskID, _ := newPlanRevisionReviewServer(t)
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec": {Model: "planner", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}

	var immediate workorder.Context
	server.OnIntervention = func(ctx context.Context, task core.Task, latest core.Job, intervention core.Intervention) error {
		interventions, listErr := st.ListInterventions(ctx, task.ID)
		if listErr != nil || len(interventions) != 1 || interventions[0].ReasonCode != dispatch.PlanRevisionApprovedReasonCode {
			return fmt.Errorf("approval was not durable before dispatch: interventions=%+v err=%v", interventions, listErr)
		}
		if err := dispatcher.HandleIntervention(ctx, task, latest, intervention); err != nil {
			return err
		}
		if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
			return err
		}
		orders, err := st.ListTaskWorkOrders(ctx, task.ID)
		if err != nil {
			return err
		}
		planOrder := orders[len(orders)-1]
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, planOrder.ID, core.WorkOrderClaim{SessionID: "immediate-plan-session", ClientToken: "plan-token", WorkerID: "plan-worker", Agent: "codex", Model: "planner", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			return err
		}
		immediate, err = service.Get(ctx, claimed.ID, claimed.SessionID)
		return err
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"redirect","reason_code":"plan-revision-approved","comment":"Keep compatibility."}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if immediate.PlanRevision == nil || immediate.PlanRevision.ContestedPlanVersion != 1 || immediate.PlanRevision.Rationale != "the API changed" || immediate.PlanRevision.OperatorDirection != "Keep compatibility." {
		t.Fatalf("immediate plan-revision context=%+v", immediate.PlanRevision)
	}
	for _, required := range []string{"# Plan revision context", "the API changed", "# Operator direction\n\nKeep compatibility."} {
		if !strings.Contains(immediate.RolePrompt, required) {
			t.Errorf("immediate plan prompt missing %q: %s", required, immediate.RolePrompt)
		}
	}
}

func newPlanRevisionReviewServer(t *testing.T) (context.Context, store.Store, *Server, string, string) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	taskID := "plan-revision-review-" + core.NewTaskID()
	now := time.Now().UTC()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
		t.Fatal(err)
	}
	orderID := taskID + "-implement-1"
	job := core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", WorkerID: "worker", Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		return st.RequestPlanRevisionCommand(ctx, lease, orderID, core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: claimed.ClaimantID, SessionID: claimed.SessionID}, "the API changed")
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "token"
	server.OnIntervention = dispatcher.HandleIntervention
	return ctx, st, server, taskID, orderID
}

func TestReviewApprovalWithoutReviewedHeadConflictsBeforeIntervention(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "missing-reviewed-head", Workspace: "demo", Repo: "api", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, &config.Config{Workspace: "demo"}, nil)
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnIntervention = d.HandleIntervention
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "reviewed head SHA is unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != gate.Task.State {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 0 {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
}

func TestApprovedTaskRedirectAndRejectConflictWithoutIntervention(t *testing.T) {
	for _, action := range []core.InterventionAction{core.InterventionRedirect, core.InterventionReject} {
		t.Run(string(action), func(t *testing.T) {
			st := store.NewMemory()
			task := core.Task{ID: "approved-" + string(action), State: core.TaskApproved, CreatedAt: time.Now()}
			if err := st.CreateTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			s := NewServer(st)
			s.BearerToken = "token"
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", bytes.NewReader([]byte(fmt.Sprintf(`{"action":%q,"reason_code":"operator-decision"}`, action))))
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			interventions, err := st.ListInterventions(t.Context(), task.ID)
			if err != nil || len(interventions) != 0 {
				t.Fatalf("interventions=%+v err=%v", interventions, err)
			}
		})
	}
}

func TestConcurrentReviewApprovalReturnsConflictForLoser(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "concurrent-approval", Workspace: "demo", Repo: "api", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, &config.Config{Workspace: "demo"}, nil)
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnIntervention = d.HandleIntervention
	handler := s.Handler()
	responses := make(chan int, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response.Code
		}()
	}
	close(start)
	statuses := []int{<-responses, <-responses}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusAccepted, http.StatusConflict}) {
		t.Fatalf("statuses=%v", statuses)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
}

func TestReviewRequiresReasonCodeAndHumanGate(t *testing.T) {
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "running", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(context.Background(), core.Task{ID: "parked", State: core.TaskParked, RecoveryStage: core.StageTriage, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(st)
	s.BearerToken = "token"
	h := s.Handler()

	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/running/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("running task status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/parked/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("parked task status = %d", response.Code)
	}
	parkedInterventions, err := st.ListInterventions(context.Background(), "parked")
	if err != nil || len(parkedInterventions) != 0 {
		t.Fatalf("parked interventions=%+v err=%v", parkedInterventions, err)
	}

	if _, err := taskops.New(st).Perform(context.Background(), "running", taskops.Command{Kind: core.TaskJobFail}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/running/review", bytes.NewReader([]byte(`{"action":"approve"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status = %d", response.Code)
	}

	if _, err := taskops.New(st).Perform(context.Background(), "running", taskops.Command{Kind: core.TaskInterventionApproveReview}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/running/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "merge operation") {
		t.Fatalf("approved review status=%d body=%s", response.Code, response.Body.String())
	}
	interventions, err := st.ListInterventions(context.Background(), "running")
	if err != nil || len(interventions) != 0 {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
}

func TestMergeTaskRequiresAuthAndConfirmedMergedState(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.CreateTask(ctx, core.Task{ID: "approved", State: core.TaskApproved, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(st)
	s.BearerToken = "token"
	mergeCalls := 0
	s.OnMerge = func(ctx context.Context, task core.Task) error {
		mergeCalls++
		if task.State == core.TaskApproved {
			_, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskMergeConfirm, ProjectStages: true})
			return err
		}
		return nil
	}
	h := s.Handler()

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/tasks/approved/merge", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/approved/merge", nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	current, err := st.GetTask(ctx, "approved")
	if err != nil || current.State != core.TaskMerged || mergeCalls != 2 {
		t.Fatalf("task=%+v mergeCalls=%d err=%v", current, mergeCalls, err)
	}
}

func TestMergeTaskDoesNotReportUnconfirmedSuccess(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "approved", State: core.TaskApproved}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnMerge = func(context.Context, core.Task) error { return nil }
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/approved/merge", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "not confirmed") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type observedStore struct {
	store.Store
	listTasksCalls           int
	listWorkOrdersCalls      int
	listPendingCalls         int
	pendingProjectionCalls   int
	listJobsCalls            int
	listEventsCalls          int
	listInterventionsCalls   int
	listActivityMarkersCalls int
	getLatestJobCalls        int
	listEventsAfterCalls     int
	afterHook                func()
}

func (s *observedStore) ListTasks(ctx context.Context) ([]core.Task, error) {
	s.listTasksCalls++
	return s.Store.ListTasks(ctx)
}

func (s *observedStore) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	s.listWorkOrdersCalls++
	return s.Store.ListWorkOrders(ctx)
}

func (s *observedStore) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	s.listPendingCalls++
	return s.Store.ListPendingProposals(ctx)
}

func (s *observedStore) PendingProposalsProjection(ctx context.Context) (store.PendingProposalsProjection, error) {
	s.pendingProjectionCalls++
	return s.Store.PendingProposalsProjection(ctx)
}

func (s *observedStore) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	s.listJobsCalls++
	return s.Store.ListJobs(ctx, taskID)
}

func (s *observedStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	s.listEventsCalls++
	return s.Store.ListEvents(ctx, taskID)
}

func (s *observedStore) ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error) {
	s.listInterventionsCalls++
	return s.Store.ListInterventions(ctx, taskID)
}

func (s *observedStore) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	s.listActivityMarkersCalls++
	return s.Store.ListActivityMarkers(ctx)
}

func (s *observedStore) GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error) {
	s.getLatestJobCalls++
	return s.Store.GetLatestJob(ctx, taskID)
}

func (s *observedStore) ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error) {
	s.listEventsAfterCalls++
	events, err := s.Store.ListEventsAfter(ctx, taskID, afterID)
	if s.afterHook != nil {
		s.afterHook()
	}
	return events, err
}

func TestPendingProposalsEndpointUsesOnlyBoundedStoreProjection(t *testing.T) {
	base := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	if err := base.CreateTask(ctx, core.Task{ID: "attention", Workspace: "demo", State: core.TaskAwaiting, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{Store: base}
	server := NewServer(observed)
	server.Workspace = "demo"
	server.BearerToken = "token"
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil)
	request.Header.Set("Authorization", "Bearer token")
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if observed.pendingProjectionCalls != 1 || observed.listTasksCalls != 0 || observed.listActivityMarkersCalls != 0 || observed.listWorkOrdersCalls != 0 || observed.listPendingCalls != 0 {
		t.Fatalf("projection=%d tasks=%d markers=%d orders=%d proposals=%d", observed.pendingProjectionCalls,
			observed.listTasksCalls, observed.listActivityMarkersCalls, observed.listWorkOrdersCalls, observed.listPendingCalls)
	}
}

func TestActivityIndexAvoidsPerTaskHistoryQueries(t *testing.T) {
	base := store.NewMemory()
	for _, id := range []string{"one", "two", "three"} {
		if err := base.CreateTask(context.Background(), core.Task{ID: id, State: core.TaskQueued}); err != nil {
			t.Fatal(err)
		}
	}
	observed := &observedStore{Store: base}
	response := httptest.NewRecorder()
	NewServer(observed).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/activity", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if observed.listActivityMarkersCalls != 1 || observed.listJobsCalls != 0 || observed.listEventsCalls != 0 || observed.listInterventionsCalls != 0 {
		t.Fatalf("activity query calls = markers:%d jobs:%d events:%d interventions:%d",
			observed.listActivityMarkersCalls, observed.listJobsCalls, observed.listEventsCalls, observed.listInterventionsCalls)
	}
}

func TestTaskActivityLoadsOneHistoryAndOmitsRunningEnd(t *testing.T) {
	base := store.NewMemory()
	task := core.Task{ID: "running-detail", State: core.TaskRunning}
	if err := base.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateJob(context.Background(), core.Job{
		ID: "job-running", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning,
	}); err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{Store: base}
	response := httptest.NewRecorder()
	NewServer(observed).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/running-detail/activity", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"ended_at"`)) {
		t.Fatalf("running detail contains ended_at: %s", response.Body.String())
	}
	if observed.listJobsCalls != 1 || observed.listEventsCalls != 1 || observed.listInterventionsCalls != 1 {
		t.Fatalf("detail query calls = jobs:%d events:%d interventions:%d",
			observed.listJobsCalls, observed.listEventsCalls, observed.listInterventionsCalls)
	}
}

func TestTaskActivityScopesWorkerStatusToActionableTaskOrders(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	for _, task := range []core.Task{
		{ID: "queued-auto", Workspace: "demo", Mode: core.TaskModeAuto, State: core.TaskQueued},
		{ID: "awaiting-auto", Workspace: "demo", Mode: core.TaskModeAuto, State: core.TaskAwaiting},
		{ID: "approved-auto", Workspace: "demo", Mode: core.TaskModeAuto, State: core.TaskApproved},
		{ID: "merged-auto", Workspace: "demo", Mode: core.TaskModeAuto, State: core.TaskMerged},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, job := range []core.Job{
		{ID: "queued-auto-implement-1", TaskID: "queued-auto", Stage: core.StageImplement, State: core.JobPending},
		{ID: "awaiting-auto-review-1-seat-1", TaskID: "awaiting-auto", Stage: core.StageReview, State: core.JobRunning},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	createMemoryWorkOrderInState(t, st, ctx, core.WorkOrder{
		ID: "queued-auto-implement-1", TaskID: "queued-auto", JobID: "queued-auto-implement-1",
		Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredHarness: "codex",
	})
	createMemoryWorkOrderInState(t, st, ctx, core.WorkOrder{
		ID: "awaiting-auto-review-1-seat-1", TaskID: "awaiting-auto", JobID: "awaiting-auto-review-1-seat-1",
		Stage: core.StageReview, State: core.WorkOrderCompleted, RequiredHarness: "codex", ReviewRound: 1, ReviewSeat: 1,
	})
	if err := st.AppendEvent(ctx, core.Event{
		TaskID: "awaiting-auto", JobID: "awaiting-auto-review-1-seat-1", Kind: "review.publication_retry",
		Payload: core.JSONPayload(map[string]any{"review_work_order_id": "awaiting-auto-review-1-seat-1", "last_error": "GitHub unavailable"}),
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.Workers = &workerservice.Service{Store: st}
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}}, nil
	}

	queued := httptest.NewRecorder()
	server.Handler().ServeHTTP(queued, httptest.NewRequest(http.MethodGet, "/v1/tasks/queued-auto/activity?workspace_id=demo", nil))
	if queued.Code != http.StatusOK || !bytes.Contains(queued.Body.Bytes(), []byte(`"worker_status"`)) ||
		!bytes.Contains(queued.Body.Bytes(), []byte(`"required_harnesses":["codex"]`)) ||
		!bytes.Contains(queued.Body.Bytes(), []byte(`"queue_context":"never_started"`)) {
		t.Fatalf("queued activity status=%d body=%s", queued.Code, queued.Body.String())
	}

	for _, taskID := range []string{"awaiting-auto", "approved-auto"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/activity?workspace_id=demo", nil))
		if response.Code != http.StatusOK || bytes.Contains(response.Body.Bytes(), []byte(`"worker_status"`)) {
			t.Fatalf("%s activity status=%d body=%s", taskID, response.Code, response.Body.String())
		}
		if taskID == "awaiting-auto" && !bytes.Contains(response.Body.Bytes(), []byte(`"kind":"review.publication_retry"`)) {
			t.Fatalf("awaiting activity lost publication retry: %s", response.Body.String())
		}
	}

	merged := httptest.NewRecorder()
	server.Handler().ServeHTTP(merged, httptest.NewRequest(http.MethodGet, "/v1/tasks/merged-auto/activity?workspace_id=demo", nil))
	if merged.Code != http.StatusOK || bytes.Contains(merged.Body.Bytes(), []byte(`"worker_status"`)) {
		t.Fatalf("merged activity status=%d body=%s", merged.Code, merged.Body.String())
	}
}

func TestReviewUsesLatestJobWithoutLoadingHistory(t *testing.T) {
	base := store.NewMemory()
	task := core.Task{ID: "review-latest", State: core.TaskAwaiting}
	if err := base.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateJob(context.Background(), core.Job{ID: "job-latest", TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{Store: base}
	server := NewServer(observed)
	server.BearerToken = "token"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/review-latest/review", bytes.NewReader([]byte(`{"action":"approve","reason_code":"approved"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if observed.getLatestJobCalls != 1 || observed.listJobsCalls != 0 {
		t.Fatalf("review query calls = latest:%d list:%d", observed.getLatestJobCalls, observed.listJobsCalls)
	}
}

func TestEventStreamUsesIncrementalReads(t *testing.T) {
	base := store.NewMemory()
	if err := base.CreateTask(context.Background(), core.Task{ID: "stream-task", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observed := &observedStore{Store: base, afterHook: cancel}
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/stream-task/events/stream", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	NewServer(observed).Handler().ServeHTTP(response, request)
	if observed.listEventsAfterCalls != 1 || observed.listEventsCalls != 0 {
		t.Fatalf("stream query calls = incremental:%d full:%d", observed.listEventsAfterCalls, observed.listEventsCalls)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("event: activity")) {
		t.Fatalf("stream body = %s", response.Body.String())
	}
}

// taskOperationsResponse decodes the Tasks-view projection for assertions that
// walk REQ-1 and AC-1.1 through AC-1.5.
type taskOperationsResponse []struct {
	Task struct {
		ID              string   `json:"id"`
		Title           string   `json:"title"`
		Repo            string   `json:"repo"`
		State           string   `json:"state"`
		BlockingTaskIDs []string `json:"blocking_task_ids"`
		Dependencies    []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"dependencies"`
		Context struct {
			Requirements []struct {
				ID      string `json:"id"`
				Title   string `json:"title"`
				Version int    `json:"version"`
			} `json:"requirements"`
			Designs []struct {
				ID      string `json:"id"`
				Title   string `json:"title"`
				Version int    `json:"version"`
			} `json:"designs"`
		} `json:"context"`
	} `json:"task"`
	Stalled              *taskStalledSummary `json:"stalled"`
	NeedsAttention       bool                `json:"needs_attention"`
	UnsatisfiableTaskIDs []string            `json:"unsatisfiable_task_ids"`
	ChildRollup          *taskChildRollup    `json:"child_rollup"`
	Plan                 taskPlanStatus      `json:"plan"`
}

func taskOperations(t *testing.T, st store.Store) taskOperationsResponse {
	t.Helper()
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var items taskOperationsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return items
}

func taskOperationsRow(t *testing.T, items taskOperationsResponse, id string) int {
	t.Helper()
	for index := range items {
		if items[index].Task.ID == id {
			return index
		}
	}
	t.Fatalf("task %s missing from the operations list", id)
	return -1
}

type taskOperationsBoundedStore struct {
	store.Store
	listEventsCalls int
	latestPlanCalls int
}

func (st *taskOperationsBoundedStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	st.listEventsCalls++
	return nil, fmt.Errorf("per-task ListEvents forbidden for %s", taskID)
}

func (st *taskOperationsBoundedStore) GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	st.latestPlanCalls++
	return core.SpecVersion{}, false, fmt.Errorf("per-task latest plan forbidden for %s", taskID)
}

func TestTaskOperationsPaginationFiltersAndBatchesProjectionReads(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	for index, task := range []core.Task{
		{ID: "first", Workspace: "demo", Repo: "conveyor", State: core.TaskQueued},
		{ID: "second", Workspace: "demo", Repo: "web", State: core.TaskRunning},
		{ID: "third", Workspace: "demo", Repo: "web", State: core.TaskRunning},
	} {
		task.CreatedAt = time.Date(2026, 8, 7, 10, index, 0, 0, time.UTC)
		if err := base.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	observed := &taskOperationsBoundedStore{Store: base}
	server := NewServer(observed)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/v1/task-operations?workspace_id=demo&state=running&repository=web&limit=1&offset=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Conveyor-Total") != "2" || response.Header().Get("X-Conveyor-Limit") != "1" || response.Header().Get("X-Conveyor-Offset") != "1" {
		t.Fatalf("pagination headers = %+v", response.Header())
	}
	var items taskOperationsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	// The filtered page is newest-created first: third is offset zero, so the
	// second-created task occupies offset one.
	if len(items) != 1 || items[0].Task.ID != "second" {
		t.Fatalf("paged rows = %+v", items)
	}
	if observed.listEventsCalls != 0 || observed.latestPlanCalls != 0 {
		t.Fatalf("per-task reads = events:%d plans:%d", observed.listEventsCalls, observed.latestPlanCalls)
	}

	bad := httptest.NewRecorder()
	server.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet,
		"/v1/task-operations?workspace_id=demo&state=unknown&limit=1", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter status=%d body=%s", bad.Code, bad.Body.String())
	}

	// Pagination is additive: a caller that does not opt in still receives the
	// whole workspace as a bare array, with no pagination metadata attached.
	compat := httptest.NewRecorder()
	server.Handler().ServeHTTP(compat, httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id=demo", nil))
	if compat.Code != http.StatusOK {
		t.Fatalf("compatibility status=%d body=%s", compat.Code, compat.Body.String())
	}
	if compat.Header().Get("X-Conveyor-Total") != "" || compat.Header().Get("X-Conveyor-Limit") != "" || compat.Header().Get("X-Conveyor-Offset") != "" {
		t.Fatalf("unpaginated response carried pagination headers: %+v", compat.Header())
	}
	var all taskOperationsResponse
	if err := json.Unmarshal(compat.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Task.ID != "third" || all[2].Task.ID != "first" {
		t.Fatalf("unpaginated rows = %+v", all)
	}

	// offset without limit is a caller error rather than a silent full read.
	orphan := httptest.NewRecorder()
	server.Handler().ServeHTTP(orphan, httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id=demo&offset=1", nil))
	if orphan.Code != http.StatusBadRequest {
		t.Fatalf("offset without limit status=%d body=%s", orphan.Code, orphan.Body.String())
	}

	// The largest representable offset is served — an empty page past the end,
	// with the total still reported so a client can correct itself.
	edge := httptest.NewRecorder()
	server.Handler().ServeHTTP(edge, httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/v1/task-operations?workspace_id=demo&limit=1&offset=%d", store.MaxTaskOperationsOffset), nil))
	if edge.Code != http.StatusOK {
		t.Fatalf("boundary offset status=%d body=%s", edge.Code, edge.Body.String())
	}
	if edge.Header().Get("X-Conveyor-Total") != "3" ||
		edge.Header().Get("X-Conveyor-Offset") != strconv.Itoa(store.MaxTaskOperationsOffset) {
		t.Fatalf("boundary pagination headers = %+v", edge.Header())
	}
	var beyond taskOperationsResponse
	if err := json.Unmarshal(edge.Body.Bytes(), &beyond); err != nil {
		t.Fatal(err)
	}
	if len(beyond) != 0 {
		t.Fatalf("boundary offset rows = %+v", beyond)
	}

	// Anything past that bound is rejected at the edge. Passed through, it
	// narrows to int32 in Postgres — 2^31 to a negative OFFSET Postgres
	// rejects, 2^32+5 to a page starting at row 5 — while memory returned an
	// empty page for both. The stores must agree, so neither sees it.
	for _, offset := range []string{
		strconv.FormatInt(int64(store.MaxTaskOperationsOffset)+1, 10),
		"4294967301",
		"9223372036854775808",
		"-1",
	} {
		overflow := httptest.NewRecorder()
		server.Handler().ServeHTTP(overflow, httptest.NewRequest(http.MethodGet,
			"/v1/task-operations?workspace_id=demo&limit=1&offset="+offset, nil))
		if overflow.Code != http.StatusBadRequest {
			t.Fatalf("offset=%s status=%d body=%s", offset, overflow.Code, overflow.Body.String())
		}
	}

	// The limit bound is the store's, not a second copy of it.
	overLimit := httptest.NewRecorder()
	server.Handler().ServeHTTP(overLimit, httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/v1/task-operations?workspace_id=demo&limit=%d", store.MaxTaskOperationsLimit+1), nil))
	if overLimit.Code != http.StatusBadRequest {
		t.Fatalf("over-limit status=%d body=%s", overLimit.Code, overLimit.Body.String())
	}
}

// AC-1.1 and AC-1.2: the projection is one flat, filterable list carrying the
// state and repository every row filters on, plus the durable dependency and
// blocking authority — never a second derivation of it (REQ-1).
func TestTaskOperationsListsEveryTaskWithDependencyAndBlockingState(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	for _, id := range []string{"dep-open", "dep-closed", "dep-merged"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Title: id, Repo: "conveyor", State: core.TaskRunning}); err != nil {
			t.Fatal(err)
		}
	}
	blocked := core.Task{ID: "blocked", Workspace: "demo", Title: "Blocked work", Repo: "web", State: core.TaskQueued}
	if err := st.CreateTaskWithDependencies(ctx, blocked, []string{"dep-open", "dep-closed", "dep-merged"}); err != nil {
		t.Fatal(err)
	}
	plane := taskops.New(st)
	if _, err := plane.Perform(ctx, "dep-merged", taskops.Command{Kind: core.TaskMergeRecover}); err != nil {
		t.Fatal(err)
	}
	if _, err := plane.Perform(ctx, "dep-closed", taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}

	items := taskOperations(t, st)
	if len(items) != 4 {
		t.Fatalf("operations list carries %d rows, want every task", len(items))
	}
	row := items[taskOperationsRow(t, items, "blocked")]
	if row.Task.Repo != "web" || row.Task.State != "queued" {
		t.Fatalf("row filter fields = repo:%q state:%q", row.Task.Repo, row.Task.State)
	}
	if len(row.Task.Dependencies) != 3 {
		t.Fatalf("dependencies = %+v", row.Task.Dependencies)
	}
	// A merged dependency is satisfied, so it is a dependency but not a
	// blocker; a closed one blocks and can never be satisfied.
	if !slices.Equal(row.Task.BlockingTaskIDs, []string{"dep-closed", "dep-open"}) {
		t.Fatalf("blocking_task_ids = %v", row.Task.BlockingTaskIDs)
	}
	if !slices.Equal(row.UnsatisfiableTaskIDs, []string{"dep-closed"}) {
		t.Fatalf("unsatisfiable_task_ids = %v", row.UnsatisfiableTaskIDs)
	}
	unblocked := items[taskOperationsRow(t, items, "dep-open")]
	if len(unblocked.Task.BlockingTaskIDs) != 0 || len(unblocked.UnsatisfiableTaskIDs) != 0 {
		t.Fatalf("independent task reports blockers: %+v", unblocked)
	}
}

// AC-1.2: a rollup exists only where children exist, so a childless task
// reports absence instead of a rollup of zeroes.
func TestTaskOperationsRollsUpChildrenOnlyWhenChildrenExist(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "anchor", Workspace: "demo", Title: "Historical anchor", Repo: "conveyor", State: core.TaskRunning},
		{ID: "child-merged", Workspace: "demo", Title: "Merged child", Repo: "conveyor", State: core.TaskMerged, ParentTaskID: "anchor", OriginSpecVersion: 1, OriginSubID: "SUB-1"},
		{ID: "child-closed", Workspace: "demo", Title: "Closed child", Repo: "conveyor", State: core.TaskClosed, ParentTaskID: "anchor", OriginSpecVersion: 1, OriginSubID: "SUB-2"},
		{ID: "child-open", Workspace: "demo", Title: "Open child", Repo: "conveyor", State: core.TaskQueued, ParentTaskID: "anchor", OriginSpecVersion: 1, OriginSubID: "SUB-3"},
		{ID: "solo", Workspace: "demo", Title: "Standalone task", Repo: "conveyor", State: core.TaskQueued},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	items := taskOperations(t, st)
	rollup := items[taskOperationsRow(t, items, "anchor")].ChildRollup
	if rollup == nil || *rollup != (taskChildRollup{Total: 3, Merged: 1, Closed: 1, Open: 1}) {
		t.Fatalf("anchor rollup = %+v", rollup)
	}
	if solo := items[taskOperationsRow(t, items, "solo")].ChildRollup; solo != nil {
		t.Fatalf("childless task fabricated a rollup: %+v", solo)
	}
}

// AC-1.3: each row carries the task's attached served requirements and
// governing System Design documents by identity and version, and represents an
// unattached task as unattached.
func TestTaskOperationsLinksAttachedContextAndReportsItsAbsence(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-tasks-view", Title: "Task-centric operations view"}, core.RequirementVersion{
		Content: "Task-centric operations view", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Operators shall manage delivery through a list-first Tasks view."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-lifecycle", Title: "Work-order lifecycle", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Work-order lifecycle\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	attached := core.Task{ID: "attached", Workspace: "demo", Title: "Attached task", Repo: "conveyor", State: core.TaskQueued}
	if err = st.CreateTaskWithDependenciesAndContext(ctx, attached, nil, store.TaskContextInput{
		RequirementIDs: []string{requirement.ID}, DesignIDs: []string{design.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(ctx, core.Task{ID: "bare", Workspace: "demo", Title: "Unattached task", Repo: "conveyor", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}

	items := taskOperations(t, st)
	context := items[taskOperationsRow(t, items, "attached")].Task.Context
	if len(context.Requirements) != 1 || context.Requirements[0].ID != requirement.ID ||
		context.Requirements[0].Title != "Task-centric operations view" || context.Requirements[0].Version != 1 {
		t.Fatalf("attached requirements = %+v", context.Requirements)
	}
	if len(context.Designs) != 1 || context.Designs[0].ID != design.ID ||
		context.Designs[0].Title != "Work-order lifecycle" || context.Designs[0].Version != 1 {
		t.Fatalf("attached designs = %+v", context.Designs)
	}
	bare := items[taskOperationsRow(t, items, "bare")].Task.Context
	if len(bare.Requirements) != 0 || len(bare.Designs) != 0 {
		t.Fatalf("unattached task fabricated context: %+v", bare)
	}
}

// AC-1.4: every plan-status outcome is read off durable authority — the
// persisted plan version and the audited gate command that parked or resumed
// it (design-task-lifecycle).
func TestTaskOperationsDerivesEveryPlanStatusFromDurableAuthority(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	plane := taskops.New(st)
	for _, id := range []string{"no-plan", "pending", "approved", "redirected"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Title: id, Repo: "conveyor", State: core.TaskRunning}); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(taskID string) core.SpecVersion {
		t.Helper()
		version, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "## Approach\n\n## Done criteria\n"})
		if err != nil {
			t.Fatal(err)
		}
		return version
	}
	perform := func(taskID string, command core.TaskCommand) {
		t.Helper()
		if _, err := plane.Perform(ctx, taskID, taskops.Command{Kind: command}); err != nil {
			t.Fatal(err)
		}
	}
	plan("pending")
	perform("pending", core.TaskGateSpec)
	approved := plan("approved")
	if err := st.ApproveSpecVersion(ctx, "approved", approved.Version); err != nil {
		t.Fatal(err)
	}
	plan("redirected")
	perform("redirected", core.TaskGateSpec)
	perform("redirected", core.TaskInterventionRedirect)

	items := taskOperations(t, st)
	for taskID, want := range map[string]taskPlanStatus{
		"no-plan":    {State: taskPlanNone},
		"pending":    {State: taskPlanPendingGate, Version: 1},
		"approved":   {State: taskPlanApproved, Version: 1},
		"redirected": {State: taskPlanRedirected, Version: 1},
	} {
		if got := items[taskOperationsRow(t, items, taskID)].Plan; got != want {
			t.Fatalf("task %s plan = %+v, want %+v", taskID, got, want)
		}
	}
}

// AC-1.5: the projection exposes no priority or declared-phase field. Assignee
// is durable task authority and therefore belongs on the task projection.
func TestTaskOperationsExposesAssigneeButNoRetiredFields(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "solo", Workspace: "demo", Title: "Standalone task", Repo: "conveyor", State: core.TaskQueued}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-assigned", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, "solo", "usr-assigned"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/task-operations?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(rows[0]["task"], &task); err != nil {
		t.Fatal(err)
	}
	if _, present := task["assignee"]; !present {
		t.Fatalf("task omits assignee: %s", response.Body.String())
	}
	for _, barred := range []string{"priority", "phase", "declared_phase"} {
		if _, present := rows[0][barred]; present {
			t.Fatalf("row carries barred field %q: %s", barred, response.Body.String())
		}
		if _, present := task[barred]; present {
			t.Fatalf("task carries barred field %q: %s", barred, response.Body.String())
		}
	}
}

// The shared Tasks/Board filter family reaches the store as query parameters on
// the same authenticated route (AC-2.4). What each member means is asserted
// once against both stores in storetest; what this covers is the edge: that the
// route reads every member, that both surfaces read it from the same parser, and
// that unusable input is refused rather than dropped — a silently ignored filter
// renders a workspace holding the wrong rows.
func TestTaskFilterParametersReachTheStoreOnBothSurfaces(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	get := func(t *testing.T, target string) *httptest.ResponseRecorder {
		t.Helper()
		server := NewServer(st)
		server.Workspace = "demo"
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		return response
	}
	for index, seed := range []struct {
		id     string
		title  string
		repo   string
		branch string
		state  core.TaskState
		serves string
	}{
		{"ledger-sweep", "Ledger sweep", "conveyor", "conveyor/task-ledger-sweep", core.TaskQueued, "req-ledger"},
		{"rollout", "Rollout", "web", "web/hotfix-rollout", core.TaskRunning, ""},
	} {
		task := core.Task{
			ID: seed.id, Workspace: "demo", Title: seed.title, Repo: seed.repo, Source: "operator",
			Branch: seed.branch, State: seed.state,
			CreatedAt: time.Date(2026, 8, 1, 10, index, 0, 0, time.UTC),
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if seed.serves == "" {
			continue
		}
		if err := st.AppendEvent(ctx, core.Event{
			TaskID: seed.id, Kind: store.TaskContextRequirementAdded,
			At:      time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
			Payload: core.JSONPayload(map[string]any{"id": seed.serves, "version": 1}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-ledger", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, "ledger-sweep", "usr-ledger"); err != nil {
		t.Fatal(err)
	}

	// Each member narrows to exactly the seeded row that carries it. The
	// created-at bounds straddle the two creation instants. The later attachment
	// event must not move ledger-sweep across this boundary.
	for _, applied := range []struct {
		name  string
		query string
		want  string
	}{
		{"free text", "q=LEDGER", "ledger-sweep"},
		{"free text on the assigned branch", "q=hotfix", "rollout"},
		{"served requirement", "serves_requirement=req-ledger", "ledger-sweep"},
		{"created from", "created_from=2026-08-01T10:00:30Z", "rollout"},
		{"created to", "created_to=2026-08-01T10:00:30Z", "ledger-sweep"},
		{"state and repository", "state=running&repository=web", "rollout"},
		{"assigned user", "assignee=usr-ledger", "ledger-sweep"},
		{"unassigned", "assignee=unassigned", "rollout"},
	} {
		t.Run(applied.name, func(t *testing.T) {
			for _, surface := range []string{"/v1/task-operations", "/v1/activity"} {
				response := get(t, surface+"?workspace_id=demo&"+applied.query)
				if response.Code != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", surface, response.Code, response.Body.String())
				}
				var rows []struct {
					Task struct {
						ID string `json:"id"`
					} `json:"task"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
					t.Fatalf("%s decode %s: %v", surface, response.Body.String(), err)
				}
				if len(rows) != 1 || rows[0].Task.ID != applied.want {
					t.Fatalf("%s rows = %s, want only %q", surface, response.Body.String(), applied.want)
				}
			}
		})
	}

	// A repeated parameter is a disjunction: both listed states match, so both
	// seeded rows come back through one filter member.
	several := get(t, "/v1/task-operations?workspace_id=demo&state=queued&state=running")
	if several.Code != http.StatusOK ||
		!strings.Contains(several.Body.String(), `"id":"ledger-sweep"`) ||
		!strings.Contains(several.Body.String(), `"id":"rollout"`) {
		t.Fatalf("repeated state filter status=%d body=%s", several.Code, several.Body.String())
	}

	// A governing-design filter no task carries is an empty result, not the
	// unfiltered workspace: an unread parameter would look like the latter.
	empty := get(t, "/v1/task-operations?workspace_id=demo&governing_design=design-nobody-attached")
	if empty.Code != http.StatusOK || strings.Contains(empty.Body.String(), `"id":"rollout"`) {
		t.Fatalf("unattached design filter status=%d body=%s", empty.Code, empty.Body.String())
	}
	unknownAssignee := get(t, "/v1/task-operations?workspace_id=demo&assignee=usr-nobody&limit=1&offset=0")
	if unknownAssignee.Code != http.StatusOK || unknownAssignee.Header().Get("X-Conveyor-Total") != "0" || unknownAssignee.Body.String() != "[]\n" {
		t.Fatalf("unknown assignee status=%d total=%q body=%s", unknownAssignee.Code, unknownAssignee.Header().Get("X-Conveyor-Total"), unknownAssignee.Body.String())
	}

	for _, rejected := range []struct {
		name  string
		query string
	}{
		{"unparseable instant", "created_from=last-tuesday"},
		{"non-RFC-3339 date", "created_to=2026-08-02"},
		{"inverted range", "created_from=2026-08-09T00:00:00Z&created_to=2026-08-02T00:00:00Z"},
	} {
		t.Run("rejects "+rejected.name, func(t *testing.T) {
			for _, surface := range []string{"/v1/task-operations", "/v1/activity"} {
				response := get(t, surface+"?workspace_id=demo&"+rejected.query)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("%s status=%d body=%s", surface, response.Code, response.Body.String())
				}
			}
		})
	}

	// The old parameters represented latest activity, so accepting them as
	// creation bounds would silently change a stale caller's result. The API
	// replacement is intentionally explicit; browser saved state migrates in
	// the shared filter component instead.
	for _, legacy := range []string{
		"updated_from=2026-08-01T00:00:00Z",
		"updated_to=2026-08-02T00:00:00Z",
	} {
		for _, surface := range []string{"/v1/task-operations", "/v1/activity"} {
			response := get(t, surface+"?workspace_id=demo&"+legacy)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "retired") {
				t.Fatalf("%s legacy %q status=%d body=%s", surface, legacy, response.Code, response.Body.String())
			}
		}
	}

	// The review inbox is the whole outstanding queue by construction: an
	// operator narrowing a board must not also narrow what is waiting on them.
	reviews := get(t, "/v1/reviews?workspace_id=demo&q=nothing-matches-this")
	if reviews.Code != http.StatusOK {
		t.Fatalf("reviews status=%d body=%s", reviews.Code, reviews.Body.String())
	}
}

// Staleness renders from the task-level surface, so the
// row carries the derived §21.34 reason a task cannot move — "needs operator"
// alone does not say why. It travels as the list-scoped summary, never the work
// order the detail surfaces render. A terminal task carries none: there is
// nothing left to unstick.
func TestTaskOperationsProjectsStalledStateAndDropsItOnTerminalTasks(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	const failure = "harness exited before completing work order"
	for _, task := range []core.Task{
		{ID: "stuck", Workspace: "demo", Title: "Stuck work", Repo: "conveyor", State: core.TaskQueued},
		{ID: "shipped", Workspace: "demo", Title: "Shipped work", Repo: "conveyor", State: core.TaskMerged},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		jobID := task.ID + "-implement-1"
		if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: task.ID, Stage: core.StageImplement}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
			ID: jobID, TaskID: task.ID, JobID: jobID,
			Stage: core.StageImplement, AutomaticRetryCount: 3, LastFailureMessage: failure,
		}); err != nil {
			t.Fatal(err)
		}
	}

	items := taskOperations(t, st)
	stuck := items[taskOperationsRow(t, items, "stuck")]
	if stuck.Stalled == nil || !stuck.Stalled.Needed {
		t.Fatalf("stalled row = %+v", stuck)
	}
	if stuck.Stalled.Reason != "dispatch is failing repeatedly" || stuck.Stalled.LastFailure != failure {
		t.Fatalf("stalled reason = %q failure = %q", stuck.Stalled.Reason, stuck.Stalled.LastFailure)
	}
	// Stalled and needs-attention are independent facts: a task can hold at a
	// gate and carry a stalled order at once, so the row states both.
	if !stuck.NeedsAttention {
		t.Fatalf("a stalled row must also read as needing an operator: %+v", stuck)
	}
	if shipped := items[taskOperationsRow(t, items, "shipped")]; shipped.Stalled != nil {
		t.Fatalf("terminal row retained stalled state: %+v", shipped.Stalled)
	}
}
