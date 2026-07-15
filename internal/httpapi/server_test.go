package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestCreateTaskRequiresBearerToken(t *testing.T) {
	st := store.NewMemory()
	created := make(chan string, 1)
	s := NewServer(st)
	s.Repos = []string{"api"}
	s.Workspace = "test"
	s.BearerToken = "secret-token"
	s.OnCreate = func(_ context.Context, id string) { created <- id }
	h := s.Handler()
	body := []byte(`{"title":"fix it","repo":"api"}`)

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

func TestRedispatchRefusesHaltedTaskWithoutDecidedStage(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "halted", State: core.TaskParked, RecoveryStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(st)
	s.BearerToken = "token"
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/halted/redispatch", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	persisted, _ := st.GetTask(context.Background(), "halted")
	if persisted.State != core.TaskParked || persisted.NextStage != "" {
		t.Fatalf("halted task changed: %+v", persisted)
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
		if err := st.SetTaskTransition(ctx, task.ID, core.TaskQueued, task.RecoveryStage, ""); err != nil {
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
	if len(interventions) != 1 || interventions[0].ReasonCode != "spec-wrong" || interventions[0].ActorID != "kidus" {
		t.Fatalf("interventions = %+v", interventions)
	}
	events, err := st.ListEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[1].Kind != "intervention.redirect" || events[2].Kind != "task.state_changed" || events[3].Kind != "pipeline.transition_decided" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReviewRequiresReasonCodeAndHumanGate(t *testing.T) {
	st := store.NewMemory()
	if err := st.CreateTask(context.Background(), core.Task{ID: "running", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
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

	if err := st.UpdateTaskState(context.Background(), "running", core.TaskAwaiting); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/running/review", bytes.NewReader([]byte(`{"action":"approve"}`)))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing reason status = %d", response.Code)
	}
}

type observedStore struct {
	store.Store
	listJobsCalls            int
	listEventsCalls          int
	listInterventionsCalls   int
	listActivityMarkersCalls int
	getLatestJobCalls        int
	listEventsAfterCalls     int
	afterHook                func()
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
