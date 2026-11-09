package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime/multipart"
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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
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
func (l *lockedMemberships) AuthorizeWorkspaceCapabilities(ctx context.Context, userID, workspaceID string, capabilities []core.Capability) (map[core.Capability]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.AuthorizeWorkspaceCapabilities(ctx, userID, workspaceID, capabilities)
}
func (l *lockedMemberships) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.AuthorizeDeployment(ctx, userID, capability)
}
func (l *lockedMemberships) AuthorizeInstanceAdministration(ctx context.Context, userID string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.AuthorizeInstanceAdministration(ctx, userID)
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

// sessionIntakeFixture serves full /mcp and REST requests through credential
// verification, workspace resolution, and live membership authorization.
type sessionIntakeFixture struct {
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

var sessionIntakeRoles = []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator}

func humanIntakeToken(role core.WorkspaceRole) string { return "human-" + string(role) }
func intakeOwner(role core.WorkspaceRole) string      { return "usr_" + string(role) }

func newSessionIntakeFixture(t *testing.T) *sessionIntakeFixture {
	t.Helper()
	members := &lockedMemberships{inner: &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}, {ID: "beta"}},
		roles:      map[string]map[string]core.WorkspaceRole{},
	}}
	credentials := staticCredentialVerifier{}
	for _, role := range sessionIntakeRoles {
		members.setRole(intakeOwner(role), "alpha", role)
		credentials[humanIntakeToken(role)] = core.AuthenticatedCredential{ID: "pat_" + string(role), OwnerUserID: intakeOwner(role), Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	}
	// A second token of the maintainer owner, a member of another workspace,
	// and a caller with no binding.
	credentials["pat-maintainer-second"] = core.AuthenticatedCredential{ID: "pat_maintainer_second", OwnerUserID: intakeOwner(core.WorkspaceRoleMaintainer), Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "beta", core.WorkspaceRoleMaintainer)
	members.setRole("usr_foreign", "beta", core.WorkspaceRoleOperator)
	credentials["pat-foreign"] = core.AuthenticatedCredential{ID: "pat_foreign", OwnerUserID: "usr_foreign", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	credentials["pat-unbound"] = core.AuthenticatedCredential{ID: "pat_unbound", OwnerUserID: "usr_unbound", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}

	f := &sessionIntakeFixture{store: store.NewMemory(), members: members, cfg: &config.Config{
		Workspace: "alpha",
		Execution: config.ExecutionPolicy{DefaultPlanState: "needed", MergeApproval: true},
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

func (f *sessionIntakeFixture) setDefaults(spec, merge bool) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	copied := *f.cfg
	copied.Execution.DefaultPlanState, copied.Execution.MergeApproval = "not_required", merge
	if spec {
		copied.Execution.DefaultPlanState = "needed"
	}
	f.cfg = &copied
}

func (f *sessionIntakeFixture) setConfigError(err error) {
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
func (f *sessionIntakeFixture) callMCP(t *testing.T, token, tool string, args map[string]any) mcpCallResult {
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

func (f *sessionIntakeFixture) taskCount(t *testing.T) int {
	t.Helper()
	tasks, err := f.store.ListTasks(store.WithWorkspace(t.Context(), "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func (f *sessionIntakeFixture) createdEvents(t *testing.T, taskID string) []core.Event {
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

// allCreatedEvents counts task.created events across every task in the
// workspace, so a refusal that left an orphaned creation event is visible.
func (f *sessionIntakeFixture) allCreatedEvents(t *testing.T) int {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "alpha")
	tasks, err := f.store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, task := range tasks {
		count += len(f.createdEvents(t, task.ID))
	}
	return count
}

// requireNoSideEffects asserts a refused call reached no title generation,
// task write, creation event, or triage notification.
func (f *sessionIntakeFixture) requireNoSideEffects(t *testing.T, label string, tasks int, titles, notified int32) {
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

// requirePersonCreator proves the task a session filed names its person and
// records no agent provenance (component-task-lifecycle invariant 21; DEC-116(5)).
func requirePersonCreator(t *testing.T, f *sessionIntakeFixture, taskID, ownerID string) {
	t.Helper()
	created := f.createdEvents(t, taskID)
	if len(created) != 1 {
		t.Fatalf("task.created events=%d", len(created))
	}
	event := created[0]
	if event.ActorID != store.UserActorID(ownerID) || event.ActorRole != core.ActorUser {
		t.Fatalf("actor=%q role=%q", event.ActorID, event.ActorRole)
	}
	if provenance, ok, err := store.TaskCreatedProvenance(event.Payload); err != nil || ok {
		t.Fatalf("provenance=%+v ok=%t err=%v payload=%s", provenance, ok, err, event.Payload)
	}
}

func sessionCreatesTask(t *testing.T, role core.WorkspaceRole) {
	t.Helper()
	f := newSessionIntakeFixture(t)
	result := f.callMCP(t, humanIntakeToken(role), "create_task", intakeArgs("session-"+string(role), map[string]any{"source": "mcp:issue-7"}))
	if result.errText != "" || !result.created {
		t.Fatalf("create err=%q created=%t", result.errText, result.created)
	}
	task := result.task
	if task.Workspace != "alpha" || task.Title != "Generated title" || task.Body != "Agent filed **work**\n\n- keep verbatim" || task.Source != "mcp:issue-7" ||
		task.PlanState != core.PlanNeeded || !task.MergeApproval || task.State != core.TaskQueued || task.NextStage != core.StageTriage || task.Branch != "conveyor/task-"+task.ID {
		t.Fatalf("task=%+v", task)
	}
	requirePersonCreator(t, f, task.ID, intakeOwner(role))
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

func TestMCPPersonMaintainerCreatesTask(t *testing.T) {
	sessionCreatesTask(t, core.WorkspaceRoleMaintainer)
}

func TestMCPPersonOperatorCreatesTask(t *testing.T) {
	sessionCreatesTask(t, core.WorkspaceRoleOperator)
}

func TestMCPPersonWithoutCreateTasksCannotCreateTask(t *testing.T) {
	f := newSessionIntakeFixture(t)
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
		refuse(string(role), humanIntakeToken(role), intakeArgs("lower-"+string(role), nil))
		// Turning a gate off through intake opens no route around the capability.
		refuse(string(role)+" with merge_approval false", humanIntakeToken(role), intakeArgs("lower-off-"+string(role), map[string]any{"merge_approval": false, "spec_approval": false}))
	}
	refuse("absent binding", "pat-unbound", intakeArgs("unbound", nil))
	refuse("foreign workspace", "pat-foreign", intakeArgs("foreign", nil))

	// A binding revoked after a successful creation stops the next one.
	if result := f.callMCP(t, humanIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("before-revocation", nil)); result.errText != "" {
		t.Fatalf("before revocation: %q", result.errText)
	}
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", "")
	refuse("revoked binding", humanIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("after-revocation", nil))
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", core.WorkspaceRoleContributor)
	refuse("demoted binding", humanIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("after-demotion", nil))

	// Without a membership provider authorization fails closed.
	f.members.setRole(intakeOwner(core.WorkspaceRoleMaintainer), "alpha", core.WorkspaceRoleMaintainer)
	f.server.Memberships = nil
	refuse("membership provider unavailable", humanIntakeToken(core.WorkspaceRoleMaintainer), intakeArgs("no-provider", nil))
}

// TestMCPPersonCreateTaskGateValuesApply proves over MCP that a gate value a
// person supplies applies under every workspace default, and that an omitted
// gate takes the default (req-intake-and-triage AC-4.2; DEC-116(5);
// component-task-lifecycle invariants 16 and 18). The task still enters triage,
// names its person, and grants the session nothing (AC-4.3).
func TestMCPPersonCreateTaskGateValuesApply(t *testing.T) {
	for _, defaults := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		t.Run(fmt.Sprintf("defaults spec=%t merge=%t", defaults[0], defaults[1]), func(t *testing.T) {
			f := newSessionIntakeFixture(t)
			f.setDefaults(defaults[0], defaults[1])
			owner, token := intakeOwner(core.WorkspaceRoleMaintainer), humanIntakeToken(core.WorkspaceRoleMaintainer)
			for _, test := range []struct {
				key                 string
				extra               map[string]any
				wantSpec, wantMerge bool
			}{
				{"omitted", nil, defaults[0], defaults[1]},
				{"merge-off", map[string]any{"merge_approval": false}, defaults[0], false},
				{"merge-on", map[string]any{"merge_approval": true}, defaults[0], true},
				{"spec-off", map[string]any{"spec_approval": false}, false, defaults[1]},
				{"spec-on", map[string]any{"spec_approval": true}, true, defaults[1]},
				{"both-off", map[string]any{"spec_approval": false, "merge_approval": false}, false, false},
				{"both-on", map[string]any{"spec_approval": true, "merge_approval": true}, true, true},
			} {
				result := f.callMCP(t, token, "create_task", intakeArgs(test.key, test.extra))
				wantPlanState := core.PlanNotRequired
				if test.wantSpec {
					wantPlanState = core.PlanNeeded
				}
				if result.errText != "" || !result.created || result.task.PlanState != wantPlanState || result.task.MergeApproval != test.wantMerge {
					t.Fatalf("%s: err=%q task=%+v", test.key, result.errText, result.task)
				}
				persisted, err := f.store.GetTask(store.WithWorkspace(t.Context(), "alpha"), result.task.ID)
				if err != nil || persisted.SpecApproval != test.wantSpec || persisted.MergeApproval != test.wantMerge {
					t.Fatalf("%s: persisted=%+v err=%v", test.key, persisted, err)
				}
				if persisted.State != core.TaskQueued || persisted.NextStage != core.StageTriage || persisted.Assignee != nil {
					t.Fatalf("%s: did not enter triage like any other task: %+v", test.key, persisted)
				}
				if persisted.Source != "mcp" {
					t.Fatalf("%s: source=%q, want mcp", test.key, persisted.Source)
				}
				requirePersonCreator(t, f, persisted.ID, owner)
				// A retry repeating the request returns the task; one changing
				// the gate is a different request, not a silent override.
				if retry := f.callMCP(t, token, "create_task", intakeArgs(test.key, test.extra)); retry.errText != "" || retry.created || retry.task.ID != persisted.ID {
					t.Fatalf("%s: retry=%+v", test.key, retry)
				}
				if test.key != "omitted" {
					flipped := map[string]any{}
					for gate, value := range test.extra {
						flipped[gate] = !value.(bool)
					}
					if changed := f.callMCP(t, token, "create_task", intakeArgs(test.key, flipped)); !strings.Contains(changed.errText, "idempotency_key is already used by a different task") {
						t.Fatalf("%s: changed gate retry err=%q", test.key, changed.errText)
					}
				}
			}
		})
	}
}

// TestMCPCreateTaskMistypedGateRefuses proves a mistyped gate refuses for every
// caller who may file a task, on both gates, with no side effect and never by
// taking the workspace default, even when its key names a task
// (component-task-lifecycle invariant 16; req-intake-and-triage AC-4.2).
func TestMCPCreateTaskMistypedGateRefuses(t *testing.T) {
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		for _, defaults := range [][2]bool{{true, true}, {false, false}} {
			t.Run(fmt.Sprintf("%s defaults %t", role, defaults[0]), func(t *testing.T) {
				f := newSessionIntakeFixture(t)
				f.setDefaults(defaults[0], defaults[1])
				token := humanIntakeToken(role)
				existing := f.callMCP(t, token, "create_task", intakeArgs("existing", nil))
				if existing.errText != "" || !existing.created {
					t.Fatalf("existing=%+v", existing)
				}
				values := map[string]any{"string false": "false", "string true": "true", "null": nil, "empty string": "", "zero": 0, "one": 1, "object": map[string]any{"on": true}, "array": []any{false}}
				for _, gate := range []string{"spec_approval", "merge_approval"} {
					for name, value := range values {
						for _, key := range []string{gate + "-" + name, "existing"} {
							label := gate + " " + name + " key " + key
							tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
							result := f.callMCP(t, token, "create_task", intakeArgs(key, map[string]any{gate: value}))
							if !strings.HasPrefix(result.errText, "invalid_agent_gate_override: ") || !strings.Contains(result.errText, gate) {
								t.Fatalf("%s: err=%q", label, result.errText)
							}
							if strings.Contains(result.errText, "false") || result.created || result.task.ID != "" {
								t.Fatalf("%s: echoed caller text or returned a task: %+v", label, result)
							}
							f.requireNoSideEffects(t, label, tasks, titles, notified)
						}
					}
				}
				if retry := f.callMCP(t, token, "create_task", intakeArgs("existing", nil)); retry.errText != "" || retry.created || retry.task.ID != existing.task.ID {
					t.Fatalf("retry=%+v", retry)
				}
			})
		}
	}
}

// TestRESTCreateTaskMistypedGateRefuses proves the same refusal on the REST
// doors, which decode a gate into a typed field: "merge_approval":"false" never
// takes the workspace default there either.
func TestRESTCreateTaskMistypedGateRefuses(t *testing.T) {
	f := newSessionIntakeFixture(t)
	f.setDefaults(true, true)
	token := humanIntakeToken(core.WorkspaceRoleMaintainer)
	for _, gate := range []string{"spec_approval", "merge_approval"} {
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		body := fmt.Sprintf(`{"body":"rest intake","repo":"api",%q:"false"}`, gate)
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=alpha", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("JSON %s status=%d body=%s", gate, response.Code, response.Body.String())
		}
		f.requireNoSideEffects(t, "JSON "+gate, tasks, titles, notified)

		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		if err := writer.WriteField("task", body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request = httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=alpha", &form)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response = httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("multipart %s status=%d body=%s", gate, response.Code, response.Body.String())
		}
		f.requireNoSideEffects(t, "multipart "+gate, tasks, titles, notified)
	}
}

// TestMCPCreateTaskLegacyPolicyInputs proves a legacy level sets the plan
// state and the merge approval gate, an explicit value wins last, and anything
// else refuses before any lookup (component-task-lifecycle invariants 16 and
// 18; DEC-122(1)).
func TestMCPCreateTaskLegacyPolicyInputs(t *testing.T) {
	f := newSessionIntakeFixture(t)
	f.setDefaults(false, false)
	token := humanIntakeToken(core.WorkspaceRoleMaintainer)
	refuse := func(label, key string, extra map[string]any, code, field string) {
		t.Helper()
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", intakeArgs(key, extra))
		if !strings.HasPrefix(result.errText, code+": ") || !strings.Contains(result.errText, field) {
			t.Fatalf("%s: err=%q", label, result.errText)
		}
		f.requireNoSideEffects(t, label, tasks, titles, notified)
	}
	for _, test := range []struct {
		level               string
		extra               map[string]any
		wantSpec, wantMerge bool
	}{
		{"L0", nil, false, false},
		{"L1", nil, false, true},
		{"L2", nil, true, true},
		// L3 maps both gates on exactly like L2 and implies no reservation
		// (DEC-122(3)).
		{"L3", nil, true, true},
		{"L0", map[string]any{"spec_approval": true, "merge_approval": true}, true, true},
		{"L2", map[string]any{"merge_approval": false}, true, false},
	} {
		key := fmt.Sprintf("level-%s-%t-%t", test.level, test.wantSpec, test.wantMerge)
		extra := map[string]any{"level": test.level}
		maps.Copy(extra, test.extra)
		result := f.callMCP(t, token, "create_task", intakeArgs(key, extra))
		wantPlanState := core.PlanNotRequired
		if test.wantSpec {
			wantPlanState = core.PlanNeeded
		}
		if result.errText != "" || result.task.PlanState != wantPlanState || result.task.MergeApproval != test.wantMerge {
			t.Fatalf("%s: err=%q task=%+v", key, result.errText, result.task)
		}
	}
	first := f.callMCP(t, token, "create_task", intakeArgs("level-existing", map[string]any{"level": "L2"}))
	if first.errText != "" {
		t.Fatalf("level-existing: %q", first.errText)
	}
	for name, value := range map[string]any{"unknown": "L9", "lowercase": "l2", "padded": " L2", "number": 2, "null": nil} {
		refuse("level "+name, "level-"+name, map[string]any{"level": value}, invalidGateOverride, "level")
		// The refusal precedes the idempotency lookup.
		refuse("level "+name+" on existing key", "level-existing", map[string]any{"level": value}, invalidGateOverride, "level")
	}
	// Hold is retired for every caller (DEC-122(3)): a new true is refused
	// with no side effect, and false is accepted with the workspace defaults.
	refuse("hold true", "hold-true", map[string]any{"hold": true}, taskHoldRetired, "hold is retired (DEC-122)")
	result := f.callMCP(t, token, "create_task", intakeArgs("hold-false", map[string]any{"hold": false}))
	if result.errText != "" || result.task.SpecApproval || result.task.MergeApproval {
		t.Fatalf("hold false: err=%q task=%+v", result.errText, result.task)
	}
	requirePersonCreator(t, f, result.task.ID, intakeOwner(core.WorkspaceRoleMaintainer))
	for _, field := range []string{"setup", "setup_contract", "policy_contract", "execution_settings", "routing", "harness", "model", "effort", "argv"} {
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", intakeArgs("retired-"+field, map[string]any{field: "x"}))
		if !strings.Contains(result.errText, field+" is retired execution detail") {
			t.Fatalf("%s: err=%q", field, result.errText)
		}
		f.requireNoSideEffects(t, field, tasks, titles, notified)
	}
}

func TestMCPCreateTaskPreservesIntakeValidation(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleMaintainer)
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

	// No title generator configured: MCP creation fails closed exactly as REST
	// intake does, before any task, creation event, or triage intent
	// exists (req-intake-and-triage AC-1.1; DEC-8).
	generator := f.server.GenerateTaskTitle
	f.server.GenerateTaskTitle = nil
	tasksBefore, eventsBefore, notifiedBefore := f.taskCount(t), f.allCreatedEvents(t), f.notified.Load()
	absent := f.callMCP(t, token, "create_task", intakeArgs("no-title-generator", nil))
	if absent.errText != "task title generation is unavailable" {
		t.Fatalf("absent title generator err=%q", absent.errText)
	}
	if f.taskCount(t) != tasksBefore || f.allCreatedEvents(t) != eventsBefore || f.notified.Load() != notifiedBefore {
		t.Fatalf("absent title generator wrote: tasks=%d/%d created events=%d/%d notified=%d/%d",
			f.taskCount(t), tasksBefore, f.allCreatedEvents(t), eventsBefore, f.notified.Load(), notifiedBefore)
	}
	if _, found, err := f.store.GetTaskByIntakeKey(store.WithWorkspace(t.Context(), "alpha"), "no-title-generator"); err != nil || found {
		t.Fatalf("absent title generator recorded the intake key found=%t err=%v", found, err)
	}
	f.server.GenerateTaskTitle = generator

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

func TestMCPCreateTaskRetryKeepsFirstCreator(t *testing.T) {
	f := newSessionIntakeFixture(t)
	first := f.callMCP(t, humanIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("retry-key", map[string]any{"spec_approval": true}))
	if first.errText != "" || !first.created {
		t.Fatalf("first=%+v", first)
	}
	titles, notified := f.titles.Load(), f.notified.Load()
	retry := func(label, token string) {
		t.Helper()
		result := f.callMCP(t, token, "create_task", intakeArgs("retry-key", map[string]any{"spec_approval": true}))
		if result.errText != "" || result.created || result.task.ID != first.task.ID || result.task.Title != first.task.Title ||
			result.task.SpecApproval != first.task.SpecApproval || result.task.MergeApproval != first.task.MergeApproval {
			t.Fatalf("%s: result=%+v first=%+v", label, result, first.task)
		}
		if f.titles.Load() != titles || f.notified.Load() != notified || f.taskCount(t) != 1 {
			t.Fatalf("%s regenerated or re-notified", label)
		}
		requirePersonCreator(t, f, first.task.ID, intakeOwner(core.WorkspaceRoleMaintainer))
	}
	retry("exact retry", humanIntakeToken(core.WorkspaceRoleMaintainer))
	f.setDefaults(false, false)
	retry("retry after default change", humanIntakeToken(core.WorkspaceRoleMaintainer))
	f.setConfigError(errors.New("configuration unavailable"))
	retry("retry with configuration unavailable", humanIntakeToken(core.WorkspaceRoleMaintainer))
	f.setConfigError(nil)
	// Another credential of the same owner retrying the key gets the original
	// task, which still names its person.
	retry("retry by another token of the person", "pat-maintainer-second")
	if result := f.callMCP(t, humanIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("retry-key", map[string]any{"body": "different"})); !strings.Contains(result.errText, "idempotency_key is already used by a different task") {
		t.Fatalf("changed input err=%q", result.errText)
	}
}

func TestMCPCreateTaskConcurrentRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sameBody bool
	}{{"identical input", true}, {"differing input", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionIntakeFixture(t)
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
			tokens := []string{humanIntakeToken(core.WorkspaceRoleMaintainer), "pat-maintainer-second"}
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
			requirePersonCreator(t, f, results[winner].task.ID, intakeOwner(core.WorkspaceRoleMaintainer))
		})
	}
}

// TestMCPSessionTaskCreationGrantsNoAdditionalCapabilities proves filing a task
// opens no route: the filer gains no right over the task beyond the person's
// role, the task stays in triage with no assignee, and a member without a
// capability is refused the reserved route (req-intake-and-triage AC-4.3;
// req-security-boundaries AC-1.1; DEC-116(5)).
func TestMCPSessionTaskCreationGrantsNoAdditionalCapabilities(t *testing.T) {
	f := newSessionIntakeFixture(t)
	f.setDefaults(true, true)
	created := f.callMCP(t, humanIntakeToken(core.WorkspaceRoleMaintainer), "create_task", intakeArgs("filed", map[string]any{"merge_approval": false, "spec_approval": false}))
	if created.errText != "" {
		t.Fatalf("create: %q", created.errText)
	}
	taskID := created.task.ID
	f.server.WorkOrders = &workorder.Service{Store: f.store}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor} {
		token := humanIntakeToken(role)
		for _, route := range []struct{ method, path, body string }{
			{http.MethodPost, "/v1/tasks?workspace_id=alpha", `{"body":"rest intake","repo":"api"}`},
			{http.MethodPut, "/v1/tasks/" + taskID + "/assignee?workspace_id=alpha", `{"assignee_user_id":"usr_operator"}`},
			{http.MethodPost, "/v1/tasks/" + taskID + "/review?workspace_id=alpha", `{"action":"approve"}`},
			{http.MethodPost, "/v1/tasks/" + taskID + "/policy?workspace_id=alpha", `{"spec_approval":true}`},
			{http.MethodPost, "/v1/tasks/" + taskID + "/close?workspace_id=alpha", `{}`},
			{http.MethodPost, "/v1/tasks/" + taskID + "/dependencies?workspace_id=alpha", `{}`},
			{http.MethodPost, "/v1/system-designs/component-x/versions/1/confirm?workspace_id=alpha", ``},
			{http.MethodPost, "/v1/workspaces/alpha/members", `{"email":"x@example.test","role":"operator"}`},
		} {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "workspace_not_found") {
				t.Fatalf("%s %s %s status=%d body=%s", role, route.method, route.path, response.Code, response.Body.String())
			}
		}
	}
	if f.taskCount(t) != 1 {
		t.Fatalf("tasks=%d", f.taskCount(t))
	}
	persisted, err := f.store.GetTask(store.WithWorkspace(t.Context(), "alpha"), taskID)
	if err != nil || persisted.Assignee != nil || persisted.SpecApproval || persisted.MergeApproval || persisted.State != core.TaskQueued || persisted.NextStage != core.StageTriage {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

// TestCreateTaskRequestPolicyFieldsAreClassified pins every field of the shared
// intake request. Adding a field fails here until it is classified; a pipeline
// policy field must also be type-checked by createTaskPolicyArgError
// (component-task-lifecycle invariant 16).
func TestCreateTaskRequestPolicyFieldsAreClassified(t *testing.T) {
	classified := map[string]string{
		"repositoryInstallAttempt": "server-only registration intake",
		"attachments":              "multipart REST only",
		"multipartIntake":          "multipart REST only",
		"Body":                     "content",
		"Repo":                     "routing to a configured repository",
		"BaseBranch":               "branch",
		"Source":                   "intake label, never provenance",
		"Level":                    "policy: type-checked",
		"Hold":                     "retired (DEC-122(3)); shared intake refuses true after the idempotency lookup",
		"SpecApproval":             "policy: type-checked",
		"plan":                     "policy: plan_state, plan, plan_base_commit, and plan_approved, type-checked before the idempotency lookup",
		"MergeApproval":            "policy: type-checked",
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
			t.Fatalf("createTaskReq field %s is unclassified; decide whether it is a policy input and type-check it", name)
		}
	}
	for name := range classified {
		if !seen[name] {
			t.Fatalf("classified field %s no longer exists", name)
		}
	}
	for name, args := range map[string]map[string]any{
		"spec string":  {"spec_approval": "false"},
		"merge string": {"merge_approval": "false"},
		"spec null":    {"spec_approval": nil},
		"merge null":   {"merge_approval": nil},
		"level number": {"level": 2},
		"level null":   {"level": nil},
		"L4":           {"level": "L4"},
	} {
		if err := createTaskPolicyArgError(args); err == nil || err.Code != invalidGateOverride {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	for name, args := range map[string]map[string]any{
		"omitted":   {},
		"spec off":  {"spec_approval": false},
		"merge off": {"merge_approval": false},
		"both on":   {"spec_approval": true, "merge_approval": true},
		"empty":     {"level": ""},
		"L0":        {"level": "L0"},
		"L1":        {"level": "L1"},
		"L2":        {"level": "L2"},
		"L3":        {"level": "L3"},
		"hold":      {"hold": true},
	} {
		if err := createTaskPolicyArgError(args); err != nil {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	// Hold passes the type check but cannot bypass the shared intake's
	// retirement rule, which every caller meets (DEC-122(3)).
	var typed *taskCreateError
	server := NewServer(store.NewMemory())
	server.Workspace, server.Repos = "demo", []string{"api"}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "unused", nil }
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if _, err := server.createTaskRecord(ctx, createTaskReq{Body: "hold only", Repo: "api", Hold: true}, "hold-only", "mcp"); !errors.As(err, &typed) || typed.Code != taskHoldRetired {
		t.Fatalf("hold only err=%v", err)
	}
}
