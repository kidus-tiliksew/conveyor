package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type planningHTTPAgent struct {
	output string
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
