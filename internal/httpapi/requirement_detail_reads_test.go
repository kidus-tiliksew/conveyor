package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

const (
	requirementDetailTestBody = "REQUIREMENT-DETAIL-TEST-BODY"
	requirementDetailTestPlan = "REQUIREMENT-DETAIL-TEST-PLAN"
)

func requirementDetailRecorder(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, response.Code, response.Body.String())
	}
	return response
}

func objectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The detail's serving_tasks and serving_blueprints carry only the fields the
// dashboard reads: no task body and no plan content
// (component-document-corpus; component-web-document-surfaces).
func TestRequirementDetailTaskFieldsAreCompact(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := store.NewMemory()
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-compact", Title: "Compact detail"}, core.RequirementVersion{
		Content: "# Compact detail", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The detail stays compact."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	newTask := func(id, title, parent string, state core.TaskState) {
		t.Helper()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id,
			Title: title, Body: requirementDetailTestBody + " " + title, State: state, ParentTaskID: parent, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	attach := func(id string) {
		t.Helper()
		if _, err := st.UpdateTaskContext(ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
			t.Fatal(err)
		}
	}
	link := func(id string) {
		t.Helper()
		if _, err := st.ProposeRequirementServes(ctx, id, requirement.ID, core.RequirementServesOperator, true); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(id string, approve bool) {
		t.Helper()
		spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: requirementDetailTestPlan, Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
		if err != nil {
			t.Fatal(err)
		}
		if approve {
			if err = st.ApproveSpecVersion(ctx, id, spec.Version); err != nil {
				t.Fatal(err)
			}
		}
	}
	newTask("task-b", "Context task", "", core.TaskRunning)
	attach("task-b")
	newTask("task-c", "Linked task without a plan", "", core.TaskQueued)
	link("task-c")
	newTask("task-a", "Linked task with plans", "", core.TaskMerged)
	link("task-a")
	plan("task-a", true)
	plan("task-a", false)
	newTask("task-anchor", "Anchor", "", core.TaskRunning)
	newTask("task-anchor-child", "Anchor child", "task-anchor", core.TaskQueued)
	attach("task-anchor")

	server := NewServer(st)
	server.Workspace = "demo"
	response := requirementDetailRecorder(t, server, "/v1/requirements/"+requirement.ID)
	body := response.Body.String()
	for _, sentinel := range []string{requirementDetailTestBody, requirementDetailTestPlan, `"body"`, `"content":"` + requirementDetailTestPlan} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("detail carries %q: %s", sentinel, body)
		}
	}
	var detail map[string]any
	if err = json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	taskKeys := []string{"id", "state", "title"}
	servingIDs := []string{}
	for _, raw := range detail["serving_tasks"].([]any) {
		item := raw.(map[string]any)
		if keys := objectKeys(item); !reflect.DeepEqual(keys, taskKeys) {
			t.Fatalf("serving task keys = %v, want %v", keys, taskKeys)
		}
		servingIDs = append(servingIDs, item["id"].(string))
	}
	// Sorted by ID, and the anchor with a child is excluded.
	if want := []string{"task-a", "task-b", "task-c"}; !reflect.DeepEqual(servingIDs, want) {
		t.Fatalf("serving task IDs = %v, want %v", servingIDs, want)
	}
	links := []string{}
	for _, raw := range detail["requirement_links"].([]any) {
		item := raw.(map[string]any)
		if item["state"] == string(core.RequirementServesConfirmed) {
			links = append(links, item["blueprint_task_id"].(string))
		}
	}
	blueprints := detail["serving_blueprints"].([]any)
	if len(blueprints) != 2 || len(links) != 2 {
		t.Fatalf("serving blueprints = %v, confirmed links = %v", blueprints, links)
	}
	for index, raw := range blueprints {
		item := raw.(map[string]any)
		task := item["task"].(map[string]any)
		if keys := objectKeys(task); !reflect.DeepEqual(keys, taskKeys) {
			t.Fatalf("blueprint task keys = %v, want %v", keys, taskKeys)
		}
		// Blueprint rows keep the confirmed link order.
		if task["id"] != links[index] {
			t.Fatalf("blueprint %d = %v, want link order %v", index, task["id"], links)
		}
		switch task["id"] {
		case "task-a":
			if keys := objectKeys(item); !reflect.DeepEqual(keys, []string{"spec", "task"}) {
				t.Fatalf("blueprint keys = %v", keys)
			}
			spec := item["spec"].(map[string]any)
			if !reflect.DeepEqual(spec, map[string]any{"version": float64(2), "approved": false}) {
				t.Fatalf("blueprint spec = %v", spec)
			}
			if task["title"] != "Linked task with plans" || task["state"] != string(core.TaskMerged) {
				t.Fatalf("blueprint task = %v", task)
			}
		case "task-c":
			if keys := objectKeys(item); !reflect.DeepEqual(keys, []string{"task"}) {
				t.Fatalf("a blueprint without a plan has keys %v", keys)
			}
		default:
			t.Fatalf("unexpected blueprint %v", task)
		}
	}

	// The list keeps its serving task summaries.
	list := requirementDetailRecorder(t, server, "/v1/requirements")
	var summaries []map[string]any
	if err = json.Unmarshal(list.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("list = %v", summaries)
	}
	listIDs := []string{}
	for _, raw := range summaries[0]["serving_tasks"].([]any) {
		item := raw.(map[string]any)
		if keys := objectKeys(item); !reflect.DeepEqual(keys, taskKeys) {
			t.Fatalf("list serving task keys = %v", keys)
		}
		listIDs = append(listIDs, item["id"].(string))
	}
	if !reflect.DeepEqual(listIDs, servingIDs) {
		t.Fatalf("list serving IDs = %v, detail %v", listIDs, servingIDs)
	}
}

// The detail reads its tasks and their plans with the same store calls for a
// requirement with 2 serving tasks and one with 40 serving tasks and 10
// confirmed links with plan versions (component-persistence). The database
// statement-count tests on PostgreSQL and SingleStore read the same fixture.
func TestRequirementDetailStoreCallsDoNotGrowWithTasks(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	inner := store.NewMemory()
	small := storetest.SeedRequirementDetail(t, inner, ctx, "small", 2, 1)
	large := storetest.SeedRequirementDetail(t, inner, ctx, "large", 40, 10)
	// Each requirement has its own artifacts, and its serving tasks have
	// artifacts the detail does not show.
	artifactCounts := map[string]int{small: 1, large: 3}
	for _, id := range []string{small, large} {
		for index := 0; index < artifactCounts[id]; index++ {
			if _, err := inner.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("%s-%d.txt", id, index), ContentType: "text/plain",
				Role: core.ArtifactRoleTaskContext, RequirementID: id}, []byte(fmt.Sprintf("artifact %d of %s", index, id))); err != nil {
				t.Fatal(err)
			}
		}
	}
	tasks, err := inner.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if _, err = inner.CreateArtifact(ctx, core.Artifact{Name: task.ID + ".txt", ContentType: "text/plain",
			Role: core.ArtifactRoleTaskContext, TaskID: task.ID}, []byte("artifact of "+task.ID)); err != nil {
			t.Fatal(err)
		}
	}
	counter := &requirementOwnerCountingStore{taskDetailCountingStore: newTaskDetailCountingStore(inner)}
	server := NewServer(counter)
	server.Workspace = "demo"
	read := func(id string, serving, linked int) (map[string]int, int) {
		t.Helper()
		requirementDetailRecorder(t, server, "/v1/requirements/"+id) // warm any first-request work
		counter.reset()
		counter.takeOwners()
		response := requirementDetailRecorder(t, server, "/v1/requirements/"+id)
		calls := counter.reset()
		var view requirementView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if len(view.ServingTasks) != serving || len(view.ServingBlueprints) != linked || view.Staleness.PartialEvaluation {
			t.Fatalf("%s fixture: serving=%d blueprints=%d partial=%t", id, len(view.ServingTasks), len(view.ServingBlueprints), view.Staleness.PartialEvaluation)
		}
		if len(view.Artifacts) != artifactCounts[id] {
			t.Fatalf("%s fixture: artifacts = %+v", id, view.Artifacts)
		}
		for _, artifact := range view.Artifacts {
			if artifact.RequirementID != id || artifact.TaskID != "" {
				t.Fatalf("%s fixture: artifact %+v", id, artifact)
			}
		}
		if owners := counter.takeOwners(); len(owners) != 1 || !reflect.DeepEqual(owners[0], store.ArtifactOwners{RequirementIDs: []string{id}}) {
			t.Fatalf("%s fixture: artifact owner reads = %+v", id, owners)
		}
		for _, blueprint := range view.ServingBlueprints {
			if blueprint.Spec == nil || *blueprint.Spec != (requirementPlanSummary{Version: 2, Approved: false}) {
				t.Fatalf("%s blueprint = %+v", id, blueprint)
			}
		}
		return calls, response.Body.Len()
	}
	smallCalls, smallBytes := read(small, 2, 1)
	largeCalls, largeBytes := read(large, 40, 10)
	if !reflect.DeepEqual(smallCalls, largeCalls) {
		t.Fatalf("store calls grew with tasks:\nsmall %v\nlarge %v", smallCalls, largeCalls)
	}
	for _, method := range []string{"GetTask", "GetLatestSpecVersion", "ListCompactEvents", "GetTaskByIntakeKey"} {
		if smallCalls[method] != 0 {
			t.Fatalf("detail called %s %d times: %v", method, smallCalls[method], smallCalls)
		}
	}
	if smallCalls["ListRequirementDetailTasks"] != 1 || smallCalls["ListLatestPlanSummaries"] != 1 {
		t.Fatalf("detail batch reads = %v", smallCalls)
	}
	// The detail reads its display links and artifacts directly: no
	// neighborhood walk, no lineage artifact read, and no whole-workspace
	// scan (component-lineage; component-artifacts).
	for method, want := range map[string]int{"ListLineageNeighborhood": 0, "ListArtifactsForLineage": 0, "ListLineageLinks": 0, "ListArtifacts": 0,
		"ListLineageKindLinks": 1, "ListArtifactsForOwners": 1, "ListRequirementDeliveryLineage": 1, "ListRequirementDeliveryEventsForTasks": 1} {
		if smallCalls[method] != want {
			t.Fatalf("detail called %s %d times, want %d: %v", method, smallCalls[method], want, smallCalls)
		}
	}
	t.Logf("MEASUREMENT memory requirement detail store calls=%d small_bytes=%d large_bytes=%d", totalCalls(smallCalls), smallBytes, largeBytes)
}

func totalCalls(calls map[string]int) int {
	total := 0
	for _, count := range calls {
		total += count
	}
	return total
}

// requirementDisplayFixture builds one confirmed requirement and the tasks,
// links, and artifacts around it for the detail display tests.
type requirementDisplayFixture struct {
	t           *testing.T
	ctx         context.Context
	st          store.Store
	requirement core.Requirement
	created     time.Time
	nextEventID int64
}

func newRequirementDisplayFixture(t *testing.T, st store.Store, id string) *requirementDisplayFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Display " + id}, core.RequirementVersion{
		Content: "# Display " + id, Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The detail lists the tasks that serve it."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	return &requirementDisplayFixture{t: t, ctx: ctx, st: st, requirement: requirement, created: time.Now().UTC().Add(-time.Hour), nextEventID: 1 << 40}
}

func (f *requirementDisplayFixture) task(id, parent string) {
	f.t.Helper()
	if err := f.st.CreateTask(f.ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id,
		Title: "Task " + id, State: core.TaskRunning, ParentTaskID: parent, CreatedAt: time.Now().UTC()}); err != nil {
		f.t.Fatal(err)
	}
}

// attach projects requirement -serves-> task through task context.
func (f *requirementDisplayFixture) attach(id string) {
	f.t.Helper()
	if _, err := f.st.UpdateTaskContext(f.ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{f.requirement.ID}}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *requirementDisplayFixture) link(id string, confirm bool) {
	f.t.Helper()
	if _, err := f.st.ProposeRequirementServes(f.ctx, id, f.requirement.ID, core.RequirementServesOperator, confirm); err != nil {
		f.t.Fatal(err)
	}
}

// inbound returns a legacy task -serves-> requirement link. The projector no
// longer writes this direction, so the tests inject it through
// requirementDisplayLinkStore.
func (f *requirementDisplayFixture) inbound(taskID string) core.LineageLink {
	f.nextEventID++
	f.created = f.created.Add(time.Second)
	return core.LineageLink{Workspace: "demo", SrcType: core.LineageTask, SrcID: taskID, DstType: core.LineageRequirement, DstID: f.requirement.ID,
		Kind: "serves", CreatedByEventID: f.nextEventID, CreatedAt: f.created}
}

func (f *requirementDisplayFixture) artifact(name string, owner core.Artifact) core.Artifact {
	f.t.Helper()
	owner.Name, owner.ContentType = name, "text/plain"
	if owner.Role == "" {
		owner.Role = core.ArtifactRoleTaskContext
	}
	artifact, err := f.st.CreateArtifact(f.ctx, owner, []byte(name))
	if err != nil {
		f.t.Fatal(err)
	}
	return artifact
}

// requirementDisplayLinkStore adds lineage links that the memory projector
// cannot write. Its neighborhood and kind-link reads follow the store
// contract over the inner links plus the added ones: the neighborhood asks
// core.FetchLineageNeighborhood for ranked prefixes, and the kind-link read
// keeps the first Limit links in core.SortLineageKindLinks order
// (component-lineage).
type requirementDisplayLinkStore struct {
	store.Store
	extra       []core.LineageLink
	kindQueries []core.LineageKindLinkQuery
}

func (s *requirementDisplayLinkStore) allLinks(ctx context.Context) ([]core.LineageLink, error) {
	links, err := s.Store.ListLineageLinks(ctx)
	if err != nil {
		return nil, err
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	for _, link := range s.extra {
		if link.Workspace == workspace {
			links = append(links, link)
		}
	}
	return links, nil
}

func (s *requirementDisplayLinkStore) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	links, err := s.allLinks(ctx)
	if err != nil {
		return nil, err
	}
	budget.Workspace, _ = store.WorkspaceFromContext(ctx)
	return core.FetchLineageNeighborhood(ctx, roots, budget, func(_ context.Context, nodes []core.LineageNode, limit int) (map[core.LineageNode][]core.LineageLink, error) {
		out := make(map[core.LineageNode][]core.LineageLink, len(nodes))
		for _, node := range nodes {
			incident := []core.LineageLink{}
			for _, link := range links {
				if (link.SrcType == node.Type && link.SrcID == node.ID) || (link.DstType == node.Type && link.DstID == node.ID) {
					incident = append(incident, link)
				}
			}
			out[node] = core.RankLineageLinks(node, incident, limit)
		}
		return out, nil
	})
}

func (s *requirementDisplayLinkStore) ListLineageKindLinks(ctx context.Context, query core.LineageKindLinkQuery) ([]core.LineageLink, error) {
	s.kindQueries = append(s.kindQueries, query)
	if err := query.Validate(); err != nil {
		return nil, err
	}
	links, err := s.allLinks(ctx)
	if err != nil {
		return nil, err
	}
	nodes := map[core.LineageNode]bool{}
	for _, node := range query.Nodes {
		nodes[node] = true
	}
	out := []core.LineageLink{}
	for _, link := range links {
		near, far := core.LineageNode{Type: link.SrcType, ID: link.SrcID}, link.DstType
		if query.Inbound {
			near, far = core.LineageNode{Type: link.DstType, ID: link.DstID}, link.SrcType
		}
		if link.Kind == query.Kind && nodes[near] && far == query.FarType && link.Validate() == nil {
			out = append(out, link)
		}
	}
	core.SortLineageKindLinks(out)
	if len(out) > query.Limit {
		if query.Newest {
			out = out[len(out)-query.Limit:]
		} else {
			out = out[:query.Limit]
		}
	}
	return out, nil
}

// legacyRequirementDisplay recomputes serving_tasks and artifacts as the
// detail read did before it dropped the 5-hop neighborhood walk: the
// directly serving tasks among the fetched neighborhood links, without tasks
// that have children, in task-ID order, and the requirement's own artifacts
// among those of the traversed nodes. It is the parity oracle for the direct
// reads that replaced the walk.
func legacyRequirementDisplay(t *testing.T, st store.Store, ctx context.Context, requirementID string) ([]requirementTaskSummary, []core.Artifact) {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	root := core.LineageNode{Type: core.LineageRequirement, ID: requirementID}
	budget := core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 256, MaxLinks: 1024, Workspace: workspace}
	links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{root}, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(links, []core.LineageNode{root}, budget)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := st.ListArtifactsForLineage(ctx, graph.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := []core.Artifact{}
	for _, artifact := range listed {
		if artifact.RequirementID == requirementID {
			artifacts = append(artifacts, artifact)
		}
	}
	ids := sortedIDs(directServingTaskIDs(links, requirementID))
	tasks, err := st.ListRequirementDetailTasks(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	serving := []requirementTaskSummary{}
	for _, id := range ids {
		task, found := tasks[id]
		if !found {
			t.Fatalf("legacy serving task %s has no summary", id)
		}
		if !task.HasChildren {
			serving = append(serving, requirementTaskSummaryOf(task))
		}
	}
	return serving, artifacts
}

func readRequirementDetail(t *testing.T, st store.Store, id string) (requirementView, map[string]any) {
	t.Helper()
	server := NewServer(st)
	server.Workspace = "demo"
	response := requirementDetailRecorder(t, server, "/v1/requirements/"+id)
	var view requirementView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	return view, object
}

func assertSameJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s differs from the neighborhood walk:\n got %s\nwant %s", label, gotJSON, wantJSON)
	}
}

func servingTaskIDs(tasks []requirementTaskSummary) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// The direct reads give the detail the same serving tasks and artifacts as
// the neighborhood walk did: outbound and legacy inbound serves links, one
// entry for a task linked in both directions, no task with children, the
// requirement's own artifacts in order, and no artifact of a serving task.
// Requirement links, blueprints, and staleness keep their values
// (component-lineage; component-artifacts).
func TestRequirementDetailDirectReadsMatchNeighborhoodWalk(t *testing.T) {
	inner := store.NewMemory()
	f := newRequirementDisplayFixture(t, inner, "req-display-parity")
	other := newRequirementDisplayFixture(t, inner, "req-display-other")
	for _, id := range []string{"out-a", "out-b", "both", "parent", "linked", "pending", "elsewhere"} {
		f.task(id, "")
	}
	f.task("parent-child", "parent")
	f.task("inbound-only", "")
	f.task("inbound-parent", "")
	f.task("inbound-parent-child", "inbound-parent")
	for _, id := range []string{"out-a", "out-b", "both", "parent"} {
		f.attach(id)
	}
	f.link("linked", true)
	f.link("pending", false)
	other.attach("elsewhere")
	spec, err := inner.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: "linked", Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
	if err != nil {
		t.Fatal(err)
	}
	if err = inner.ApproveSpecVersion(f.ctx, "linked", spec.Version); err != nil {
		t.Fatal(err)
	}
	first := f.artifact("first.txt", core.Artifact{RequirementID: f.requirement.ID})
	second := f.artifact("second.txt", core.Artifact{RequirementID: f.requirement.ID, Role: core.ArtifactRoleGeneratedOutput})
	f.artifact("task-only.txt", core.Artifact{TaskID: "out-a"})
	f.artifact("other-requirement.txt", core.Artifact{RequirementID: other.requirement.ID})

	// The link store reads the memory store's own links like the memory
	// store does before any link is added.
	plain := &requirementDisplayLinkStore{Store: inner}
	root := core.LineageNode{Type: core.LineageRequirement, ID: f.requirement.ID}
	budget := core.LineageTraversalBudget{MaxDepth: 5, MaxNodes: 256, MaxLinks: 1024, Workspace: "demo"}
	wantNeighborhood, err := inner.ListLineageNeighborhood(f.ctx, []core.LineageNode{root}, budget)
	if err != nil {
		t.Fatal(err)
	}
	gotNeighborhood, err := plain.ListLineageNeighborhood(f.ctx, []core.LineageNode{root}, budget)
	if err != nil || !reflect.DeepEqual(gotNeighborhood, wantNeighborhood) {
		t.Fatalf("link store neighborhood = %v err=%v, want %v", gotNeighborhood, err, wantNeighborhood)
	}
	query := core.LineageKindLinkQuery{Nodes: []core.LineageNode{root}, Kind: "serves", FarType: core.LineageTask, Limit: 10}
	wantKind, err := inner.ListLineageKindLinks(f.ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if gotKind, kindErr := plain.ListLineageKindLinks(f.ctx, query); kindErr != nil || !reflect.DeepEqual(gotKind, wantKind) {
		t.Fatalf("link store kind links = %v err=%v, want %v", gotKind, kindErr, wantKind)
	}

	st := &requirementDisplayLinkStore{Store: inner, extra: []core.LineageLink{f.inbound("both"), f.inbound("inbound-only"), f.inbound("inbound-parent")}}
	view, _ := readRequirementDetail(t, st, f.requirement.ID)
	legacyServing, legacyArtifacts := legacyRequirementDisplay(t, st, f.ctx, f.requirement.ID)
	assertSameJSON(t, "serving_tasks", view.ServingTasks, legacyServing)
	assertSameJSON(t, "artifacts", view.Artifacts, legacyArtifacts)
	if got, want := servingTaskIDs(view.ServingTasks), []string{"both", "inbound-only", "linked", "out-a", "out-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("serving task IDs = %v, want %v", got, want)
	}
	if len(view.Artifacts) != 2 || view.Artifacts[0].ID != first.ID || view.Artifacts[1].ID != second.ID ||
		view.Artifacts[1].Role != core.ArtifactRoleGeneratedOutput {
		t.Fatalf("artifacts = %+v", view.Artifacts)
	}
	links := map[string]core.RequirementServesState{}
	for _, link := range view.RequirementLinks {
		links[link.BlueprintTaskID] = link.State
	}
	if len(links) != 2 || links["linked"] != core.RequirementServesConfirmed || links["pending"] == core.RequirementServesConfirmed {
		t.Fatalf("requirement links = %+v", view.RequirementLinks)
	}
	if len(view.ServingBlueprints) != 1 || view.ServingBlueprints[0].Task.ID != "linked" || view.ServingBlueprints[0].Spec == nil ||
		*view.ServingBlueprints[0].Spec != (requirementPlanSummary{Version: 1, Approved: true}) {
		t.Fatalf("serving blueprints = %+v", view.ServingBlueprints)
	}
	if view.Staleness.PartialEvaluation || view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 0 {
		t.Fatalf("staleness = %+v", view.Staleness)
	}
	// One bounded inbound read names only the requirement.
	if len(st.kindQueries) != 1 {
		t.Fatalf("kind-link reads = %+v", st.kindQueries)
	}
	if got := st.kindQueries[0]; !reflect.DeepEqual(got.Nodes, []core.LineageNode{root}) || got.Kind != "serves" || !got.Inbound ||
		got.FarType != core.LineageTask || got.Limit != requirementDetailServingTaskLimit+1 || got.Newest {
		t.Fatalf("inbound read = %+v", got)
	}
}

// The detail lists at most 255 serving tasks. At 255 it matches the
// neighborhood walk exactly. Above 255 it lists 255 of the tasks the walk
// listed, in task-ID order, with the same response shape, and the cut sets
// no marker: staleness.partial_evaluation still reports only delivery
// truncation.
func TestRequirementDetailCapsServingTasksAt255(t *testing.T) {
	var shape []string
	for _, count := range []int{255, 256, 300} {
		t.Run(fmt.Sprintf("%d outbound", count), func(t *testing.T) {
			st := store.NewMemory()
			f := newRequirementDisplayFixture(t, st, fmt.Sprintf("req-cap-%d", count))
			for index := 0; index < count; index++ {
				id := fmt.Sprintf("cap-%03d", index)
				f.task(id, "")
				f.attach(id)
			}
			view, object := readRequirementDetail(t, st, f.requirement.ID)
			legacyServing, _ := legacyRequirementDisplay(t, st, f.ctx, f.requirement.ID)
			root := core.LineageNode{Type: core.LineageRequirement, ID: f.requirement.ID}
			budget := core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 256, MaxLinks: 1024, Workspace: "demo"}
			delivery, err := st.ListRequirementDeliveryLineage(f.ctx, f.requirement.ID, budget)
			if err != nil {
				t.Fatal(err)
			}
			graph, err := core.TraverseLineage(delivery, []core.LineageNode{root}, budget)
			if err != nil {
				t.Fatal(err)
			}
			if view.Staleness.PartialEvaluation != graph.Truncated {
				t.Fatalf("partial_evaluation = %t, delivery truncated = %t", view.Staleness.PartialEvaluation, graph.Truncated)
			}
			keys := objectKeys(object)
			if shape == nil {
				shape = keys
			} else if !reflect.DeepEqual(keys, shape) {
				t.Fatalf("response keys = %v, want %v", keys, shape)
			}
			if count <= requirementDetailServingTaskLimit {
				assertSameJSON(t, "serving_tasks", view.ServingTasks, legacyServing)
				return
			}
			ids := servingTaskIDs(view.ServingTasks)
			if len(ids) != requirementDetailServingTaskLimit || !sort.StringsAreSorted(ids) {
				t.Fatalf("capped serving tasks = %d sorted=%t", len(ids), sort.StringsAreSorted(ids))
			}
			legacy := map[string]bool{}
			for _, id := range servingTaskIDs(legacyServing) {
				legacy[id] = true
			}
			for _, id := range ids {
				if !legacy[id] {
					t.Fatalf("capped list names %s, which the walk did not list", id)
				}
			}
			t.Logf("serving tasks: walk listed %d, detail lists %d", len(legacyServing), len(ids))
		})
	}
}

// The cap applies to the union of outbound and legacy inbound serving tasks
// after duplicates and tasks with children are removed. Below the cap, a
// mixed fixture matches the neighborhood walk exactly.
func TestRequirementDetailCapsMixedServingTasks(t *testing.T) {
	build := func(t *testing.T, id string, outbound, parents, inboundOnly, duplicates int) (*requirementDisplayLinkStore, *requirementDisplayFixture, []string) {
		t.Helper()
		inner := store.NewMemory()
		f := newRequirementDisplayFixture(t, inner, id)
		st := &requirementDisplayLinkStore{Store: inner}
		eligible := []string{}
		for index := 0; index < outbound; index++ {
			task := fmt.Sprintf("out-%03d", index)
			f.task(task, "")
			f.attach(task)
			if index < parents {
				f.task(task+"-child", task)
				continue
			}
			if index >= outbound-duplicates {
				st.extra = append(st.extra, f.inbound(task))
			}
			eligible = append(eligible, task)
		}
		for index := 0; index < inboundOnly; index++ {
			task := fmt.Sprintf("in-%03d", index)
			f.task(task, "")
			st.extra = append(st.extra, f.inbound(task))
			eligible = append(eligible, task)
		}
		sort.Strings(eligible)
		return st, f, eligible
	}
	t.Run("below the cap", func(t *testing.T) {
		st, f, eligible := build(t, "req-mixed-below", 200, 2, 10, 10)
		view, _ := readRequirementDetail(t, st, f.requirement.ID)
		legacyServing, _ := legacyRequirementDisplay(t, st, f.ctx, f.requirement.ID)
		assertSameJSON(t, "serving_tasks", view.ServingTasks, legacyServing)
		if got := servingTaskIDs(view.ServingTasks); !reflect.DeepEqual(got, eligible) {
			t.Fatalf("serving tasks = %d, want %d eligible", len(got), len(eligible))
		}
	})
	t.Run("above the cap", func(t *testing.T) {
		st, f, eligible := build(t, "req-mixed-above", 250, 3, 20, 5)
		if len(eligible) <= requirementDetailServingTaskLimit {
			t.Fatalf("fixture has %d eligible tasks", len(eligible))
		}
		view, _ := readRequirementDetail(t, st, f.requirement.ID)
		if got, want := servingTaskIDs(view.ServingTasks), eligible[:requirementDetailServingTaskLimit]; !reflect.DeepEqual(got, want) {
			t.Fatalf("capped serving tasks = %v, want the first %d eligible IDs", got, requirementDetailServingTaskLimit)
		}
		// The delivery graph holds all 250 outbound links, so the display
		// cut leaves delivery evaluation complete.
		if view.Staleness.PartialEvaluation {
			t.Fatalf("a display cut set partial_evaluation: %+v", view.Staleness)
		}
	})
}

// requirementOwnerCountingStore adds the artifact owner read to the shared
// counting store and records each read's owners.
type requirementOwnerCountingStore struct {
	*taskDetailCountingStore
	ownerMu sync.Mutex
	owners  []store.ArtifactOwners
}

func (c *requirementOwnerCountingStore) ListArtifactsForOwners(ctx context.Context, owners store.ArtifactOwners) ([]core.Artifact, error) {
	c.count("ListArtifactsForOwners")
	c.ownerMu.Lock()
	c.owners = append(c.owners, owners)
	c.ownerMu.Unlock()
	return c.taskDetailCountingStore.ListArtifactsForOwners(ctx, owners)
}

func (c *requirementOwnerCountingStore) takeOwners() []store.ArtifactOwners {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	out := c.owners
	c.owners = nil
	return out
}
