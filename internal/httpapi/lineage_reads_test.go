package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// lineageReadsFixture is a requirement with two confirmed versions and a
// System Design with two confirmed versions. The old task served
// requirement v1 and design v1 and merged; the new task served v2 of each
// and merged; the running task served requirement v2. The old task carries a
// review verdict from round 1, seat 2 (component-lineage;
// req-260802-72fc68 AC-5.2, AC-5.3).
type lineageReadsFixture struct {
	server                    *Server
	handler                   http.Handler
	ctx                       context.Context
	requirement, design       string
	oldTask, newTask, running string
	reviewOrder               string
}

func newLineageReadsFixture(t *testing.T, contextSettings config.LineageContextSettings) lineageReadsFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator-lineage-reads", Role: core.ActorHuman}), "demo")
	st := store.NewMemory()
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-lineage-reads", Title: "Lineage reads"}, core.RequirementVersion{
		Content: "# Lineage reads", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Answer lineage questions."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	governs := "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/lineage/**\n```"
	design, designFirst, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-lineage-reads", Title: "Lineage reads design", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Lineage reads design" + governs, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designFirst.Version, 0); err != nil {
		t.Fatal(err)
	}
	// A terminal task refuses context changes, so the attachment events the
	// context writer records are appended directly; the projector derives
	// the same version edges from them.
	newTask := func(id string, state core.TaskState, version int) string {
		t.Helper()
		task := core.Task{ID: id, Workspace: "demo", Title: "Task " + id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, CreatedAt: time.Now().UTC()}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		for _, event := range []core.Event{
			{TaskID: id, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": version})},
			{TaskID: id, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": design.ID, "version": version})},
		} {
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	fixture := lineageReadsFixture{ctx: ctx, requirement: requirement.ID, design: design.ID}
	fixture.oldTask = newTask("task-lineage-old", core.TaskMerged, 1)
	second, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Lineage reads v2", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Answer every lineage question."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, second.Version); err != nil {
		t.Fatal(err)
	}
	designSecond, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Lineage reads design v2" + governs, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designSecond.Version, designFirst.Version); err != nil {
		t.Fatal(err)
	}
	fixture.newTask = newTask("task-lineage-new", core.TaskMerged, 2)
	fixture.running = newTask("task-lineage-running", core.TaskRunning, 2)
	fixture.reviewOrder = fixture.oldTask + "-review-1-seat-2"
	if err = st.CreateJob(ctx, core.Job{ID: fixture.reviewOrder, TaskID: fixture.oldTask, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.CreateWorkOrder(ctx, st, core.WorkOrder{ID: fixture.reviewOrder, TaskID: fixture.oldTask, JobID: fixture.reviewOrder, Stage: core.StageReview, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: fixture.oldTask, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": fixture.reviewOrder})},
		{TaskID: fixture.oldTask, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": fixture.reviewOrder, "verdict": "approve", "review_round": 1, "review_seat": 2})},
		{TaskID: fixture.oldTask, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "base_sha": "old-base", "head_sha": "old-head"})},
		{TaskID: fixture.newTask, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "conveyor", "base_sha": "new-base", "head_sha": "new-head"})},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	fixture.server = NewServer(st)
	fixture.server.Workspace = "demo"
	fixture.server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: contextSettings}}}}, nil
	}
	fixture.handler = fixture.server.Handler()
	return fixture
}

func (f lineageReadsFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, authenticatedMemoryRead(f.server, httptest.NewRequest(http.MethodGet, path, nil)))
	return response
}

func (f lineageReadsFixture) walk(t *testing.T, path string) lineageGraphView {
	t.Helper()
	response := f.get(t, path)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	var graph lineageGraphView
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

func nodeKeys(nodes []lineageNodeView) []string {
	keys := make([]string, 0, len(nodes))
	for _, node := range nodes {
		keys = append(keys, string(node.Type)+":"+node.ID)
	}
	sort.Strings(keys)
	return keys
}

func TestLineageHTTPDefaultWalkIsUnchanged(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	path := "/v1/lineage/task/" + f.oldTask
	response := f.get(t, path)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := authenticatedMemoryRead(f.server, httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.ctx))
	graph, err := f.server.lineageGraph(request, core.LineageNode{Type: core.LineageTask, ID: f.oldTask},
		core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes, MaxLinks: config.DefaultLineageContextNodes * config.DefaultLineageContextLinksPerNode})
	if err != nil {
		t.Fatal(err)
	}
	want := httptest.NewRecorder()
	writeJSON(want, http.StatusOK, graph)
	if response.Body.String() != want.Body.String() {
		t.Fatalf("default walk changed\ngot  %s\nwant %s", response.Body.String(), want.Body.String())
	}
	for _, field := range []string{`"depth"`, `"edge_path"`, `"status"`, `"filter"`} {
		if strings.Contains(response.Body.String(), field) {
			t.Fatalf("default walk carries %s: %s", field, response.Body.String())
		}
	}
	if both := f.get(t, path+"?direction=both"); both.Body.String() != response.Body.String() {
		t.Fatalf("direction=both differs from the default walk\n%s\n%s", both.Body.String(), response.Body.String())
	}
}

func TestLineageHTTPDirectionKindAndNodeTypeFilters(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	version1 := "requirement_version:" + core.RequirementVersionLineageID(f.requirement, 1)
	version2 := "requirement_version:" + core.RequirementVersionLineageID(f.requirement, 2)
	root := "/v1/lineage/requirement_version/" + core.RequirementVersionLineageID(f.requirement, 1)
	// Out of v1 is only the task it served, then that task's own out-links.
	out := f.walk(t, root+"?direction=out&max_depth=1")
	if got, want := nodeKeys(out.Nodes), []string{version1, "task:" + f.oldTask}; !reflect.DeepEqual(got, want) {
		t.Fatalf("direction=out nodes=%v want %v", got, want)
	}
	// Into v1 are its requirement and the version that superseded it.
	in := f.walk(t, root+"?direction=in&max_depth=1")
	if got, want := nodeKeys(in.Nodes), []string{"requirement:" + f.requirement, version1, version2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("direction=in nodes=%v want %v", got, want)
	}
	if in.Budget.Filter == nil || in.Budget.Filter.Direction != core.LineageDirectionIn {
		t.Fatalf("filtered walk budget=%+v", in.Budget)
	}
	// A kind filter follows only serves, so the requirement reaches tasks
	// and never its versions.
	served := f.walk(t, "/v1/lineage/requirement/"+f.requirement+"?kind=serves&max_depth=1")
	if got, want := nodeKeys(served.Nodes), []string{"requirement:" + f.requirement, "task:" + f.newTask, "task:" + f.oldTask, "task:" + f.running}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kind=serves nodes=%v want %v", got, want)
	}
	for _, link := range served.Links {
		if link.Kind != "serves" {
			t.Fatalf("kind filter returned link %+v", link)
		}
	}
	// A node-type filter admits only the named types beyond the root.
	typed := f.walk(t, "/v1/lineage/task/"+f.oldTask+"?node_type=work_order,verdict")
	if got, want := nodeKeys(typed.Nodes), []string{"task:" + f.oldTask, "verdict:" + core.VerdictLineageID(f.reviewOrder), "work_order:" + f.reviewOrder}; !reflect.DeepEqual(got, want) {
		t.Fatalf("node_type nodes=%v want %v", got, want)
	}
	for _, query := range []string{"direction=sideways", "direction=out&direction=in", "kind=volunteered", "node_type=feature", "include=everything", "max_nodes=0"} {
		if response := f.get(t, root+"?"+query); response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}

func TestLineageHTTPIncludesDepthPathAndStatus(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	graph := f.walk(t, "/v1/lineage/task/"+f.oldTask+"?include=depth,path&include=status")
	byKey := map[string]lineageNodeView{}
	for _, node := range graph.Nodes {
		byKey[string(node.Type)+":"+node.ID] = node
		if node.Depth == nil || len(node.EdgePath) != *node.Depth {
			t.Fatalf("node %s depth=%v path=%v", node.ID, node.Depth, node.EdgePath)
		}
		if len(node.EdgePath) > 0 {
			last := node.EdgePath[len(node.EdgePath)-1]
			if !(last.SrcType == node.Type && last.SrcID == node.ID) && !(last.DstType == node.Type && last.DstID == node.ID) {
				t.Fatalf("node %s path ends at %+v", node.ID, last)
			}
		}
	}
	verdict := byKey["verdict:"+core.VerdictLineageID(f.reviewOrder)]
	if verdict.Status == nil || *verdict.Status != (core.LineageNodeStatus{Outcome: "approve", ReviewRound: 1, ReviewSeat: 2}) || *verdict.Depth != 2 {
		t.Fatalf("verdict view=%+v status=%+v", verdict, verdict.Status)
	}
	root := byKey["task:"+f.oldTask]
	if root.Status == nil || root.Status.State != string(core.TaskMerged) || *root.Depth != 0 || root.EdgePath != nil {
		t.Fatalf("root view=%+v", root)
	}
	version := byKey["requirement_version:"+core.RequirementVersionLineageID(f.requirement, 1)]
	if version.Status == nil || *version.Status != (core.LineageNodeStatus{VersionStatus: core.LineageVersionSuperseded, CurrentVersion: 2}) {
		t.Fatalf("version view=%+v", version)
	}
	designVersion := byKey["system_design_version:"+core.SystemDesignVersionLineageID(f.design, 1)]
	if designVersion.Status == nil || designVersion.Status.VersionStatus != core.LineageVersionSuperseded {
		t.Fatalf("design version view=%+v", designVersion)
	}
	order := byKey["work_order:"+f.reviewOrder]
	if order.Status == nil || order.Status.State != string(core.WorkOrderQueued) {
		t.Fatalf("work order view=%+v", order)
	}
	// Depth alone adds only depth.
	depthOnly := f.get(t, "/v1/lineage/task/"+f.oldTask+"?include=depth")
	if !strings.Contains(depthOnly.Body.String(), `"depth":0`) || strings.Contains(depthOnly.Body.String(), `"edge_path"`) || strings.Contains(depthOnly.Body.String(), `"status"`) {
		t.Fatalf("include=depth body=%s", depthOnly.Body.String())
	}
}

func TestLineageHTTPOperatorReadBudgetIsSeparateAndBounded(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{Depth: 2, Nodes: 8, OperatorReadDepth: 4, OperatorReadNodes: 16})
	path := "/v1/lineage/task/" + f.oldTask
	defaults := f.walk(t, path)
	if defaults.Budget.MaxDepth != 2 || defaults.Budget.MaxNodes != 8 || defaults.Budget.MaxLinks != 32 {
		t.Fatalf("default budget=%+v, want the context budget", defaults.Budget)
	}
	wide := f.walk(t, path+"?max_depth=4&max_nodes=16&max_links=64")
	if wide.Budget.MaxDepth != 4 || wide.Budget.MaxNodes != 16 {
		t.Fatalf("operator budget=%+v", wide.Budget)
	}
	for _, query := range []string{"max_depth=5", "max_nodes=17", "max_links=65"} {
		response := f.get(t, path+"?"+query)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "operator read budget") {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}

func TestLineageHTTPImpactListsOnlyMergedWorkOnOldVersions(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	impact := func(t *testing.T, path string) lineageImpactView {
		t.Helper()
		response := f.get(t, path)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var view lineageImpactView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	summary := func(view lineageImpactView) []string {
		out := []string{}
		for _, item := range view.Items {
			out = append(out, strings.Join([]string{item.Task.ID, item.VersionStatus, item.TaskState, strings.Join(item.MergedRanges, " ")}, "|"))
		}
		sort.Strings(out)
		return out
	}
	for _, root := range []string{"requirement/" + f.requirement, "system_design/" + f.design} {
		all := impact(t, "/v1/lineage/"+root+"/impact")
		want := []string{
			f.newTask + "|current|merged|" + core.CommitRangeLineageID("conveyor", "new-base", "new-head"),
			f.oldTask + "|superseded|merged|" + core.CommitRangeLineageID("conveyor", "old-base", "old-head"),
		}
		want = append(want, f.running+"|current|running|")
		sort.Strings(want)
		if got := summary(all); all.CurrentVersion != 2 || all.Total != len(want) || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s impact=%v total=%d current=%d\nwant %v", root, got, all.Total, all.CurrentVersion, want)
		}
		outdated := impact(t, "/v1/lineage/"+root+"/impact?outdated=true")
		if got := summary(outdated); outdated.Total != 1 || !reflect.DeepEqual(got, want[1:2]) || outdated.Items[0].Version != 1 || !outdated.OutdatedOnly {
			t.Fatalf("%s outdated impact=%v", root, got)
		}
	}
	paged := impact(t, "/v1/lineage/requirement/"+f.requirement+"/impact?limit=1")
	if len(paged.Items) != 1 || paged.Total != 3 || paged.NextOffset == nil || *paged.NextOffset != 1 {
		t.Fatalf("paged impact=%+v", paged)
	}
	last := impact(t, "/v1/lineage/requirement/"+f.requirement+"/impact?limit=1&offset=2")
	if len(last.Items) != 1 || last.NextOffset != nil {
		t.Fatalf("last impact page=%+v", last)
	}
	for path, status := range map[string]int{
		"/v1/lineage/task/" + f.oldTask + "/impact":                       http.StatusBadRequest,
		"/v1/lineage/requirement/absent/impact":                           http.StatusNotFound,
		"/v1/lineage/requirement/" + f.requirement + "/impact?limit=101":  http.StatusBadRequest,
		"/v1/lineage/requirement/" + f.requirement + "/impact?offset=4":   http.StatusBadRequest,
		"/v1/lineage/requirement/" + f.requirement + "/impact?outdated=x": http.StatusBadRequest,
	} {
		if response := f.get(t, path); response.Code != status {
			t.Fatalf("GET %s status=%d want %d body=%s", path, response.Code, status, response.Body.String())
		}
	}
}

// TestLineageHTTPImpactNeverCallsATruncatedVersionUnrecorded proves the
// impact read recovers a task's recorded version from the task side when the
// version read leaves that version out, and when the version-edge read is
// truncated. Neither case may report the task as unrecorded
// (component-lineage; req-260802-72fc68 AC-5.2).
func TestLineageHTTPImpactNeverCallsATruncatedVersionUnrecorded(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	restore := func(versions, entries int) func() {
		previousVersions, previousEntries := lineageImpactMaxVersions, lineageImpactMaxEntries
		lineageImpactMaxVersions, lineageImpactMaxEntries = versions, entries
		return func() { lineageImpactMaxVersions, lineageImpactMaxEntries = previousVersions, previousEntries }
	}
	read := func(t *testing.T, query string) lineageImpactView {
		t.Helper()
		response := f.get(t, "/v1/lineage/requirement/"+f.requirement+"/impact"+query)
		if response.Code != http.StatusOK {
			t.Fatalf("impact status=%d body=%s", response.Code, response.Body.String())
		}
		var view lineageImpactView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	byTask := func(view lineageImpactView) map[string]lineageImpactEntry {
		out := map[string]lineageImpactEntry{}
		for _, item := range view.Items {
			out[item.Task.ID] = item
		}
		return out
	}
	t.Run("version outside the selected versions", func(t *testing.T) {
		defer restore(1, 1000)()
		view := read(t, "")
		old := byTask(view)[f.oldTask]
		if !view.Truncated || !reflect.DeepEqual(view.ExhaustionReasons, []string{"versions"}) || view.Total != 3 ||
			old.Version != 1 || old.VersionStatus != core.LineageVersionSuperseded {
			t.Fatalf("impact=%+v old=%+v", view, old)
		}
		outdated := read(t, "?outdated=true")
		if outdated.Total != 1 || outdated.Items[0].Task.ID != f.oldTask {
			t.Fatalf("outdated impact=%+v", outdated)
		}
	})
	t.Run("truncated version links", func(t *testing.T) {
		defer restore(1, 1)()
		view := read(t, "")
		old := byTask(view)[f.oldTask]
		if !view.Truncated || old.Version != 1 || old.VersionStatus != core.LineageVersionSuperseded {
			t.Fatalf("impact=%+v old=%+v", view, old)
		}
		for _, item := range view.Items {
			if item.VersionStatus == lineageVersionUnrecorded {
				t.Fatalf("truncated read called %s unrecorded: %+v", item.Task.ID, view)
			}
		}
	})
	t.Run("an attachment without a version is unrecorded", func(t *testing.T) {
		task := core.Task{ID: "task-lineage-legacy", Workspace: "demo", Title: "Legacy attachment", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-task-lineage-legacy", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
		if err := f.server.Store.CreateTask(f.ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := f.server.Store.AppendEvent(f.ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": f.requirement})}); err != nil {
			t.Fatal(err)
		}
		view := read(t, "")
		legacy := byTask(view)[task.ID]
		if view.Truncated || legacy.VersionStatus != lineageVersionUnrecorded || legacy.Version != 0 {
			t.Fatalf("impact=%+v legacy=%+v", view, legacy)
		}
		if outdated := read(t, "?outdated=true"); outdated.Total != 1 || outdated.Items[0].Task.ID != f.oldTask {
			t.Fatalf("an unrecorded version counted as outdated: %+v", outdated)
		}
	})
}

// TestLineageHTTPImpactBoundsCombinedTasksAboveTheNodeCap seeds 25 earlier
// attachments without a version and 1005 later versioned attachments, so the
// versioned and fallback candidates together exceed the 1024-node read bound.
// The read stays a bounded, truncated page (component-lineage;
// req-260802-72fc68 AC-5.2).
func TestLineageHTTPImpactBoundsCombinedTasksAboveTheNodeCap(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	attach := func(id string, version int) {
		t.Helper()
		task := core.Task{ID: id, Workspace: "demo", Title: id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskMerged, CreatedAt: time.Now().UTC()}
		if err := f.server.Store.CreateTask(f.ctx, task); err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{"id": f.requirement}
		if version > 0 {
			payload["version"] = version
		}
		if err := f.server.Store.AppendEvent(f.ctx, core.Event{TaskID: id, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 25; index++ {
		attach(fmt.Sprintf("task-legacy-%04d", index), 0)
	}
	for index := 0; index < 1005; index++ {
		attach(fmt.Sprintf("task-versioned-%04d", index), 1)
	}
	for _, query := range []string{"?limit=1", "?limit=100&offset=900", "?outdated=true&limit=1"} {
		response := f.get(t, "/v1/lineage/requirement/"+f.requirement+"/impact"+query)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
		var view lineageImpactView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if !view.Truncated || !slices.Contains(view.ExhaustionReasons, "tasks") || view.Total > lineageImpactMaxEntries || len(view.Items) == 0 {
			t.Fatalf("%s impact total=%d truncated=%v reasons=%v items=%d", query, view.Total, view.Truncated, view.ExhaustionReasons, len(view.Items))
		}
	}
}

// TestLineageHTTPImpactKeepsTheNewestVersionWindow confirms more design
// versions than the version cap and governs a task on the newest one. The
// read keeps the newest window, so that task stays in scope
// (component-lineage; req-260802-72fc68 AC-5.2).
func TestLineageHTTPImpactKeepsTheNewestVersionWindow(t *testing.T) {
	f := newLineageReadsFixture(t, config.LineageContextSettings{})
	governs := "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/lineage/**\n```"
	current := 2
	for number := 3; number <= 5; number++ {
		proposed, err := f.server.Store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: f.design, Content: fmt.Sprintf("# Lineage reads design v%d", number) + governs, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.server.Store.ConfirmSystemDesignVersion(f.ctx, f.design, proposed.Version, current); err != nil {
			t.Fatal(err)
		}
		current = proposed.Version
	}
	task := core.Task{ID: "task-lineage-newest", Workspace: "demo", Title: "Newest design", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-task-lineage-newest", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := f.server.Store.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Store.AppendEvent(f.ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": f.design, "version": current})}); err != nil {
		t.Fatal(err)
	}
	previous := lineageImpactMaxVersions
	lineageImpactMaxVersions = 2
	defer func() { lineageImpactMaxVersions = previous }()
	response := f.get(t, "/v1/lineage/system_design/"+f.design+"/impact")
	if response.Code != http.StatusOK {
		t.Fatalf("impact status=%d body=%s", response.Code, response.Body.String())
	}
	var view lineageImpactView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentVersion != current || !view.Truncated || !slices.Contains(view.ExhaustionReasons, "versions") || view.Total != 1 ||
		view.Items[0].Task.ID != task.ID || view.Items[0].Version != current || view.Items[0].VersionStatus != core.LineageVersionCurrent {
		t.Fatalf("newest window impact=%+v", view)
	}
}
