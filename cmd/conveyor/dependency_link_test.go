package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskLinkCommandRequiresAuditFieldsAndCallsREST(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/dependencies" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(store.DependencyAdditionResult{RequestID: body["request_id"], Added: true})
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	command := addTaskDependencyCmd()
	var output strings.Builder
	command.SetOut(&output)
	command.SetArgs([]string{"task-1", "task-2", "--reason", "deliver first", "--request-id", "link-request"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if body["depends_on_task_id"] != "task-2" || body["reason"] != "deliver first" || body["request_id"] != "link-request" || !strings.Contains(output.String(), "task task-1 now depends on task-2") {
		t.Fatalf("body=%v output=%q", body, output.String())
	}

	missing := addTaskDependencyCmd()
	missing.SetArgs([]string{"task-1", "task-2", "--request-id", "missing-reason"})
	if err := missing.Execute(); err == nil || !strings.Contains(err.Error(), "--reason and --request-id are required") {
		t.Fatalf("missing flags error=%v", err)
	}
}
