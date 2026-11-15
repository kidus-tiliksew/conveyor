package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
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
	counter := newTaskDetailCountingStore(inner)
	server := NewServer(counter)
	server.Workspace = "demo"
	read := func(id string, serving, linked int) (map[string]int, int) {
		t.Helper()
		requirementDetailRecorder(t, server, "/v1/requirements/"+id) // warm any first-request work
		counter.reset()
		response := requirementDetailRecorder(t, server, "/v1/requirements/"+id)
		calls := counter.reset()
		var view requirementView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if len(view.ServingTasks) != serving || len(view.ServingBlueprints) != linked || view.Staleness.PartialEvaluation {
			t.Fatalf("%s fixture: serving=%d blueprints=%d partial=%t", id, len(view.ServingTasks), len(view.ServingBlueprints), view.Staleness.PartialEvaluation)
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
	t.Logf("MEASUREMENT memory requirement detail store calls=%d small_bytes=%d large_bytes=%d", totalCalls(smallCalls), smallBytes, largeBytes)
}

func totalCalls(calls map[string]int) int {
	total := 0
	for _, count := range calls {
		total += count
	}
	return total
}
