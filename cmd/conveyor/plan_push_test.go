package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// planPushFake is a minimal httptest control plane for the push tests. It
// records every request so a test can assert what was and was not called.
type planPushFake struct {
	t *testing.T

	mu                 sync.Mutex
	requests           []string
	confirmSeen        bool
	requirementCreates int
	referenceCreates   int
	taskBodies         []string

	requirementExists    map[string]bool
	requirementConfirmed map[string]bool
	decisions            []core.Decision
	designExists         map[string]bool
	designConfirmed      map[string]bool
	reposJSON            string
}

func newPlanPushFake(t *testing.T) *planPushFake {
	t.Helper()
	return &planPushFake{
		t:                    t,
		requirementExists:    map[string]bool{},
		requirementConfirmed: map[string]bool{},
		designExists:         map[string]bool{},
		designConfirmed:      map[string]bool{},
		reposJSON:            `[{"name":"conveyor","base":"main"}]`,
	}
}

func (f *planPushFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		path := strings.ToLower(r.URL.Path)
		if strings.Contains(path, "/confirm") || strings.Contains(path, "/dismiss") {
			f.confirmSeen = true
		}
		f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/requirements":
			io.WriteString(w, f.requirementsJSON())
		case r.Method == http.MethodGet && r.URL.Path == "/v1/decisions":
			data, _ := json.Marshal(f.decisions)
			w.Write(data)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/system-designs":
			io.WriteString(w, f.designsJSON())
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspace/config":
			io.WriteString(w, `{"document":{"repos":`+f.reposJSON+`}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/reference-documents":
			f.mu.Lock()
			f.referenceCreates++
			f.mu.Unlock()
			io.WriteString(w, `{"document":{"id":"ref-1"},"version":{"version":1}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/requirements":
			f.mu.Lock()
			f.requirementCreates++
			f.mu.Unlock()
			var payload struct {
				ID string `json:"id"`
			}
			json.Unmarshal(body, &payload)
			fmt.Fprintf(w, `{"requirement":{"id":%q},"version":{"version":1}}`, payload.ID)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/decisions":
			io.WriteString(w, `{"id":"DEC-3","status":"proposed"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/system-designs":
			io.WriteString(w, `{"document":{"id":"mechanism"},"version":{"version":1}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
			f.mu.Lock()
			f.taskBodies = append(f.taskBodies, string(body))
			f.mu.Unlock()
			io.WriteString(w, `{"id":"task-1"}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *planPushFake) requirementsJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return planPushDocumentListJSON(f.requirementExists, f.requirementConfirmed, "requirement")
}

func (f *planPushFake) designsJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return planPushDocumentListJSON(f.designExists, f.designConfirmed, "document")
}

func planPushDocumentListJSON(exists, confirmed map[string]bool, key string) string {
	ids := make([]string, 0, len(exists))
	for id := range exists {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		version := 0
		if confirmed[id] {
			version = 1
		}
		parts = append(parts, fmt.Sprintf(`{%q:{"id":%q,"current_version":%d}}`, key, id, version))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (f *planPushFake) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *planPushFake) posted(path string) bool {
	for _, request := range f.paths() {
		if request == http.MethodPost+" "+path {
			return true
		}
	}
	return false
}

// planPushTestEnv points newClient at the fake server and isolates stored
// credentials and CLI flag globals.
func planPushTestEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv("CONVEYOR_ADDR", serverURL)
	t.Setenv("CONVEYOR_API_TOKEN", "test-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldServer, oldWorkspace := serverFlag, workspaceFlag
	oldServerExplicit, oldWorkspaceExplicit := serverFlagExplicit, workspaceFlagExplicit
	serverFlag, workspaceFlag = "", ""
	serverFlagExplicit, workspaceFlagExplicit = false, false
	t.Cleanup(func() {
		serverFlag, workspaceFlag = oldServer, oldWorkspace
		serverFlagExplicit, workspaceFlagExplicit = oldServerExplicit, oldWorkspaceExplicit
	})
}

func planApproveDraft(t *testing.T, root string) {
	t.Helper()
	draftDir := filepath.Join(root, "draft")
	draft, err := loadPlanDraft(draftDir)
	if err != nil {
		t.Fatalf("load draft for approval: %v", err)
	}
	review := &planReviewFile{}
	for _, item := range draft.Items {
		review.Items = append(review.Items, planReviewEntry{ID: item.ID, Hash: item.Hash(), Verdict: "approve"})
	}
	if err := writePlanReview(draftDir, review); err != nil {
		t.Fatalf("write review: %v", err)
	}
}

func planPushRun(t *testing.T, dir string, options planPushOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runPlanPush(context.Background(), &out, &out, dir, options)
	return out.String(), err
}

func planPushLayer(t *testing.T, name string) planItemKind {
	t.Helper()
	kind, err := parsePlanPushLayer(name)
	if err != nil {
		t.Fatalf("parse layer %q: %v", name, err)
	}
	return kind
}

func newPlanPushServer(t *testing.T) (*planPushFake, string) {
	t.Helper()
	fake := newPlanPushFake(t)
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	planPushTestEnv(t, server.URL)
	return fake, server.URL
}

func TestPlanPushLayerFlagRejectsUnknownLayer(t *testing.T) {
	if _, err := parsePlanPushLayer("blueprints"); err == nil {
		t.Fatal("unknown layer must be rejected")
	}
	if _, err := parsePlanPushLayer(""); err == nil {
		t.Fatal("empty layer must be rejected")
	}
}

func TestPlanPushCmdRejectsUnknownLayer(t *testing.T) {
	command := planPushCmd()
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetArgs([]string{"--layer", "blueprints", t.TempDir()})
	if err := command.Execute(); err == nil {
		t.Fatal("unknown --layer must fail before contacting the server")
	}
}

func TestPlanPushDryRunPrintsPayloadsInLayerOrder(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true
	fake.requirementConfirmed["req-alpha"] = true
	fake.designExists["mechanism"] = true
	fake.designConfirmed["mechanism"] = true
	fake.decisions = []core.Decision{{ID: "DEC-3", Status: core.DecisionConfirmed}}
	// The decisions layer runs before designs; its mapping is what lets a
	// design citation resolve.
	if err := writePlanPushState(draftDir, &planPushState{Items: map[string]planPushStateItem{
		"D1": {Kind: "decision", Hash: "sha256:x", ServerID: "DEC-3"},
	}}); err != nil {
		t.Fatal(err)
	}

	var combined string
	for _, name := range []string{"requirements", "decisions", "designs", "tasks"} {
		out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, name), DryRun: true})
		if err != nil {
			t.Fatalf("dry run %s: %v\n%s", name, err, out)
		}
		combined += out
	}
	for _, want := range []string{"requirements brief.md", "requirements req-alpha", "decisions D1", "designs mechanism", "tasks T1"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, combined)
		}
	}
	previous := -1
	for _, marker := range []string{"requirements brief.md", "decisions D1", "designs mechanism", "tasks T1"} {
		index := strings.Index(combined, marker)
		if index <= previous {
			t.Fatalf("payloads not emitted in layer order at %q:\n%s", marker, combined)
		}
		previous = index
	}
	if fake.posted("/v1/requirements") || fake.posted("/v1/decisions") || fake.posted("/v1/tasks") {
		t.Fatalf("dry run posted: %v", fake.paths())
	}
	if fake.confirmSeen {
		t.Fatal("a confirm endpoint was called")
	}
}

func TestPlanPushRefusesLayerWithUnconfirmedRequirement(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true // exists, current_version 0

	out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "decisions"), DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "unconfirmed requirement") {
		t.Fatalf("expected unconfirmed-requirement refusal, got err=%v out=%q", err, out)
	}
	if fake.confirmSeen {
		t.Fatal("a confirm endpoint was called")
	}
}

func TestPlanPushRefusesLayerWithUnconfirmedDecision(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)
	if err := writePlanPushState(draftDir, &planPushState{Items: map[string]planPushStateItem{
		"D1": {Kind: "decision", Hash: "sha256:x", ServerID: "DEC-3"},
	}}); err != nil {
		t.Fatal(err)
	}

	fake, _ := newPlanPushServer(t)
	fake.decisions = []core.Decision{{ID: "DEC-3", Status: core.DecisionProposed}}

	out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "designs"), DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "unconfirmed decision DEC-3") {
		t.Fatalf("expected unconfirmed-decision refusal, got err=%v out=%q", err, out)
	}
}

func TestPlanPushRefusesLayerWithUnconfirmedDesign(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true
	fake.requirementConfirmed["req-alpha"] = true
	fake.designExists["mechanism"] = true // exists, current_version 0

	out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "tasks"), DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "unconfirmed design") {
		t.Fatalf("expected unconfirmed-design refusal, got err=%v out=%q", err, out)
	}
}

func TestPlanPushRefusesUnapprovedItem(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root, draftDir string)
		want    string
	}{
		{
			name:    "no review at all",
			prepare: func(t *testing.T, root, draftDir string) {},
			want:    "no review verdict",
		},
		{
			name: "changed content",
			prepare: func(t *testing.T, root, draftDir string) {
				planApproveDraft(t, root)
				review, err := readPlanReview(draftDir)
				if err != nil {
					t.Fatal(err)
				}
				review.Items[0].Hash = "sha256:stale"
				if err := writePlanReview(draftDir, review); err != nil {
					t.Fatal(err)
				}
			},
			want: "content changed since approval",
		},
		{
			name: "requested change",
			prepare: func(t *testing.T, root, draftDir string) {
				planApproveDraft(t, root)
				review, err := readPlanReview(draftDir)
				if err != nil {
					t.Fatal(err)
				}
				review.Items[0].Verdict = "request_change"
				if err := writePlanReview(draftDir, review); err != nil {
					t.Fatal(err)
				}
			},
			want: "not approve",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := planFixtureRepo(t, planValidDraftFiles())
			draftDir := filepath.Join(root, "draft")
			tc.prepare(t, root, draftDir)

			fake, _ := newPlanPushServer(t)
			fake.requirementExists["req-alpha"] = true
			fake.requirementConfirmed["req-alpha"] = true

			out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "requirements")})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected refusal %q, got err=%v out=%q", tc.want, err, out)
			}
			if fake.posted("/v1/requirements") {
				t.Fatal("refused push still posted a requirement")
			}
		})
	}
}

func TestPlanPushBlockingHookStopsRequirementsLayer(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)
	writePlanningConfigFile(t, root, planningConfigYAML(t, hookCommand(planHookTestBinary(t)), 30, true))
	t.Setenv("GO_WANT_PLAN_HOOK_HELPER", "1")
	t.Setenv(planHookHelperModeEnv, "fail")

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true
	fake.requirementConfirmed["req-alpha"] = true

	out, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "requirements")})
	if err == nil || !strings.Contains(err.Error(), "pre-push review hook") {
		t.Fatalf("expected hook failure to stop the layer, got err=%v out=%q", err, out)
	}
	if fake.posted("/v1/requirements") {
		t.Fatal("requirements posted despite a blocking hook failure")
	}
}

func TestPlanPushRequirementsAreIdempotentAcrossRuns(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	for run := range 2 {
		if _, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "requirements")}); err != nil {
			t.Fatalf("requirements run %d: %v", run+1, err)
		}
	}
	fake.mu.Lock()
	creates, references := fake.requirementCreates, fake.referenceCreates
	fake.mu.Unlock()
	if creates != 1 {
		t.Fatalf("requirement create count = %d, want 1", creates)
	}
	if references != 1 {
		t.Fatalf("reference create count = %d, want 1", references)
	}
	if fake.confirmSeen {
		t.Fatal("a confirm endpoint was called")
	}
}

func TestPlanPushDecisionsPersistServerMapping(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true
	fake.requirementConfirmed["req-alpha"] = true

	if _, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "decisions")}); err != nil {
		t.Fatalf("decisions push: %v", err)
	}
	state, err := readPlanPushState(draftDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Items["D1"].ServerID != "DEC-3" {
		t.Fatalf("decision mapping = %+v", state.Items["D1"])
	}
	if fake.confirmSeen {
		t.Fatal("a confirm endpoint was called")
	}
}

func TestPlanPushTasksFileWithContextAndDependencies(t *testing.T) {
	root := planFixtureRepo(t, map[string]string{
		"requirements/req-alpha.md": planValidRequirement,
		"decisions.yml":             planValidDecisions,
		"designs/mechanism.md":      planValidDesign,
		"tasks.yml": `- id: T1
  title: Implement alpha
  body: Do the work.
  depends_on: []
  governing:
    - req-alpha/REQ-1
    - mechanism
  docs_paths:
    - docs/alpha.md
  docs_none_reason: ""
`,
	})
	draftDir := filepath.Join(root, "draft")
	planApproveDraft(t, root)

	fake, _ := newPlanPushServer(t)
	fake.requirementExists["req-alpha"] = true
	fake.requirementConfirmed["req-alpha"] = true
	fake.designExists["mechanism"] = true
	fake.designConfirmed["mechanism"] = true

	if _, err := planPushRun(t, draftDir, planPushOptions{Layer: planPushLayer(t, "tasks")}); err != nil {
		t.Fatalf("tasks push: %v", err)
	}
	fake.mu.Lock()
	bodies := append([]string(nil), fake.taskBodies...)
	fake.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("task bodies = %v", bodies)
	}
	var payload planTaskPayload
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repo != "conveyor" || payload.BaseBranch != "main" {
		t.Fatalf("repo/base = %s/%s", payload.Repo, payload.BaseBranch)
	}
	if len(payload.RequirementIDs) != 1 || payload.RequirementIDs[0] != "req-alpha" {
		t.Fatalf("requirement_ids = %v", payload.RequirementIDs)
	}
	if len(payload.SystemDesignIDs) != 1 || payload.SystemDesignIDs[0] != "mechanism" {
		t.Fatalf("system_design_ids = %v", payload.SystemDesignIDs)
	}
	if !strings.Contains(payload.Body, "Implement alpha") || !strings.Contains(payload.Body, "docs/alpha.md") {
		t.Fatalf("task body = %q", payload.Body)
	}
	if fake.confirmSeen {
		t.Fatal("a confirm endpoint was called")
	}
}
