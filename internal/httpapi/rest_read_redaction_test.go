package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	rawRowToken  = "cv_pat_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6kM9nP2r"
	rawRowAppKey = "fixture-stored-app-key-value"
	rawRowDigest = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	rawRowBody   = "Rotate " + rawRowToken + " and " + rawRowAppKey + "\n"
)

type readSecretSource struct {
	values []string
	err    error
}

func (s readSecretSource) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.values, s.err
}

// rawRowFixture stores a task and events the way rows stored before redaction
// reached them look: secrets in the title, body, and event payloads.
func rawRowFixture(t *testing.T, id string, secrets readSecretSource) (context.Context, store.Store, *Server) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: id, Workspace: "demo", Repo: "api", Title: "Rotate " + rawRowToken, Body: rawRowBody, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: id, Kind: "work_order.progress_reported", Payload: core.JSONPayload(map[string]any{"message": "stored raw " + rawRowToken, "nested": map[string]any{"key": rawRowAppKey}})},
		{TaskID: id, Kind: "artifact.recorded", Payload: core.JSONPayload(map[string]any{"artifact_id": rawRowDigest, "count": 7})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, RedactionSecrets: secrets}
	return ctx, st, server
}

func requireRawRowsUnchanged(ctx context.Context, t *testing.T, st store.Store, id string) {
	t.Helper()
	task, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Body != rawRowBody || !strings.Contains(task.Title, rawRowToken) {
		t.Fatal("the read rewrote the stored task row")
	}
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || (event.Kind == "work_order.progress_reported" && strings.Contains(string(event.Payload), rawRowToken))
	}
	if !found {
		t.Fatal("the read rewrote the recorded event")
	}
}

func requireNoRawRowSecret(t *testing.T, name, text string) {
	t.Helper()
	if strings.Contains(text, rawRowToken) || strings.Contains(text, rawRowAppKey) {
		t.Fatalf("%s serves a stored secret", name)
	}
}

// readStreamToFirstPoll serves the event stream until its first poll has read
// the store, then cancels the request through the store hook.
func readStreamToFirstPoll(t *testing.T, server *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(store.WithActor(context.Background(), store.SystemActor()))
	defer cancel()
	server.Store = &observedStore{Store: server.Store, afterHook: cancel}
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+id+"/events/stream", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	authenticatedMemoryHandler(server).ServeHTTP(response, request)
	return response
}

// TestRESTReadsRedactRowsStoredRawWithoutRewritingThem covers GET task, GET
// events, and the event stream (req-security-boundaries REQ-3, REQ-5).
func TestRESTReadsRedactRowsStoredRawWithoutRewritingThem(t *testing.T) {
	t.Parallel()
	const id = "raw-row"
	ctx, st, server := rawRowFixture(t, id, readSecretSource{values: []string{rawRowAppKey}})
	handler := authenticatedMemoryHandler(server)
	get := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
		return response
	}

	task := get("/v1/tasks/" + id)
	requireNoRawRowSecret(t, "getTask", task.Body.String())
	var served struct {
		ID         string          `json:"id"`
		Title      string          `json:"title"`
		Body       string          `json:"body"`
		NextAction json.RawMessage `json:"next_action"`
	}
	if err := json.Unmarshal(task.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if served.ID != id || served.Body != "Rotate [REDACTED:pattern] and [REDACTED:exact]\n" || served.Title != "Rotate [REDACTED:pattern]" || len(served.NextAction) == 0 {
		t.Fatalf("getTask served id=%q next_action=%d bytes, redacted body=%t", served.ID, len(served.NextAction), served.Body == "Rotate [REDACTED:pattern] and [REDACTED:exact]\n")
	}

	events := get("/v1/tasks/" + id + "/events")
	requireNoRawRowSecret(t, "listEvents", events.Body.String())
	var listed []core.Event
	if err := json.Unmarshal(events.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	redacted, digest := false, false
	for _, event := range listed {
		if event.Kind == "work_order.progress_reported" {
			redacted = strings.Contains(string(event.Payload), "stored raw [REDACTED:pattern]") && strings.Contains(string(event.Payload), "[REDACTED:exact]")
		}
		if event.Kind == "artifact.recorded" {
			digest = strings.Contains(string(event.Payload), rawRowDigest) && strings.Contains(string(event.Payload), `"count":7`)
		}
	}
	if !redacted || !digest {
		t.Fatalf("listEvents redacted=%t kept digest=%t", redacted, digest)
	}

	stream := readStreamToFirstPoll(t, server, id)
	requireNoRawRowSecret(t, "streamEvents", stream.Body.String())
	frames := stream.Body.String()
	if !strings.Contains(frames, "event: activity") || !strings.Contains(frames, "stored raw [REDACTED:pattern]") || !strings.Contains(frames, rawRowDigest) {
		t.Fatalf("stream did not serve the redacted events: %d bytes", len(frames))
	}

	requireRawRowsUnchanged(ctx, t, st, id)
}

// TestRESTReadsRefuseWhenSecretsCannotBeRead proves no stored text is served
// when the redactor can't be built.
func TestRESTReadsRefuseWhenSecretsCannotBeRead(t *testing.T) {
	t.Parallel()
	const id = "raw-row-refused"
	ctx, st, server := rawRowFixture(t, id, readSecretSource{err: errors.New("key decrypt failed")})
	handler := authenticatedMemoryHandler(server)
	for _, path := range []string{"/v1/tasks/" + id, "/v1/tasks/" + id + "/events"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
			t.Fatalf("%s status=%d body bytes=%d", path, response.Code, response.Body.Len())
		}
	}
	ctxStream, cancel := context.WithCancel(store.WithActor(context.Background(), store.SystemActor()))
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+id+"/events/stream", nil).WithContext(ctxStream)
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, request)
	if body := stream.Body.String(); body != "event: error\ndata: {}\n\n" {
		t.Fatalf("stream served %d bytes instead of one error frame", len(body))
	}
	requireRawRowsUnchanged(ctx, t, st, id)
}

// TestWorkOrderDeliversTaskBodyByteExactWhileRESTRedacts walks the path work
// orders use: get_work_order over MCP delivers the stored body as submitted,
// while the REST and MCP task reads serve it redacted
// (req-intake-and-triage AC-1.2; req-security-boundaries REQ-3).
func TestWorkOrderDeliversTaskBodyByteExactWhileRESTRedacts(t *testing.T) {
	t.Parallel()
	const body = "Rotate this leaked value: " + rawRowToken + "\n\n- keep **markdown**  spacing\n"
	f := newDeliveryFixture(t, "exact-delivery", func(task *core.Task) { task.Body = body })
	order := f.order(t, core.StageImplement)
	const session = "exact-delivery-session"
	f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "delivery-token", "agent": "claude-code", "model": "claude-opus-5-5", "lease_seconds": 3600}))
	_, contractText := f.call(t, "get_work_order", deliveryArgs(order, session, nil))
	var contract struct {
		Task struct {
			Body string `json:"body"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(contractText), &contract); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(contract.Task.Body), []byte(body)) {
		t.Fatal("get_work_order did not deliver the task body byte for byte")
	}

	response := httptest.NewRecorder()
	authenticatedMemoryHandler(f.server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+f.task.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("getTask status=%d", response.Code)
	}
	requireNoRawRowSecret(t, "getTask", response.Body.String())

	stored, err := f.store.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Body != body {
		t.Fatal("the store does not hold the body as submitted")
	}
}

type failingPlanningAgent struct {
	err    error
	before func()
}

func (a failingPlanningAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
	if a.before != nil {
		a.before()
	}
	return inprocess.Result{}, a.err
}

// contextReadSecretSource fails the way a store read does once its context has
// ended.
type contextReadSecretSource struct{ values []string }

func (s contextReadSecretSource) ListGitHubAppKeysForRedaction(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.values, nil
}

// TestPlanningRunFailureLogsRedactedDetail covers the planning log line,
// including a request whose client disconnected before the failure, whose
// cancelled context must not cost the secret read (req-security-boundaries
// REQ-2).
func TestPlanningRunFailureLogsRedactedDetail(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "planning-log-session"})
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.WorkOrders = &workorder.Service{Store: st, RedactionSecrets: contextReadSecretSource{values: []string{rawRowAppKey}}}
	server.Planning = &planning.Service{Store: st, Agent: failingPlanningAgent{err: errors.New("model failed holding " + rawRowToken + " and " + rawRowAppKey), before: disconnect}, Model: "planner", Prompt: planningHTTPPrompt}
	logged := captureLog(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages", strings.NewReader(`{"content":"Plan this"}`)).WithContext(requestCtx)
	request.Header.Set("Authorization", "Bearer token")
	server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	text := logged.String()
	if !strings.Contains(text, "planning session "+session.ID+" run failed") {
		t.Fatalf("no failure line in %d bytes of log", len(text))
	}
	requireNoRawRowSecret(t, "planning log", text)
	if !strings.Contains(text, "[REDACTED:pattern]") || !strings.Contains(text, "[REDACTED:exact]") {
		t.Fatalf("the log line lost its detail in %d bytes of log", len(text))
	}
}
