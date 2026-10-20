package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type reviewSummaryCall struct {
	method, path, rawPath, authorization, workspace string
	body                                            map[string]any
}

// runTaskReviewSummaryCommand executes the cobra command against a test
// server with an explicit server, token, and workspace.
func runTaskReviewSummaryCommand(t *testing.T, stdin string, args []string, respond http.HandlerFunc) (string, error, []reviewSummaryCall) {
	t.Helper()
	isolateLocalAuthTest(t)
	var mu sync.Mutex
	var calls []reviewSummaryCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := reviewSummaryCall{method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(), authorization: r.Header.Get("Authorization"), workspace: r.Header.Get("X-Workspace-ID")}
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
	command := taskReviewSummaryCmd()
	command.SetArgs(args)
	command.SetIn(strings.NewReader(stdin))
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	mu.Lock()
	defer mu.Unlock()
	return output.String(), err, append([]reviewSummaryCall(nil), calls...)
}

const reviewSummaryResultBody = `{"task_id":"task/1","review_round":2,"version":3,"event_id":41,"recorded_at":"2026-10-20T07:00:00Z"}`

func createdSummary(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(reviewSummaryResultBody))
}

// TestTaskReviewSummary covers conveyor task review-summary: file and stdin
// input, the request path and payload, the resolved credential and workspace,
// the printed result, and failures for unreadable or malformed input and
// server refusals (feature-review-round-summary SU-4, SU-7).
func TestTaskReviewSummary(t *testing.T) {
	document := `{"summary":"Seat 2 found a defect.","findings":[{"severity":"blocking","problem":"Defect.","location":"a.go","fix":"Fix.","seats":[2]}]}`
	wantBody := map[string]any{
		"request_id": "req-1", "summary": "Seat 2 found a defect.", "author_model": "claude-opus-5-5",
		"findings": []any{map[string]any{"severity": "blocking", "problem": "Defect.", "location": "a.go", "fix": "Fix.", "seats": []any{float64(2)}}},
	}

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "summary.json")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err, calls := runTaskReviewSummaryCommand(t, "", []string{"task/1", "--round", "2", "--file", path, "--request-id", "req-1", "--author-model", "claude-opus-5-5"}, createdSummary)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].rawPath != "/v1/tasks/task%2F1/review-rounds/2/summaries" {
			t.Fatalf("calls=%+v", calls)
		}
		if calls[0].authorization != "Bearer user-secret-token" || calls[0].workspace != "demo" {
			t.Fatalf("authorization=%q workspace=%q", calls[0].authorization, calls[0].workspace)
		}
		if !reflect.DeepEqual(calls[0].body, wantBody) {
			t.Fatalf("body=%v\nwant=%v", calls[0].body, wantBody)
		}
		var printed map[string]any
		if err := json.Unmarshal([]byte(output), &printed); err != nil || printed["version"] != float64(3) || printed["event_id"] != float64(41) {
			t.Fatalf("output=%q err=%v", output, err)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		_, err, calls := runTaskReviewSummaryCommand(t, `{"summary":"All seats approved."}`, []string{"task-2", "--round", "1", "--file", "-", "--request-id", "req-2"}, createdSummary)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"request_id": "req-2", "summary": "All seats approved.", "findings": []any{}}
		if len(calls) != 1 || calls[0].path != "/v1/tasks/task-2/review-rounds/1/summaries" || !reflect.DeepEqual(calls[0].body, want) {
			t.Fatalf("calls=%+v", calls)
		}
	})

	for name, tc := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"unreadable file":  {"", []string{"task-3", "--round", "1", "--file", filepath.Join(t.TempDir(), "missing.json"), "--request-id", "r"}, "read review summary input"},
		"malformed json":   {`{"summary":`, []string{"task-3", "--round", "1", "--file", "-", "--request-id", "r"}, "one {summary, findings} JSON object"},
		"unknown field":    {`{"summary":"s","findings":[],"task_id":"x"}`, []string{"task-3", "--round", "1", "--file", "-", "--request-id", "r"}, "unknown field"},
		"trailing value":   {`{"summary":"s"} {}`, []string{"task-3", "--round", "1", "--file", "-", "--request-id", "r"}, "one {summary, findings} JSON object"},
		"missing request":  {`{"summary":"s"}`, []string{"task-3", "--round", "1", "--file", "-"}, "request-id"},
		"missing round":    {`{"summary":"s"}`, []string{"task-3", "--file", "-", "--request-id", "r"}, "round"},
		"blank request id": {`{"summary":"s"}`, []string{"task-3", "--round", "1", "--file", "-", "--request-id", " "}, "--request-id is required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err, calls := runTaskReviewSummaryCommand(t, tc.stdin, tc.args, createdSummary)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if len(calls) != 0 {
				t.Fatalf("invalid input reached the server: %+v", calls)
			}
		})
	}

	for name, status := range map[string]int{"validation": http.StatusBadRequest, "conflict": http.StatusConflict, "capability": http.StatusNotFound} {
		t.Run("server refusal "+name, func(t *testing.T) {
			refusal := "review_summary_invalid: every seat that requested changes must be cited by at least one finding; uncited seats: 2"
			_, err, calls := runTaskReviewSummaryCommand(t, document, []string{"task-4", "--round", "2", "--file", "-", "--request-id", "r"}, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, refusal, status)
			})
			if err == nil || !strings.Contains(err.Error(), refusal) || !strings.Contains(err.Error(), http.StatusText(status)) {
				t.Fatalf("err=%v, want the server refusal", err)
			}
			if len(calls) != 1 {
				t.Fatalf("calls=%+v", calls)
			}
		})
	}
}
