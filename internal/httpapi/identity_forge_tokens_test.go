package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type forgeTokenFixture struct {
	status                               core.ForgeTokenStatus
	token                                string
	storeCalls, statusCalls, deleteCalls int
}

func (f *forgeTokenFixture) StoreForgeToken(_ context.Context, _ string, token, login string) (core.ForgeTokenStatus, error) {
	f.storeCalls++
	f.token = token
	f.status = core.ForgeTokenStatus{Configured: true, ForgeLogin: login, StoredAt: time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC)}
	return f.status, nil
}
func (f *forgeTokenFixture) DeleteForgeToken(context.Context, string) error {
	f.deleteCalls++
	f.status = core.ForgeTokenStatus{}
	f.token = ""
	return nil
}
func (f *forgeTokenFixture) GetForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error) {
	f.statusCalls++
	return f.status, nil
}
func (f *forgeTokenFixture) GetForgeTokenForUse(context.Context, string) (core.ForgeTokenCredential, error) {
	return core.ForgeTokenCredential{}, nil
}
func (f *forgeTokenFixture) ListForgeTokensForRedaction(context.Context) ([]string, error) {
	if f.token == "" {
		return nil, nil
	}
	return []string{f.token}, nil
}

func TestForgeTokenSelfServiceLifecycleAndCredentialClasses(t *testing.T) {
	fixture := &forgeTokenFixture{}
	server := NewServer(store.NewMemory())
	server.ForgeTokens = fixture
	server.Credentials = staticCredentialVerifier{
		"human": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent": {ID: "agt", OwnerUserID: "usr", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	server.ValidateForgeToken = func(_ context.Context, token string) (string, error) {
		if token == "valid-forge-secret" {
			return "octocat", nil
		}
		return "", errors.New("child detail containing " + token)
	}
	call := func(method, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/forge-token", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if got := call(method, "agent", `{"token":"valid-forge-secret"}`); got.Code != http.StatusUnauthorized || fixture.storeCalls+fixture.statusCalls+fixture.deleteCalls != 0 {
			t.Fatalf("agent %s status=%d calls=%d", method, got.Code, fixture.storeCalls+fixture.statusCalls+fixture.deleteCalls)
		}
		if got := call(method, "worker", `{"token":"valid-forge-secret"}`); got.Code != http.StatusUnauthorized || fixture.storeCalls+fixture.statusCalls+fixture.deleteCalls != 0 {
			t.Fatalf("worker %s status=%d calls=%d", method, got.Code, fixture.storeCalls+fixture.statusCalls+fixture.deleteCalls)
		}
	}
	if got := call(http.MethodPut, "human", `{"token":"invalid-forge-secret"}`); got.Code != http.StatusUnprocessableEntity || got.Body.String() != forgeTokenValidationFailure+"\n" || fixture.storeCalls != 0 || strings.Contains(got.Body.String(), "invalid-forge-secret") {
		t.Fatalf("invalid status=%d body=%q stores=%d", got.Code, got.Body.String(), fixture.storeCalls)
	}
	put := call(http.MethodPut, "human", `{"token":"valid-forge-secret"}`)
	if put.Code != http.StatusOK || strings.Contains(put.Body.String(), "valid-forge-secret") || !strings.Contains(put.Body.String(), `"forge_login":"octocat"`) {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	get := call(http.MethodGet, "human", "")
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "valid-forge-secret") || !strings.Contains(get.Body.String(), `"configured":true`) {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
	if got := call(http.MethodDelete, "human", ""); got.Code != http.StatusNoContent || got.Body.Len() != 0 {
		t.Fatalf("delete status=%d body=%q", got.Code, got.Body.String())
	}
	if got := call(http.MethodDelete, "human", ""); got.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete status=%d", got.Code)
	}
}

func TestForgeTokenPutStrictJSON(t *testing.T) {
	fixture := &forgeTokenFixture{}
	server := NewServer(store.NewMemory())
	server.ForgeTokens = fixture
	server.Credentials = staticCredentialVerifier{"human": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser}}
	server.ValidateForgeToken = func(context.Context, string) (string, error) { return "octocat", nil }
	for _, body := range []string{`{}`, `{"token":"x","extra":true}`, `{"token":"x"} {}`} {
		r := httptest.NewRequest(http.MethodPut, "/v1/forge-token", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer human")
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnprocessableEntity || fixture.storeCalls != 0 {
			t.Fatalf("body=%q status=%d stores=%d", body, w.Code, fixture.storeCalls)
		}
	}
}
