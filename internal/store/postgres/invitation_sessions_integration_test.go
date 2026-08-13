package postgres

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	invite := call(http.MethodPost, "/v1/workspaces/"+workspace+"/members", legacy, `{"email":"`+email+`","role":"user"}`, nil, false)
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
	token := strings.TrimPrefix(grant.SignInURL, "https://conveyor.example/sign-in?token=")
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
	token = strings.TrimPrefix(grant.SignInURL, "https://conveyor.example/sign-in?token=")
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
