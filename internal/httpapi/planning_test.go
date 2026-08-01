package httpapi

import (
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
)

type planningHTTPAgent struct {
	output string
}

type blockingPlanningHTTPAgent struct {
	started chan struct{}
	release chan struct{}
}

func (a *blockingPlanningHTTPAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
	close(a.started)
	<-a.release
	return inprocess.Result{Output: `{"response_text":"First run complete.","tool_calls":[]}`}, nil
}

type failingPlanningHTTPAgent struct{}

func (failingPlanningHTTPAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
	return inprocess.Result{}, errors.New("git cat-file secret-path: private stderr diagnostic")
}

func (a planningHTTPAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
	return inprocess.Result{Output: a.output}, nil
}

func TestPlanningHTTPStreamsAISDKProtocolAndRestoresDurableMessages(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Planning = &planning.Service{
		Store: st, Agent: planningHTTPAgent{
			output: `{"response_text":"Tell me the target repository.","tool_calls":[]}`,
		},
		Model: "planner",
	}
	handler := server.Handler()

	create := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions",
		strings.NewReader(`{"title":"Plan retries"}`))
	create.Header.Set("Authorization", "Bearer token")
	createdResponse := httptest.NewRecorder()
	handler.ServeHTTP(createdResponse, create)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &session); err != nil || session.ID == "" {
		t.Fatalf("created session=%+v err=%v", session, err)
	}

	chat := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages",
		strings.NewReader(`{"message":{"role":"user","parts":[{"type":"text","text":"Plan a bounded retry policy."}]}}`))
	chat.Header.Set("Authorization", "Bearer token")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, chat)
	if stream.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", stream.Code, stream.Body.String())
	}
	if got := stream.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type=%q", got)
	}
	if got := stream.Header().Get("X-Vercel-AI-UI-Message-Stream"); got != "v1" {
		t.Fatalf("protocol header=%q", got)
	}
	body := stream.Body.String()
	for _, marker := range []string{
		`"type":"start"`, `"type":"start-step"`, `"type":"text-start"`,
		`"type":"text-delta"`, `"type":"text-end"`, `"type":"finish-step"`,
		`"type":"finish"`, "data: [DONE]",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("stream missing %q:\n%s", marker, body)
		}
	}

	restore := httptest.NewRequest(http.MethodGet, "/v1/planning-sessions/"+session.ID+"/messages", nil)
	restoredResponse := httptest.NewRecorder()
	handler.ServeHTTP(restoredResponse, restore)
	if restoredResponse.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restoredResponse.Code, restoredResponse.Body.String())
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content string          `json:"content"`
		Parts   json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(restoredResponse.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" ||
		messages[0].Content != "Plan a bounded retry policy." ||
		messages[1].Role != "assistant" ||
		!strings.Contains(string(messages[1].Parts), `"text-delta"`) {
		t.Fatalf("restored messages=%+v", messages)
	}
}

func TestPlanningHTTPRequiresMutationAuthAndKeepsWorkspaceScope(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions",
		strings.NewReader(`{"title":"No auth"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unauthorized)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	conflict := httptest.NewRequest(http.MethodGet, "/v1/planning-sessions?workspace_id=demo", nil)
	conflict.Header.Set("X-Workspace-ID", "other")
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusBadRequest ||
		!strings.Contains(conflictResponse.Body.String(), "workspace_conflict") {
		t.Fatalf("status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
}

func TestPlanningHTTPConcurrentRunReturnsConflictBeforeSSECommit(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-http-run-claim"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &blockingPlanningHTTPAgent{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Planning = &planning.Service{Store: st, Agent: agent, Model: "planner"}
	handler := server.Handler()

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages",
		strings.NewReader(`{"content":"First message"}`))
	firstRequest.Header.Set("Authorization", "Bearer token")
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(firstResponse, firstRequest)
	}()
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first planning run did not reach the model")
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages",
		strings.NewReader(`{"content":"Competing message"}`))
	secondRequest.Header.Set("Authorization", "Bearer token")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusConflict ||
		strings.Contains(secondResponse.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("second response status=%d headers=%v body=%s",
			secondResponse.Code, secondResponse.Header(), secondResponse.Body.String())
	}
	close(agent.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first planning run did not finish")
	}
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	messages, err := st.ListPlanningMessages(ctx, session.ID)
	if err != nil || len(messages) != 2 || messages[0].Content != "First message" {
		t.Fatalf("serialized messages=%+v err=%v", messages, err)
	}
}

func TestPlanningHTTPRedactsInternalRunErrors(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-http-redaction"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Planning = &planning.Service{Store: st, Agent: failingPlanningHTTPAgent{}, Model: "planner"}
	request := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages",
		strings.NewReader(`{"content":"Trigger internal failure"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Planning request failed") ||
		strings.Contains(response.Body.String(), "secret-path") || strings.Contains(response.Body.String(), "private stderr") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
