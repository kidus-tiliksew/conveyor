package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskSetupCommandSendsExplicitFutureOnlyChange(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/setup" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(store.SetupChangeResult{Task: core.Task{ID: "task-1", SetupName: "next"}, ReviewTransition: "none"})
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	command := changeTaskSetupCmd()
	var output strings.Builder
	command.SetOut(&output)
	command.SetArgs([]string{"task-1", "--setup", "next", "--reason", "repair routing", "--request-id", "request-1"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if body["setup"] != "next" || body["reason"] != "repair routing" || body["request_id"] != "request-1" || !strings.Contains(output.String(), "affects future work only") {
		t.Fatalf("body=%v output=%q", body, output.String())
	}
}

func TestTaskSetupCommandAllowsOmittedReason(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(store.SetupChangeResult{Task: core.Task{ID: "task-1", SetupName: "next"}, ReviewTransition: "none"})
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	command := changeTaskSetupCmd()
	command.SetArgs([]string{"task-1", "--setup", "next", "--request-id", "request-blank-reason"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "" || body["request_id"] != "request-blank-reason" {
		t.Fatalf("body=%v", body)
	}
}
