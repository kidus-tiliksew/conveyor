package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestAttemptObservabilityHTTPBindingAndBodyLimit covers the run-plane
// parent route: only the invoking user's credential reaches it, identity is
// bound to the immutable claim, refusals write nothing, and the encoded body
// is capped at 6 MiB (req-260820-221be8 AC-2.1; component-http-api).
func TestAttemptObservabilityHTTPBindingAndBodyLimit(t *testing.T) {
	server, st, handler := taskRunHTTPFixture(t)
	server.Credentials = staticCredentialVerifier{
		"user-token": {ID: "pat_user", OwnerUserID: "local-operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"child-token": {
			ID: "agt_child", OwnerUserID: "local-operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser,
			RunWorkspaceID: "demo", RunWorkOrderID: "target-implement-1", RunSessionID: "run-session",
		},
		"stranger-token": {ID: "pat_stranger", OwnerUserID: "someone-else", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	ctx := store.WithWorkspace(t.Context(), "demo")
	target := createTaskRunOrder(t, st, "target")
	createTaskRunOrder(t, st, "other")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"local-codex","model":"local-model"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed core.WorkOrder
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/tasks/target/run-orders/" + target.ID + "/attempt-observability"
	body := func(session, attempt, reason, transcript string) string {
		value := `{"session_id":"` + session + `","attempt_id":"` + attempt + `","termination_reason":"` + reason + `"`
		if transcript != "" {
			value += `,"transcript":` + transcript
		}
		return value + "}"
	}
	assertNoCapture := func(label string) {
		t.Helper()
		captures, err := st.ListWorkOrderTranscriptCaptures(ctx, target.ID)
		if err != nil || len(captures) != 0 {
			t.Fatalf("%s: captures=%+v err=%v", label, captures, err)
		}
	}
	// A still-active attempt has no durable ending yet.
	active := taskRunHTTPCallAs(handler, "user-token", http.MethodPost, path, body("run-session", claimed.AttemptID, "", `{"content":"early"}`))
	if active.Code != http.StatusConflict || active.Header().Get("X-Conveyor-Error-Code") != "attempt_capture_unverified" {
		t.Fatalf("active attempt status=%d code=%q body=%s", active.Code, active.Header().Get("X-Conveyor-Error-Code"), active.Body.String())
	}
	before, err := st.GetWorkOrder(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != core.WorkOrderClaimed || before.SessionID != "run-session" {
		t.Fatalf("refused capture changed lifecycle: %+v", before)
	}
	release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/release", `{"session_id":"run-session","reason":"local run ended","outcome":"released"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	released, err := st.GetWorkOrder(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, token, path, body, code string
		status                        int
	}{
		{name: "child agent credential", token: "child-token", path: path, body: body("run-session", claimed.AttemptID, "", ""), status: http.StatusUnauthorized},
		{name: "revoked or unknown credential", token: "revoked-token", path: path, body: body("run-session", claimed.AttemptID, "", ""), status: http.StatusUnauthorized},
		{name: "different user", token: "stranger-token", path: path, body: body("run-session", claimed.AttemptID, "", ""), status: http.StatusConflict, code: "attempt_capture_unauthorized"},
		{name: "wrong session", token: "user-token", path: path, body: body("other-session", claimed.AttemptID, "", ""), status: http.StatusConflict, code: "attempt_capture_unauthorized"},
		{name: "wrong attempt", token: "user-token", path: path, body: body("run-session", "attempt-forged", "", ""), status: http.StatusConflict, code: "attempt_capture_unauthorized"},
		{name: "wrong task", token: "user-token", path: "/v1/tasks/other/run-orders/" + target.ID + "/attempt-observability", body: body("run-session", claimed.AttemptID, "", ""), status: http.StatusConflict, code: "attempt_capture_unauthorized"},
		{name: "conflicting reason", token: "user-token", path: path, body: body("run-session", claimed.AttemptID, "invented ending", `{"content":"x"}`), status: http.StatusConflict, code: "attempt_capture_reason_conflict"},
		{name: "missing attempt", token: "user-token", path: path, body: body("run-session", "", "", ""), status: http.StatusBadRequest},
		{name: "malformed body", token: "user-token", path: path, body: `{"session_id":`, status: http.StatusBadRequest},
		{name: "malformed transcript", token: "user-token", path: path, body: body("run-session", claimed.AttemptID, "", `"not-an-object"`), status: http.StatusBadRequest},
		{name: "over 6 MiB", token: "user-token", path: path, body: body("run-session", claimed.AttemptID, "", `{"content":"`+strings.Repeat("a", attemptObservabilityBodyLimit)+`"}`), status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := taskRunHTTPCallAs(handler, test.token, http.MethodPost, test.path, test.body)
			if response.Code != test.status || response.Header().Get("X-Conveyor-Error-Code") != test.code {
				t.Fatalf("status=%d code=%q body=%s", response.Code, response.Header().Get("X-Conveyor-Error-Code"), response.Body.String())
			}
			assertNoCapture(test.name)
			after, err := st.GetWorkOrder(ctx, target.ID)
			if err != nil || after.State != released.State || after.LastAttemptID != released.LastAttemptID || after.LastFailureMessage != released.LastFailureMessage || !after.UpdatedAt.Equal(released.UpdatedAt) {
				t.Fatalf("refusal wrote lifecycle state: before=%+v after=%+v err=%v", released, after, err)
			}
		})
	}
	// Exactly at the encoded-body limit is admitted and bounded by the service.
	prefix := body("run-session", claimed.AttemptID, "local run ended", `{"content":"`)
	padding := attemptObservabilityBodyLimit - len(prefix) - len(`"}}`)
	atLimit := prefix + strings.Repeat("b", padding) + `"}}`
	if len(atLimit) != attemptObservabilityBodyLimit {
		t.Fatalf("fixture length=%d", len(atLimit))
	}
	accepted := taskRunHTTPCallAs(handler, "user-token", http.MethodPost, path, atLimit)
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"created":true`) || !strings.Contains(accepted.Body.String(), `"termination_reason":"local run ended"`) {
		t.Fatalf("at-limit status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	captures, err := st.ListWorkOrderTranscriptCaptures(ctx, target.ID)
	if err != nil || len(captures) != 1 || captures[0].AttemptID != claimed.AttemptID || captures[0].TerminationReason != "local run ended" || !captures[0].Truncated || len(captures[0].Content) != workerservice.AttemptTranscriptLimit {
		t.Fatalf("captures=%d err=%v", len(captures), err)
	}
	duplicate := taskRunHTTPCallAs(handler, "user-token", http.MethodPost, path, body("run-session", claimed.AttemptID, "", `{"content":"replacement"}`))
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"created":false`) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if again, _ := st.ListWorkOrderTranscriptCaptures(ctx, target.ID); len(again) != 1 || again[0].Content != captures[0].Content {
		t.Fatal("duplicate capture rewrote the stored capture")
	}
}

// TestAttemptObservabilityWorkerPlaneBinding proves the worker route binds
// the capture to the claiming enrolled worker for a non-implement stage.
func TestAttemptObservabilityWorkerPlaneBinding(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	workOrders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: workOrders, ConfigProvider: provider, Now: func() time.Time { return now }}
	owner := core.Worker{ID: "worker-a", Workspace: "demo", OwnerUserID: "usr-owner", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}}
	intruder := core.Worker{ID: "worker-b", Workspace: "demo", OwnerUserID: "usr-owner", LeaseExpiresAt: now.Add(time.Minute)}
	task := core.Task{ID: "worker-capture", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/worker-capture", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workers = workers
	call := func(worker *core.Worker, name, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		route := chi.NewRouteContext()
		route.URLParams.Add("id", job.ID)
		requestCtx := context.WithValue(ctx, chi.RouteCtxKey, route)
		if worker != nil {
			requestCtx = context.WithValue(requestCtx, workerContextKey{}, *worker)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/worker/work-orders/"+job.ID+"/"+name, strings.NewReader(body)).WithContext(requestCtx)
		response := httptest.NewRecorder()
		handler(response, request)
		return response
	}
	claim := call(&owner, "claim", `{"session_id":"worker-session","client_token":"worker-client","lease_seconds":60}`, server.claimWorkerOrder)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var delivery workerservice.ClaimDelivery
	if err := json.Unmarshal(claim.Body.Bytes(), &delivery); err != nil {
		t.Fatal(err)
	}
	if release := call(&owner, "release", `{"session_id":"worker-session","outcome":"child_failure","reason":"harness exited without terminal verdict submission","release_cause":"session_exit"}`, server.releaseWorkerOrder); release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	capture := `{"session_id":"worker-session","attempt_id":"` + delivery.WorkOrder.AttemptID + `","termination_reason":"harness exited without terminal verdict submission","transcript":{"content":"review output"}}`
	if missing := call(nil, "attempt-observability", capture, server.captureWorkerOrderAttempt); missing.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", missing.Code)
	}
	if other := call(&intruder, "attempt-observability", capture, server.captureWorkerOrderAttempt); other.Code != http.StatusConflict || other.Header().Get("X-Conveyor-Error-Code") != "attempt_capture_unauthorized" {
		t.Fatalf("other worker status=%d body=%s", other.Code, other.Body.String())
	}
	accepted := call(&owner, "attempt-observability", capture, server.captureWorkerOrderAttempt)
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"created":true`) {
		t.Fatalf("owner status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	captures, err := st.ListWorkOrderTranscriptCaptures(ctx, job.ID)
	if err != nil || len(captures) != 1 || captures[0].Content != "review output" || captures[0].TerminationReason != "harness exited without terminal verdict submission" {
		t.Fatalf("captures=%+v err=%v", captures, err)
	}
	order, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(order); strings.Contains(string(encoded), "review output") {
		t.Fatal("capture leaked into the work-order read model")
	}
}
