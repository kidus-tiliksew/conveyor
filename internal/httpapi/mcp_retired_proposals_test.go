package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// retiredProposalToolNames is written out rather than read from the production
// table, so emptying that table fails these tests (DEC-102;
// req-task-authority-boundary REQ-1).
var retiredProposalToolNames = []string{"propose_system_design_revision", "propose_requirement_revision", "propose_decision"}

// retiredProposalArgs is a complete old-client call: it would have authored a
// version on the pre-retirement surface, including the retired checkpoint flag.
func retiredProposalArgs(tool, workspace, orderID, session string) map[string]any {
	args := map[string]any{"work_order_id": orderID, "session_id": session, "authority_conflict_checkpoint": true}
	if workspace != "" {
		args["workspace_id"] = workspace
	}
	switch tool {
	case "propose_system_design_revision":
		args["document_id"] = governanceDesignID
		args["content"] = "# Governance revision\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"
	case "propose_requirement_revision":
		args["document_id"] = governanceReqID
		args["content"] = "# Governance\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep executor claims exact.\n```"
	case "propose_decision":
		args["statement"] = "Keep governance claims exact."
		args["context"] = "A claim tried to propose task-local governance."
		args["alternatives_rejected"] = "Grant freestanding proposal authority."
	}
	return args
}

func requireRetiredRefusal(t *testing.T, tool, text string, isErr bool) {
	t.Helper()
	if !isErr {
		t.Fatalf("%s was accepted: %s", tool, text)
	}
	for _, required := range []string{tool, "request_plan_revision", "authority-conflict checkpoint", "release_work_order", core.WorkOrderReleaseReasonOperatorCheckpointReached, core.WorkOrderCheckpointClassAuthorityConflict} {
		if !strings.Contains(text, required) {
			t.Fatalf("%s refusal lacks %q: %s", tool, required, text)
		}
	}
}

// AC-1.1: no session's tool list offers a tool that creates a document version.
func TestMCPToolListOmitsRetiredProposalTools(t *testing.T) {
	t.Parallel()
	f := newGovernanceAuthFixture(t)
	userToken := f.user("usr_tool_list", core.WorkspaceRoleExecutor)
	for label, token := range map[string]string{"user": userToken} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s tools/list status=%d body=%s", label, response.Code, response.Body.String())
		}
		body := response.Body.String()
		var envelope struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Result.Tools) == 0 {
			t.Fatalf("%s tools/list is empty: %s", label, body)
		}
		for _, tool := range envelope.Result.Tools {
			if slices.Contains(retiredProposalToolNames, tool.Name) || strings.HasPrefix(tool.Name, "propose_") {
				t.Fatalf("%s tool list still offers %s", label, tool.Name)
			}
		}
		if strings.Contains(body, "authority_conflict_checkpoint") {
			t.Fatalf("%s tool list still declares the retired checkpoint flag", label)
		}
	}
	for _, name := range retiredProposalToolNames {
		if _, mapped := mcpCapabilities[name]; mapped {
			t.Fatalf("%s keeps a capability entry", name)
		}
	}
}

// AC-1.2: a retired name is refused with its remedies for every credential and
// claim shape, nothing is written, and the workspace, capability, and claim
// checks are never consulted, because no path remains to reach a write.
func TestMCPRetiredProposalToolsRefuseWithoutWriting(t *testing.T) {
	t.Parallel()
	const owner, other = "usr_retired_owner", "usr_retired_other"
	type scenario struct {
		name  string
		setup func(f *governanceAuthFixture) (orderID string, args func(tool string) map[string]any)
	}
	scenarios := []scenario{
		{name: "own live implement claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			orderID := f.order("own", core.StageImplement)
			f.claimRun(orderID, owner, "own-session")
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "own-session")
			}
		}},
		{name: "own live review claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			orderID := f.order("review", core.StageReview)
			f.claimRun(orderID, owner, "own-session")
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "own-session")
			}
		}},
		{name: "own live plan claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			orderID := f.order("spec", core.StageSpec)
			f.claimRun(orderID, owner, "own-session")
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "own-session")
			}
		}},
		{name: "claim-bound worker claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			orderID := f.order("worker", core.StageImplement)
			if _, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, orderID, core.WorkOrderClaim{SessionID: "worker-session", ClientToken: "worker-secret", ClaimantID: "worker-retired", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
				f.t.Fatal(err)
			}
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "worker-session")
			}
		}},
		{name: "foreign claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			f.user(other, core.WorkspaceRoleExecutor)
			orderID := f.order("foreign", core.StageImplement)
			f.claimRun(orderID, other, "foreign-session")
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "foreign-session")
			}
		}},
		{name: "expired claim", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			orderID := f.order("expired", core.StageImplement)
			f.claimRun(orderID, owner, "own-session")
			f.ageClaim(orderID, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
			return orderID, func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, orderID, "own-session")
			}
		}},
		{name: "no claim and no order", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			return "", func(tool string) map[string]any {
				return retiredProposalArgs(tool, governanceWorkspace, "gov-order-missing", "own-session")
			}
		}},
		{name: "workspace omitted", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			return "", func(tool string) map[string]any {
				return retiredProposalArgs(tool, "", "gov-order-missing", "own-session")
			}
		}},
		{name: "workspace not visible", setup: func(f *governanceAuthFixture) (string, func(string) map[string]any) {
			return "", func(tool string) map[string]any {
				return retiredProposalArgs(tool, "beta", "gov-order-missing", "own-session")
			}
		}},
	}
	roles := []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator}
	for _, tool := range retiredProposalToolNames {
		for _, role := range roles {
			for _, test := range scenarios {
				t.Run(tool+"/"+string(role)+"/"+test.name, func(t *testing.T) {
					t.Parallel()
					f := newGovernanceAuthFixture(t)
					userToken := f.user(owner, role)
					orderID, args := test.setup(f)
					before := f.snapshot(orderID)
					for label, token := range map[string]string{"user": userToken} {
						text, isErr := f.call(token, tool, args(tool))
						requireRetiredRefusal(t, tool, text, isErr)
						f.requireUnchanged(tool+" "+label, before)
						if got := f.members.lastCapability(); got != "" {
							t.Fatalf("%s %s consulted capability %q before refusing", tool, label, got)
						}
					}
				})
			}
		}
	}
}

// The refusal travels the tools/call error path the session can read in band
// (component-mcp-protocol), and an unrelated unknown name keeps the generic
// refusal.
func TestMCPRetiredProposalToolRefusalIsInBand(t *testing.T) {
	t.Parallel()
	f := newGovernanceAuthFixture(t)
	userToken := f.user("usr_in_band", core.WorkspaceRoleExecutor)
	raw := func(tool string) (isError bool, text string, protocolError *rpcError) {
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{"workspace_id": governanceWorkspace}}})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
		request.Header.Set("Authorization", "Bearer "+userToken)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *rpcError `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || len(envelope.Result.Content) != 1 {
			t.Fatalf("%s status=%d body=%s", tool, response.Code, response.Body.String())
		}
		return envelope.Result.IsError, envelope.Result.Content[0].Text, envelope.Error
	}
	for _, tool := range retiredProposalToolNames {
		isError, text, protocolError := raw(tool)
		if protocolError != nil {
			t.Fatalf("%s refused at the protocol layer: %+v", tool, protocolError)
		}
		requireRetiredRefusal(t, tool, text, isError)
	}
	isError, text, _ := raw("propose_something_else")
	if !isError || !strings.Contains(text, "unknown tool") || strings.Contains(text, "request_plan_revision") {
		t.Fatalf("unrelated unknown tool isError=%v text=%q", isError, text)
	}
}

// req-260820-394cac AC-3.1: a live claim releases at an authority-conflict
// checkpoint that has no pending proposal on any cited document, its decision
// request naming no proposal identifier. AC-2.1's citation validation stands.
func TestMCPAuthorityConflictReleaseNeedsNoProposal(t *testing.T) {
	t.Parallel()
	const reason = core.WorkOrderReleaseReasonOperatorCheckpointReached
	const request = "The work would require changing the cited clause. Decide whether to revise it."
	checkpoint := func(documentID string, version int, clause string) map[string]any {
		return map[string]any{
			"decision_request": request,
			"class":            core.WorkOrderCheckpointClassAuthorityConflict,
			"citations":        []any{map[string]any{"document_id": documentID, "cited_version": version, "statement_or_section_id": clause}},
		}
	}
	setup := func(t *testing.T) (*governanceAuthFixture, string, string) {
		t.Helper()
		f := newGovernanceAuthFixture(t)
		if _, _, err := f.store.ConfirmRequirementVersion(f.ctx, governanceReqID, 1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.store.ConfirmSystemDesignVersion(f.ctx, governanceDesignID, 1); err != nil {
			t.Fatal(err)
		}
		userToken := f.user("usr_checkpoint", core.WorkspaceRoleExecutor)
		orderID := f.order("checkpoint", core.StageImplement)
		f.claimRun(orderID, "usr_checkpoint", "checkpoint-session")
		if pending, err := f.store.ListPendingProposals(f.ctx); err != nil || len(pending) != 0 {
			t.Fatalf("fixture pending proposals=%+v err=%v", pending, err)
		}
		return f, userToken, orderID
	}
	for _, credential := range []string{"user"} {
		for _, documentID := range []string{governanceReqID, governanceDesignID} {
			t.Run(credential+"/"+documentID, func(t *testing.T) {
				t.Parallel()
				f, token, orderID := setup(t)
				before := f.snapshot(orderID)
				text, isErr := f.call(token, "release_work_order", map[string]any{
					"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": "checkpoint-session",
					"reason": reason, "checkpoint": checkpoint(documentID, 1, "REQ-1"),
				})
				if isErr {
					t.Fatalf("no-proposal checkpoint release refused: %s", text)
				}
				released, err := f.store.GetWorkOrder(f.ctx, orderID)
				if err != nil {
					t.Fatal(err)
				}
				if released.State == core.WorkOrderClaimed || released.SessionID == "checkpoint-session" || released.Checkpoint == nil {
					t.Fatalf("order was not released at the checkpoint: %+v", released)
				}
				if released.Checkpoint.DecisionRequest != request || released.Checkpoint.Class != core.WorkOrderCheckpointClassAuthorityConflict || len(released.Checkpoint.Citations) != 1 || released.Checkpoint.Citations[0].DocumentID != documentID || released.Checkpoint.Citations[0].CitedVersion != 1 {
					t.Fatalf("recorded checkpoint=%+v", released.Checkpoint)
				}
				after := f.snapshot(orderID)
				if after.designVersions != before.designVersions || after.reqVersions != before.reqVersions || after.decisions != before.decisions || after.pending != 0 {
					t.Fatalf("release wrote a proposal: before=%+v after=%+v", before, after)
				}
				// The operator is asked for authorship, because nothing is
				// pending on the cited document (AC-3.2).
				status, isErr := f.call(token, "get_task_status", map[string]any{"workspace_id": governanceWorkspace, "task_id": released.TaskID})
				if isErr {
					t.Fatalf("task status: %s", status)
				}
				var action struct {
					Label   string `json:"label"`
					Details struct {
						Checkpoint       string `json:"checkpoint"`
						CheckpointClass  string `json:"checkpoint_class"`
						PendingProposals []any  `json:"pending_proposals"`
					} `json:"details"`
				}
				if err := json.Unmarshal([]byte(status), &action); err != nil {
					t.Fatal(err)
				}
				if action.Label != "Cited documents need a revision" || action.Details.Checkpoint != request || action.Details.CheckpointClass != string(core.WorkOrderCheckpointClassAuthorityConflict) || len(action.Details.PendingProposals) != 0 {
					t.Fatalf("next action=%s", status)
				}
			})
		}
	}
	for _, refusal := range []struct {
		name       string
		checkpoint map[string]any
		want       string
	}{
		{"missing document", checkpoint("design-missing", 1, "Clause"), "does not exist in this workspace"},
		{"nonpositive cited version", checkpoint(governanceDesignID, 0, "Clause"), "cited_version must be positive"},
		{"no decision request", map[string]any{"class": core.WorkOrderCheckpointClassAuthorityConflict, "citations": []any{map[string]any{"document_id": governanceDesignID, "cited_version": 1}}}, "decision"},
	} {
		t.Run("refuses "+refusal.name, func(t *testing.T) {
			t.Parallel()
			f, agentToken, orderID := setup(t)
			before := f.snapshot(orderID)
			text, isErr := f.call(agentToken, "release_work_order", map[string]any{
				"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": "checkpoint-session",
				"reason": reason, "checkpoint": refusal.checkpoint,
			})
			if !isErr || !strings.Contains(text, refusal.want) {
				t.Fatalf("isError=%v text=%q, want %q", isErr, text, refusal.want)
			}
			f.requireUnchanged(refusal.name, before)
		})
	}
}

// AC-1.3 and AC-1.4: retiring the tools leaves the document surfaces alone. A
// contributor still proposes over REST, a task-origin version recorded before
// this change keeps its origin in history, and an operator still decides it.
func TestDocumentSurfacesSurviveProposalToolRetirement(t *testing.T) {
	t.Parallel()
	f := newGovernanceAuthFixture(t)
	if _, _, err := f.store.ConfirmRequirementVersion(f.ctx, governanceReqID, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.ConfirmSystemDesignVersion(f.ctx, governanceDesignID, 1); err != nil {
		t.Fatal(err)
	}
	contributor := f.user("usr_contributor", core.WorkspaceRoleContributor)
	rest := func(token, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Workspace-ID", governanceWorkspace)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		return response
	}
	for _, post := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/requirements/" + governanceReqID + "/versions", map[string]any{"content": "# Governance\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep operator claims exact.\n```"}},
		{"/v1/system-designs/" + governanceDesignID + "/versions", map[string]any{"content": "# Governance revised\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"}},
		{"/v1/decisions", map[string]any{"statement": "Keep planning proposals.", "context": "Planning owns document change.", "alternatives_rejected": "Retiring every proposal path."}},
	} {
		if response := rest(contributor, http.MethodPost, post.path, post.body); response.Code != http.StatusCreated {
			t.Fatalf("POST %s status=%d body=%s", post.path, response.Code, response.Body.String())
		}
	}
	pending, err := f.store.ListPendingProposals(f.ctx)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending after REST proposals=%+v err=%v", pending, err)
	}

	// Versions a task authored before this change stay decidable by an operator,
	// and their origin stays in history after the decision, on all three tiers.
	const originTask = "task-before-retirement"
	if err := f.store.CreateTask(f.ctx, core.Task{ID: originTask, Workspace: governanceWorkspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	design, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{
		DocumentID: governanceDesignID, Content: "# Governance from a task\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```",
		Origin: core.SystemDesignOriginImplementation, OriginTaskID: originTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, err := f.store.ProposeRequirementVersion(f.ctx, core.RequirementVersion{
		RequirementID: governanceReqID, Content: "# Governance\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep task claims exact.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep task claims exact."}},
		Origin:     core.RequirementOriginImplementation, OriginTaskID: originTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.store.ProposeDecision(f.ctx, core.Decision{
		Statement: "A task once proposed this.", Context: "Recorded before retirement.", AlternativesRejected: "None.",
		Origin: core.DecisionOriginImplementation, OriginTaskID: originTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.members.setRole("usr_operator", governanceWorkspace, core.WorkspaceRoleOperator)
	f.credentials["pat-operator"] = core.AuthenticatedCredential{ID: "pat_operator", OwnerUserID: "usr_operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	for _, path := range []string{
		fmt.Sprintf("/v1/system-designs/%s/versions/%d/confirm", governanceDesignID, design.Version),
		fmt.Sprintf("/v1/requirements/%s/versions/%d/dismiss", governanceReqID, requirement.Version),
		"/v1/decisions/" + decision.ID + "/confirm",
	} {
		if response := rest("pat-operator", http.MethodPost, path, map[string]any{}); response.Code != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	designs, err := f.store.ListSystemDesignVersions(f.ctx, governanceDesignID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(designs, func(item core.SystemDesignVersion) bool {
		return item.Version == design.Version && item.Origin == core.SystemDesignOriginImplementation && item.OriginTaskID == originTask && item.Confirmed
	}) {
		t.Fatalf("design history lost the task origin: %+v", designs)
	}
	requirements, err := f.store.ListRequirementVersions(f.ctx, governanceReqID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(requirements, func(item core.RequirementVersion) bool {
		return item.Version == requirement.Version && item.Origin == core.RequirementOriginImplementation && item.OriginTaskID == originTask
	}) {
		t.Fatalf("requirement history lost the task origin: %+v", requirements)
	}
	confirmed, err := f.store.GetDecision(f.ctx, decision.ID)
	if err != nil || confirmed.Status != core.DecisionConfirmed || confirmed.Origin != core.DecisionOriginImplementation || confirmed.OriginTaskID != originTask {
		t.Fatalf("decision history=%+v err=%v", confirmed, err)
	}
	reader := f.user("usr_reader", core.WorkspaceRoleViewer)
	text, isErr := f.call(reader, "get_document", map[string]any{"workspace_id": governanceWorkspace, "kind": "system_design", "document_id": governanceDesignID, "version": design.Version})
	if isErr || !strings.Contains(text, originTask) {
		t.Fatalf("get_document isError=%v, want origin task %s in %s", isErr, originTask, text)
	}
}
