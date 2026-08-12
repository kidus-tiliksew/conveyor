package postgres

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestWorkspaceMembershipAuthorizationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "membership-legacy-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Membership Org", Email: "owner@example.test", DisplayName: "Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapWorkspace := "member-bootstrap-" + core.NewTaskID()
	if seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), bootstrapWorkspace), isolationConfig(bootstrapWorkspace)); err != nil || !seeded {
		t.Fatalf("bootstrap workspace seeded=%t err=%v", seeded, err)
	}
	if allowed, err := st.AuthorizeWorkspace(t.Context(), owner.ID, bootstrapWorkspace, core.CapabilityManageWorkspace); err != nil || !allowed {
		t.Fatalf("legacy bootstrap membership allowed=%t err=%v", allowed, err)
	}
	operatorCredential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	operatorCtx := store.WithCredential(t.Context(), operatorCredential)
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})

	suffix := core.NewTaskID()
	workspaceA, workspaceB := "member-a-"+suffix, "member-b-"+suffix
	for _, workspaceID := range []string{workspaceA, workspaceB} {
		if _, err := st.CreateWorkspace(operatorCtx, workspaceID, workspaceID, isolationConfig(workspaceID)); err != nil {
			t.Fatal(err)
		}
	}
	workspaceBCtx := store.WithWorkspace(operatorCtx, workspaceB)
	if err := st.RevokeWorkspaceRole(workspaceBCtx, owner.ID, workspaceB); !errors.Is(err, store.ErrLastWorkspaceOperator) {
		t.Fatalf("sole operator revocation error=%v", err)
	}
	secondOperator, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_operator_" + suffix, Email: "operator-" + suffix + "@example.test", DisplayName: "Second Operator"})
	if err != nil {
		t.Fatal(err)
	}
	if grant, err := st.GrantWorkspaceRole(workspaceBCtx, secondOperator.Email, workspaceB, core.WorkspaceRoleOperator); err != nil || grant.Membership == nil {
		t.Fatalf("second operator grant=%+v err=%v", grant, err)
	}
	if err := st.RevokeWorkspaceRole(workspaceBCtx, owner.ID, workspaceB); err != nil {
		t.Fatalf("non-sole operator revocation: %v", err)
	}
	member, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_member_" + suffix, Email: "member-" + suffix + "@example.test", DisplayName: "Member"})
	if err != nil {
		t.Fatal(err)
	}
	grantCtx := store.WithWorkspace(operatorCtx, workspaceA)
	grant, err := st.GrantWorkspaceRole(grantCtx, member.Email, workspaceA, core.WorkspaceRoleUser)
	if err != nil || grant.Membership == nil || grant.Membership.UserID != member.ID {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	visible, err := st.ListWorkspacesForUser(t.Context(), member.ID)
	if err != nil || len(visible) != 1 || visible[0].ID != workspaceA {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	if allowed, err := st.AuthorizeWorkspace(t.Context(), member.ID, workspaceA, core.CapabilityProposeDocuments); err != nil || !allowed {
		t.Fatalf("member proposal allowed=%t err=%v", allowed, err)
	}
	if allowed, err := st.AuthorizeWorkspace(t.Context(), member.ID, workspaceA, core.CapabilityManageMembership); err != nil || allowed {
		t.Fatalf("member management allowed=%t err=%v", allowed, err)
	}

	token, err := st.IssuePersonalAccessToken(t.Context(), member.ID, "member test")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspaces, server.Workspace = st, workspaceA
	handler := server.Handler()
	call := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token.Value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	listed := call("/v1/workspaces")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), workspaceA) || strings.Contains(listed.Body.String(), workspaceB) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, route := range []string{"/v1/tasks", "/v1/activity", "/v1/work-orders", "/v1/monitor", "/v1/pending-proposals", "/v1/tasks/unknown/events/stream"} {
		unbound := call(route + "?workspace_id=" + workspaceB)
		missing := call(route + "?workspace_id=missing-workspace")
		if unbound.Code != http.StatusNotFound || unbound.Body.String() != missing.Body.String() {
			t.Fatalf("%s unbound=%d/%q missing=%d/%q", route, unbound.Code, unbound.Body.String(), missing.Code, missing.Body.String())
		}
	}
	now := time.Now().UTC()
	task := core.Task{ID: "revoked-assignee-" + suffix, Workspace: workspaceA, Repo: "repo", BaseBranch: "main", Branch: "conveyor/revoked-assignee", State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(grantCtx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(grantCtx, task.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(grantCtx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(st).CreateWorkOrder(grantCtx, order); err != nil {
		t.Fatal(err)
	}

	if err := st.RevokeWorkspaceRole(grantCtx, member.ID, workspaceA); err != nil {
		t.Fatal(err)
	}
	cleared, err := st.GetTask(grantCtx, task.ID)
	if err != nil || cleared.Assignee != nil {
		t.Fatalf("task after membership revocation assignee=%+v err=%v", cleared.Assignee, err)
	}
	var clearEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND kind='task.assignee.cleared' AND payload_json->>'revoked_user_id'=$3`, workspaceA, task.ID, member.ID).Scan(&clearEvents); err != nil || clearEvents != 1 {
		t.Fatalf("assignment clear audit count=%d err=%v", clearEvents, err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(grantCtx, order.ID, core.WorkOrderClaim{SessionID: "first-come", ClientToken: "first-come", OwnerUserID: owner.ID}); err != nil {
		t.Fatalf("claim after membership revocation: %v", err)
	}
	var audited int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind IN ('workspace.membership_granted','workspace.membership_revoked')`, workspaceA).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("membership audit count=%d err=%v", audited, err)
	}
}
