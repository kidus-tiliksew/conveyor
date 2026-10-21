package httpapi

// Tests for the requirement insight routes (feature-requirement-insights
// RI-6, RI-5; req-requirement-insights REQ-2, AC-2.5 to AC-2.7, REQ-3,
// AC-3.7, REQ-4; DEC-87; DEC-61).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	insightBodySentinel   = "INSIGHT-HANDLER-BODY-SENTINEL"
	insightRequirementID  = "req-insight"
	insightFixtureSecret  = "fixture-app-key-4f1c9a7e2b8d6035"
	insightHeadlineOutput = "Two of five tasks are open; one waits on the operator and the critical path runs through two tasks."
)

// insightReply is one scripted model answer.
type insightReply struct {
	output, model string
	in, out       int64
	err           error
}

// insightAgent scripts the in-process model. When started is set, each call
// announces itself there; when release is set, each call then blocks until
// release is closed. Neither is a sleep.
type insightAgent struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	replies []insightReply
	reply   insightReply
	started chan struct{}
	release chan struct{}
}

func (agent *insightAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.mu.Lock()
	index := agent.calls
	agent.calls++
	agent.prompts = append(agent.prompts, input.Prompt)
	reply := agent.reply
	if index < len(agent.replies) {
		reply = agent.replies[index]
	}
	started, release := agent.started, agent.release
	agent.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if reply.err != nil {
		return inprocess.Result{}, reply.err
	}
	return inprocess.Result{Output: reply.output, Model: reply.model, TokensIn: reply.in, TokensOut: reply.out}, nil
}

func (agent *insightAgent) callCount() int {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.calls
}

type insightSecretSource struct {
	secrets []string
	err     error
}

func (source insightSecretSource) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return source.secrets, source.err
}

// insightFaultStore injects store failures and a cross-process winner into
// the insight capability and document reads.
type insightFaultStore struct {
	store.Store
	inputsErr, headlineErr, recordErr, designsErr, decisionsErr error
	// winner, when set, is recorded through the inner store just before the
	// handler's own write, as a second conveyord process would.
	winner func(store.RequirementInsightHeadline) store.RequirementInsightHeadline
}

func (f *insightFaultStore) ReadRequirementInsightInputs(ctx context.Context, id string) (store.RequirementInsightInputs, error) {
	if f.inputsErr != nil {
		return store.RequirementInsightInputs{}, f.inputsErr
	}
	return f.Store.ReadRequirementInsightInputs(ctx, id)
}

func (f *insightFaultStore) GetRequirementInsightHeadline(ctx context.Context, id string) (store.RequirementInsightHeadline, error) {
	if f.headlineErr != nil {
		return store.RequirementInsightHeadline{}, f.headlineErr
	}
	return f.Store.GetRequirementInsightHeadline(ctx, id)
}

func (f *insightFaultStore) RecordRequirementInsightHeadline(ctx context.Context, headline store.RequirementInsightHeadline) (store.RequirementInsightHeadline, error) {
	if f.recordErr != nil {
		return store.RequirementInsightHeadline{}, f.recordErr
	}
	if f.winner != nil {
		if _, err := f.Store.RecordRequirementInsightHeadline(ctx, f.winner(headline)); err != nil {
			return store.RequirementInsightHeadline{}, err
		}
	}
	return f.Store.RecordRequirementInsightHeadline(ctx, headline)
}

func (f *insightFaultStore) ListSystemDesigns(ctx context.Context, includeArchived bool) ([]core.SystemDesign, error) {
	if f.designsErr != nil {
		return nil, f.designsErr
	}
	return f.Store.ListSystemDesigns(ctx, includeArchived)
}

func (f *insightFaultStore) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	if f.decisionsErr != nil {
		return nil, f.decisionsErr
	}
	return f.Store.ListDecisions(ctx)
}

// insightSeed names one workspace's fixture: a confirmed requirement served
// by two open independent tasks, an open dependent of both, a merged task,
// and a closed task; an archived and an unconfirmed requirement; designs and
// decisions covering every association test.
type insightSeed struct {
	workspace                                     string
	first, second, dependent, merged, closed      string
	archived, unconfirmed                         string
	attachedDesign, namingDesign, unrelatedDesign string
	namingDecision, citedDecision, taskDecision   string
	dismissedDecision, unrelatedDecision          string
	requirementVersion                            int
}

func (seed insightSeed) taskIDs() []string {
	ids := []string{seed.first, seed.second, seed.dependent, seed.merged, seed.closed}
	sort.Strings(ids)
	return ids
}

func insightWorkspaceContext(ctx context.Context, workspace string) context.Context {
	return store.WithActor(store.WithWorkspace(ctx, workspace), store.Actor{ID: "operator", Role: core.ActorHuman})
}

// insightDesignContent appends the governs fence every System Design needs.
func insightDesignContent(content string) string {
	return content + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/insight-fixture/**\n```"
}

// requirementTitle keeps titles unique, since titles derive slugs.
func requirementTitle(workspace, id string) string {
	if id == insightRequirementID {
		return "Insight " + workspace
	}
	return "Insight " + workspace + " " + id
}

func seedRequirementInsight(t *testing.T, st store.Store, workspace string) insightSeed {
	t.Helper()
	ctx := insightWorkspaceContext(t.Context(), workspace)
	seed := insightSeed{workspace: workspace}
	decision := func(statement string, confirm, dismiss bool) string {
		t.Helper()
		proposed, err := st.ProposeDecision(ctx, core.Decision{Statement: statement, Context: "Insight fixture context.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, err = st.ConfirmDecision(ctx, proposed.ID); err != nil {
				t.Fatal(err)
			}
		}
		if dismiss {
			if _, err = st.DismissDecision(ctx, proposed.ID); err != nil {
				t.Fatal(err)
			}
		}
		return proposed.ID
	}
	seed.namingDecision = decision("Insights for "+insightRequirementID+" are informative.", true, false)
	seed.citedDecision = decision("The requirement cites this decision.", true, false)
	seed.taskDecision = decision("Serving tasks cite this decision.", false, false)
	seed.dismissedDecision = decision("Dismissed "+insightRequirementID+" decision.", false, true)
	seed.unrelatedDecision = decision("An unrelated decision.", true, false)

	statements := []core.RequirementStatement{{
		ID: "REQ-1", Statement: "The panel shall summarize progress.",
		AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "A headline is shown."}, {ID: "AC-1.2", Statement: "Charts are shown."}},
	}}
	requirement := func(id string, confirm bool) {
		t.Helper()
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: requirementTitle(workspace, id)}, core.RequirementVersion{
			Content: "# " + requirementTitle(workspace, id) + "\n\nGoverned by " + seed.citedDecision + ".", Origin: core.RequirementOriginOperator, Statements: statements,
		})
		if err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
				t.Fatal(err)
			}
			seed.requirementVersion = version.Version
		}
	}
	requirement(insightRequirementID, true)
	seed.archived, seed.unconfirmed = "req-insight-archived", "req-insight-unconfirmed"
	requirement(seed.archived, true)
	if err := st.ArchiveRequirement(ctx, seed.archived, "operator", nil); err != nil {
		t.Fatal(err)
	}
	requirement(seed.unconfirmed, false)
	seed.requirementVersion = 1

	design := func(id, content string) string {
		t.Helper()
		_, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Design " + id, Category: "Component design"}, core.SystemDesignVersion{Content: insightDesignContent(content), Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seed.attachedDesign = design("component-insight-attached", "# Attached design")
	seed.namingDesign = design("component-insight-naming", "# Naming design\n\nImplements "+insightRequirementID+".")
	seed.unrelatedDesign = design("component-insight-unrelated", "# Unrelated design mentions "+insightRequirementID+"-other only.")
	if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: seed.attachedDesign, Content: insightDesignContent("# Attached design v2"), Origin: core.SystemDesignOriginOperator}); err != nil {
		t.Fatal(err)
	}

	created := time.Now().UTC().Add(-time.Hour)
	task := func(title string, dependencies ...string) string {
		t.Helper()
		id := workspace + "-" + core.NewTaskID()
		created = created.Add(time.Second)
		body := insightBodySentinel + " implements AC-1.1 under " + seed.taskDecision + "."
		if err := st.CreateTaskWithDependencies(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: title, Body: body, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created}, dependencies); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seed.first = task("first open")
	seed.second = task("second open")
	seed.dependent = task("dependent open", seed.first, seed.second)
	seed.merged = task("merged")
	seed.closed = task("closed")
	for _, id := range seed.taskIDs() {
		change := store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{insightRequirementID}}}
		if id == seed.first || id == seed.dependent {
			change.Add.DesignIDs = []string{seed.attachedDesign}
		}
		if _, err := st.UpdateTaskContext(ctx, id, change); err != nil {
			t.Fatal(err)
		}
	}
	plane := taskops.New(st)
	if _, err := plane.Perform(ctx, seed.merged, taskops.Command{Kind: core.TaskMergeRecover}); err != nil {
		t.Fatal(err)
	}
	if _, err := plane.Perform(ctx, seed.closed, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}
	return seed
}

type insightHarness struct {
	t       *testing.T
	st      store.Store
	server  *Server
	agent   *insightAgent
	members *membershipFixture
	demo    insightSeed
	other   insightSeed
}

// newInsightHarness serves two workspaces holding the same requirement ID.
// viewer may only view demo; creator may create tasks in both.
func newInsightHarness(t *testing.T, wrap func(store.Store) store.Store) *insightHarness {
	t.Helper()
	inner := store.NewMemory()
	h := &insightHarness{t: t, st: inner}
	h.demo = seedRequirementInsight(t, inner, "demo")
	h.other = seedRequirementInsight(t, inner, "other")
	served := store.Store(inner)
	if wrap != nil {
		served = wrap(inner)
	}
	h.members = &membershipFixture{
		workspaces: []core.Workspace{{ID: "demo"}, {ID: "other"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"viewer":  {"demo": core.WorkspaceRoleViewer},
			"creator": {"demo": core.WorkspaceRoleMaintainer, "other": core.WorkspaceRoleMaintainer},
		},
	}
	h.server = NewServer(served)
	h.server.Workspaces, h.server.Memberships = h.members, h.members
	h.server.Credentials = staticCredentialVerifier{
		"viewer":  {ID: "pat_viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"creator": {ID: "pat_creator", OwnerUserID: "creator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent":   {ID: "agent_1", OwnerUserID: "creator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	h.server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
		ID: "session_creator", OwnerUserID: "creator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession,
	}}
	h.agent = &insightAgent{reply: insightReply{output: insightHeadlineOutput, model: "served-model-2026", in: 512, out: 64}}
	h.installDispatcher(h.agent)
	h.server.WorkOrders.RedactionSecrets = insightSecretSource{secrets: []string{insightFixtureSecret}}
	return h
}

func (h *insightHarness) installDispatcher(agent inprocess.Agent) {
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "triage-model", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess},
	}}}
	h.server.WorkOrders = &workorder.Service{Store: h.st, Dispatcher: dispatch.New(h.st, cfg, agent)}
}

func (h *insightHarness) call(method, token, workspace, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(method, path+"?workspace_id="+workspace, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	return response
}

func insightPath(requirementID, view string) string {
	return "/v1/requirements/" + requirementID + "/insights/" + view
}

func (h *insightHarness) stored(workspace string) (store.RequirementInsightHeadline, bool) {
	h.t.Helper()
	headline, err := h.st.GetRequirementInsightHeadline(store.WithWorkspace(h.t.Context(), workspace), insightRequirementID)
	if errors.Is(err, store.ErrNotFound) {
		return store.RequirementInsightHeadline{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return headline, true
}

// expectedState projects the requirement independently of the handler: the
// store read, the shared next-action projection for the caller, and
// nextaction.CompactFrom (DEC-61).
func (h *insightHarness) expectedState(workspace, user string) (insight.Inputs, map[string]nextaction.Compact) {
	h.t.Helper()
	ctx := store.WithWorkspace(h.t.Context(), workspace)
	ctx = store.WithCredential(ctx, core.AuthenticatedCredential{ID: "pat_" + user, OwnerUserID: user, Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
	inputs, err := h.st.ReadRequirementInsightInputs(ctx, insightRequirementID)
	if err != nil {
		h.t.Fatal(err)
	}
	opts, err := h.server.nextActionOptionsFor(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	actions := map[string]nextaction.Compact{}
	for _, serving := range inputs.Tasks {
		one, err := store.ReadNextAction(ctx, h.st, serving.Task.ID, opts)
		if err != nil {
			h.t.Fatal(err)
		}
		actions[serving.Task.ID] = nextaction.CompactFrom(one)
	}
	return inputs, actions
}

// lifecycleSnapshot is every task's row, event count, and work orders.
func (h *insightHarness) lifecycleSnapshot(seed insightSeed) string {
	h.t.Helper()
	ctx := store.WithWorkspace(h.t.Context(), seed.workspace)
	var out strings.Builder
	for _, id := range seed.taskIDs() {
		task, err := h.st.GetTask(ctx, id)
		if err != nil {
			h.t.Fatal(err)
		}
		events, err := h.st.ListEvents(ctx, id)
		if err != nil {
			h.t.Fatal(err)
		}
		fmt.Fprintf(&out, "%s %s %s %d\n", id, task.State, task.NextStage, len(events))
	}
	orders, err := h.st.ListWorkOrdersForTasks(ctx, seed.taskIDs())
	if err != nil {
		h.t.Fatal(err)
	}
	fmt.Fprintf(&out, "orders %d", len(orders))
	return out.String()
}

func decodeInsight[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %T: %v body=%s", value, err, response.Body.String())
	}
	return value
}

func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode object: %v raw=%s", err, raw)
	}
	return mapKeys(object)
}

func requireInsightKeys(t *testing.T, what string, raw json.RawMessage, want ...string) {
	t.Helper()
	sort.Strings(want)
	if keys := jsonKeys(t, raw); !slices.Equal(keys, want) {
		t.Fatalf("%s keys=%v want %v", what, keys, want)
	}
}

type summaryWire struct {
	Headline *struct {
		Text        string    `json:"text"`
		Model       string    `json:"model"`
		GeneratedAt time.Time `json:"generated_at"`
		GeneratedBy string    `json:"generated_by"`
		Current     bool      `json:"current"`
	} `json:"headline"`
	Fingerprint  string                     `json:"fingerprint"`
	CanGenerate  bool                       `json:"can_generate"`
	Groups       []insight.Group            `json:"groups"`
	CriticalPath insight.CriticalPathResult `json:"critical_path"`
	Forecast     *insight.Forecast          `json:"forecast"`
}

// TestRequirementInsightAuthorizationAndWorkspaceIsolation proves every
// route's admission: anonymous, forged, and agent credentials are refused,
// an unbound workspace answers as a missing one, a viewer reads but cannot
// generate, a session POST needs its CSRF proof, and the same requirement ID
// in two workspaces never mixes (DEC-87 (6); DEC-19).
func TestRequirementInsightAuthorizationAndWorkspaceIsolation(t *testing.T) {
	h := newInsightHarness(t, nil)
	routes := []struct{ method, view string }{
		{http.MethodGet, "summary"}, {http.MethodPost, "summary"}, {http.MethodGet, "progress"}, {http.MethodGet, "documents"},
	}
	for _, route := range routes {
		path := insightPath(insightRequirementID, route.view)
		for _, token := range []string{"", "forged", "agent"} {
			if response := h.call(route.method, token, "demo", path); response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s token %q status=%d body=%s", route.method, path, token, response.Code, response.Body.String())
			}
		}
		unbound, missing := h.call(route.method, "viewer", "other", path), h.call(route.method, "viewer", "absent", path)
		if unbound.Code != http.StatusNotFound || unbound.Body.String() != canonicalWorkspaceNotFoundBody() || missing.Body.String() != unbound.Body.String() {
			t.Fatalf("%s %s unbound=%d/%q missing=%d/%q", route.method, path, unbound.Code, unbound.Body.String(), missing.Code, missing.Body.String())
		}
	}
	for _, view := range []string{"summary", "progress", "documents"} {
		h.members.capabilityCalls = nil
		if response := h.call(http.MethodGet, "viewer", "demo", insightPath(insightRequirementID, view)); response.Code != http.StatusOK {
			t.Fatalf("viewer %s status=%d body=%s", view, response.Code, response.Body.String())
		}
		for _, capability := range h.members.capabilityCalls {
			if capability != core.CapabilityViewWorkspace && capability != core.CapabilityCreateTasks && !slices.Contains(nextActionCapabilities, capability) {
				t.Fatalf("viewer %s asked for %q (calls=%v)", view, capability, h.members.capabilityCalls)
			}
		}
	}
	if response := h.call(http.MethodPost, "viewer", "demo", insightPath(insightRequirementID, "summary")); response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("viewer POST status=%d body=%s", response.Code, response.Body.String())
	}
	// The workspace header selects the workspace as the query does, and a
	// header that disagrees with the query is refused before any read.
	for _, route := range routes {
		headerOnly := httptest.NewRequest(route.method, insightPath(insightRequirementID, route.view), nil)
		headerOnly.Header.Set("Authorization", "Bearer viewer")
		headerOnly.Header.Set("X-Workspace-ID", "other")
		response := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(response, headerOnly)
		if response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
			t.Fatalf("header-selected unbound %s %s status=%d body=%s", route.method, route.view, response.Code, response.Body.String())
		}
		conflicting := httptest.NewRequest(route.method, insightPath(insightRequirementID, route.view)+"?workspace_id=demo", nil)
		conflicting.Header.Set("Authorization", "Bearer creator")
		conflicting.Header.Set("X-Workspace-ID", "other")
		response = httptest.NewRecorder()
		h.server.Handler().ServeHTTP(response, conflicting)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "workspace_conflict") {
			t.Fatalf("conflicting workspace %s %s status=%d body=%s", route.method, route.view, response.Code, response.Body.String())
		}
	}
	headerRead := httptest.NewRequest(http.MethodGet, insightPath(insightRequirementID, "progress"), nil)
	headerRead.Header.Set("Authorization", "Bearer creator")
	headerRead.Header.Set("X-Workspace-ID", "other")
	headerResponse := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(headerResponse, headerRead)
	if headerResponse.Code != http.StatusOK || headerResponse.Body.String() != h.call(http.MethodGet, "creator", "other", insightPath(insightRequirementID, "progress")).Body.String() {
		t.Fatalf("header-selected read status=%d body=%s", headerResponse.Code, headerResponse.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, insightPath(insightRequirementID, "summary")+"?workspace_id=demo", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
	refused := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(refused, request)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("session POST without CSRF proof status=%d body=%s", refused.Code, refused.Body.String())
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("refused requests called the model %d times", calls)
	}
	if _, ok := h.stored("demo"); ok {
		t.Fatal("a refused request stored a headline")
	}

	// A session POST with its proof generates, stamped with the session's
	// user; the other workspace's same requirement ID stays untouched.
	request = httptest.NewRequest(http.MethodPost, insightPath(insightRequirementID, "summary")+"?workspace_id=demo", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
	request.Header.Set("X-Conveyor-CSRF", "1")
	request.Header.Set("Origin", "http://example.com")
	generated := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(generated, request)
	if generated.Code != http.StatusOK {
		t.Fatalf("session POST status=%d body=%s", generated.Code, generated.Body.String())
	}
	if row, ok := h.stored("demo"); !ok || row.GeneratedBy != store.UserActorID("creator") || row.WorkspaceID != "demo" {
		t.Fatalf("demo row=%+v ok=%v", row, ok)
	}
	if _, ok := h.stored("other"); ok {
		t.Fatal("generation in demo stored a headline in other")
	}
	otherSummary := decodeInsight[summaryWire](t, h.call(http.MethodGet, "creator", "other", insightPath(insightRequirementID, "summary")))
	if otherSummary.Headline != nil {
		t.Fatalf("other workspace saw demo's headline: %+v", otherSummary.Headline)
	}
	for _, view := range []string{"summary", "progress", "documents"} {
		demoBody := h.call(http.MethodGet, "creator", "demo", insightPath(insightRequirementID, view)).Body.String()
		otherBody := h.call(http.MethodGet, "creator", "other", insightPath(insightRequirementID, view)).Body.String()
		for _, id := range h.other.taskIDs() {
			if strings.Contains(demoBody, id) {
				t.Fatalf("demo %s leaked other task %s", view, id)
			}
		}
		for _, id := range h.demo.taskIDs() {
			if strings.Contains(otherBody, id) {
				t.Fatalf("other %s leaked demo task %s", view, id)
			}
		}
	}
}

// TestRequirementInsightNotFound proves an unknown, archived, or unconfirmed
// requirement answers 404 on every route without a model call (RI-6).
func TestRequirementInsightNotFound(t *testing.T) {
	h := newInsightHarness(t, nil)
	for _, id := range []string{"req-unknown", h.demo.archived, h.demo.unconfirmed} {
		for _, route := range []struct{ method, view string }{
			{http.MethodGet, "summary"}, {http.MethodPost, "summary"}, {http.MethodGet, "progress"}, {http.MethodGet, "documents"},
		} {
			response := h.call(route.method, "creator", "demo", insightPath(id, route.view))
			if response.Code != http.StatusNotFound || strings.TrimSpace(response.Body.String()) != `{"error":"requirement_not_found"}` {
				t.Fatalf("%s %s %s status=%d body=%s", route.method, id, route.view, response.Code, response.Body.String())
			}
		}
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("not-found requests called the model %d times", calls)
	}
}

// TestRequirementInsightSummaryProjection proves the summary view's exact
// field allowlist, its computed sections against the dependency's pure
// functions over the shared compact next actions, can_generate for a viewer
// and a task creator, the null, current, and stale headline states, and that
// GET never calls the model (AC-2.1 to AC-2.6; DEC-61; DEC-87 (3), (4)).
func TestRequirementInsightSummaryProjection(t *testing.T) {
	h := newInsightHarness(t, nil)
	path := insightPath(insightRequirementID, "summary")
	viewed := h.call(http.MethodGet, "viewer", "demo", path)
	if viewed.Code != http.StatusOK || strings.Contains(viewed.Body.String(), insightBodySentinel) {
		t.Fatalf("viewer summary status=%d body=%s", viewed.Code, viewed.Body.String())
	}
	requireInsightKeys(t, "summary", viewed.Body.Bytes(), "headline", "fingerprint", "can_generate", "groups", "critical_path", "forecast")
	var raw struct {
		Groups       []json.RawMessage `json:"groups"`
		CriticalPath json.RawMessage   `json:"critical_path"`
		Forecast     json.RawMessage   `json:"forecast"`
		Headline     json.RawMessage   `json:"headline"`
	}
	if err := json.Unmarshal(viewed.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	sameJSON := func(what string, got any, want any) {
		t.Helper()
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("%s=%s\nwant %s", what, gotJSON, wantJSON)
		}
	}
	if string(raw.Headline) != "null" {
		t.Fatalf("headline=%s, want null", raw.Headline)
	}
	requireInsightKeys(t, "critical_path", raw.CriticalPath, "tasks", "length")
	for _, group := range raw.Groups {
		requireInsightKeys(t, "group", group, "actor", "count", "tasks")
		var tasks struct {
			Tasks []json.RawMessage `json:"tasks"`
		}
		if err := json.Unmarshal(group, &tasks); err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks.Tasks {
			requireInsightKeys(t, "group task", task, "id", "title", "next_action")
		}
	}

	view := decodeInsight[summaryWire](t, viewed)
	inputs, actions := h.expectedState("demo", "viewer")
	now := time.Now()
	if view.CanGenerate {
		t.Fatal("viewer can_generate=true")
	}
	sameJSON("groups", raw.Groups, insight.Groups(inputs, actions, now))
	sameJSON("critical_path", raw.CriticalPath, insight.CriticalPath(inputs, actions, now))
	sameJSON("forecast", raw.Forecast, insight.Progress(inputs, actions, now).Forecast)
	if view.CriticalPath.Length != 2 || view.Forecast == nil {
		t.Fatalf("critical path=%+v forecast=%+v", view.CriticalPath, view.Forecast)
	}
	if view.Fingerprint != insight.Fingerprint(inputs, actions, now) {
		t.Fatalf("fingerprint=%s want %s", view.Fingerprint, insight.Fingerprint(inputs, actions, now))
	}
	if len(view.Groups) != len(insight.Actors) {
		t.Fatalf("groups=%+v", view.Groups)
	}
	open := 0
	for i, group := range view.Groups {
		if group.Actor != insight.Actors[i] || group.Count != len(group.Tasks) {
			t.Fatalf("group %d=%+v", i, group)
		}
		open += group.Count
		for _, task := range group.Tasks {
			if task.ID == h.demo.merged || task.ID == h.demo.closed || task.NextAction.Label == "" {
				t.Fatalf("group task %+v", task)
			}
		}
	}
	if open != 3 {
		t.Fatalf("open grouped tasks=%d, want 3", open)
	}
	if first := view.CriticalPath.Tasks[len(view.CriticalPath.Tasks)-1]; first.ID != h.demo.dependent {
		t.Fatalf("critical path does not end at the dependent: %+v", view.CriticalPath)
	}

	creator := decodeInsight[summaryWire](t, h.call(http.MethodGet, "creator", "demo", path))
	if !creator.CanGenerate || creator.Headline != nil {
		t.Fatalf("creator summary can_generate=%v headline=%+v", creator.CanGenerate, creator.Headline)
	}
	if generated := h.call(http.MethodPost, "creator", "demo", path); generated.Code != http.StatusOK {
		t.Fatalf("generate status=%d body=%s", generated.Code, generated.Body.String())
	}
	current := h.call(http.MethodGet, "viewer", "demo", path)
	var headline struct {
		Headline json.RawMessage `json:"headline"`
	}
	if err := json.Unmarshal(current.Body.Bytes(), &headline); err != nil {
		t.Fatal(err)
	}
	requireInsightKeys(t, "headline", headline.Headline, "text", "model", "generated_at", "generated_by", "current")
	if got := decodeInsight[summaryWire](t, current); got.Headline == nil || !got.Headline.Current || got.Headline.Text != insightHeadlineOutput || got.CanGenerate {
		t.Fatalf("current summary=%+v", got.Headline)
	}

	// A new serving task changes the input, so the stored headline is stale.
	ctx := insightWorkspaceContext(t.Context(), "demo")
	added := "demo-" + core.NewTaskID()
	if err := h.st.CreateTaskWithDependencies(ctx, core.Task{ID: added, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + added, Title: "late task", Body: insightBodySentinel, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpdateTaskContext(ctx, added, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{insightRequirementID}}}); err != nil {
		t.Fatal(err)
	}
	stale := decodeInsight[summaryWire](t, h.call(http.MethodGet, "viewer", "demo", path))
	if stale.Headline == nil || stale.Headline.Current || stale.Headline.Text != insightHeadlineOutput || stale.Fingerprint == creator.Fingerprint {
		t.Fatalf("stale summary=%+v fingerprint=%s", stale.Headline, stale.Fingerprint)
	}
	if calls := h.agent.callCount(); calls != 1 {
		t.Fatalf("model calls=%d, want only the POST's", calls)
	}
}

// TestRequirementInsightGenerateOnceAndCurrent proves the first POST
// generates and stores the actual model, tokens, actor, and time; a second
// POST with an unchanged fingerprint answers exactly 409 without a model call
// (AC-2.6); and a changed fingerprint permits replacement.
func TestRequirementInsightGenerateOnceAndCurrent(t *testing.T) {
	h := newInsightHarness(t, nil)
	path := insightPath(insightRequirementID, "summary")
	before := h.lifecycleSnapshot(h.demo)
	started := time.Now().UTC()
	first := h.call(http.MethodPost, "creator", "demo", path)
	if first.Code != http.StatusOK || strings.Contains(first.Body.String(), insightBodySentinel) {
		t.Fatalf("first POST status=%d body=%s", first.Code, first.Body.String())
	}
	requireInsightKeys(t, "generated summary", first.Body.Bytes(), "headline", "fingerprint", "can_generate", "groups", "critical_path", "forecast")
	view := decodeInsight[summaryWire](t, first)
	row, ok := h.stored("demo")
	if !ok || row.Headline != insightHeadlineOutput || row.Model != "served-model-2026" || row.TokensIn != 512 || row.TokensOut != 64 || row.GeneratedBy != store.UserActorID("creator") || row.Fingerprint != view.Fingerprint || row.GeneratedAt.Before(started.Truncate(time.Microsecond)) {
		t.Fatalf("stored row=%+v ok=%v view=%+v", row, ok, view)
	}
	if view.Headline == nil || !view.Headline.Current || view.Headline.Text != row.Headline || view.Headline.Model != row.Model || !view.Headline.GeneratedAt.Equal(row.GeneratedAt) || view.Headline.GeneratedBy != row.GeneratedBy || !view.CanGenerate {
		t.Fatalf("generated view=%+v", view.Headline)
	}
	prompt := h.agent.prompts[0]
	if strings.Contains(prompt, insightBodySentinel) || !strings.Contains(prompt, h.demo.first) || !strings.Contains(prompt, "Insight demo") {
		t.Fatalf("prompt=%s", prompt)
	}

	second := h.call(http.MethodPost, "creator", "demo", path)
	if second.Code != http.StatusConflict || strings.TrimSpace(second.Body.String()) != `{"error":"insight_headline_current"}` {
		t.Fatalf("second POST status=%d body=%s", second.Code, second.Body.String())
	}
	if calls := h.agent.callCount(); calls != 1 {
		t.Fatalf("model calls=%d after a current refusal", calls)
	}
	if again, _ := h.stored("demo"); again != row {
		t.Fatalf("refusal changed the row: %+v", again)
	}
	if after := h.lifecycleSnapshot(h.demo); after != before {
		t.Fatalf("generation changed task lifecycle state:\nbefore %s\nafter  %s", before, after)
	}

	// Removing a serving task changes the fingerprint; regeneration replaces
	// the row.
	h.agent.mu.Lock()
	h.agent.reply = insightReply{output: "One fewer task serves the requirement.", model: "served-model-2027", in: 1, out: 2}
	h.agent.mu.Unlock()
	if _, err := h.st.UpdateTaskContext(insightWorkspaceContext(t.Context(), "demo"), h.demo.second, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{insightRequirementID}}}); err != nil {
		t.Fatal(err)
	}
	third := h.call(http.MethodPost, "creator", "demo", path)
	if third.Code != http.StatusOK {
		t.Fatalf("regenerate status=%d body=%s", third.Code, third.Body.String())
	}
	replaced, _ := h.stored("demo")
	if replaced.Headline != "One fewer task serves the requirement." || replaced.Model != "served-model-2027" || replaced.Fingerprint == row.Fingerprint || h.agent.callCount() != 2 {
		t.Fatalf("replaced row=%+v calls=%d", replaced, h.agent.callCount())
	}
}

// waitForInsightWaiters spins, without sleeping, until n requests hold or
// wait on the key's generation mutex.
func waitForInsightWaiters(t *testing.T, server *Server, key requirementInsightKey, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		server.requirementInsightLocks.mu.Lock()
		refs := 0
		if entry := server.requirementInsightLocks.entries[key]; entry != nil {
			refs = entry.refs
		}
		server.requirementInsightLocks.mu.Unlock()
		if refs >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiters for %+v=%d, want %d", key, refs, n)
		default:
			runtime.Gosched()
		}
	}
}

type insightResult struct {
	code int
	body string
}

func (h *insightHarness) postAsync(token, workspace, requirementID string) <-chan insightResult {
	done := make(chan insightResult, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, insightPath(requirementID, "summary")+"?workspace_id="+workspace, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(response, request)
		done <- insightResult{code: response.Code, body: response.Body.String()}
	}()
	return done
}

// TestRequirementInsightConcurrentGeneration proves the per-workspace,
// per-requirement mutex with channel barriers: an overlapping same-key POST
// waits, re-reads, and answers 409 without a second model call; the same
// requirement ID in another workspace and another requirement generate while
// the first holds its key; a waiter retries after a failed first generation;
// and the lock registry empties afterwards (RI-5).
func TestRequirementInsightConcurrentGeneration(t *testing.T) {
	h := newInsightHarness(t, nil)
	ctx := insightWorkspaceContext(t.Context(), "demo")
	second := "req-insight-second"
	_, version, err := h.st.CreateRequirement(ctx, core.Requirement{ID: second, Title: "Second"}, core.RequirementVersion{Content: "# Second", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Second."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = h.st.ConfirmRequirementVersion(ctx, second, version.Version); err != nil {
		t.Fatal(err)
	}
	agent := &insightAgent{reply: h.agent.reply, started: make(chan struct{}, 8), release: make(chan struct{})}
	h.installDispatcher(agent)
	demoKey := requirementInsightKey{workspace: "demo", requirement: insightRequirementID}

	first := h.postAsync("creator", "demo", insightRequirementID)
	<-agent.started
	waiter := h.postAsync("creator", "demo", insightRequirementID)
	waitForInsightWaiters(t, h.server, demoKey, 2)
	otherWorkspace := h.postAsync("creator", "other", insightRequirementID)
	<-agent.started
	otherRequirement := h.postAsync("creator", "demo", second)
	<-agent.started
	if calls := agent.callCount(); calls != 3 {
		t.Fatalf("model calls while the first holds its key=%d, want 3", calls)
	}
	close(agent.release)
	for name, done := range map[string]<-chan insightResult{"first": first, "other workspace": otherWorkspace, "other requirement": otherRequirement} {
		if result := <-done; result.code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", name, result.code, result.body)
		}
	}
	if result := <-waiter; result.code != http.StatusConflict || strings.TrimSpace(result.body) != `{"error":"insight_headline_current"}` {
		t.Fatalf("waiter status=%d body=%s", result.code, result.body)
	}
	if calls := agent.callCount(); calls != 3 {
		t.Fatalf("model calls=%d, want 3: the waiter re-read the stored row", calls)
	}

	// A failed first generation lets the waiter generate.
	h2 := newInsightHarness(t, nil)
	failing := &insightAgent{replies: []insightReply{{err: errors.New("provider unavailable")}}, reply: h.agent.reply, started: make(chan struct{}, 8), release: make(chan struct{})}
	h2.installDispatcher(failing)
	failed := h2.postAsync("creator", "demo", insightRequirementID)
	<-failing.started
	retry := h2.postAsync("creator", "demo", insightRequirementID)
	waitForInsightWaiters(t, h2.server, demoKey, 2)
	close(failing.release)
	if result := <-failed; result.code != http.StatusBadGateway {
		t.Fatalf("failed status=%d body=%s", result.code, result.body)
	}
	<-failing.started
	if result := <-retry; result.code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", result.code, result.body)
	}
	if calls := failing.callCount(); calls != 2 {
		t.Fatalf("model calls=%d, want 2", calls)
	}
	for _, server := range []*Server{h.server, h2.server} {
		server.requirementInsightLocks.mu.Lock()
		remaining := len(server.requirementInsightLocks.entries)
		server.requirementInsightLocks.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("lock registry kept %d entries", remaining)
		}
	}
}

// TestRequirementInsightGenerationFailuresPreserveHeadline proves 503 for an
// unconfigured deployment and 502 for an invalid reply or provider error,
// each with an absent and an existing row left exactly as it was, and a
// redacted failure detail (AC-2.7).
func TestRequirementInsightGenerationFailuresPreserveHeadline(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(insightFixtureSecret))
	providerErr := fmt.Errorf("Responses API rejected key %s (encoded %s) and sk-abcdefghijklmnopqrstuvwxyz", insightFixtureSecret, encoded)
	for _, test := range []struct {
		name   string
		setup  func(*insightHarness)
		status int
		body   string
	}{
		{name: "no work-order service", setup: func(h *insightHarness) { h.server.WorkOrders = nil }, status: http.StatusServiceUnavailable, body: `{"error":"insight_model_unconfigured"}`},
		{name: "no agent", setup: func(h *insightHarness) {
			h.installDispatcher(nil)
		}, status: http.StatusServiceUnavailable, body: `{"error":"insight_model_unconfigured"}`},
		{name: "no triage route", setup: func(h *insightHarness) {
			h.server.WorkOrders.Dispatcher.Cfg = &config.Config{Workspace: "demo"}
		}, status: http.StatusServiceUnavailable, body: `{"error":"insight_model_unconfigured"}`},
		{name: "invalid reply", setup: func(h *insightHarness) {
			h.agent.reply = insightReply{output: "# Heading\nwith two lines"}
		}, status: http.StatusBadGateway},
		{name: "provider error", setup: func(h *insightHarness) {
			h.agent.reply = insightReply{err: providerErr}
		}, status: http.StatusBadGateway},
	} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s existing=%v", test.name, existing), func(t *testing.T) {
				h := newInsightHarness(t, nil)
				path := insightPath(insightRequirementID, "summary")
				var before store.RequirementInsightHeadline
				if existing {
					// A stale stored row: a different fingerprint than the
					// current inputs.
					stored, err := h.st.RecordRequirementInsightHeadline(store.WithWorkspace(t.Context(), "demo"), store.RequirementInsightHeadline{RequirementID: insightRequirementID, Fingerprint: "stale", Headline: "Earlier headline.", Model: "earlier", GeneratedBy: "user:creator"})
					if err != nil {
						t.Fatal(err)
					}
					before = stored
				}
				test.setup(h)
				response := h.call(http.MethodPost, "creator", "demo", path)
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if test.body != "" && strings.TrimSpace(response.Body.String()) != test.body {
					t.Fatalf("body=%s want %s", response.Body.String(), test.body)
				}
				if test.status == http.StatusBadGateway {
					var failure map[string]string
					if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure["error"] != "insight_generation_failed" || failure["detail"] == "" || len(failure) != 2 {
						t.Fatalf("failure=%v err=%v", failure, err)
					}
					for _, leaked := range []string{insightFixtureSecret, encoded, "sk-abcdefghijklmnopqrstuvwxyz", "# Heading"} {
						if strings.Contains(failure["detail"], leaked) {
							t.Fatalf("detail leaked %q: %s", leaked, failure["detail"])
						}
					}
				}
				after, ok := h.stored("demo")
				if ok != existing || after != before {
					t.Fatalf("row changed: before=%+v after=%+v ok=%v", before, after, ok)
				}
				summary := decodeInsight[summaryWire](t, h.call(http.MethodGet, "creator", "demo", path))
				if existing != (summary.Headline != nil) || len(summary.Groups) != len(insight.Actors) {
					t.Fatalf("summary after failure=%+v", summary)
				}
			})
		}
	}
}

// TestRequirementInsightFailureDetailRedaction proves the detail redacts the
// in-process client's provider key and the stored secrets, and withholds
// the detail when redaction itself fails.
func TestRequirementInsightFailureDetailRedaction(t *testing.T) {
	providerKey := "provider-key-0123456789abcdef"
	server := NewServer(store.NewMemory())
	server.WorkOrders = &workorder.Service{
		Dispatcher:       dispatch.New(store.NewMemory(), nil, &inprocess.OpenAI{APIKey: providerKey}),
		RedactionSecrets: insightSecretSource{secrets: []string{insightFixtureSecret}},
	}
	detail := server.requirementInsightFailureDetail(t.Context(), fmt.Errorf("upstream echoed %s and %s and %s", providerKey, insightFixtureSecret, base64.RawURLEncoding.EncodeToString([]byte(providerKey))))
	if strings.Contains(detail, providerKey) || strings.Contains(detail, insightFixtureSecret) || strings.Contains(detail, base64.RawURLEncoding.EncodeToString([]byte(providerKey))) || !strings.Contains(detail, "upstream echoed") {
		t.Fatalf("detail=%s", detail)
	}
	server.WorkOrders.RedactionSecrets = insightSecretSource{err: errors.New("secret store offline")}
	if detail := server.requirementInsightFailureDetail(t.Context(), fmt.Errorf("raw %s", providerKey)); strings.Contains(detail, providerKey) || strings.Contains(detail, "raw") || detail == "" {
		t.Fatalf("redaction failure detail=%s", detail)
	}
}

// TestRequirementInsightStoreFailuresAndCurrentRace proves read and write
// failures answer a generic 500 without a model call or a row change, and a
// row another process recorded with the same fingerprint during generation
// answers 409 and keeps that winner (RI-5).
func TestRequirementInsightStoreFailuresAndCurrentRace(t *testing.T) {
	offline := errors.New("database offline: password=supersecretvalue1234567890")
	for _, test := range []struct {
		name   string
		fault  func(*insightFaultStore)
		method string
		view   string
		calls  int
	}{
		{name: "inputs read summary", fault: func(f *insightFaultStore) { f.inputsErr = offline }, method: http.MethodGet, view: "summary"},
		{name: "inputs read progress", fault: func(f *insightFaultStore) { f.inputsErr = offline }, method: http.MethodGet, view: "progress"},
		{name: "inputs read documents", fault: func(f *insightFaultStore) { f.inputsErr = offline }, method: http.MethodGet, view: "documents"},
		{name: "inputs read generate", fault: func(f *insightFaultStore) { f.inputsErr = offline }, method: http.MethodPost, view: "summary"},
		{name: "headline read summary", fault: func(f *insightFaultStore) { f.headlineErr = offline }, method: http.MethodGet, view: "summary"},
		{name: "headline read generate", fault: func(f *insightFaultStore) { f.headlineErr = offline }, method: http.MethodPost, view: "summary"},
		{name: "design read", fault: func(f *insightFaultStore) { f.designsErr = offline }, method: http.MethodGet, view: "documents"},
		{name: "decision read", fault: func(f *insightFaultStore) { f.decisionsErr = offline }, method: http.MethodGet, view: "documents"},
		{name: "headline write", fault: func(f *insightFaultStore) { f.recordErr = offline }, method: http.MethodPost, view: "summary", calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fault *insightFaultStore
			h := newInsightHarness(t, func(inner store.Store) store.Store {
				fault = &insightFaultStore{Store: inner}
				return fault
			})
			test.fault(fault)
			response := h.call(test.method, "creator", "demo", insightPath(insightRequirementID, test.view))
			if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if calls := h.agent.callCount(); calls != test.calls {
				t.Fatalf("model calls=%d want %d", calls, test.calls)
			}
			if _, ok := h.stored("demo"); ok {
				t.Fatal("a failed request stored a headline")
			}
		})
	}

	var fault *insightFaultStore
	h := newInsightHarness(t, func(inner store.Store) store.Store {
		fault = &insightFaultStore{Store: inner}
		return fault
	})
	before := h.lifecycleSnapshot(h.demo)
	fault.winner = func(mine store.RequirementInsightHeadline) store.RequirementInsightHeadline {
		winner := mine
		winner.Headline, winner.Model, winner.GeneratedBy = "Written by another process.", "winner-model", "user:someone-else"
		return winner
	}
	response := h.call(http.MethodPost, "creator", "demo", insightPath(insightRequirementID, "summary"))
	if response.Code != http.StatusConflict || strings.TrimSpace(response.Body.String()) != `{"error":"insight_headline_current"}` {
		t.Fatalf("race status=%d body=%s", response.Code, response.Body.String())
	}
	row, ok := h.stored("demo")
	if !ok || row.Headline != "Written by another process." || row.Model != "winner-model" || row.GeneratedBy != "user:someone-else" {
		t.Fatalf("winner overwritten: %+v ok=%v", row, ok)
	}
	if after := h.lifecycleSnapshot(h.demo); after != before {
		t.Fatalf("race changed task lifecycle state:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRequirementInsightProgressAndDocuments proves both views equal the
// dependency's computations over the fixture, the documents view's evidence
// for every design, decision, and criterion including attached_task_ids
// (feature-requirement-insights v2), and that no view carries a task body
// (AC-3.1 to AC-3.5, AC-4.1 to AC-4.3).
func TestRequirementInsightProgressAndDocuments(t *testing.T) {
	h := newInsightHarness(t, nil)
	inputs, actions := h.expectedState("demo", "viewer")
	now := time.Now()

	progress := h.call(http.MethodGet, "viewer", "demo", insightPath(insightRequirementID, "progress"))
	if progress.Code != http.StatusOK || strings.Contains(progress.Body.String(), insightBodySentinel) {
		t.Fatalf("progress status=%d body=%s", progress.Code, progress.Body.String())
	}
	requireInsightKeys(t, "progress", progress.Body.Bytes(), "daily", "forecast", "weekly", "delivered", "total", "open", "throughput_per_week", "median_cycle_seconds", "status_mix")
	wantProgress, _ := json.Marshal(insight.Progress(inputs, actions, now))
	if !bytes.Equal(bytes.TrimSpace(progress.Body.Bytes()), wantProgress) {
		t.Fatalf("progress=%s\nwant %s", progress.Body.String(), wantProgress)
	}
	gotProgress := decodeInsight[insight.ProgressResult](t, progress)
	if gotProgress.Delivered != 1 || gotProgress.Total != 4 || gotProgress.Open != 3 || gotProgress.StatusMix.Closed != 1 || len(gotProgress.Weekly) != 8 || gotProgress.Forecast == nil || gotProgress.MedianCycleSeconds == nil {
		t.Fatalf("progress=%+v", gotProgress)
	}

	documents := h.call(http.MethodGet, "viewer", "demo", insightPath(insightRequirementID, "documents"))
	if documents.Code != http.StatusOK || strings.Contains(documents.Body.String(), insightBodySentinel) {
		t.Fatalf("documents status=%d body=%s", documents.Code, documents.Body.String())
	}
	requireInsightKeys(t, "documents", documents.Body.Bytes(), "designs", "decisions", "acceptance_criteria", "cited_count", "total")
	var raw struct {
		Designs   []json.RawMessage `json:"designs"`
		Decisions []json.RawMessage `json:"decisions"`
		Criteria  []json.RawMessage `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal(documents.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, design := range raw.Designs {
		requireInsightKeys(t, "design", design, "id", "title", "current_version", "revision_pending", "attached_task_ids", "attached_task_count", "names_requirement")
	}
	for _, decision := range raw.Decisions {
		requireInsightKeys(t, "decision", decision, "id", "statement", "context", "status", "names_requirement", "cited_by_requirement", "citing_task_ids")
	}
	for _, criterion := range raw.Criteria {
		requireInsightKeys(t, "criterion", criterion, "id", "statement", "citing_task_ids")
	}
	got := decodeInsight[insight.AssociationsResult](t, documents)
	attachedTasks := []string{h.demo.first, h.demo.dependent}
	sort.Strings(attachedTasks)
	wantDesigns := []insight.DesignAssociation{
		{ID: h.demo.attachedDesign, Title: "Attached design", CurrentVersion: 1, RevisionPending: true, AttachedTaskCount: 2, AttachedTaskIDs: attachedTasks},
		{ID: h.demo.namingDesign, Title: "Naming design", CurrentVersion: 1, NamesRequirement: true, AttachedTaskIDs: []string{}},
	}
	if !reflect.DeepEqual(got.Designs, wantDesigns) {
		t.Fatalf("designs=%+v\nwant %+v", got.Designs, wantDesigns)
	}
	var decisionIDs []string
	for _, decision := range got.Decisions {
		decisionIDs = append(decisionIDs, decision.ID)
	}
	if !slices.Equal(decisionIDs, []string{h.demo.namingDecision, h.demo.citedDecision, h.demo.taskDecision}) {
		t.Fatalf("decisions=%v", decisionIDs)
	}
	if d := got.Decisions[0]; !d.NamesRequirement || d.CitedByRequirement || len(d.CitingTaskIDs) != 0 || d.Status != core.DecisionConfirmed {
		t.Fatalf("naming decision=%+v", d)
	}
	if d := got.Decisions[1]; d.NamesRequirement || !d.CitedByRequirement || len(d.CitingTaskIDs) != 0 {
		t.Fatalf("cited decision=%+v", d)
	}
	if d := got.Decisions[2]; d.NamesRequirement || d.CitedByRequirement || !slices.Equal(d.CitingTaskIDs, h.demo.taskIDs()) || d.Status != core.DecisionProposed {
		t.Fatalf("task-cited decision=%+v", d)
	}
	if len(got.AcceptanceCriteria) != 2 || got.AcceptanceCriteria[0].ID != "AC-1.1" || !slices.Equal(got.AcceptanceCriteria[0].CitingTaskIDs, h.demo.taskIDs()) || len(got.AcceptanceCriteria[1].CitingTaskIDs) != 0 || got.CitedCount != 1 || got.Total != 2 {
		t.Fatalf("criteria=%+v cited=%d total=%d", got.AcceptanceCriteria, got.CitedCount, got.Total)
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("reads called the model %d times", calls)
	}
}

// TestRequirementInsightProgress200TasksUnderOneSecond proves AC-3.7 on the
// memory store: the authenticated progress route answers a requirement
// served by 200 tasks, completely, within one second.
func TestRequirementInsightProgress200TasksUnderOneSecond(t *testing.T) {
	st := store.NewMemory()
	ctx := insightWorkspaceContext(t.Context(), "demo")
	_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: insightRequirementID, Title: "Scale"}, core.RequirementVersion{Content: "# Scale", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Scale."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, insightRequirementID, version.Version); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-24 * time.Hour)
	var previous string
	for i := range 200 {
		id := fmt.Sprintf("demo-scale-%03d", i)
		var dependencies []string
		if previous != "" && i%4 != 0 {
			dependencies = []string{previous}
		}
		if err := st.CreateTaskWithDependencies(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Scale task " + id, Body: insightBodySentinel, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created.Add(time.Duration(i) * time.Second)}, dependencies); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateTaskContext(ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{insightRequirementID}}}); err != nil {
			t.Fatal(err)
		}
		previous = id
	}
	members := &membershipFixture{workspaces: []core.Workspace{{ID: "demo"}}, roles: map[string]map[string]core.WorkspaceRole{"viewer": {"demo": core.WorkspaceRoleViewer}}}
	server := NewServer(st)
	server.Workspaces, server.Memberships = members, members
	server.Credentials = staticCredentialVerifier{"viewer": {ID: "pat_viewer", OwnerUserID: "viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	handler := server.Handler()

	request := httptest.NewRequest(http.MethodGet, insightPath(insightRequirementID, "progress")+"?workspace_id=demo", nil)
	request.Header.Set("Authorization", "Bearer viewer")
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	elapsed := time.Since(started)
	t.Logf("progress read for 200 serving tasks took %s", elapsed)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if elapsed >= time.Second {
		t.Fatalf("progress read took %s, want under one second (AC-3.7)", elapsed)
	}
	got := decodeInsight[insight.ProgressResult](t, response)
	openByActor := 0
	for _, count := range got.StatusMix.Open {
		openByActor += count.Count
	}
	if got.Total != 200 || got.Open != 200 || openByActor != 200 || len(got.Daily) == 0 || got.Daily[len(got.Daily)-1].Scope != 200 || len(got.Weekly) != 8 {
		t.Fatalf("incomplete progress=%+v", got)
	}
	if strings.Contains(response.Body.String(), insightBodySentinel) {
		t.Fatal("progress carried a task body")
	}
}
