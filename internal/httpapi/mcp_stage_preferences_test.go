package httpapi

// MCP tools for stage preferences against the real router and a volatile
// backend (req-stage-preferences AC-1.5, AC-1.6, AC-2.3, AC-2.4;
// component-stage-preferences invariants 3, 4, and 6; component-mcp-protocol
// invariants 5 and 7).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type stagePreferenceMCPResult struct {
	text    string
	isError bool
}

func (h *stagePreferenceHarness) mcp(token, tool string, args map[string]any) stagePreferenceMCPResult {
	h.t.Helper()
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": args})
	if err != nil {
		h.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		h.t.Fatalf("MCP status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		h.t.Fatal(err)
	}
	if envelope.Error != nil {
		h.t.Fatalf("MCP protocol error: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Content) != 1 {
		h.t.Fatalf("MCP content = %s", response.Body.String())
	}
	return stagePreferenceMCPResult{text: envelope.Result.Content[0].Text, isError: envelope.Result.IsError}
}

func (h *stagePreferenceHarness) mcpSet(token, tool string, args map[string]any) store.StagePreferences {
	h.t.Helper()
	result := h.mcp(token, tool, args)
	if result.isError {
		h.t.Fatalf("%s failed: %s", tool, result.text)
	}
	var set store.StagePreferences
	if err := json.Unmarshal([]byte(result.text), &set); err != nil {
		h.t.Fatalf("decode %s: %v", result.text, err)
	}
	return set
}

func (h *stagePreferenceHarness) mcpRefused(token, tool string, args map[string]any, want ...string) {
	h.t.Helper()
	result := h.mcp(token, tool, args)
	if !result.isError {
		h.t.Fatalf("%s %v succeeded: %s", tool, args, result.text)
	}
	for _, part := range want {
		if !strings.Contains(result.text, part) {
			h.t.Fatalf("%s refusal %q does not name %q", tool, result.text, part)
		}
	}
}

func TestStagePreferenceMCPToolSchemas(t *testing.T) {
	tools := map[string]map[string]any{}
	for _, tool := range mcpTools() {
		if name, _ := tool["name"].(string); isStagePreferenceMCPTool(name) {
			tools[name] = tool
		}
	}
	if len(tools) != 2 {
		t.Fatalf("stage preference tools = %v", tools)
	}
	for name, tool := range tools {
		schema := tool["inputSchema"].(map[string]any)
		required := schema["required"].([]string)
		if required[0] != "workspace_id" || schema["additionalProperties"] != false {
			t.Fatalf("%s schema = %v", name, schema)
		}
		if mcpCapability(name) == "" {
			t.Fatalf("%s has no capability", name)
		}
	}
	if hints := tools[getStagePreferencesTool]["annotations"].(map[string]any); hints["readOnlyHint"] != true || hints["destructiveHint"] != false {
		t.Fatalf("get_stage_preferences hints = %v", hints)
	}
	if hints := tools[setStagePreferenceTool]["annotations"].(map[string]any); hints["readOnlyHint"] != false {
		t.Fatalf("set_stage_preference hints = %v", hints)
	}
	set := tools[setStagePreferenceTool]["inputSchema"].(map[string]any)
	if fmt.Sprint(set["required"]) != "[workspace_id stage value]" {
		t.Fatalf("set_stage_preference required = %v", set["required"])
	}
	if _, versioned := set["properties"].(map[string]any)["version"]; versioned {
		t.Fatal("set_stage_preference takes no expected version")
	}
	stage := set["properties"].(map[string]any)["stage"].(map[string]any)
	if fmt.Sprint(stage["enum"]) != "[plan implement verify risk review]" {
		t.Fatalf("set_stage_preference stage enum = %v, want the five stages", stage["enum"])
	}
	for _, name := range []string{getStagePreferencesTool, setStagePreferenceTool} {
		if description, _ := tools[name]["description"].(string); !strings.Contains(description, "risk") {
			t.Fatalf("%s description does not name the risk stage: %s", name, description)
		}
	}
}

// TestStagePreferenceMCPRoundTrip saves through MCP and REST and reads the
// same set and version back through the other surface and from both
// workspaces (invariant 6; AC-2.3, AC-2.4).
func TestStagePreferenceMCPRoundTrip(t *testing.T) {
	h := newStagePreferenceHarness(t)
	empty := h.mcpSet(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
	if empty.Version != 0 || empty.UpdatedAt != nil || empty.Stages != (store.StagePreferenceStages{}) {
		t.Fatalf("never-saved MCP read = %+v", empty)
	}

	plan := h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": map[string]any{"harness": " codex ", "model": "gpt-6.1-sol", "effort": "medium"}})
	if plan.Version != 1 || plan.Stages.Plan == nil || plan.Stages.Plan.Harness != "codex" {
		t.Fatalf("set plan = %+v", plan)
	}
	review := h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "beta", "stage": "review", "value": map[string]any{
		"primary": map[string]any{"harness": "claude-code", "model": "claude-opus-5-5", "effort": nil},
		"seats":   []any{map[string]any{"harness": "codex", "model": "gpt-6.1-sol"}, map[string]any{"harness": "claude-code", "model": "claude-sonnet-5-5"}},
	}})
	if review.Version != 2 || review.Stages.Plan == nil || review.Stages.Review == nil || len(review.Stages.Review.Seats) != 2 {
		t.Fatalf("set review kept plan? %+v", review)
	}

	// REST, through the dashboard session, reads what MCP saved.
	rest := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, session: h.ownerSession}))
	if rest.Version != review.Version || rest.Stages.Review.Seats[1].Model != "claude-sonnet-5-5" || rest.Stages.Plan.Effort != "medium" {
		t.Fatalf("REST read %+v, MCP saved %+v", rest, review)
	}
	// MCP reads what REST saved, from either workspace.
	saved := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken, body: `{"stages":{"verify":{"harness":"opencode","model":"local"},"review":{"seats":[{"harness":"codex","model":"gpt-6.1-sol"}]}}}`, header: map[string]string{"If-Match": "2"}}))
	for _, workspace := range []string{"alpha", "beta"} {
		read := h.mcpSet(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": workspace})
		if read.Version != saved.Version || read.Stages.Plan != nil || read.Stages.Verify == nil || read.Stages.Review.Primary != nil || len(read.Stages.Review.Seats) != 1 {
			t.Fatalf("%s MCP read %+v, REST saved %+v", workspace, read, saved)
		}
	}

	// The risk stage holds one entry, keeps every other stage, and reads the
	// same through REST (AC-1.1, AC-2.3; DEC-135(13)).
	withRisk := h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "beta", "stage": "risk", "value": map[string]any{"harness": "claude-code", "model": " claude-haiku-5-5 ", "effort": "low"}})
	if withRisk.Version != saved.Version+1 || withRisk.Stages.Risk == nil || withRisk.Stages.Risk.Model != "claude-haiku-5-5" || withRisk.Stages.Verify == nil || withRisk.Stages.Review == nil {
		t.Fatalf("set risk = %+v", withRisk)
	}
	if rest := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, session: h.ownerSession})); rest.Version != withRisk.Version || !reflect.DeepEqual(rest.Stages, withRisk.Stages) {
		t.Fatalf("REST read %+v, MCP saved %+v", rest, withRisk)
	}
	riskCleared := h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "risk", "value": nil})
	if riskCleared.Version != withRisk.Version+1 || riskCleared.Stages.Risk != nil || riskCleared.Stages.Verify == nil || riskCleared.Stages.Review == nil {
		t.Fatalf("clear risk = %+v", riskCleared)
	}
	saved = riskCleared

	// null clears one stage and keeps the others.
	cleared := h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "verify", "value": nil})
	if cleared.Version != saved.Version+1 || cleared.Stages.Verify != nil || cleared.Stages.Review == nil {
		t.Fatalf("clear verify = %+v", cleared)
	}
	cleared = h.mcpSet(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "review", "value": map[string]any{"primary": nil, "seats": []any{}}})
	if cleared.Stages.Review != nil {
		t.Fatalf("an empty review was not unset: %+v", cleared.Stages.Review)
	}
	if got := h.eventCount(); got != h.recordedEvents {
		t.Fatalf("stage preference writes appended %d events", got-h.recordedEvents)
	}
}

// TestStagePreferenceMCPRefusals covers the closed shape, the workspace
// admission, and person isolation (AC-1.5, AC-1.6).
func TestStagePreferenceMCPRefusals(t *testing.T) {
	h := newStagePreferenceHarness(t)
	entry := map[string]any{"harness": "codex", "model": "m"}
	nine := make([]any, 9)
	for i := range nine {
		nine[i] = entry
	}
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": entry, "execution_host": "box"}, "validation_failed", "execution_host")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": entry, "user_id": h.other.ID}, "validation_failed", "user_id")
	h.mcpRefused(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha", "user_id": h.other.ID}, "user_id")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": map[string]any{"harness": "codex", "model": "m", "sandbox": "none"}}, "value.sandbox")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": []any{entry}}, "value", "not a list")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "review", "value": map[string]any{"seats": nine}}, "value.seats[8]", "review seat 9")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "review", "value": map[string]any{"seats": []any{entry, map[string]any{"harness": "", "model": "m"}}}}, "value.seats[1].harness", "review seat 2")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "triage", "value": nil}, "stage")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "risk", "value": []any{entry}}, "value", "not a list")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "risk", "value": map[string]any{"primary": entry}}, "value.primary")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "risk", "value": map[string]any{"harness": "codex", "model": "m", "command": "run"}}, "value.command")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan"}, "value", "null")
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "value": nil}, "stage")
	// Workspace admission: the ID is required and must be visible to the
	// caller, though it never selects the set.
	h.mcpRefused(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "missing"})
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "missing", "stage": "plan", "value": entry})
	h.mcpRefused(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{})
	h.mcpRefused(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": ""})
	if read := h.mcpSet(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); read.Version != 0 {
		t.Fatalf("refused calls wrote a set: %+v", read)
	}

	// Each token reads and writes only its own person's set.
	h.mcpSet(h.otherToken, setStagePreferenceTool, map[string]any{"workspace_id": "beta", "stage": "implement", "value": entry})
	if read := h.mcpSet(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "beta"}); read.Version != 0 || read.Stages.Implement != nil {
		t.Fatalf("another person's write reached the owner: %+v", read)
	}
	if read := h.mcpSet(h.otherToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}); read.Version != 1 || read.Stages.Implement == nil {
		t.Fatalf("other person's own read = %+v", read)
	}
}

// TestStagePreferenceMCPConcurrentStagesKeepEveryChange proves the lost-update
// guard at the tool boundary: concurrent one-stage writes from several agent
// sessions of one person all land (invariant 4).
func TestStagePreferenceMCPConcurrentStagesKeepEveryChange(t *testing.T) {
	h := newStagePreferenceHarness(t)
	var wg sync.WaitGroup
	stages := []string{"plan", "implement", "verify", "risk", "review"}
	results := make([]stagePreferenceMCPResult, len(stages))
	for i, stage := range stages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value := map[string]any{"harness": "h-" + stage, "model": "m"}
			if stage == "review" {
				value = map[string]any{"primary": value}
			}
			results[i] = h.mcp(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": stage, "value": value})
		}()
	}
	wg.Wait()
	for _, result := range results {
		if result.isError {
			t.Fatal(result.text)
		}
	}
	read := h.mcpSet(stagePreferenceOwnerToken, getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
	if read.Version != int64(len(stages)) || read.Stages.Plan == nil || read.Stages.Implement == nil || read.Stages.Verify == nil || read.Stages.Risk == nil || read.Stages.Review == nil {
		t.Fatalf("concurrent stage writes lost a change: %+v", read)
	}
}

// erroringStagePreferences returns err from every store call, modelling a
// backend that passed a driver or transport error through untranslated.
type erroringStagePreferences struct{ err error }

func (e erroringStagePreferences) GetStagePreferences(context.Context, string) (store.StagePreferences, error) {
	return store.StagePreferences{}, e.err
}

func (e erroringStagePreferences) UpdateStagePreferences(context.Context, string, store.StagePreferenceUpdate) (store.StagePreferences, error) {
	return store.StagePreferences{}, e.err
}

// mcpWithRequestID calls a tool with a fixed X-Request-Id, so the response's
// request ID can be asserted.
func (h *stagePreferenceHarness) mcpWithRequestID(requestID, tool string, args map[string]any) stagePreferenceMCPResult {
	h.t.Helper()
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": args})
	if err != nil {
		h.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	request.Header.Set("Authorization", "Bearer "+stagePreferenceOwnerToken)
	request.Header.Set(middleware.RequestIDHeader, requestID)
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result.Content) != 1 {
		h.t.Fatalf("MCP response %s: %v", response.Body.String(), err)
	}
	return stagePreferenceMCPResult{text: envelope.Result.Content[0].Text, isError: envelope.Result.IsError}
}

// TestStagePreferenceMCPStoreTransportErrorIsGeneric covers component-
// persistence Errors invariants 11 to 13 and component-mcp-protocol Transport
// invariant 2: a host-bearing dial error that a backend passes through, as the
// SingleStore driver can during an outage, never reaches the MCP response.
// Both tools answer the generic backend error with the request ID.
func TestStagePreferenceMCPStoreTransportErrorIsGeneric(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 77, 3, 9), Port: 3306}, Err: errors.New("connect: connection refused")}
	for _, cause := range []error{dial, fmt.Errorf("[mysql] driver: bad connection to db-secret.internal: %w", dial)} {
		h := newStagePreferenceHarness(t)
		h.server.StagePreferences = erroringStagePreferences{err: cause}
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{getStagePreferencesTool, map[string]any{"workspace_id": "alpha"}},
			{setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": map[string]any{"harness": "codex", "model": "m"}}},
		} {
			requestID := "req-stage-preferences-" + call.tool
			result := h.mcpWithRequestID(requestID, call.tool, call.args)
			if !result.isError {
				t.Fatalf("%s succeeded on a store failure: %s", call.tool, result.text)
			}
			for _, leaked := range []string{"10.77.3.9", "3306", "dial", "tcp", "connection refused", "mysql", "db-secret", "bad connection"} {
				if strings.Contains(result.text, leaked) {
					t.Fatalf("%s response %q carries driver or host text %q", call.tool, result.text, leaked)
				}
			}
			if !strings.Contains(result.text, store.ErrBackendOperation.Error()) || !strings.Contains(result.text, "(request_id "+requestID+")") {
				t.Fatalf("%s response %q lacks the generic backend error and its request ID", call.tool, result.text)
			}
		}
	}
}

// TestStagePreferenceMCPKeepsDomainErrors proves the generic answer replaces
// only unexpected store failures: cancellation and retryable conflicts keep
// their meaning, and validation still names the field.
func TestStagePreferenceMCPKeepsDomainErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, context.Canceled.Error()},
		{fmt.Errorf("read: %w", context.DeadlineExceeded), context.DeadlineExceeded.Error()},
		{fmt.Errorf("deadlock on 10.77.3.9: %w", store.ErrRetryable), store.ErrRetryable.Error()},
	} {
		h := newStagePreferenceHarness(t)
		h.server.StagePreferences = erroringStagePreferences{err: tc.err}
		result := h.mcpWithRequestID("req-domain", getStagePreferencesTool, map[string]any{"workspace_id": "alpha"})
		if !result.isError || result.text != tc.want {
			t.Fatalf("store error %v answered %q, want %q", tc.err, result.text, tc.want)
		}
	}
	h := newStagePreferenceHarness(t)
	h.server.StagePreferences = erroringStagePreferences{err: errors.New("never reached")}
	h.mcpRefused(stagePreferenceOwnerToken, setStagePreferenceTool, map[string]any{"workspace_id": "alpha", "stage": "plan", "value": map[string]any{"harness": "", "model": "m"}}, "validation_failed", "value.harness")
}
