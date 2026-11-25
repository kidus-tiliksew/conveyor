package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// readsForge is a stubbed GitHub boundary for the read tests: one line of
// history in acme/app, and a gate that holds every call until it opens.
type readsForge struct {
	mu      sync.Mutex
	history []string
	gate    chan struct{}
	entered chan struct{}
	calls   int
}

func newReadsForge(history ...string) *readsForge {
	gate := make(chan struct{})
	close(gate)
	return &readsForge{history: history, gate: gate, entered: make(chan struct{}, 1024)}
}

func (f *readsForge) hold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
}

func (f *readsForge) open() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.gate)
}

func (f *readsForge) wait() {
	f.mu.Lock()
	f.calls++
	gate := f.gate
	f.mu.Unlock()
	f.entered <- struct{}{}
	<-gate
}

func (f *readsForge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *readsForge) ReadCommit(_ context.Context, slug, ref string) (string, error) {
	f.wait()
	for _, commit := range f.history {
		if slug == "acme/app" && len(ref) >= 7 && strings.HasPrefix(commit, ref) {
			return commit, nil
		}
	}
	return "", github.CommitNotFoundError(ref)
}

func (f *readsForge) Compare(_ context.Context, slug, base, head string, _ int) (feedback.CompareResult, error) {
	f.wait()
	baseAt, headAt := slices.Index(f.history, base), slices.Index(f.history, head)
	switch {
	case slug != "acme/app" || baseAt < 0 || headAt < 0:
		return feedback.CompareResult{}, github.StatusError(404, errors.New("not found"))
	case baseAt == headAt:
		return feedback.CompareResult{Status: "identical"}, nil
	case baseAt < headAt:
		return feedback.CompareResult{Status: "ahead"}, nil
	}
	return feedback.CompareResult{Status: "behind"}, nil
}

// readsHarness is the signal harness with a GitHub slug for the workspace's
// repository and a stubbed forge.
type readsHarness struct {
	*feedbackHarness
	forge *readsForge
}

func readsCommit(seed string) string { return (seed + strings.Repeat("0", 40))[:40] }

func newReadsHarness(t *testing.T, history ...string) *readsHarness {
	t.Helper()
	h := &readsHarness{feedbackHarness: newFeedbackHarness(t), forge: newReadsForge(history...)}
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/app"}}}); err != nil {
		t.Fatal(err)
	}
	h.server.feedbackForge = h.forge
	return h
}

// merged creates a merged task of the app repository.
func (h *readsHarness) merged(ctx context.Context, merge, crateID string) core.Task {
	h.t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "app", Title: "Merged", Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if crateID != "" {
		task.Crate = &core.TaskCrate{ID: crateID}
	}
	if err := h.st.CreateTask(ctx, task); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "merge_commit_sha": merge})}); err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *readsHarness) settle() {
	h.t.Helper()
	h.server.feedbackWorker().wait()
}

func decodeFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return fields
}

func (h *readsHarness) taskRead(id string) map[string]json.RawMessage {
	h.t.Helper()
	return decodeFields(h.t, h.want(http.StatusOK, http.MethodGet, "/v1/tasks/"+id, "").Body.Bytes())
}

func (h *readsHarness) mcpTask(id string) map[string]json.RawMessage {
	h.t.Helper()
	page := readAs(h.t, h.handler, h.manager(), "get_task", map[string]any{"workspace_id": "alpha", "task_id": id})
	if len(page.Items) != 1 {
		h.t.Fatalf("get_task page=%+v", page)
	}
	return decodeFields(h.t, page.Items[0])
}

func statusesOf(t *testing.T, raw json.RawMessage) []feedback.TaskStatus {
	t.Helper()
	var statuses []feedback.TaskStatus
	if err := json.Unmarshal(raw, &statuses); err != nil {
		t.Fatalf("decode statuses %s: %v", raw, err)
	}
	return statuses
}

func feedbackSignalBody(service string, commits ...string) string {
	versions := make([]string, len(commits))
	for i, commit := range commits {
		versions[i] = fmt.Sprintf(`{"commit":%q}`, commit[:12])
	}
	return fmt.Sprintf(`{"next_signal_seconds":60,"services":[{"name":%q,"versions":[%s]}]}`, service, strings.Join(versions, ","))
}

// TestFeedbackLiveStatusReads covers component-production-feedback invariants
// 8, 14 to 20, and 1, and req-production-feedback AC-1.4, AC-3.1, AC-3.2,
// AC-3.4, AC-3.6, AC-10.1, and AC-11.1 through the full handler: the route
// answers before any forge call, the background work resolves and compares,
// and the task read, the crate read, and MCP get_task and get_crate serve one
// derivation.
func TestFeedbackLiveStatusReads(t *testing.T) {
	early, merge, deployed, later := readsCommit("a1a1a1a1a1a1"), readsCommit("b2b2b2b2b2b2"), readsCommit("c3c3c3c3c3c3"), readsCommit("d4d4d4d4d4d4")
	h := newReadsHarness(t, early, merge, deployed, later)
	crate, err := h.st.CreateCrate(h.ctx, store.CrateInput{Name: "Wave"})
	if err != nil {
		t.Fatal(err)
	}
	live := h.merged(h.ctx, merge, crate.ID)
	notYet := h.merged(h.ctx, later, crate.ID)

	// Before any deploy no environment is relevant, so no read adds the field.
	if _, found := h.taskRead(live.ID)["environments"]; found {
		t.Fatal("a task read before any deploy holds environments")
	}
	beforeTask := h.taskRead(live.ID)
	beforeOrders, err := h.st.ListWorkOrders(h.ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The route answers while the forge is held, so no forge call holds it.
	h.forge.hold()
	answered := make(chan int, 1)
	go func() {
		h.advance(feedbackSignalWindow)
		answered <- h.signal(h.key.Value, feedbackSignalBody("api", deployed)).Code
	}()
	select {
	case code := <-answered:
		if code != http.StatusAccepted {
			t.Fatalf("signal status=%d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the signal route waited for the forge")
	}
	select {
	case <-h.forge.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the background work made no forge call")
	}
	h.forge.open()
	h.settle()
	arrival := h.clock()

	statuses := statusesOf(t, h.taskRead(live.ID)["environments"])
	if len(statuses) != 1 || statuses[0].Status != feedback.StatusLive || statuses[0].Environment.Name != "Production" || !slices.Equal(statuses[0].Services, []string{"api"}) || !statuses[0].LiveSince.Equal(store.EnvironmentTime(arrival)) || statuses[0].AsOf == nil || !statuses[0].AsOf.Equal(store.EnvironmentTime(arrival)) {
		t.Fatalf("live statuses=%+v", statuses)
	}
	if statuses := statusesOf(t, h.taskRead(notYet.ID)["environments"]); len(statuses) != 1 || statuses[0].Status != feedback.StatusNotLiveYet || statuses[0].OnTrackedBranch {
		t.Fatalf("not live yet statuses=%+v", statuses)
	}
	// MCP get_task serves the same field.
	if rest, mcp := h.taskRead(live.ID)["environments"], h.mcpTask(live.ID)["environments"]; string(rest) != string(mcp) {
		t.Fatalf("REST %s, MCP %s", rest, mcp)
	}

	// The crate read and MCP get_crate serve the same lines.
	crateBody := decodeFields(t, h.want(http.StatusOK, http.MethodGet, "/v1/crates/"+crate.ID, "").Body.Bytes())
	var lines []feedback.CrateLine
	if err := json.Unmarshal(crateBody["environments"], &lines); err != nil || len(lines) != 1 || lines[0].LiveTaskCount != 1 || lines[0].MergedTaskCount != 2 || !lines[0].NewestLiveAt.Equal(store.EnvironmentTime(arrival)) || !lines[0].Listening {
		t.Fatalf("crate lines=%s err=%v", crateBody["environments"], err)
	}
	var rows []struct {
		ID     string   `json:"id"`
		LiveIn []string `json:"live_in"`
	}
	if err := json.Unmarshal(crateBody["tasks"], &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if want := row.ID == live.ID; want != slices.Equal(row.LiveIn, []string{"Production"}) {
			t.Fatalf("row %s live_in=%v", row.ID, row.LiveIn)
		}
	}
	page := readAs(t, h.handler, h.manager(), "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})
	mcpCrate := decodeFields(t, page.Items[0])
	var mcpLines []feedback.CrateLine
	if err := json.Unmarshal(mcpCrate["environments"], &mcpLines); err != nil || !reflect.DeepEqual(mcpLines, lines) || string(mcpCrate["live_in"]) != fmt.Sprintf(`{%q:["Production"]}`, live.ID) {
		t.Fatalf("MCP crate environments=%s live_in=%s", mcpCrate["environments"], mcpCrate["live_in"])
	}

	// Live status changed and nothing of the task's delivery did.
	afterTask := h.taskRead(live.ID)
	for _, field := range []string{"state", "gate_kind", "plan_state", "next_stage", "next_action", "plan_acts"} {
		if string(beforeTask[field]) != string(afterTask[field]) {
			t.Fatalf("%s changed: %s -> %s", field, beforeTask[field], afterTask[field])
		}
	}
	if afterOrders, err := h.st.ListWorkOrders(h.ctx); err != nil || len(afterOrders) != len(beforeOrders) {
		t.Fatalf("work orders %d -> %d err=%v", len(beforeOrders), len(afterOrders), err)
	}

	// A read never calls the forge: with the forge held, a task merged after
	// the comparisons ran is read at once without a status, and the missing
	// comparison is requested in the background.
	fresh := h.merged(h.ctx, early, "")
	h.forge.hold()
	calls := h.forge.count()
	read := make(chan map[string]json.RawMessage, 1)
	go func() { read <- h.taskRead(fresh.ID) }()
	select {
	case fields := <-read:
		if _, found := fields["environments"]; found {
			t.Fatalf("a status that needs a missing comparison was served: %s", fields["environments"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the task read waited for the forge")
	}
	select {
	case <-h.forge.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the read requested no comparison")
	}
	h.forge.open()
	h.settle()
	if h.forge.count() <= calls {
		t.Fatal("no background comparison ran")
	}
	if statuses := statusesOf(t, h.taskRead(fresh.ID)["environments"]); len(statuses) != 1 || statuses[0].Status != feedback.StatusLive {
		t.Fatalf("statuses after the background comparison=%+v", statuses)
	}
}

// TestFeedbackEnvironmentNewestUnresolved covers invariant 21 and
// req-production-feedback AC-2.3 on GET /v1/environments.
func TestFeedbackEnvironmentNewestUnresolved(t *testing.T) {
	known := readsCommit("e5e5e5e5e5e5")
	h := newReadsHarness(t, known)
	row := func() map[string]json.RawMessage {
		t.Helper()
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(h.want(http.StatusOK, http.MethodGet, "/v1/environments", "").Body.Bytes(), &rows); err != nil || len(rows) != 1 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
		return rows[0]
	}
	if got := string(row()["newest_unresolved"]); got != "null" {
		t.Fatalf("newest_unresolved before a signal=%s", got)
	}
	h.accept(feedbackSignalBody("api", known))
	h.settle()
	if got := string(row()["newest_unresolved"]); got != "null" {
		t.Fatalf("newest_unresolved with a resolved version=%s", got)
	}
	unknown := readsCommit("f6f6f6f6f6f6")
	h.accept(`{"next_signal_seconds":60,"services":[{"name":"worker","versions":[{"commit":"` + unknown[:12] + `"}]}]}`)
	h.settle()
	var unresolved struct {
		Service string `json:"service"`
		Commit  string `json:"commit"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(row()["newest_unresolved"], &unresolved); err != nil || unresolved.Service != "worker" || unresolved.Commit != unknown[:12] || unresolved.Reason != store.FeedbackReasonNoMatch {
		t.Fatalf("newest_unresolved=%+v err=%v", unresolved, err)
	}
}

// TestFeedbackReadsWithoutEnvironment proves that a workspace with no
// environment gets no new field on any of the four reads.
func TestFeedbackReadsWithoutEnvironment(t *testing.T) {
	h := newReadsHarness(t)
	crate, err := h.st.CreateCrate(h.betaCtx, store.CrateInput{Name: "Beta wave"})
	if err != nil {
		t.Fatal(err)
	}
	task := h.merged(h.betaCtx, readsCommit("a1"), crate.ID)
	foreign := h.foreign
	get := func(path string) map[string]json.RawMessage {
		t.Helper()
		request := h.do(foreign, http.MethodGet, path, "")
		if request.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, request.Code, request.Body)
		}
		return decodeFields(t, request.Body.Bytes())
	}
	// h.do names workspace alpha; beta reads go through their own header.
	_ = get
	beta := func(path string) map[string]json.RawMessage {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+foreign)
		request.Header.Set("X-Workspace-ID", "beta")
		response := h.serve(request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body)
		}
		return decodeFields(t, response.Body.Bytes())
	}
	if _, found := beta("/v1/tasks/" + task.ID)["environments"]; found {
		t.Fatal("task read holds environments")
	}
	crateBody := beta("/v1/crates/" + crate.ID)
	if _, found := crateBody["environments"]; found || strings.Contains(string(crateBody["tasks"]), "live_in") {
		t.Fatalf("crate read=%s", crateBody["environments"])
	}
	taskPage := readAs(t, h.handler, foreign, "get_task", map[string]any{"workspace_id": "beta", "task_id": task.ID})
	if _, found := decodeFields(t, taskPage.Items[0])["environments"]; found {
		t.Fatal("MCP get_task holds environments")
	}
	cratePage := readAs(t, h.handler, foreign, "get_crate", map[string]any{"workspace_id": "beta", "crate_id": crate.ID})
	fields := decodeFields(t, cratePage.Items[0])
	if _, found := fields["environments"]; found {
		t.Fatal("MCP get_crate holds environments")
	}
	if _, found := fields["live_in"]; found {
		t.Fatal("MCP get_crate holds live_in")
	}
}

// TestMCPCrateCountsIgnoreTheMemberWindow proves the crate lines count every
// merged member even when task_ids is truncated, and live_in names only
// members task_ids lists.
func TestMCPCrateCountsIgnoreTheMemberWindow(t *testing.T) {
	h := newReadsHarness(t)
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+h.environment.ID, `{"tracked_branches":{"app":"main"}}`)
	crate, err := h.st.CreateCrate(h.ctx, store.CrateInput{Name: "Large"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range mcpCrateTaskIDLimit + 1 {
		h.merged(h.ctx, readsCommit(fmt.Sprintf("%08x", i)), crate.ID)
	}
	page := readAs(t, h.handler, h.manager(), "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})
	fields := decodeFields(t, page.Items[0])
	var taskIDs []string
	var lines []feedback.CrateLine
	if err := json.Unmarshal(fields["task_ids"], &taskIDs); err != nil || len(taskIDs) != mcpCrateTaskIDLimit || string(fields["task_ids_truncated"]) != "true" {
		t.Fatalf("task_ids=%d truncated=%s err=%v", len(taskIDs), fields["task_ids_truncated"], err)
	}
	if err := json.Unmarshal(fields["environments"], &lines); err != nil || len(lines) != 1 || lines[0].MergedTaskCount != mcpCrateTaskIDLimit+1 || lines[0].LiveTaskCount != 0 {
		t.Fatalf("lines=%s err=%v", fields["environments"], err)
	}
	h.settle()
}
