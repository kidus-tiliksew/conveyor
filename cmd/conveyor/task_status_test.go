package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
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
		{"confirm:system_design:design-x:3", "POST", "/v1/system-designs/design-x/versions/3/confirm", "no CLI command; use the dashboard: POST /v1/system-designs/design-x/versions/3/confirm"},
		{"dismiss:system_design:design-x:3", "POST", "/v1/system-designs/design-x/versions/3/dismiss", "no CLI command; use the dashboard: POST /v1/system-designs/design-x/versions/3/dismiss"},
		{"confirm:requirement:req-x:2", "POST", "/v1/requirements/req-x/versions/2/confirm", "no CLI command; use the dashboard: POST /v1/requirements/req-x/versions/2/confirm"},
		{"dismiss:requirement:req-x:2", "POST", "/v1/requirements/req-x/versions/2/dismiss", "no CLI command; use the dashboard: POST /v1/requirements/req-x/versions/2/dismiss"},
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

const taskStatusAgentQuestionFixture = `{
  "label": "Cited revisions need a decision",
  "actor": "operator",
  "kind": "agent_question",
  "judgment": true,
  "summary": "The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.",
  "question": "Will you confirm or dismiss the cited revisions, then recover the order with direction?",
  "choices": [
    {"id": "confirm:system_design:component-http-api:44", "label": "Confirm component-http-api v44", "effect": "Confirms this pending revision. The order stays released until you recover it.", "method": "POST", "path": "/v1/system-designs/component-http-api/versions/44/confirm", "capability": "confirm_documents", "allowed": true},
    {"id": "dismiss:system_design:component-http-api:44", "label": "Dismiss component-http-api v44", "effect": "Dismisses this pending revision. The order stays released until you recover it.", "method": "POST", "path": "/v1/system-designs/component-http-api/versions/44/dismiss", "capability": "confirm_documents", "allowed": true},
    {"id": "answer", "label": "Answer", "effect": "Recovers the order with direction.", "method": "POST", "path": "/v1/work-orders/261011-aaaaaa-implement-1/recover", "capability": "recover_work", "allowed": false}
  ],
  "if_ignored": "The order stays released.",
  "deadline": null,
  "details": {
    "release_outcome": "checkpoint",
    "checkpoint": "Confirm component-http-api v44, then recover this order.",
    "checkpoint_class": "authority_conflict",
    "citations": [{"document_id": "component-http-api", "cited_version": 43, "statement_or_section_id": "Route index"}, {"document_id": "req-checkpoints", "cited_version": 2}],
    "pending_proposals": [{"tier": "system_design", "id": "component-http-api", "version": 44}]
  }
}`

// The agent question prints the server's wording, choices, and checkpoint
// context; the class prints with the machine identifiers (AC-1.6, AC-1.8, AC-5.1).
func TestTaskStatusPrintsAgentQuestionCheckpoint(t *testing.T) {
	var out bytes.Buffer
	if err := runTaskStatus(serveTaskStatus(t, taskStatusAgentQuestionFixture), &out, taskStatusTestTask, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	ordered := []string{
		"Cited revisions need a decision\nThe agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.\n",
		"question:      Will you confirm or dismiss the cited revisions, then recover the order with direction?",
		"request:       Confirm component-http-api v44, then recover this order.",
		"cites:         component-http-api v43 (Route index)",
		"cites:         req-checkpoints v2\n",
		"proposal:      component-http-api v44 (system_design, pending)",
		"  - Confirm component-http-api v44: Confirms this pending revision. The order stays released until you recover it.",
		"      no CLI command; use the dashboard: POST /v1/system-designs/component-http-api/versions/44/confirm",
		"  - Dismiss component-http-api v44: Dismisses this pending revision. The order stays released until you recover it.",
		"      no CLI command; use the dashboard: POST /v1/system-designs/component-http-api/versions/44/dismiss",
		"  - Answer: Recovers the order with direction.",
		"      cli: conveyor order recover 261011-aaaaaa-implement-1 --direction \"<answer>\"",
		"      not permitted for this credential",
		"kind:          agent_question",
		"release:       checkpoint",
		"checkpoint:    authority_conflict",
	}
	at := 0
	for _, want := range ordered {
		index := strings.Index(text[at:], want)
		if index < 0 {
			t.Fatalf("missing or out of order %q after offset %d in:\n%s", want, at, text)
		}
		at += index + len(want)
	}
	var raw bytes.Buffer
	if err := runTaskStatus(serveTaskStatus(t, taskStatusAgentQuestionFixture), &raw, taskStatusTestTask, true); err != nil {
		t.Fatal(err)
	}
	if raw.String() != taskStatusAgentQuestionFixture+"\n" {
		t.Fatalf("--json did not print the server body verbatim:\n%s", raw.String())
	}
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

// TestTaskStatusPrintsRealServerClaimWait runs task status against the real
// handler for a row-8 claim wait: --json prints the statement GET
// /v1/tasks/{id}/next-action returns, and the text names the withholding
// version and its version routes, never a /v1/documents/ route
// (req-operator-next-action AC-1.4, AC-5.1; feature-operator-next-action NA-8).
func TestTaskStatusPrintsRealServerClaimWait(t *testing.T) {
	isolateLocalAuthTest(t)
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	const taskID = "261015-claimw"
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: taskID + "-review-1", TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: taskID + "-review-1", TaskID: taskID, JobID: taskID + "-review-1", Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	governs := "```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
	design, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-claim-wait", Title: "Claim wait", Category: "Component design"}, core.SystemDesignVersion{Content: "# Claim wait\n\n" + governs, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, initial.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Claim wait\n\nRevised.\n\n" + governs, Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace, server.BearerToken = "demo", "operator-secret-token"
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	t.Setenv("CONVEYOR_ADDR", httpServer.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "operator-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/v1/tasks/"+taskID+"/next-action?workspace_id=demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-secret-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var rest bytes.Buffer
	if _, err = rest.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err = runTaskStatus(newClient(), &raw, taskID, true); err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err = json.Unmarshal(raw.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(rest.Bytes(), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task status --json differs from REST:\ncli=%s\nrest=%s", raw.String(), rest.String())
	}

	var out bytes.Buffer
	if err = runTaskStatus(newClient(), &out, taskID, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	ordered := []string{
		"Document proposal\nA document proposal is withholding the next claim.\n",
		"question:      Confirm or dismiss the proposal?",
		"proposal:      component-claim-wait v2 (system_design, pending)",
		"  - Confirm component-claim-wait v2: Confirms this pending revision.",
		"      no CLI command; use the dashboard: POST /v1/system-designs/component-claim-wait/versions/2/confirm",
		"  - Dismiss component-claim-wait v2: Dismisses this pending revision.",
		"      no CLI command; use the dashboard: POST /v1/system-designs/component-claim-wait/versions/2/dismiss",
		"if ignored:    The claim stays withheld.",
		"kind:          document_proposal",
	}
	at := 0
	for _, line := range ordered {
		index := strings.Index(text[at:], line)
		if index < 0 {
			t.Fatalf("missing or out of order %q after offset %d in:\n%s", line, at, text)
		}
		at += index + len(line)
	}
	if strings.Contains(text, "/v1/documents/") {
		t.Fatalf("task status names /v1/documents/:\n%s", text)
	}

	// A context-only task prints the context row and its task-context routes
	// (req-intake-and-triage AC-7.3).
	const contextTask = "261015-ctxonl"
	if err = st.CreateTask(ctx, core.Task{ID: contextTask, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if _, suppressed, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: contextTask, TargetKind: core.TaskContextProposalSystemDesign, TargetID: design.ID, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this design."}); err != nil || suppressed {
		t.Fatalf("suppressed=%t err=%v", suppressed, err)
	}
	var contextOut bytes.Buffer
	if err = runTaskStatus(newClient(), &contextOut, contextTask, false); err != nil {
		t.Fatal(err)
	}
	contextText := contextOut.String()
	at = 0
	for _, line := range []string{
		"Context proposal\nA context proposal is waiting for a decision.\n",
		"question:      Confirm or dismiss the proposed context?",
		"  - Confirm context system_design component-claim-wait: Attaches the proposed context to this task.",
		"      no CLI command; use the dashboard: POST /v1/tasks/" + contextTask + "/context/proposals/system_design/component-claim-wait/confirm",
		"  - Dismiss context system_design component-claim-wait: Dismisses the proposed context.",
		"      no CLI command; use the dashboard: POST /v1/tasks/" + contextTask + "/context/proposals/system_design/component-claim-wait/dismiss",
		"if ignored:    The proposal stays pending.",
		"kind:          context_proposal",
	} {
		index := strings.Index(contextText[at:], line)
		if index < 0 {
			t.Fatalf("missing or out of order %q after offset %d in:\n%s", line, at, contextText)
		}
		at += index + len(line)
	}
}
