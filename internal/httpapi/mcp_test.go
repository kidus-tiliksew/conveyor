package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMCPToolsListRequiresAuthAndPublishesLifecycle(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	handler := server.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	want := []string{"create_task", "list_work_orders", "claim_work_order", "redispatch_work_order", "get_work_order", "report_progress", "report_usage", "upload_transcript", "submit_for_review", "await_review", "submit_review_verdict"}
	if len(envelope.Result.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(envelope.Result.Tools), len(want))
	}
	for i, name := range want {
		if envelope.Result.Tools[i].Name != name {
			t.Fatalf("tool[%d] = %q, want %q", i, envelope.Result.Tools[i].Name, name)
		}
	}
}

func TestMCPCreateTaskEnqueuesTriageIdempotently(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken = "operator-token"
	server.Workspace = "demo"
	server.Repos = []string{"api"}
	enqueued := 0
	server.OnCreate = func(context.Context, string) { enqueued++ }
	handler := server.Handler()

	call := func(title string) (core.Task, bool, bool) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_task","arguments":{"title":` + fmt.Sprintf("%q", title) + `,"body":"from an MCP issue","repo":"api","source":"mcp:test-issue","level":"L2","idempotency_key":"issue-42"}}}`
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer operator-token")
		request.Header.Set("X-Conveyor-Actor", "issue-triage-agent")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.IsError {
			return core.Task{}, false, true
		}
		var result struct {
			Task    core.Task `json:"task"`
			Created bool      `json:"created"`
		}
		if len(envelope.Result.Content) != 1 {
			t.Fatalf("content = %+v", envelope.Result.Content)
		}
		if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &result); err != nil {
			t.Fatal(err)
		}
		return result.Task, result.Created, false
	}

	first, created, failed := call("Triage this issue")
	if failed || !created || first.State != core.TaskQueued || first.NextStage != core.StageTriage {
		t.Fatalf("first task=%+v created=%t failed=%t", first, created, failed)
	}
	second, created, failed := call("Triage this issue")
	if failed || created || second.ID != first.ID || enqueued != 1 {
		t.Fatalf("retry task=%+v created=%t failed=%t enqueued=%d", second, created, failed, enqueued)
	}
	if _, _, failed = call("Different issue"); !failed {
		t.Fatal("reusing the idempotency key for different input succeeded")
	}
	tasks, err := st.ListTasks(t.Context())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	events, err := st.ListEvents(t.Context(), first.ID)
	if err != nil || len(events) != 1 || events[0].ActorID != "issue-triage-agent" || events[0].ActorRole != core.ActorAgent {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}
