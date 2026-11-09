package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	intakeTestPlan   = "## Approach\nAdd the plan state.\n\n## Files touched\n- internal/store/plan_state.go\n\n## Ordering\n1. Store, then intake.\n\n## Risks\n- None.\n\n## Done criteria\n- Every task read carries plan_state."
	intakeTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

type restIntakeResult struct {
	status int
	body   string
	task   core.Task
	fields map[string]json.RawMessage
}

func (r restIntakeResult) code() string {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &payload)
	return payload.Error
}

// postTaskJSON posts one JSON task intake through the full REST handler.
func (f *sessionIntakeFixture) postTaskJSON(t *testing.T, token string, fields map[string]any) restIntakeResult {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=alpha", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	return f.serveIntake(t, request)
}

// postTaskMultipart posts one multipart task intake, with an optional
// idempotency key, through the full REST handler.
func (f *sessionIntakeFixture) postTaskMultipart(t *testing.T, token string, fields map[string]any, key string) restIntakeResult {
	t.Helper()
	task, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err = writer.WriteField("task", string(task)); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		if err = writer.WriteField("idempotency_key", key); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks?workspace_id=alpha", &form)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return f.serveIntake(t, request)
}

func (f *sessionIntakeFixture) serveIntake(t *testing.T, request *http.Request) restIntakeResult {
	t.Helper()
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	result := restIntakeResult{status: response.Code, body: response.Body.String()}
	if response.Code == http.StatusCreated || response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &result.task); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result.fields); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func (f *sessionIntakeFixture) getJSON(t *testing.T, token, path string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func intakeFields(extra map[string]any) map[string]any {
	fields := map[string]any{"body": "REST intake with a plan", "repo": "api"}
	for key, value := range extra {
		fields[key] = value
	}
	return fields
}

// TestRESTIntakePlanRefusalsPrecedeTheIdempotencyLookup proves each 400 of
// component-plan-state invariant 2 on JSON and multipart intake records
// nothing, and that a multipart refusal answers before its key reaches the
// idempotency lookup, even when the key names a task (component-task-lifecycle
// invariant 16; req-task-plan-state AC-2.2, AC-2.5, AC-2.6).
func TestRESTIntakePlanRefusalsPrecedeTheIdempotencyLookup(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	existing := f.postTaskMultipart(t, token, intakeFields(nil), "plan-refusal-key")
	if existing.status != http.StatusCreated {
		t.Fatalf("seed status=%d body=%s", existing.status, existing.body)
	}
	for _, refusal := range []struct {
		name  string
		extra map[string]any
	}{
		{"invalid plan", map[string]any{"plan": "## Approach\nOnly an approach."}},
		{"plan with a conveyor fence", map[string]any{"plan": intakeTestPlan + "\n\n```conveyor:spec\n{}\n```"}},
		{"plan_state with plan", map[string]any{"plan": intakeTestPlan, "plan_state": "needed"}},
		{"disagreeing spec_approval and plan_state", map[string]any{"spec_approval": true, "plan_state": "not_required"}},
		{"disagreeing spec_approval false and plan_state", map[string]any{"spec_approval": false, "plan_state": "needed"}},
		{"spec_approval false with plan", map[string]any{"spec_approval": false, "plan": intakeTestPlan}},
		{"plan_base_commit without plan", map[string]any{"plan_base_commit": intakeTestCommit}},
		{"short plan_base_commit", map[string]any{"plan": intakeTestPlan, "plan_base_commit": "0123456"}},
		{"uppercase plan_base_commit", map[string]any{"plan": intakeTestPlan, "plan_base_commit": strings.ToUpper(intakeTestCommit)}},
		{"plan_approved without plan", map[string]any{"plan_approved": true}},
		{"plan_approved not boolean", map[string]any{"plan": intakeTestPlan, "plan_approved": "yes"}},
		{"plan_approved null", map[string]any{"plan": intakeTestPlan, "plan_approved": nil}},
		{"plan_state not a string", map[string]any{"plan_state": 1}},
		{"plan_state awaiting_approval without plan", map[string]any{"plan_state": "awaiting_approval"}},
		{"plan_state completed without plan", map[string]any{"plan_state": "completed"}},
		{"plan not a string", map[string]any{"plan": true}},
		{"plan_state under two spellings", map[string]any{"plan_state": "needed", "PLAN_STATE": "not_required"}},
		{"plan under two spellings", map[string]any{"plan": intakeTestPlan, "Plan": intakeTestPlan}},
	} {
		for _, surface := range []string{"json", "multipart", "multipart retry"} {
			tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
			var result restIntakeResult
			switch surface {
			case "json":
				result = f.postTaskJSON(t, token, intakeFields(refusal.extra))
			case "multipart":
				result = f.postTaskMultipart(t, token, intakeFields(refusal.extra), "")
			default:
				result = f.postTaskMultipart(t, token, intakeFields(refusal.extra), "plan-refusal-key")
			}
			if result.status != http.StatusBadRequest || result.code() != invalidPlanIntake {
				t.Fatalf("%s (%s): status=%d body=%s", refusal.name, surface, result.status, result.body)
			}
			f.requireNoSideEffects(t, refusal.name+" ("+surface+")", tasks, titles, notified)
		}
	}
}

// TestRESTIntakePlanApprovalRequiresOperateGates proves plan_approved true is
// judged after admission from the live binding: a filer without
// operate_gates gets 403 with nothing recorded, before the idempotency
// lookup, and an operator's approval records the approve intervention and
// spec.version_approved under that person at completed (component-http-api
// invariant 12; component-plan-state invariant 2; req-task-plan-state
// AC-2.3; req-review-gates-evidence REQ-1, AC-1.2).
func TestRESTIntakePlanApprovalRequiresOperateGates(t *testing.T) {
	f := newSessionIntakeFixture(t)
	// Every shipped role that holds create_tasks also holds operate_gates, so
	// the live binding here withholds operate_gates from the maintainer.
	f.server.Memberships = withheldCapability{lockedMemberships: f.members, userID: intakeOwner(core.WorkspaceRoleMaintainer), capability: core.CapabilityOperateGates}
	maintainer := humanIntakeToken(core.WorkspaceRoleMaintainer)
	seed := f.postTaskMultipart(t, maintainer, intakeFields(nil), "approval-key")
	if seed.status != http.StatusCreated {
		t.Fatalf("seed status=%d body=%s", seed.status, seed.body)
	}
	approved := intakeFields(map[string]any{"plan": intakeTestPlan, "plan_base_commit": intakeTestCommit, "plan_approved": true})
	for _, surface := range []string{"json", "multipart retry"} {
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		var result restIntakeResult
		if surface == "json" {
			result = f.postTaskJSON(t, maintainer, approved)
		} else {
			result = f.postTaskMultipart(t, maintainer, approved, "approval-key")
		}
		if result.status != http.StatusForbidden || result.code() != planApprovalForbidden {
			t.Fatalf("maintainer %s: status=%d body=%s", surface, result.status, result.body)
		}
		f.requireNoSideEffects(t, "maintainer "+surface, tasks, titles, notified)
	}
	if got := f.members.lastCapability(); got != core.CapabilityOperateGates {
		t.Fatalf("judged capability=%q", got)
	}

	operator := humanIntakeToken(core.WorkspaceRoleOperator)
	created := f.postTaskJSON(t, operator, approved)
	if created.status != http.StatusCreated || created.task.PlanState != core.PlanCompleted {
		t.Fatalf("operator approval status=%d body=%s", created.status, created.body)
	}
	if _, present := created.fields["spec_approval"]; present {
		t.Fatalf("intake response serves spec_approval: %s", created.body)
	}
	ctx := store.WithWorkspace(t.Context(), "alpha")
	plan, found, err := f.store.GetApprovedSpecVersion(ctx, created.task.ID)
	if err != nil || !found || plan.Version != 1 || plan.Origin != core.PlanOriginIntake || plan.BaseCommit == nil || *plan.BaseCommit != intakeTestCommit {
		t.Fatalf("approved plan=%+v found=%t err=%v", plan, found, err)
	}
	interventions, err := f.store.ListInterventions(ctx, created.task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].Action != core.InterventionApprove || interventions[0].ActorID != store.UserActorID(intakeOwner(core.WorkspaceRoleOperator)) || interventions[0].ActorRole != core.ActorUser {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}

	// plan_approved false leaves the plan awaiting approval and needs no
	// capability beyond create_tasks.
	pending := f.postTaskJSON(t, maintainer, intakeFields(map[string]any{"plan": intakeTestPlan, "plan_approved": false}))
	if pending.status != http.StatusCreated || pending.task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("plan_approved false status=%d body=%s", pending.status, pending.body)
	}
}

// withheldCapability is a live membership authority that withholds one
// capability from one user and delegates everything else.
type withheldCapability struct {
	*lockedMemberships
	userID     string
	capability core.Capability
}

func (w withheldCapability) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	allowed, err := w.lockedMemberships.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
	if userID == w.userID && capability == w.capability {
		return false, err
	}
	return allowed, err
}

// TestRESTIntakeResolvesThePlanState proves intake's order: a filed plan, then
// plan_state, then the legacy spec_approval, then the legacy level, then the
// workspace default, which is needed when unset (component-plan-state
// invariant 1; req-task-plan-state AC-2.1, AC-2.2).
func TestRESTIntakeResolvesThePlanState(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	for _, test := range []struct {
		name       string
		defaultSet string
		extra      map[string]any
		want       core.PlanState
	}{
		{"workspace default needed", "needed", nil, core.PlanNeeded},
		{"workspace default not_required", "not_required", nil, core.PlanNotRequired},
		{"unset workspace default", "", nil, core.PlanNeeded},
		{"plan_state wins over the default", "needed", map[string]any{"plan_state": "not_required"}, core.PlanNotRequired},
		{"plan_state wins over the level", "not_required", map[string]any{"plan_state": "needed", "level": "L0"}, core.PlanNeeded},
		{"spec_approval alias true", "not_required", map[string]any{"spec_approval": true}, core.PlanNeeded},
		{"spec_approval alias false wins over the level", "needed", map[string]any{"spec_approval": false, "level": "L3"}, core.PlanNotRequired},
		{"agreeing spec_approval and plan_state", "needed", map[string]any{"spec_approval": false, "plan_state": "not_required"}, core.PlanNotRequired},
		{"level L1", "needed", map[string]any{"level": "L1"}, core.PlanNotRequired},
		{"level L2", "not_required", map[string]any{"level": "L2"}, core.PlanNeeded},
		{"a plan ignores the level", "not_required", map[string]any{"plan": intakeTestPlan, "level": "L0"}, core.PlanAwaitingApproval},
		{"a plan with spec_approval true", "not_required", map[string]any{"plan": intakeTestPlan, "spec_approval": true}, core.PlanAwaitingApproval},
	} {
		f.cfgMu.Lock()
		copied := *f.cfg
		copied.Execution.DefaultPlanState = test.defaultSet
		f.cfg = &copied
		f.cfgMu.Unlock()
		result := f.postTaskJSON(t, token, intakeFields(test.extra))
		if result.status != http.StatusCreated || result.task.PlanState != test.want {
			t.Fatalf("%s: status=%d body=%s", test.name, result.status, result.body)
		}
		stored, err := f.store.GetTask(store.WithWorkspace(t.Context(), "alpha"), result.task.ID)
		if err != nil || stored.PlanState != test.want {
			t.Fatalf("%s: stored=%q err=%v", test.name, stored.PlanState, err)
		}
	}
	plain := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": intakeTestPlan, "plan_base_commit": intakeTestCommit}))
	plan, found, err := f.store.GetLatestSpecVersion(store.WithWorkspace(t.Context(), "alpha"), plain.task.ID)
	if err != nil || !found || plan.Version != 1 || plan.Origin != core.PlanOriginIntake || plan.Approved || plan.BaseCommit == nil || *plan.BaseCommit != intakeTestCommit {
		t.Fatalf("intake plan=%+v found=%t err=%v", plan, found, err)
	}
}

// TestRESTIntakeRetryComparesPlanFields proves a keyed multipart retry
// returns the original task only when its plan state and plan fields repeat
// the original, and answers 409 otherwise (component-task-lifecycle
// invariant 17).
func TestRESTIntakeRetryComparesPlanFields(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	withPlan := intakeFields(map[string]any{"plan": intakeTestPlan, "plan_base_commit": intakeTestCommit, "plan_approved": true})
	first := f.postTaskMultipart(t, token, withPlan, "retry-plan")
	if first.status != http.StatusCreated || first.task.PlanState != core.PlanCompleted {
		t.Fatalf("first status=%d body=%s", first.status, first.body)
	}
	titles := f.titles.Load()
	again := f.postTaskMultipart(t, token, withPlan, "retry-plan")
	if again.status != http.StatusOK || again.task.ID != first.task.ID || f.titles.Load() != titles {
		t.Fatalf("exact retry status=%d body=%s", again.status, again.body)
	}
	for name, changed := range map[string]map[string]any{
		"different plan":        {"plan": intakeTestPlan + "\n- Also the reads.", "plan_base_commit": intakeTestCommit, "plan_approved": true},
		"different base commit": {"plan": intakeTestPlan, "plan_base_commit": strings.Repeat("a", 40), "plan_approved": true},
		"no base commit":        {"plan": intakeTestPlan, "plan_approved": true},
		"approval withdrawn":    {"plan": intakeTestPlan, "plan_base_commit": intakeTestCommit},
		"no plan":               {"plan_state": "needed"},
	} {
		result := f.postTaskMultipart(t, token, intakeFields(changed), "retry-plan")
		if result.status != http.StatusConflict {
			t.Fatalf("%s: status=%d body=%s", name, result.status, result.body)
		}
	}

	stated := intakeFields(map[string]any{"plan_state": "not_required"})
	if result := f.postTaskMultipart(t, token, stated, "retry-state"); result.status != http.StatusCreated {
		t.Fatalf("stated status=%d body=%s", result.status, result.body)
	}
	if result := f.postTaskMultipart(t, token, stated, "retry-state"); result.status != http.StatusOK {
		t.Fatalf("stated retry status=%d body=%s", result.status, result.body)
	}
	for name, changed := range map[string]map[string]any{
		"plan_state needed":    {"plan_state": "needed"},
		"spec_approval true":   {"spec_approval": true},
		"a plan":               {"plan": intakeTestPlan},
		"level L3 maps needed": {"level": "L3"},
	} {
		result := f.postTaskMultipart(t, token, intakeFields(changed), "retry-state")
		if result.status != http.StatusConflict {
			t.Fatalf("%s: status=%d body=%s", name, result.status, result.body)
		}
	}
	// The legacy alias with the same meaning repeats the request.
	if result := f.postTaskMultipart(t, token, intakeFields(map[string]any{"spec_approval": false}), "retry-state"); result.status != http.StatusOK {
		t.Fatalf("alias retry status=%d body=%s", result.status, result.body)
	}
}

// TestTaskReadsCarryPlanStateAndNoSpecApproval proves every task read serves
// plan_state and no spec_approval (component-plan-state invariant 1;
// req-task-plan-state AC-1.4).
func TestTaskReadsCarryPlanStateAndNoSpecApproval(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	created := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan_state": "not_required"}))
	if created.status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.status, created.body)
	}
	for _, path := range []string{"/v1/tasks/" + created.task.ID, "/v1/tasks", "/v1/task-operations", "/v1/activity"} {
		status, body := f.getJSON(t, token, path+"?workspace_id=alpha")
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, status, body)
		}
		if !strings.Contains(body, `"plan_state":"not_required"`) {
			t.Fatalf("%s omits plan_state: %s", path, body)
		}
		if strings.Contains(body, `"spec_approval"`) {
			t.Fatalf("%s serves spec_approval: %s", path, body)
		}
	}
	read := f.callMCP(t, token, "get_task", map[string]any{"workspace_id": "alpha", "task_id": created.task.ID})
	if read.errText != "" {
		t.Fatalf("get_task: %s", read.errText)
	}
	text := f.callMCPText(t, token, "get_task", map[string]any{"workspace_id": "alpha", "task_id": created.task.ID})
	if !strings.Contains(text, `"plan_state":"not_required"`) || strings.Contains(text, `"spec_approval"`) {
		t.Fatalf("get_task=%s", text)
	}
	events := f.callMCPText(t, token, "list_task_events", map[string]any{"workspace_id": "alpha", "task_id": created.task.ID})
	if !strings.Contains(events, `"kind":"task.plan_state_changed"`) || !strings.Contains(events, `"cause":"intake"`) || !strings.Contains(events, `"to":"not_required"`) {
		t.Fatalf("list_task_events=%s", events)
	}
}

// callMCPText returns the raw text of one successful MCP tool result.
func (f *sessionIntakeFixture) callMCPText(t *testing.T, token, tool string, args map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("%s: status=%d body=%s err=%v", tool, response.Code, response.Body.String(), err)
	}
	return envelope.Result.Content[0].Text
}

// TestMCPCreateTaskPlanArguments proves MCP create_task applies plan_state and
// files a plan at plan awaiting approval, and refuses any plan_approved, null
// included, and mistyped plan arguments with nothing recorded, before the
// idempotency lookup (component-mcp-protocol invariant 38;
// req-task-plan-state AC-1.4, AC-2.4).
func TestMCPCreateTaskPlanArguments(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	stated := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan-state", map[string]any{"plan_state": "not_required"}))
	if stated.errText != "" || stated.task.PlanState != core.PlanNotRequired {
		t.Fatalf("plan_state: err=%q task=%+v", stated.errText, stated.task)
	}
	planned := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan", map[string]any{"plan": intakeTestPlan, "plan_base_commit": intakeTestCommit}))
	if planned.errText != "" || planned.task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("plan: err=%q task=%+v", planned.errText, planned.task)
	}
	ctx := store.WithWorkspace(t.Context(), "alpha")
	plan, found, err := f.store.GetLatestSpecVersion(ctx, planned.task.ID)
	if err != nil || !found || plan.Origin != core.PlanOriginIntake || plan.Approved || plan.BaseCommit == nil || *plan.BaseCommit != intakeTestCommit {
		t.Fatalf("MCP plan=%+v found=%t err=%v", plan, found, err)
	}
	if interventions, err := f.store.ListInterventions(ctx, planned.task.ID); err != nil || len(interventions) != 0 {
		t.Fatalf("MCP recorded a plan approval: %+v err=%v", interventions, err)
	}

	for _, value := range []any{true, false, nil} {
		for _, key := range []string{"mcp-approved-fresh", "mcp-plan"} {
			tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
			result := f.callMCP(t, token, "create_task", intakeArgs(key, map[string]any{"plan": intakeTestPlan, "plan_approved": value}))
			if !strings.HasPrefix(result.errText, invalidGateOverride+": ") || !strings.Contains(result.errText, "plan_approved") {
				t.Fatalf("plan_approved %v key %s: err=%q", value, key, result.errText)
			}
			f.requireNoSideEffects(t, "plan_approved", tasks, titles, notified)
		}
	}
	// The refused key holds no idempotency record: its first accepted call
	// creates a task.
	fresh := f.callMCP(t, token, "create_task", intakeArgs("mcp-approved-fresh", map[string]any{"plan": intakeTestPlan}))
	if fresh.errText != "" || !fresh.created || fresh.task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("fresh key: err=%q created=%t task=%+v", fresh.errText, fresh.created, fresh.task)
	}

	for name, extra := range map[string]map[string]any{
		"plan_state number":    {"plan_state": 1},
		"plan_state null":      {"plan_state": nil},
		"plan boolean":         {"plan": true},
		"base commit number":   {"plan": intakeTestPlan, "plan_base_commit": 7},
		"invalid plan":         {"plan": "## Approach\nOnly."},
		"plan_state with plan": {"plan": intakeTestPlan, "plan_state": "needed"},
		"short base commit":    {"plan": intakeTestPlan, "plan_base_commit": "abc"},
	} {
		tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
		result := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan", extra))
		if !strings.HasPrefix(result.errText, invalidPlanIntake+": ") {
			t.Fatalf("%s: err=%q", name, result.errText)
		}
		f.requireNoSideEffects(t, name, tasks, titles, notified)
	}
}

// TestWorkspaceConfigEditKeepsExistingPlanStates proves a change of the
// workspace default plan state applies to new tasks only, and that GET
// /v1/workspace serves the effective default (req-task-plan-state AC-2.1,
// AC-3.5; component-runtime invariant 39).
func TestWorkspaceConfigEditKeepsExistingPlanStates(t *testing.T) {
	document := contextualWorkspaceDocument()
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}
	st := store.NewMemory()
	s := NewServer(st)
	s.BearerToken = "token"
	s.Deployment = &config.Config{Workspace: "demo", Database: config.Database{Backend: "memory"}}
	s.ConfigStore = backend
	s.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Execution: backend.record.Document.Execution, Repos: backend.record.Document.Repos, Database: config.Database{Backend: "memory"}}, nil
	}
	s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Plan state default", nil }
	h := s.Handler()
	serve := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	createTask := func() core.Task {
		t.Helper()
		response := serve(http.MethodPost, "/v1/tasks", `{"body":"default plan state","repo":"conveyor"}`, map[string]string{"Content-Type": "application/json"})
		if response.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body)
		}
		var task core.Task
		if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	workspaceDefault := func() string {
		t.Helper()
		response := serve(http.MethodGet, "/v1/workspace", "", nil)
		var info map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &info) != nil {
			t.Fatalf("GET /v1/workspace status=%d body=%s", response.Code, response.Body)
		}
		value, _ := info["default_plan_state"].(string)
		return value
	}
	if got := workspaceDefault(); got != "needed" {
		t.Fatalf("default_plan_state=%q, want needed", got)
	}
	before := createTask()
	if before.PlanState != core.PlanNeeded {
		t.Fatalf("task before the edit=%q", before.PlanState)
	}

	document.Execution.DefaultPlanState = "not_required"
	body, _ := json.Marshal(map[string]any{"document": document})
	put := serve(http.MethodPut, "/v1/workspace/config", string(body), map[string]string{"If-Match": "1"})
	if put.Code != http.StatusOK || strings.Contains(put.Body.String(), `"spec_approval"`) || !strings.Contains(put.Body.String(), `"default_plan_state":"not_required"`) {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body)
	}
	if got := workspaceDefault(); got != "not_required" {
		t.Fatalf("default_plan_state after the edit=%q", got)
	}
	unchanged, err := st.GetTask(store.WithWorkspace(t.Context(), "demo"), before.ID)
	if err != nil || unchanged.PlanState != core.PlanNeeded {
		t.Fatalf("existing task after the edit=%q err=%v", unchanged.PlanState, err)
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == core.EventTaskPlanStateChanged && !strings.Contains(string(event.Payload), `"cause":"intake"`) {
			t.Fatalf("the edit moved an existing task: %s", event.Payload)
		}
	}
	if after := createTask(); after.PlanState != core.PlanNotRequired {
		t.Fatalf("task after the edit=%q", after.PlanState)
	}

	// A PUT of the legacy alias stores only default_plan_state, and a
	// disagreeing pair answers 422 naming the field.
	alias := document
	alias.Execution.DefaultPlanState = ""
	needed := true
	alias.Execution.SpecApproval = &needed
	body, _ = json.Marshal(map[string]any{"document": alias})
	put = serve(http.MethodPut, "/v1/workspace/config", string(body), map[string]string{"If-Match": "2"})
	if put.Code != http.StatusOK || backend.record.Document.Execution.DefaultPlanState != "needed" || backend.record.Document.Execution.SpecApproval != nil {
		t.Fatalf("alias PUT status=%d stored=%+v body=%s", put.Code, backend.record.Document.Execution, put.Body)
	}
	disagreeing := alias
	disagreeing.Execution.DefaultPlanState = "not_required"
	body, _ = json.Marshal(map[string]any{"document": disagreeing})
	put = serve(http.MethodPut, "/v1/workspace/config", string(body), map[string]string{"If-Match": "3"})
	if put.Code != http.StatusUnprocessableEntity || !strings.Contains(put.Body.String(), `"validation_failed"`) || !strings.Contains(put.Body.String(), `"field":"execution.default_plan_state"`) || backend.updates != 2 {
		t.Fatalf("disagreeing PUT status=%d updates=%d body=%s", put.Code, backend.updates, put.Body)
	}
	invalid := document
	invalid.Execution.DefaultPlanState = "completed"
	body, _ = json.Marshal(map[string]any{"document": invalid})
	put = serve(http.MethodPut, "/v1/workspace/config", string(body), map[string]string{"If-Match": "3"})
	if put.Code != http.StatusUnprocessableEntity || !strings.Contains(put.Body.String(), `"field":"execution.default_plan_state"`) || backend.updates != 2 {
		t.Fatalf("invalid PUT status=%d updates=%d body=%s", put.Code, backend.updates, put.Body)
	}
}
