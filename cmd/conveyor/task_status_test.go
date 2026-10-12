package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for conveyor task status (req-operator-next-action REQ-5, AC-5.1,
// AC-5.5, AC-1.4, AC-1.8).

const taskStatusTestTask = "261011-aaaaaa"

func serveTaskStatus(t *testing.T, body string) *client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/"+taskStatusTestTask+"/next-action" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t"}
}

const taskStatusFixture = `{
  "label": "Plan approval pending",
  "actor": "operator",
  "kind": "plan_approval",
  "judgment": true,
  "summary": "Plan v2 awaits your decision.",
  "question": "Approve plan v2?",
  "choices": [
    {"id": "approve", "label": "Approve", "effect": "Accepts the gate.", "method": "POST", "path": "/v1/tasks/261011-aaaaaa/review", "capability": "operate_gates", "allowed": true},
    {"id": "redirect", "label": "Redirect", "effect": "Sends the task back with guidance.", "method": "POST", "path": "/v1/tasks/261011-aaaaaa/review", "capability": "operate_gates", "allowed": true},
    {"id": "reject", "label": "Reject", "effect": "Closes the task.", "method": "POST", "path": "/v1/tasks/261011-aaaaaa/review", "capability": "operate_gates", "allowed": false}
  ],
  "if_ignored": "The task stays parked at the plan gate.",
  "deadline": "2026-10-13T08:00:00Z",
  "blocked_by": [{"task_id": "261011-bbbbbb", "title": "Schema migration", "state": "running"}],
  "current_order": {"id": "261011-aaaaaa-spec-2", "stage": "spec", "state": "submitted", "queue_deadline": null, "execution_deadline": null, "lease_expires_at": null},
  "attempt": {"harness": "claude", "model": "opus", "user": "kidus", "claimed_at": null, "last_check_in": null, "lease_expires_at": null, "progress": "drafting", "elapsed_seconds": 42},
  "details": {"gate_kind": "spec", "reason_code": "plan_pending", "refusals": [{"choice": "close", "text": "A spec order is in flight."}]}
}`

func TestTaskStatusPrintsNextAction(t *testing.T) {
	var out bytes.Buffer
	if err := runTaskStatus(serveTaskStatus(t, taskStatusFixture), &out, taskStatusTestTask, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	ordered := []string{
		"Plan approval pending\nPlan v2 awaits your decision.\n",
		"actor:         operator (judgment required)",
		"question:      Approve plan v2?",
		"  - Approve: Accepts the gate.",
		"  - Redirect: Sends the task back with guidance.",
		"  - Reject: Closes the task.",
		"if ignored:    The task stays parked at the plan gate.",
		"deadline:      2026-10-13T08:00:00Z",
		"blocked by:    261011-bbbbbb Schema migration (running)",
		"current order: 261011-aaaaaa-spec-2 (spec, submitted)",
		"attempt:       harness=claude model=opus user=kidus progress=drafting elapsed=42s",
		"refused:       close: A spec order is in flight.",
		"kind:          plan_approval",
		"gate kind:     spec",
		"reason code:   plan_pending",
	}
	at := 0
	for _, want := range ordered {
		index := strings.Index(text[at:], want)
		if index < 0 {
			t.Fatalf("missing or out of order %q after offset %d in:\n%s", want, at, text)
		}
		at += index + len(want)
	}
	if !strings.HasPrefix(text, "Plan approval pending\n") {
		t.Fatalf("label must print first:\n%s", text)
	}
}

func TestTaskStatusChoiceEquivalents(t *testing.T) {
	const task = taskStatusTestTask
	const order = "261011-aaaaaa-implement-1"
	taskPath := "/v1/tasks/" + task
	orderPath := "/v1/work-orders/" + order
	cases := []struct {
		id, method, path string
		want             string
	}{
		{"approve", "POST", taskPath + "/review", "cli: conveyor run " + task},
		{"redirect", "POST", taskPath + "/review", "cli: conveyor run " + task},
		{"approve_revision", "POST", taskPath + "/review", "cli: conveyor run " + task},
		{"decline", "POST", taskPath + "/review", "cli: conveyor run " + task},
		{"reject", "POST", taskPath + "/review", "no CLI command; use the dashboard: POST " + taskPath + "/review"},
		{"request_changes", "POST", taskPath + "/request-changes", "cli: conveyor task request-changes " + task},
		{"merge", "POST", taskPath + "/merge", "cli: conveyor task merge " + task},
		{"close", "POST", taskPath + "/close", "cli: conveyor task close " + task},
		{"fix_merge_conflict", "POST", taskPath + "/merge-conflict-fix", "no CLI command; use the dashboard: POST " + taskPath + "/merge-conflict-fix"},
		{"answer", "POST", orderPath + "/recover", "cli: conveyor order recover " + order + ` --direction "<answer>"`},
		{"applied", "POST", orderPath + "/recover", "no CLI command; use the dashboard: POST " + orderPath + "/recover"},
		{"not_applied", "POST", orderPath + "/recover", "no CLI command; use the dashboard: POST " + orderPath + "/recover"},
		{"confirm", "POST", "/v1/documents/design-x/confirm", "no CLI command; use the dashboard: POST /v1/documents/design-x/confirm"},
		{"dismiss", "POST", "/v1/documents/design-x/dismiss", "no CLI command; use the dashboard: POST /v1/documents/design-x/dismiss"},
		{"recover", "POST", taskPath + "/redispatch", "cli: conveyor order redispatch 261011-aaaaaa-spec-9"},
		{"start_over", "POST", taskPath + "/restart", "cli: conveyor task restart " + task},
		{"recover", "POST", orderPath + "/recover", "cli: conveyor order recover " + order},
		{"redispatch", "POST", taskPath + "/redispatch", "cli: conveyor order redispatch 261011-aaaaaa-spec-9"},
		{"change_policy", "POST", taskPath + "/policy", "no CLI command; use the dashboard: POST " + taskPath + "/policy"},
		{"retry_round", "POST", taskPath + "/review-round/retry", "no CLI command; use the dashboard: POST " + taskPath + "/review-round/retry"},
		{"recover_round", "POST", taskPath + "/review-round/recover", "no CLI command; use the dashboard: POST " + taskPath + "/review-round/recover"},
		{"remove_dependency", "DELETE", taskPath + "/dependencies/261011-cccccc", "cli: conveyor task unlink " + task + " 261011-cccccc"},
	}
	for _, tc := range cases {
		t.Run(tc.id+" "+tc.path, func(t *testing.T) {
			action := nextActionView{
				Label: "L", Actor: "operator", Kind: "k", Summary: "S",
				CurrentOrder: &nextActionOrder{ID: "261011-aaaaaa-spec-9", Stage: "spec", State: "queued"},
				Choices:      []nextActionChoice{{ID: tc.id, Label: "Choice", Effect: "Effect.", Method: tc.method, Path: tc.path, Allowed: true}},
			}
			var out bytes.Buffer
			if err := writeTaskStatus(&out, task, action); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "      "+tc.want+"\n") {
				t.Fatalf("want line %q in:\n%s", tc.want, out.String())
			}
			if strings.Contains(out.String(), "not permitted") {
				t.Fatalf("allowed choice printed as not permitted:\n%s", out.String())
			}
		})
	}

	t.Run("redispatch without current order prints the route", func(t *testing.T) {
		action := nextActionView{Choices: []nextActionChoice{{ID: "redispatch", Method: "POST", Path: taskPath + "/redispatch", Allowed: true}}}
		var out bytes.Buffer
		_ = writeTaskStatus(&out, task, action)
		if !strings.Contains(out.String(), "no CLI command; use the dashboard: POST "+taskPath+"/redispatch") {
			t.Fatalf("output:\n%s", out.String())
		}
	})
	t.Run("absent choices are never synthesized and disallowed ones are marked", func(t *testing.T) {
		action := nextActionView{Choices: []nextActionChoice{{ID: "close", Method: "POST", Path: taskPath + "/close", Allowed: false}}}
		var out bytes.Buffer
		_ = writeTaskStatus(&out, task, action)
		if strings.Contains(out.String(), "task merge") {
			t.Fatalf("merge printed without a merge choice:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "      not permitted for this credential\n") {
			t.Fatalf("allowed=false not marked:\n%s", out.String())
		}
	})
}

func TestTaskStatusJSONVerbatim(t *testing.T) {
	body := `{"label":"Running","actor":"agent","kind":"running","judgment":false,"summary":"s","choices":[],"deadline":null,"details":{},"future_field":{"nested":[1,2]}}`
	var out bytes.Buffer
	if err := runTaskStatus(serveTaskStatus(t, body), &out, taskStatusTestTask, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), body+"\n")
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("output is not JSON")
	}
}
