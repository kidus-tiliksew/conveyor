package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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

// attemptCaptureBoundPlane is one parent plane with an ended attempt whose
// activity snapshot is still stored, so a refusal can be checked against
// capture, snapshot, and lifecycle state.
type attemptCaptureBoundPlane struct {
	name    string
	attempt string
	session string
	reason  string
	post    func(body io.Reader, contentLength int64) *httptest.ResponseRecorder
	state   func() string
}

func attemptCaptureRunPlane(t *testing.T) attemptCaptureBoundPlane {
	t.Helper()
	_, st, handler := taskRunHTTPFixture(t)
	ctx := store.WithWorkspace(t.Context(), "demo")
	target := createTaskRunOrder(t, st, "target")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"local-codex","model":"local-model"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed core.WorkOrder
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if renew := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/renew", `{"session_id":"run-session","activity_snapshot":{"content":"latest run output"}}`); renew.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renew.Code, renew.Body.String())
	}
	if release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/release", `{"session_id":"run-session","reason":"local run ended","outcome":"released"}`); release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	return attemptCaptureBoundPlane{
		name: "run", attempt: claimed.AttemptID, session: "run-session", reason: "local run ended",
		post: func(body io.Reader, contentLength int64) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/attempt-observability", body)
			request.ContentLength = contentLength
			request.Header.Set("Authorization", "Bearer user-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		},
		state: func() string { return attemptCaptureStoreState(t, st, ctx, target.ID) },
	}
}

func attemptCaptureWorkerPlane(t *testing.T) attemptCaptureBoundPlane {
	t.Helper()
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
	task := core.Task{ID: "worker-bound", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/worker-bound", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
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
	call := func(name string, body io.Reader, contentLength int64, handler http.HandlerFunc) *httptest.ResponseRecorder {
		route := chi.NewRouteContext()
		route.URLParams.Add("id", job.ID)
		request := httptest.NewRequest(http.MethodPost, "/v1/worker/work-orders/"+job.ID+"/"+name, body)
		request.ContentLength = contentLength
		request = request.WithContext(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, route), workerContextKey{}, owner))
		response := httptest.NewRecorder()
		handler(response, request)
		return response
	}
	text := func(name, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		return call(name, strings.NewReader(body), int64(len(body)), handler)
	}
	claim := text("claim", `{"session_id":"worker-session","client_token":"worker-client","lease_seconds":60}`, server.claimWorkerOrder)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var delivery workerservice.ClaimDelivery
	if err := json.Unmarshal(claim.Body.Bytes(), &delivery); err != nil {
		t.Fatal(err)
	}
	if renew := text("renew", `{"session_id":"worker-session","activity_snapshot":{"content":"latest review output"}}`, server.renewWorkerOrder); renew.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renew.Code, renew.Body.String())
	}
	if release := text("release", `{"session_id":"worker-session","outcome":"stalled","reason":"harness produced no output before stall_timeout","release_cause":"session_exit"}`, server.releaseWorkerOrder); release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	return attemptCaptureBoundPlane{
		name: "worker", attempt: delivery.WorkOrder.AttemptID, session: "worker-session", reason: "harness produced no output before stall_timeout",
		post: func(body io.Reader, contentLength int64) *httptest.ResponseRecorder {
			return call("attempt-observability", body, contentLength, server.captureWorkerOrderAttempt)
		},
		state: func() string { return attemptCaptureStoreState(t, st, ctx, job.ID) },
	}
}

// attemptCaptureStoreState renders capture, activity snapshot, and lifecycle
// state for unchanged-state comparisons.
func attemptCaptureStoreState(t *testing.T, st store.Store, ctx context.Context, orderID string) string {
	t.Helper()
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	captures, err := st.ListWorkOrderTranscriptCaptures(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, err := st.GetWorkOrderActivitySnapshot(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"order": order, "captures": captures, "snapshot": snapshot, "snapshot_exists": exists})
	return string(encoded)
}

// TestAttemptObservabilityWholeBodyBoundBothPlanes proves the 6 MiB cap
// covers the whole encoded body on both parent planes: a declared or unknown
// (chunked) length over the cap, trailing whitespace pushing a small valid
// capture past the cap, and trailing data are refused with capture, snapshot,
// and lifecycle state unchanged; exactly at the cap is admitted
// (req-260820-221be8 AC-2.1; component-http-api).
func TestAttemptObservabilityWholeBodyBoundBothPlanes(t *testing.T) {
	for _, build := range []func(*testing.T) attemptCaptureBoundPlane{attemptCaptureRunPlane, attemptCaptureWorkerPlane} {
		plane := build(t)
		t.Run(plane.name, func(t *testing.T) {
			small := `{"session_id":"` + plane.session + `","attempt_id":"` + plane.attempt + `","transcript":{"content":"bounded"}}`
			padTo := func(prefix string, total int, filler byte) string {
				return prefix + strings.Repeat(string(filler), total-len(prefix))
			}
			for _, test := range []struct {
				name          string
				body          string
				unknownLength bool
				status        int
			}{
				{name: "cap plus one with declared length", body: padTo(small, attemptObservabilityBodyLimit+1, ' '), status: http.StatusRequestEntityTooLarge},
				{name: "cap plus one with unknown length", body: padTo(small, attemptObservabilityBodyLimit+1, ' '), unknownLength: true, status: http.StatusRequestEntityTooLarge},
				{name: "trailing whitespace far past the cap", body: padTo(small, 2*attemptObservabilityBodyLimit, '\n'), unknownLength: true, status: http.StatusRequestEntityTooLarge},
				{name: "trailing JSON value", body: small + ` {"attempt_id":"other"}`, status: http.StatusBadRequest},
				{name: "trailing garbage", body: small + "garbage", status: http.StatusBadRequest},
				{name: "trailing garbage after whitespace near the cap", body: padTo(small, attemptObservabilityBodyLimit-1, ' ') + "x", status: http.StatusBadRequest},
			} {
				t.Run(test.name, func(t *testing.T) {
					before := plane.state()
					length := int64(len(test.body))
					if test.unknownLength {
						length = -1
					}
					response := plane.post(strings.NewReader(test.body), length)
					if response.Code != test.status {
						t.Fatalf("status=%d want %d body=%s", response.Code, test.status, response.Body.String())
					}
					if after := plane.state(); after != before {
						t.Fatalf("refusal changed state:\nbefore=%s\nafter=%s", before, after)
					}
				})
			}
			t.Run("exactly at the cap with trailing whitespace", func(t *testing.T) {
				atCap := padTo(small, attemptObservabilityBodyLimit, ' ')
				response := plane.post(strings.NewReader(atCap), -1)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created":true`) || !strings.Contains(response.Body.String(), plane.reason) {
					t.Fatalf("at-cap status=%d body=%s", response.Code, response.Body.String())
				}
				state := plane.state()
				if !strings.Contains(state, `"content":"bounded"`) || !strings.Contains(state, `"snapshot_exists":false`) {
					t.Fatalf("admitted capture state=%s", state)
				}
			})
		})
	}
}

type failingAttemptRedaction struct{}

func (failingAttemptRedaction) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return nil, errors.New("key store unavailable")
}

// TestAttemptObservabilityDuplicateUploadsRaceAndRedactionFailure releases
// concurrent duplicate deliveries of one ended attempt together on the run
// plane: exactly one inserts the capture and every response carries the same
// persisted reason. A delivery whose redaction source fails stores no
// transcript and leaves the ended attempt's lifecycle state unchanged.
func TestAttemptObservabilityDuplicateUploadsRaceAndRedactionFailure(t *testing.T) {
	t.Run("duplicate uploads", func(t *testing.T) {
		plane := attemptCaptureRunPlane(t)
		body := `{"session_id":"` + plane.session + `","attempt_id":"` + plane.attempt + `","termination_reason":"` + plane.reason + `","transcript":{"content":"racing upload"}}`
		const uploads = 8
		start := make(chan struct{})
		results := make(chan string, uploads)
		var ready, done sync.WaitGroup
		for range uploads {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				response := plane.post(strings.NewReader(body), int64(len(body)))
				results <- fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String()))
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()
		close(results)
		created := 0
		for result := range results {
			if !strings.HasPrefix(result, "200 ") || !strings.Contains(result, `"termination_reason":"`+plane.reason+`"`) {
				t.Fatalf("upload result=%s", result)
			}
			if strings.Contains(result, `"created":true`) {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("created=%d want exactly one", created)
		}
		if state := plane.state(); strings.Count(state, `"racing upload"`) != 1 {
			t.Fatalf("state=%s", state)
		}
	})
	t.Run("redaction failure", func(t *testing.T) {
		server, st, handler := taskRunHTTPFixture(t)
		server.Workers.RedactionSecrets = failingAttemptRedaction{}
		ctx := store.WithWorkspace(t.Context(), "demo")
		target := createTaskRunOrder(t, st, "target")
		claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"local-codex","model":"local-model"}`)
		var claimed core.WorkOrder
		if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
			t.Fatal(err)
		}
		if release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/release", `{"session_id":"run-session","reason":"local run ended","outcome":"released"}`); release.Code != http.StatusOK {
			t.Fatalf("release status=%d", release.Code)
		}
		before, err := st.GetWorkOrder(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		response := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/attempt-observability", `{"session_id":"run-session","attempt_id":"`+claimed.AttemptID+`","transcript":{"content":"unredactable output"}}`)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created":false`) || !strings.Contains(response.Body.String(), "local run ended") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if captures, err := st.ListWorkOrderTranscriptCaptures(ctx, target.ID); err != nil || len(captures) != 0 {
			t.Fatalf("unredacted transcript stored: %+v %v", captures, err)
		}
		after, err := st.GetWorkOrder(ctx, target.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("redaction failure changed lifecycle: before=%+v after=%+v", before, after)
		}
	})
}
