package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/queue/riverqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestIdentityBootstrapAndPersonalAccessTokenLifecycleIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	identity := config.FirstOperatorIdentity{OrganizationName: "Example Org", Email: "owner@example.test", DisplayName: "Example Owner"}
	legacy := "legacy-token-value"
	seeded, err := st.BootstrapIdentity(t.Context(), identity, legacy)
	if err != nil || !seeded {
		t.Fatalf("bootstrap seeded=%t err=%v", seeded, err)
	}
	var orgCount, userCount, tokenCount int
	if err := st.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM orgs), (SELECT count(*) FROM users), (SELECT count(*) FROM user_tokens)`).Scan(&orgCount, &userCount, &tokenCount); err != nil {
		t.Fatal(err)
	}
	if orgCount != 1 || userCount != 1 || tokenCount != 1 {
		t.Fatalf("bootstrap counts org=%d user=%d token=%d, want 1/1/1", orgCount, userCount, tokenCount)
	}
	var markedCount int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_tokens WHERE deployment_credential`).Scan(&markedCount); err != nil || markedCount != 1 {
		t.Fatalf("bootstrap deployment marker count=%d err=%v", markedCount, err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil || principal.Email != identity.Email || principal.Status != "active" {
		t.Fatalf("legacy verification principal=%+v err=%v", principal, err)
	}
	workspace := "identity-rotation-" + core.NewTaskID()
	if seededWorkspace, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), workspace), isolationConfig(workspace)); err != nil || !seededWorkspace {
		t.Fatalf("bootstrap rotation workspace seeded=%t err=%v", seededWorkspace, err)
	}
	var storedHash []byte
	if err := st.pool.QueryRow(t.Context(), `SELECT token_hash FROM user_tokens WHERE deployment_credential`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(legacy))
	if string(storedHash) != string(wantHash[:]) || string(storedHash) == legacy {
		t.Fatal("legacy token was not stored exclusively as its SHA-256 hash")
	}

	seeded, err = st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Replacement", Email: "other@example.test", DisplayName: "Other"}, "replacement-token")
	if err != nil || !seeded {
		t.Fatalf("rotation bootstrap changed=%t err=%v, want remap", seeded, err)
	}
	var orgName, email string
	if err := st.pool.QueryRow(t.Context(), `SELECT name FROM orgs`).Scan(&orgName); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT email FROM users`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if orgName != identity.OrganizationName || email != identity.Email {
		t.Fatalf("restart overwrote identity: org=%q email=%q", orgName, email)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), legacy); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("old token verification err=%v, want invalid", err)
	}
	if rotated, err := st.VerifyPersonalAccessToken(t.Context(), "replacement-token"); err != nil || rotated.ID != principal.ID {
		t.Fatalf("replacement token principal=%+v err=%v", rotated, err)
	}
	var rotationEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_token_rotated'
		AND payload_json ? 'credential_id' AND payload_json::text NOT LIKE '%replacement-token%'`).Scan(&rotationEvents); err != nil || rotationEvents != 1 {
		t.Fatalf("rotation audit count=%d err=%v", rotationEvents, err)
	}
	seeded, err = st.BootstrapIdentity(t.Context(), identity, "replacement-token")
	if err != nil || seeded {
		t.Fatalf("healthy restart bootstrap changed=%t err=%v, want no-op", seeded, err)
	}

	issued, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "CLI")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Value == "" || !strings.HasPrefix(issued.Value, "cv_pat_") || strings.Count(issued.Value, ".") == 2 {
		t.Fatal("issued token does not have the opaque non-JWT format")
	}
	if verified, err := st.VerifyPersonalAccessToken(t.Context(), issued.Value); err != nil || verified.ID != principal.ID {
		t.Fatalf("issued token verification principal=%+v err=%v", verified, err)
	}
	agent, err := st.IssueAgentCredential(t.Context(), principal.ID, "Codex")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := st.VerifyCredential(t.Context(), agent.Value)
	if err != nil || resolved.ID != agent.ID || resolved.OwnerUserID != principal.ID || resolved.Kind != core.CredentialAgent || resolved.Scope != core.CredentialScopeUser {
		t.Fatalf("resolved agent credential=%+v err=%v", resolved, err)
	}
	if _, err = st.VerifyPersonalAccessToken(t.Context(), agent.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("agent credential authenticated as PAT: %v", err)
	}
	if _, err = st.RevokePersonalAccessToken(t.Context(), agent.ID); err == nil {
		t.Fatal("personal-token revocation accepted an agent credential")
	}
	if resolved, err = st.VerifyCredential(t.Context(), agent.Value); err != nil || resolved.Kind != core.CredentialAgent {
		t.Fatalf("agent credential changed by PAT revocation: credential=%+v err=%v", resolved, err)
	}
	if err = st.RevokeAgentCredential(t.Context(), principal.ID, agent.ID); err != nil {
		t.Fatalf("revoke agent credential: %v", err)
	}
	if _, err = st.VerifyCredential(t.Context(), agent.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("revoked agent verification err=%v, want invalid", err)
	}
	if revoked, err := st.RevokePersonalAccessToken(t.Context(), issued.ID); err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke token=%+v err=%v", revoked, err)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), issued.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("revoked verification err=%v, want invalid", err)
	}

	second, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	boundLabel, err := store.RunAgentCredentialLabel(store.RunAgentCredentialBinding{WorkspaceID: "demo", WorkOrderID: "order-1", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	ownerBoundAgent, err := st.IssueAgentCredential(t.Context(), principal.ID, boundLabel)
	if err != nil {
		t.Fatal(err)
	}
	if credential, verifyErr := st.VerifyCredential(t.Context(), ownerBoundAgent.Value); verifyErr != nil || credential.RunWorkspaceID != "demo" || credential.RunWorkOrderID != "order-1" || credential.RunSessionID != "session-1" {
		t.Fatalf("bound agent credential=%+v err=%v", credential, verifyErr)
	}
	revocationBinding := store.RunAgentCredentialBinding{WorkspaceID: "demo", WorkOrderID: "order-2", SessionID: "session-2"}
	revocationLabel, err := store.RunAgentCredentialLabel(revocationBinding)
	if err != nil {
		t.Fatal(err)
	}
	revocableAgent, err := st.IssueAgentCredential(t.Context(), principal.ID, revocationLabel)
	if err != nil {
		t.Fatal(err)
	}
	for name, mismatched := range map[string]store.RunAgentCredentialBinding{
		"workspace":  {WorkspaceID: "other", WorkOrderID: "order-2", SessionID: "session-2"},
		"work order": {WorkspaceID: "demo", WorkOrderID: "order-3", SessionID: "session-2"},
		"session":    {WorkspaceID: "demo", WorkOrderID: "order-2", SessionID: "session-3"},
	} {
		if revokeErr := st.RevokeRunAgentCredential(t.Context(), principal.ID, revocableAgent.ID, mismatched); !errors.Is(revokeErr, store.ErrRunAgentCredentialBindingMismatch) {
			t.Fatalf("%s mismatch revoke err=%v", name, revokeErr)
		}
		if credential, verifyErr := st.VerifyCredential(t.Context(), revocableAgent.Value); verifyErr != nil || credential.ID != revocableAgent.ID {
			t.Fatalf("%s mismatch changed credential=%+v err=%v", name, credential, verifyErr)
		}
	}
	if err = st.RevokeRunAgentCredential(t.Context(), principal.ID, revocableAgent.ID, revocationBinding); err != nil {
		t.Fatalf("revoke bound agent credential: %v", err)
	}
	if err = st.RevokeRunAgentCredential(t.Context(), principal.ID, revocableAgent.ID, revocationBinding); err != nil {
		t.Fatalf("repeat bound agent revocation: %v", err)
	}
	if _, err = st.VerifyCredential(t.Context(), revocableAgent.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("revoked bound agent verification err=%v, want invalid", err)
	}
	if user, err := st.DeactivateIdentityUser(t.Context(), principal.ID); err != nil || user.Status != "deactivated" {
		t.Fatalf("deactivate user=%+v err=%v", user, err)
	}
	if _, err := st.VerifyPersonalAccessToken(t.Context(), second.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("deactivated-user verification err=%v, want invalid", err)
	}
	if _, err := st.VerifyCredential(t.Context(), ownerBoundAgent.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("deactivated-owner agent verification err=%v, want invalid", err)
	}
	if _, err := st.IssuePersonalAccessToken(t.Context(), principal.ID, "forbidden"); err == nil {
		t.Fatal("issued token for deactivated user")
	}
}

func TestFailedInvitationRevocationLeavesInvitationRedeemableIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "revocation-failure-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Revocation Org", Email: "owner@example.test", DisplayName: "Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	operatorCtx := store.WithCredential(t.Context(), credential)
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	workspaceID := "revocation-failure-" + core.NewTaskID()
	if _, err = st.CreateWorkspace(operatorCtx, workspaceID, workspaceID, isolationConfig(workspaceID)); err != nil {
		t.Fatal(err)
	}

	server := httpapi.NewServer(st)
	server.Workspaces, server.Workspace = st, workspaceID
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+legacy)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	missing := call(http.MethodDelete, "/v1/workspaces/"+workspaceID+"/invitations/missing@example.test", "")
	canonicalNotFound := "{\"error\":\"workspace_not_found\",\"message\":\"workspace not found\"}\n"
	if missing.Code != http.StatusNotFound || missing.Body.String() != canonicalNotFound {
		t.Fatalf("missing invitation status=%d body=%q", missing.Code, missing.Body.String())
	}

	invitedEmail := "still-pending@example.test"
	grant := call(http.MethodPost, "/v1/workspaces/"+workspaceID+"/members", `{"email":"`+invitedEmail+`","role":"contributor"}`)
	if grant.Code != http.StatusCreated {
		t.Fatalf("grant invitation status=%d body=%s", grant.Code, grant.Body.String())
	}
	if _, err = st.pool.Exec(t.Context(), `CREATE FUNCTION reject_invitation_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected invitation delete failure with secret detail'; END $$;
		CREATE TRIGGER reject_invitation_delete BEFORE DELETE ON workspace_membership_invitations
		FOR EACH ROW EXECUTE FUNCTION reject_invitation_delete()`); err != nil {
		t.Fatal(err)
	}
	failure := call(http.MethodDelete, "/v1/workspaces/"+workspaceID+"/invitations/"+invitedEmail, "")
	if failure.Code != http.StatusInternalServerError || failure.Body.String() != "internal server error\n" || strings.Contains(failure.Body.String(), "secret detail") {
		t.Fatalf("failed revocation status=%d body=%q", failure.Code, failure.Body.String())
	}
	var invitations int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2`, workspaceID, invitedEmail).Scan(&invitations); err != nil || invitations != 1 {
		t.Fatalf("pending invitation count=%d err=%v", invitations, err)
	}
	if _, err = st.pool.Exec(t.Context(), `DROP TRIGGER reject_invitation_delete ON workspace_membership_invitations;
		DROP FUNCTION reject_invitation_delete()`); err != nil {
		t.Fatal(err)
	}

	provision := call(http.MethodPost, "/v1/users", `{"email":"`+invitedEmail+`","display_name":"Still Pending"}`)
	if provision.Code != http.StatusCreated {
		t.Fatalf("provision after failed revocation status=%d body=%s", provision.Code, provision.Body.String())
	}
	var user core.IdentityUser
	if err = json.Unmarshal(provision.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	var bindings int
	if err = st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2),
		(SELECT count(*) FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$3)`,
		workspaceID, user.ID, invitedEmail).Scan(&bindings, &invitations); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || invitations != 0 {
		t.Fatalf("post-provision bindings=%d invitations=%d", bindings, invitations)
	}
}

func TestIdentityBootstrapRevocationAndDeploymentAuditIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	identity := config.FirstOperatorIdentity{OrganizationName: "Audit Org", Email: "audit-owner@example.test", DisplayName: "Audit Owner"}
	if _, err := st.BootstrapIdentity(t.Context(), identity, "legacy-one"); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.BootstrapIdentity(t.Context(), identity, "legacy-two"); err != nil || !changed {
		t.Fatalf("zero-workspace rotation changed=%t err=%v", changed, err)
	}
	var rotated int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_token_rotated'`).Scan(&rotated); err != nil || rotated != 1 {
		t.Fatalf("zero-workspace rotation events=%d err=%v", rotated, err)
	}

	principal, err := st.VerifyPersonalAccessToken(t.Context(), "legacy-two")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "identity-heal-" + core.NewTaskID()
	if seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), workspace), isolationConfig(workspace)); err != nil || !seeded {
		t.Fatalf("workspace bootstrap seeded=%t err=%v", seeded, err)
	}
	if _, err := st.pool.Exec(t.Context(), `DELETE FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspace, principal.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.BootstrapIdentity(t.Context(), identity, "legacy-two"); err != nil || !changed {
		t.Fatalf("binding healing changed=%t err=%v", changed, err)
	}
	var healed int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_bindings_healed'`).Scan(&healed); err != nil || healed != 1 {
		t.Fatalf("binding-healing events=%d err=%v", healed, err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events WHERE kind='identity.legacy_token_rotated'`).Scan(&rotated); err != nil || rotated != 1 {
		t.Fatalf("binding healing emitted rotation events=%d err=%v", rotated, err)
	}

	var tokenID string
	if err := st.pool.QueryRow(t.Context(), `SELECT id FROM user_tokens WHERE deployment_credential`).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokePersonalAccessToken(t.Context(), tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapIdentity(t.Context(), identity, "legacy-two"); err == nil || !strings.Contains(err.Error(), "legacy token revoked; remove CONVEYOR_API_TOKEN or issue a new PAT") {
		t.Fatalf("revoked legacy restart error=%v", err)
	}
	var revoked bool
	if err := st.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM user_tokens WHERE id=$1`, tokenID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("revoked legacy mapping resurrected=%t err=%v", !revoked, err)
	}
}

func TestDeploymentMutationAuthorizationTracksLiveOperatorBindingsIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "live-binding-http-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Live Org", Email: "live-owner@example.test", DisplayName: "Live Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	workspace := "live-binding-" + core.NewTaskID()
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err := st.CreateWorkspace(operatorCtx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	pat, err := st.IssuePersonalAccessToken(t.Context(), owner.ID, "live binding")
	if err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspaces, server.Workspace = st, workspace
	handler := server.Handler()
	callWithToken := func(token, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		return callWithToken(pat.Value, method, path, body)
	}
	if response := call(http.MethodPost, "/v1/users", `{"email":"before@example.test","display_name":"Before"}`); response.Code != http.StatusCreated {
		t.Fatalf("authorized provisioning status=%d body=%s", response.Code, response.Body.String())
	}
	second, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_second_" + core.NewTaskID(), Email: "second-" + core.NewTaskID() + "@example.test", DisplayName: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'contributor')`, workspace, second.ID); err != nil {
		t.Fatal(err)
	}
	userScopePAT, err := st.IssuePersonalAccessToken(t.Context(), second.ID, "issued as user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `UPDATE workspace_role_bindings SET role='operator' WHERE workspace_id=$1 AND user_id=$2`, workspace, second.ID); err != nil {
		t.Fatal(err)
	}
	if response := callWithToken(userScopePAT.Value, http.MethodPost, "/v1/users", `{"email":"scope-denied@example.test","display_name":"Scope Denied"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("user-scope PAT status=%d body=%s", response.Code, response.Body.String())
	}
	revokeCtx := store.WithCredential(store.WithWorkspace(operatorCtx, workspace), core.AuthenticatedCredential{ID: pat.ID, OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	if err := st.RevokeWorkspaceRole(revokeCtx, owner.ID, workspace); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/users", `{"email":"denied@example.test","display_name":"Denied"}`},
		{http.MethodPost, "/v1/workspaces", `{}`},
	} {
		if response := call(request.method, request.path, request.body); response.Code != http.StatusUnauthorized {
			t.Fatalf("revoked %s status=%d body=%s", request.path, response.Code, response.Body.String())
		}
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'operator')`, workspace, owner.ID); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodPost, "/v1/users", `{"email":"restored@example.test","display_name":"Restored"}`); response.Code != http.StatusCreated {
		t.Fatalf("re-granted provisioning status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHTTPMutationDerivesLegacyUserAndRejectsAgentCredentialIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "legacy-http-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "HTTP Org", Email: "owner@example.test", DisplayName: "Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.IssueAgentCredential(t.Context(), principal.ID, "integration agent")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "identity-http-" + core.NewTaskID()
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "legacy", OwnerUserID: principal.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(principal.ID), Role: core.ActorUser})
	if _, err = st.CreateWorkspace(operatorCtx, workspace, workspace, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(operatorCtx, workspace)
	taskID := "identity-event-" + core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Source: "test", Title: "Credential actor", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace = workspace
	handler := server.Handler()
	call := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/close?workspace_id="+workspace, strings.NewReader(`{"reason":"integration"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Conveyor-Actor", "attacker")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(agent.Value); response.Code != http.StatusUnauthorized {
		t.Fatalf("agent mutation status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(legacy); response.Code != http.StatusAccepted {
		t.Fatalf("legacy mutation status=%d body=%s", response.Code, response.Body.String())
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task.cancelled" {
			found = event.ActorID == store.UserActorID(principal.ID) && event.ActorRole == core.ActorUser
			if strings.Contains(string(event.Payload), legacy) || strings.Contains(string(event.Payload), agent.Value) || strings.Contains(string(event.Payload), "attacker") {
				t.Fatalf("credential or asserted actor leaked into event: %s", event.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("credential-derived user actor missing from events: %+v", events)
	}
}

func TestIssuedRunAgentCredentialCompletesMCPStageLifecyclesAndRejectsOperatorActsIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "run-agent-lifecycle-operator"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Run Agent Org", Email: "owner@example.test", DisplayName: "Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	workspace := "run-agent-lifecycle-" + core.NewTaskID()
	cfg := isolationConfig(workspace)
	for stage, route := range cfg.Routing.Stages {
		if stage == string(core.StageSpec) || stage == string(core.StageImplement) || stage == string(core.StageReview) {
			route.Execution = config.ExecutionMCP
			cfg.Routing.Stages[stage] = route
		}
	}
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{
		ID: "deployment", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator,
	})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = st.CreateWorkspace(operatorCtx, workspace, workspace, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(operatorCtx, workspace)
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	orders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	server := httpapi.NewServer(st)
	server.Workspace, server.ConfigProvider, server.WorkOrders, server.Workers = workspace, provider, orders, workers
	handler := server.Handler()

	call := func(token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	type mcpEnvelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	mcpCall := func(token, name string, args map[string]any) mcpEnvelope {
		t.Helper()
		payload, marshalErr := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		response := call(token, http.MethodPost, "/mcp", string(payload))
		if response.Code != http.StatusOK {
			t.Fatalf("MCP %s status=%d body=%s", name, response.Code, response.Body.String())
		}
		var envelope mcpEnvelope
		if unmarshalErr := json.Unmarshal(response.Body.Bytes(), &envelope); unmarshalErr != nil {
			t.Fatalf("decode MCP %s: %v body=%s", name, unmarshalErr, response.Body.String())
		}
		return envelope
	}
	createOrder := func(stage core.Stage) (core.Task, core.WorkOrder) {
		t.Helper()
		taskID := "run-agent-" + string(stage) + "-" + core.NewTaskID()
		task := core.Task{ID: taskID, Workspace: workspace, Source: "test", Title: "Run agent " + string(stage), Repo: "repo", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: stage, PolicyVersion: 1, CreatedAt: time.Now().UTC()}
		if createErr := st.CreateTask(ctx, task); createErr != nil {
			t.Fatal(createErr)
		}
		job := core.Job{ID: taskID + "-" + string(stage) + "-1", TaskID: taskID, Stage: stage, State: core.JobPending}
		if createErr := st.CreateJob(ctx, job); createErr != nil {
			t.Fatal(createErr)
		}
		order := core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().Add(time.Hour), CreatedAt: time.Now().UTC()}
		if stage == core.StageReview {
			order.ReviewRound, order.ReviewSeat = 1, 1
			order.ServedRequirementSnapshot = []core.ServedRequirementContext{}
			order.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}, PendingDesignProposals: []core.PendingSystemDesignProposal{}}
		}
		if createErr := storetest.For(st).CreateWorkOrder(ctx, order); createErr != nil {
			t.Fatal(createErr)
		}
		return task, order
	}
	claimAndIssue := func(task core.Task, order core.WorkOrder) (string, string) {
		t.Helper()
		sessionID := "session-" + string(order.Stage) + "-" + core.NewTaskID()
		basePath := "/v1/tasks/" + task.ID + "/run-orders/" + order.ID
		claimBody, _ := json.Marshal(map[string]string{"session_id": sessionID, "client_token": "client-" + sessionID, "agent": "codex", "model": "integration"})
		claimed := call(legacy, http.MethodPost, basePath+"/claim?workspace_id="+workspace, string(claimBody))
		if claimed.Code != http.StatusOK {
			t.Fatalf("claim %s status=%d body=%s", order.Stage, claimed.Code, claimed.Body.String())
		}
		issueBody, _ := json.Marshal(map[string]string{"session_id": sessionID})
		issuedResponse := call(legacy, http.MethodPost, basePath+"/agent-credential?workspace_id="+workspace, string(issueBody))
		if issuedResponse.Code != http.StatusCreated {
			t.Fatalf("issue %s status=%d body=%s", order.Stage, issuedResponse.Code, issuedResponse.Body.String())
		}
		var issued store.IssuedAgentCredential
		if decodeErr := json.Unmarshal(issuedResponse.Body.Bytes(), &issued); decodeErr != nil || issued.ID == "" || issued.Value == "" {
			t.Fatalf("decode issued %s credential=%+v err=%v", order.Stage, issued, decodeErr)
		}
		verified, verifyErr := st.VerifyCredential(t.Context(), issued.Value)
		if verifyErr != nil || verified.Kind != core.CredentialAgent || verified.OwnerUserID != owner.ID || verified.RunWorkspaceID != workspace || verified.RunWorkOrderID != order.ID || verified.RunSessionID != sessionID {
			t.Fatalf("verified %s credential=%+v err=%v", order.Stage, verified, verifyErr)
		}
		return sessionID, issued.Value
	}
	baseArgs := func(order core.WorkOrder, sessionID string) map[string]any {
		return map[string]any{"workspace_id": workspace, "work_order_id": order.ID, "session_id": sessionID}
	}

	specTask, specOrder := createOrder(core.StageSpec)
	specSession, specAgent := claimAndIssue(specTask, specOrder)
	specArgs := baseArgs(specOrder, specSession)
	specArgs["markdown"] = "## Approach\nExercise the issued credential.\n\n## Files touched\n- internal/httpapi/mcp.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Authorization drift.\n\n## Done criteria\n- The issued credential submits the plan."
	specArgs["decomposition"] = []any{}
	if result := mcpCall(specAgent, "submit_plan", specArgs); result.Result.IsError {
		t.Fatalf("issued spec credential failed: %+v", result)
	}
	if submitted, getErr := st.GetWorkOrder(ctx, specOrder.ID); getErr != nil || submitted.State != core.WorkOrderCompleted {
		t.Fatalf("submitted spec order=%+v err=%v", submitted, getErr)
	}

	implementTask, implementOrder := createOrder(core.StageImplement)
	implementSession, implementAgent := claimAndIssue(implementTask, implementOrder)
	if result := mcpCall(implementAgent, "submit_for_review", baseArgs(implementOrder, implementSession)); result.Result.IsError {
		t.Fatalf("issued implement credential failed: %+v", result)
	}
	if submitted, getErr := st.GetWorkOrder(ctx, implementOrder.ID); getErr != nil || submitted.State != core.WorkOrderSubmitted {
		t.Fatalf("submitted implement order=%+v err=%v", submitted, getErr)
	}

	reviewTask, reviewOrder := createOrder(core.StageReview)
	reviewSession, reviewAgent := claimAndIssue(reviewTask, reviewOrder)
	reviewArgs := baseArgs(reviewOrder, reviewSession)
	reviewArgs["verdict"], reviewArgs["reason_code"], reviewArgs["summary"], reviewArgs["feedback"] = "changes_requested", "coverage", "Issued credential completed review", "Exercise the correction bounce without an external forge."
	reviewArgs["requirement_citations"] = map[string]any{"applicable": false, "cited_ids": []any{}, "unknown_ids": []any{}, "unserved_ids": []any{}, "conflicts": []any{}}
	reviewArgs["done_criteria_coverage"] = map[string]any{"applicable": false, "summary": "No approved plan", "satisfied": []any{}, "unsatisfied": []any{}, "unverified": []any{}, "conflicts": []any{}}
	reviewArgs["governance_assessment"] = map[string]any{"design_applicable": false, "decision_citable": false, "cited_ids": []any{}, "unknown_ids": []any{}, "ungoverned_ids": []any{}, "superseded_ids": []any{}, "conflicts": []any{}}
	if result := mcpCall(reviewAgent, "submit_review_verdict", reviewArgs); result.Result.IsError {
		t.Fatalf("issued review credential failed: %+v", result)
	}
	if submitted, getErr := st.GetWorkOrder(ctx, reviewOrder.ID); getErr != nil || submitted.State != core.WorkOrderCompleted {
		t.Fatalf("submitted review order=%+v err=%v", submitted, getErr)
	}

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/requirements/missing/versions/1/confirm?workspace_id=" + workspace, "{}"},
		{http.MethodPost, "/v1/workspaces/" + workspace + "/members", `{"email":"denied@example.test","role":"contributor"}`},
		{http.MethodPost, "/v1/tokens", `{"label":"denied"}`},
	} {
		if response := call(reviewAgent, route.method, route.path, route.body); response.Code != http.StatusUnauthorized {
			t.Fatalf("agent operator route %s status=%d body=%s", route.path, response.Code, response.Body.String())
		}
	}
	for _, tool := range []string{"create_task", "set_assignee", "redispatch_work_order", "report_continuation"} {
		result := mcpCall(reviewAgent, tool, map[string]any{"workspace_id": workspace})
		if !result.Result.IsError || len(result.Result.Content) == 0 || !strings.Contains(result.Result.Content[0].Text, "operator-scoped user credential") {
			t.Fatalf("human-reserved MCP tool %s result=%+v", tool, result)
		}
	}
}

func TestCallerIdentityHTTPUsesHumanCredentialAndOptionalWorkspaceRoleIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "caller-identity-legacy"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Caller Identity Org", Email: "caller@example.test", DisplayName: "Caller",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.IssueAgentCredential(t.Context(), principal.ID, "caller identity agent")
	if err != nil {
		t.Fatal(err)
	}
	workspace := "caller-identity-" + core.NewTaskID()
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "legacy", OwnerUserID: principal.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(principal.ID), Role: core.ActorUser})
	if _, err = st.CreateWorkspace(operatorCtx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	workerCredential := "caller-worker-" + core.NewTaskID()
	if err = st.CreateWorker(store.WithWorkspace(operatorCtx, workspace), core.Worker{
		ID: "worker-" + core.NewTaskID(), Workspace: workspace, OwnerUserID: principal.ID,
		Name: "caller worker", CredentialHash: workerCredential, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	handler := httpapi.NewServer(st).Handler()
	call := func(token, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	unscoped := call(legacy, "/v1/me?user_id=usr-attacker")
	if unscoped.Code != http.StatusOK || !strings.Contains(unscoped.Body.String(), `"id":"`+principal.ID+`"`) ||
		!strings.Contains(unscoped.Body.String(), `"email":"caller@example.test"`) || strings.Contains(unscoped.Body.String(), `"role"`) {
		t.Fatalf("unscoped status=%d body=%s", unscoped.Code, unscoped.Body.String())
	}
	scoped := call(legacy, "/v1/me?workspace_id="+workspace+"&user_id=usr-attacker")
	if scoped.Code != http.StatusOK || !strings.Contains(scoped.Body.String(), `"id":"`+principal.ID+`"`) || !strings.Contains(scoped.Body.String(), `"role":"operator"`) {
		t.Fatalf("scoped status=%d body=%s", scoped.Code, scoped.Body.String())
	}
	for name, token := range map[string]string{"agent": agent.Value, "worker": workerCredential} {
		response := call(token, "/v1/me?workspace_id="+workspace)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), principal.Email) {
			t.Fatalf("%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	hidden := call(legacy, "/v1/me?workspace_id=absent")
	if hidden.Code != http.StatusNotFound || strings.Contains(hidden.Body.String(), principal.Email) {
		t.Fatalf("hidden status=%d body=%s", hidden.Code, hidden.Body.String())
	}
}

func TestOwnDisplayNameSessionMutationAndAuditIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "profile-owner-legacy"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Profile Org", Email: "profile@example.test", DisplayName: "Provisioned Name",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	actorCtx := store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(principal.ID), Role: core.ActorUser})
	issued, err := st.IssueSignInLink(actorCtx, principal.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(t.Context(), issued.Value)
	if err != nil {
		t.Fatal(err)
	}

	handler := httpapi.NewServer(st).Handler()
	request := httptest.NewRequest(http.MethodPut, "http://conveyor.example/v1/me", strings.NewReader(`{"display_name":"  Chosen Name  "}`))
	request.AddCookie(&http.Cookie{Name: "conveyor_session", Value: session.Value})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Conveyor-CSRF", "1")
	request.Header.Set("Origin", "http://conveyor.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"`+principal.ID+`"`) ||
		!strings.Contains(response.Body.String(), `"display_name":"Chosen Name"`) {
		t.Fatalf("session status=%d body=%s", response.Code, response.Body.String())
	}

	bearer := httptest.NewRequest(http.MethodPut, "/v1/me", strings.NewReader(`{"display_name":"Bearer Name"}`))
	bearer.Header.Set("Authorization", "Bearer "+legacy)
	bearer.Header.Set("Content-Type", "application/json")
	bearerResponse := httptest.NewRecorder()
	handler.ServeHTTP(bearerResponse, bearer)
	if bearerResponse.Code != http.StatusBadRequest {
		t.Fatalf("bearer status=%d body=%s", bearerResponse.Code, bearerResponse.Body.String())
	}

	var storedName, actorID, payload string
	if err = st.pool.QueryRow(t.Context(), `SELECT u.display_name,e.actor_id,e.payload_json::text
		FROM users u JOIN deployment_events e ON e.kind='identity.display_name_changed'
		WHERE u.id=$1`, principal.ID).Scan(&storedName, &actorID, &payload); err != nil {
		t.Fatal(err)
	}
	if storedName != "Chosen Name" || actorID != store.UserActorID(principal.ID) ||
		strings.Contains(payload, "Chosen Name") || strings.Contains(payload, session.Value) {
		t.Fatalf("stored=%q actor=%q payload=%s", storedName, actorID, payload)
	}
	if _, err = st.SetOwnDisplayName(actorCtx, "usr-attacker", session.ID, "Attacker Name"); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("cross-user update err=%v", err)
	}
}

func TestIdentityBootstrapConcurrentStartsConvergeIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	identity := config.FirstOperatorIdentity{OrganizationName: "Concurrent Org", Email: "owner@example.test", DisplayName: "Owner"}
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			seeded, err := st.BootstrapIdentity(t.Context(), identity, "shared-legacy-token")
			results <- seeded
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	seededCount := 0
	for seeded := range results {
		if seeded {
			seededCount++
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if seededCount != 1 {
		t.Fatalf("concurrent bootstrap seeded count=%d, want 1", seededCount)
	}
	var users, tokens, marked int
	if err := st.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM user_tokens), (SELECT count(*) FROM user_tokens WHERE deployment_credential)`).Scan(&users, &tokens, &marked); err != nil {
		t.Fatal(err)
	}
	if users != 1 || tokens != 1 || marked != 1 {
		t.Fatalf("concurrent bootstrap rows users=%d tokens=%d marked=%d, want 1/1/1", users, tokens, marked)
	}
}

func TestDeploymentCredentialMarkerMigrationBackfillAndRerunIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 97)
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO users(id,email,display_name,status)
		VALUES('usr_marker_owner','marker-owner@example.test','Marker Owner','active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO user_tokens(id,user_id,label,token_hash,kind,scope)
		VALUES('pat_legacy_marker','usr_marker_owner','legacy API token',decode(repeat('01',32),'hex'),'user','operator')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 98); err != nil {
		t.Fatalf("apply marker migration: %v", err)
	}

	var marked bool
	var markedCount int
	if err := st.pool.QueryRow(t.Context(), `SELECT deployment_credential FROM user_tokens WHERE id='pat_legacy_marker'`).Scan(&marked); err != nil || !marked {
		t.Fatalf("backfilled marker=%t err=%v", marked, err)
	}
	// The legacy label is display-only after migration and may collide.
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO user_tokens(id,user_id,label,token_hash,kind,scope)
		VALUES('pat_label_collision','usr_marker_owner','legacy API token',decode(repeat('02',32),'hex'),'user','operator')`); err != nil {
		t.Fatalf("duplicate display label rejected: %v", err)
	}

	raw, err := migrationFiles.ReadFile("migrations/098_deployment_credential_marker.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql, err := renderMigration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), string(sql)); err != nil {
		t.Fatalf("rerun marker migration: %v", err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_tokens WHERE deployment_credential`).Scan(&markedCount); err != nil || markedCount != 1 {
		t.Fatalf("marker count after rerun=%d err=%v", markedCount, err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO user_tokens(id,user_id,label,token_hash,kind,scope,deployment_credential)
		VALUES('pat_second_marker','usr_marker_owner','other label',decode(repeat('03',32),'hex'),'user','operator',true)`); err == nil {
		t.Fatal("deployment marker uniqueness accepted a second marked row")
	}
}

func TestIdentityMigrationsUpgradeExistingWorkspaceIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 80)
	workspace := "identity-upgrade-" + core.NewTaskID()
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO workspaces (id,name,config_yaml) VALUES ($1,$1,'')`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO users(id,identity_provider_ref,role) VALUES('phase2-user','legacy-subject','operator')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 0); err != nil {
		t.Fatalf("upgrade from migration 080: %v", err)
	}
	identity := config.FirstOperatorIdentity{OrganizationName: "Upgraded Org", Email: "upgrade-owner@example.test", DisplayName: "Upgrade Owner"}
	if seeded, err := st.BootstrapIdentity(t.Context(), identity, "upgrade-legacy-token"); err != nil || !seeded {
		t.Fatalf("upgrade bootstrap seeded=%t err=%v", seeded, err)
	}
	principal, err := st.VerifyPersonalAccessToken(t.Context(), "upgrade-legacy-token")
	if err != nil || principal.Email != identity.Email {
		t.Fatalf("upgrade legacy token principal=%+v err=%v", principal, err)
	}
	var legacyUsers, orgs int
	if err := st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM users WHERE email LIKE '%@legacy.invalid'),
		(SELECT count(*) FROM orgs)`).Scan(&legacyUsers, &orgs); err != nil || legacyUsers != 1 || orgs != 1 {
		t.Fatalf("upgrade preserved users=%d orgs=%d err=%v", legacyUsers, orgs, err)
	}
	if seeded, err := st.BootstrapIdentity(t.Context(), identity, "upgrade-legacy-token"); err != nil || seeded {
		t.Fatalf("upgrade healthy restart seeded=%t err=%v", seeded, err)
	}
	var orgID string
	if err := st.pool.QueryRow(t.Context(), `SELECT org_id FROM workspaces WHERE id=$1`, workspace).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if orgID != "deployment" {
		t.Fatalf("upgraded workspace org_id=%q, want deployment", orgID)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO orgs (id,name) VALUES ('second','Second')`); err == nil {
		t.Fatal("singleton organization constraint accepted a second row")
	}
	if _, err := st.pool.Exec(t.Context(), `DELETE FROM orgs WHERE id='deployment'`); err == nil {
		t.Fatal("singleton organization could be deleted")
	}
	if _, err := st.pool.Exec(t.Context(), `UPDATE workspaces SET org_id='missing' WHERE id=$1`, workspace); err == nil {
		t.Fatal("workspace organization foreign key accepted a missing organization")
	}
}

func TestSelfServicePersonalAccessTokensAreOwnerScopedIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "self-service-legacy-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Self Service Org", Email: "owner@example.test", DisplayName: "Owner",
	}, legacy); err != nil {
		t.Fatal(err)
	}
	suffix := core.NewTaskID()
	alice, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_alice_" + suffix, Email: "alice-" + suffix + "@example.test", DisplayName: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_bob_" + suffix, Email: "bob-" + suffix + "@example.test", DisplayName: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	// Each account's first credential is seeded the way provisioning hands one
	// out; from there the surface under test is the only way to mint more.
	aliceSeed, err := st.IssuePersonalAccessToken(t.Context(), alice.ID, "seed")
	if err != nil {
		t.Fatal(err)
	}
	bobSeed, err := st.IssuePersonalAccessToken(t.Context(), bob.ID, "seed")
	if err != nil {
		t.Fatal(err)
	}

	handler := httpapi.NewServer(st).Handler()
	call := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	// The bootstrap credential is marked only in its owner's non-secret list.
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	ownerListed := call(http.MethodGet, "/v1/tokens", legacy, "")
	var ownerTokens []core.PersonalAccessToken
	if ownerListed.Code != http.StatusOK {
		t.Fatalf("deployment token list status=%d body=%s", ownerListed.Code, ownerListed.Body.String())
	}
	if err := json.Unmarshal(ownerListed.Body.Bytes(), &ownerTokens); err != nil {
		t.Fatalf("decode deployment token list: %v", err)
	}
	if len(ownerTokens) != 1 || ownerTokens[0].UserID != owner.ID || !ownerTokens[0].DeploymentCredential {
		t.Fatalf("deployment token list=%+v", ownerTokens)
	}
	deploymentTokenID := ownerTokens[0].ID
	for _, forbidden := range []string{legacy, "token_hash", `"value"`} {
		if strings.Contains(ownerListed.Body.String(), forbidden) {
			t.Fatalf("deployment token list carried secret material %q: %s", forbidden, ownerListed.Body.String())
		}
	}

	ordinary := call(http.MethodPost, "/v1/tokens", legacy, `{"label":"operator laptop"}`)
	if ordinary.Code != http.StatusCreated {
		t.Fatalf("ordinary operator token status=%d body=%s", ordinary.Code, ordinary.Body.String())
	}
	ownerListed = call(http.MethodGet, "/v1/tokens", legacy, "")
	if err := json.Unmarshal(ownerListed.Body.Bytes(), &ownerTokens); err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, item := range ownerTokens {
		if item.DeploymentCredential {
			marked++
			if item.ID != deploymentTokenID {
				t.Fatalf("deployment marker moved to ordinary token: %+v", ownerTokens)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("deployment marker count=%d tokens=%+v", marked, ownerTokens)
	}

	spoofed := call(http.MethodPost, "/v1/tokens", legacy, `{"label":"legacy API token"}`)
	if spoofed.Code != http.StatusCreated {
		t.Fatalf("spoof-label issue status=%d body=%s", spoofed.Code, spoofed.Body.String())
	}
	var spoofedToken core.IssuedPersonalAccessToken
	if err := json.Unmarshal(spoofed.Body.Bytes(), &spoofedToken); err != nil {
		t.Fatal(err)
	}
	if spoofedToken.DeploymentCredential {
		t.Fatalf("spoof-label issuance received deployment marker: %+v", spoofedToken)
	}
	ownerListed = call(http.MethodGet, "/v1/tokens", spoofedToken.Value, "")
	if err := json.Unmarshal(ownerListed.Body.Bytes(), &ownerTokens); err != nil {
		t.Fatal(err)
	}
	marked = 0
	for _, item := range ownerTokens {
		if item.DeploymentCredential {
			marked++
			if item.ID != deploymentTokenID {
				t.Fatalf("spoof label moved deployment marker: %+v", ownerTokens)
			}
		}
		if item.ID == spoofedToken.ID && item.DeploymentCredential {
			t.Fatalf("spoofed token listed as deployment credential: %+v", item)
		}
	}
	if marked != 1 {
		t.Fatalf("deployment marker count after label spoof=%d tokens=%+v", marked, ownerTokens)
	}

	// Existing bootstrap rotation re-maps the credential without changing list
	// secrecy; the marker follows the row that now authenticates the env value.
	rotatedLegacy := "self-service-rotated-legacy-token"
	if rotated, rotateErr := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{
		OrganizationName: "Self Service Org", Email: "owner@example.test", DisplayName: "Owner",
	}, rotatedLegacy); rotateErr != nil || !rotated {
		t.Fatalf("rotate deployment credential rotated=%t err=%v", rotated, rotateErr)
	}
	if stale := call(http.MethodGet, "/v1/tokens", legacy, ""); stale.Code != http.StatusUnauthorized {
		t.Fatalf("pre-rotation env token status=%d body=%s", stale.Code, stale.Body.String())
	}
	if inert := call(http.MethodGet, "/v1/tokens", spoofedToken.Value, ""); inert.Code != http.StatusOK {
		t.Fatalf("spoof-label token was altered by bootstrap rotation: status=%d body=%s", inert.Code, inert.Body.String())
	}
	ownerListed = call(http.MethodGet, "/v1/tokens", rotatedLegacy, "")
	if err := json.Unmarshal(ownerListed.Body.Bytes(), &ownerTokens); err != nil {
		t.Fatal(err)
	}
	marked = 0
	for _, item := range ownerTokens {
		if item.DeploymentCredential {
			marked++
			if item.ID != deploymentTokenID {
				t.Fatalf("rotated deployment marker token=%+v want id=%s", item, deploymentTokenID)
			}
		}
	}
	if marked != 1 || strings.Contains(ownerListed.Body.String(), rotatedLegacy) {
		t.Fatalf("rotated deployment token list=%s", ownerListed.Body.String())
	}

	created := call(http.MethodPost, "/v1/tokens", aliceSeed.Value, `{"label":"laptop"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", created.Code, created.Body.String())
	}
	var issued core.IssuedPersonalAccessToken
	if err := json.Unmarshal(created.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Value == "" || issued.UserID != alice.ID {
		t.Fatalf("issued token=%+v", issued)
	}
	var storedValues int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_tokens WHERE id=$1 AND encode(token_hash,'escape')=$2`, issued.ID, issued.Value).Scan(&storedValues); err != nil {
		t.Fatal(err)
	}
	if storedValues != 0 {
		t.Fatal("issued token value was persisted in cleartext")
	}
	var issueEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events
		WHERE kind='identity.personal_token_issued'
		  AND actor_id=$1 AND actor_role='human'
		  AND payload_json=jsonb_build_object('credential_id',$2::text,'label','laptop')
		  AND payload_json::text NOT LIKE $3`, store.UserActorID(alice.ID), issued.ID, "%"+issued.Value+"%").Scan(&issueEvents); err != nil || issueEvents != 1 {
		t.Fatalf("personal-token issue audit count=%d err=%v", issueEvents, err)
	}

	// The new credential authenticates, and listing never reproduces its value.
	listed := call(http.MethodGet, "/v1/tokens", issued.Value, "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), issued.Value) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var aliceTokens []core.PersonalAccessToken
	if err := json.Unmarshal(listed.Body.Bytes(), &aliceTokens); err != nil {
		t.Fatal(err)
	}
	if len(aliceTokens) != 2 {
		t.Fatalf("alice tokens=%+v", aliceTokens)
	}
	for _, item := range aliceTokens {
		if item.UserID != alice.ID || item.RevokedAt != nil {
			t.Fatalf("alice token=%+v", item)
		}
	}

	// Bob sees only his own, and cannot revoke one of Alice's.
	bobListed := call(http.MethodGet, "/v1/tokens", bobSeed.Value, "")
	if bobListed.Code != http.StatusOK || strings.Contains(bobListed.Body.String(), issued.ID) {
		t.Fatalf("bob list status=%d body=%s", bobListed.Code, bobListed.Body.String())
	}
	var bobTokens []core.PersonalAccessToken
	if err := json.Unmarshal(bobListed.Body.Bytes(), &bobTokens); err != nil {
		t.Fatal(err)
	}
	for _, item := range bobTokens {
		if item.DeploymentCredential {
			t.Fatalf("deployment marker leaked to another owner: %+v", bobTokens)
		}
	}
	if crossUser := call(http.MethodDelete, "/v1/tokens/"+issued.ID, bobSeed.Value, ""); crossUser.Code != http.StatusNotFound {
		t.Fatalf("cross-user revocation status=%d body=%s", crossUser.Code, crossUser.Body.String())
	}
	if survived := call(http.MethodGet, "/v1/tokens", issued.Value, ""); survived.Code != http.StatusOK {
		t.Fatalf("alice token stopped working after another user's revocation attempt: status=%d", survived.Code)
	}

	// Self-revocation fails the credential closed and is still listed as revoked.
	if revoked := call(http.MethodDelete, "/v1/tokens/"+issued.ID, aliceSeed.Value, ""); revoked.Code != http.StatusNoContent {
		t.Fatalf("self revocation status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if closed := call(http.MethodGet, "/v1/tokens", issued.Value, ""); closed.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential status=%d body=%s", closed.Code, closed.Body.String())
	}
	if _, err := st.VerifyCredential(t.Context(), issued.Value); !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("revoked credential verification err=%v", err)
	}
	after := call(http.MethodGet, "/v1/tokens", aliceSeed.Value, "")
	if err := json.Unmarshal(after.Body.Bytes(), &aliceTokens); err != nil {
		t.Fatal(err)
	}
	var revokedRows int
	for _, item := range aliceTokens {
		if item.ID == issued.ID && item.RevokedAt != nil {
			revokedRows++
		}
	}
	if revokedRows != 1 || strings.Contains(after.Body.String(), issued.Value) {
		t.Fatalf("post-revocation list=%s", after.Body.String())
	}
	var revokeEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events
		WHERE kind='identity.personal_token_revoked'
		  AND actor_id=$1 AND actor_role='human'
		  AND payload_json=jsonb_build_object('credential_id',$2::text,'label','laptop')
		  AND payload_json::text NOT LIKE $3`, store.UserActorID(alice.ID), issued.ID, "%"+issued.Value+"%").Scan(&revokeEvents); err != nil || revokeEvents != 1 {
		t.Fatalf("personal-token revoke audit count=%d err=%v", revokeEvents, err)
	}
	for _, statement := range []string{
		`UPDATE deployment_events SET actor_id='tampered' WHERE payload_json->>'credential_id'=$1`,
		`DELETE FROM deployment_events WHERE payload_json->>'credential_id'=$1`,
	} {
		if _, err := st.pool.Exec(t.Context(), statement, issued.ID); err == nil {
			t.Fatalf("deployment token audit accepted append-only mutation: %s", statement)
		}
	}

	// Agent credentials share the user_tokens table but are not human
	// credentials: they can neither enumerate nor mint their owner's tokens.
	agent, err := st.IssueAgentCredential(t.Context(), alice.ID, "execution")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range [][3]string{
		{http.MethodGet, "/v1/tokens", ""},
		{http.MethodPost, "/v1/tokens", `{"label":"smuggled"}`},
		{http.MethodDelete, "/v1/tokens/" + aliceSeed.ID, ""},
	} {
		if response := call(route[0], route[1], agent.Value, route[2]); response.Code != http.StatusUnauthorized {
			t.Fatalf("agent credential %s %s status=%d body=%s", route[0], route[1], response.Code, response.Body.String())
		}
	}
}

func newIdentityIntegrationStore(t *testing.T, maxVersion int) *Store {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "identity_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrateControlPlaneToVersion(t.Context(), pool, maxVersion); err != nil {
		t.Fatalf("migrate identity fixture to %d: %v", maxVersion, err)
	}
	if maxVersion == 0 {
		if migrateErr := riverqueue.Migrate(t.Context(), pool); migrateErr != nil {
			t.Fatal(migrateErr)
		}
	}
	queue, err := newRiverDispatchQueue(pool)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{pool: pool, queries: db.New(pool), queue: queue}
}
