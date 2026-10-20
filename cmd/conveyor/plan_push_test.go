package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
)

// planFakeFactory is an httptest stand-in for the factory routes push uses
// (feature-local-planning-drafts LP-5). Its POST routes add a proposal or
// task on every call, as the real routes do. Any confirm, dismiss, or other
// unexpected request fails the test (AC-3.4).
type planFakeFactory struct {
	t      *testing.T
	mu     sync.Mutex
	server *httptest.Server

	requirements map[string]*planFakeRequirement
	designs      map[string]*planFakeDesign
	decisions    map[string]core.Decision
	tasks        map[string]core.Task
	taskKeys     map[string]string
	taskRequests map[string]string
	nextTask     int

	// seeded names fixtures confirmAll leaves alone.
	seeded map[string]bool

	posts []planFakePost
	// refuse answers a POST path with the status and message.
	refuse map[string]planFakeRefusal
	// refusePost refuses one POST by its body.
	refusePost func(path string, body map[string]any) (planFakeRefusal, bool)
	// dropAnswer applies a POST to path and then closes the connection
	// without an answer, once.
	dropAnswer map[string]bool
	// beforeDecisionPost runs before a decision POST is applied.
	beforeDecisionPost func(id string)
}

type planFakeRequirement struct {
	doc      core.Requirement
	versions []core.RequirementVersion
}

type planFakeDesign struct {
	doc      core.SystemDesign
	versions []core.SystemDesignVersion
}

type planFakePost struct {
	Path      string
	Body      map[string]any
	FormNames []string
	Files     int
	IntakeKey string
}

type planFakeRefusal struct {
	status  int
	message string
}

func newPlanFakeFactory(t *testing.T) *planFakeFactory {
	t.Helper()
	f := &planFakeFactory{
		t:            t,
		requirements: map[string]*planFakeRequirement{},
		designs:      map[string]*planFakeDesign{},
		decisions:    map[string]core.Decision{},
		tasks:        map[string]core.Task{},
		taskKeys:     map[string]string{},
		taskRequests: map[string]string{},
		refuse:       map[string]planFakeRefusal{},
		dropAnswer:   map[string]bool{},
		seeded:       map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if planForbiddenRoute(r.URL.Path) {
			t.Errorf("push called a confirm or dismiss route: %s %s", r.Method, r.URL.Path)
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		http.Error(w, "unexpected request", http.StatusTeapot)
	})
	mux.HandleFunc("GET /v1/requirements", f.listRequirements)
	mux.HandleFunc("POST /v1/requirements", f.createRequirement)
	mux.HandleFunc("GET /v1/requirements/{id}", f.getRequirement)
	mux.HandleFunc("GET /v1/requirements/{id}/versions", f.listRequirementVersions)
	mux.HandleFunc("POST /v1/requirements/{id}/versions", f.proposeRequirementVersion)
	mux.HandleFunc("GET /v1/system-designs", f.listDesigns)
	mux.HandleFunc("POST /v1/system-designs", f.createDesign)
	mux.HandleFunc("GET /v1/system-designs/{id}", f.getDesign)
	mux.HandleFunc("GET /v1/system-designs/{id}/versions", f.listDesignVersions)
	mux.HandleFunc("POST /v1/system-designs/{id}/versions", f.proposeDesignVersion)
	mux.HandleFunc("GET /v1/decisions", f.listDecisions)
	mux.HandleFunc("POST /v1/decisions", f.proposeDecision)
	mux.HandleFunc("GET /v1/decisions/{id}", f.getDecision)
	mux.HandleFunc("POST /v1/tasks", f.createTask)
	mux.HandleFunc("GET /v1/tasks/{id}", f.getTask)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Workspace-ID") != "demo" || r.Header.Get("Authorization") != "Bearer plan-token" {
			t.Errorf("%s %s: workspace %q, authorization %q", r.Method, r.URL.Path, r.Header.Get("X-Workspace-ID"), r.Header.Get("Authorization"))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CONVEYOR_ADDR", f.server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "plan-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	return f
}

func planFakeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// recordPost decodes a JSON POST body and records it. It reports false when
// the request was refused or its answer dropped by test configuration.
func (f *planFakeFactory) recordPost(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("POST %s: decode body: %v", r.URL.Path, err)
	}
	f.posts = append(f.posts, planFakePost{Path: r.URL.Path, Body: body})
	if refusal, ok := f.refusal(r.URL.Path, body); ok {
		http.Error(w, refusal.message, refusal.status)
		return nil, false
	}
	return body, true
}

func (f *planFakeFactory) refusal(path string, body map[string]any) (planFakeRefusal, bool) {
	if refusal, ok := f.refuse[path]; ok {
		return refusal, true
	}
	if f.refusePost != nil {
		return f.refusePost(path, body)
	}
	return planFakeRefusal{}, false
}

// answer writes value, or applies the drop configured for the path.
func (f *planFakeFactory) answer(w http.ResponseWriter, r *http.Request, status int, value any) {
	if f.dropAnswer[r.URL.Path] {
		delete(f.dropAnswer, r.URL.Path)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			f.t.Fatal(err)
		}
		_ = connection.Close()
		return
	}
	planFakeJSON(w, status, value)
}

func (f *planFakeFactory) listRequirements(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("include_archived") != "true" {
		f.t.Errorf("requirement listing omits archived documents: %s", r.URL)
	}
	type summary struct {
		Requirement core.Requirement `json:"requirement"`
	}
	result := []summary{}
	for _, id := range planSortedKeys(f.requirements) {
		result = append(result, summary{f.requirements[id].doc})
	}
	planFakeJSON(w, http.StatusOK, result)
}

func (f *planFakeFactory) getRequirement(w http.ResponseWriter, r *http.Request) {
	requirement, ok := f.requirements[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, map[string]any{"requirement": requirement.doc})
}

func (f *planFakeFactory) listRequirementVersions(w http.ResponseWriter, r *http.Request) {
	requirement, ok := f.requirements[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, requirement.versions)
}

func (f *planFakeFactory) newRequirementVersion(id string, number int, content string) (core.RequirementVersion, error) {
	document, err := pipeline.ParseRequirementDocument(content)
	if err != nil {
		return core.RequirementVersion{}, err
	}
	return core.RequirementVersion{RequirementID: id, Version: number, Content: document.Markdown, Statements: document.Statements, Origin: core.RequirementOriginOperator, Workspace: "demo"}, nil
}

func (f *planFakeFactory) createRequirement(w http.ResponseWriter, r *http.Request) {
	body, ok := f.recordPost(w, r)
	if !ok {
		return
	}
	id, _ := body["id"].(string)
	title, _ := body["title"].(string)
	content, _ := body["content"].(string)
	if _, held := f.requirements[id]; held {
		http.Error(w, "requirement "+id+" already exists", http.StatusConflict)
		return
	}
	version, err := f.newRequirementVersion(id, 1, content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	doc := core.Requirement{ID: id, Title: title, Slug: core.RequirementSlug(title), StatementHighWaterMark: core.RequirementStatementHighWaterMark(version.Statements), Workspace: "demo"}
	f.requirements[id] = &planFakeRequirement{doc: doc, versions: []core.RequirementVersion{version}}
	f.answer(w, r, http.StatusCreated, map[string]any{"requirement": doc, "version": version})
}

func (f *planFakeFactory) proposeRequirementVersion(w http.ResponseWriter, r *http.Request) {
	body, ok := f.recordPost(w, r)
	if !ok {
		return
	}
	requirement, held := f.requirements[r.PathValue("id")]
	if !held {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if requirement.doc.Archived {
		http.Error(w, "requirement is archived", http.StatusConflict)
		return
	}
	content, _ := body["content"].(string)
	version, err := f.newRequirementVersion(requirement.doc.ID, len(requirement.versions)+1, content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	requirement.versions = append(requirement.versions, version)
	requirement.doc.StatementHighWaterMark = max(requirement.doc.StatementHighWaterMark, core.RequirementStatementHighWaterMark(version.Statements))
	f.answer(w, r, http.StatusCreated, version)
}

func (f *planFakeFactory) listDesigns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("include_archived") != "true" {
		f.t.Errorf("system design listing omits archived documents: %s", r.URL)
	}
	type summary struct {
		Document core.SystemDesign `json:"document"`
	}
	result := []summary{}
	for _, id := range planSortedKeys(f.designs) {
		result = append(result, summary{f.designs[id].doc})
	}
	planFakeJSON(w, http.StatusOK, result)
}

func (f *planFakeFactory) getDesign(w http.ResponseWriter, r *http.Request) {
	design, ok := f.designs[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, map[string]any{"document": design.doc, "versions": design.versions})
}

func (f *planFakeFactory) listDesignVersions(w http.ResponseWriter, r *http.Request) {
	design, ok := f.designs[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, design.versions)
}

func (f *planFakeFactory) createDesign(w http.ResponseWriter, r *http.Request) {
	body, ok := f.recordPost(w, r)
	if !ok {
		return
	}
	id, _ := body["id"].(string)
	title, _ := body["title"].(string)
	category, _ := body["category"].(string)
	content, _ := body["content"].(string)
	if _, held := f.designs[id]; held {
		http.Error(w, "system design id conflict", http.StatusConflict)
		return
	}
	doc := core.SystemDesign{ID: id, Title: title, Slug: core.RequirementSlug(title), Category: category, Workspace: "demo"}
	version := core.SystemDesignVersion{DocumentID: id, Version: 1, Content: core.NormalizeSystemDesignContent(content), Origin: core.SystemDesignOriginOperator, Workspace: "demo"}
	f.designs[id] = &planFakeDesign{doc: doc, versions: []core.SystemDesignVersion{version}}
	f.answer(w, r, http.StatusCreated, map[string]any{"document": doc, "version": version})
}

func (f *planFakeFactory) proposeDesignVersion(w http.ResponseWriter, r *http.Request) {
	body, ok := f.recordPost(w, r)
	if !ok {
		return
	}
	design, held := f.designs[r.PathValue("id")]
	if !held {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	content, _ := body["content"].(string)
	version := core.SystemDesignVersion{DocumentID: design.doc.ID, Version: len(design.versions) + 1, Content: core.NormalizeSystemDesignContent(content), Origin: core.SystemDesignOriginOperator, Workspace: "demo"}
	design.versions = append(design.versions, version)
	f.answer(w, r, http.StatusCreated, version)
}

func (f *planFakeFactory) listDecisions(w http.ResponseWriter, r *http.Request) {
	result := []core.Decision{}
	for _, id := range planSortedKeys(f.decisions) {
		result = append(result, f.decisions[id])
	}
	planFakeJSON(w, http.StatusOK, result)
}

func (f *planFakeFactory) getDecision(w http.ResponseWriter, r *http.Request) {
	decision, ok := f.decisions[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, decision)
}

func (f *planFakeFactory) proposeDecision(w http.ResponseWriter, r *http.Request) {
	body, ok := f.recordPost(w, r)
	if !ok {
		return
	}
	id, _ := body["id"].(string)
	if f.beforeDecisionPost != nil {
		f.beforeDecisionPost(id)
	}
	if _, held := f.decisions[id]; held || id == "" {
		http.Error(w, "decision id conflict: "+id, http.StatusConflict)
		return
	}
	text := func(name string) string { value, _ := body[name].(string); return value }
	decision := core.Decision{ID: id, Statement: text("statement"), Context: text("context"), AlternativesRejected: text("alternatives_rejected"), Supersedes: text("supersedes"), Status: core.DecisionProposed, Origin: core.DecisionOriginOperator, Workspace: "demo"}
	f.decisions[id] = decision
	f.answer(w, r, http.StatusCreated, decision)
}

func (f *planFakeFactory) createTask(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		f.t.Errorf("task posted as %q; push must use multipart intake", r.Header.Get("Content-Type"))
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		f.t.Errorf("parse task form: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var names []string
	for name := range r.MultipartForm.Value {
		names = append(names, name)
	}
	sort.Strings(names)
	files := 0
	for _, headers := range r.MultipartForm.File {
		files += len(headers)
	}
	raw := r.FormValue("task")
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		f.t.Errorf("decode task part: %v", err)
	}
	key := r.FormValue("idempotency_key")
	f.posts = append(f.posts, planFakePost{Path: r.URL.Path, Body: body, FormNames: names, Files: files, IntakeKey: key})
	if refusal, ok := f.refusal(r.URL.Path, body); ok {
		http.Error(w, refusal.message, refusal.status)
		return
	}
	canonical, _ := json.Marshal(body)
	if id, ok := f.taskKeys[key]; ok {
		if f.taskRequests[id] != string(canonical) {
			http.Error(w, "idempotency_key is already used by a different task", http.StatusConflict)
			return
		}
		f.answer(w, r, http.StatusOK, f.tasks[id])
		return
	}
	f.nextTask++
	text := func(name string) string { value, _ := body[name].(string); return value }
	task := core.Task{ID: fmt.Sprintf("261020-t%03d", f.nextTask), Body: text("body"), Repo: text("repo"), State: "queued", Workspace: "demo"}
	f.tasks[task.ID], f.taskKeys[key], f.taskRequests[task.ID] = task, task.ID, string(canonical)
	f.answer(w, r, http.StatusCreated, task)
}

func (f *planFakeFactory) getTask(w http.ResponseWriter, r *http.Request) {
	task, ok := f.tasks[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	planFakeJSON(w, http.StatusOK, task)
}

// Operator acts, applied directly to the fake; push never sends them.

func (f *planFakeFactory) confirmAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, requirement := range f.requirements {
		if f.seeded[id] {
			continue
		}
		for index := range requirement.versions {
			if version := &requirement.versions[index]; !version.Confirmed && !version.Retired {
				version.Confirmed, requirement.doc.CurrentVersion = true, version.Version
			}
		}
	}
	for id, design := range f.designs {
		if f.seeded[id] {
			continue
		}
		for index := range design.versions {
			if version := &design.versions[index]; !version.Confirmed && !version.Dismissed {
				version.Confirmed, design.doc.CurrentVersion = true, version.Version
			}
		}
	}
	for id, decision := range f.decisions {
		if decision.Status == core.DecisionProposed && !f.seeded[id] {
			decision.Status = core.DecisionConfirmed
			f.decisions[id] = decision
		}
	}
}

func (f *planFakeFactory) dismissRequirement(id string, number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requirement := f.requirements[id]
	requirement.versions[number-1].Retired = true
	if requirement.doc.CurrentVersion == 0 {
		requirement.doc.Archived, requirement.doc.ArchiveReason = true, core.ArchiveReasonOnlyProposalDismissed
	}
}

func (f *planFakeFactory) dismissDesign(id string, number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	design := f.designs[id]
	design.versions[number-1].Dismissed = true
	if design.doc.CurrentVersion == 0 {
		design.doc.Archived, design.doc.ArchiveReason = true, core.ArchiveReasonOnlyProposalDismissed
	}
}

func (f *planFakeFactory) setDecisionStatus(id string, status core.DecisionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	decision, held := f.decisions[id]
	if !held {
		f.seeded[id] = true
	}
	decision.ID, decision.Status = id, status
	if decision.Statement == "" {
		decision.Statement, decision.Context, decision.AlternativesRejected = "Seeded "+id+".", "Seeded context.", "None."
	}
	f.decisions[id] = decision
}

// seedRequirement stores versions from contents; versions up to current are
// confirmed.
func (f *planFakeFactory) seedRequirement(t *testing.T, id, title string, current, highWater int, contents ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	requirement := &planFakeRequirement{doc: core.Requirement{ID: id, Title: title, Slug: core.RequirementSlug(title), CurrentVersion: current, StatementHighWaterMark: highWater, Workspace: "demo"}}
	for index, content := range contents {
		version, err := f.newRequirementVersion(id, index+1, content)
		if err != nil {
			t.Fatal(err)
		}
		version.Confirmed = index+1 <= current
		requirement.versions = append(requirement.versions, version)
	}
	f.requirements[id], f.seeded[id] = requirement, true
}

func (f *planFakeFactory) seedDesign(id, title string, current int, contents ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	design := &planFakeDesign{doc: core.SystemDesign{ID: id, Title: title, Slug: core.RequirementSlug(title), Category: "Feature design", CurrentVersion: current, Workspace: "demo"}}
	for index, content := range contents {
		design.versions = append(design.versions, core.SystemDesignVersion{DocumentID: id, Version: index + 1, Content: core.NormalizeSystemDesignContent(content), Confirmed: index+1 <= current, Workspace: "demo"})
	}
	f.designs[id], f.seeded[id] = design, true
}

func (f *planFakeFactory) postsTo(prefix string) []planFakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []planFakePost
	for _, post := range f.posts {
		if strings.HasPrefix(post.Path, prefix) {
			found = append(found, post)
		}
	}
	return found
}

func (f *planFakeFactory) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func pushPlan(t *testing.T, root, layer string, extra ...string) (string, error) {
	t.Helper()
	return runPlanCommand(t, append([]string{"push", root, "--layer", layer}, extra...)...)
}

func mustPushPlan(t *testing.T, root, layer string) string {
	t.Helper()
	out, err := pushPlan(t, root, layer)
	if err != nil {
		t.Fatalf("push %s: %v\n%s", layer, err, out)
	}
	return out
}

// pushPlanThrough pushes and confirms every layer before layer.
func pushPlanThrough(t *testing.T, f *planFakeFactory, root, layer string) {
	t.Helper()
	for _, earlier := range PlanLayers[:planLayerRank(layer)] {
		mustPushPlan(t, root, earlier)
		f.confirmAll()
	}
}

func planPushRecord(t *testing.T, root, itemID string) PlanPushRecord {
	t.Helper()
	state, err := LoadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Pushes[itemID]
}

func readPlanStateBytes(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(planStatePath(root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return data
}

// failPlanStateSaveAt fails the nth state replacement from now; the
// returned function restores the writer.
func failPlanStateSaveAt(t *testing.T, n int) func() {
	t.Helper()
	previous := planStateWriter.rename
	calls := 0
	planStateWriter.rename = func(oldPath, newPath string) error {
		calls++
		if calls == n {
			return errors.New("injected state write failure")
		}
		return previous(oldPath, newPath)
	}
	restore := func() { planStateWriter.rename = previous }
	t.Cleanup(restore)
	return restore
}

func expectPlanLine(t *testing.T, out, prefix string, fragments ...string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				matched = false
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("no line %q containing %q in:\n%s", prefix, fragments, out)
}

// expectPlanRefusal asserts the push failed, printed the refusal, and
// posted nothing.
func expectPlanRefusal(t *testing.T, f *planFakeFactory, root, layer, prefix string, fragments ...string) string {
	t.Helper()
	before := f.postCount()
	out, err := pushPlan(t, root, layer)
	if err == nil {
		t.Fatalf("push %s succeeded; want refusal %q\n%s", layer, prefix, out)
	}
	expectPlanLine(t, out, prefix, fragments...)
	if after := f.postCount(); after != before {
		t.Fatalf("refused push posted %d request(s)", after-before)
	}
	return out
}

// newPlanDraftDir returns an empty draft directory outside any git working
// tree.
func newPlanDraftDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	isolatePlanGit(t, base)
	return filepath.Join(base, "draft")
}

func planRequirementText(title string, criteria ...string) string {
	var body strings.Builder
	fmt.Fprintf(&body, "# %s\n\nThe %s keeps drafts local.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The CLI shall check drafts.\n  acceptance_criteria:\n", title, strings.ToLower(title))
	for _, id := range criteria {
		fmt.Fprintf(&body, "    - id: %s\n      statement: When %s runs, the CLI shall pass.\n", id, id)
	}
	body.WriteString("```\n")
	return body.String()
}

const planDesignText = "# Revised design\n\nThe design text.\n\n```conveyor:governs\n- repo: elsewhere\n  paths:\n    - src/**\n```\n"

// REQ-3, AC-3.5, AC-3.7, AC-3.9: the layers post in order as unconfirmed
// proposals and tasks; each layer waits for the confirmation of the earlier
// ones; draft-local IDs become server IDs; cited items post first.
func TestPlanPushLayersInOrder(t *testing.T) {
	f := newPlanFakeFactory(t)
	f.setDecisionStatus("DEC-80", core.DecisionConfirmed)
	root := newPlanFixture(t)
	writePlanFile(t, root, "decisions/a-follow.yaml", "statement: Follow draft:dec/use-cli for req-demo REQ-1.\ncontext: req-demo AC-1.1 needs it.\nalternatives_rejected: None.\n")
	writePlanFile(t, root, "tasks/a-docs.md", "---\nrepo: conveyor\ndepends_on: [draft:task/build]\n---\nDocument req-demo AC-1.1 after draft:task/build.\n")
	approveAllPlanItems(t, root)

	expectPlanRefusal(t, f, root, PlanLayerDecisions, "req-demo: AC-3.8:", "unpushed")
	out := mustPushPlan(t, root, PlanLayerRequirements)
	expectPlanLine(t, out, "posted req-demo:", "req-demo v1 (pending)")
	expectPlanRefusal(t, f, root, PlanLayerDecisions, "req-demo: AC-3.8:", "req-demo v1 is pending")
	f.confirmAll()

	out = mustPushPlan(t, root, PlanLayerDecisions)
	decisions := f.postsTo("/v1/decisions")
	if len(decisions) != 2 || decisions[0].Body["id"] != "DEC-81" || decisions[1].Body["id"] != "DEC-82" {
		t.Fatalf("decision posts = %+v", decisions)
	}
	if !strings.Contains(decisions[1].Body["statement"].(string), "Follow DEC-81 for") {
		t.Fatalf("draft:dec/a-follow was posted before the decision it cites, or kept the draft-local ID: %v", decisions[1].Body)
	}
	expectPlanLine(t, out, "posted draft:dec/use-cli:", "DEC-81")
	expectPlanLine(t, out, "posted draft:dec/a-follow:", "DEC-82")
	expectPlanRefusal(t, f, root, PlanLayerSystemDesigns, "draft:dec/use-cli: AC-3.8:", "DEC-81 is pending")
	f.confirmAll()

	mustPushPlan(t, root, PlanLayerSystemDesigns)
	design := f.postsTo("/v1/system-designs")[0]
	if want := []string{"id", "title", "category", "content"}; !reflect.DeepEqual(planSortedKeys(design.Body), planSorted(want)) {
		t.Fatalf("system design body keys = %v", planSortedKeys(design.Body))
	}
	if content := design.Body["content"].(string); !strings.Contains(content, "under DEC-81.") || strings.Contains(content, "draft:") {
		t.Fatalf("system design content = %q", content)
	}
	expectPlanRefusal(t, f, root, PlanLayerTasks, "feature-demo: AC-3.8:", "feature-demo v1 is pending")
	f.confirmAll()

	out = mustPushPlan(t, root, PlanLayerTasks)
	tasks := f.postsTo("/v1/tasks")
	if len(tasks) != 3 {
		t.Fatalf("task posts = %+v", tasks)
	}
	order := []string{}
	for _, post := range tasks {
		order = append(order, post.IntakeKey[strings.Index(post.IntakeKey, "draft:task/"):strings.LastIndex(post.IntakeKey, ":sha256")])
	}
	if strings.Join(order, ",") != "draft:task/build,draft:task/a-docs,draft:task/docs" {
		t.Fatalf("task post order = %v", order)
	}
	build := tasks[0].Body
	if build["repo"] != "conveyor" || !reflect.DeepEqual(build["requirement_ids"], []any{"req-demo"}) || !reflect.DeepEqual(build["system_design_ids"], []any{"feature-demo"}) {
		t.Fatalf("build task body = %v", build)
	}
	if !strings.Contains(build["body"].(string), "following DEC-81.") {
		t.Fatalf("build task body keeps draft-local IDs: %q", build["body"])
	}
	for _, gate := range []string{"spec_approval", "merge_approval"} {
		if _, ok := build[gate]; ok {
			t.Fatalf("task states no %s but the request carries %v", gate, build[gate])
		}
	}
	buildID := planPushRecord(t, root, "draft:task/build").ServerID
	for _, post := range tasks[1:] {
		if !reflect.DeepEqual(post.Body["depends_on"], []any{buildID}) {
			t.Fatalf("dependent task depends_on = %v, want [%s]", post.Body["depends_on"], buildID)
		}
		if !strings.Contains(post.Body["body"].(string), "after "+buildID+".") {
			t.Fatalf("dependent task body = %q", post.Body["body"])
		}
	}
	expectPlanLine(t, out, "posted draft:task/build:", "task "+buildID+" (queued)")
}

func planSorted(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

// AC-3.1: a pending or changed item refuses the layer and posts nothing.
func TestPlanPushRequiresCurrentApproval(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-demo: AC-3.1:", "pending approval")
	approveAllPlanItems(t, root)
	writePlanFile(t, root, "requirements/req-demo.md", strings.Replace(planFixtureRequirement, "keeps drafts local", "keeps every draft local", 1))
	expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-demo: AC-3.1:", "changed since approval")
}

// AC-3.8, AC-3.14: a pending or dismissed earlier-layer item refuses the
// layer; removing a dismissed item stops the block, and the offline check
// fails every remaining item that cites it.
func TestPlanPushEarlierLayerStates(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	writePlanFile(t, root, "decisions/extra.yaml", "statement: Keep req-demo REQ-1 small.\ncontext: req-demo AC-1.1 is narrow.\nalternatives_rejected: None.\n")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerDecisions)
	mustPushPlan(t, root, PlanLayerDecisions)
	extra := planPushRecord(t, root, "draft:dec/extra").ServerID
	useCLI := planPushRecord(t, root, "draft:dec/use-cli").ServerID
	f.setDecisionStatus(useCLI, core.DecisionConfirmed)
	f.setDecisionStatus(extra, core.DecisionDismissed)
	expectPlanRefusal(t, f, root, PlanLayerSystemDesigns, "draft:dec/extra: AC-3.8:", extra+" is dismissed")

	if err := os.Remove(filepath.Join(root, "decisions", "extra.yaml")); err != nil {
		t.Fatal(err)
	}
	out := mustPushPlan(t, root, PlanLayerSystemDesigns)
	expectPlanLine(t, out, "posted feature-demo:", "feature-demo v1")

	f.setDecisionStatus(useCLI, core.DecisionDismissed)
	if err := os.Remove(filepath.Join(root, "decisions", "use-cli.yaml")); err != nil {
		t.Fatal(err)
	}
	out = expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/build: AC-1.6:", "draft:dec/use-cli")
	expectPlanLine(t, out, "feature-demo: AC-1.6:", "draft:dec/use-cli")
}

// AC-3.2, AC-3.15: unconfirmed citations outside the layer and an
// unconfirmed supersedes target refuse the layer; a confirmed supersedes
// target is carried.
func TestPlanPushCitationsConfirmed(t *testing.T) {
	cases := []struct {
		name, layer, rel, content, prefix string
		fragments                         []string
	}{
		{"unheld requirement", PlanLayerRequirements, "requirements/req-demo.md", strings.Replace(planFixtureRequirement, "keeps drafts local.", "keeps drafts local, as req-missing REQ-1 says.", 1), "req-demo: AC-3.2:", []string{"req-missing", "does not hold"}},
		{"proposed decision", PlanLayerDecisions, "decisions/use-cli.yaml", "statement: Check drafts as req-demo REQ-1 and DEC-7 require.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n", "draft:dec/use-cli: AC-3.2:", []string{"DEC-7", "proposed"}},
		{"superseded decision", PlanLayerDecisions, "decisions/use-cli.yaml", "statement: Check drafts as req-demo REQ-1 and DEC-6 require.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n", "draft:dec/use-cli: AC-3.2:", []string{"DEC-6", "superseded"}},
		{"unheld decision", PlanLayerDecisions, "decisions/use-cli.yaml", "statement: Check drafts as req-demo REQ-1 and DEC-99 require.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n", "draft:dec/use-cli: AC-3.2:", []string{"DEC-99", "does not hold"}},
		{"unconfirmed supersedes", PlanLayerDecisions, "decisions/use-cli.yaml", planFixtureDecision + "supersedes: DEC-7\n", "draft:dec/use-cli: AC-3.15:", []string{"DEC-7", "proposed"}},
		{"unheld design", PlanLayerTasks, "tasks/build.md", "---\nrepo: conveyor\nsystem_design_ids: [feature-demo, component-missing]\n---\nImplement req-demo AC-1.1 following draft:dec/use-cli.\n", "draft:task/build: AC-3.2:", []string{"component-missing", "does not hold"}},
		{"unconfirmed design", PlanLayerTasks, "tasks/build.md", "---\nrepo: conveyor\n---\nImplement req-demo AC-1.1 as component-pending and draft:dec/use-cli describe.\n", "draft:task/build: AC-3.2:", []string{"component-pending", "no confirmed version"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			f.setDecisionStatus("DEC-6", core.DecisionSuperseded)
			f.setDecisionStatus("DEC-7", core.DecisionProposed)
			f.seedDesign("component-pending", "Pending component", 0, planDesignText)
			root := newPlanFixture(t)
			writePlanFile(t, root, tc.rel, tc.content)
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, tc.layer)
			expectPlanRefusal(t, f, root, tc.layer, tc.prefix, tc.fragments...)
		})
	}

	f := newPlanFakeFactory(t)
	f.setDecisionStatus("DEC-8", core.DecisionConfirmed)
	root := newPlanFixture(t)
	writePlanFile(t, root, "decisions/use-cli.yaml", planFixtureDecision+"supersedes: DEC-8\n")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerDecisions)
	mustPushPlan(t, root, PlanLayerDecisions)
	if posted := f.postsTo("/v1/decisions")[0]; posted.Body["supersedes"] != "DEC-8" || posted.Body["id"] != "DEC-9" {
		t.Fatalf("superseding decision body = %v", posted.Body)
	}
}

// AC-3.11, AC-3.12: stale or unequal bases, reused identifiers across every
// version, held IDs and slugs, and missing or archived revised documents
// refuse the layer and post nothing.
func TestPlanPushRevisionRules(t *testing.T) {
	v1 := planRequirementText("Revised capability", "AC-1.1", "AC-1.3")
	v2 := planRequirementText("Revised capability", "AC-1.1")
	revise := func(base int, content string) string {
		return fmt.Sprintf("---\nmode: revise\nbase_version: %d\n---\n%s", base, content)
	}
	cases := []struct {
		name, rel, content, base, prefix string
		fragments                        []string
	}{
		{"stale base version", "requirements/req-rev.md", revise(1, planRequirementText("Revised capability", "AC-1.1", "AC-1.4")), v1, "req-rev: AC-3.11:", []string{"base version is v1", "current confirmed version is v2"}},
		{"base copy differs", "requirements/req-rev.md", revise(2, planRequirementText("Revised capability", "AC-1.1", "AC-1.4")), strings.Replace(v2, "keeps drafts", "keeps all drafts", 1), "req-rev: AC-3.11:", []string{"v2 differs from requirements/req-rev.base.md"}},
		{"criterion reused from an older version", "requirements/req-rev.md", revise(2, planRequirementText("Revised capability", "AC-1.1", "AC-1.2")), v2, "req-rev: AC-3.12:", []string{"AC-1.2 reuses a retired identifier", "AC-1.3"}},
		{"missing document", "requirements/req-none.md", revise(1, v2), v2, "req-none: AC-3.12:", []string{"does not hold"}},
		{"archived document", "requirements/req-arch.md", revise(1, v2), v2, "req-arch: AC-3.12:", []string{"is archived"}},
		{"held ID", "requirements/req-arch.md", "---\nmode: create\n---\n" + planRequirementText("Fresh capability", "AC-1.1"), "", "req-arch: AC-3.12:", []string{"already holds that ID"}},
		{"held slug", "requirements/req-fresh.md", "---\nmode: create\n---\n" + planRequirementText("Archived capability", "AC-1.1"), "", "req-fresh: AC-3.12:", []string{`"archived-capability"`, "req-arch"}},
		{"held design slug", "system-designs/feature-fresh.md", "---\nmode: create\ntitle: Pending component\ncategory: Feature design\n---\n" + planDesignText, "", "feature-fresh: AC-3.12:", []string{`"pending-component"`, "component-pending"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			f.seedRequirement(t, "req-rev", "Revised capability", 2, 1, v1, v2)
			f.seedRequirement(t, "req-arch", "Archived capability", 1, 1, v2)
			f.mu.Lock()
			f.requirements["req-arch"].doc.Archived = true
			f.mu.Unlock()
			f.seedDesign("component-pending", "Pending component", 0, planDesignText)
			root := newPlanDraftDir(t)
			writePlanFile(t, root, tc.rel, tc.content)
			if tc.base != "" {
				writePlanFile(t, root, strings.TrimSuffix(tc.rel, ".md")+".base.md", tc.base)
			}
			approveAllPlanItems(t, root)
			layer := strings.Split(tc.rel, "/")[0]
			expectPlanRefusal(t, f, root, layer, tc.prefix, tc.fragments...)
		})
	}

	t.Run("statement under the high-water mark", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		f.seedRequirement(t, "req-rev", "Revised capability", 2, 3, v1, v2)
		root := newPlanDraftDir(t)
		writePlanFile(t, root, "requirements/req-rev.md", revise(2, planRequirementWithSecond()))
		writePlanFile(t, root, "requirements/req-rev.base.md", v2)
		approveAllPlanItems(t, root)
		expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-rev: AC-3.12:", "REQ-2 reuses a retired identifier", "REQ-3")
	})

	t.Run("intent to create a held ID", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		f.seedRequirement(t, "req-demo", "Other capability", 0, 1, planRequirementText("Other capability", "AC-1.1"))
		root := newPlanFixture(t)
		approveAllPlanItems(t, root)
		draft := readPlanFixture(t, root)
		intentAt := time.Now().UTC()
		draft.State.Pushes["req-demo"] = PlanPushRecord{Digest: draft.State.Approvals["req-demo"], Phase: PlanPushIntent, IntentAt: &intentAt}
		if err := SavePlanState(root, draft.State); err != nil {
			t.Fatal(err)
		}
		out := expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-demo: AC-3.12:", "earlier push recorded intent", "no version equals this item")
		if lines := planLinesWith(strings.Split(strings.TrimSpace(out), "\n"), "req-demo:"); len(lines) != 1 {
			t.Fatalf("refusal lines = %q", lines)
		}
	})

	t.Run("criterion reinstated from an older version", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		f.seedRequirement(t, "req-rev", "Revised capability", 2, 1, v1, v2)
		root := newPlanDraftDir(t)
		writePlanFile(t, root, "requirements/req-rev.md", revise(2, v1))
		writePlanFile(t, root, "requirements/req-rev.base.md", v2)
		approveAllPlanItems(t, root)
		out := mustPushPlan(t, root, PlanLayerRequirements)
		expectPlanLine(t, out, "posted req-rev:", "req-rev v3")
	})
}

func planRequirementWithSecond() string {
	return "# Revised capability\n\nThe revised capability keeps drafts local.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The CLI shall check drafts.\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: When AC-1.1 runs, the CLI shall pass.\n- id: REQ-2\n  statement: The CLI shall stay small.\n  acceptance_criteria:\n    - id: AC-2.1\n      statement: When it grows, the CLI shall refuse.\n```\n"
}

// AC-3.13, DEC-83(2), DEC-83(3): a changed item is refused while its
// proposal is pending, after it was confirmed, or after its task was
// created; once its proposal is dismissed and the item is approved again,
// it posts as a new proposal.
func TestPlanPushChangedItems(t *testing.T) {
	change := strings.Replace(planFixtureRequirement, "keeps drafts local", "keeps every draft local", 1)

	t.Run("pending and confirmed", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		root := newPlanFixture(t)
		approveAllPlanItems(t, root)
		mustPushPlan(t, root, PlanLayerRequirements)
		writePlanFile(t, root, "requirements/req-demo.md", change)
		approveAllPlanItems(t, root)
		expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-demo: AC-3.13:", "req-demo v1 is pending")
		f.confirmAll()
		expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-demo: AC-3.13:", "req-demo v1 was confirmed")
	})

	t.Run("task created", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		root := newPlanFixture(t)
		approveAllPlanItems(t, root)
		pushPlanThrough(t, f, root, PlanLayerTasks)
		mustPushPlan(t, root, PlanLayerTasks)
		id := planPushRecord(t, root, "draft:task/build").ServerID
		writePlanFile(t, root, "tasks/build.md", planFixtureBuildTask+"Also log the result.\n")
		approveAllPlanItems(t, root)
		expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/build: AC-3.13:", "task "+id)
	})

	t.Run("dismissed revision", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		v1 := planRequirementText("Revised capability", "AC-1.1")
		f.seedRequirement(t, "req-rev", "Revised capability", 1, 1, v1)
		root := newPlanDraftDir(t)
		revise := "---\nmode: revise\nbase_version: 1\n---\n"
		writePlanFile(t, root, "requirements/req-rev.md", revise+strings.Replace(v1, "keeps drafts", "keeps all drafts", 1))
		writePlanFile(t, root, "requirements/req-rev.base.md", v1)
		approveAllPlanItems(t, root)
		mustPushPlan(t, root, PlanLayerRequirements)
		f.dismissRequirement("req-rev", 2)
		out := mustPushPlan(t, root, PlanLayerRequirements)
		expectPlanLine(t, out, "req-rev: unchanged since push;", "req-rev v2 is dismissed")
		writePlanFile(t, root, "requirements/req-rev.md", revise+strings.Replace(v1, "keeps drafts", "keeps most drafts", 1))
		expectPlanRefusal(t, f, root, PlanLayerRequirements, "req-rev: AC-3.1:", "changed since approval")
		approveAllPlanItems(t, root)
		out = mustPushPlan(t, root, PlanLayerRequirements)
		expectPlanLine(t, out, "posted req-rev:", "req-rev v3 (pending)")
		if record := planPushRecord(t, root, "req-rev"); record.Version != 3 || record.Phase != PlanPushPosted {
			t.Fatalf("record after re-push = %+v", record)
		}
	})

	// req-local-planning v6 AC-3.13, DEC-83(2): a created document whose only
	// proposal was dismissed stays archived, so the revised item is refused
	// with the remedy, and the renamed item posts.
	dismissedCreations := []struct {
		name, rel, renamed, first, changed, again string
		dismiss                                   func(f *planFakeFactory)
		posts                                     string
	}{
		{"dismissed created requirement", "requirements/req-new.md", "requirements/req-new-two.md",
			"---\nmode: create\n---\n" + planRequirementText("New capability", "AC-1.1"),
			"---\nmode: create\n---\n" + strings.Replace(planRequirementText("New capability", "AC-1.1"), "keeps drafts", "keeps all drafts", 1),
			"---\nmode: create\n---\n" + strings.Replace(planRequirementText("Second capability", "AC-1.1"), "keeps drafts", "keeps all drafts", 1),
			func(f *planFakeFactory) { f.dismissRequirement("req-new", 1) }, "/v1/requirements"},
		{"dismissed created system design", "system-designs/feature-new.md", "system-designs/feature-new-two.md",
			"---\nmode: create\ntitle: New design\ncategory: Feature design\n---\n" + planDesignText,
			"---\nmode: create\ntitle: New design\ncategory: Feature design\n---\n" + strings.Replace(planDesignText, "The design text.", "The changed design text.", 1),
			"---\nmode: create\ntitle: Second design\ncategory: Feature design\n---\n" + strings.Replace(planDesignText, "The design text.", "The changed design text.", 1),
			func(f *planFakeFactory) { f.dismissDesign("feature-new", 1) }, "/v1/system-designs"},
	}
	for _, tc := range dismissedCreations {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			root := newPlanDraftDir(t)
			layer := strings.Split(tc.rel, "/")[0]
			itemID := strings.TrimSuffix(strings.Split(tc.rel, "/")[1], ".md")
			writePlanFile(t, root, tc.rel, tc.first)
			approveAllPlanItems(t, root)
			mustPushPlan(t, root, layer)
			tc.dismiss(f)
			writePlanFile(t, root, tc.rel, tc.changed)
			approveAllPlanItems(t, root)
			out := expectPlanRefusal(t, f, root, layer, itemID+": AC-3.13:", "whose only proposal "+itemID+" v1 was dismissed", "archived "+itemID, "new document ID and title")
			if lines := planLinesWith(strings.Split(strings.TrimSpace(out), "\n"), itemID+":"); len(lines) != 1 {
				t.Fatalf("refusal lines = %q", lines)
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(tc.rel))); err != nil {
				t.Fatal(err)
			}
			writePlanFile(t, root, tc.renamed, tc.again)
			approveAllPlanItems(t, root)
			renamed := strings.TrimSuffix(strings.Split(tc.renamed, "/")[1], ".md")
			out = mustPushPlan(t, root, layer)
			expectPlanLine(t, out, "posted "+renamed+":", renamed+" v1 (pending)")
			if posts := f.postsTo(tc.posts); len(posts) != 2 {
				t.Fatalf("create posts = %d, want the original and the renamed item", len(posts))
			}
		})
	}

	t.Run("dismissed decision", func(t *testing.T) {
		f := newPlanFakeFactory(t)
		root := newPlanFixture(t)
		approveAllPlanItems(t, root)
		pushPlanThrough(t, f, root, PlanLayerDecisions)
		mustPushPlan(t, root, PlanLayerDecisions)
		f.setDecisionStatus("DEC-1", core.DecisionDismissed)
		writePlanFile(t, root, "decisions/use-cli.yaml", strings.Replace(planFixtureDecision, "Check drafts", "Check every draft", 1))
		approveAllPlanItems(t, root)
		out := mustPushPlan(t, root, PlanLayerDecisions)
		expectPlanLine(t, out, "posted draft:dec/use-cli:", "DEC-2 (pending)")
	})
}

// AC-3.10: a refused item stops the layer, keeps earlier records, and names
// the item and the factory's reason.
func TestPlanPushStopsOnRefusal(t *testing.T) {
	f := newPlanFakeFactory(t)
	f.seedRequirement(t, "req-demo", "Demo capability", 1, 1, strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n"))
	root := newPlanDraftDir(t)
	for _, name := range []string{"a", "b", "c"} {
		writePlanFile(t, root, "decisions/"+name+".yaml", fmt.Sprintf("statement: Decide %s for req-demo REQ-1.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n", name))
	}
	approveAllPlanItems(t, root)
	f.refusePost = func(path string, body map[string]any) (planFakeRefusal, bool) {
		return planFakeRefusal{http.StatusBadRequest, "decision statement is too vague"}, strings.HasPrefix(body["statement"].(string), "Decide b ")
	}
	out, err := pushPlan(t, root, PlanLayerDecisions)
	if err == nil {
		t.Fatalf("push succeeded after a refusal\n%s", out)
	}
	expectPlanLine(t, out, "draft:dec/b: refused by the factory:", "400", "decision statement is too vague")
	if posts := f.postsTo("/v1/decisions"); len(posts) != 2 {
		t.Fatalf("posted %d decision(s); want a and b only", len(posts))
	}
	if record := planPushRecord(t, root, "draft:dec/a"); record.Phase != PlanPushPosted || record.ServerID != "DEC-1" {
		t.Fatalf("earlier record = %+v", record)
	}
	if _, ok := readPlanFixture(t, root).State.Pushes["draft:dec/c"]; ok {
		t.Fatal("an item after the refusal has a push record")
	}
	f.refusePost = nil
	out = mustPushPlan(t, root, PlanLayerDecisions)
	expectPlanLine(t, out, "draft:dec/a: unchanged since push;", "DEC-1 is pending")
	expectPlanLine(t, out, "posted draft:dec/b:", "DEC-2")
	expectPlanLine(t, out, "posted draft:dec/c:", "DEC-3")
}

// planDryRunPayloads maps each `would post` item to the JSON payload printed
// after its header.
func planDryRunPayloads(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	payloads := map[string]map[string]any{}
	offset := 0
	for _, line := range strings.SplitAfter(out, "\n") {
		start := offset
		offset += len(line)
		header, ok := strings.CutPrefix(line, "would post ")
		if !ok {
			continue
		}
		id := header[:strings.Index(header, ": POST ")]
		brace := start + strings.Index(out[start:], "{")
		var payload map[string]any
		if err := json.NewDecoder(strings.NewReader(out[brace:])).Decode(&payload); err != nil {
			t.Fatalf("decode dry-run payload for %s: %v\n%s", id, err, out)
		}
		payloads[id] = payload
	}
	return payloads
}

// AC-3.3: a dry run applies every check, exits as push would, prints the
// payloads push sends, posts nothing, and changes no state.
func TestPlanPushDryRunParity(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	writePlanFile(t, root, "decisions/a-follow.yaml", "statement: Follow draft:dec/use-cli for req-demo REQ-1.\ncontext: req-demo AC-1.1 needs it.\nalternatives_rejected: None.\n")
	approveAllPlanItems(t, root)

	state := readPlanStateBytes(t, root)
	dryOut, dryErr := pushPlan(t, root, PlanLayerDecisions, "--dry-run")
	realOut, realErr := pushPlan(t, root, PlanLayerDecisions)
	if dryErr == nil || realErr == nil || dryErr.Error() != realErr.Error() || dryOut != realOut {
		t.Fatalf("refused layer: dry run %v\n%s\npush %v\n%s", dryErr, dryOut, realErr, realOut)
	}
	if f.postCount() != 0 || !bytes.Equal(state, readPlanStateBytes(t, root)) {
		t.Fatal("refused dry run posted or changed state")
	}

	mustPushPlan(t, root, PlanLayerRequirements)
	f.confirmAll()
	state, posts := readPlanStateBytes(t, root), f.postCount()
	dryOut, dryErr = pushPlan(t, root, PlanLayerDecisions, "--dry-run")
	if dryErr != nil || f.postCount() != posts || !bytes.Equal(state, readPlanStateBytes(t, root)) {
		t.Fatalf("dry run: %v, posts %d→%d, state changed %v\n%s", dryErr, posts, f.postCount(), !bytes.Equal(state, readPlanStateBytes(t, root)), dryOut)
	}
	expectPlanLine(t, dryOut, "plan push: decisions layer:", "would post 2")
	predicted := planDryRunPayloads(t, dryOut)
	mustPushPlan(t, root, PlanLayerDecisions)
	for index, id := range []string{"draft:dec/use-cli", "draft:dec/a-follow"} {
		if sent := f.postsTo("/v1/decisions")[index].Body; !reflect.DeepEqual(predicted[id], sent) {
			t.Fatalf("%s: dry run printed %v, push sent %v", id, predicted[id], sent)
		}
	}
	f.confirmAll()
	mustPushPlan(t, root, PlanLayerSystemDesigns)
	f.confirmAll()

	dryOut, dryErr = pushPlan(t, root, PlanLayerTasks, "--dry-run")
	if dryErr != nil {
		t.Fatalf("task dry run: %v\n%s", dryErr, dryOut)
	}
	expectPlanLine(t, dryOut, "would post draft:task/build: POST /v1/tasks", "multipart form, no attachments")
	expectPlanLine(t, dryOut, "unresolved: draft:task/build", "gets its server ID")
	predicted = planDryRunPayloads(t, dryOut)
	mustPushPlan(t, root, PlanLayerTasks)
	tasks := f.postsTo("/v1/tasks")
	if !reflect.DeepEqual(predicted["draft:task/build"], tasks[0].Body) {
		t.Fatalf("build: dry run printed %v, push sent %v", predicted["draft:task/build"], tasks[0].Body)
	}
	if got := predicted["draft:task/docs"]["depends_on"]; !reflect.DeepEqual(got, []any{"draft:task/build"}) {
		t.Fatalf("dry run hides the unresolved binding: depends_on %v", got)
	}

	// An intent that needs recovery is read back but neither adopted nor
	// written by a dry run.
	f = newPlanFakeFactory(t)
	root = newPlanFixture(t)
	approveAllPlanItems(t, root)
	restore := failPlanStateSaveAt(t, 2)
	if _, err := pushPlan(t, root, PlanLayerRequirements); err == nil {
		t.Fatal("push succeeded with a failed posted record")
	}
	restore()
	state, posts = readPlanStateBytes(t, root), f.postCount()
	dryOut, dryErr = pushPlan(t, root, PlanLayerRequirements, "--dry-run")
	if dryErr != nil || f.postCount() != posts || !bytes.Equal(state, readPlanStateBytes(t, root)) {
		t.Fatalf("recovery dry run: %v, posts %d→%d\n%s", dryErr, posts, f.postCount(), dryOut)
	}
	expectPlanLine(t, dryOut, "would recover req-demo:", "already holds it as req-demo v1")
}

// AC-3.7, AC-3.9, LP-5: a task posts as a multipart form with a task part,
// an intake key, and no attachments; stated gates are carried, unstated
// gates are omitted; an intake key over 200 characters is refused.
func TestPlanPushTaskRequests(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	writePlanFile(t, root, "tasks/gated.md", "---\nrepo: conveyor\nbase_branch: release\nplan_approval: false\nmerge_approval: true\n---\nShip req-demo AC-1.1.\n")
	digests := approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerTasks)
	mustPushPlan(t, root, PlanLayerTasks)
	draftID := readPlanFixture(t, root).State.DraftID
	for _, post := range f.postsTo("/v1/tasks") {
		if !reflect.DeepEqual(post.FormNames, []string{"idempotency_key", "task"}) || post.Files != 0 {
			t.Fatalf("task form parts %v with %d file(s)", post.FormNames, post.Files)
		}
	}
	var gated planFakePost
	for _, post := range f.postsTo("/v1/tasks") {
		if strings.Contains(post.IntakeKey, ":draft:task/gated:") {
			gated = post
		}
	}
	if want := planIntakeKey(draftID, "draft:task/gated", digests["draft:task/gated"]); gated.IntakeKey != want {
		t.Fatalf("intake key = %q, want %q", gated.IntakeKey, want)
	}
	if gated.Body["spec_approval"] != false || gated.Body["merge_approval"] != true || gated.Body["base_branch"] != "release" {
		t.Fatalf("gated task body = %v", gated.Body)
	}
	if record := planPushRecord(t, root, "draft:task/gated"); record.IntakeKey != gated.IntakeKey || record.Phase != PlanPushPosted {
		t.Fatalf("gated record = %+v", record)
	}

	f = newPlanFakeFactory(t)
	root = newPlanFixture(t)
	long := strings.Repeat("x", 80)
	writePlanFile(t, root, "tasks/"+long+".md", "---\nrepo: conveyor\n---\nShip req-demo AC-1.1.\n")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerTasks)
	expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/"+long+": LP-5:", "at most 200")
}

// AC-3.6, DEC-83(1): for every kind, a rerun after the factory accepted a
// post but before the CLI recorded it creates no second proposal or task.
func TestPlanPushRecoversAcceptedPosts(t *testing.T) {
	v1 := planRequirementText("Revised capability", "AC-1.1")
	cases := []struct {
		name, layer, itemID string
		setup               func(t *testing.T, f *planFakeFactory) string
		count               func(f *planFakeFactory) int
		want                string
	}{
		{"requirement create", PlanLayerRequirements, "req-demo", func(t *testing.T, f *planFakeFactory) string {
			root := newPlanFixture(t)
			approveAllPlanItems(t, root)
			return root
		}, func(f *planFakeFactory) int { return len(f.requirements["req-demo"].versions) }, "req-demo v1"},
		{"requirement revise", PlanLayerRequirements, "req-rev", func(t *testing.T, f *planFakeFactory) string {
			f.seedRequirement(t, "req-rev", "Revised capability", 1, 1, v1)
			root := newPlanDraftDir(t)
			writePlanFile(t, root, "requirements/req-rev.md", "---\nmode: revise\nbase_version: 1\n---\n"+strings.Replace(v1, "keeps drafts", "keeps all drafts", 1))
			writePlanFile(t, root, "requirements/req-rev.base.md", v1)
			approveAllPlanItems(t, root)
			return root
		}, func(f *planFakeFactory) int { return len(f.requirements["req-rev"].versions) }, "req-rev v2"},
		{"system design create", PlanLayerSystemDesigns, "feature-demo", func(t *testing.T, f *planFakeFactory) string {
			root := newPlanFixture(t)
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, PlanLayerSystemDesigns)
			return root
		}, func(f *planFakeFactory) int { return len(f.designs["feature-demo"].versions) }, "feature-demo v1"},
		{"system design revise", PlanLayerSystemDesigns, "feature-rev", func(t *testing.T, f *planFakeFactory) string {
			f.seedDesign("feature-rev", "Revised design", 1, planDesignText)
			root := newPlanDraftDir(t)
			writePlanFile(t, root, "system-designs/feature-rev.md", "---\nmode: revise\nbase_version: 1\n---\n"+strings.Replace(planDesignText, "The design text.", "The revised design text.", 1))
			writePlanFile(t, root, "system-designs/feature-rev.base.md", planDesignText)
			approveAllPlanItems(t, root)
			return root
		}, func(f *planFakeFactory) int { return len(f.designs["feature-rev"].versions) }, "feature-rev v2"},
		{"decision", PlanLayerDecisions, "draft:dec/use-cli", func(t *testing.T, f *planFakeFactory) string {
			root := newPlanFixture(t)
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, PlanLayerDecisions)
			return root
		}, func(f *planFakeFactory) int { return len(f.decisions) }, "DEC-1"},
		{"task", PlanLayerTasks, "draft:task/build", func(t *testing.T, f *planFakeFactory) string {
			root := newPlanFixture(t)
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, PlanLayerTasks)
			return root
		}, func(f *planFakeFactory) int {
			built := 0
			for _, task := range f.tasks {
				if strings.HasPrefix(task.Body, "Implement req-demo AC-1.1") {
					built++
				}
			}
			return built
		}, "261020-t001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			root := tc.setup(t, f)
			restore := failPlanStateSaveAt(t, 2)
			out, err := pushPlan(t, root, tc.layer)
			if err == nil || !strings.Contains(err.Error(), "rerun push to recover") {
				t.Fatalf("push with a failed posted record: %v\n%s", err, out)
			}
			restore()
			record := planPushRecord(t, root, tc.itemID)
			if record.Phase != PlanPushIntent || record.IntentAt == nil {
				t.Fatalf("record after the failed save = %+v", record)
			}
			created := tc.count(f)
			out = mustPushPlan(t, root, tc.layer)
			if after := tc.count(f); after != created {
				t.Fatalf("rerun created a second proposal or task: %d → %d\n%s", created, after, out)
			}
			record = planPushRecord(t, root, tc.itemID)
			if record.Phase != PlanPushPosted || !strings.HasPrefix(tc.want, record.ServerID) {
				t.Fatalf("record after recovery = %+v, want %s", record, tc.want)
			}
			if tc.layer == PlanLayerTasks {
				posts := f.postsTo("/v1/tasks")
				if posts[0].IntakeKey != posts[1].IntakeKey || record.IntakeKey != posts[0].IntakeKey {
					t.Fatalf("task re-post changed its intake key: %q, %q, record %q", posts[0].IntakeKey, posts[1].IntakeKey, record.IntakeKey)
				}
			}
			out = mustPushPlan(t, root, tc.layer)
			expectPlanLine(t, out, tc.itemID+": unchanged since push;", tc.want)
		})
	}
}

// AC-3.6, AC-3.10: a post whose answer is lost and a failed intent write.
func TestPlanPushLostAnswerAndIntentFailure(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	approveAllPlanItems(t, root)
	f.dropAnswer["/v1/requirements"] = true
	out, err := pushPlan(t, root, PlanLayerRequirements)
	if err == nil || !strings.Contains(out, "req-demo: the post got no factory answer") {
		t.Fatalf("push with a lost answer: %v\n%s", err, out)
	}
	// Read-back compares factory-normalized content, so line endings and
	// edge whitespace in the stored copy do not hide the accepted post.
	f.mu.Lock()
	stored := &f.requirements["req-demo"].versions[0]
	stored.Content = "\n" + strings.ReplaceAll(stored.Content, "\n", "\r\n") + "\r\n\r\n"
	f.mu.Unlock()
	out = mustPushPlan(t, root, PlanLayerRequirements)
	expectPlanLine(t, out, "recovered req-demo:", "req-demo v1")
	if len(f.postsTo("/v1/requirements")) != 1 || len(f.requirements["req-demo"].versions) != 1 {
		t.Fatal("recovery posted the requirement again")
	}

	f = newPlanFakeFactory(t)
	root = newPlanFixture(t)
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerDecisions)
	posts := f.postCount()
	failPlanStateSaveAt(t, 1)
	out, err = pushPlan(t, root, PlanLayerDecisions)
	if err == nil || !strings.Contains(err.Error(), "record intent") || f.postCount() != posts {
		t.Fatalf("push with a failed intent write: %v, posts %d→%d\n%s", err, posts, f.postCount(), out)
	}
}

// LP-5 decision row: an intent whose chosen ID holds different fields, and a
// 409 on post, choose a new ID and record a new intent.
func TestPlanPushDecisionCollisions(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerDecisions)
	draft := readPlanFixture(t, root)
	intentAt := time.Now().UTC()
	draft.State.Pushes["draft:dec/use-cli"] = PlanPushRecord{Digest: draft.State.Approvals["draft:dec/use-cli"], Phase: PlanPushIntent, IntentAt: &intentAt, ChosenID: "DEC-5"}
	if err := SavePlanState(root, draft.State); err != nil {
		t.Fatal(err)
	}
	f.setDecisionStatus("DEC-5", core.DecisionProposed)
	f.beforeDecisionPost = func(id string) {
		if id == "DEC-6" {
			f.decisions["DEC-6"] = core.Decision{ID: "DEC-6", Statement: "Taken concurrently.", Status: core.DecisionProposed}
		}
	}
	out := mustPushPlan(t, root, PlanLayerDecisions)
	posts := f.postsTo("/v1/decisions")
	if len(posts) != 2 || posts[0].Body["id"] != "DEC-6" || posts[1].Body["id"] != "DEC-7" {
		t.Fatalf("decision posts = %+v", posts)
	}
	expectPlanLine(t, out, "posted draft:dec/use-cli:", "DEC-7 (pending)")
	if record := planPushRecord(t, root, "draft:dec/use-cli"); record.ChosenID != "DEC-7" || record.ServerID != "DEC-7" {
		t.Fatalf("record = %+v", record)
	}
}

// AC-3.6: a rerun with only posted records reports each state and posts
// nothing.
func TestPlanPushReportsPostedStates(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	approveAllPlanItems(t, root)
	mustPushPlan(t, root, PlanLayerRequirements)
	posts := f.postCount()
	out := mustPushPlan(t, root, PlanLayerRequirements)
	expectPlanLine(t, out, "req-demo: unchanged since push;", "req-demo v1 is pending")
	expectPlanLine(t, out, "plan push: requirements layer:", "posted 0, recovered 0, unchanged 1")
	f.confirmAll()
	out = mustPushPlan(t, root, PlanLayerRequirements)
	expectPlanLine(t, out, "req-demo: unchanged since push;", "req-demo v1 is confirmed")
	if f.postCount() != posts {
		t.Fatal("a rerun of a posted layer posted again")
	}
}

// AC-3.4: push sends no confirm or dismiss request, and its source names no
// such route.
func TestPlanPushNeverConfirmsOrDismisses(t *testing.T) {
	api := &planAPI{base: "http://127.0.0.1:1", httpClient: http.DefaultClient}
	for _, path := range []string{"/v1/requirements/req-demo/versions/1/confirm", "/v1/decisions/DEC-1/dismiss"} {
		if err := api.postJSON(t.Context(), path, map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "never calls a confirm or dismiss route") {
			t.Fatalf("POST %s: %v", path, err)
		}
	}
	files, err := filepath.Glob("plan*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, _ := strconv.Unquote(literal.Value)
			if strings.Contains(value, "/confirm") || strings.Contains(value, "/dismiss") {
				t.Errorf("%s names a confirm or dismiss route: %s", name, literal.Value)
			}
			return true
		})
	}
}

// AC-3.3, AC-3.5: a citation or dependency cycle within the layer, a
// self-citation included, refuses the layer in both dry run and push before
// any post, even when an independent item would post first.
func TestPlanPushRefusesSameLayerCycles(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		lines [][]string
	}{
		{"citation and dependency cycle", map[string]string{
			"tasks/build.md": planFixtureBuildTask + "See draft:task/docs.\n",
		}, [][]string{{"draft:task/build: AC-3.5:", "cites or depends on draft:task/docs", "cycle among draft:task/build, draft:task/docs"}, {"draft:task/docs: AC-3.5:", "cites or depends on draft:task/build"}}},
		{"independent item sorts first", map[string]string{
			"tasks/build.md":    planFixtureBuildTask + "See draft:task/docs.\n",
			"tasks/aaa-solo.md": "---\nrepo: conveyor\n---\nShip req-demo AC-1.1 alone.\n",
		}, [][]string{{"draft:task/build: AC-3.5:"}, {"draft:task/docs: AC-3.5:"}}},
		{"self-citation", map[string]string{
			"tasks/build.md": planFixtureBuildTask + "Track it as draft:task/build.\n",
		}, [][]string{{"draft:task/build: AC-3.5:", "cites or depends on draft:task/build", "cycle among draft:task/build"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			root := newPlanFixture(t)
			for rel, content := range tc.files {
				writePlanFile(t, root, rel, content)
			}
			if _, err := runPlanCommand(t, "check", root); err != nil {
				t.Fatalf("offline check refused the draft: %v", err)
			}
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, PlanLayerTasks)
			posts, state := f.postCount(), readPlanStateBytes(t, root)
			dryOut, dryErr := pushPlan(t, root, PlanLayerTasks, "--dry-run")
			realOut, realErr := pushPlan(t, root, PlanLayerTasks)
			if dryErr == nil || realErr == nil || dryErr.Error() != realErr.Error() || dryOut != realOut {
				t.Fatalf("dry run %v\n%s\npush %v\n%s", dryErr, dryOut, realErr, realOut)
			}
			for _, line := range tc.lines {
				expectPlanLine(t, realOut, line[0], line[1:]...)
			}
			if f.postCount() != posts || !bytes.Equal(state, readPlanStateBytes(t, root)) {
				t.Fatalf("a refused layer posted %d request(s) or changed state", f.postCount()-posts)
			}
		})
	}
}
