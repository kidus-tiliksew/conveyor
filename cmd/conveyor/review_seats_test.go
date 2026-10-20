package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type reviewSeatsCall struct {
	method, path, authorization, workspace string
	body                                   map[string]any
}

// runTaskAddReviewSeatsCommand executes the cobra command against a test
// server with an explicit server, token, and workspace.
func runTaskAddReviewSeatsCommand(t *testing.T, args []string, respond http.HandlerFunc) (string, error, []reviewSeatsCall) {
	t.Helper()
	isolateLocalAuthTest(t)
	var mu sync.Mutex
	var calls []reviewSeatsCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := reviewSeatsCall{method: r.Method, path: r.URL.EscapedPath(), authorization: r.Header.Get("Authorization"), workspace: r.Header.Get("X-Workspace-ID")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &call.body); err != nil {
				t.Errorf("body %q: %v", raw, err)
			}
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "user-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	var output bytes.Buffer
	command := taskAddReviewSeatsCmd()
	command.SetArgs(args)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	mu.Lock()
	defer mu.Unlock()
	return output.String(), err, append([]reviewSeatsCall(nil), calls...)
}

// echoSeatAddition answers with a result built from the request body.
func echoSeatAddition(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RequestID string `json:"request_id"`
		Add       int    `json:"add"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"request_id": request.RequestID, "task_id": "task/1", "review_round": 2, "prior_seat_count": 3, "seat_count": 3 + request.Add})
}

// TestTaskAddReviewSeatsCommand covers conveyor task add-review-seats: the
// route, the resolved credential and workspace, a generated request ID per
// invocation, the default and explicit counts, and the printed result
// (feature-review-seat-minimum RS-5).
func TestTaskAddReviewSeatsCommand(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range []struct {
		args []string
		add  float64
		want string
	}{
		{[]string{"task/1", "--reason", "second opinion"}, 1, "added 1 review seat(s) to round 2 of task task/1: 3 -> 4 seats"},
		{[]string{"task/1", "-r", "second opinion", "--count", "3"}, 3, "added 3 review seat(s) to round 2 of task task/1: 3 -> 6 seats"},
	} {
		output, err, calls := runTaskAddReviewSeatsCommand(t, tc.args, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"echo","task_id":"task/1","review_round":2,"prior_seat_count":3,"seat_count":` + map[float64]string{1: "4", 3: "6"}[tc.add] + `}`))
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if len(calls) != 1 {
			t.Fatalf("calls=%+v", calls)
		}
		call := calls[0]
		requestID, _ := call.body["request_id"].(string)
		if call.method != http.MethodPost || call.path != "/v1/tasks/task%2F1/review-round/seats" || call.authorization != "Bearer user-secret-token" || call.workspace != "demo" {
			t.Fatalf("call=%+v", call)
		}
		want := map[string]any{"request_id": requestID, "reason": "second opinion", "add": tc.add}
		if strings.TrimSpace(requestID) == "" || len(requestID) != 32 || !reflect.DeepEqual(call.body, want) {
			t.Fatalf("body=%+v, want %+v", call.body, want)
		}
		if seen[requestID] {
			t.Fatalf("request ID %s reused across invocations", requestID)
		}
		seen[requestID] = true
		if !strings.Contains(output, tc.want) || !strings.Contains(output, "(request echo)") {
			t.Fatalf("output=%q, want %q", output, tc.want)
		}
	}
}

// TestTaskAddReviewSeatsValidation: invalid local input fails before any
// request is sent.
func TestTaskAddReviewSeatsValidation(t *testing.T) {
	for _, args := range [][]string{
		{"task-1"},
		{"task-1", "--reason", "  "},
		{"task-1", "--reason", "x", "--count", "0"},
		{"task-1", "--reason", "x", "--count", "-2"},
		{" ", "--reason", "x"},
		{"--reason", "x"},
	} {
		_, err, calls := runTaskAddReviewSeatsCommand(t, args, echoSeatAddition)
		if err == nil || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", args, err, calls)
		}
	}
}

// TestTaskAddReviewSeatsRefusal: a server refusal surfaces its text as a
// command failure.
func TestTaskAddReviewSeatsRefusal(t *testing.T) {
	output, err, calls := runTaskAddReviewSeatsCommand(t, []string{"task-1", "--reason", "x"}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"review_round_settled","message":"review_round_settled: review round 2 of task task-1 has settled"}`))
	})
	if err == nil || !strings.Contains(err.Error(), "review_round_settled") || !strings.Contains(err.Error(), "409") || len(calls) != 1 || strings.Contains(output, "added") {
		t.Fatalf("refusal err=%v output=%q calls=%d", err, output, len(calls))
	}
}
