package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// rest serves one GET through the full handler with token and returns the body.
func (f *governanceAuthFixture) rest(token, path string) []byte {
	f.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		f.t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

func decodeJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

// gateTask stores an awaiting plan-approval task with no work order.
func (f *governanceAuthFixture) gateTask(id string, createdAt time.Time) {
	f.t.Helper()
	if err := f.store.CreateTask(f.ctx, core.Task{ID: id, Workspace: governanceWorkspace, Repo: "conveyor", Branch: id, State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: createdAt}); err != nil {
		f.t.Fatal(err)
	}
}

// dependentOrder stores a queued implement order whose task depends on
// dependency, so the store derives the blocker the projection reads.
func (f *governanceAuthFixture) dependentOrder(name, dependency string) {
	f.t.Helper()
	f.order(name, core.StageImplement)
	if _, err := f.store.AddTaskDependency(f.ctx, store.DependencyAdditionRequest{TaskID: "gov-task-" + name, DependsOnTaskID: dependency, Reason: "dependency first", RequestID: "dep-" + name}); err != nil {
		f.t.Fatal(err)
	}
}

// TestMCPGetTaskStatusAdmission pins who may read a task's next action over
// MCP (req-operator-next-action AC-5.3). A user reads any workspace task. An
// agent reads only the task of an order its session claimed or submitted, and
// every agent refusal is one error, so it never discloses whether the task
// exists (DEC-19).
func TestMCPGetTaskStatusAdmission(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, agentToken := f.user("usr_status", core.WorkspaceRoleContributor)
	claimed := f.order("status-claimed", core.StageImplement)
	f.claimRun(claimed, "usr_status", "claimed-session")
	submitted := f.order("status-submitted", core.StageImplement)
	f.claimRun(submitted, "usr_status", "submitted-session")
	f.ageClaim(submitted, func(order *core.WorkOrder) { order.State = core.WorkOrderSubmitted })
	other := f.order("status-other", core.StageImplement)
	child := "child-status"
	f.credentials[child] = core.AuthenticatedCredential{ID: "agt_child_status", OwnerUserID: "usr_status", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: governanceWorkspace, RunWorkOrderID: claimed, RunSessionID: "claimed-session"}

	read := func(token string, args map[string]any) (string, bool) {
		args["workspace_id"] = governanceWorkspace
		return f.call(token, "get_task_status", args)
	}
	admit := func(label, token string, args map[string]any) {
		t.Helper()
		text, isErr := read(token, args)
		if isErr {
			t.Fatalf("%s refused: %s", label, text)
		}
		var action nextaction.NextAction
		if err := json.Unmarshal([]byte(text), &action); err != nil || action.Kind == "" || action.Label == "" {
			t.Fatalf("%s result=%s err=%v", label, text, err)
		}
	}
	admit("user without an order", userToken, map[string]any{"task_id": "gov-task-status-other"})
	admit("agent with its claimed order", agentToken, map[string]any{"task_id": "gov-task-status-claimed", "work_order_id": claimed, "session_id": "claimed-session"})
	admit("agent with its submitted order", agentToken, map[string]any{"task_id": "gov-task-status-submitted", "work_order_id": submitted, "session_id": "submitted-session"})
	admit("run child with its own order", child, map[string]any{"task_id": "gov-task-status-claimed", "work_order_id": claimed, "session_id": "claimed-session"})

	missing, isErr := read(agentToken, map[string]any{"task_id": "gov-task-absent", "work_order_id": claimed, "session_id": "claimed-session"})
	if !isErr || missing == "" {
		t.Fatalf("agent read of a nonexistent task admitted: %s", missing)
	}
	for label, refusal := range map[string]struct {
		token string
		args  map[string]any
	}{
		"agent naming another task":          {agentToken, map[string]any{"task_id": "gov-task-status-other", "work_order_id": claimed, "session_id": "claimed-session"}},
		"agent with a wrong session":         {agentToken, map[string]any{"task_id": "gov-task-status-claimed", "work_order_id": claimed, "session_id": "other-session"}},
		"agent submitted with wrong session": {agentToken, map[string]any{"task_id": "gov-task-status-submitted", "work_order_id": submitted, "session_id": "claimed-session"}},
		"agent without work_order_id":        {agentToken, map[string]any{"task_id": "gov-task-status-claimed", "session_id": "claimed-session"}},
		"agent with an unclaimed order":      {agentToken, map[string]any{"task_id": "gov-task-status-other", "work_order_id": other, "session_id": "claimed-session"}},
		"run child naming another order":     {child, map[string]any{"task_id": "gov-task-status-submitted", "work_order_id": submitted, "session_id": "submitted-session"}},
	} {
		text, refused := read(refusal.token, refusal.args)
		if !refused || text != missing {
			t.Fatalf("%s isError=%v text=%q, want the nonexistent-task refusal %q", label, refused, text, missing)
		}
	}

	// A real worker bearer token reaches the tool through the full handler,
	// holding the order it names, so only the worker refusal can stop it.
	workerToken := "worker-status-token"
	sum := sha256.Sum256([]byte(workerToken))
	if err := createOwnedWorker(f.store, f.ctx, core.Worker{ID: "worker-status", Workspace: governanceWorkspace, OwnerUserID: "usr_status", Name: "worker-status", CredentialHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	workerOrder := f.order("status-worker", core.StageImplement)
	if _, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, workerOrder, core.WorkOrderClaim{SessionID: "worker-session", ClientToken: "secret-worker-session", ClaimantID: "worker-status", WorkerID: "worker-status", Agent: "codex", Model: "model", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if text, isErr := read(workerToken, map[string]any{"task_id": "gov-task-status-worker", "work_order_id": workerOrder, "session_id": "worker-session"}); !isErr || text != "get_task_status requires a user or agent credential" {
		t.Fatalf("worker read isError=%v text=%q, want the worker refusal", isErr, text)
	}
}

// TestMCPGetTaskStatusParity pins one derivation (DEC-61) and a side-effect
// free read (AC-1.7): the MCP result equals GET /v1/tasks/{id}/next-action
// for the same credential, and an agent read neither renews the lease nor
// records an event.
func TestMCPGetTaskStatusParity(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, agentToken := f.user("usr_parity", core.WorkspaceRoleContributor)
	f.gateTask("gov-task-parity-gate", time.Now().UTC())
	text, isErr := f.call(userToken, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": "gov-task-parity-gate"})
	if isErr {
		t.Fatalf("user read refused: %s", text)
	}
	rest := f.rest(userToken, "/v1/tasks/gov-task-parity-gate/next-action?workspace_id="+governanceWorkspace)
	if got, want := decodeJSON(t, []byte(text)), decodeJSON(t, rest); !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP and REST next actions differ:\nmcp=%s\nrest=%s", text, rest)
	}

	claimed := f.order("parity-claimed", core.StageImplement)
	f.claimRun(claimed, "usr_parity", "parity-session")
	before := f.snapshot(claimed)
	if text, isErr = f.call(agentToken, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": "gov-task-parity-claimed", "work_order_id": claimed, "session_id": "parity-session"}); isErr {
		t.Fatalf("agent read refused: %s", text)
	}
	after := f.snapshot(claimed)
	if !after.order.LeaseExpiresAt.Equal(before.order.LeaseExpiresAt) || after.events != before.events {
		t.Fatalf("status read mutated the claim: lease %v -> %v, events %d -> %d", before.order.LeaseExpiresAt, after.order.LeaseExpiresAt, before.events, after.events)
	}
}

// TestMCPGetTaskStatusAgentQuestionParity pins one statement for an
// authority_conflict checkpoint: GET /v1/tasks/{id}/next-action and MCP
// get_task_status return the same label, choices, and checkpoint details,
// including the cited pending proposal (req-operator-next-action AC-1.6,
// AC-1.7, AC-5.3; component-http-api, Next-action and task-flow reads).
func TestMCPGetTaskStatusAgentQuestionParity(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, _ := f.user("usr_question", core.WorkspaceRoleContributor)
	design, initial, err := f.store.CreateSystemDesign(f.ctx, core.SystemDesign{ID: "component-question", Title: "Question", Category: "Component design"}, core.SystemDesignVersion{Content: "# Question\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, design.ID, initial.Version); err != nil {
		t.Fatal(err)
	}
	proposed, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Question\n\nRevised.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	order := f.order("question", core.StageImplement)
	f.claimWorker(order, "worker-question", "usr_question", "question-session")
	if _, err = storetest.ReleaseWorkerClaim(f.ctx, f.store, order, "worker-question", core.WorkOrderRelease{
		SessionID: "question-session", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Confirm component-question.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: design.ID, CitedVersion: initial.Version, StatementOrSectionID: "Section"}}},
	}); err != nil {
		t.Fatal(err)
	}
	text, isErr := f.call(userToken, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": "gov-task-question"})
	if isErr {
		t.Fatalf("user read refused: %s", text)
	}
	rest := f.rest(userToken, "/v1/tasks/gov-task-question/next-action?workspace_id="+governanceWorkspace)
	if got, want := decodeJSON(t, []byte(text)), decodeJSON(t, rest); !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP and REST next actions differ:\nmcp=%s\nrest=%s", text, rest)
	}
	var action nextaction.NextAction
	if err = json.Unmarshal(rest, &action); err != nil {
		t.Fatal(err)
	}
	wantProposals := []nextaction.ProposalRef{{Tier: "system_design", ID: design.ID, Version: proposed.Version}}
	if action.Kind != nextaction.KindAgentQuestion || action.Label != "Cited revisions need a decision" || !reflect.DeepEqual(action.Details.PendingProposals, wantProposals) ||
		action.Details.Checkpoint != "Confirm component-question." || action.Details.CheckpointClass != core.WorkOrderCheckpointClassAuthorityConflict || len(action.Details.Citations) != 1 {
		t.Fatalf("next action = %s %q details %+v", action.Kind, action.Label, action.Details)
	}
	if len(action.Choices) != 3 || action.Choices[0].Path != "/v1/system-designs/component-question/versions/2/confirm" || action.Choices[2].ID != "answer" {
		t.Fatalf("choices = %+v", action.Choices)
	}
}

// TestMCPGetTaskStatusClaimWaitParity pins one statement for a row-8 claim
// wait and for the 261015-676ea5 shape: GET /v1/tasks/{id}/next-action and MCP
// get_task_status agree, row 8 lists the withholding version on its version
// route, a triage task-context proposal never selects row 8, and no choice
// names /v1/documents/ (req-operator-next-action AC-1.4, AC-1.7, AC-5.3;
// req-260810-70ce2f AC-1.1, AC-1.3).
func TestMCPGetTaskStatusClaimWaitParity(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, _ := f.user("usr_claim_wait", core.WorkspaceRoleOperator)
	governs := "```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
	design := func(id string) core.SystemDesign {
		t.Helper()
		created, initial, err := f.store.CreateSystemDesign(f.ctx, core.SystemDesign{ID: id, Title: id, Category: "Component design"}, core.SystemDesignVersion{Content: "# " + id + "\n\n" + governs, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, created.ID, initial.Version); err != nil {
			t.Fatal(err)
		}
		return created
	}
	propose := func(documentID, taskID string) int {
		t.Helper()
		proposed, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: documentID, Content: "# " + documentID + "\n\nRevised for " + taskID + ".\n\n" + governs, Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID})
		if err != nil {
			t.Fatal(err)
		}
		return proposed.Version
	}
	suggest := func(taskID, documentID string) {
		t.Helper()
		if _, suppressed, err := f.store.ProposeTaskContext(f.ctx, core.TaskContextProposalInput{TaskID: taskID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: documentID, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this design."}); err != nil || suppressed {
			t.Fatalf("suppressed=%t err=%v", suppressed, err)
		}
	}
	read := func(taskID string) nextaction.NextAction {
		t.Helper()
		text, isErr := f.call(userToken, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": taskID})
		if isErr {
			t.Fatalf("user read refused: %s", text)
		}
		rest := f.rest(userToken, "/v1/tasks/"+taskID+"/next-action?workspace_id="+governanceWorkspace)
		if got, want := decodeJSON(t, []byte(text)), decodeJSON(t, rest); !reflect.DeepEqual(got, want) {
			t.Fatalf("MCP and REST next actions differ:\nmcp=%s\nrest=%s", text, rest)
		}
		if strings.Contains(string(rest), "/v1/documents/") {
			t.Fatalf("next action names /v1/documents/: %s", rest)
		}
		var action nextaction.NextAction
		if err := json.Unmarshal(rest, &action); err != nil {
			t.Fatal(err)
		}
		return action
	}

	suggested := design("component-claim-wait-context")
	withheld := design("component-claim-wait")
	f.order("claim-wait", core.StageReview)
	suggest("gov-task-claim-wait", suggested.ID)
	version := propose(withheld.ID, "gov-task-claim-wait")
	action := read("gov-task-claim-wait")
	route := fmt.Sprintf("/v1/system-designs/%s/versions/%d", withheld.ID, version)
	wantProposals := []nextaction.ProposalRef{{Tier: "system_design", ID: withheld.ID, Version: version}}
	if action.Kind != nextaction.KindDocumentProposal || !reflect.DeepEqual(action.Details.PendingProposals, wantProposals) {
		t.Fatalf("row 8 = %s details %+v", action.Kind, action.Details)
	}
	if len(action.Choices) != 2 || action.Choices[0].Path != route+"/confirm" || action.Choices[1].Path != route+"/dismiss" || !action.Choices[0].Allowed || !action.Choices[1].Allowed {
		t.Fatalf("row 8 choices = %+v", action.Choices)
	}

	// A context-only task keeps operator attention through the context row,
	// with task-context routes under operate_gates and no claim wait
	// (req-intake-and-triage AC-7.3).
	f.order("context-only", core.StageImplement)
	suggest("gov-task-context-only", suggested.ID)
	contextOnly := read("gov-task-context-only")
	contextRoute := "/v1/tasks/gov-task-context-only/context/proposals/system_design/" + suggested.ID
	if contextOnly.Kind != nextaction.KindContextProposal || contextOnly.Actor != nextaction.ActorOperator || len(contextOnly.Details.PendingProposals) != 0 ||
		len(contextOnly.Choices) != 2 || contextOnly.Choices[0].Path != contextRoute+"/confirm" || contextOnly.Choices[1].Path != contextRoute+"/dismiss" || !contextOnly.Choices[0].Allowed {
		t.Fatalf("context-only = %s choices %+v details %+v", contextOnly.Kind, contextOnly.Choices, contextOnly.Details)
	}
	inbox, isErr := f.call(userToken, "list_operator_inbox", map[string]any{"workspace_id": governanceWorkspace, "band": "decisions"})
	if isErr || !strings.Contains(inbox, `"gov-task-context-only"`) || !strings.Contains(inbox, `"context_proposal"`) {
		t.Fatalf("inbox decisions omit the context-only task: err=%t %s", isErr, inbox)
	}

	first, second := design("component-cited-a"), design("component-cited-b")
	order := f.order("context-question", core.StageImplement)
	suggest("gov-task-context-question", suggested.ID)
	firstVersion, secondVersion := propose(first.ID, "gov-task-context-question"), propose(second.ID, "gov-task-context-question")
	f.claimWorker(order, "worker-context-question", "usr_claim_wait", "context-question-session")
	if _, err := storetest.ReleaseWorkerClaim(f.ctx, f.store, order, "worker-context-question", core.WorkOrderRelease{
		SessionID: "context-question-session", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Confirm or dismiss both revisions.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{
			{DocumentID: first.ID, CitedVersion: 1, StatementOrSectionID: "Section A"},
			{DocumentID: second.ID, CitedVersion: 1, StatementOrSectionID: "Section B"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	question := read("gov-task-context-question")
	wantCited := []nextaction.ProposalRef{{Tier: "system_design", ID: first.ID, Version: firstVersion}, {Tier: "system_design", ID: second.ID, Version: secondVersion}}
	if question.Kind != nextaction.KindAgentQuestion || !reflect.DeepEqual(question.Details.PendingProposals, wantCited) {
		t.Fatalf("676ea5 shape = %s details %+v", question.Kind, question.Details)
	}
	for _, secondary := range question.Details.Secondary {
		if secondary.Kind == nextaction.KindDocumentProposal {
			t.Fatalf("task-context proposal recorded as document_proposal: %+v", question.Details.Secondary)
		}
	}
}

// TestMCPListOperatorInbox pins the MCP inbox to GET /v1/next-actions: the
// same items in the same server order for one filter and band, refused to
// agent and worker credentials.
func TestMCPListOperatorInbox(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, agentToken := f.user("usr_inbox", core.WorkspaceRoleContributor)
	now := time.Now().UTC().Truncate(time.Second)
	f.gateTask("z-gate", now.Add(-time.Hour))
	f.gateTask("a-gate", now)
	f.dependentOrder("inbox-dependent-b", "z-gate")
	f.dependentOrder("inbox-dependent-a", "a-gate")
	f.order("inbox-queued", core.StageImplement)
	if err := f.store.CreateTask(f.ctx, core.Task{ID: "inbox-merging", Workspace: governanceWorkspace, Repo: "conveyor", Branch: "inbox-merging", State: core.TaskApproved, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	items := func(raw []byte) []any {
		t.Helper()
		var page struct {
			Items []any `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return page.Items
	}
	for _, tc := range []struct {
		query string
		args  map[string]any
	}{
		{"", map[string]any{}},
		{"&band=decisions", map[string]any{"band": "decisions"}},
		{"&band=automatic", map[string]any{"band": "automatic"}},
		{"&band=blocked", map[string]any{"band": "blocked"}},
		{"&state=awaiting_human&query=gate", map[string]any{"state": "awaiting_human", "query": "gate"}},
	} {
		tc.args["workspace_id"] = governanceWorkspace
		text, isErr := f.call(userToken, "list_operator_inbox", tc.args)
		if isErr {
			t.Fatalf("%q refused: %s", tc.query, text)
		}
		got, want := items([]byte(text)), items(f.rest(userToken, "/v1/next-actions?workspace_id="+governanceWorkspace+tc.query))
		if len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q MCP inbox differs from REST:\nmcp=%v\nrest=%v", tc.query, got, want)
		}
		if tc.query == "&band=decisions" {
			// z-gate gates a dependent, so server order puts it ahead of
			// a-gate; an alphabetical re-sort would not.
			first, _ := got[0].(map[string]any)["task"].(map[string]any)
			if first["id"] != "z-gate" {
				t.Fatalf("decisions lead with %v, want z-gate: %v", first["id"], got)
			}
		}
	}
	if text, isErr := f.call(agentToken, "list_operator_inbox", map[string]any{"workspace_id": governanceWorkspace}); !isErr || !strings.Contains(text, "requires an operator-scoped user credential") {
		t.Fatalf("agent inbox isError=%v text=%q", isErr, text)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(context.WithValue(store.WithActor(t.Context(), store.SystemActor()), workerContextKey{}, core.Worker{ID: "worker", Workspace: governanceWorkspace}))
	if _, err := f.server.callMCPTool(request, "list_operator_inbox", map[string]any{"workspace_id": governanceWorkspace}); err == nil {
		t.Fatal("worker credential read the inbox")
	}
}
