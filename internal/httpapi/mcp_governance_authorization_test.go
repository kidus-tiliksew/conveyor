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
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	governanceWorkspace = "demo"
	governanceDesignID  = "design-governance-auth"
	governanceReqID     = "req-governance-auth"
)

var governanceMCPTools = []string{"request_plan_revision", "propose_system_design_revision", "propose_requirement_revision", "propose_decision"}

// claimMutationMCPTools are the seven mutations a live claim admits for its
// holder; a viewer binding must refuse every one of them.
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
	ctx := store.WithWorkspace(t.Context(), governanceWorkspace)
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
	server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
	return &governanceAuthFixture{t: t, ctx: ctx, store: st, server: server, handler: server.Handler(), members: members, credentials: credentials}
}

// user registers a user personal access token and an agent credential for
// owner, binding owner to role in the fixture workspace.
func (f *governanceAuthFixture) user(owner string, role core.WorkspaceRole) (userToken, agentToken string) {
	f.members.setRole(owner, governanceWorkspace, role)
	userToken, agentToken = "pat-"+owner, "agent-"+owner
	f.credentials[userToken] = core.AuthenticatedCredential{ID: "pat_" + owner, OwnerUserID: owner, Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	f.credentials[agentToken] = core.AuthenticatedCredential{ID: "agt_" + owner, OwnerUserID: owner, Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	return userToken, agentToken
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

// claimWorker enrolls a worker for owner and stores its live claim.
func (f *governanceAuthFixture) claimWorker(orderID, workerID, owner, session string) {
	f.t.Helper()
	if err := f.store.CreateWorker(f.ctx, core.Worker{ID: workerID, Workspace: governanceWorkspace, OwnerUserID: owner, Name: workerID, CredentialHash: "hash-" + workerID, CreatedAt: time.Now()}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, orderID, core.WorkOrderClaim{SessionID: session, ClientToken: "secret-" + session, ClaimantID: workerID, WorkerID: workerID, Agent: "codex", Model: "model", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
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
	case "propose_system_design_revision":
		args["document_id"] = governanceDesignID
		args["content"] = "# Governance revision\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"
	case "propose_requirement_revision":
		args["document_id"] = governanceReqID
		args["content"] = "# Governance\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep executor claims exact.\n```"
	case "propose_decision":
		args["statement"] = "Keep governance claims exact."
		args["context"] = "Claims may propose task-local governance."
		args["alternatives_rejected"] = "Grant freestanding proposal authority."
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
			userToken, _ := f.user(owner, core.WorkspaceRoleExecutor)
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

func TestMCPDemotedWorkerHeldAgentClaimRefusesMutations(t *testing.T) {
	t.Parallel()
	for _, tool := range claimMutationMCPTools {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			f := newGovernanceAuthFixture(t)
			owner := "usr_demoted_worker_owner"
			_, agentToken := f.user(owner, core.WorkspaceRoleExecutor)
			orderID := f.order("worker", core.StageImplement)
			const session = "worker-session"
			f.claimWorker(orderID, "worker-owned", owner, session)

			// Demote the owner in the membership fixture while the stored
			// claim and the enrolled worker both remain, so the refusal cannot
			// be attributed to worker revocation.
			f.members.setRole(owner, governanceWorkspace, core.WorkspaceRoleViewer)
			before := f.snapshot(orderID)
			if before.order.State != core.WorkOrderClaimed || before.order.WorkerID != "worker-owned" {
				t.Fatalf("worker claim=%+v", before.order)
			}
			workers, err := f.store.ListWorkers(f.ctx)
			if err != nil || !slices.ContainsFunc(workers, func(worker core.Worker) bool { return worker.ID == "worker-owned" && worker.RevokedAt.IsZero() }) {
				t.Fatalf("enrolled worker missing or revoked: %+v err=%v", workers, err)
			}
			text, isErr := f.call(agentToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, session))
			f.requireViewerRefusal("demoted worker-held "+tool, text, isErr)
			f.requireUnchanged("demoted worker-held "+tool, before)

			// Control with the binding restored. A worker-held claim is not
			// renewable by an agent credential; that separate boundary answers
			// claim loss, which the demoted call never reached.
			f.members.setRole(owner, governanceWorkspace, core.WorkspaceRoleExecutor)
			text, isErr = f.call(agentToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, session))
			if tool == "renew_work_order" {
				if !isErr || !strings.Contains(text, store.ErrWorkOrderClaimLost.Error()) {
					t.Fatalf("restored agent renew of worker claim isError=%v text=%q, want claim lost", isErr, text)
				}
				return
			}
			if isErr {
				t.Fatalf("restored executor agent %s: %s", tool, text)
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
		case "propose_system_design_revision":
			var version core.SystemDesignVersion
			if err := json.Unmarshal([]byte(text), &version); err != nil || version.Origin != core.SystemDesignOriginImplementation || version.OriginTaskID != taskID || version.Confirmed {
				t.Fatalf("design proposal=%+v err=%v", version, err)
			}
		case "propose_requirement_revision":
			var version core.RequirementVersion
			if err := json.Unmarshal([]byte(text), &version); err != nil || version.Origin != core.RequirementOriginImplementation || version.OriginTaskID != taskID || version.Confirmed {
				t.Fatalf("requirement proposal=%+v err=%v", version, err)
			}
		case "propose_decision":
			var decision core.Decision
			if err := json.Unmarshal([]byte(text), &decision); err != nil || decision.Origin != core.DecisionOriginImplementation || decision.OriginTaskID != taskID || decision.Status != core.DecisionProposed {
				t.Fatalf("decision proposal=%+v err=%v", decision, err)
			}
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
					userToken, _ := f.user(owner, role)
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

			t.Run("own worker-held claim through owner agent", func(t *testing.T) {
				f := newGovernanceAuthFixture(t)
				_, agentToken := f.user("usr_worker_owner", core.WorkspaceRoleExecutor)
				orderID := f.order("worker", core.StageImplement)
				f.claimWorker(orderID, "worker-own", "usr_worker_owner", "worker-session")
				text, isErr := f.call(agentToken, tool, governanceCallArgs(tool, governanceWorkspace, orderID, "worker-session"))
				if isErr {
					t.Fatalf("owned worker claim: %s", text)
				}
				requireSuccess(t, f, tool, orderID, text)
			})

			for _, refusal := range []struct {
				name  string
				setup func(f *governanceAuthFixture) (token string, args map[string]any, orderID string)
				want  string
			}{
				{name: "foreign owner user-run claim", want: store.ErrWorkOrderClaimUnauthorized.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
					f.user("usr_other", core.WorkspaceRoleExecutor)
					orderID := f.order("foreign", core.StageImplement)
					f.claimRun(orderID, "usr_other", "foreign-session")
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "foreign-session"), orderID
				}},
				{name: "foreign worker claim", want: store.ErrWorkOrderClaimUnauthorized.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					_, agentToken := f.user("usr_caller", core.WorkspaceRoleOperator)
					f.user("usr_other", core.WorkspaceRoleExecutor)
					orderID := f.order("foreign-worker", core.StageImplement)
					f.claimWorker(orderID, "worker-foreign", "usr_other", "foreign-worker-session")
					return agentToken, governanceCallArgs(tool, governanceWorkspace, orderID, "foreign-worker-session"), orderID
				}},
				{name: "wrong session", want: "claim", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleExecutor)
					orderID := f.order("session", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "other-session"), orderID
				}},
				{name: "wrong workspace", want: "workspace_not_found", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("workspace", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					return userToken, governanceCallArgs(tool, "beta", orderID, "own-session"), orderID
				}},
				{name: "missing order", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
					return userToken, governanceCallArgs(tool, governanceWorkspace, "gov-order-missing", "own-session"), ""
				}},
				{name: "expired lease", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("expired", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					f.ageClaim(orderID, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"), orderID
				}},
				{name: "elapsed execution deadline", want: store.ErrWorkOrderClaimLost.Error(), setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
					orderID := f.order("deadline", core.StageImplement)
					f.claimRun(orderID, "usr_caller", "own-session")
					f.ageClaim(orderID, func(order *core.WorkOrder) { order.ExecutionDeadline = time.Now().Add(-time.Minute) })
					return userToken, governanceCallArgs(tool, governanceWorkspace, orderID, "own-session"), orderID
				}},
				{name: "non-implement stage", setup: func(f *governanceAuthFixture) (string, map[string]any, string) {
					userToken, _ := f.user("usr_caller", core.WorkspaceRoleOperator)
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

// TestMCPAgentCapabilityCeilingUsesToolTable iterates the capability table
// rather than a fixed tool list. It mutates the package-level table for the
// synthetic cases, so it must not run in parallel; parallel tests resume only
// after this sequential test restores the table.
func TestMCPAgentCapabilityCeilingUsesToolTable(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	userToken, agentToken := f.user("usr_ceiling_operator", core.WorkspaceRoleOperator)
	orderID := f.order("ceiling", core.StageImplement)
	other := f.order("ceiling-other", core.StageImplement)
	taskID := strings.Replace(orderID, "gov-order-", "gov-task-", 1)
	otherTask := strings.Replace(other, "gov-order-", "gov-task-", 1)
	args := map[string]any{
		"workspace_id": governanceWorkspace, "task_id": taskID, "depends_on_task_id": otherTask, "reason": "ordering",
		"request_id": "ceiling-request", "assignee_user_id": "usr_ceiling_operator", "branch": "feature/ceiling", "work_order_id": orderID,
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range mcpCapabilities {
			if !yield(name) {
				return
			}
		}
	})
	var refused []string
	for _, name := range names {
		capability := mcpCapabilities[name]
		before := f.snapshot(orderID)
		text, isErr := f.call(agentToken, name, args)
		ceilingRefusal := isErr && strings.Contains(text, name+" requires an operator-scoped user credential")
		switch {
		case !agentMayExerciseCapability(capability):
			if !ceilingRefusal {
				t.Fatalf("%s (%s) isError=%v text=%q, want ceiling refusal", name, capability, isErr, text)
			}
			f.requireUnchanged(name, before)
			refused = append(refused, name)
		case isMCPRead(name) || name == "report_continuation":
			// Independent user-only read and launcher-only continuation rules.
			if !ceilingRefusal {
				t.Fatalf("%s isError=%v text=%q, want credential refusal", name, isErr, text)
			}
		default:
			if ceilingRefusal {
				t.Fatalf("%s (%s) was refused by the agent ceiling", name, capability)
			}
		}
	}
	// The derived refusals are exactly the tools mapped above contributor
	// other than create_tasks; create_task stays admitted.
	if slices.Contains(refused, "create_task") || len(refused) == 0 {
		t.Fatalf("ceiling refused=%v", refused)
	}
	for _, name := range []string{"add_task_dependency", "attach_task_branch", "set_assignee", "redispatch_work_order"} {
		if !slices.Contains(refused, name) {
			t.Fatalf("ceiling refused=%v lacks %s", refused, name)
		}
	}

	// A future tool is classified by its capability alone, with no name
	// list to edit: a maintainer capability is refused for the agent, while a
	// contributor capability passes the ceiling and reaches dispatch.
	const maintainerTool, contributorTool = "synthetic_maintainer_tool", "synthetic_contributor_tool"
	mcpCapabilities[maintainerTool] = core.CapabilityOperateGates
	mcpCapabilities[contributorTool] = core.CapabilityProposeDocuments
	t.Cleanup(func() {
		delete(mcpCapabilities, maintainerTool)
		delete(mcpCapabilities, contributorTool)
	})
	if text, isErr := f.call(agentToken, maintainerTool, args); !isErr || !strings.Contains(text, maintainerTool+" requires an operator-scoped user credential") {
		t.Fatalf("synthetic maintainer tool isError=%v text=%q", isErr, text)
	}
	if text, isErr := f.call(agentToken, contributorTool, args); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("synthetic contributor tool isError=%v text=%q, want dispatch to unknown tool", isErr, text)
	}
	// The ceiling binds agent credentials only; the owner's own credential
	// reaches dispatch for the same synthetic maintainer tool.
	if text, isErr := f.call(userToken, maintainerTool, args); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("synthetic maintainer tool for user isError=%v text=%q", isErr, text)
	}
	// An unmapped tool fails closed for an agent.
	if text, isErr := f.call(agentToken, "unmapped_tool", args); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("unmapped tool isError=%v text=%q", isErr, text)
	}
}
