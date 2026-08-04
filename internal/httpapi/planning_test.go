package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
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

func TestPlanningHTTPAttachmentWithoutRequirementHasDurableSessionOwner(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Planning = &planning.Service{
		Store: st, Agent: planningHTTPAgent{output: `{"response_text":"Attachment received.","tool_calls":[]}`},
		Model: "planner", Prompt: planningHTTPPrompt,
	}
	handler := server.Handler()
	ctx := store.WithWorkspace(t.Context(), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-no-requirement"})
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "brief.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("durable planning context")); err != nil {
		t.Fatal(err)
	}
	if err = form.WriteField("planning_session_id", session.ID); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost, "/v1/artifacts", &body)
	upload.Header.Set("Authorization", "Bearer token")
	upload.Header.Set("Content-Type", form.FormDataContentType())
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var artifact core.Artifact
	if err = json.Unmarshal(uploadResponse.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.PlanningSessionID != session.ID || artifact.RequirementID != "" || artifact.TaskID != "" {
		t.Fatalf("artifact owner=%+v", artifact)
	}

	chatBody := fmt.Sprintf(`{"message":{"role":"user","parts":[{"type":"text","text":"Use this file."},{"type":"file","artifactId":%q,"filename":"brief.txt","mediaType":"text/plain"}]}}`, artifact.ID)
	chat := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/messages", strings.NewReader(chatBody))
	chat.Header.Set("Authorization", "Bearer token")
	chatResponse := httptest.NewRecorder()
	handler.ServeHTTP(chatResponse, chat)
	if chatResponse.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chatResponse.Code, chatResponse.Body.String())
	}

	listed, err := st.ListArtifacts(ctx)
	if err != nil || len(listed) != 1 || listed[0].PlanningSessionID != session.ID {
		t.Fatalf("listed artifacts=%+v err=%v", listed, err)
	}
	restored, content, err := st.GetArtifactForPlanningSession(ctx, artifact.ID, session.ID)
	if err != nil || restored.PlanningSessionID != session.ID || string(content) != "durable planning context" {
		t.Fatalf("restored artifact=%+v content=%q err=%v", restored, content, err)
	}

	other, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-other"})
	if err != nil {
		t.Fatal(err)
	}
	foreignChat := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+other.ID+"/messages", strings.NewReader(chatBody))
	foreignChat.Header.Set("Authorization", "Bearer token")
	foreignResponse := httptest.NewRecorder()
	handler.ServeHTTP(foreignResponse, foreignChat)
	if foreignResponse.Code != http.StatusBadRequest || !strings.Contains(foreignResponse.Body.String(), "is not owned") {
		t.Fatalf("foreign attachment status=%d body=%s", foreignResponse.Code, foreignResponse.Body.String())
	}
}

func TestPlanningHTTPAbandonPersistsReason(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-abandon-reason"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	request := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions/"+session.ID+"/abandon", strings.NewReader(`{"reason":"No longer needed"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("abandon status=%d body=%s", response.Code, response.Body.String())
	}
	events, err := st.(interface {
		ListPlanningSessionEvents(context.Context, string) ([]core.Event, error)
	}).ListPlanningSessionEvents(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "planning_session.abandoned" && strings.Contains(string(event.Payload), `"reason":"No longer needed"`) {
			return
		}
	}
	t.Fatalf("abandon reason missing from events=%+v", events)
}

const planningHTTPPrompt = "test planning role"

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
		Model: "planner", Prompt: planningHTTPPrompt,
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
	server.Planning = &planning.Service{Store: st, Agent: agent, Model: "planner", Prompt: planningHTTPPrompt}
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
	server.Planning = &planning.Service{Store: st, Agent: failingPlanningHTTPAgent{}, Model: "planner", Prompt: planningHTTPPrompt}
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

// The creation route accepts the goal once and refuses an unknown one; the
// stored session carries the goal-derived provisional title (spec §21.57).
func TestPlanningHTTPCreateDeclaresGoalAndProvisionalTitle(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	create := func(body string) (int, core.PlanningSession) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var session core.PlanningSession
		if response.Code == http.StatusCreated {
			if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
				t.Fatalf("decode %s: %v", response.Body.String(), err)
			}
		}
		return response.Code, session
	}

	code, drafting := create(`{"goal":"requirement"}`)
	if code != http.StatusCreated || drafting.Goal != core.PlanningGoalRequirement ||
		drafting.Title != "Drafting requirement…" {
		t.Fatalf("requirement-goal create status=%d session=%+v", code, drafting)
	}
	// A caller-supplied title carries no weight: the session is named by its
	// goal and then by the artifact it produces (spec §21.57 change 3).
	code, planning := create(`{"goal":"blueprint","title":"Bound the retry loop"}`)
	if code != http.StatusCreated || planning.Goal != core.PlanningGoalBlueprint ||
		planning.Title != "Planning work…" {
		t.Fatalf("blueprint-goal create status=%d session=%+v", code, planning)
	}
	// Omitting the goal stays compatible and reads back as open.
	code, compatible := create(`{}`)
	if code != http.StatusCreated || compatible.Goal != core.PlanningGoalOpen ||
		compatible.Title != "Exploring…" {
		t.Fatalf("goal-less create status=%d session=%+v", code, compatible)
	}
	if code, _ = create(`{"goal":"epic"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown goal status=%d, want 400", code)
	}

	ctx := store.WithWorkspace(t.Context(), "demo")
	listed, err := st.ListPlanningSessions(ctx)
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed=%d err=%v, want the three accepted sessions", len(listed), err)
	}
	// The goal is declared once: there is no update route for it.
	reread, err := st.GetPlanningSession(ctx, drafting.ID)
	if err != nil || reread.Goal != core.PlanningGoalRequirement {
		t.Fatalf("re-read session=%+v err=%v", reread, err)
	}
}

func TestPlanningHTTPFallbackRejectsUncheckedPromotions(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()
	ctx := store.WithWorkspace(t.Context(), "demo")
	document, version, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-http-promotion", Name: "Overview"}, core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Retry policy\nRetry twice."})
	if err != nil {
		t.Fatal(err)
	}
	create := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/planning-sessions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for name, body := range map[string]string{
		"missing document": `{"goal":"requirement","promotion":{"document_id":"missing","version":1,"section_anchor":"#retry-policy","target_id":"REQ-1"}}`,
		"invalid anchor":   fmt.Sprintf(`{"goal":"requirement","promotion":{"document_id":%q,"version":%d,"section_anchor":"#missing","target_id":"REQ-1"}}`, document.ID, version.Version),
		"blueprint goal":   fmt.Sprintf(`{"goal":"blueprint","promotion":{"document_id":%q,"version":%d,"section_anchor":"#retry-policy","target_id":"REQ-1"}}`, document.ID, version.Version),
	} {
		t.Run(name, func(t *testing.T) {
			response := create(body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	valid := create(fmt.Sprintf(`{"goal":"requirement","promotion":{"document_id":%q,"version":%d,"section_anchor":"#retry-policy","target_id":"REQ-1"}}`, document.ID, version.Version))
	if valid.Code != http.StatusCreated {
		t.Fatalf("valid promotion status=%d body=%s", valid.Code, valid.Body.String())
	}
	sessions, err := st.ListPlanningSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("persisted sessions=%+v err=%v", sessions, err)
	}
}
