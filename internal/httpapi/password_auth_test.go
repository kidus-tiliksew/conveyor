package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestPasswordSignInRefusalsAndRateLimit(t *testing.T) {
	server := NewServer(nil)
	server.InvitationSessions = &invitationSessionFixture{}
	server.InvitationDelivery.PublicURL = "https://conveyor.example"
	handler := server.Handler()
	call := func(email, source string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/sign-in/password", strings.NewReader(`{"email":"`+email+`","password":"not-the-password"}`))
		request.RemoteAddr = source + ":4321"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Conveyor-CSRF", "1")
		request.Header.Set("Origin", "https://conveyor.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	unknown := call("unknown@example.test", "192.0.2.1")
	passwordless := call("passwordless@example.test", "192.0.2.2")
	wrong := call("known@example.test", "192.0.2.3")
	if unknown.Code != http.StatusUnauthorized || passwordless.Code != unknown.Code || wrong.Code != unknown.Code || unknown.Body.String() != passwordless.Body.String() || unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("refusals differ: unknown=(%d,%q) passwordless=(%d,%q) wrong=(%d,%q)", unknown.Code, unknown.Body.String(), passwordless.Code, passwordless.Body.String(), wrong.Code, wrong.Body.String())
	}
	for attempt := 1; attempt <= passwordAttemptBudget; attempt++ {
		response := call("limited@example.test", "198.51.100."+string(rune('0'+attempt)))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d", attempt, response.Code)
		}
	}
	if response := call("limited@example.test", "198.51.100.99"); response.Code != http.StatusTooManyRequests || response.Body.String() != passwordSignInRefusal+"\n" {
		t.Fatalf("limited status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestPasswordLimiterRecoversAndStaysBounded(t *testing.T) {
	limiter := newPasswordAttemptLimiter()
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	for attempt := 0; attempt < passwordAttemptBudget; attempt++ {
		if !limiter.allow("account") {
			t.Fatalf("attempt %d refused early", attempt+1)
		}
	}
	if limiter.allow("account") {
		t.Fatal("budget did not trip")
	}
	now = now.Add(passwordAttemptWindow)
	if !limiter.allow("account") {
		t.Fatal("budget did not recover")
	}
	for index := 0; index < passwordLimiterMaxKeys+20; index++ {
		limiter.allow(string(rune(index + 1)))
	}
	if len(limiter.windows) > passwordLimiterMaxKeys {
		t.Fatalf("limiter keys=%d", len(limiter.windows))
	}
}

func TestSessionVerificationRefreshesCookie(t *testing.T) {
	expires := time.Now().Add(7 * 24 * time.Hour).UTC()
	server := NewServer(nil)
	server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{ID: "ses_1", OwnerUserID: "usr_1", Kind: core.CredentialUser, Scope: core.CredentialScopeUser, Method: core.CredentialMethodSession, SessionExpiresAt: expires}}
	request := httptest.NewRequest(http.MethodGet, "/v1/tokens", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "cv_session_secret"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	cookies := response.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != dashboardSessionCookie || cookies[0].Value != "cv_session_secret" || cookies[0].Expires.Sub(expires).Abs() >= time.Second {
		t.Fatalf("refreshed cookies=%+v", cookies)
	}
}
