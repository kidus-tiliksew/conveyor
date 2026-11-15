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

func runTaskSetLaterRoundReviewCommand(t *testing.T, args []string, respond http.HandlerFunc) (string, error, []distinctModelsCall) {
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
	command := taskSetLaterRoundReviewCmd()
	command.SetArgs(args)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	server.Close()
	return output.String(), err, calls
}

// TestTaskSetLaterRoundReviewCommand covers conveyor task
// set-later-round-review: the policy route, the resolved credential and
// workspace, a generated request ID, a body holding only this policy field,
// and the printed request ID (feature-delta-review-rounds DR-6;
// req-review-gates-evidence AC-12.8).
func TestTaskSetLaterRoundReviewCommand(t *testing.T) {
	for _, tc := range []struct{ flag, value string }{{"--full", "full"}, {"--delta", "delta"}} {
		output, err, calls := runTaskSetLaterRoundReviewCommand(t, []string{"task/1", tc.flag, "--reason", "review only new changes"}, func(w http.ResponseWriter, _ *http.Request) {
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
		if len(call.body) != 3 || call.body["later_round_review"] != tc.value || call.body["reason"] != "review only new changes" || len(requestID) != 32 {
			t.Fatalf("body=%+v", call.body)
		}
		if !strings.Contains(output, "later_round_review to "+tc.value) || !strings.Contains(output, "task/1") || !strings.Contains(output, "(request echo)") {
			t.Fatalf("output=%q", output)
		}
	}
}

// Invalid local input fails before any request is sent, and a server refusal
// is a command failure.
func TestTaskSetLaterRoundReviewRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"task-1", "--reason", "x"},
		{"task-1", "--full", "--delta", "--reason", "x"},
		{"task-1", "--delta"},
		{"task-1", "--full", "--reason", "  "},
		{" ", "--delta", "--reason", "x"},
		{"--delta", "--reason", "x"},
	} {
		_, err, calls := runTaskSetLaterRoundReviewCommand(t, args, func(http.ResponseWriter, *http.Request) {})
		if err == nil || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", args, err, calls)
		}
	}
	_, err, _ := runTaskSetLaterRoundReviewCommand(t, []string{"task-1", "--delta", "--reason", "x"}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"setup change conflict: terminal task task-1 cannot change policy"}`))
	})
	if err == nil || !strings.Contains(err.Error(), "terminal task") {
		t.Fatalf("server refusal err=%v", err)
	}
}

// The command is registered on conveyor task beside set-distinct-models.
func TestTaskSetLaterRoundReviewRegistered(t *testing.T) {
	root := newRootCmd()
	command, _, err := root.Find([]string{"task", "set-later-round-review"})
	if err != nil || command == nil || command.Name() != "set-later-round-review" {
		t.Fatalf("command=%v err=%v", command, err)
	}
}
