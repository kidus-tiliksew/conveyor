package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestAttachTaskBranchHTTP(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	task := core.Task{
		ID: "attach-http", Workspace: "demo", Repo: "conveyor", Title: "Attach",
		BaseBranch: "main", Branch: gitx.BranchName("attach-http"), State: core.TaskRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	membership := &membershipFixture{
		workspaces: []core.Workspace{{ID: "demo", Name: "Demo"}},
		roles:      map[string]map[string]core.WorkspaceRole{"usr_attach": {"demo": core.WorkspaceRoleOperator}},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = membership, membership
	server.Credentials = staticCredentialVerifier{
		"operator-token": {ID: "pat_attach", OwnerUserID: "usr_attach", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"agent-token":    {ID: "agt_attach", OwnerUserID: "usr_attach", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
		"child-token":    {ID: "agt_child", OwnerUserID: "usr_attach", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: "demo", RunWorkOrderID: "order", RunSessionID: "session"},
		"worker-token":   {ID: "wrk_attach", OwnerUserID: "usr_attach", Kind: core.CredentialKind("worker")},
	}
	handler := server.Handler()
	call := func(token, taskID, body, workspace string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/branch?workspace_id="+workspace, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, token := range []string{"agent-token", "child-token", "worker-token"} {
		rec := call(token, task.ID, `{"branch":"feature/denied"}`, "demo")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d body=%s", token, rec.Code, rec.Body.String())
		}
	}

	foreign := call("operator-token", task.ID, `{"branch":"feature/demo"}`, "other")
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("workspace scope status=%d body=%q", foreign.Code, foreign.Body.String())
	}

	ok := call("operator-token", task.ID, `{"branch":"feature/demo"}`, "demo")
	if ok.Code != http.StatusOK {
		t.Fatalf("success status=%d body=%s", ok.Code, ok.Body.String())
	}
	var got core.Task
	if err := json.Unmarshal(ok.Body.Bytes(), &got); err != nil || got.Branch != "feature/demo" || got.ID != task.ID {
		t.Fatalf("success body=%s err=%v", ok.Body.String(), err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil || len(events) == 0 || events[len(events)-1].Kind != "task.branch_attached" {
		t.Fatalf("success events=%v err=%v", events, err)
	}

	same := call("operator-token", task.ID, `{"branch":"feature/demo"}`, "demo")
	if same.Code != http.StatusOK {
		t.Fatalf("same-name status=%d body=%s", same.Code, same.Body.String())
	}
	after, err := st.ListEvents(ctx, task.ID)
	if err != nil || len(after) != len(events) {
		t.Fatal("same-name appended an event")
	}

	for _, tc := range []struct {
		body   string
		error  string
		status int
	}{
		{`{"branch":""}`, "invalid_branch", http.StatusBadRequest},
		{`{"branch":"HEAD"}`, "invalid_branch", http.StatusBadRequest},
		{`{"branch":"main"}`, "branch_not_attachable", http.StatusConflict},
		{`{"branch":"` + gitx.BranchName("other-id") + `"}`, "branch_not_attachable", http.StatusConflict},
	} {
		rec := call("operator-token", task.ID, tc.body, "demo")
		if rec.Code != tc.status {
			t.Fatalf("%s status=%d body=%s", tc.error, rec.Code, rec.Body.String())
		}
		assertJSONError(t, rec, tc.error)
		current, getErr := st.GetTask(ctx, task.ID)
		if getErr != nil || current.Branch != "feature/demo" {
			t.Fatalf("%s mutated branch=%s err=%v", tc.error, current.Branch, getErr)
		}
	}

	if _, err := taskops.New(st).Cancel(ctx, core.Intervention{
		TaskID: task.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", Comment: "stop",
	}); err != nil {
		t.Fatal(err)
	}
	terminal := call("operator-token", task.ID, `{"branch":"feature/later"}`, "demo")
	if terminal.Code != http.StatusConflict {
		t.Fatalf("terminal status=%d body=%s", terminal.Code, terminal.Body.String())
	}
	assertJSONError(t, terminal, "task_terminal")

	claimed := core.Task{
		ID: "attach-claimed", Workspace: "demo", Repo: "conveyor", Title: "Claimed",
		BaseBranch: "main", Branch: gitx.BranchName("attach-claimed"), State: core.TaskRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateJob(ctx, core.Job{ID: "attach-claimed-job", TaskID: claimed.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: "attach-claimed-job", JobID: "attach-claimed-job", TaskID: claimed.ID, Stage: core.StageImplement,
		QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, "attach-claimed-job", core.WorkOrderClaim{
		SessionID: "attach-session", ClientToken: "attach-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	claimedRec := call("operator-token", claimed.ID, `{"branch":"feature/claimed"}`, "demo")
	if claimedRec.Code != http.StatusConflict {
		t.Fatalf("claimed status=%d body=%s", claimedRec.Code, claimedRec.Body.String())
	}
	assertJSONError(t, claimedRec, "work_order_claimed")

	pr := core.Task{
		ID: "attach-pr", Workspace: "demo", Repo: "conveyor", Title: "PR",
		BaseBranch: "main", Branch: gitx.BranchName("attach-pr"), State: core.TaskRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: pr.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7})}); err != nil {
		t.Fatal(err)
	}
	prRec := call("operator-token", pr.ID, `{"branch":"feature/pr"}`, "demo")
	if prRec.Code != http.StatusConflict {
		t.Fatalf("pr status=%d body=%s", prRec.Code, prRec.Body.String())
	}
	assertJSONError(t, prRec, "pull_request_recorded")

	holder := core.Task{
		ID: "attach-holder", Workspace: "demo", Repo: "conveyor", Title: "Holder",
		BaseBranch: "main", Branch: gitx.BranchName("attach-holder"), State: core.TaskRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if rec := call("operator-token", holder.ID, `{"branch":"feature/held"}`, "demo"); rec.Code != http.StatusOK {
		t.Fatalf("holder status=%d body=%s", rec.Code, rec.Body.String())
	}
	collider := core.Task{
		ID: "attach-collider", Workspace: "demo", Repo: "conveyor", Title: "Collider",
		BaseBranch: "main", Branch: gitx.BranchName("attach-collider"), State: core.TaskRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, collider); err != nil {
		t.Fatal(err)
	}
	inUse := call("operator-token", collider.ID, `{"branch":"feature/held"}`, "demo")
	if inUse.Code != http.StatusConflict {
		t.Fatalf("in use status=%d body=%s", inUse.Code, inUse.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(inUse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["error"] != "branch_in_use" || payload["other_task_id"] != holder.ID {
		t.Fatalf("in use payload=%v", payload)
	}
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v body=%s", code, err, rec.Body.String())
	}
	if payload["error"] != code {
		t.Fatalf("error=%q want %q body=%s", payload["error"], code, rec.Body.String())
	}
}

func TestAttachTaskBranchHTTPMissingTask(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace = "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/missing/branch?workspace_id=demo", strings.NewReader(`{"branch":"feature/demo"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
