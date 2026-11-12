package httpapi

// REST routes for stage preferences (req-stage-preferences AC-1.3, AC-1.6,
// AC-1.7, AC-2.4; component-stage-preferences invariants 1, 4, and 7).

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	stagePreferenceOwnerToken = "stage-preferences-owner-token"
	stagePreferenceOrigin     = "http://example.com"
)

// stagePreferenceHarness serves the real router over a volatile backend with
// two workspaces, the deployment owner, and a second person.
type stagePreferenceHarness struct {
	t              *testing.T
	st             store.Backend
	server         *Server
	owner, other   core.IdentityUser
	otherToken     string
	ownerSession   string
	ownerTokenCtx  context.Context
	workspaces     []string
	recordedEvents int
}

func newStagePreferenceHarness(t *testing.T) *stagePreferenceHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	system := store.WithActor(t.Context(), store.SystemActor())
	workspaces := []string{"alpha", "beta"}
	for _, workspace := range workspaces {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(system, workspace), &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapIdentity(system, config.FirstOperatorIdentity{OrganizationName: "Preferences", Email: "owner@example.test", DisplayName: "Owner"}, stagePreferenceOwnerToken); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(system, stagePreferenceOwnerToken)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	other, err := st.ProvisionIdentityUser(ownerCtx, "other@example.test", "Other")
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range workspaces {
		for _, grant := range []struct {
			email string
			role  core.WorkspaceRole
		}{{owner.Email, core.WorkspaceRoleOperator}, {other.Email, core.WorkspaceRoleViewer}} {
			if _, err := st.GrantWorkspaceRole(ownerCtx, grant.email, workspace, grant.role); err != nil {
				t.Fatal(err)
			}
		}
	}
	otherToken, err := st.IssueOwnPersonalAccessToken(store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(other.ID), Role: core.ActorUser}), other.ID, "other agent")
	if err != nil {
		t.Fatal(err)
	}
	link, err := st.IssueSignInLink(system, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(system, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspaces = st
	server.ConfigStore = st
	h := &stagePreferenceHarness{t: t, st: st, server: server, owner: owner, other: other, otherToken: otherToken.Value, ownerSession: session.Value, ownerTokenCtx: ownerCtx, workspaces: workspaces}
	h.recordedEvents = h.eventCount()
	return h
}

// eventCount counts every event in both workspaces, so a test can prove a
// preference write appends none (AC-3.3).
func (h *stagePreferenceHarness) eventCount() int {
	h.t.Helper()
	total := 0
	for _, workspace := range h.workspaces {
		events, err := h.st.ListEvents(store.WithWorkspace(store.WithActor(h.t.Context(), store.SystemActor()), workspace), "")
		if err != nil {
			h.t.Fatal(err)
		}
		total += len(events)
	}
	return total
}

type stagePreferenceRequest struct {
	method, path, body string
	token              string
	session            string
	header             map[string]string
}

func (h *stagePreferenceHarness) call(r stagePreferenceRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	if r.token != "" {
		request.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.session != "" {
		request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: r.session})
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range r.header {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	return response
}

func decodeStagePreferences(t *testing.T, response *httptest.ResponseRecorder) store.StagePreferences {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var set store.StagePreferences
	if err := json.Unmarshal(response.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	return set
}

func decodeStagePreferenceError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	if body["error"] != code {
		t.Fatalf("error=%v, want %s; body=%s", body["error"], code, response.Body.String())
	}
	return body
}

const stagePreferencePath = "/v1/me/stage-preferences"

func TestStagePreferencesRESTNeverSavedReadsUnset(t *testing.T) {
	h := newStagePreferenceHarness(t)
	response := h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken})
	if got := strings.TrimSpace(response.Body.String()); got != `{"version":0,"updated_at":null,"stages":{"plan":null,"implement":null,"verify":null,"review":null}}` {
		t.Fatalf("never-saved body = %s", got)
	}
	if response.Header().Get("ETag") != `"0"` {
		t.Fatalf("ETag = %q", response.Header().Get("ETag"))
	}
	if response := h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath}); response.Code != http.StatusUnauthorized {
		t.Fatalf("no credential status = %d", response.Code)
	}
}

func TestStagePreferencesRESTConditionalReplacement(t *testing.T) {
	h := newStagePreferenceHarness(t)
	put := func(ifMatch, body string) *httptest.ResponseRecorder {
		header := map[string]string{}
		if ifMatch != "" {
			header["If-Match"] = ifMatch
		}
		return h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken, body: body, header: header})
	}
	first := `{"stages":{"plan":{"harness":"codex","model":"gpt-6.1-sol","effort":"medium"},"review":{"primary":{"harness":"claude-code","model":"claude-opus-5-5"},"seats":[{"harness":"codex","model":"gpt-6.1-sol"}]}}}`

	// 428 without If-Match, and nothing is written.
	decodeStagePreferenceError(t, put("", first), http.StatusPreconditionRequired, "if_match_required")
	decodeStagePreferenceError(t, put("soon", first), http.StatusPreconditionRequired, "if_match_required")
	decodeStagePreferenceError(t, put("-1", first), http.StatusPreconditionRequired, "if_match_required")

	saved := decodeStagePreferences(t, put(`"0"`, first))
	if saved.Version != 1 || saved.Stages.Plan == nil || saved.Stages.Plan.Effort != "medium" || len(saved.Stages.Review.Seats) != 1 {
		t.Fatalf("first save = %+v", saved)
	}

	// 412 on a stale version, and the stored set is unchanged.
	decodeStagePreferenceError(t, put("0", `{"stages":{}}`), http.StatusPreconditionFailed, "version_conflict")
	decodeStagePreferenceError(t, put("5", `{"stages":{}}`), http.StatusPreconditionFailed, "version_conflict")

	// 422 names the field and the seat position, and the set is unchanged.
	seats := strings.Repeat(`{"harness":"h","model":"m"},`, 8) + `{"harness":"h","model":"m"}`
	body := decodeStagePreferenceError(t, put("1", `{"stages":{"review":{"seats":[`+seats+`]}}}`), http.StatusUnprocessableEntity, "validation_failed")
	fields, _ := body["fields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("fields = %v", body["fields"])
	}
	field, _ := fields[0].(map[string]any)
	if field["field"] != "stages.review.seats[8]" || field["position"] != float64(9) {
		t.Fatalf("ninth seat refusal = %v", field)
	}
	body = decodeStagePreferenceError(t, put("1", `{"stages":{"plan":{"harness":"h","model":"m"}},"execution_host":"sandbox-1"}`), http.StatusUnprocessableEntity, "validation_failed")
	if fields := body["fields"].([]any); fields[0].(map[string]any)["field"] != "execution_host" {
		t.Fatalf("execution host refusal = %v", body)
	}
	decodeStagePreferenceError(t, put("1", `{"stages":{"plan":{"harness":"h","model":"`+strings.Repeat("m", 129)+`"}}}`), http.StatusUnprocessableEntity, "validation_failed")

	read := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken}))
	if read.Version != 1 || read.Stages.Plan == nil || read.Stages.Plan.Harness != "codex" || len(read.Stages.Review.Seats) != 1 {
		t.Fatalf("refused writes changed the set: %+v", read)
	}

	// A current version advances by one.
	next := put("1", `{"stages":{"plan":null,"review":{"seats":[]}}}`)
	if next.Header().Get("ETag") != `"2"` {
		t.Fatalf("ETag = %q", next.Header().Get("ETag"))
	}
	if set := decodeStagePreferences(t, next); set.Version != 2 || set.Stages != (store.StagePreferenceStages{}) {
		t.Fatalf("clearing every stage = %+v", set)
	}
	if got := h.eventCount(); got != h.recordedEvents {
		t.Fatalf("preference writes appended %d events", got-h.recordedEvents)
	}
}

// TestStagePreferencesStayOutOfWorkspaceConfiguration covers AC-3.3: a saved
// set appears in no workspace configuration read and appends no event.
func TestStagePreferencesStayOutOfWorkspaceConfiguration(t *testing.T) {
	h := newStagePreferenceHarness(t)
	const marker = "advice-only-marker-harness"
	decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken,
		body: `{"stages":{"implement":{"harness":"` + marker + `","model":"` + marker + `"},"review":{"primary":{"harness":"` + marker + `","model":"m"}}}}`, header: map[string]string{"If-Match": "0"}}))
	for _, workspace := range h.workspaces {
		for _, path := range []string{"/v1/workspaces/" + workspace + "/config", "/v1/workspaces/" + workspace, "/v1/me?workspace_id=" + workspace} {
			response := h.call(stagePreferenceRequest{method: http.MethodGet, path: path, token: stagePreferenceOwnerToken})
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), marker) || strings.Contains(response.Body.String(), "stage_preferences") {
				t.Fatalf("GET %s carries the stage preferences: %s", path, response.Body.String())
			}
		}
	}
	if got := h.eventCount(); got != h.recordedEvents {
		t.Fatalf("a preference write appended %d events", got-h.recordedEvents)
	}
}

// TestStagePreferencesRESTCredentials proves the routes accept the owner's
// token and dashboard session, keep the session's mutation proof, and act only
// on the credential's own person (AC-1.6; DEC-116(2)).
func TestStagePreferencesRESTCredentials(t *testing.T) {
	h := newStagePreferenceHarness(t)
	planOnly := `{"stages":{"plan":{"harness":"codex","model":"gpt-6.1-sol"}}}`

	// A session without CSRF proof or with a foreign origin is refused.
	if response := h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, session: h.ownerSession, body: planOnly, header: map[string]string{"If-Match": "0"}}); response.Code != http.StatusForbidden {
		t.Fatalf("session without CSRF proof: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, session: h.ownerSession, body: planOnly, header: map[string]string{"If-Match": "0", "X-Conveyor-CSRF": "1", "Origin": "https://attacker.example"}}); response.Code != http.StatusForbidden {
		t.Fatalf("session from a foreign origin: status=%d", response.Code)
	}
	saved := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, session: h.ownerSession, body: planOnly, header: map[string]string{"If-Match": "0", "X-Conveyor-CSRF": "1", "Origin": stagePreferenceOrigin}}))
	if saved.Version != 1 {
		t.Fatalf("session save = %+v", saved)
	}

	// The owner's token reads the set the session saved, from either
	// workspace header, and a query naming another user changes nothing.
	for _, header := range []map[string]string{nil, {"X-Workspace-ID": "alpha"}, {"X-Workspace-ID": "beta"}} {
		read := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath + "?user_id=" + h.other.ID + "&workspace_id=beta", token: stagePreferenceOwnerToken, header: header}))
		if read.Version != 1 || read.Stages.Plan == nil {
			t.Fatalf("owner read with %v = %+v", header, read)
		}
	}

	// Another person's token sees only that person's own set and cannot name
	// the owner in the body.
	otherRead := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, token: h.otherToken}))
	if otherRead.Version != 0 || otherRead.Stages.Plan != nil {
		t.Fatalf("other person read the owner's set: %+v", otherRead)
	}
	body := decodeStagePreferenceError(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: h.otherToken, body: `{"user_id":"` + h.owner.ID + `","stages":{}}`, header: map[string]string{"If-Match": "1"}}), http.StatusUnprocessableEntity, "validation_failed")
	if body["fields"].([]any)[0].(map[string]any)["field"] != "user_id" {
		t.Fatalf("user_id refusal = %v", body)
	}
	otherSaved := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: h.otherToken, body: `{"stages":{"verify":{"harness":"opencode","model":"m"}}}`, header: map[string]string{"If-Match": "0"}}))
	if otherSaved.Version != 1 || otherSaved.Stages.Verify == nil || otherSaved.Stages.Plan != nil {
		t.Fatalf("other person's save = %+v", otherSaved)
	}
	ownerRead := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, session: h.ownerSession}))
	if ownerRead.Version != 1 || ownerRead.Stages.Verify != nil || ownerRead.Stages.Plan == nil {
		t.Fatalf("other person's save reached the owner: %+v", ownerRead)
	}
}

// failingStagePreferences fails every store call, so the route's 500 path is
// observable.
type failingStagePreferences struct{}

func (failingStagePreferences) GetStagePreferences(context.Context, string) (store.StagePreferences, error) {
	return store.StagePreferences{}, errors.New("pq: connection refused to secret-host")
}

func (failingStagePreferences) UpdateStagePreferences(context.Context, string, store.StagePreferenceUpdate) (store.StagePreferences, error) {
	return store.StagePreferences{}, errors.New("pq: connection refused to secret-host")
}

// TestStagePreferencesRESTStoreFailure covers the component's store-failure
// rule and DEC-105(1): a 500 with a generic body, never the cause or a set.
func TestStagePreferencesRESTStoreFailure(t *testing.T) {
	h := newStagePreferenceHarness(t)
	h.server.StagePreferences = failingStagePreferences{}
	for _, request := range []stagePreferenceRequest{
		{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken},
		{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken, body: `{"stages":{}}`, header: map[string]string{"If-Match": "0"}},
	} {
		response := h.call(request)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret-host") || strings.Contains(response.Body.String(), "version") {
			t.Fatalf("%s store failure: status=%d body=%q", request.method, response.Code, response.Body.String())
		}
	}
	h.server.StagePreferences = nil
	if response := h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken}); response.Code != http.StatusNotFound {
		t.Fatalf("no stage preference store: status=%d", response.Code)
	}
}

// TestStagePreferencesRESTOversizedBody covers component-http-api invariant
// 17: a body over the 64 KiB bound answers 413 with a valid credential and a
// current If-Match, and the stored set does not change. A body inside the
// bound with a refused field still answers 422.
func TestStagePreferencesRESTOversizedBody(t *testing.T) {
	h := newStagePreferenceHarness(t)
	saved := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken,
		body: `{"stages":{"plan":{"harness":"codex","model":"gpt-6.1-sol"}}}`, header: map[string]string{"If-Match": "0"}}))
	oversized := `{"stages":{"verify":{"harness":"h","model":"` + strings.Repeat("m", maxStagePreferenceBody) + `"}}}`
	unchanged := func(step string) {
		t.Helper()
		read := decodeStagePreferences(t, h.call(stagePreferenceRequest{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken}))
		if read.Version != saved.Version || read.Stages.Verify != nil || read.Stages.Plan == nil || read.Stages.Plan.Model != "gpt-6.1-sol" {
			t.Fatalf("%s changed the set: %+v", step, read)
		}
	}
	// A declared Content-Length over the bound.
	decodeStagePreferenceError(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken,
		body: oversized, header: map[string]string{"If-Match": "1"}}), http.StatusRequestEntityTooLarge, "body_too_large")
	unchanged("an oversized body")

	// A body of unknown length that passes the bound while it is read.
	request := httptest.NewRequest(http.MethodPut, stagePreferencePath, strings.NewReader(oversized))
	request.ContentLength = -1
	request.Header.Set("Authorization", "Bearer "+stagePreferenceOwnerToken)
	request.Header.Set("If-Match", "1")
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	decodeStagePreferenceError(t, response, http.StatusRequestEntityTooLarge, "body_too_large")
	unchanged("an oversized body of unknown length")

	// Field validation inside the bound stays 422.
	decodeStagePreferenceError(t, h.call(stagePreferenceRequest{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken,
		body: `{"stages":{"verify":{"harness":"h","model":"` + strings.Repeat("m", 129) + `"}}}`, header: map[string]string{"If-Match": "1"}}), http.StatusUnprocessableEntity, "validation_failed")
	unchanged("a refused field")
	if got := h.eventCount(); got != h.recordedEvents {
		t.Fatalf("refused writes appended %d events", got-h.recordedEvents)
	}
}

// TestStagePreferencesRESTTransportErrorIsGeneric shows the REST routes share
// the MCP rule: a host-bearing dial error answers a generic 500 that carries
// no host or driver text (DEC-105(1); component-persistence Errors
// invariants 11 to 13).
func TestStagePreferencesRESTTransportErrorIsGeneric(t *testing.T) {
	h := newStagePreferenceHarness(t)
	h.server.StagePreferences = erroringStagePreferences{err: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 77, 3, 9), Port: 3306}, Err: errors.New("connect: connection refused")}}
	for _, request := range []stagePreferenceRequest{
		{method: http.MethodGet, path: stagePreferencePath, token: stagePreferenceOwnerToken},
		{method: http.MethodPut, path: stagePreferencePath, token: stagePreferenceOwnerToken, body: `{"stages":{}}`, header: map[string]string{"If-Match": "0"}},
	} {
		response := h.call(request)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "10.77.3.9") || strings.Contains(response.Body.String(), "dial") {
			t.Fatalf("%s transport failure: status=%d body=%q", request.method, response.Code, response.Body.String())
		}
	}
}
