package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type distinctModelsCall struct {
	method, path, authorization, workspace string
	body                                   map[string]any
}

func runTaskSetDistinctModelsCommand(t *testing.T, args []string, respond http.HandlerFunc) (string, error, []distinctModelsCall) {
	t.Helper()
	isolateLocalAuthTest(t)
	var calls []distinctModelsCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := distinctModelsCall{method: r.Method, path: r.URL.EscapedPath(), authorization: r.Header.Get("Authorization"), workspace: r.Header.Get("X-Workspace-ID")}
		if err := json.Unmarshal(raw, &call.body); err != nil {
			t.Errorf("body %q: %v", raw, err)
		}
		calls = append(calls, call)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "user-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	var output bytes.Buffer
	command := taskSetDistinctModelsCmd()
	command.SetArgs(args)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	server.Close()
	return output.String(), err, calls
}

// TestTaskSetDistinctModelsCommand covers conveyor task set-distinct-models:
// the policy route, the resolved credential and workspace, a generated request
// ID, and a body holding only this policy field (req-review-gates-evidence
// AC-2.11).
func TestTaskSetDistinctModelsCommand(t *testing.T) {
	for _, tc := range []struct {
		flag  string
		value bool
	}{{"--on", true}, {"--off", false}} {
		output, err, calls := runTaskSetDistinctModelsCommand(t, []string{"task/1", tc.flag, "--reason", "panel repeats a model"}, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"echo"}`))
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.flag, err)
		}
		if len(calls) != 1 {
			t.Fatalf("calls=%+v", calls)
		}
		call := calls[0]
		if call.method != http.MethodPost || call.path != "/v1/tasks/task%2F1/policy" || call.authorization != "Bearer user-secret-token" || call.workspace != "demo" {
			t.Fatalf("call=%+v", call)
		}
		requestID, _ := call.body["request_id"].(string)
		if len(call.body) != 3 || call.body["review_distinct_models"] != tc.value || call.body["reason"] != "panel repeats a model" || len(requestID) != 32 {
			t.Fatalf("body=%+v", call.body)
		}
		if !strings.Contains(output, "task/1") || !strings.Contains(output, "(request echo)") {
			t.Fatalf("output=%q", output)
		}
	}
}

// Invalid local input fails before any request is sent, and a server refusal
// is a command failure.
func TestTaskSetDistinctModelsRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"task-1", "--reason", "x"},
		{"task-1", "--on", "--off", "--reason", "x"},
		{"task-1", "--on"},
		{"task-1", "--off", "--reason", "  "},
		{" ", "--on", "--reason", "x"},
		{"--on", "--reason", "x"},
	} {
		_, err, calls := runTaskSetDistinctModelsCommand(t, args, func(http.ResponseWriter, *http.Request) {})
		if err == nil || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", args, err, calls)
		}
	}
	_, err, _ := runTaskSetDistinctModelsCommand(t, []string{"task-1", "--on", "--reason", "x"}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"setup change conflict: task has an executing attempt or in-flight review verdict"}`))
	})
	if err == nil || !strings.Contains(err.Error(), "in-flight review verdict") {
		t.Fatalf("server refusal err=%v", err)
	}
}
