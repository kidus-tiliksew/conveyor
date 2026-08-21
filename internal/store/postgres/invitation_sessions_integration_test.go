package postgres

import (
	"context"
	"encoding/json"
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
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestInvitationLinkSessionAndFirstPATIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "invitation-owner-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Invite Org", Email: "owner@example.test", DisplayName: "Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator, Method: core.CredentialMethodBearer}
	ctx := store.WithActor(store.WithCredential(t.Context(), credential), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	workspace := "signin-" + core.NewTaskID()
	if _, err = st.CreateWorkspace(ctx, workspace, workspace, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}

	server := httpapi.NewServer(st)
	server.Workspaces = st
	server.Workspace = workspace
	server.InvitationDelivery = config.InvitationDelivery{PublicURL: "https://conveyor.example"}
	provider := func(context.Context) (*config.Config, error) { return isolationConfig(workspace), nil }
	server.ConfigProvider = provider
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders, ConfigProvider: provider}
	handler := server.Handler()
	call := func(method, path, bearer, body string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.Header.Set("X-Conveyor-CSRF", "1")
			r.Header.Set("Origin", "https://conveyor.example")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	email := "new-user@example.test"
	invite := call(http.MethodPost, "/v1/workspaces/"+workspace+"/members", legacy, `{"email":"`+email+`","role":"operator"}`, nil, false)
	if invite.Code != http.StatusCreated {
		t.Fatalf("invite status=%d body=%s", invite.Code, invite.Body.String())
	}
	var grant core.MembershipGrant
	if err = json.Unmarshal(invite.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.SignInURL == "" || grant.Delivery != "fallback" {
		t.Fatalf("grant=%+v", grant)
	}
	token := strings.TrimPrefix(grant.SignInURL, "https://conveyor.example/sign-in#token=")
	if _, err = st.pool.Exec(t.Context(), `UPDATE invitation_signin_tokens SET expires_at=now()-interval '1 second' WHERE email=$1 AND redeemed_at IS NULL`, email); err != nil {
		t.Fatal(err)
	}
	if expired := call(http.MethodPost, "/v1/sign-in/redeem", "", `{"token":"`+token+`"}`, nil, false); expired.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d", expired.Code)
	}
	resend := call(http.MethodPost, "/v1/workspaces/"+workspace+"/invitations/"+email+"/resend", legacy, "", nil, false)
	if resend.Code != http.StatusOK {
		t.Fatalf("resend status=%d body=%s", resend.Code, resend.Body.String())
	}
	if err = json.Unmarshal(resend.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	token = strings.TrimPrefix(grant.SignInURL, "https://conveyor.example/sign-in#token=")
	redeem := call(http.MethodPost, "/v1/sign-in/redeem", "", `{"token":"`+token+`"}`, nil, false)
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}
	cookies := redeem.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "conveyor_session" || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookies=%+v", cookies)
	}
	if repeated := call(http.MethodPost, "/v1/sign-in/redeem", "", `{"token":"`+token+`"}`, nil, false); repeated.Code != http.StatusUnauthorized {
		t.Fatalf("repeat status=%d", repeated.Code)
	}
	workspaceMutation := `{"email":"second-user@example.test","role":"contributor"}`
	if refused := call(http.MethodPost, "/v1/workspaces/"+workspace+"/members", "", workspaceMutation, cookies[0], false); refused.Code != http.StatusForbidden {
		t.Fatalf("workspace mutation without CSRF status=%d body=%s", refused.Code, refused.Body.String())
	}
	if accepted := call(http.MethodPost, "/v1/workspaces/"+workspace+"/members", "", workspaceMutation, cookies[0], true); accepted.Code != http.StatusCreated {
		t.Fatalf("workspace mutation with CSRF status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if refused := call(http.MethodPost, "/v1/tokens", "", `{"label":"first"}`, cookies[0], false); refused.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", refused.Code, refused.Body.String())
	}
	issued := call(http.MethodPost, "/v1/tokens", "", `{"label":"first"}`, cookies[0], true)
	if issued.Code != http.StatusCreated {
		t.Fatalf("PAT issue status=%d body=%s", issued.Code, issued.Body.String())
	}
	var pat core.IssuedPersonalAccessToken
	if err = json.Unmarshal(issued.Body.Bytes(), &pat); err != nil {
		t.Fatal(err)
	}
	if pat.Value == "" {
		t.Fatal("first PAT missing")
	}
	now := time.Now().UTC()
	runCtx := store.WithWorkspace(ctx, workspace)
	runTask := core.Task{ID: "session-pat-run-" + core.NewTaskID(), Workspace: workspace, Repo: "repo", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	runTask.Branch = "conveyor/task-" + runTask.ID
	if err = st.CreateTask(runCtx, runTask); err != nil {
		t.Fatal(err)
	}
	runJob := core.Job{ID: runTask.ID + "-implement-1", TaskID: runTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(runCtx, runJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(runCtx, core.WorkOrder{ID: runJob.ID, TaskID: runTask.ID, JobID: runJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	runBase := "/v1/tasks/" + runTask.ID
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, runBase + "/run-order", ""},
		{http.MethodPost, runBase + "/run-orders/" + runJob.ID + "/claim", `{}`},
		{http.MethodPost, runBase + "/run-orders/" + runJob.ID + "/renew", `{}`},
		{http.MethodGet, runBase + "/run-orders/" + runJob.ID + "/reconcile", ""},
		{http.MethodPost, runBase + "/run-orders/" + runJob.ID + "/attempt-checkpoint", `{}`},
		{http.MethodPost, runBase + "/run-orders/" + runJob.ID + "/release", `{}`},
	} {
		refused := call(route.method, route.path, "", route.body, cookies[0], true)
		if refused.Code != http.StatusUnauthorized || refused.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("session reached run route %s %s: status=%d body=%s", route.method, route.path, refused.Code, refused.Body.String())
		}
	}
	if response := call(http.MethodGet, runBase+"/run-order", pat.Value, "", nil, false); response.Code != http.StatusOK {
		t.Fatalf("PAT run-order status=%d body=%s", response.Code, response.Body.String())
	}
	claim := call(http.MethodPost, runBase+"/run-orders/"+runJob.ID+"/claim", pat.Value, `{"session_id":"pat-run-session","client_token":"pat-run-secret","agent":"codex","model":"gpt"}`, nil, false)
	if claim.Code != http.StatusOK {
		t.Fatalf("PAT claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed core.WorkOrder
	if err = json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodPost, runBase+"/run-orders/"+runJob.ID+"/renew", pat.Value, `{"session_id":"pat-run-session"}`, nil, false); response.Code != http.StatusOK {
		t.Fatalf("PAT renew status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodGet, runBase+"/run-orders/"+runJob.ID+"/reconcile?session_id=pat-run-session", pat.Value, "", nil, false); response.Code != http.StatusOK {
		t.Fatalf("PAT reconcile status=%d body=%s", response.Code, response.Body.String())
	}
	checkpoint := `{"session_id":"pat-run-session","attempt_id":"` + claimed.AttemptID + `","termination_reason":"integration preservation","commit_sha":"1111111111111111111111111111111111111111","push_result":"pushed"}`
	if response := call(http.MethodPost, runBase+"/run-orders/"+runJob.ID+"/attempt-checkpoint", pat.Value, checkpoint, nil, false); response.Code != http.StatusOK {
		t.Fatalf("PAT checkpoint status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPost, runBase+"/run-orders/"+runJob.ID+"/release", pat.Value, `{"session_id":"pat-run-session","reason":"integration complete","outcome":"released"}`, nil, false); response.Code != http.StatusOK {
		t.Fatalf("PAT release status=%d body=%s", response.Code, response.Body.String())
	}
	if me := call(http.MethodGet, "/v1/me", pat.Value, "", nil, false); me.Code != http.StatusOK {
		t.Fatalf("PAT /me status=%d body=%s", me.Code, me.Body.String())
	}
	if mcp := call(http.MethodPost, "/mcp", "", "{}", cookies[0], true); mcp.Code != http.StatusUnauthorized {
		t.Fatalf("session reached MCP status=%d", mcp.Code)
	}
	if signout := call(http.MethodPost, "/v1/sign-out", "", "", cookies[0], true); signout.Code != http.StatusNoContent {
		t.Fatalf("signout status=%d body=%s", signout.Code, signout.Body.String())
	}
	if after := call(http.MethodGet, "/v1/tokens", "", "", cookies[0], false); after.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d", after.Code)
	}
	link, err := st.IssueSignInLink(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	session, identity, err := st.RedeemSignInLink(t.Context(), link.Value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyDashboardSession(t.Context(), session.Value); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeactivateIdentityUser(ctx, identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyDashboardSession(t.Context(), session.Value); err == nil {
		t.Fatal("deactivated user retained a live session")
	}
	var persistedSecrets int
	if err = st.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM invitation_signin_tokens WHERE encode(token_hash,'escape')=$1) +
		(SELECT count(*) FROM dashboard_sessions WHERE encode(session_hash,'escape')=$2)`, token, cookies[0].Value).Scan(&persistedSecrets); err != nil {
		t.Fatal(err)
	}
	if persistedSecrets != 0 {
		t.Fatal("cleartext sign-in or session secret persisted")
	}
	var lifecycleEvents int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_events
		WHERE kind IN ('identity.signin_link_issued','identity.signin_link_redeemed','identity.dashboard_session_created','identity.dashboard_session_revoked','identity.invitation_delivery_fallback')
		AND payload_json::text NOT LIKE '%cv_signin_%' AND payload_json::text NOT LIKE '%cv_session_%'`).Scan(&lifecycleEvents); err != nil {
		t.Fatal(err)
	}
	if lifecycleEvents < 7 {
		t.Fatalf("audited lifecycle events=%d, want at least 7", lifecycleEvents)
	}
}

func TestPasswordSignInResetAndSlidingSessionIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	legacy := "password-owner-token"
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Password Org", Email: "password-owner@example.test", DisplayName: "Password Owner"}, legacy); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	credential := core.AuthenticatedCredential{ID: "legacy", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator, Method: core.CredentialMethodBearer}
	ctx := store.WithActor(store.WithCredential(t.Context(), credential), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = st.ProvisionIdentityUser(ctx, "passwordless@example.test", "Passwordless"); err != nil {
		t.Fatal(err)
	}

	server := httpapi.NewServer(st)
	server.InvitationDelivery = config.InvitationDelivery{PublicURL: "https://conveyor.example"}
	handler := server.Handler()
	call := func(method, path, body, source string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.RemoteAddr = source + ":4321"
		request.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if csrf {
			request.Header.Set("X-Conveyor-CSRF", "1")
			request.Header.Set("Origin", "https://conveyor.example")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	link, err := st.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	redeem := call(http.MethodPost, "/v1/sign-in/redeem", `{"token":"`+link.Value+`"}`, "192.0.2.1", nil, false)
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}
	linkCookie := redeem.Result().Cookies()[0]
	if set := call(http.MethodPost, "/v1/password", `{"current_password":"","new_password":"first-password-value"}`, "192.0.2.2", linkCookie, true); set.Code != http.StatusNoContent {
		t.Fatalf("first password status=%d body=%s", set.Code, set.Body.String())
	}

	passwordSignIn := call(http.MethodPost, "/v1/sign-in/password", `{"email":"PASSWORD-OWNER@example.test","password":"first-password-value"}`, "192.0.2.3", nil, true)
	if passwordSignIn.Code != http.StatusOK {
		t.Fatalf("password sign-in status=%d body=%s", passwordSignIn.Code, passwordSignIn.Body.String())
	}
	passwordCookie := passwordSignIn.Result().Cookies()[0]

	refusals := []*httptest.ResponseRecorder{
		call(http.MethodPost, "/v1/sign-in/password", `{"email":"missing@example.test","password":"wrong-password-value"}`, "192.0.2.4", nil, true),
		call(http.MethodPost, "/v1/sign-in/password", `{"email":"passwordless@example.test","password":"wrong-password-value"}`, "192.0.2.5", nil, true),
		call(http.MethodPost, "/v1/sign-in/password", `{"email":"password-owner@example.test","password":"wrong-password-value"}`, "192.0.2.6", nil, true),
	}
	for index, refusal := range refusals {
		if refusal.Code != http.StatusUnauthorized || refusal.Body.String() != refusals[0].Body.String() {
			t.Fatalf("refusal %d status=%d body=%q first=%q", index, refusal.Code, refusal.Body.String(), refusals[0].Body.String())
		}
	}

	if changed := call(http.MethodPost, "/v1/password", `{"current_password":"wrong-password-value","new_password":"second-password-value"}`, "192.0.2.7", passwordCookie, true); changed.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password status=%d body=%s", changed.Code, changed.Body.String())
	}
	if changed := call(http.MethodPost, "/v1/password", `{"current_password":"first-password-value","new_password":"second-password-value"}`, "192.0.2.8", passwordCookie, true); changed.Code != http.StatusNoContent {
		t.Fatalf("change password status=%d body=%s", changed.Code, changed.Body.String())
	}

	resetLink, err := st.IssueSignInLink(ctx, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	resetRedeem := call(http.MethodPost, "/v1/sign-in/redeem", `{"token":"`+resetLink.Value+`"}`, "192.0.2.9", nil, false)
	if resetRedeem.Code != http.StatusOK {
		t.Fatalf("reset redeem status=%d body=%s", resetRedeem.Code, resetRedeem.Body.String())
	}
	if reset := call(http.MethodPost, "/v1/password", `{"current_password":"","new_password":"reset-password-value"}`, "192.0.2.10", resetRedeem.Result().Cookies()[0], true); reset.Code != http.StatusNoContent {
		t.Fatalf("link reset status=%d body=%s", reset.Code, reset.Body.String())
	}

	session, _, err := st.SignInWithPassword(t.Context(), owner.Email, "reset-password-value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(t.Context(), `UPDATE dashboard_sessions SET expires_at=now()+interval '1 hour' WHERE id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}
	verified, err := st.VerifyDashboardSession(t.Context(), session.Value)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(verified.SessionExpiresAt) < 6*24*time.Hour+23*time.Hour {
		t.Fatalf("session was not renewed through the seven-day window: %s", verified.SessionExpiresAt)
	}
	if err = st.RevokeDashboardSession(t.Context(), owner.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyDashboardSession(t.Context(), session.Value); err == nil {
		t.Fatal("revoked session was renewed")
	}

	deactivatedSession, _, err := st.SignInWithPassword(t.Context(), owner.Email, "reset-password-value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeactivateIdentityUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyDashboardSession(t.Context(), deactivatedSession.Value); err == nil {
		t.Fatal("deactivated user retained password session")
	}
	if _, _, err = st.SignInWithPassword(t.Context(), owner.Email, "reset-password-value"); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("deactivated password sign-in err=%v", err)
	}

	var cleartextHashes int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE password_hash IN ('first-password-value','second-password-value','reset-password-value')`).Scan(&cleartextHashes); err != nil {
		t.Fatal(err)
	}
	if cleartextHashes != 0 {
		t.Fatal("cleartext password persisted")
	}
}
