package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestNextActionInboxBandsPagingAndActs(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	parent := core.Task{ID: "z-parent", Workspace: "demo", Repo: "conveyor", Branch: "parent", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now.Add(time.Hour)}
	child := core.Task{ID: "a-child", Workspace: "demo", Repo: "conveyor", Branch: "child", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	running := core.Task{ID: "m-running", Workspace: "demo", Repo: "conveyor", Branch: "running", State: core.TaskRunning, CreatedAt: now}
	otherRepo := core.Task{ID: "other-repo", Workspace: "demo", Repo: "other", Branch: "other", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	for _, task := range []core.Task{parent, child, running, otherRepo} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: child.ID, DependsOnTaskID: parent.ID, Reason: "parent first", RequestID: "dep-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "stall-job", TaskID: running.ID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: "stall-order", TaskID: running.ID, JobID: "stall-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueDeadline: now.Add(time.Hour), RetrySuppressed: true}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	plain := core.Task{ID: "plain-running", Workspace: "demo", Repo: "conveyor", Branch: "plain", State: core.TaskRunning, CreatedAt: now.Add(2 * time.Hour)}
	if err := st.CreateTask(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: parent.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: child.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: otherRepo.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	recent := core.Event{TaskID: plain.ID, Kind: "pipeline.bounced", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"source": "mcp-review"})}
	old := core.Event{TaskID: plain.ID, Kind: "merge.conflict_fix_dispatched", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(-25 * time.Hour)}
	for _, event := range []core.Event{recent, old} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-inbox", Title: "Inbox requirement"}, core.RequirementVersion{
		Content: "# Inbox", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Show the inbox."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "# Inbox revised", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Show the revised inbox."}},
	}); err != nil {
		t.Fatal(err)
	}
	confirmed, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-context", Title: "Context requirement"}, core.RequirementVersion{
		Content: "# Context", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Attach context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, confirmed.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: parent.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: confirmed.ID, Source: core.TaskContextProposalTriage, Justification: "The task implements REQ-1."}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
		return response
	}
	invalid := call("/v1/next-actions?workspace_id=demo&band=nope")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid band status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	badFilter := call("/v1/next-actions?workspace_id=demo&state=not-a-state")
	if badFilter.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter status=%d body=%s", badFilter.Code, badFilter.Body.String())
	}

	body := call("/v1/next-actions?workspace_id=demo")
	if body.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", body.Code, body.Body.String())
	}
	if body.Header().Get("X-Conveyor-Total") == "" || body.Header().Get("X-Conveyor-Limit") == "" || body.Header().Get("X-Conveyor-Offset") != "0" {
		t.Fatalf("paging headers=%v", body.Header())
	}
	var payload struct {
		Items []struct {
			Band     string `json:"band"`
			Kind     string `json:"kind"`
			Sentence string `json:"sentence"`
			Task     *struct {
				ID string `json:"id"`
			} `json:"task"`
			NextAction *struct {
				Label string `json:"label"`
			} `json:"next_action"`
			Proposal *struct {
				Tier string `json:"tier"`
				ID   string `json:"id"`
			} `json:"proposal"`
		} `json:"items"`
		Acts []struct {
			Kind string `json:"kind"`
		} `json:"acts"`
	}
	if err = json.Unmarshal(body.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Body.String(), `"tier":"task_context"`) {
		t.Fatal("task_context proposal leaked into the inbox")
	}
	if !strings.Contains(body.Body.String(), dismissEarlierVersions) {
		t.Fatalf("missing multi-version sentence: %s", body.Body.String())
	}
	ids := make([]string, 0)
	for _, item := range payload.Items {
		if item.Task != nil {
			ids = append(ids, item.Task.ID)
			if item.NextAction == nil || item.NextAction.Label == "" {
				t.Fatalf("item %s missing compact label", item.Task.ID)
			}
		}
	}
	parentAt, childAt, otherAt := indexOf(ids, parent.ID), indexOf(ids, child.ID), indexOf(ids, otherRepo.ID)
	// The child waits at its own plan gate with no queued order, so it names
	// no edge in blocks; the parent leads on its older last_event_at.
	if parentAt < 0 || childAt < 0 || parentAt > childAt {
		t.Fatalf("last_event_at order=%v", ids)
	}
	if otherAt < 0 || childAt < 0 || otherAt > childAt {
		t.Fatalf("older last_event_at first order=%v", ids)
	}
	stallAt, plainAt := indexOf(ids, running.ID), indexOf(ids, plain.ID)
	if stallAt < 0 || plainAt < 0 || stallAt > plainAt {
		t.Fatalf("deadline-before-nil order=%v", ids)
	}
	var sawOld bool
	for _, act := range payload.Acts {
		if act.Kind == "merge.conflict_fix_dispatched" {
			sawOld = true
		}
	}
	if sawOld || len(payload.Acts) == 0 {
		t.Fatalf("acts=%+v", payload.Acts)
	}

	page := call("/v1/next-actions?workspace_id=demo&limit=1&offset=0")
	if page.Header().Get("X-Conveyor-Limit") != "1" || page.Header().Get("X-Conveyor-Offset") != "0" || page.Header().Get("X-Conveyor-Total") != body.Header().Get("X-Conveyor-Total") {
		t.Fatalf("page headers=%v", page.Header())
	}
	for _, band := range []string{"decisions", "blocked", "automatic"} {
		response := call("/v1/next-actions?workspace_id=demo&band=" + band)
		if response.Code != http.StatusOK {
			t.Fatalf("band %s status=%d", band, response.Code)
		}
		var filtered struct {
			Items []struct {
				Band string `json:"band"`
			} `json:"items"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
			t.Fatal(err)
		}
		for _, item := range filtered.Items {
			if item.Band != band {
				t.Fatalf("band %s item=%s", band, item.Band)
			}
		}
	}
	filtered := call("/v1/next-actions?workspace_id=demo&repository=other")
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body.String(), `"id":"`+parent.ID+`"`) || !strings.Contains(filtered.Body.String(), `"id":"`+otherRepo.ID+`"`) {
		t.Fatalf("repository filter body=%s", filtered.Body.String())
	}
}

// GET /v1/next-actions serializes blocks on each task item's compact next
// action and orders an item with dependents ahead of one with an earlier
// deadline, also when the request's filter excludes the dependent
// (feature-operator-next-action NA-5; req-operator-next-action AC-2.1).
func TestNextActionInboxOrdersDependentsFirst(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	stalled := func(id, repo string, deadline time.Time) {
		t.Helper()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Title: "Task " + id, Repo: repo, Branch: id, State: core.TaskRunning, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id + "-order", TaskID: id, JobID: id + "-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: deadline, RetrySuppressed: true}); err != nil {
			t.Fatal(err)
		}
	}
	stalled("z-blocker", "conveyor", now.Add(2*time.Hour))
	stalled("a-earlier", "conveyor", now.Add(time.Hour))
	if err := st.CreateTask(ctx, core.Task{ID: "m-dependent", Workspace: "demo", Title: "Dependent", Repo: "other", Branch: "dependent", State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "m-dependent-job", TaskID: "m-dependent", Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "m-dependent-order", TaskID: "m-dependent", JobID: "m-dependent-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: "m-dependent", DependsOnTaskID: "z-blocker", Reason: "blocker first", RequestID: "inbox-dependents"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	type item struct {
		Band string `json:"band"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
		NextAction *struct {
			Deadline *time.Time              `json:"deadline"`
			Blocks   []nextaction.BlockerRef `json:"blocks"`
		} `json:"next_action"`
	}
	read := func(path string) ([]item, string) {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var payload struct {
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Items, response.Body.String()
	}
	want := []nextaction.BlockerRef{{TaskID: "m-dependent", Title: "Dependent", State: string(core.TaskRunning)}}
	for _, path := range []string{"/v1/next-actions?workspace_id=demo&band=automatic", "/v1/next-actions?workspace_id=demo&band=automatic&repository=conveyor"} {
		items, body := read(path)
		var order []string
		byID := map[string]item{}
		for _, it := range items {
			if it.Task != nil {
				order = append(order, it.Task.ID)
				byID[it.Task.ID] = it
			}
		}
		blocker, earlier := byID["z-blocker"], byID["a-earlier"]
		if blocker.NextAction == nil || earlier.NextAction == nil || earlier.NextAction.Deadline == nil || blocker.NextAction.Deadline == nil || !earlier.NextAction.Deadline.Before(*blocker.NextAction.Deadline) {
			t.Fatalf("%s fixture deadlines: %s", path, body)
		}
		if indexOf(order, "z-blocker") < 0 || indexOf(order, "z-blocker") > indexOf(order, "a-earlier") {
			t.Fatalf("%s order=%v, want the blocker before the earlier deadline", path, order)
		}
		if !reflect.DeepEqual(blocker.NextAction.Blocks, want) {
			t.Fatalf("%s blocker blocks=%+v, want %+v", path, blocker.NextAction.Blocks, want)
		}
		if earlier.NextAction.Blocks == nil || len(earlier.NextAction.Blocks) != 0 || !strings.Contains(body, `"blocks":[]`) {
			t.Fatalf("%s earlier blocks=%#v body=%s", path, earlier.NextAction.Blocks, body)
		}
	}
}

func indexOf(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}
