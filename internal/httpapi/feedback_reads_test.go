package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
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

// hold closes the gate and returns its release, which is safe to call more
// than once. The test registers it for cleanup, so a failed assertion never
// leaves a forge call blocked.
func (f *readsForge) hold(t *testing.T) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.gate = gate
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
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

// requireEmptyArray fails unless raw is exactly the JSON array []. A
// missing key (nil) or null fails (feature-production-feedback-live v1,
// Rollout and closure).
func requireEmptyArray(t *testing.T, what string, raw json.RawMessage) {
	t.Helper()
	if raw == nil {
		t.Fatalf("%s: the key is missing, want []", what)
	}
	if got := strings.TrimSpace(string(raw)); got != "[]" {
		t.Fatalf("%s=%s, want []", what, got)
	}
}

// requireAbsent fails when the key is present, null included.
func requireAbsent(t *testing.T, what string, fields map[string]json.RawMessage, key string) {
	t.Helper()
	if raw, found := fields[key]; found {
		t.Fatalf("%s holds %s=%s, want no such key", what, key, raw)
	}
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

	// Before any deploy no environment is relevant, so a merged task's list
	// is empty, not missing.
	requireEmptyArray(t, "task environments before any deploy", h.taskRead(live.ID)["environments"])
	requireEmptyArray(t, "MCP task environments before any deploy", h.mcpTask(live.ID)["environments"])
	beforeTask := h.taskRead(live.ID)
	beforeOrders, err := h.st.ListWorkOrders(h.ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The route answers while the forge is held, so no forge call holds it.
	release := h.forge.hold(t)
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
	release()
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
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(crateBody["tasks"], &rawRows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rawRows {
		if string(row["id"]) == strconv.Quote(notYet.ID) {
			requireEmptyArray(t, "crate live_in of a member live nowhere", row["live_in"])
		}
	}
	page := readAs(t, h.handler, h.manager(), "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})
	mcpCrate := decodeFields(t, page.Items[0])
	var mcpLines []feedback.CrateLine
	var mcpLiveIn map[string]json.RawMessage
	if err := json.Unmarshal(mcpCrate["live_in"], &mcpLiveIn); err != nil || len(mcpLiveIn) != 2 || string(mcpLiveIn[live.ID]) != `["Production"]` {
		t.Fatalf("MCP crate live_in=%s err=%v", mcpCrate["live_in"], err)
	}
	requireEmptyArray(t, "MCP crate live_in of a member live nowhere", mcpLiveIn[notYet.ID])
	if err := json.Unmarshal(mcpCrate["environments"], &mcpLines); err != nil || !reflect.DeepEqual(mcpLines, lines) {
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
	release = h.forge.hold(t)
	calls := h.forge.count()
	type response struct {
		code int
		body []byte
	}
	read := make(chan response, 1)
	// The goroutine only sends the answer; every assertion runs here.
	go func() {
		recorded := h.do(h.manager(), http.MethodGet, "/v1/tasks/"+fresh.ID, "")
		read <- response{code: recorded.Code, body: recorded.Body.Bytes()}
	}()
	select {
	case got := <-read:
		if got.code != http.StatusOK {
			t.Fatalf("task read status=%d body=%s", got.code, got.body)
		}
		// The only status needs a comparison that is not stored, so it is
		// left out and the merged task's list is empty.
		requireEmptyArray(t, "task environments while the comparison is missing", decodeFields(t, got.body)["environments"])
	case <-time.After(10 * time.Second):
		t.Fatal("the task read waited for the forge")
	}
	select {
	case <-h.forge.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the read requested no comparison")
	}
	release()
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

// TestFeedbackReadsWithoutEnvironment covers feature-production-feedback-live
// v1, Rollout and closure, and component-production-feedback, Public
// surfaces: in a workspace with no environment, each list in its stated
// scope is the JSON array [], never null or a missing key. A merged task's
// environments, a crate's environments, a merged REST crate member's live_in,
// and the MCP crate detail's live_in value for each merged member in
// task_ids are []. A task that is not merged has no environments, and a
// crate member that is not merged has no live_in.
func TestFeedbackReadsWithoutEnvironment(t *testing.T) {
	h := newReadsHarness(t)
	crate, err := h.st.CreateCrate(h.betaCtx, store.CrateInput{Name: "Beta wave"})
	if err != nil {
		t.Fatal(err)
	}
	merged := h.merged(h.betaCtx, readsCommit("a1"), crate.ID)
	open := core.Task{ID: core.NewTaskID(), Workspace: "beta", Repo: "elsewhere", Title: "Open", Body: "Open.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Crate: &core.TaskCrate{ID: crate.ID}}
	if err := h.st.CreateTask(h.betaCtx, open); err != nil {
		t.Fatal(err)
	}
	beta := func(path string) map[string]json.RawMessage {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+h.foreign)
		request.Header.Set("X-Workspace-ID", "beta")
		response := h.serve(request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body)
		}
		return decodeFields(t, response.Body.Bytes())
	}
	mcp := func(tool string, args map[string]any) map[string]json.RawMessage {
		t.Helper()
		args["workspace_id"] = "beta"
		page := readAs(t, h.handler, h.foreign, tool, args)
		if len(page.Items) != 1 {
			t.Fatalf("%s page=%+v", tool, page)
		}
		return decodeFields(t, page.Items[0])
	}

	// Task detail: [] for the merged task, no key for the open one.
	requireEmptyArray(t, "REST merged task environments", beta("/v1/tasks/" + merged.ID)["environments"])
	requireAbsent(t, "REST open task", beta("/v1/tasks/"+open.ID), "environments")
	requireEmptyArray(t, "MCP merged task environments", mcp("get_task", map[string]any{"task_id": merged.ID})["environments"])
	requireAbsent(t, "MCP open task", mcp("get_task", map[string]any{"task_id": open.ID}), "environments")

	// REST crate detail: [] lines, [] live_in for the merged member only.
	crateBody := beta("/v1/crates/" + crate.ID)
	requireEmptyArray(t, "REST crate environments", crateBody["environments"])
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(crateBody["tasks"], &rows); err != nil || len(rows) != 2 {
		t.Fatalf("crate rows=%s err=%v", crateBody["tasks"], err)
	}
	for _, row := range rows {
		switch string(row["id"]) {
		case strconv.Quote(merged.ID):
			requireEmptyArray(t, "REST merged member live_in", row["live_in"])
		case strconv.Quote(open.ID):
			requireAbsent(t, "REST open member", row, "live_in")
		default:
			t.Fatalf("unexpected crate row %s", row["id"])
		}
	}

	// MCP crate detail: [] lines, and a live_in object whose only entry is
	// the merged member, with the value [].
	crateFields := mcp("get_crate", map[string]any{"crate_id": crate.ID})
	requireEmptyArray(t, "MCP crate environments", crateFields["environments"])
	var liveIn map[string]json.RawMessage
	if raw := crateFields["live_in"]; raw == nil || string(raw) == "null" || json.Unmarshal(raw, &liveIn) != nil {
		t.Fatalf("MCP crate live_in=%s, want an object", raw)
	}
	if len(liveIn) != 1 {
		t.Fatalf("MCP crate live_in=%s, want only the merged member", crateFields["live_in"])
	}
	requireEmptyArray(t, "MCP merged member live_in", liveIn[merged.ID])
	if _, found := liveIn[open.ID]; found {
		t.Fatal("MCP crate live_in names a member that is not merged")
	}

	// List endpoints get none of these fields.
	listRequest, _ := http.NewRequest(http.MethodGet, "/v1/tasks", nil)
	listRequest.Header.Set("Authorization", "Bearer "+h.foreign)
	listRequest.Header.Set("X-Workspace-ID", "beta")
	if list := h.serve(listRequest); list.Code != http.StatusOK || strings.Contains(list.Body.String(), `"environments"`) || strings.Contains(list.Body.String(), `"live_in"`) {
		t.Fatalf("task list status=%d holds a feedback field: %s", list.Code, list.Body)
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
	// live_in holds an entry, [], for each merged member task_ids lists, and
	// none for the member outside that window.
	var liveIn map[string]json.RawMessage
	if err := json.Unmarshal(fields["live_in"], &liveIn); err != nil || len(liveIn) != mcpCrateTaskIDLimit {
		t.Fatalf("live_in entries=%d err=%v", len(liveIn), err)
	}
	listed := map[string]bool{}
	for _, id := range taskIDs {
		listed[id] = true
		requireEmptyArray(t, "live_in of member "+id, liveIn[id])
	}
	for id := range liveIn {
		if !listed[id] {
			t.Fatalf("live_in names member %s outside task_ids", id)
		}
	}
	h.settle()
}
