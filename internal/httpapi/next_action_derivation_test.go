package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestNextActionDerivationAndAttentionParity(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	assignee := &core.TaskAssignee{UserID: "usr_1", Email: "operator@example.test"}
	gates := []core.Task{
		{ID: "plan", Workspace: "demo", Repo: "conveyor", Branch: "plan", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, Assignee: assignee},
		{ID: "revision", Workspace: "demo", Repo: "conveyor", Branch: "revision", State: core.TaskAwaiting, GateKind: core.GateKindPlanRevision, Assignee: assignee},
		{ID: "merge", Workspace: "demo", Repo: "conveyor", Branch: "merge", BaseBranch: "main", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, Assignee: assignee},
		{ID: "bounce", Workspace: "demo", Repo: "conveyor", Branch: "bounce", State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit, Assignee: assignee},
		{ID: "failed", Workspace: "demo", Repo: "conveyor", Branch: "failed", State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, Assignee: assignee},
		{ID: "execute", Workspace: "demo", Repo: "conveyor", Branch: "execute", BaseBranch: "main", State: core.TaskApproved, MergeApproval: true, Assignee: assignee},
		{ID: "running", Workspace: "demo", Repo: "conveyor", Branch: "running", State: core.TaskRunning, Assignee: assignee},
	}
	for _, task := range gates {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat_1", OwnerUserID: assignee.UserID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
		return response
	}
	activity := decodeItems(t, call("/v1/activity?workspace_id=demo"))
	operations := decodeItems(t, call("/v1/task-operations?workspace_id=demo"))
	reviews := decodeItems(t, call("/v1/reviews?workspace_id=demo"))
	attention := decodeItems(t, call("/v1/attention/tasks?workspace_id=demo&assignee=someone-else&subject=someone-else"))
	inbox := call("/v1/next-actions?workspace_id=demo")
	if inbox.Code != http.StatusOK {
		t.Fatalf("inbox status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	for _, task := range gates {
		projected, err := store.ReadNextAction(ctx, st, task.ID, server.nextActionOptions(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)))
		if err != nil {
			t.Fatal(err)
		}
		for _, surface := range []map[string]compactView{activity, operations} {
			item, ok := surface[task.ID]
			if !ok || item.Actor != projected.Actor || item.Kind != projected.Kind || item.Label != projected.Label || item.Needs != (projected.Actor == nextaction.ActorOperator) {
				t.Fatalf("surface drift %s item=%+v projected=%s/%s/%s", task.ID, item, projected.Actor, projected.Kind, projected.Label)
			}
		}
		if task.State == core.TaskAwaiting || task.State == core.TaskApproved {
			item := reviews[task.ID]
			if item.Actor != projected.Actor || item.Kind != projected.Kind || item.Label != projected.Label || item.Needs != (projected.Actor == nextaction.ActorOperator) {
				t.Fatalf("reviews drift %s item=%+v", task.ID, item)
			}
		}
		if projected.Actor == nextaction.ActorOperator {
			item, ok := attention[task.ID]
			if !ok || item.Actor != projected.Actor || item.Kind != projected.Kind || item.Label != projected.Label || !item.Needs {
				t.Fatalf("attention drift %s item=%+v ok=%t", task.ID, item, ok)
			}
		}
		detail := call("/v1/tasks/" + task.ID + "/activity?workspace_id=demo")
		var taskDetail struct {
			NeedsAttention bool `json:"needs_attention"`
			NextAction     struct {
				Actor, Kind, Label string
			} `json:"next_action"`
		}
		if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &taskDetail) != nil || taskDetail.NextAction.Actor != projected.Actor || taskDetail.NextAction.Kind != projected.Kind || taskDetail.NextAction.Label != projected.Label || taskDetail.NeedsAttention != (projected.Actor == nextaction.ActorOperator) {
			t.Fatalf("activity detail %s status=%d body=%s", task.ID, detail.Code, detail.Body.String())
		}
		if !strings.Contains(inbox.Body.String(), `"id":"`+task.ID+`"`) {
			continue
		}
		if !strings.Contains(inbox.Body.String(), `"kind":"`+projected.Kind+`"`) || !strings.Contains(inbox.Body.String(), `"label":"`+projected.Label+`"`) {
			t.Fatalf("inbox missing %s kind/label: %s", task.ID, inbox.Body.String())
		}
		gate, _, err := server.taskRunGate(ctx, task, false)
		if err != nil {
			t.Fatal(err)
		}
		entry, _ := nextaction.Lookup(projected.Kind, projected.Details.StallReason, projected.Details.ReleaseOutcome, projected.Details.GateKind)
		if entry.RunGate == nil {
			if gate != nil {
				t.Fatalf("%s gate=%+v want none", task.ID, gate)
			}
			continue
		}
		if gate == nil || gate.Kind != entry.RunGate.Kind || gate.Label != entry.RunGate.Label {
			t.Fatalf("%s gate=%+v want %s/%s", task.ID, gate, entry.RunGate.Kind, entry.RunGate.Label)
		}
	}
	if _, ok := attention["running"]; ok {
		t.Fatal("attention included a factory task or honored the subject query")
	}
	pending := call("/v1/pending-proposals?workspace_id=demo")
	var summary struct {
		Attention struct {
			OperatorDecisions int `json:"operator_decisions"`
			Total             int `json:"total"`
		} `json:"attention"`
	}
	if pending.Code != http.StatusOK || json.Unmarshal(pending.Body.Bytes(), &summary) != nil || summary.Attention.OperatorDecisions != summary.Attention.Total || summary.Attention.Total != len(attention) {
		t.Fatalf("decisions status=%d summary=%+v attention=%d body=%s", pending.Code, summary, len(attention), pending.Body.String())
	}
}

func TestRunGateGoldenJSON(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	server := NewServer(st)
	cases := []struct {
		task core.Task
		want string
	}{
		{core.Task{ID: "spec", Workspace: "demo", Repo: "conveyor", Branch: "spec", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, `{"kind":"spec","label":"spec approval gate","summary":"The execution plan is waiting for approval.","can_operate":false,"can_request_changes":false}`},
		{core.Task{ID: "merge", Workspace: "demo", Repo: "conveyor", Branch: "merge-branch", BaseBranch: "main", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval}, `{"kind":"merge","label":"merge approval gate","summary":"The reviewed branch is waiting for merge approval.","can_operate":false,"can_request_changes":false}`},
		{core.Task{ID: "revision", Workspace: "demo", Repo: "conveyor", Branch: "revision", State: core.TaskAwaiting, GateKind: core.GateKindPlanRevision}, `{"kind":"plan_revision","label":"plan revision gate","summary":"Implementation asked to revise the plan.","can_operate":false,"can_request_changes":false}`},
		{core.Task{ID: "bounce", Workspace: "demo", Repo: "conveyor", Branch: "bounce", State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, `{"kind":"human","label":"human recovery gate","summary":"Review bounced until the limit.","can_operate":false,"can_request_changes":false}`},
		{core.Task{ID: "failed", Workspace: "demo", Repo: "conveyor", Branch: "failed", State: core.TaskAwaiting, GateKind: core.GateKindJobFailure}, `{"kind":"human","label":"human recovery gate","summary":"A job failed and needs a person.","can_operate":false,"can_request_changes":false}`},
		{core.Task{ID: "execute", Workspace: "demo", Repo: "conveyor", Branch: "execute-branch", BaseBranch: "main", State: core.TaskApproved, MergeApproval: true}, `{"kind":"merge_execution","label":"merge execution pending","summary":"execute-branch into main; merge readiness is not configured on this server","can_operate":false,"can_request_changes":false}`},
	}
	for _, tc := range cases {
		if err := st.CreateTask(ctx, tc.task); err != nil {
			t.Fatal(err)
		}
		gate, _, err := server.taskRunGate(ctx, tc.task, false)
		if err != nil || gate == nil {
			t.Fatalf("%s gate=%+v err=%v", tc.task.ID, gate, err)
		}
		raw, err := json.Marshal(gate)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("%s gate=%s want %s", tc.task.ID, raw, tc.want)
		}
	}
}

func TestNextActionRouteIsTheGroupBoundary(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "boundary", Workspace: "demo", Repo: "conveyor", Branch: "boundary", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "order", TaskID: task.ID, JobID: "job", Stage: core.StageImplement, State: core.WorkOrderQueued, RecoveryRefusal: "operator must recover this"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/missing/next-action?workspace_id=demo", nil)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/boundary/next-action?workspace_id=demo", nil)))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"repo"`) {
		t.Fatalf("boundary status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"label", "actor", "kind", "judgment", "summary", "choices", "deadline", "details"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("missing %s in %s", field, response.Body.String())
		}
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(body["details"], &details) != nil {
		t.Fatal(response.Body.String())
	}
	for _, field := range []string{"gate_kind", "reason_code", "secondary"} {
		if _, ok := details[field]; !ok {
			t.Fatalf("details missing %s in %s", field, response.Body.String())
		}
	}
	working := core.Task{ID: "working", Workspace: "demo", Repo: "conveyor", Branch: "working", State: core.TaskRunning}
	if err := st.CreateTask(ctx, working); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "working-job", TaskID: working.ID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "working-order", TaskID: working.ID, JobID: "working-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueDeadline: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, "working-order", core.WorkOrderClaim{ClaimantID: "agent", SessionID: "session", Agent: "omp", Model: "model", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	claimed := httptest.NewRecorder()
	server.Handler().ServeHTTP(claimed, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/working/next-action?workspace_id=demo", nil)))
	var claimedBody map[string]json.RawMessage
	if claimed.Code != http.StatusOK || json.Unmarshal(claimed.Body.Bytes(), &claimedBody) != nil {
		t.Fatalf("working status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	var order map[string]json.RawMessage
	var attempt map[string]json.RawMessage
	if json.Unmarshal(claimedBody["current_order"], &order) != nil || json.Unmarshal(claimedBody["attempt"], &attempt) != nil {
		t.Fatal(claimed.Body.String())
	}
	for _, field := range []string{"id", "stage", "state", "queue_deadline", "execution_deadline", "lease_expires_at"} {
		if _, ok := order[field]; !ok {
			t.Fatalf("current_order missing %s in %s", field, claimed.Body.String())
		}
	}
	for _, field := range []string{"harness", "model", "session_id", "claimed_at", "last_check_in", "lease_expires_at", "elapsed_seconds"} {
		if _, ok := attempt[field]; !ok {
			t.Fatalf("attempt missing %s in %s", field, claimed.Body.String())
		}
	}
}

type compactView struct {
	Actor, Kind, Label string
	Needs              bool
}

func decodeItems(t *testing.T, response *httptest.ResponseRecorder) map[string]compactView {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var items []struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
		NeedsAttention bool `json:"needs_attention"`
		NextAction     struct {
			Actor, Kind, Label string
		} `json:"next_action"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	out := map[string]compactView{}
	for _, item := range items {
		out[item.Task.ID] = compactView{Actor: item.NextAction.Actor, Kind: item.NextAction.Kind, Label: item.NextAction.Label, Needs: item.NeedsAttention}
	}
	return out
}
