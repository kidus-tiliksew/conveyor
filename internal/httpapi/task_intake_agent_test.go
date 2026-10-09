package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// lockedMemberships serializes the shared membership fixture so concurrent
// intake requests stay race-free under -race.
type lockedMemberships struct {
	mu    sync.Mutex
	inner *membershipFixture
}

func (l *lockedMemberships) ListWorkspaces(ctx context.Context) ([]core.Workspace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.ListWorkspaces(ctx)
}
func (l *lockedMemberships) GetWorkspace(ctx context.Context, id string) (core.Workspace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.GetWorkspace(ctx, id)
}
func (l *lockedMemberships) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.CreateWorkspace(ctx, id, name, cfg)
}
func (l *lockedMemberships) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
}
func (l *lockedMemberships) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.AuthorizeDeployment(ctx, userID, capability)
}
func (l *lockedMemberships) ListWorkspacesForUser(ctx context.Context, userID string) ([]core.Workspace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.ListWorkspacesForUser(ctx, userID)
}
func (l *lockedMemberships) ListWorkspaceMembers(ctx context.Context, workspaceID, userID string) ([]core.WorkspaceMembership, error) {
	return l.inner.ListWorkspaceMembers(ctx, workspaceID, userID)
}
func (l *lockedMemberships) ListWorkspaceInvitations(ctx context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.ListWorkspaceInvitations(ctx, workspaceID)
}
func (l *lockedMemberships) GrantWorkspaceRole(ctx context.Context, workspaceID, email string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	return l.inner.GrantWorkspaceRole(ctx, workspaceID, email, role)
}
func (l *lockedMemberships) RevokeWorkspaceInvitation(ctx context.Context, email, workspaceID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.RevokeWorkspaceInvitation(ctx, email, workspaceID)
}
func (l *lockedMemberships) RevokeWorkspaceRole(ctx context.Context, workspaceID, userID string) error {
	return l.inner.RevokeWorkspaceRole(ctx, workspaceID, userID)
}

// setRole changes or removes (empty role) a binding under the lock.
func (l *lockedMemberships) setRole(userID, workspaceID string, role core.WorkspaceRole) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if role == "" {
		delete(l.inner.roles[userID], workspaceID)
		return
	}
	if l.inner.roles[userID] == nil {
		l.inner.roles[userID] = map[string]core.WorkspaceRole{}
	}
	l.inner.roles[userID][workspaceID] = role
}

func (l *lockedMemberships) lastCapability() core.Capability {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.inner.capabilityCalls) == 0 {
		return ""
	}
	return l.inner.capabilityCalls[len(l.inner.capabilityCalls)-1]
}

// agentIntakeFixture serves full /mcp and REST requests through credential
// verification, workspace resolution, and live membership authorization.
type agentIntakeFixture struct {
	server    *Server
	handler   http.Handler
	store     store.Store
	members   *lockedMemberships
	cfgMu     sync.Mutex
	cfg       *config.Config
	cfgErr    error
	titles    atomic.Int32
	notified  atomic.Int32
	titleHook func(context.Context, core.Task) (string, error)
}

var agentIntakeRoles = []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator}

func agentIntakeToken(role core.WorkspaceRole) string { return "agent-" + string(role) }
func humanIntakeToken(role core.WorkspaceRole) string { return "human-" + string(role) }
func intakeOwner(role core.WorkspaceRole) string      { return "usr_" + string(role) }
func intakeAgentID(role core.WorkspaceRole) string    { return "agt_" + string(role) }

func newAgentIntakeFixture(t *testing.T) *agentIntakeFixture {
	t.Helper()
	members := &lockedMemberships{inner: &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}, {ID: "beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{},
	}}
	credentials := staticCredentialVerifier{}
	for _, role := range agentIntakeRoles {
		members.setRole(intakeOwner(role), "alpha", role)
		credentials[agentIntakeToken(role)] = core.AuthenticatedCredential{ID: intakeAgentID(role), OwnerUserID: intakeOwner(role), Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
		credentials[humanIntakeToken(role)] = core.AuthenticatedCredential{ID: "pat_" + string(role), OwnerUserID: intakeOwner(role), Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	}
	// A second agent of the maintainer owner, and a run child bound to alpha.
	credentials["agent-maintainer-second"] = core.AuthenticatedCredential{ID: "agt_maintainer_second", OwnerUserID: intakeOwner(core.WorkspaceRoleMaintainer), Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	credentials["run-child"] = core.AuthenticatedCredential{ID: "agt_run_child", OwnerUserID: intakeOwner(core.WorkspaceRoleMaintainer), Kind: core.CredentialAgent, Scope: core.CredentialScopeUser,
		RunWorkspaceID: "alpha", RunWorkOrderID: "bound-order", RunSessionID: "bound-session"}
	members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "beta", core.WorkspaceRoleMaintainer)
	members.setRole("usr_foreign", "beta", core.WorkspaceRoleOperator)
	credentials["agent-foreign"] = core.AuthenticatedCredential{ID: "agt_foreign", OwnerUserID: "usr_foreign", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	credentials["agent-unbound"] = core.AuthenticatedCredential{ID: "agt_unbound", OwnerUserID: "usr_unbound", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}

	f := &agentIntakeFixture{store: store.NewMemory(), members: members, cfg: &config.Config{
		Workspace: "alpha",
		Execution: config.ExecutionPolicy{SpecApproval: true, MergeApproval: true},
		Repos:     []config.Repo{{Name: "api", Base: "main"}, {Name: "ui", Base: "trunk"}},
	}}
	f.server = NewServer(f.store)
	f.server.Workspaces, f.server.Memberships = members, members
	f.server.Credentials = credentials
	f.server.ConfigProvider = func(context.Context) (*config.Config, error) {
		f.cfgMu.Lock()
		defer f.cfgMu.Unlock()
		if f.cfgErr != nil {
			return nil, f.cfgErr
		}
		copied := *f.cfg
		return &copied, nil
	}
	f.server.GenerateTaskTitle = func(ctx context.Context, task core.Task) (string, error) {
		f.titles.Add(1)
		if f.titleHook != nil {
			return f.titleHook(ctx, task)
		}
		return "Generated title", nil
	}
	f.server.OnCreate = func(context.Context, string) { f.notified.Add(1) }
	f.handler = f.server.Handler()
	return f
}

func (f *agentIntakeFixture) setDefaults(spec, merge bool) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	copied := *f.cfg
	copied.Execution.SpecApproval, copied.Execution.MergeApproval = spec, merge
	f.cfg = &copied
}

func (f *agentIntakeFixture) setConfigError(err error) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	f.cfgErr = err
}

type mcpCallResult struct {
	task    core.Task
	created bool
	errText string
}

// callMCP posts one tools/call request through the full /mcp handler.
func (f *agentIntakeFixture) callMCP(t *testing.T, token, tool string, args map[string]any) mcpCallResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer "+token)
	// A client-asserted actor header is never identity.
	request.Header.Set("X-Conveyor-Actor", "spoofed-actor")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", tool, response.Code, response.Body.String())
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
		t.Fatal(err)
	}
	if envelope.Error != nil {
		return mcpCallResult{errText: envelope.Error.Message}
	}
	if len(envelope.Result.Content) != 1 {
		t.Fatalf("content=%+v", envelope.Result.Content)
	}
	text := envelope.Result.Content[0].Text
	if envelope.Result.IsError {
		return mcpCallResult{errText: text}
	}
	if tool != "create_task" {
		return mcpCallResult{}
	}
	var result struct {
		Task    core.Task `json:"task"`
		Created bool      `json:"created"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	return mcpCallResult{task: result.Task, created: result.Created}
}

func intakeArgs(key string, extra map[string]any) map[string]any {
	args := map[string]any{"workspace_id": "alpha", "body": "Agent filed **work**\n\n- keep verbatim", "repo": "api", "idempotency_key": key}
	maps.Copy(args, extra)
	return args
}

func (f *agentIntakeFixture) taskCount(t *testing.T) int {
	t.Helper()
	tasks, err := f.store.ListTasks(store.WithWorkspace(t.Context(), "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func (f *agentIntakeFixture) createdEvents(t *testing.T, taskID string) []core.Event {
	t.Helper()
	events, err := f.store.ListEvents(store.WithWorkspace(t.Context(), "alpha"), taskID)
	if err != nil {
		t.Fatal(err)
	}
	var created []core.Event
	for _, event := range events {
		if event.Kind == "task.created" {
			created = append(created, event)
		}
	}
	return created
}

// requireNoSideEffects asserts a refused call reached no title generation,
// task write, creation event, or triage notification.
func (f *agentIntakeFixture) requireNoSideEffects(t *testing.T, label string, tasks int, titles, notified int32) {
	t.Helper()
	if got := f.taskCount(t); got != tasks {
		t.Fatalf("%s: tasks=%d want %d", label, got, tasks)
	}
	if got := f.titles.Load(); got != titles {
		t.Fatalf("%s: title calls=%d want %d", label, got, titles)
	}
	if got := f.notified.Load(); got != notified {
		t.Fatalf("%s: triage notifications=%d want %d", label, got, notified)
	}
}

func requireAgentProvenance(t *testing.T, f *agentIntakeFixture, taskID, agentID, ownerID string) {
	t.Helper()
	created := f.createdEvents(t, taskID)
	if len(created) != 1 {
		t.Fatalf("task.created events=%d", len(created))
	}
	event := created[0]
	if event.ActorID != store.AgentActorID(agentID) || event.ActorRole != core.ActorAgent {
		t.Fatalf("actor=%q role=%q", event.ActorID, event.ActorRole)
	}
	provenance, ok, err := store.TaskCreatedProvenance(event.Payload)
	if err != nil || !ok || provenance.AgentCredentialID != agentID || provenance.OwnerUserID != ownerID {
		t.Fatalf("provenance=%+v ok=%t err=%v payload=%s", provenance, ok, err, event.Payload)
	}
}

func agentCreatesTask(t *testing.T, role core.WorkspaceRole) {
	t.Helper()
	f := newAgentIntakeFixture(t)
	result := f.callMCP(t, agentIntakeToken(role), "create_task", intakeArgs("agent-"+string(role), map[string]any{"source": "mcp:issue-7"}))
	if result.errText != "" || !result.created {
		t.Fatalf("create err=%q created=%t", result.errText, result.created)
	}
	task := result.task
	if task.Workspace != "alpha" || task.Title != "Generated title" || task.Body != "Agent filed **work**\n\n- keep verbatim" || task.Source != "mcp:issue-7" ||
		!task.SpecApproval || !task.MergeApproval || task.Hold || task.State != core.TaskQueued || task.NextStage != core.StageTriage || task.Branch != "conveyor/task-"+task.ID {
		t.Fatalf("task=%+v", task)
	}
	requireAgentProvenance(t, f, task.ID, intakeAgentID(role), intakeOwner(role))
	if f.titles.Load() != 1 || f.notified.Load() != 1 || f.taskCount(t) != 1 {
		t.Fatalf("titles=%d notified=%d tasks=%d", f.titles.Load(), f.notified.Load(), f.taskCount(t))
	}
	if got := f.members.lastCapability(); got != core.CapabilityCreateTasks {
		t.Fatalf("authorized capability=%q", got)
	}
	persisted, err := f.store.GetTask(store.WithWorkspace(t.Context(), "alpha"), task.ID)
	if err != nil || persisted.Assignee != nil || persisted.State != core.TaskQueued {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestMCPAgentMaintainerCreatesTask(t *testing.T) {
	agentCreatesTask(t, core.WorkspaceRoleMaintainer)
}

func TestMCPAgentOperatorCreatesTask(t *testing.T) {
	agentCreatesTask(t, core.WorkspaceRoleOperator)
}

func TestMCPAgentNonMaintainerCannotCreateTask(t *testing.T) {
	f := newAgentIntakeFixture(t)
	refuse := func(label, token string, args map[string]any) {
		t.Helper()
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", args)
		if !strings.Contains(result.errText, "workspace_not_found") {
			t.Fatalf("%s: err=%q", label, result.errText)
		}
		f.requireNoSideEffects(t, label, tasks, titles, notified)
	}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor} {
		refuse(string(role), agentIntakeToken(role), intakeArgs("lower-"+string(role), nil))
	}
	refuse("absent binding", "agent-unbound", intakeArgs("unbound", nil))
	refuse("foreign workspace", "agent-foreign", intakeArgs("foreign", nil))

	// A binding revoked after a successful creation stops the next one.
	if result := f.callMCP(t, agentIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("before-revocation", nil)); result.errText != "" {
		t.Fatalf("before revocation: %q", result.errText)
	}
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", "")
	refuse("revoked binding", agentIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("after-revocation", nil))
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", core.WorkspaceRoleContributor)
	refuse("demoted binding", agentIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("after-demotion", nil))

	// Without a membership provider authorization fails closed.
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", core.WorkspaceRoleMaintainer)
	f.server.Memberships = nil
	refuse("membership provider unavailable", agentIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("no-provider", nil))
}

func TestMCPAgentGateOverrides(t *testing.T) {
	for _, defaults := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		t.Run(fmt.Sprintf("defaults spec=%t merge=%t", defaults[0], defaults[1]), func(t *testing.T) {
			f := newAgentIntakeFixture(t)
			f.setDefaults(defaults[0], defaults[1])
			token := agentIntakeToken(core.WorkspaceRoleMaintainer)
			accept := func(key string, extra map[string]any, wantSpec, wantMerge bool) core.Task {
				t.Helper()
				result := f.callMCP(t, token, "create_task", intakeArgs(key, extra))
				if result.errText != "" || !result.created || result.task.SpecApproval != wantSpec || result.task.MergeApproval != wantMerge {
					t.Fatalf("%s: err=%q task=%+v", key, result.errText, result.task)
				}
				return result.task
			}
			accept("omitted", nil, defaults[0], defaults[1])
			accept("spec-on", map[string]any{"spec_approval": true}, true, defaults[1])
			accept("merge-on", map[string]any{"merge_approval": true}, defaults[0], true)
			existing := accept("both-on", map[string]any{"spec_approval": true, "merge_approval": true}, true, true)

			refuse := func(label, key string, extra map[string]any, code, field string) {
				t.Helper()
				tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
				result := f.callMCP(t, token, "create_task", intakeArgs(key, extra))
				if !strings.HasPrefix(result.errText, code+": ") || !strings.Contains(result.errText, field) {
					t.Fatalf("%s: err=%q", label, result.errText)
				}
				f.requireNoSideEffects(t, label, tasks, titles, notified)
			}
			for _, gate := range []string{"spec_approval", "merge_approval"} {
				refuse(gate+" false", gate+"-off", map[string]any{gate: false}, agentGateDisableForbidden, gate)
				// The refusal precedes the idempotency lookup, so an existing
				// key never turns a gate-off attempt into a retry.
				refuse(gate+" false on existing key", "both-on", map[string]any{gate: false}, agentGateDisableForbidden, gate)
				for name, value := range map[string]any{"null": nil, "string": "true", "number": 1, "object": map[string]any{"on": true}} {
					refuse(gate+" "+name, gate+"-"+name, map[string]any{gate: value}, agentGateOverrideInvalid, gate)
				}
			}
			// An exact retry of an accepted request still returns its task.
			if retry := f.callMCP(t, token, "create_task", intakeArgs("both-on", map[string]any{"spec_approval": true, "merge_approval": true})); retry.errText != "" || retry.created || retry.task.ID != existing.ID {
				t.Fatalf("retry=%+v", retry)
			}
		})
	}
}

func TestMCPAgentLegacyPolicyOverrides(t *testing.T) {
	f := newAgentIntakeFixture(t)
	f.setDefaults(false, false)
	token := agentIntakeToken(core.WorkspaceRoleMaintainer)
	refuse := func(label, key string, extra map[string]any, code, field string) {
		t.Helper()
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", intakeArgs(key, extra))
		if !strings.HasPrefix(result.errText, code+": ") || !strings.Contains(result.errText, field) {
			t.Fatalf("%s: err=%q", label, result.errText)
		}
		f.requireNoSideEffects(t, label, tasks, titles, notified)
	}
	for _, level := range []string{"L0", "L1"} {
		refuse(level, "level-"+level, map[string]any{"level": level}, agentGateDisableForbidden, "level "+level)
		refuse(level+" with gates on", "level-on-"+level, map[string]any{"level": level, "spec_approval": true, "merge_approval": true}, agentGateDisableForbidden, "level "+level)
	}
	for name, value := range map[string]any{"unknown": "L9", "lowercase": "l2", "padded": " L2", "number": 2, "null": nil} {
		refuse("level "+name, "level-"+name, map[string]any{"level": value}, agentGateOverrideInvalid, "level")
	}
	for _, level := range []string{"L2", "L3"} {
		refuse(level+" with gate off", "level-off-"+level, map[string]any{"level": level, "merge_approval": false}, agentGateDisableForbidden, "merge_approval")
		result := f.callMCP(t, token, "create_task", intakeArgs("level-ok-"+level, map[string]any{"level": level}))
		if result.errText != "" || !result.task.SpecApproval || !result.task.MergeApproval || result.task.Hold != (level == "L3") {
			t.Fatalf("%s: err=%q task=%+v", level, result.errText, result.task)
		}
	}
	for _, hold := range []bool{true, false} {
		result := f.callMCP(t, token, "create_task", intakeArgs(fmt.Sprintf("hold-%t", hold), map[string]any{"hold": hold}))
		if result.errText != "" || result.task.Hold != hold || result.task.SpecApproval || result.task.MergeApproval {
			t.Fatalf("hold %t: err=%q task=%+v", hold, result.errText, result.task)
		}
		requireAgentProvenance(t, f, result.task.ID, intakeAgentID(core.WorkspaceRoleMaintainer), intakeOwner(core.WorkspaceRoleMaintainer))
	}
	for _, field := range []string{"setup", "setup_contract", "policy_contract", "execution_settings", "routing", "harness", "model", "effort", "argv"} {
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", intakeArgs("retired-"+field, map[string]any{field: "x"}))
		if !strings.Contains(result.errText, field+" is retired execution detail") {
			t.Fatalf("%s: err=%q", field, result.errText)
		}
		f.requireNoSideEffects(t, field, tasks, titles, notified)
	}
}

func TestMCPAgentCreationPreservesIntakeValidation(t *testing.T) {
	f := newAgentIntakeFixture(t)
	token := agentIntakeToken(core.WorkspaceRoleMaintainer)
	refuse := func(label string, args map[string]any, want string) {
		t.Helper()
		tasks, notified := f.taskCount(t), f.notified.Load()
		result := f.callMCP(t, token, "create_task", args)
		if !strings.Contains(result.errText, want) {
			t.Fatalf("%s: err=%q want %q", label, result.errText, want)
		}
		if f.taskCount(t) != tasks || f.notified.Load() != notified {
			t.Fatalf("%s wrote a task or notified triage", label)
		}
	}
	missingKey := intakeArgs("", nil)
	delete(missingKey, "idempotency_key")
	refuse("missing key", missingKey, "idempotency_key is required")
	refuse("blank key", intakeArgs("   ", nil), "idempotency_key is required")
	refuse("201-character key", intakeArgs(strings.Repeat("k", 201), nil), "idempotency_key must be at most 200 characters")
	if result := f.callMCP(t, token, "create_task", intakeArgs(strings.Repeat("k", 200), nil)); result.errText != "" || !result.created {
		t.Fatalf("200-character key: %q", result.errText)
	}
	refuse("supplied title", intakeArgs("titled", map[string]any{"title": "Mine"}), "title is generated and must not be supplied")
	refuse("blank body", intakeArgs("blank-body", map[string]any{"body": "  "}), "body is required")
	refuse("unknown repository", intakeArgs("unknown-repo", map[string]any{"repo": "nope"}), "unknown repo nope")
	titles := f.titles.Load()
	refuse("invalid dependency", intakeArgs("bad-dependency", map[string]any{"depends_on": []any{"missing-task"}}), "invalid_dependencies")
	if f.titles.Load() != titles {
		t.Fatal("dependency validation did not precede title generation")
	}
	// Context references are validated inside the creation transaction, so
	// the refusal leaves no task, event, or triage notification.
	refuse("invalid context", intakeArgs("bad-context", map[string]any{"requirement_ids": []any{"req-missing"}}), "invalid_context_reference")

	exact := strings.Repeat("T", 200)
	for label, hook := range map[string]struct {
		title string
		err   error
		want  string
	}{
		"title failure": {"", errors.New("model down"), "generate task title: model down"},
		"empty title":   {"   ", nil, "AI returned an invalid title"},
		"201 title":     {exact + "T", nil, "AI returned an invalid title"},
	} {
		f.titleHook = func(context.Context, core.Task) (string, error) { return hook.title, hook.err }
		refuse(label, intakeArgs("title-"+strings.ReplaceAll(label, " ", "-"), nil), hook.want)
	}
	f.titleHook = func(context.Context, core.Task) (string, error) { return exact, nil }
	result := f.callMCP(t, token, "create_task", intakeArgs("exact-title", map[string]any{"repo": "ui"}))
	if result.errText != "" || result.task.Title != exact || result.task.BaseBranch != "trunk" || result.task.Body != "Agent filed **work**\n\n- keep verbatim" || result.task.Source != "mcp" {
		t.Fatalf("exact title: err=%q task=%+v", result.errText, result.task)
	}
}

func TestMCPAgentCreateTaskRetryPreservesProvenance(t *testing.T) {
	f := newAgentIntakeFixture(t)
	first := f.callMCP(t, agentIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("retry-key", map[string]any{"hold": true}))
	if first.errText != "" || !first.created {
		t.Fatalf("first=%+v", first)
	}
	titles, notified := f.titles.Load(), f.notified.Load()
	retry := func(label, token string) {
		t.Helper()
		result := f.callMCP(t, token, "create_task", intakeArgs("retry-key", map[string]any{"hold": true}))
		if result.errText != "" || result.created || result.task.ID != first.task.ID || result.task.Title != first.task.Title ||
			result.task.SpecApproval != first.task.SpecApproval || result.task.MergeApproval != first.task.MergeApproval || !result.task.Hold {
			t.Fatalf("%s: result=%+v first=%+v", label, result, first.task)
		}
		if f.titles.Load() != titles || f.notified.Load() != notified || f.taskCount(t) != 1 {
			t.Fatalf("%s regenerated or re-notified", label)
		}
		requireAgentProvenance(t, f, first.task.ID, intakeAgentID(core.WorkspaceRoleMaintainer), intakeOwner(core.WorkspaceRoleMaintainer))
	}
	retry("exact retry", agentIntakeToken(core.WorkspaceRoleMaintainer))
	f.setDefaults(false, false)
	retry("retry after default change", agentIntakeToken(core.WorkspaceRoleMaintainer))
	f.setConfigError(errors.New("configuration unavailable"))
	retry("retry with configuration unavailable", agentIntakeToken(core.WorkspaceRoleMaintainer))
	f.setConfigError(nil)
	// Another credential of the same owner retrying the key gets the original
	// task; the recorded provenance still names the first agent.
	retry("retry by sibling agent", "agent-maintainer-second")
	if result := f.callMCP(t, agentIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("retry-key", map[string]any{"body": "different"})); !strings.Contains(result.errText, "idempotency_key is already used by a different task") {
		t.Fatalf("changed input err=%q", result.errText)
	}
}

func TestMCPAgentCreateTaskConcurrentRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sameBody bool
	}{{"identical input", true}, {"differing input", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentIntakeFixture(t)
			// Both requests pass the idempotency lookup, then meet at the title
			// barrier, so both reach the store insert and race on the key.
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			f.titleHook = func(ctx context.Context, _ core.Task) (string, error) {
				arrived <- struct{}{}
				select {
				case <-release:
					return "Raced title", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			tokens := []string{agentIntakeToken(core.WorkspaceRoleMaintainer), "agent-maintainer-second"}
			bodies := []string{"Concurrent body", "Concurrent body"}
			if !tc.sameBody {
				bodies[1] = "Other concurrent body"
			}
			results := make([]mcpCallResult, 2)
			var wg sync.WaitGroup
			for i := range tokens {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = f.callMCP(t, tokens[i], "create_task", intakeArgs("raced-key", map[string]any{"body": bodies[i]}))
				}()
			}
			for range tokens {
				select {
				case <-arrived:
				case <-t.Context().Done():
					t.Fatal("requests never reached the title barrier")
				}
			}
			close(release)
			wg.Wait()

			if f.taskCount(t) != 1 || f.notified.Load() != 1 {
				t.Fatalf("tasks=%d notified=%d results=%+v", f.taskCount(t), f.notified.Load(), results)
			}
			winner := slices.IndexFunc(results, func(r mcpCallResult) bool { return r.created })
			if winner < 0 {
				t.Fatalf("no winner: %+v", results)
			}
			loser := results[1-winner]
			if tc.sameBody {
				if loser.errText != "" || loser.created || loser.task.ID != results[winner].task.ID {
					t.Fatalf("loser=%+v winner=%+v", loser, results[winner])
				}
			} else if !strings.Contains(loser.errText, "idempotency_key is already used by a different task") {
				t.Fatalf("differing loser err=%q", loser.errText)
			}
			winnerAgent := intakeAgentID(core.WorkspaceRoleMaintainer)
			if winner == 1 {
				winnerAgent = "agt_maintainer_second"
			}
			requireAgentProvenance(t, f, results[winner].task.ID, winnerAgent, intakeOwner(core.WorkspaceRoleMaintainer))
		})
	}
}

func TestMCPRunChildCreateTaskStaysConfined(t *testing.T) {
	f := newAgentIntakeFixture(t)
	created := f.callMCP(t, "run-child", "create_task", intakeArgs("run-child-key", nil))
	if created.errText != "" || !created.created {
		t.Fatalf("run child create: %q", created.errText)
	}
	requireAgentProvenance(t, f, created.task.ID, "agt_run_child", intakeOwner(core.WorkspaceRoleMaintainer))
	foreign := intakeArgs("run-child-foreign", nil)
	foreign["workspace_id"] = "beta"
	if result := f.callMCP(t, "run-child", "create_task", foreign); !strings.Contains(result.errText, "workspace_not_found") {
		t.Fatalf("foreign workspace err=%q", result.errText)
	}
	if result := f.callMCP(t, "run-child", "claim_work_order", map[string]any{"workspace_id": "alpha", "work_order_id": created.task.ID + "-spec-1", "session_id": "bound-session", "client_token": "token", "agent": "test", "model": "test"}); !strings.Contains(result.errText, "claim_work_order is unavailable to a session-bound run child credential") {
		t.Fatalf("claim err=%q", result.errText)
	}
	for _, binding := range []struct{ order, session string }{
		{created.task.ID + "-spec-1", "bound-session"},
		{"bound-order", "other-session"},
	} {
		result := f.callMCP(t, "run-child", "report_progress", map[string]any{"workspace_id": "alpha", "work_order_id": binding.order, "session_id": binding.session, "message": "not mine"})
		if !strings.Contains(result.errText, store.ErrWorkOrderClaimUnauthorized.Error()) {
			t.Fatalf("%+v err=%q", binding, result.errText)
		}
	}
}

func TestMCPAgentTaskCreationGrantsNoAdditionalCapabilities(t *testing.T) {
	f := newAgentIntakeFixture(t)
	token := agentIntakeToken(core.WorkspaceRoleOperator)
	created := f.callMCP(t, token, "create_task", intakeArgs("operator-agent", nil))
	if created.errText != "" {
		t.Fatalf("create: %q", created.errText)
	}
	taskID := created.task.ID
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		if !humanReservedMCPTool(name) {
			continue
		}
		result := f.callMCP(t, token, name, map[string]any{"workspace_id": "alpha", "task_id": taskID, "depends_on_task_id": taskID, "assignee_user_id": intakeOwner(core.WorkspaceRoleOperator), "branch": "feature/x", "work_order_id": taskID + "-spec-1"})
		if !strings.Contains(result.errText, "requires an operator-scoped user credential") {
			t.Fatalf("%s err=%q", name, result.errText)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/tasks?workspace_id=alpha", `{"body":"rest intake","repo":"api"}`},
		{http.MethodPut, "/v1/tasks/" + taskID + "/hold?workspace_id=alpha", `{"hold":false}`},
		{http.MethodPut, "/v1/tasks/" + taskID + "/assignee?workspace_id=alpha", `{"assignee_user_id":"usr_operator"}`},
		{http.MethodPost, "/v1/tasks/" + taskID + "/review?workspace_id=alpha", `{"action":"approve"}`},
		{http.MethodPost, "/v1/tasks/" + taskID + "/policy?workspace_id=alpha", `{"spec_approval":false}`},
		{http.MethodPost, "/v1/tasks/" + taskID + "/close?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/tasks/" + taskID + "/dependencies?workspace_id=alpha", `{}`},
		{http.MethodPost, "/v1/reference-documents?workspace_id=alpha", ``},
		{http.MethodPost, "/v1/system-designs/component-x/versions/1/confirm?workspace_id=alpha", ``},
		{http.MethodPost, "/v1/workspaces/alpha/members", `{"email":"x@example.test","role":"operator"}`},
	} {
		request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d body=%s", route.method, route.path, response.Code, response.Body.String())
		}
	}
	if f.taskCount(t) != 1 {
		t.Fatalf("tasks=%d", f.taskCount(t))
	}
	persisted, err := f.store.GetTask(store.WithWorkspace(t.Context(), "alpha"), taskID)
	if err != nil || persisted.Hold || persisted.Assignee != nil || !persisted.SpecApproval {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	// A worker credential still cannot create tasks.
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker", Workspace: "alpha"}))
	if _, err := f.server.callMCPTool(request, "create_task", intakeArgs("worker", nil)); err == nil || !strings.Contains(err.Error(), "worker credentials cannot create tasks") {
		t.Fatalf("worker err=%v", err)
	}
}

// TestCreateTaskRequestPolicyFieldsAreAgentChecked pins every field of the
// shared intake request. Adding a field fails here until it is classified; a
// pipeline-policy field must also be enforced by agentIntakePolicyError
// (DEC-60).
func TestCreateTaskRequestPolicyFieldsAreAgentChecked(t *testing.T) {
	classified := map[string]string{
		"repositoryInstallAttempt": "server-only registration intake",
		"attachments":              "multipart REST only",
		"multipartIntake":          "multipart REST only",
		"Body":                     "content",
		"Repo":                     "routing to a configured repository",
		"BaseBranch":               "branch",
		"Source":                   "intake label, never provenance",
		"Level":                    "policy: agent-checked",
		"Hold":                     "reservation, not a gate (DEC-55(3))",
		"SpecApproval":             "policy: agent-checked",
		"MergeApproval":            "policy: agent-checked",
		"Setup":                    "retired; refused by MCP and REST decoders",
		"DependsOn":                "ordering",
		"RequirementIDs":           "context",
		"SystemDesignIDs":          "context",
	}
	fields := reflect.TypeFor[createTaskReq]()
	seen := map[string]bool{}
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		seen[name] = true
		if _, ok := classified[name]; !ok {
			t.Fatalf("createTaskReq field %s is unclassified; decide whether agents may set it and enforce the gate-on-only rule", name)
		}
	}
	for name := range classified {
		if !seen[name] {
			t.Fatalf("classified field %s no longer exists", name)
		}
	}
	off, on := false, true
	for name, req := range map[string]createTaskReq{
		"spec off":  {SpecApproval: &off},
		"merge off": {MergeApproval: &off},
		"L0":        {Level: core.L0},
		"L1":        {Level: core.L1},
		"L2 + off":  {Level: core.L2, SpecApproval: &off},
	} {
		var typed *taskCreateError
		if err := agentIntakePolicyError(req); !errors.As(err, &typed) || typed.Code != agentGateDisableForbidden {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	for name, req := range map[string]createTaskReq{
		"omitted":   {},
		"both on":   {SpecApproval: &on, MergeApproval: &on},
		"L2":        {Level: core.L2},
		"L3":        {Level: core.L3},
		"hold only": {Hold: true},
	} {
		if err := agentIntakePolicyError(req); err != nil {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	var typed *taskCreateError
	if err := agentIntakePolicyError(createTaskReq{Level: "L4"}); !errors.As(err, &typed) || typed.Code != agentGateOverrideInvalid {
		t.Fatalf("L4 err=%v", err)
	}
}
