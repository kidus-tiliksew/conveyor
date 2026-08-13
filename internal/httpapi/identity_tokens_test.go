package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// personalTokenFixture records the owner every call was scoped to. The handlers
// pass no other subject, so a recorded owner that is not the caller would mean
// the self-service boundary leaked.
type personalTokenFixture struct {
	tokens     map[string][]core.PersonalAccessToken
	listOwners []string
	issued     []string
	revoked    [][2]string
	listErr    error
	issueErr   error
}

func (f *personalTokenFixture) ListOwnPersonalAccessTokens(_ context.Context, userID string) ([]core.PersonalAccessToken, error) {
	f.listOwners = append(f.listOwners, userID)
	return f.tokens[userID], f.listErr
}

func (f *personalTokenFixture) IssueOwnPersonalAccessToken(_ context.Context, userID, label string) (core.IssuedPersonalAccessToken, error) {
	f.issued = append(f.issued, userID+"/"+label)
	if f.issueErr != nil {
		return core.IssuedPersonalAccessToken{}, f.issueErr
	}
	issued := core.IssuedPersonalAccessToken{
		PersonalAccessToken: core.PersonalAccessToken{ID: "pat_new", UserID: userID, Label: label, CreatedAt: time.Unix(0, 0).UTC()},
		Value:               "cv_pat_pat_new_secret",
	}
	f.tokens[userID] = append(f.tokens[userID], issued.PersonalAccessToken)
	return issued, nil
}

func (f *personalTokenFixture) RevokeOwnPersonalAccessToken(_ context.Context, userID, tokenID string) (core.PersonalAccessToken, error) {
	f.revoked = append(f.revoked, [2]string{userID, tokenID})
	for _, item := range f.tokens[userID] {
		if item.ID == tokenID {
			revoked := time.Unix(0, 0).UTC()
			item.RevokedAt = &revoked
			return item, nil
		}
	}
	return core.PersonalAccessToken{}, store.ErrNotFound
}

func newPersonalTokenServer(tokens *personalTokenFixture) *Server {
	server := NewServer(store.NewMemory())
	server.PersonalTokens = tokens
	server.Credentials = staticCredentialVerifier{
		"owner-token":  {ID: "pat_owner", OwnerUserID: "owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"other-token":  {ID: "pat_other", OwnerUserID: "other", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-token":  {ID: "agt_owner", OwnerUserID: "owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
		"legacy-token": {ID: "pat_legacy", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	return server
}

func personalTokenCall(t *testing.T, server *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestPersonalAccessTokenIssuanceReturnsTheSecretOnlyOnce(t *testing.T) {
	tokens := &personalTokenFixture{tokens: map[string][]core.PersonalAccessToken{}}
	server := newPersonalTokenServer(tokens)

	issued := personalTokenCall(t, server, http.MethodPost, "/v1/tokens", "owner-token", `{"label":"laptop"}`)
	if issued.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", issued.Code, issued.Body.String())
	}
	var created core.IssuedPersonalAccessToken
	if err := json.Unmarshal(issued.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode issued token: %v", err)
	}
	if created.Value == "" || created.Label != "laptop" {
		t.Fatalf("issued token=%+v", created)
	}

	listed := personalTokenCall(t, server, http.MethodGet, "/v1/tokens", "owner-token", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), created.Value) || strings.Contains(listed.Body.String(), "value") {
		t.Fatalf("list response carried a token value: %s", listed.Body.String())
	}
	var items []core.PersonalAccessToken
	if err := json.Unmarshal(listed.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	if len(items) != 1 || items[0].Label != "laptop" {
		t.Fatalf("token list=%+v", items)
	}
}

func TestPersonalAccessTokenRoutesScopeEveryCallToTheCredentialOwner(t *testing.T) {
	tokens := &personalTokenFixture{tokens: map[string][]core.PersonalAccessToken{
		"owner": {{ID: "pat_owner_one", UserID: "owner", Label: "laptop"}},
		"other": {{ID: "pat_other_one", UserID: "other", Label: "desktop"}},
	}}
	server := newPersonalTokenServer(tokens)

	// A body that names another subject is refused at decode: the request shape
	// has no field for one, so it cannot be smuggled past the credential.
	if response := personalTokenCall(t, server, http.MethodPost, "/v1/tokens", "owner-token", `{"label":"x","user_id":"other"}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("subject-carrying body status=%d body=%s", response.Code, response.Body.String())
	}

	if response := personalTokenCall(t, server, http.MethodGet, "/v1/tokens", "other-token", ""); response.Code != http.StatusOK {
		t.Fatalf("other list status=%d", response.Code)
	}
	if len(tokens.listOwners) != 1 || tokens.listOwners[0] != "other" {
		t.Fatalf("list owners=%v", tokens.listOwners)
	}

	// "other" cannot revoke a token belonging to "owner": the owner recorded is
	// always the caller, so the store sees a token id that is not theirs.
	crossUser := personalTokenCall(t, server, http.MethodDelete, "/v1/tokens/pat_owner_one", "other-token", "")
	if crossUser.Code != http.StatusNotFound {
		t.Fatalf("cross-user revocation status=%d body=%s", crossUser.Code, crossUser.Body.String())
	}
	if len(tokens.revoked) != 1 || tokens.revoked[0] != [2]string{"other", "pat_owner_one"} {
		t.Fatalf("revocation calls=%v", tokens.revoked)
	}
	if own := personalTokenCall(t, server, http.MethodDelete, "/v1/tokens/pat_owner_one", "owner-token", ""); own.Code != http.StatusNoContent {
		t.Fatalf("own revocation status=%d body=%s", own.Code, own.Body.String())
	}
}

func TestPersonalAccessTokenRoutesRefuseNonHumanAndUnauthenticatedCallers(t *testing.T) {
	tokens := &personalTokenFixture{tokens: map[string][]core.PersonalAccessToken{}}
	server := newPersonalTokenServer(tokens)
	for _, bearer := range []string{"", "agent-token", "unknown-token"} {
		for _, route := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/v1/tokens", ""},
			{http.MethodPost, "/v1/tokens", `{"label":"laptop"}`},
			{http.MethodDelete, "/v1/tokens/pat_owner_one", ""},
		} {
			response := personalTokenCall(t, server, route.method, route.path, bearer, route.body)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s bearer=%q status=%d body=%s", route.method, route.path, bearer, response.Code, response.Body.String())
			}
		}
	}
	if len(tokens.listOwners)+len(tokens.issued)+len(tokens.revoked) != 0 {
		t.Fatalf("store reached without a human credential: %v %v %v", tokens.listOwners, tokens.issued, tokens.revoked)
	}
}

func TestPersonalAccessTokenIssuanceValidatesLabelAndHidesStoreFailures(t *testing.T) {
	tokens := &personalTokenFixture{tokens: map[string][]core.PersonalAccessToken{}}
	server := newPersonalTokenServer(tokens)
	for _, body := range []string{`{"label":""}`, `{"label":"   "}`, `{"label":"` + strings.Repeat("a", maxTokenLabelLength+1) + `"}`} {
		if response := personalTokenCall(t, server, http.MethodPost, "/v1/tokens", "owner-token", body); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("label %q status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
	if len(tokens.issued) != 0 {
		t.Fatalf("invalid labels reached the store: %v", tokens.issued)
	}

	rawError := "credential database exploded with secret detail"
	tokens.issueErr = errors.New(rawError)
	response := personalTokenCall(t, server, http.MethodPost, "/v1/tokens", "owner-token", `{"label":"laptop"}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), rawError) {
		t.Fatalf("issue failure status=%d body=%q", response.Code, response.Body.String())
	}

	tokens.listErr = errors.New(rawError)
	response = personalTokenCall(t, server, http.MethodGet, "/v1/tokens", "owner-token", "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), rawError) {
		t.Fatalf("list failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestPersonalAccessTokenRoutesAreAbsentWithoutADurableCredentialStore(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Credentials = staticCredentialVerifier{
		"owner-token": {ID: "pat_owner", OwnerUserID: "owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
	}
	if response := personalTokenCall(t, server, http.MethodGet, "/v1/tokens", "owner-token", ""); response.Code != http.StatusNotFound {
		t.Fatalf("memory-store list status=%d body=%s", response.Code, response.Body.String())
	}
}
