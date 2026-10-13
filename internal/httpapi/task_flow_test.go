package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

type failingFlowEventsStore struct{ store.Store }

func (failingFlowEventsStore) ListEvents(context.Context, string) ([]core.Event, error) {
	return nil, errors.New("ledger unavailable")
}

func flowGet(t *testing.T, server *Server, path string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authenticated {
		request = authenticatedMemoryRead(server, request)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestTaskFlowRouteIsTheGroupBoundary(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "boundary", Workspace: "demo", Repo: "conveyor", Branch: "boundary", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}); err != nil {
		t.Fatal(err)
	}
	foreign := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "foreign")
	if err := st.CreateTask(foreign, core.Task{ID: "foreign-task", Workspace: "foreign", Repo: "conveyor", Branch: "foreign"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"

	response := flowGet(t, server, "/v1/tasks/boundary/flow?workspace_id=demo", true)
	var envelope map[string]map[string]json.RawMessage
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &envelope) != nil || len(envelope) != 1 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, field := range []string{"stages", "loops", "side", "next_action", "raw"} {
		if _, ok := envelope["flow"][field]; !ok {
			t.Fatalf("flow missing %s in %s", field, response.Body.String())
		}
	}
	for _, kind := range []string{"mystery.kind", "pipeline.bounced", "work_order.claimed"} {
		if err := st.AppendEvent(ctx, core.Event{TaskID: "boundary", Kind: kind, Payload: json.RawMessage(`{"broken`)}); err != nil {
			t.Fatal(err)
		}
	}
	if noisy := flowGet(t, server, "/v1/tasks/boundary/flow?workspace_id=demo", true); noisy.Code != http.StatusOK || noisy.Body.String() != response.Body.String() {
		t.Fatalf("malformed events: status=%d body=%s\nwant %s", noisy.Code, noisy.Body.String(), response.Body.String())
	}
	for _, id := range []string{"missing", "foreign-task"} {
		if code := flowGet(t, server, "/v1/tasks/"+id+"/flow?workspace_id=demo", true).Code; code != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", id, code)
		}
	}
	flow := flowGet(t, server, "/v1/tasks/boundary/flow?workspace_id=demo", false)
	next := flowGet(t, server, "/v1/tasks/boundary/next-action?workspace_id=demo", false)
	if flow.Code == http.StatusOK || flow.Code != next.Code || flow.Body.String() != next.Body.String() {
		t.Fatalf("unauthenticated flow %d %q, next-action %d %q", flow.Code, flow.Body.String(), next.Code, next.Body.String())
	}

	failing := NewServer(failingFlowEventsStore{st})
	failing.BearerToken, failing.Workspace = "token", "demo"
	if code := flowGet(t, failing, "/v1/tasks/boundary/flow?workspace_id=demo", true).Code; code != http.StatusInternalServerError {
		t.Fatalf("store failure status=%d, want 500", code)
	}
}

// seededFlowServer serves the shared command-built flow scenarios.
func seededFlowServer(t *testing.T) (*Server, store.Backend, context.Context, []storetest.TaskFlowScenario) {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	scenarios := storetest.SeedTaskFlowScenarios(t, st, ctx, "demo")
	// The Store view alone, as memory-only deployments serve: the shared
	// bearer token is the operator credential.
	server := NewServer(struct{ store.Store }{st})
	server.BearerToken, server.Workspace = "token", "demo"
	return server, st, ctx, scenarios
}

func flowBody(t *testing.T, server *Server, id string) json.RawMessage {
	t.Helper()
	response := flowGet(t, server, "/v1/tasks/"+id+"/flow?workspace_id=demo", true)
	var envelope struct {
		Flow json.RawMessage `json:"flow"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &envelope) != nil || envelope.Flow == nil {
		t.Fatalf("%s flow status=%d body=%s", id, response.Code, response.Body.String())
	}
	return envelope.Flow
}

// withoutElapsed drops attempt.elapsed_seconds, the one field that moves
// between two reads of a claimed order.
func withoutElapsed(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var action map[string]any
	if err := json.Unmarshal(raw, &action); err != nil {
		t.Fatal(err)
	}
	if attempt, ok := action["attempt"].(map[string]any); ok {
		delete(attempt, "elapsed_seconds")
	}
	return action
}

func TestTaskFlowEmbedsNextAction(t *testing.T) {
	server, _, _, scenarios := seededFlowServer(t)
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			var flow struct {
				NextAction json.RawMessage `json:"next_action"`
			}
			if err := json.Unmarshal(flowBody(t, server, scenario.TaskID), &flow); err != nil {
				t.Fatal(err)
			}
			response := flowGet(t, server, "/v1/tasks/"+scenario.TaskID+"/next-action?workspace_id=demo", true)
			if response.Code != http.StatusOK {
				t.Fatalf("next-action status=%d body=%s", response.Code, response.Body.String())
			}
			direct := bytes.TrimSpace(response.Body.Bytes())
			embedded, err := json.Marshal(withoutElapsed(t, flow.NextAction))
			if err != nil {
				t.Fatal(err)
			}
			stated, err := json.Marshal(withoutElapsed(t, direct))
			if err != nil {
				t.Fatal(err)
			}
			if string(embedded) != string(stated) {
				t.Fatalf("flow.next_action differs from the next-action route:\n%s\nwant\n%s", flow.NextAction, direct)
			}
			if !bytes.Contains(direct, []byte(`"attempt":{`)) && !bytes.Equal(flow.NextAction, direct) {
				t.Fatalf("unclaimed next action is not byte-equal:\n%s\nwant\n%s", flow.NextAction, direct)
			}
		})
	}
}

func TestTaskFlowRebuild(t *testing.T) {
	server, _, _, scenarios := seededFlowServer(t)
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			body := flowBody(t, server, scenario.TaskID)
			var served store.TaskFlow
			if err := json.Unmarshal(body, &served); err != nil {
				t.Fatal(err)
			}
			replayed, err := json.Marshal(storetest.ReplayTaskFlow(t, server.Store, store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), replayFixture(t), scenario.TaskID, served.NextAction, time.Now().UTC()))
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, body); err != nil {
				t.Fatal(err)
			}
			if string(replayed) != compact.String() {
				t.Fatalf("replayed ledger folds differently from GET /flow:\n%s\nwant\n%s", replayed, compact.String())
			}
		})
	}
}

// Every shared scenario's flow facts hold through the real route, including
// side.retrying for an order whose automatic recovery is scheduled.
func TestTaskFlowRouteScenarioFacts(t *testing.T) {
	server, _, _, scenarios := seededFlowServer(t)
	retrying := false
	for _, scenario := range scenarios {
		retrying = retrying || scenario.Name == "retrying"
		if scenario.Check == nil {
			continue
		}
		t.Run(scenario.Name, func(t *testing.T) {
			var served store.TaskFlow
			if err := json.Unmarshal(flowBody(t, server, scenario.TaskID), &served); err != nil {
				t.Fatal(err)
			}
			scenario.Check(t, served)
		})
	}
	if !retrying {
		t.Fatal("no retrying scenario seeded")
	}
}

// replayFixture is a fresh volatile backend whose seeded events are copied
// verbatim through AppendEvent; only the jobs those events name are created,
// so no task, order or intervention projection is built by a command.
func replayFixture(t *testing.T) storetest.Fixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	seed := func(t *testing.T, ctx context.Context, _ int64, events []core.Event) []core.Event {
		t.Helper()
		var jobs []string
		for _, event := range events {
			if event.JobID != "" && !slices.Contains(jobs, event.JobID) {
				jobs = append(jobs, event.JobID)
				if err := st.CreateJob(ctx, core.Job{ID: event.JobID, TaskID: event.TaskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
					t.Fatal(err)
				}
			}
		}
		before := map[int64]bool{}
		if len(events) > 0 {
			existing, err := st.ListEvents(ctx, events[0].TaskID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range existing {
				before[event.ID] = true
			}
		}
		for _, event := range events {
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		var stored []core.Event
		if len(events) > 0 {
			all, err := st.ListEvents(ctx, events[0].TaskID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range all {
				if !before[event.ID] {
					stored = append(stored, event)
				}
			}
		}
		return stored
	}
	return storetest.Fixture{Backend: st, Context: ctx, Workspace: "demo", SeedEvents: seed}
}

// The flow's retrying.of bound is the workspace document's automatic-recovery
// bound, the clock's default when no document is stored, and a read failure
// otherwise.
func TestTaskFlowRecoveryBound(t *testing.T) {
	document := contextualWorkspaceDocument()
	document.Recovery = config.RecoveryPolicy{AutomaticRecoveries: config.DefaultAutomaticRecoveries + 2, WindowText: "24h"}
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/x/flow", nil)
	for name, tc := range map[string]struct {
		backend *fakeWorkspaceConfigStore
		want    int
		fails   bool
	}{
		"stored":     {&fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 1}}, config.DefaultAutomaticRecoveries + 2, false},
		"absent":     {&fakeWorkspaceConfigStore{readErr: store.ErrNotFound}, config.DefaultAutomaticRecoveries, false},
		"unreadable": {&fakeWorkspaceConfigStore{readErr: errors.New("config unavailable")}, 0, true},
	} {
		server := NewServer(store.NewMemory())
		server.ConfigStore = tc.backend
		got, err := server.recoveryPolicy(request)
		if (err != nil) != tc.fails || !tc.fails && got.AutomaticRecoveries != tc.want {
			t.Fatalf("%s: policy=%+v err=%v, want bound %d", name, got, err, tc.want)
		}
	}
}

// The flow payload carries the fold's server-stated copy on every stage: the
// stage description, the status label and sentence, and a missed check-in
// sentence judged at the request's own instant (feature-operator-next-action
// NA-7).
func TestTaskFlowRouteStatesStageCopy(t *testing.T) {
	fixture := replayFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	seed := func(id string, lease time.Time) {
		task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning,
			MergeApproval: true, SetupContract: config.ExecutionSetup{VerifyStage: true}, CreatedAt: now.Add(-3 * time.Hour)}
		if err := fixture.Backend.CreateTask(fixture.Context, task); err != nil {
			t.Fatal(err)
		}
		at := func(minutes int) time.Time { return now.Add(-2*time.Hour + time.Duration(minutes)*time.Minute) }
		order := core.WorkOrder{ID: id + "-impl", JobID: id + "-job", TaskID: id, Stage: core.StageImplement, State: core.WorkOrderQueued}
		claimed := order
		claimed.State, claimed.AttemptID, claimed.SessionID, claimed.Agent, claimed.Model = core.WorkOrderClaimed, "attempt-1", "session-1", "omp", "opus"
		claimed.ExecutionStartedAt, claimed.LastAgentActivityAt, claimed.LeaseExpiresAt = at(3), at(3), lease
		fixture.SeedEvents(t, fixture.Context, 0, []core.Event{
			{ID: 1, TaskID: id, Kind: "task.created", ActorRole: core.ActorHuman, Payload: core.JSONPayload(task), At: at(0)},
			{ID: 2, TaskID: id, Kind: "pipeline.transition_decided", ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"from_stage": core.StageTriage, "next_stage": core.StageImplement}), At: at(1)},
			{ID: 3, TaskID: id, JobID: order.JobID, Kind: "work_order.created", ActorRole: core.ActorSystem, Payload: core.JSONPayload(order), At: at(2)},
			{ID: 4, TaskID: id, JobID: order.JobID, Kind: "work_order.claimed", ActorRole: core.ActorAgent, Payload: core.JSONPayload(claimed), At: at(3)},
		})
	}
	lapsed := now.Add(-time.Hour)
	seed("lapsed", lapsed)
	seed("live", now.Add(time.Hour))
	server := NewServer(struct{ store.Store }{fixture.Backend})
	server.BearerToken, server.Workspace = "token", "demo"

	read := func(id string) map[string]map[string]any {
		var flow struct {
			Stages []map[string]any `json:"stages"`
		}
		if err := json.Unmarshal(flowBody(t, server, id), &flow); err != nil {
			t.Fatal(err)
		}
		stages := map[string]map[string]any{}
		for _, stage := range flow.Stages {
			for _, field := range []string{"description", "status_label", "status_text", "missed_check_in"} {
				if _, ok := stage[field].(string); !ok {
					t.Fatalf("%s stage %v: %s is not a string field: %v", id, stage["id"], field, stage)
				}
			}
			stages[stage["id"].(string)] = stage
		}
		return stages
	}
	stages := read("lapsed")
	want := map[string]map[string]any{
		"spec":      {"status": "off", "status_label": "Off for this task", "status_text": "This task's policy turned Plan off.", "missed_check_in": ""},
		"implement": {"status": "in_progress", "status_label": "In progress", "status_text": "Implement is running now.", "description": "An agent makes the change on the task branch and opens the pull request.", "missed_check_in": "Missed check-in: the lease expired at " + lapsed.Format("2006-01-02 15:04") + " UTC and the agent has not renewed it."},
		"review":    {"status": "upcoming", "status_label": "Upcoming", "status_text": "Code review has not started yet.", "missed_check_in": ""},
	}
	for id, fields := range want {
		for field, value := range fields {
			if stages[id][field] != value {
				t.Fatalf("lapsed %s.%s = %v, want %v", id, field, stages[id][field], value)
			}
		}
	}
	if got := read("live")["implement"]["missed_check_in"]; got != "" {
		t.Fatalf("live lease: missed_check_in = %v, want empty", got)
	}
}

// The task detail page iterates interventions and jobs unguarded, so the
// activity read serializes an empty list as [] on the memory backend too:
// every seeded flow scenario plus a task with neither list populated.
func TestTaskActivitySerializesEmptyListsAsArrays(t *testing.T) {
	server, st, ctx, scenarios := seededFlowServer(t)
	bare := core.Task{ID: "bare", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-bare", PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, bare); err != nil {
		t.Fatal(err)
	}
	ids := []string{bare.ID}
	for _, scenario := range scenarios {
		ids = append(ids, scenario.TaskID)
	}
	for _, id := range ids {
		response := flowGet(t, server, "/v1/tasks/"+id+"/activity?workspace_id=demo", true)
		var item struct {
			Interventions json.RawMessage `json:"interventions"`
			Jobs          json.RawMessage `json:"jobs"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &item) != nil {
			t.Fatalf("%s activity status=%d body=%s", id, response.Code, response.Body.String())
		}
		interventions, err := st.ListInterventions(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := st.ListJobs(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for field, list := range map[string]struct {
			raw   json.RawMessage
			empty bool
		}{"interventions": {item.Interventions, len(interventions) == 0}, "jobs": {item.Jobs, len(jobs) == 0}} {
			if list.empty && string(list.raw) != "[]" || !bytes.HasPrefix(list.raw, []byte("[")) {
				t.Errorf("%s activity %s=%s, want a JSON array", id, field, list.raw)
			}
		}
	}
}
