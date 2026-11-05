package httpapi

// Full-transport MCP authorization regressions for claim-bound governance and
// the capability-derived agent ceiling (req-accounts-and-membership AC-2.2,
// AC-2.3, AC-2.4, AC-3.4, AC-5.1; req-security-boundaries REQ-1;
// component-mcp-protocol). Every call goes through credential verification,
// workspace resolution, and live membership authorization over /mcp.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	governanceWorkspace = "demo"
	governanceDesignID  = "design-governance-auth"
	governanceReqID     = "req-governance-auth"
)

var governanceMCPTools = []string{"request_plan_revision"}

// claimMutationMCPTools are the mutations a live claim admits for its holder; a
// viewer binding must refuse every one of them.
var claimMutationMCPTools = append(slices.Clone(governanceMCPTools), "renew_work_order", "report_progress", "release_work_order")

type governanceAuthFixture struct {
	t           *testing.T
	ctx         context.Context
	store       store.Store
	server      *Server
	handler     http.Handler
	members     *lockedMemberships
	credentials staticCredentialVerifier
}

func newGovernanceAuthFixture(t *testing.T) *governanceAuthFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), governanceWorkspace)
	st := store.NewMemory()
	if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: governanceDesignID, Title: "Governance", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Governance\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: governanceReqID, Title: "Governance"}, core.RequirementVersion{Content: "# Governance\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep claims exact.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep claims exact."}}, Origin: core.RequirementOriginOperator}); err != nil {
		t.Fatal(err)
	}
	members := &lockedMemberships{inner: &membershipFixture{
		workspaces: []core.Workspace{{ID: governanceWorkspace}, {ID: "beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{},
	}}
	credentials := staticCredentialVerifier{}
	server := NewServer(st)
	server.Workspace = governanceWorkspace
	server.Workspaces, server.Memberships = members, members
	server.Credentials = credentials
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: governanceWorkspace, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	return &governanceAuthFixture{t: t, ctx: ctx, store: st, server: server, handler: server.Handler(), members: members, credentials: credentials}
}

// user registers a personal access token for owner, binding owner to role in
// the fixture workspace.
func (f *governanceAuthFixture) user(owner string, role core.WorkspaceRole) (userToken string) {
	f.members.setRole(owner, governanceWorkspace, role)
	userToken = "pat-" + owner
	f.credentials[userToken] = core.AuthenticatedCredential{ID: "pat_" + owner, OwnerUserID: owner, Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	return userToken
}

// order creates a queued order at stage with an approved plan and returns
// its ID; the task ID equals the order ID's prefix.
func (f *governanceAuthFixture) order(name string, stage core.Stage) string {
	f.t.Helper()
	taskID, orderID := "gov-task-"+name, "gov-order-"+name
	if err := f.store.CreateTask(f.ctx, core.Task{ID: taskID, Workspace: governanceWorkspace, Repo: "conveyor", State: core.TaskRunning, NextStage: stage, CreatedAt: time.Now()}); err != nil {
		f.t.Fatal(err)
	}
	plan, err := f.store.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: taskID, Content: "approved plan"})
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.store.ApproveSpecVersion(f.ctx, taskID, plan.Version); err != nil {
		f.t.Fatal(err)
	}
	if err = f.store.CreateJob(f.ctx, core.Job{ID: orderID, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
		f.t.Fatal(err)
	}
	if err = storetest.For(f.store).CreateWorkOrder(f.ctx, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: stage, State: core.WorkOrderQueued}); err != nil {
		f.t.Fatal(err)
	}
	return orderID
}

// claimRun stores a live user-run claim for owner without the MCP surface.
func (f *governanceAuthFixture) claimRun(orderID, owner, session string) {
	f.t.Helper()
	if _, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, orderID, core.WorkOrderClaim{SessionID: session, ClientToken: "secret-" + session, ClaimantID: core.TaskRunClaimantID(owner), OwnerUserID: owner, Agent: "codex", Model: "model", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		f.t.Fatal(err)
	}
}

// call posts one tools/call through the full /mcp handler and returns the
// in-band text and whether it is an error.
func (f *governanceAuthFixture) call(token, tool string, args map[string]any) (string, bool) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		f.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s status=%d body=%s", tool, response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		f.t.Fatal(err)
	}
	if envelope.Error != nil {
		return envelope.Error.Message, true
	}
	if len(envelope.Result.Content) != 1 {
		f.t.Fatalf("%s content=%+v", tool, envelope.Result.Content)
	}
	return envelope.Result.Content[0].Text, envelope.Result.IsError
}

func governanceCallArgs(tool, workspace, orderID, session string) map[string]any {
	args := map[string]any{"workspace_id": workspace, "work_order_id": orderID, "session_id": session}
	switch tool {
	case "request_plan_revision":
		args["rationale"] = "the approved plan conflicts with the API"
	case "renew_work_order":
		args["lease_seconds"] = 600
	case "report_progress":
		args["message"] = "progress under a live claim"
	case "release_work_order":
		args["reason"] = "agent handoff"
	}
	return args
}

// governanceState is everything a refused mutation must leave untouched.
type governanceState struct {
	order          core.WorkOrder
	task           core.Task
	events         int
	designVersions int
	reqVersions    int
	decisions      int
	pending        int
}

func (f *governanceAuthFixture) snapshot(orderID string) governanceState {
	f.t.Helper()
	var state governanceState
	var err error
	if orderID != "" {
		if state.order, err = f.store.GetWorkOrder(f.ctx, orderID); err != nil {
			f.t.Fatal(err)
		}
		if state.task, err = f.store.GetTask(f.ctx, state.order.TaskID); err != nil {
			f.t.Fatal(err)
		}
		events, listErr := f.store.ListEvents(f.ctx, state.order.TaskID)
		if listErr != nil {
			f.t.Fatal(listErr)
		}
		state.events = len(events)
	}
	designs, err := f.store.ListSystemDesignVersions(f.ctx, governanceDesignID)
	if err != nil {
		f.t.Fatal(err)
	}
	requirements, err := f.store.ListRequirementVersions(f.ctx, governanceReqID)
	if err != nil {
		f.t.Fatal(err)
	}
	decisions, err := f.store.ListDecisions(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	pending, err := f.store.ListPendingProposals(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	state.designVersions, state.reqVersions, state.decisions, state.pending = len(designs), len(requirements), len(decisions), len(pending)
	return state
}

func (f *governanceAuthFixture) requireUnchanged(label string, before governanceState) {
	f.t.Helper()
	if after := f.snapshot(before.order.ID); !reflect.DeepEqual(before, after) {
		f.t.Fatalf("%s changed state:\nbefore=%+v\nafter=%+v", label, before, after)
	}
}

// requireViewerRefusal asserts the canonical capability refusal and that the
// refusing check was the tool's claim_work capability.
func (f *governanceAuthFixture) requireViewerRefusal(label, text string, isErr bool) {
	f.t.Helper()
	if !isErr || !strings.Contains(text, "workspace_not_found") {
		f.t.Fatalf("%s isError=%v text=%q, want workspace_not_found", label, isErr, text)
	}
	if got := f.members.lastCapability(); got != core.CapabilityClaimWork {
		f.t.Fatalf("%s refusing capability=%q, want claim_work", label, got)
	}
}

func TestMCPDemotedUserRunClaimRefusesMutations(t *testing.T) {
	t.Parallel()
	for _, tool := range claimMutationMCPTools {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			f := newGovernanceAuthFixture(t)
			owner := "usr_demoted_run"
			userToken := f.user(owner, core.WorkspaceRoleExecutor)
			orderID := f.order("run", core.StageImplement)
			const session = "run-session"
			if text, isErr := f.call(userToken, "claim_work_order", map[string]any{
				"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": session, "client_token": "run-secret",
				"agent": "codex", "model": "model", "lease_seconds": 3600,
			}); isErr {
				t.Fatalf("claim: %s", text)
			}
			claimed, err := f.store.GetWorkOrder(f.ctx, orderID)
			if err != nil || claimed.State != core.WorkOrderClaimed || claimed.ClaimantID != core.TaskRunClaimantID(owner) || claimed.SessionID != session || claimed.WorkerID != "" || !claimed.LeaseExpiresAt.After(time.Now()) {
				t.Fatalf("user-run claim=%+v err=%v", claimed, err)
			}

			// Demote without releasing the claim: the call-time capability
			// check is the mechanism, not claim release.
			f.members.setRole(owner, governanceWorkspace, core.WorkspaceRoleViewer)
			before := f.snapshot(orderID)
			if before.order.State != core.WorkOrderClaimed || before.order.SessionID != session {
				t.Fatalf("demotion released the claim: %+v", before.order)
			}
			text, isErr := f.call(userToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, session))
			f.requireViewerRefusal("demoted "+tool, text, isErr)
			f.requireUnchanged("demoted "+tool, before)

			// Control: the same call with the same live claim succeeds once
			// the binding is restored, so the refusal came from demotion alone.
			f.members.setRole(owner, governanceWorkspace, core.WorkspaceRoleExecutor)
			if text, isErr = f.call(userToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, session)); isErr {
				t.Fatalf("restored executor %s: %s", tool, text)
			}
		})
	}
}

func TestMCPGovernanceCapabilityAndClaimMatrix(t *testing.T) {
	t.Parallel()
	roles := []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator}

	requireSuccess := func(t *testing.T, f *governanceAuthFixture, tool, orderID, text string) {
		t.Helper()
		taskID := strings.Replace(orderID, "gov-order-", "gov-task-", 1)
		switch tool {
		case "request_plan_revision":
			events, err := f.store.ListEvents(f.ctx, taskID)
			if err != nil || !slices.ContainsFunc(events, func(event core.Event) bool { return strings.Contains(string(event.Kind), "plan_revision") }) {
				t.Fatalf("plan revision events=%+v err=%v", events, err)
			}
		}
	}

	for _, tool := range governanceMCPTools {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			for _, role := range roles {
				t.Run("own user-run claim as "+string(role), func(t *testing.T) {
					f := newGovernanceAuthFixture(t)
					owner := "usr_" + string(role)
					userToken := f.user(owner, role)
					orderID := f.order("own", core.StageImplement)
					f.claimRun(orderID, owner, "own-session")
					before := f.snapshot(orderID)
					text, isErr := f.call(userToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"))
					if role == core.WorkspaceRoleViewer {
						f.requireViewerRefusal("viewer "+tool, text, isErr)
						f.requireUnchanged("viewer "+tool, before)
						return
					}
					if isErr {
						t.Fatalf("%s %s: %s", role, tool, text)
					}
					if got := f.members.lastCapability(); got != core.CapabilityClaimWork {
						t.Fatalf("%s capability=%q", role, got)
					}
					requireSuccess(t, f, tool, orderID, text)
				})
			}

			for _, refusal := range []struct {
				name  string
				setup func(f *governanceAuthFixture) (token string, args map[string]any, orderID string)
				want  string
			}{
				{name: "foreign owner user-run claim", want: store.ErrWorkOrderClaimUnauthorized.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					f.user("usr_other", core.WorkspaceRoleExecutor)
					orderID := f.order("foreign", core.StageImplement)
					f.claimRun(orderID, "usr_other", "foreign-session")
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "foreign-session"), orderID
				}},
				{name: "wrong session", want: "claim", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleExecutor)
					orderID := f.order("session", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "other-session"), orderID
				}},
				{name: "wrong workspace", want: "workspace_not_found", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("workspace", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					return userToken, governanceCallArgs(tool, "beta", orderID, "own-session"), orderID
				}},
				{name: "missing order", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					return userToken, governanceCallArgs(tool, governanceWorkspace, "gov-order-missing", "own-session"), ""
				}},
				{name: "expired lease", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("expired", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					f.ageClaim(orderID, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"), orderID
				}},
				{name: "elapsed execution deadline", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("deadline", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					f.ageClaim(orderID, func(order *core.WorkOrder) { order.ExecutionDeadline = time.Now().Add(-time.Minute) })
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"), orderID
				}},
				{name: "non-implement stage", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("spec", core.StageSpec)
					f.claimRun(orderID, "usr_caller", "own-session")
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"), orderID
				}},
			} {
				t.Run(refusal.name, func(t *testing.T) {
					f := newGovernanceAuthFixture(t)
					token, args, orderID := refusal.setup(f)
					before := f.snapshot(orderID)
					text, isErr := f.call(token, tool, args)
					if !isErr || !strings.Contains(text, refusal.want) {
						t.Fatalf("isError=%v text=%q, want %q", isErr, text, refusal.want)
					}
					f.requireUnchanged(refusal.name, before)
				})
			}
		})
	}
}

// ageClaim moves a stored claim's clock fields into the past deterministically,
// instead of sleeping past a short lease.
func (f *governanceAuthFixture) ageClaim(orderID string, mutate func(*core.WorkOrder)) {
	f.t.Helper()
	order, err := f.store.GetWorkOrder(f.ctx, orderID)
	if err != nil {
		f.t.Fatal(err)
	}
	mutate(&order)
	if err = storetest.For(f.store).UpdateWorkOrder(f.ctx, order); err != nil {
		f.t.Fatal(err)
	}
	stored, err := f.store.GetWorkOrder(f.ctx, orderID)
	if err != nil || liveWorkOrderClaim(stored, time.Now()) {
		f.t.Fatalf("aged claim still live: %+v err=%v", stored, err)
	}
}

// TestMCPToolCapabilityDecidesEveryRole iterates the capability table rather
// than a fixed tool list. With the agent ceiling gone, a tool is admitted exactly
// when the credential owner's live role grants its capability: a member without
// it is refused, every refusal leaves the order and task unchanged, and a future
// tool is classified by its capability alone (req-security-boundaries REQ-1,
// AC-1.1; req-accounts-and-membership AC-3.2, AC-5.1; component-mcp-protocol
// invariant 6). It mutates the package-level table for the synthetic cases, so
// it must not run in parallel.
func TestMCPToolCapabilityDecidesEveryRole(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	orderID := f.order("capability", core.StageImplement)
	other := f.order("capability-other", core.StageImplement)
	taskID := strings.Replace(orderID, "gov-order-", "gov-task-", 1)
	otherTask := strings.Replace(other, "gov-order-", "gov-task-", 1)
	args := map[string]any{
		"workspace_id": governanceWorkspace, "task_id": taskID, "depends_on_task_id": otherTask, "reason": "ordering",
		"request_id": "capability-request", "assignee_user_id": "usr_capability_operator", "branch": "feature/capability", "work_order_id": orderID,
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range mcpCapabilities {
			if !yield(name) {
				return
			}
		}
	})
	refusals := []string{"workspace_not_found", store.ErrWorkOrderClaimUnauthorized.Error(), "verification access refused"}
	refusedForViewer := 0
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor} {
		token := f.user("usr_capability_"+string(role), role)
		for _, name := range names {
			capability := mcpCapabilities[name]
			if core.RoleAllows(role, capability) {
				continue
			}
			before := f.snapshot(orderID)
			callArgs := args
			if name == "refresh_work_order_context" {
				// This tool refuses an unknown field before anything else.
				callArgs = map[string]any{"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": "capability-session"}
			}
			text, isErr := f.call(token, name, callArgs)
			if !isErr || !slices.ContainsFunc(refusals, func(refusal string) bool { return strings.Contains(text, refusal) }) {
				t.Fatalf("%s without %s (%s): isError=%v text=%q, want a refusal", role, capability, name, isErr, text)
			}
			f.requireUnchanged(string(role)+" "+name, before)
			if role == core.WorkspaceRoleViewer {
				refusedForViewer++
			}
		}
	}
	if refusedForViewer == 0 {
		t.Fatal("no tool was refused for a viewer")
	}
	// The reserved tools stay out of reach of a member without their capability.
	contributor := f.user("usr_capability_reserved", core.WorkspaceRoleContributor)
	for _, name := range []string{"add_task_dependency", "attach_task_branch", "set_assignee", "create_task"} {
		before := f.snapshot(orderID)
		if text, isErr := f.call(contributor, name, args); !isErr || !strings.Contains(text, "workspace_not_found") {
			t.Fatalf("contributor %s isError=%v text=%q", name, isErr, text)
		}
		f.requireUnchanged("contributor "+name, before)
	}

	// A future tool is classified by its capability alone, with no name list
	// to edit: a member without it is refused before dispatch, and a member
	// with it reaches dispatch.
	const maintainerTool = "synthetic_maintainer_tool"
	mcpCapabilities[maintainerTool] = core.CapabilityOperateGates
	t.Cleanup(func() { delete(mcpCapabilities, maintainerTool) })
	if text, isErr := f.call(contributor, maintainerTool, args); !isErr || !strings.Contains(text, "workspace_not_found") {
		t.Fatalf("synthetic tool for contributor isError=%v text=%q", isErr, text)
	}
	operator := f.user("usr_capability_operator", core.WorkspaceRoleOperator)
	if text, isErr := f.call(operator, maintainerTool, args); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("synthetic tool for operator isError=%v text=%q", isErr, text)
	}
	// An unmapped tool fails closed.
	if text, isErr := f.call(operator, "unmapped_tool", args); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("unmapped tool isError=%v text=%q", isErr, text)
	}
}

// TestMCPRefusesARetiredAgentCredentialAtTheMount proves a credential of the
// retired kind reaches no tool and is answered like an unknown bearer, whatever
// role its owner holds (component-http-api invariant 4; DEC-116(1), (3)).
func TestMCPRefusesARetiredAgentCredentialAtTheMount(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	f.user("usr_retired_owner", core.WorkspaceRoleOperator)
	f.credentials["agent-retired"] = core.AuthenticatedCredential{ID: "agt_retired", OwnerUserID: "usr_retired_owner", Kind: retiredAgentKind, Scope: core.CredentialScopeUser}
	post := func(token string) *httptest.ResponseRecorder {
		payload := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(payload))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		return response
	}
	retired, unknown := post("agent-retired"), post("not-a-stored-credential")
	if retired.Code != http.StatusUnauthorized || retired.Body.String() != unknown.Body.String() || retired.Header().Get("WWW-Authenticate") != unknown.Header().Get("WWW-Authenticate") {
		t.Fatalf("retired status=%d body=%q, unknown status=%d body=%q", retired.Code, retired.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// TestMCPDocumentReadsOutsideAnOwnLiveClaimRecordNoConsultation is the claim
// matrix for document reads. authorizeDocumentReadClaim no longer decides whether
// a read is served, because a session reads with its person's view_workspace. It
// decides only whether the read is attributed: outside the caller's own live
// claim the read is served and records no consultation, and a caller outside the
// workspace is refused before any body read (req-design-delivery REQ-5;
// req-operator-next-action AC-5.3; DEC-103; DEC-116(2)).
func TestMCPDocumentReadsOutsideAnOwnLiveClaimRecordNoConsultation(t *testing.T) {
	type claimCase struct {
		name string
		// refused marks a caller the workspace boundary refuses outright.
		refused bool
		setup   func(f *documentReadFixture) (token string, claim map[string]any)
	}
	liveClaim := func(f *documentReadFixture, owner, name string, role core.WorkspaceRole) (string, string, string) {
		token := f.user(owner, role)
		orderID := f.order(name, core.StageImplement)
		f.claimRun(orderID, owner, name+"-session")
		return token, orderID, name + "-session"
	}
	cases := []claimCase{
		{name: "no claim arguments", setup: func(f *documentReadFixture) (string, map[string]any) {
			return f.user("usr_none", core.WorkspaceRoleOperator), nil
		}},
		{name: "work order without session", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, _ := liveClaim(f, "usr_partial", "partial", core.WorkspaceRoleOperator)
			return token, map[string]any{"work_order_id": orderID}
		}},
		{name: "session without work order", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, _, session := liveClaim(f, "usr_partial", "partial", core.WorkspaceRoleOperator)
			return token, map[string]any{"session_id": session}
		}},
		{name: "unknown order", setup: func(f *documentReadFixture) (string, map[string]any) {
			return f.user("usr_unknown", core.WorkspaceRoleOperator), map[string]any{"work_order_id": "gov-order-absent", "session_id": "absent-session"}
		}},
		{name: "unclaimed order", setup: func(f *documentReadFixture) (string, map[string]any) {
			return f.user("usr_queued", core.WorkspaceRoleOperator), map[string]any{"work_order_id": f.order("queued", core.StageImplement), "session_id": "queued-session"}
		}},
		{name: "foreign claim", setup: func(f *documentReadFixture) (string, map[string]any) {
			_, orderID, session := liveClaim(f, "usr_owner", "foreign", core.WorkspaceRoleOperator)
			return f.user("usr_other", core.WorkspaceRoleOperator), map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "another session of the own order", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, _ := liveClaim(f, "usr_session", "session", core.WorkspaceRoleOperator)
			return token, map[string]any{"work_order_id": orderID, "session_id": "not-this-session"}
		}},
		{name: "session of another order", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, _, session := liveClaim(f, "usr_cross", "cross-a", core.WorkspaceRoleOperator)
			other := f.order("cross-b", core.StageImplement)
			f.claimRun(other, "usr_cross", "cross-b-session")
			return token, map[string]any{"work_order_id": other, "session_id": session}
		}},
		{name: "expired lease", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_expired", "expired", core.WorkspaceRoleOperator)
			f.ageClaim(orderID, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "elapsed execution deadline", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_deadline", "deadline", core.WorkspaceRoleOperator)
			f.ageClaim(orderID, func(order *core.WorkOrder) { order.ExecutionDeadline = time.Now().Add(-time.Minute) })
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "released claim", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_released", "released", core.WorkspaceRoleOperator)
			if text, isErr := f.call(token, "release_work_order", map[string]any{"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": session, "reason": "done"}); isErr {
				f.t.Fatal(text)
			}
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "historical worker owned order", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_worker", "worker", core.WorkspaceRoleOperator)
			order, err := f.store.GetWorkOrder(f.ctx, orderID)
			if err != nil {
				f.t.Fatal(err)
			}
			order.WorkerID = "historical-worker"
			if err = storetest.For(f.store).UpdateWorkOrder(f.ctx, order); err != nil {
				f.t.Fatal(err)
			}
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "owner demoted to viewer", setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_demoted", "demoted", core.WorkspaceRoleOperator)
			f.members.setRole("usr_demoted", governanceWorkspace, core.WorkspaceRoleViewer)
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
		{name: "owner lost membership", refused: true, setup: func(f *documentReadFixture) (string, map[string]any) {
			token, orderID, session := liveClaim(f, "usr_removed", "removed", core.WorkspaceRoleOperator)
			f.members.setRole("usr_removed", governanceWorkspace, "")
			return token, map[string]any{"work_order_id": orderID, "session_id": session}
		}},
	}
	section := func(extra ...any) map[string]any {
		return documentReadArgs("design-refusal", append([]any{"section_id", "s0"}, extra...)...)
	}
	modes := []struct {
		name, tool string
		args       map[string]any
	}{
		{"whole document", "get_document", documentReadArgs("design-refusal")},
		{"outline", "get_document", documentReadArgs("design-refusal", "outline", true)},
		{"section", "get_document", section()},
		{"named version section", "get_document", section("version", 1)},
		{"search", "search_documents", map[string]any{"workspace_id": governanceWorkspace, "query": "refusal-needle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDocumentReadFixture(t)
			f.design("design-refusal", designBody("## Refusal\nrefusal-needle\n"))
			counter := f.countDocumentReads()
			token, claim := tc.setup(f)
			caller := documentCaller{name: tc.name, token: token, claim: claim}
			for _, mode := range modes {
				before := counter.reads.Load()
				text, isErr := f.call(token, mode.tool, caller.args(mode.args))
				if tc.refused {
					if !isErr || !strings.Contains(text, "workspace_not_found") || strings.Contains(text, "refusal-needle") {
						t.Fatalf("%s: isError=%v text=%q, want workspace_not_found", mode.name, isErr, text)
					}
					if got := counter.reads.Load() - before; got != 0 {
						t.Fatalf("%s refused after %d body reads", mode.name, got)
					}
				} else if isErr {
					t.Fatalf("%s refused: %s", mode.name, text)
				}
				f.wantConsultations(mode.name)
			}
		})
	}

	t.Run("a claim lost between pages records nothing more and still serves the page", func(t *testing.T) {
		f := newDocumentReadFixture(t)
		f.design("design-refusal", designBody("## Refusal\n"+strings.Repeat("refusal-needle \"x\"\n", 8000)))
		token, orderID, session := liveClaim(f, "usr_pages", "pages", core.WorkspaceRoleOperator)
		caller := documentCaller{name: "person", token: token, claim: map[string]any{"work_order_id": orderID, "session_id": session}}
		first := f.readItem(caller, "get_document", section())
		if first["truncated"] != true {
			t.Fatalf("first page=%v", first["truncated"])
		}
		next := section("version", first["version"], "byte_offset", first["next_byte_offset"])
		recorded := f.consultations()
		if len(recorded) != 1 {
			t.Fatalf("consultations=%+v, want the first page's one", recorded)
		}
		f.ageClaim(orderID, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
		if second := f.readItem(caller, "get_document", next); second["byte_offset"] != first["next_byte_offset"] {
			t.Fatalf("second page offset=%v", second["byte_offset"])
		}
		if got := f.consultations(); len(got) != 1 {
			t.Fatalf("consultations after expiry=%+v", got)
		}
	})

	t.Run("reads leave the claim and task unchanged", func(t *testing.T) {
		f := newDocumentReadFixture(t)
		f.design("design-refusal", designBody("## Refusal\nrefusal-needle\n"))
		token, orderID, session := liveClaim(f, "usr_observe", "observe", core.WorkspaceRoleOperator)
		caller := documentCaller{name: "person", token: token, claim: map[string]any{"work_order_id": orderID, "session_id": session}}
		before := f.snapshot(orderID)
		f.readItem(caller, "get_document", documentReadArgs("design-refusal", "outline", true))
		f.search(caller, "refusal-needle")
		f.requireUnchanged("document reads", before)
		order, err := f.store.GetWorkOrder(f.ctx, orderID)
		if err != nil || !liveWorkOrderClaim(order, time.Now()) {
			t.Fatalf("claim disturbed: %+v err=%v", order, err)
		}
	})

	t.Run("a user credential needs no claim and ignores claim arguments", func(t *testing.T) {
		f := newDocumentReadFixture(t)
		f.design("design-refusal", designBody("## Refusal\nrefusal-needle\n"))
		userToken := f.user("usr_viewer", core.WorkspaceRoleViewer)
		viewer := documentCaller{name: "viewer", token: userToken}
		if item := f.readItem(viewer, "get_document", section()); item["heading"] != "Refusal" {
			t.Fatalf("viewer section=%v", item)
		}
		named := documentCaller{name: "viewer", token: userToken, claim: map[string]any{"work_order_id": "gov-order-absent", "session_id": "absent"}}
		if result := f.search(named, "refusal-needle"); result.Total != 1 {
			t.Fatalf("viewer search=%+v", result)
		}
		nonMember := f.user("usr_gone", core.WorkspaceRoleViewer)
		f.members.setRole("usr_gone", governanceWorkspace, "")
		if text, isErr := f.call(nonMember, "search_documents", map[string]any{"workspace_id": governanceWorkspace, "query": "refusal-needle"}); !isErr || !strings.Contains(text, "workspace_not_found") {
			t.Fatalf("nonmember: isError=%v text=%q", isErr, text)
		}
	})
}
