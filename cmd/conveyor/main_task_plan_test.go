package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// planCapture records each request the CLI sends and answers it with reply.
type planCapture struct {
	mu       sync.Mutex
	requests []capturedPlanRequest
}

type capturedPlanRequest struct {
	Method, Path string
	Body         map[string]any
}

func (c *planCapture) all() []capturedPlanRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedPlanRequest(nil), c.requests...)
}

func planCaptureServer(t *testing.T, status int, reply any) *planCapture {
	t.Helper()
	capture := &planCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		capture.mu.Lock()
		capture.requests = append(capture.requests, capturedPlanRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	return capture
}

func runTaskRoot(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := taskRoot()
	var output strings.Builder
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return output.String(), err
}

func writeTaskPlanFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cliPlan = "## Approach\nDo it.\n\n## Done criteria\n- Done."

// component-runtime invariant 40: each filing flag maps to its intake field,
// and an omitted flag sends no field.
func TestTaskNewPlanFlagsMapToIntakeFields(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	planPath := writeTaskPlanFile(t, cliPlan)
	cases := []struct {
		name  string
		stdin string
		args  []string
		want  map[string]any
	}{
		{"omitted", "", nil, map[string]any{}},
		{"needed", "", []string{"--plan-state", "needed"}, map[string]any{"plan_state": "needed"}},
		{"not required", "", []string{"--plan-state", "not-required"}, map[string]any{"plan_state": "not_required"}},
		{"plan file", "", []string{"--plan-file", planPath}, map[string]any{"plan": cliPlan}},
		{"plan stdin", cliPlan, []string{"--plan-file", "-"}, map[string]any{"plan": cliPlan}},
		{"plan base and approval", "", []string{"--plan-file", planPath, "--plan-base", commit, "--approve-plan"},
			map[string]any{"plan": cliPlan, "plan_base_commit": commit, "plan_approved": true}},
		{"merge gate", "", []string{"--merge-approval", "on"}, map[string]any{"merge_approval": true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			capture := planCaptureServer(t, http.StatusOK, core.Task{ID: "task-1", Branch: "conveyor/task-task-1"})
			args := append([]string{"task", "new", "--repo", "api", "-m", "fix it"}, test.args...)
			if output, err := runTaskRoot(t, test.stdin, args...); err != nil {
				t.Fatalf("task new: %v\n%s", err, output)
			}
			requests := capture.all()
			if len(requests) != 1 || requests[0].Method != http.MethodPost || requests[0].Path != "/v1/tasks" {
				t.Fatalf("requests = %+v", requests)
			}
			body := requests[0].Body
			for _, field := range []string{"plan_state", "plan", "plan_base_commit", "plan_approved", "merge_approval", "spec_approval"} {
				want, wanted := test.want[field]
				got, sent := body[field]
				if wanted != sent {
					t.Fatalf("field %s sent=%t, want sent=%t (body %v)", field, sent, wanted, body)
				}
				if wanted && got != want {
					t.Fatalf("field %s = %#v, want %#v", field, got, want)
				}
			}
		})
	}
}

// component-runtime invariant 40: the flag combinations the CLI refuses fail
// before any request, and --spec-approval is no longer a flag.
func TestTaskNewPlanFlagRefusalsSendNothing(t *testing.T) {
	planPath := writeTaskPlanFile(t, cliPlan)
	for name, args := range map[string][]string{
		"plan base without file":     {"--plan-base", "0123456789abcdef0123456789abcdef01234567"},
		"approve without file":       {"--approve-plan"},
		"plan state with file":       {"--plan-state", "needed", "--plan-file", planPath},
		"unknown plan state":         {"--plan-state", "not_required"},
		"retired spec approval flag": {"--spec-approval", "on"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := planCaptureServer(t, http.StatusOK, core.Task{ID: "task-1"})
			if _, err := runTaskRoot(t, "", append([]string{"task", "new", "--repo", "api", "-m", "fix it"}, args...)...); err == nil {
				t.Fatal("task new accepted a refused flag combination")
			}
			if requests := capture.all(); len(requests) != 0 {
				t.Fatalf("requests = %+v, want none", requests)
			}
		})
	}
}

// component-runtime invariant 41: task plan-state posts plan_state, the
// reason, and a fresh request ID to /policy and prints the response's
// request ID.
func TestTaskPlanStatePostsPolicyChange(t *testing.T) {
	for value, wire := range map[string]string{"needed": "needed", "not-required": "not_required"} {
		capture := planCaptureServer(t, http.StatusOK, map[string]string{"request_id": "req-from-server"})
		output, err := runTaskRoot(t, "", "task", "plan-state", "task-1", value, "--reason", "needs design")
		if err != nil {
			t.Fatalf("plan-state %s: %v\n%s", value, err, output)
		}
		requests := capture.all()
		if len(requests) != 1 || requests[0].Method != http.MethodPost || requests[0].Path != "/v1/tasks/task-1/policy" {
			t.Fatalf("requests = %+v", requests)
		}
		body := requests[0].Body
		requestID, _ := body["request_id"].(string)
		if len(body) != 3 || body["plan_state"] != wire || body["reason"] != "needs design" || len(requestID) != 32 {
			t.Fatalf("body = %#v", body)
		}
		if !strings.Contains(output, "req-from-server") {
			t.Fatalf("output %q does not name the request ID", output)
		}
	}
	first := planCaptureServer(t, http.StatusOK, map[string]string{"request_id": "x"})
	for range 2 {
		if _, err := runTaskRoot(t, "", "task", "plan-state", "task-1", "needed", "--reason", "r"); err != nil {
			t.Fatal(err)
		}
	}
	if requests := first.all(); len(requests) != 2 || requests[0].Body["request_id"] == requests[1].Body["request_id"] {
		t.Fatalf("request IDs are not fresh per run: %+v", requests)
	}
}

// component-runtime invariant 41: task attach-plan posts the plan, the base
// commit only when given, the reason, and a fresh request ID to /plan.
func TestTaskAttachPlanPostsPlan(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	planPath := writeTaskPlanFile(t, cliPlan)
	for name, test := range map[string]struct {
		stdin string
		args  []string
		base  bool
	}{
		"file":        {"", []string{"--plan-file", planPath}, false},
		"stdin":       {cliPlan, []string{"--plan-file", "-"}, false},
		"with commit": {"", []string{"--plan-file", planPath, "--plan-base", commit}, true},
	} {
		t.Run(name, func(t *testing.T) {
			capture := planCaptureServer(t, http.StatusOK, map[string]any{"request_id": "req-attach", "plan_version": 2})
			output, err := runTaskRoot(t, test.stdin, append([]string{"task", "attach-plan", "task-1", "--reason", "operator plan"}, test.args...)...)
			if err != nil {
				t.Fatalf("attach-plan: %v\n%s", err, output)
			}
			requests := capture.all()
			if len(requests) != 1 || requests[0].Method != http.MethodPost || requests[0].Path != "/v1/tasks/task-1/plan" {
				t.Fatalf("requests = %+v", requests)
			}
			body := requests[0].Body
			requestID, _ := body["request_id"].(string)
			if body["plan"] != cliPlan || body["reason"] != "operator plan" || len(requestID) != 32 {
				t.Fatalf("body = %#v", body)
			}
			if _, sent := body["base_commit"]; sent != test.base || (test.base && body["base_commit"] != commit) {
				t.Fatalf("base_commit sent=%t body=%#v", sent, body)
			}
			if !strings.Contains(output, "req-attach") || !strings.Contains(output, "version 2") {
				t.Fatalf("output = %q", output)
			}
		})
	}
}

// component-runtime invariant 41: a blank reason is refused before any
// request, and a server refusal exits non-zero.
func TestTaskPlanCommandsRefuseBlankReasonAndSurfaceRefusals(t *testing.T) {
	planPath := writeTaskPlanFile(t, cliPlan)
	for name, args := range map[string][]string{
		"plan-state":          {"task", "plan-state", "task-1", "needed", "--reason", "  "},
		"attach-plan":         {"task", "attach-plan", "task-1", "--plan-file", planPath, "--reason", ""},
		"plan-state value":    {"task", "plan-state", "task-1", "awaiting_approval", "--reason", "r"},
		"attach without file": {"task", "attach-plan", "task-1", "--reason", "r"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := planCaptureServer(t, http.StatusOK, map[string]string{"request_id": "x"})
			if _, err := runTaskRoot(t, "", args...); err == nil {
				t.Fatal("command accepted a refused input")
			}
			if requests := capture.all(); len(requests) != 0 {
				t.Fatalf("requests = %+v, want none", requests)
			}
		})
	}
	for name, args := range map[string][]string{
		"plan-state":  {"task", "plan-state", "task-1", "needed", "--reason", "r"},
		"attach-plan": {"task", "attach-plan", "task-1", "--plan-file", planPath, "--reason", "r"},
	} {
		t.Run(name+" refused", func(t *testing.T) {
			capture := planCaptureServer(t, http.StatusConflict, map[string]string{"error": "setup change conflict: task has a claimed implement order"})
			output, err := runTaskRoot(t, "", args...)
			if err == nil {
				t.Fatalf("a 409 refusal exited zero: %s", output)
			}
			if requests := capture.all(); len(requests) != 1 {
				t.Fatalf("requests = %+v", requests)
			}
		})
	}
}

// component-runtime invariant 40: task show prints the task's plan_state.
func TestTaskShowPrintsPlanState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/tasks/task-1":
			_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", PlanState: core.PlanAwaitingApproval})
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			_ = json.NewEncoder(w).Encode([]core.Job{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	output, err := runTaskRoot(t, "", "task", "show", "task-1")
	if err != nil {
		t.Fatalf("task show: %v\n%s", err, output)
	}
	if !strings.Contains(output, `"plan_state": "awaiting_approval"`) || !strings.Contains(output, "Plan state: awaiting_approval") {
		t.Fatalf("task show output lacks plan_state:\n%s", output)
	}
}
