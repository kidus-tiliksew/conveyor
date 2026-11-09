package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

const httpPlan = "## Approach\nAttach a plan.\n\n## Files touched\n- internal/store/plan_state_acts.go\n\n## Ordering\n1. Store.\n\n## Risks\n- None.\n\n## Done criteria\n- The plan waits for approval."

func planActsRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func planActsJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type planActsFixtureTask struct {
	state     core.TaskState
	next      core.Stage
	planState core.PlanState
	order     core.Stage
	claimed   bool
}

func seedPlanActsTask(t *testing.T, st store.Store, id string, seed planActsFixtureTask) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	now := time.Now().UTC()
	if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-" + id, PolicyVersion: 1,
		State: seed.state, NextStage: seed.next, PlanState: seed.planState, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if seed.order == "" {
		return
	}
	jobID := id + "-" + string(seed.order)
	if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: id, Stage: seed.order, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: jobID, TaskID: id, JobID: jobID, Stage: seed.order, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if seed.claimed {
		if _, err := storetest.For(st).ClaimWorkOrder(ctx, jobID, core.WorkOrderClaim{SessionID: "session-" + id, ClientToken: "token-" + id, Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPolicyPlanStateBodyRules covers the /policy plan_state body rules: the
// value is needed or not_required and travels alone, and the route answers
// component-plan-state invariant 4's refusals with 409 (component-task-lifecycle
// invariants 44 and 45; req-task-plan-state AC-3.1).
func TestPolicyPlanStateBodyRules(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	seedPlanActsTask(t, st, "plan-policy", planActsFixtureTask{state: core.TaskQueued, next: core.StageTriage, planState: core.PlanNeeded})
	seedPlanActsTask(t, st, "plan-claimed", planActsFixtureTask{state: core.TaskRunning, next: core.StageImplement, planState: core.PlanNotRequired, order: core.StageImplement, claimed: true})
	for _, tc := range []struct {
		task, body string
		status     int
	}{
		{"plan-policy", `{"plan_state":"awaiting_approval","reason":"x","request_id":"ps-1"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"completed","reason":"x","request_id":"ps-2"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"not-required","reason":"x","request_id":"ps-3"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"not_required","verify_stage":true,"reason":"x","request_id":"ps-4"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"not_required","review_distinct_models":true,"reason":"x","request_id":"ps-5"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"not_required","stage_timeouts":{"verify":"1h"},"reason":"x","request_id":"ps-6"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"not_required","reason":" ","request_id":"ps-7"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":7,"reason":"x","request_id":"ps-8"}`, http.StatusBadRequest},
		{"plan-policy", `{"plan_state":"needed","reason":"x","request_id":"ps-9"}`, http.StatusConflict},
		{"plan-claimed", `{"plan_state":"needed","reason":"x","request_id":"ps-10"}`, http.StatusConflict},
		{"plan-policy", `{"plan_state":"not_required","reason":"small change","request_id":"ps-11"}`, http.StatusOK},
		{"plan-policy", `{"plan_state":"not_required","reason":"small change","request_id":"ps-11"}`, http.StatusOK},
		{"plan-policy", `{"plan_state":"needed","reason":"small change","request_id":"ps-11"}`, http.StatusConflict},
	} {
		response := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/"+tc.task+"/policy", tc.body)
		if response.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.task, tc.body, response.Code, response.Body)
		}
	}
	stored, err := st.GetTask(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), "plan-policy")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlanState != core.PlanNotRequired {
		t.Fatalf("plan state = %s", stored.PlanState)
	}
}

// TestAttachPlanRoute covers POST /v1/tasks/{id}/plan: the strict body, the
// intake plan rules, the 409s of component-plan-state invariant 5, the
// recorded version, and request-ID replay (req-task-plan-state AC-3.2).
func TestAttachPlanRoute(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	seedPlanActsTask(t, st, "plan-attach", planActsFixtureTask{state: core.TaskQueued, next: core.StageImplement, planState: core.PlanNotRequired, order: core.StageImplement})
	seedPlanActsTask(t, st, "plan-completed", planActsFixtureTask{state: core.TaskQueued, next: core.StageImplement, planState: core.PlanCompleted})
	seedPlanActsTask(t, st, "plan-claimed", planActsFixtureTask{state: core.TaskRunning, next: core.StageImplement, planState: core.PlanNotRequired, order: core.StageImplement, claimed: true})
	commit := "0123456789abcdef0123456789abcdef01234567"
	body := func(fields map[string]any) string { return planActsJSON(t, fields) }
	valid := map[string]any{"plan": httpPlan, "base_commit": commit, "reason": "operator plan", "request_id": "attach-1"}
	for _, tc := range []struct {
		name, task, body string
		status           int
	}{
		{"unknown field", "plan-attach", body(map[string]any{"plan": httpPlan, "reason": "x", "request_id": "a-1", "plan_approved": true}), http.StatusBadRequest},
		{"missing plan", "plan-attach", body(map[string]any{"reason": "x", "request_id": "a-2"}), http.StatusBadRequest},
		{"invalid plan", "plan-attach", body(map[string]any{"plan": "## Approach\nOnly an approach.", "reason": "x", "request_id": "a-3"}), http.StatusBadRequest},
		{"short base commit", "plan-attach", body(map[string]any{"plan": httpPlan, "base_commit": "abc123", "reason": "x", "request_id": "a-4"}), http.StatusBadRequest},
		{"blank reason", "plan-attach", body(map[string]any{"plan": httpPlan, "reason": " ", "request_id": "a-5"}), http.StatusBadRequest},
		{"blank request id", "plan-attach", body(map[string]any{"plan": httpPlan, "reason": "x", "request_id": ""}), http.StatusBadRequest},
		{"trailing json", "plan-attach", body(valid) + " {}", http.StatusBadRequest},
		{"completed plan", "plan-completed", body(map[string]any{"plan": httpPlan, "reason": "x", "request_id": "a-6"}), http.StatusConflict},
		{"claimed implement order", "plan-claimed", body(map[string]any{"plan": httpPlan, "reason": "x", "request_id": "a-7"}), http.StatusConflict},
		{"unknown task", "plan-absent", body(map[string]any{"plan": httpPlan, "reason": "x", "request_id": "a-8"}), http.StatusNotFound},
	} {
		response := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/"+tc.task+"/plan", tc.body)
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.name, response.Code, response.Body)
		}
	}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if _, ok, err := st.GetLatestSpecVersion(ctx, "plan-attach"); err != nil || ok {
		t.Fatalf("a refused attach recorded a plan version: ok=%t err=%v", ok, err)
	}
	var first store.AttachPlanResult
	response := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/plan-attach/plan", body(valid))
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &first) != nil {
		t.Fatalf("attach: %d %s", response.Code, response.Body)
	}
	if first.PlanVersion != 1 || first.Task.State != core.TaskAwaiting || first.Task.GateKind != core.GateKindPlanApproval || first.Task.PlanState != core.PlanAwaitingApproval || len(first.CancelledWorkOrders) != 1 {
		t.Fatalf("attach result = %+v", first)
	}
	spec, ok, err := st.GetSpecVersion(ctx, "plan-attach", 1)
	if err != nil || !ok || spec.Origin != core.PlanOriginOperator || spec.BaseCommit == nil || *spec.BaseCommit != commit || spec.Approved {
		t.Fatalf("recorded plan version = %+v ok=%t err=%v", spec, ok, err)
	}
	if replay := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/plan-attach/plan", body(valid)); replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"plan_version":1`) {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body)
	}
	changed := map[string]any{"plan": httpPlan, "reason": "another reason", "request_id": "attach-1"}
	if conflict := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/plan-attach/plan", body(changed)); conflict.Code != http.StatusConflict {
		t.Fatalf("reused request ID with different input: %d %s", conflict.Code, conflict.Body)
	}
}

// TestPlanActsMatchWhatTheRoutesAccept reads plan_acts on GET /v1/tasks/{id}
// and the task detail read for tasks standing in each effects-table row, then
// posts each act to its route on an identical task: a flag is true exactly
// when the route accepts the act (component-plan-state invariant 6; DEC-61).
func TestPlanActsMatchWhatTheRoutesAccept(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	seeds := map[string]planActsFixtureTask{
		"triage":            {state: core.TaskQueued, next: core.StageTriage, planState: core.PlanNeeded},
		"plan-queued":       {state: core.TaskQueued, next: core.StageSpec, planState: core.PlanNeeded, order: core.StageSpec},
		"plan-claimed":      {state: core.TaskRunning, next: core.StageSpec, planState: core.PlanNeeded, order: core.StageSpec, claimed: true},
		"implement-queued":  {state: core.TaskQueued, next: core.StageImplement, planState: core.PlanNotRequired, order: core.StageImplement},
		"implement-claimed": {state: core.TaskRunning, next: core.StageImplement, planState: core.PlanCompleted, order: core.StageImplement, claimed: true},
		"review":            {state: core.TaskQueued, next: core.StageReview, planState: core.PlanNotRequired},
		"parked":            {state: core.TaskParked, next: core.StageImplement, planState: core.PlanNotRequired},
	}
	type acts struct {
		SetNeeded      bool `json:"set_needed"`
		SetNotRequired bool `json:"set_not_required"`
		Attach         bool `json:"attach"`
	}
	read := func(t *testing.T, path string, field func(map[string]json.RawMessage) json.RawMessage) acts {
		t.Helper()
		response := planActsRequest(t, handler, http.MethodGet, path, "")
		var item map[string]json.RawMessage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &item) != nil {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body)
		}
		var got acts
		raw := field(item)
		if raw == nil || json.Unmarshal(raw, &got) != nil {
			t.Fatalf("GET %s carries no plan_acts: %s", path, response.Body)
		}
		return got
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			id := "acts-" + name
			seedPlanActsTask(t, st, id, seed)
			offered := read(t, "/v1/tasks/"+id, func(item map[string]json.RawMessage) json.RawMessage { return item["plan_acts"] })
			detail := read(t, "/v1/tasks/"+id+"/activity", func(item map[string]json.RawMessage) json.RawMessage { return item["plan_acts"] })
			if offered != detail {
				t.Fatalf("task read plan_acts %+v differ from the detail read %+v", offered, detail)
			}
			for act, accepted := range map[string]bool{"needed": offered.SetNeeded, "not_required": offered.SetNotRequired, "attach": offered.Attach} {
				twin := fmt.Sprintf("%s-%s", id, act)
				seedPlanActsTask(t, st, twin, seed)
				path, payload := "/v1/tasks/"+twin+"/policy", planActsJSON(t, map[string]any{"plan_state": act, "reason": "probe", "request_id": twin})
				if act == "attach" {
					path, payload = "/v1/tasks/"+twin+"/plan", planActsJSON(t, map[string]any{"plan": httpPlan, "reason": "probe", "request_id": twin})
				}
				response := planActsRequest(t, handler, http.MethodPost, path, payload)
				if (response.Code == http.StatusOK) != accepted {
					t.Fatalf("%s offered=%t but the route answered %d %s", act, accepted, response.Code, response.Body)
				}
				if !accepted && response.Code != http.StatusConflict {
					t.Fatalf("%s refused with %d, want 409: %s", act, response.Code, response.Body)
				}
			}
		})
	}
}

// TestTaskListPlanStateFilter covers the plan_state filter's parsing on
// GET /v1/tasks: repeated values select any listed state and an unknown value
// answers 400 (component-http-api invariant 20).
func TestTaskListPlanStateFilter(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	seedPlanActsTask(t, st, "filter-needed", planActsFixtureTask{state: core.TaskQueued, next: core.StageTriage, planState: core.PlanNeeded})
	seedPlanActsTask(t, st, "filter-none", planActsFixtureTask{state: core.TaskQueued, next: core.StageTriage, planState: core.PlanNotRequired})
	seedPlanActsTask(t, st, "filter-done", planActsFixtureTask{state: core.TaskQueued, next: core.StageImplement, planState: core.PlanCompleted})
	for _, bad := range []string{"bogus", "not-required", "NEEDED"} {
		if response := planActsRequest(t, handler, http.MethodGet, "/v1/tasks?plan_state="+bad, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("plan_state=%s: %d %s", bad, response.Code, response.Body)
		}
	}
	response := planActsRequest(t, handler, http.MethodGet, "/v1/tasks?plan_state=needed&plan_state=completed", "")
	var rows []struct {
		ID        string         `json:"id"`
		PlanState core.PlanState `json:"plan_state"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &rows) != nil {
		t.Fatalf("filtered list: %d %s", response.Code, response.Body)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	if len(rows) != 2 || !got["filter-needed"] || !got["filter-done"] || response.Header().Get("X-Conveyor-Total") != "2" {
		t.Fatalf("filtered rows = %+v total=%q", rows, response.Header().Get("X-Conveyor-Total"))
	}
}

// TestPlanStatusAfterPlanRerouteIsNotPending keeps an unapproved version that
// an operator rerouted off the plan approval gate from reading as pending at
// the gate (component-plan-state invariant 4).
func TestPlanStatusAfterPlanRerouteIsNotPending(t *testing.T) {
	events := []core.Event{
		{Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": core.TaskGateSpec})},
		{Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": core.TaskPlanReroute})},
	}
	if status := taskPlanStatusFor(core.Task{State: core.TaskQueued, NextStage: core.StageImplement}, core.SpecVersion{Version: 1}, true, events); status.State != taskPlanRedirected {
		t.Fatalf("rerouted plan status = %+v", status)
	}
}
