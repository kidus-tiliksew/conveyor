package main

// conveyor preferences against a real test server over a volatile backend
// (req-stage-preferences AC-2.2, AC-2.3; component-stage-preferences
// invariants 4 and 6).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const preferencesTestToken = "preferences-cli-owner-token"

type preferencesCLIHarness struct {
	t      *testing.T
	st     store.Backend
	owner  core.IdentityUser
	url    string
	before func(*http.Request)
	mu     sync.Mutex
}

func newPreferencesCLIHarness(t *testing.T) *preferencesCLIHarness {
	t.Helper()
	isolateLocalAuthTest(t)
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	system := store.WithActor(t.Context(), store.SystemActor())
	for _, workspace := range []string{"alpha", "beta"} {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(system, workspace), &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapIdentity(system, config.FirstOperatorIdentity{OrganizationName: "Preferences", Email: "owner@example.test", DisplayName: "Owner"}, preferencesTestToken); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(system, preferencesTestToken)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	for _, workspace := range []string{"alpha", "beta"} {
		if _, err := st.GrantWorkspaceRole(ownerCtx, owner.Email, workspace, core.WorkspaceRoleOperator); err != nil {
			t.Fatal(err)
		}
	}
	server := httpapi.NewServer(st)
	server.Workspaces = st
	handler := server.Handler()
	h := &preferencesCLIHarness{t: t, st: st, owner: owner}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		before := h.before
		h.mu.Unlock()
		if before != nil {
			before(r)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	h.url = httpServer.URL
	t.Setenv("CONVEYOR_ADDR", httpServer.URL)
	t.Setenv("CONVEYOR_API_TOKEN", preferencesTestToken)
	t.Setenv("CONVEYOR_WORKSPACE", "alpha")
	return h
}

func (h *preferencesCLIHarness) run(args ...string) (string, error) {
	h.t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"preferences"}, args...))
	err := root.Execute()
	return out.String(), err
}

func (h *preferencesCLIHarness) mustRun(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("conveyor preferences %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (h *preferencesCLIHarness) runJSON(args ...string) store.StagePreferences {
	h.t.Helper()
	var set store.StagePreferences
	if err := json.Unmarshal([]byte(h.mustRun(append(args, "--json")...)), &set); err != nil {
		h.t.Fatal(err)
	}
	return set
}

// mcpGet reads the set through the MCP tool on the same server.
func (h *preferencesCLIHarness) mcp(tool string, args map[string]any) store.StagePreferences {
	h.t.Helper()
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	request, err := http.NewRequest(http.MethodPost, h.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+preferencesTestToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		h.t.Fatal(err)
	}
	if envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		h.t.Fatalf("%s: %+v", tool, envelope.Result)
	}
	var set store.StagePreferences
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &set); err != nil {
		h.t.Fatal(err)
	}
	return set
}

func TestPreferencesShowNeverSaved(t *testing.T) {
	h := newPreferencesCLIHarness(t)
	out := h.mustRun("show")
	for _, want := range []string{"never saved", "apply in every workspace", "plan       not set", "implement  not set", "verify     not set", "review     not set"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output lacks %q:\n%s", want, out)
		}
	}
	if set := h.runJSON("show"); set.Version != 0 || set.Stages != (store.StagePreferenceStages{}) {
		t.Fatalf("show --json = %+v", set)
	}
}

// TestPreferencesSetAndClearRoundTrip saves through the CLI and reads through
// MCP, and the reverse, and covers the review seat operations.
func TestPreferencesSetAndClearRoundTrip(t *testing.T) {
	h := newPreferencesCLIHarness(t)
	out := h.mustRun("set", "plan", "--harness", "codex", "--model", "gpt-6.1-sol", "--effort", "medium")
	if !strings.Contains(out, "version 1") || !strings.Contains(out, "plan       codex, model gpt-6.1-sol, effort medium") || !strings.Contains(out, "review     not set") {
		t.Fatalf("set plan printed:\n%s", out)
	}
	h.mustRun("set", "review", "--seat", "1", "--harness", "codex", "--model", "gpt-6.1-sol")
	h.mustRun("set", "review", "--seat", "2", "--harness", "claude-code", "--model", "claude-sonnet-5-5")
	h.mustRun("set", "review", "--seat", "3", "--harness", "cursor", "--model", "auto")
	out = h.mustRun("set", "review", "--harness", "claude-code", "--model", "claude-opus-5-5", "--effort", "high")
	for _, want := range []string{"review     primary: claude-code, model claude-opus-5-5, effort high", "seat 1: codex, model gpt-6.1-sol", "seat 3: cursor, model auto", "A seat without its own entry uses the primary."} {
		if !strings.Contains(out, want) {
			t.Fatalf("set review primary printed no %q:\n%s", want, out)
		}
	}
	// Replace seat 2, then remove seat 1 so later seats move up.
	h.mustRun("set", "review", "--seat", "2", "--harness", "opencode", "--model", "local")
	set := h.runJSON("clear", "review", "--seat", "1")
	if set.Version != 7 || len(set.Stages.Review.Seats) != 2 || set.Stages.Review.Seats[0].Harness != "opencode" || set.Stages.Review.Seats[1].Harness != "cursor" || set.Stages.Review.Primary == nil {
		t.Fatalf("clear seat 1 = %+v", set)
	}

	// MCP, from either workspace, reads exactly what the CLI saved.
	for _, workspace := range []string{"alpha", "beta"} {
		if read := h.mcp("get_stage_preferences", map[string]any{"workspace_id": workspace}); read.Version != set.Version || len(read.Stages.Review.Seats) != 2 || read.Stages.Plan == nil || read.Stages.Plan.Effort != "medium" {
			t.Fatalf("MCP read in %s = %+v, CLI saved %+v", workspace, read, set)
		}
	}
	// The CLI reads what MCP saved.
	h.mcp("set_stage_preference", map[string]any{"workspace_id": "beta", "stage": "verify", "value": map[string]any{"harness": "codex", "model": "gpt-6.1-sol", "effort": "low"}})
	if read := h.runJSON("show"); read.Version != set.Version+1 || read.Stages.Verify == nil || read.Stages.Verify.Effort != "low" || len(read.Stages.Review.Seats) != 2 {
		t.Fatalf("CLI read after MCP save = %+v", read)
	}

	h.mustRun("clear", "review", "--primary")
	h.mustRun("clear", "review", "--seat", "2")
	set = h.runJSON("clear", "review", "--seat", "1")
	if set.Stages.Review != nil {
		t.Fatalf("a review with no primary and no seats is still set: %+v", set.Stages.Review)
	}
	h.mustRun("set", "review", "--primary", "--harness", "codex", "--model", "m")
	set = h.runJSON("clear", "review")
	if set.Stages.Review != nil {
		t.Fatalf("clear review = %+v", set.Stages.Review)
	}
	out = h.mustRun("clear", "plan")
	if !strings.Contains(out, "plan       not set") || !strings.Contains(out, "verify     codex, model gpt-6.1-sol, effort low") {
		t.Fatalf("clear plan printed:\n%s", out)
	}
}

func TestPreferencesRefusals(t *testing.T) {
	h := newPreferencesCLIHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "triage", "--harness", "h", "--model", "m"}, "unknown stage"},
		{[]string{"set", "plan", "--seat", "1", "--harness", "h", "--model", "m"}, "apply only to review"},
		{[]string{"clear", "verify", "--primary"}, "apply only to review"},
		{[]string{"set", "review", "--primary", "--seat", "2", "--harness", "h", "--model", "m"}, "not both"},
		{[]string{"set", "review", "--seat", "9", "--harness", "h", "--model", "m"}, "from 1 to 8"},
		{[]string{"set", "review", "--seat", "0", "--harness", "h", "--model", "m"}, "from 1 to 8"},
		{[]string{"set", "review", "--seat", "2", "--harness", "h", "--model", "m"}, "set the seats in order"},
		{[]string{"clear", "review", "--seat", "1"}, "review seat 1 is not set"},
		{[]string{"set", "plan", "--model", "m"}, "harness"},
		{[]string{"set", "plan", "--harness", "   ", "--model", "m"}, "stage preferences refused: stages.plan.harness must not be blank"},
		{[]string{"set", "plan", "--harness", "h", "--model", strings.Repeat("m", 129)}, "stages.plan.model must be at most 128 bytes"},
		{[]string{"set", "implement", "--harness", "h", "--model", "m", "--effort", "a\tb"}, "stages.implement.effort must not hold a control character"},
	} {
		if _, err := h.run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("conveyor preferences %s: error = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
	if set := h.runJSON("show"); set.Version != 0 {
		t.Fatalf("refused commands saved a set: %+v", set)
	}
}

// TestPreferencesConflictRepeatsTheEditOnTheNewSet proves the CLI never
// drops a concurrent change: a write that lands between the CLI's read and
// its PUT makes the PUT stale, and the CLI repeats only its own edit on the
// fresh set (invariant 4; AC-1.7).
func TestPreferencesConflictRepeatsTheEditOnTheNewSet(t *testing.T) {
	h := newPreferencesCLIHarness(t)
	h.mustRun("set", "review", "--seat", "1", "--harness", "codex", "--model", "gpt-6.1-sol")
	concurrent := func(edit func(*store.StagePreferenceStages)) {
		_, err := h.st.UpdateStagePreferences(context.Background(), h.owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
			next := store.CloneStagePreferenceStages(current.Stages)
			edit(&next)
			return next, nil
		})
		if err != nil {
			t.Error(err)
		}
	}
	interfered := 0
	h.mu.Lock()
	h.before = func(r *http.Request) {
		if r.Method == http.MethodPut && interfered == 0 {
			interfered++
			concurrent(func(stages *store.StagePreferenceStages) {
				stages.Verify = &store.StagePreferenceEntry{Harness: "opencode", Model: "local"}
				stages.Review.Seats = append(stages.Review.Seats, store.StagePreferenceEntry{Harness: "concurrent", Model: "seat-2"})
			})
		}
	}
	h.mu.Unlock()
	set := h.runJSON("set", "review", "--seat", "1", "--harness", "claude-code", "--model", "claude-opus-5-5")
	if interfered != 1 {
		t.Fatalf("the concurrent write ran %d times", interfered)
	}
	if set.Stages.Verify == nil || len(set.Stages.Review.Seats) != 2 || set.Stages.Review.Seats[0].Harness != "claude-code" || set.Stages.Review.Seats[1].Harness != "concurrent" {
		t.Fatalf("the retried change lost a concurrent change or its own edit: %+v", set.Stages)
	}
	if set.Version != 3 {
		t.Fatalf("version = %d, want 3: the first write, the concurrent write, and the retried CLI write", set.Version)
	}

	// A set that keeps changing ends the command after its bounded attempts
	// without saving the CLI's edit.
	h.mu.Lock()
	h.before = func(r *http.Request) {
		if r.Method == http.MethodPut {
			concurrent(func(stages *store.StagePreferenceStages) {
				stages.Implement = &store.StagePreferenceEntry{Harness: "busy", Model: "busy"}
			})
		}
	}
	h.mu.Unlock()
	if _, err := h.run("set", "plan", "--harness", "codex", "--model", "m"); err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("contended set error = %v", err)
	}
	h.mu.Lock()
	h.before = nil
	h.mu.Unlock()
	if set := h.runJSON("show"); set.Stages.Plan != nil {
		t.Fatalf("a refused CLI edit was saved: %+v", set.Stages.Plan)
	}
}
