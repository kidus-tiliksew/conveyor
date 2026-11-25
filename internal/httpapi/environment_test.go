package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// environmentHarness drives the environment routes through the full handler
// over the volatile backend, with real credentials, sessions, and role
// bindings (component-environments Verification).
type environmentHarness struct {
	t       *testing.T
	st      store.Backend
	server  *Server
	handler http.Handler
	// ctx and betaCtx bind the two workspaces for direct store calls.
	ctx, betaCtx context.Context
	tokens       map[core.WorkspaceRole]string
	foreign      string
	session      string
}

func newEnvironmentHarness(t *testing.T) *environmentHarness {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	system := store.WithActor(t.Context(), store.SystemActor())
	for workspace, repository := range map[string]string{"alpha": "app", "beta": "elsewhere"} {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(system, workspace), &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: repository, URL: "https://example.test/" + repository, Base: "main"}}}); err != nil {
			t.Fatal(err)
		}
	}
	const ownerPAT = "environment-owner-token"
	if _, err := st.BootstrapIdentity(system, config.FirstOperatorIdentity{OrganizationName: "Environments", Email: "owner@example.test", DisplayName: "Owner"}, ownerPAT); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(system, ownerPAT)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	member := func(email, workspace string, role core.WorkspaceRole) string {
		t.Helper()
		user, err := st.ProvisionIdentityUser(ownerCtx, email, email)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.GrantWorkspaceRole(ownerCtx, email, workspace, role); err != nil {
			t.Fatal(err)
		}
		token, err := st.IssueOwnPersonalAccessToken(store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(user.ID), Role: core.ActorUser}), user.ID, "environment test")
		if err != nil {
			t.Fatal(err)
		}
		return token.Value
	}
	h := &environmentHarness{t: t, st: st, tokens: map[core.WorkspaceRole]string{}}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleViewer, core.WorkspaceRoleContributor, core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		h.tokens[role] = member(string(role)+"@example.test", "alpha", role)
	}
	h.foreign = member("foreign@example.test", "beta", core.WorkspaceRoleOperator)
	link, err := st.IssueSignInLink(system, "operator@example.test")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(system, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	h.session = session.Value
	h.ctx, h.betaCtx = store.WithWorkspace(system, "alpha"), store.WithWorkspace(system, "beta")
	h.server = NewServer(st)
	h.server.Workspaces, h.server.ConfigStore = st, st
	h.server.InvitationDelivery.PublicURL = oauthTestIssuer
	h.handler = h.server.Handler()
	return h
}

func (h *environmentHarness) manager() string { return h.tokens[core.WorkspaceRoleOperator] }

func (h *environmentHarness) serve(request *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func (h *environmentHarness) do(token, method, path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Workspace-ID", "alpha")
	return h.serve(request)
}

// want answers a request as the manager and requires its status.
func (h *environmentHarness) want(status int, method, path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	response := h.do(h.manager(), method, path, body)
	if response.Code != status {
		h.t.Fatalf("%s %s status=%d body=%s, want %d", method, path, response.Code, response.Body, status)
	}
	return response
}

func (h *environmentHarness) create(body string) store.EnvironmentRecord {
	h.t.Helper()
	var record store.EnvironmentRecord
	decodeEnvironmentBody(h.t, h.want(http.StatusCreated, http.MethodPost, "/v1/environments", body), &record)
	return record
}

func (h *environmentHarness) issue(environmentID string) store.IssuedEnvironmentKey {
	h.t.Helper()
	var issued store.IssuedEnvironmentKey
	decodeEnvironmentBody(h.t, h.want(http.StatusCreated, http.MethodPost, "/v1/environments/"+environmentID+"/keys", `{"label":"deploy"}`), &issued)
	return issued
}

// recorded renders every stored record and event of both workspaces.
func (h *environmentHarness) recorded() string {
	h.t.Helper()
	var parts []any
	for _, ctx := range []context.Context{h.ctx, h.betaCtx} {
		records, err := h.st.ListEnvironmentRecords(ctx, store.EnvironmentListQuery{})
		if err != nil {
			h.t.Fatal(err)
		}
		suggestions, err := h.st.ListEnvironmentSuggestions(ctx)
		if err != nil {
			h.t.Fatal(err)
		}
		parts = append(parts, records, suggestions)
	}
	events, err := h.st.ListEvents(h.ctx, "")
	if err != nil {
		h.t.Fatal(err)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "environment.") {
			parts = append(parts, event)
		}
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func decodeEnvironmentBody(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("body %s: %v", response.Body, err)
	}
}

func environmentBodyKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("body %s: %v", raw, err)
	}
	return slices.Sorted(maps.Keys(fields))
}

var (
	environmentRowFields = []string{"archived", "created_at", "created_by", "github_environments", "id", "keys", "name", "timing_window_minutes", "tracked_branches", "version_rule"}
	environmentKeyFields = []string{"created_at", "created_by", "id", "label", "last_used_at"}
)

// environmentMutations is each mutating route with a body it accepts.
func environmentMutations(environmentID, keyID string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodPost, "/v1/environments", `{"name":"Other"}`},
		{http.MethodPatch, "/v1/environments/" + environmentID, `{"timing_window_minutes":5}`},
		{http.MethodPost, "/v1/environments/" + environmentID + "/keys", `{"label":"other"}`},
		{http.MethodDelete, "/v1/environments/" + environmentID + "/keys/" + keyID, ""},
		{http.MethodPost, "/v1/environments/suggestions/ignore", `{"github_environment":"preview"}`},
	}
}

var environmentReads = []string{"/v1/environments", "/v1/environments?archived=true", "/v1/environments/suggestions"}

func TestEnvironmentRoutes(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	created := h.want(http.StatusCreated, http.MethodPost, "/v1/environments", `{"name":" Production ","tracked_branches":{"app":"main"},"github_environments":["prod"],"version_rule":{"kind":"commit_pattern","text":"v*-{commit}"},"timing_window_minutes":90}`)
	if got := environmentBodyKeys(t, created.Body.Bytes()); !slices.Equal(got, environmentRowFields) {
		t.Fatalf("environment row fields = %v", got)
	}
	var record store.EnvironmentRecord
	decodeEnvironmentBody(t, created, &record)
	if record.Name != "Production" || record.TrackedBranches["app"] != "main" || !slices.Equal(record.GitHubEnvironments, []string{"prod"}) || record.VersionRule == nil || record.TimingWindowMinutes != 90 || record.Archived || !strings.HasPrefix(record.CreatedBy, "user:") || !strings.Contains(created.Body.String(), `"keys":[]`) {
		t.Fatalf("created = %s", created.Body)
	}
	defaults := h.create(`{"name":"Staging"}`)
	if defaults.TimingWindowMinutes != 60 || defaults.VersionRule != nil || defaults.TrackedBranches == nil || defaults.GitHubEnvironments == nil {
		t.Fatalf("defaults = %+v", defaults)
	}
	if archived := h.create(`{"name":"Old","archived":true}`); !archived.Archived {
		t.Fatalf("created archived = %+v", archived)
	}

	// An omitted field stays, and an explicit null clears the version rule.
	var patched store.EnvironmentRecord
	decodeEnvironmentBody(t, h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{"name":"Prod"}`), &patched)
	if patched.Name != "Prod" || patched.VersionRule == nil || patched.TimingWindowMinutes != 90 || patched.TrackedBranches["app"] != "main" {
		t.Fatalf("patched = %+v", patched)
	}
	patched = store.EnvironmentRecord{}
	decodeEnvironmentBody(t, h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{"version_rule":null,"tracked_branches":{},"github_environments":[]}`), &patched)
	if patched.VersionRule != nil || len(patched.TrackedBranches) != 0 || len(patched.GitHubEnvironments) != 0 || patched.Name != "Prod" {
		t.Fatalf("cleared = %+v", patched)
	}
	before := h.recorded()
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{}`)
	if h.recorded() != before {
		t.Fatal("an empty edit changed a record")
	}

	// The value is in the answer to the issue and in no other.
	response := h.want(http.StatusCreated, http.MethodPost, "/v1/environments/"+record.ID+"/keys", `{"label":" deploy "}`)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("issue headers = %v, want no-store", response.Header())
	}
	if got := environmentBodyKeys(t, response.Body.Bytes()); !slices.Equal(got, slices.Sorted(slices.Values(append([]string{"value"}, environmentKeyFields...)))) {
		t.Fatalf("issued key fields = %v", got)
	}
	var issued store.IssuedEnvironmentKey
	decodeEnvironmentBody(t, response, &issued)
	if issued.Label != "deploy" || !strings.HasPrefix(issued.Value, "cv_env_"+issued.ID+"_") || !strings.HasPrefix(issued.CreatedBy, "user:") || issued.LastUsedAt != nil {
		t.Fatalf("issued = %+v", issued.EnvironmentKey)
	}
	listed := h.want(http.StatusOK, http.MethodGet, "/v1/environments", "")
	var rows []struct {
		ID   string            `json:"id"`
		Keys []json.RawMessage `json:"keys"`
	}
	decodeEnvironmentBody(t, listed, &rows)
	if len(rows) != 2 || rows[0].ID != record.ID || len(rows[0].Keys) != 1 || !slices.Equal(environmentBodyKeys(t, rows[0].Keys[0]), environmentKeyFields) {
		t.Fatalf("listed = %s", listed.Body)
	}
	patchedAgain := h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{"timing_window_minutes":5}`)
	for _, read := range []string{listed.Body.String(), patchedAgain.Body.String(), h.recorded()} {
		if strings.Contains(read, issued.Value) || strings.Contains(read, strings.TrimPrefix(issued.Value, "cv_env_"+issued.ID+"_")) {
			t.Fatal("a later read holds the key value")
		}
	}
	if verified, err := h.st.VerifyEnvironmentKey(t.Context(), issued.Value); err != nil || verified.EnvironmentID != record.ID || verified.KeyID != issued.ID || verified.Workspace != "alpha" {
		t.Fatalf("verified = %+v %v", verified, err)
	}
	for range 2 {
		h.want(http.StatusNoContent, http.MethodDelete, "/v1/environments/"+record.ID+"/keys/"+issued.ID, "")
	}
	if strings.Contains(h.want(http.StatusOK, http.MethodGet, "/v1/environments", "").Body.String(), issued.ID) {
		t.Fatal("a read lists a revoked key")
	}

	// Archived environments are listed on request.
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{"archived":true}`)
	for query, want := range map[string][]string{"": {"Staging"}, "?archived=false": {"Staging"}, "?archived=true": {"Old", "Prod"}} {
		var records []store.EnvironmentRecord
		decodeEnvironmentBody(t, h.want(http.StatusOK, http.MethodGet, "/v1/environments"+query, ""), &records)
		var names []string
		for _, record := range records {
			names = append(names, record.Name)
		}
		if !slices.Equal(names, want) {
			t.Fatalf("list %q = %v, want %v", query, names, want)
		}
	}
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+record.ID, `{"archived":false}`)
	page := h.want(http.StatusOK, http.MethodGet, "/v1/environments?limit=1&offset=1", "")
	if page.Header().Get("X-Conveyor-Total") != "2" || !strings.Contains(page.Body.String(), "Staging") || strings.Contains(page.Body.String(), "Prod") {
		t.Fatalf("page = %v %s", page.Header(), page.Body)
	}

	// The actor of each event is the person, and the kinds are the document's.
	events, err := h.st.ListEvents(h.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "environment.") {
			kinds[event.Kind]++
			if event.ActorID != record.CreatedBy || event.ActorRole != core.ActorUser {
				t.Fatalf("event %s actor = %s %s", event.Kind, event.ActorID, event.ActorRole)
			}
		}
	}
	if want := (map[string]int{"environment.created": 3, "environment.updated": 3, "environment.archived": 1, "environment.restored": 1, "environment.key_issued": 1, "environment.key_revoked": 1}); !maps.Equal(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
}

func TestEnvironmentSuggestionRoutes(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	if body := strings.TrimSpace(h.want(http.StatusOK, http.MethodGet, "/v1/environments/suggestions", "").Body.String()); body != "[]" {
		t.Fatalf("suggestions = %s, want an empty array", body)
	}
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"preview", "canary"} {
		if recorded, err := h.st.RecordEnvironmentReport(h.ctx, store.EnvironmentReport{GitHubEnvironment: name, Repository: "app", ReportedAt: at}); err != nil || !recorded {
			t.Fatalf("report recorded=%v err=%v", recorded, err)
		}
	}
	want := `[{"github_environment":"canary","repository":"app","last_reported_at":"2026-11-25T12:00:00Z"},{"github_environment":"preview","repository":"app","last_reported_at":"2026-11-25T12:00:00Z"}]`
	if body := strings.TrimSpace(h.want(http.StatusOK, http.MethodGet, "/v1/environments/suggestions", "").Body.String()); body != want {
		t.Fatalf("suggestions = %s", body)
	}
	// A suggestion is on no other read and creates nothing.
	for _, path := range []string{"/v1/environments", "/v1/environments?archived=true"} {
		if body := strings.TrimSpace(h.want(http.StatusOK, http.MethodGet, path, "").Body.String()); body != "[]" {
			t.Fatalf("%s = %s", path, body)
		}
	}
	h.want(http.StatusNoContent, http.MethodPost, "/v1/environments/suggestions/ignore", `{"github_environment":"PREVIEW"}`)
	h.create(`{"name":"canary","github_environments":["canary"]}`)
	if body := strings.TrimSpace(h.want(http.StatusOK, http.MethodGet, "/v1/environments/suggestions", "").Body.String()); body != "[]" {
		t.Fatalf("suggestions after an ignore and a create = %s", body)
	}
}

// TestEnvironmentSuggestionDefaultPage proves that the suggestions list is
// bounded without a limit and pages through every row
// (component-http-api invariant 19).
func TestEnvironmentSuggestionDefaultPage(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	const total = 2*defaultHistoryPageLimit + 5
	for i := range total {
		if recorded, err := h.st.RecordEnvironmentReport(h.ctx, store.EnvironmentReport{GitHubEnvironment: fmt.Sprintf("preview-%04d", i), Repository: "app", ReportedAt: time.Now()}); err != nil || !recorded {
			t.Fatalf("report recorded=%v err=%v", recorded, err)
		}
	}
	for _, c := range []struct {
		query               string
		first, count        int
		limit, offset, name string
	}{
		{"", 0, defaultHistoryPageLimit, "100", "0", "default page"},
		{"?limit=200", 0, 200, "200", "0", "largest page"},
		{"?limit=100&offset=200", 200, 5, "100", "200", "last page"},
		{"?limit=100&offset=300", 0, 0, "100", "300", "page past the end"},
	} {
		response := h.want(http.StatusOK, http.MethodGet, "/v1/environments/suggestions"+c.query, "")
		var rows []store.EnvironmentSuggestion
		decodeEnvironmentBody(t, response, &rows)
		header := response.Header()
		if len(rows) != c.count || header.Get("X-Conveyor-Total") != fmt.Sprint(total) || header.Get("X-Conveyor-Limit") != c.limit || header.Get("X-Conveyor-Offset") != c.offset {
			t.Fatalf("%s: rows=%d headers=%v", c.name, len(rows), header)
		}
		if c.count > 0 && rows[0].GitHubEnvironment != fmt.Sprintf("preview-%04d", c.first) {
			t.Fatalf("%s starts at %s", c.name, rows[0].GitHubEnvironment)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?offset=1"} {
		h.want(http.StatusBadRequest, http.MethodGet, "/v1/environments/suggestions"+query, "")
	}
}

// TestEnvironmentFieldsRefuseCredentials sends a live key value and another
// credential in every text field. Each request is refused without the text,
// records nothing, and leaves the key valid (req-security-boundaries REQ-2,
// REQ-3; component-identity-membership invariant 45).
func TestEnvironmentFieldsRefuseCredentials(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	environment := h.create(`{"name":"Production"}`)
	key := h.issue(environment.ID)
	path := "/v1/environments/" + environment.ID
	before := h.recorded()
	for _, secret := range []string{key.Value, "ghp_Conformance0123456789abcdefABCDEF012"} {
		fields := map[string]string{
			`"name":"` + secret + `"`:                                                            "name",
			`"github_environments":["prod","` + secret + `"]`:                                    "github_environments",
			`"version_rule":{"kind":"commit_pattern","text":"` + secret + `{commit}"}`:           "version_rule",
			`"version_rule":{"kind":"tag_template","text":"{service}/` + secret + `/{version}"}`: "version_rule",
			`"tracked_branches":{"app":"` + secret + `"}`:                                        "tracked_branches",
			`"tracked_branches":{"` + secret + `":"main"}`:                                       "tracked_branches",
		}
		type request struct{ method, path, body, field string }
		requests := []request{
			{http.MethodPost, path + "/keys", `{"label":"` + secret + `"}`, "label"},
			{http.MethodPost, "/v1/environments/suggestions/ignore", `{"github_environment":"` + secret + `"}`, "github_environment"},
		}
		for body, field := range fields {
			requests = append(requests, request{http.MethodPatch, path, "{" + body + "}", field})
			if field != "name" {
				body = `"name":"Other",` + body
			}
			requests = append(requests, request{http.MethodPost, "/v1/environments", "{" + body + "}", field})
		}
		for _, c := range requests {
			response := h.want(http.StatusBadRequest, c.method, c.path, c.body)
			var body map[string]string
			decodeEnvironmentBody(t, response, &body)
			if body["error"] != "invalid_environment" || body["field"] != c.field || !strings.Contains(body["message"], "credential") || strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s %s field %s: %v", c.method, c.path, c.field, body["message"])
			}
		}
	}
	if after := h.recorded(); after != before {
		t.Fatal("a refused credential changed a record or an event")
	}
	if _, err := h.st.VerifyEnvironmentKey(t.Context(), key.Value); err != nil {
		t.Fatalf("the key after the refusals: %v", err)
	}
}

func TestEnvironmentRefusals(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	production := h.create(`{"name":"Production","github_environments":["prod"]}`)
	archived := h.create(`{"name":"Archived","archived":true}`)
	full := h.create(`{"name":"Full"}`)
	for range store.MaxEnvironmentKeys {
		h.issue(full.ID)
	}
	path := "/v1/environments/" + production.ID
	long := func(n int) string { return strings.Repeat("a", n) }
	eleven, err := json.Marshal(strings.Split(strings.Repeat("n,", 10)+"n", ","))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 11 {
		eleven = bytes.Replace(eleven, []byte(`"n"`), fmt.Appendf(nil, `"n%d"`, i), 1)
	}
	type refusal struct {
		name, method, path, body string
		status                   int
		code, field, mention     string
	}
	invalid := func(name, method, path, body, field, mention string) refusal {
		return refusal{name, method, path, body, http.StatusBadRequest, "invalid_environment", field, mention}
	}
	const missing = "env-000000-000000"
	cases := []refusal{
		invalid("create without a name", "POST", "/v1/environments", `{}`, "name", ""),
		invalid("null name", "POST", "/v1/environments", `{"name":null}`, "name", ""),
		invalid("name of another type", "PATCH", path, `{"name":7}`, "name", ""),
		invalid("empty name", "PATCH", path, `{"name":" "}`, "name", ""),
		invalid("long name", "POST", "/v1/environments", `{"name":"`+long(41)+`"}`, "name", ""),
		invalid("NUL in a name", "POST", "/v1/environments", `{"name":"a\u0000b"}`, "name", ""),
		invalid("taken name", "POST", "/v1/environments", `{"name":"PRODUCTION"}`, "name", "Production"),
		invalid("unknown field", "POST", "/v1/environments", `{"name":"Other","id":"env-mine"}`, "id", ""),
		invalid("field in another case", "PATCH", path, `{"Name":"Other"}`, "Name", ""),
		invalid("repeated field", "PATCH", path, `{"name":"One","name":"Two"}`, "name", ""),
		invalid("unknown repository", "PATCH", path, `{"tracked_branches":{"elsewhere":"main"}}`, "tracked_branches", "elsewhere"),
		invalid("repeated repository", "PATCH", path, `{"tracked_branches":{"app":"main","app":"next"}}`, "tracked_branches", "app"),
		invalid("illegal branch", "PATCH", path, `{"tracked_branches":{"app":"two words"}}`, "tracked_branches", "app"),
		invalid("null branches", "PATCH", path, `{"tracked_branches":null}`, "tracked_branches", ""),
		invalid("null branch", "PATCH", path, `{"tracked_branches":{"app":null}}`, "tracked_branches", ""),
		invalid("branch list", "PATCH", path, `{"tracked_branches":["main"]}`, "tracked_branches", ""),
		invalid("null GitHub names", "PATCH", path, `{"github_environments":null}`, "github_environments", ""),
		invalid("null GitHub name", "PATCH", path, `{"github_environments":[null]}`, "github_environments", ""),
		invalid("eleven GitHub names", "PATCH", path, `{"github_environments":`+string(eleven)+`}`, "github_environments", ""),
		invalid("long GitHub name", "PATCH", path, `{"github_environments":["`+long(256)+`"]}`, "github_environments", ""),
		invalid("taken GitHub name", "PATCH", "/v1/environments/"+full.ID, `{"github_environments":["PROD"]}`, "github_environments", "Production"),
		invalid("rule without a placeholder", "PATCH", path, `{"version_rule":{"kind":"commit_pattern","text":"v1"}}`, "version_rule", ""),
		invalid("tag template that names no tag", "PATCH", path, `{"version_rule":{"kind":"tag_template","text":"release {version}"}}`, "version_rule", "Git tag"),
		invalid("rule of another kind", "PATCH", path, `{"version_rule":{"kind":"regex","text":"{commit}"}}`, "version_rule", ""),
		invalid("rule without a text", "PATCH", path, `{"version_rule":{"kind":"commit_pattern"}}`, "version_rule", ""),
		invalid("rule with another field", "PATCH", path, `{"version_rule":{"kind":"commit_pattern","text":"{commit}","flags":"i"}}`, "version_rule", ""),
		invalid("rule text", "PATCH", path, `{"version_rule":"{commit}"}`, "version_rule", ""),
		invalid("zero window", "PATCH", path, `{"timing_window_minutes":0}`, "timing_window_minutes", ""),
		invalid("wide window", "PATCH", path, `{"timing_window_minutes":1441}`, "timing_window_minutes", ""),
		invalid("fractional window", "PATCH", path, `{"timing_window_minutes":1.5}`, "timing_window_minutes", ""),
		invalid("null window", "POST", "/v1/environments", `{"name":"Other","timing_window_minutes":null}`, "timing_window_minutes", ""),
		invalid("window text", "PATCH", path, `{"timing_window_minutes":"60"}`, "timing_window_minutes", ""),
		invalid("null archived", "PATCH", path, `{"archived":null}`, "archived", ""),
		invalid("archived text", "PATCH", path, `{"archived":"true"}`, "archived", ""),
		invalid("archived filter", "GET", "/v1/environments?archived=all", "", "archived", ""),
		invalid("key without a label", "POST", path+"/keys", `{}`, "label", ""),
		invalid("empty label", "POST", path+"/keys", `{"label":" "}`, "label", ""),
		invalid("long label", "POST", path+"/keys", `{"label":"`+long(61)+`"}`, "label", ""),
		invalid("key with another field", "POST", path+"/keys", `{"label":"x","value":"mine"}`, "value", ""),
		invalid("ignore without a name", "POST", "/v1/environments/suggestions/ignore", `{}`, "github_environment", ""),
		invalid("ignore of a listed name", "POST", "/v1/environments/suggestions/ignore", `{"github_environment":"prod"}`, "github_environment", "Production"),
		{"missing environment", "PATCH", "/v1/environments/" + missing, `{"name":"Other"}`, http.StatusNotFound, "environment_not_found", "", missing},
		{"key of a missing environment", "POST", "/v1/environments/" + missing + "/keys", `{"label":"x"}`, http.StatusNotFound, "environment_not_found", "", ""},
		{"revocation of a missing key", "DELETE", path + "/keys/envkey_missing", "", http.StatusNotFound, "environment_not_found", "", ""},
		{"revocation in a missing environment", "DELETE", "/v1/environments/" + missing + "/keys/envkey_missing", "", http.StatusNotFound, "environment_not_found", "", ""},
		{"edit of an archived environment", "PATCH", "/v1/environments/" + archived.ID, `{"name":"Other"}`, http.StatusConflict, "environment_archived", "", ""},
		{"edit and restore in one request", "PATCH", "/v1/environments/" + archived.ID, `{"name":"Other","archived":false}`, http.StatusConflict, "environment_archived", "", ""},
		{"key of an archived environment", "POST", "/v1/environments/" + archived.ID + "/keys", `{"label":"x"}`, http.StatusConflict, "environment_archived", "", ""},
		{"sixth key", "POST", "/v1/environments/" + full.ID + "/keys", `{"label":"sixth"}`, http.StatusConflict, "environment_key_limit", "", ""},
	}
	before := h.recorded()
	for _, c := range cases {
		response := h.want(c.status, c.method, c.path, c.body)
		var body map[string]string
		decodeEnvironmentBody(t, response, &body)
		if body["error"] != c.code || body["field"] != c.field || body["message"] == "" || !strings.Contains(body["message"], c.mention) {
			t.Fatalf("%s: body = %s", c.name, response.Body)
		}
	}
	for name, c := range map[string]struct {
		body   string
		status int
	}{
		"malformed": {`{"name":`, http.StatusBadRequest},
		"empty":     {``, http.StatusBadRequest},
		"array":     {`[]`, http.StatusBadRequest},
		"null":      {`null`, http.StatusBadRequest},
		"trailing":  {`{"name":"Other"} {}`, http.StatusBadRequest},
		"oversized": {`{"name":"` + long(environmentBodyLimit) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		for _, target := range []struct{ method, path string }{{"POST", "/v1/environments"}, {"PATCH", path}, {"POST", path + "/keys"}, {"POST", "/v1/environments/suggestions/ignore"}} {
			if response := h.do(h.manager(), target.method, target.path, c.body); response.Code != c.status {
				t.Fatalf("%s body on %s %s: status=%d body=%s", name, target.method, target.path, response.Code, response.Body)
			}
		}
	}
	if after := h.recorded(); after != before {
		t.Fatalf("a refused request changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// A refused restore answers the same code and stays archived.
	h.create(`{"name":"archived"}`)
	var body map[string]string
	decodeEnvironmentBody(t, h.want(http.StatusBadRequest, http.MethodPatch, "/v1/environments/"+archived.ID, `{"archived":false}`), &body)
	if body["error"] != "invalid_environment" || body["field"] != "name" {
		t.Fatalf("refused restore = %v", body)
	}
}

func TestEnvironmentCapabilities(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	environment := h.create(`{"name":"Production"}`)
	key := h.issue(environment.ID)
	before := h.recorded()
	requireRefusal := func(response *httptest.ResponseRecorder, status int, text, what string) {
		t.Helper()
		if response.Code != status || !strings.Contains(response.Body.String(), text) {
			t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body, status, text)
		}
	}
	for role, token := range h.tokens {
		for _, path := range environmentReads {
			if response := h.do(token, http.MethodGet, path, ""); response.Code != http.StatusOK {
				t.Fatalf("%s reads %s: %d %s", role, path, response.Code, response.Body)
			}
		}
		if role == core.WorkspaceRoleOperator {
			continue
		}
		// A missing capability answers before the body or the record is read.
		for _, c := range environmentMutations(environment.ID, key.ID) {
			requireRefusal(h.do(token, c.method, c.path, c.body), http.StatusNotFound, "workspace_not_found", string(role)+" "+c.method+" "+c.path)
		}
		for _, c := range environmentMutations("env-000000-000000", "envkey_missing") {
			requireRefusal(h.do(token, c.method, c.path, `{`), http.StatusNotFound, "workspace_not_found", string(role)+" "+c.method+" "+c.path)
		}
	}
	for _, path := range environmentReads {
		requireRefusal(h.do(h.foreign, http.MethodGet, path, ""), http.StatusNotFound, "workspace_not_found", "foreign read")
		requireRefusal(h.do("unknown-token", http.MethodGet, path, ""), http.StatusUnauthorized, "unauthorized", "unknown credential")
	}
	for _, c := range environmentMutations(environment.ID, key.ID) {
		requireRefusal(h.do(h.foreign, c.method, c.path, c.body), http.StatusNotFound, "workspace_not_found", "foreign mutation")
		requireRefusal(h.do("unknown-token", c.method, c.path, c.body), http.StatusUnauthorized, "unauthorized", "unknown credential")
		// A session needs its proof for every mutation.
		request := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		request.Header.Set("X-Workspace-ID", "alpha")
		request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: h.session})
		requireRefusal(h.serve(request), http.StatusForbidden, "", "session without proof")
	}
	// A manager of another workspace addresses this one's records through
	// their own workspace, and the answer is a missing record's.
	for _, c := range environmentMutations(environment.ID, key.ID) {
		if !strings.Contains(c.path, environment.ID) {
			continue
		}
		request := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		request.Header.Set("Authorization", "Bearer "+h.foreign)
		request.Header.Set("X-Workspace-ID", "beta")
		requireRefusal(h.serve(request), http.StatusNotFound, "environment_not_found", "a record of another workspace")
	}
	if after := h.recorded(); after != before {
		t.Fatalf("a refused request changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// A manager's session with its proof is admitted.
	request := httptest.NewRequest(http.MethodPost, "/v1/environments", strings.NewReader(`{"name":"By session"}`))
	request.Header.Set("X-Workspace-ID", "alpha")
	request.Header.Set("X-Conveyor-CSRF", "1")
	request.Header.Set("Origin", oauthTestIssuer)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: h.session})
	if response := h.serve(request); response.Code != http.StatusCreated {
		t.Fatalf("session create: %d %s", response.Code, response.Body)
	}
}

// TestEnvironmentKeyIsRefusedOnEveryRoute proves component-environments
// invariant 15 and req-security-boundaries AC-1.5: a live key is refused on
// every route before any admission, a session cookie beside it changes
// nothing, and the unmounted signal route accepts nothing.
func TestEnvironmentKeyIsRefusedOnEveryRoute(t *testing.T) {
	t.Parallel()
	h := newEnvironmentHarness(t)
	environment := h.create(`{"name":"Production"}`)
	key := h.issue(environment.ID)
	before := h.recorded()
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/tasks"}, {http.MethodPost, "/v1/tasks"}, {http.MethodGet, "/v1/workspaces"}, {http.MethodGet, "/v1/me"}, {http.MethodGet, "/v1/tokens"}, {http.MethodPost, "/v1/tokens"}, {http.MethodPost, "/v1/users"},
		{http.MethodGet, "/v1/workspace/config"}, {http.MethodGet, "/v1/crates"}, {http.MethodPost, "/v1/artifacts"}, {http.MethodGet, "/v1/workspaces/alpha/changes"},
		{http.MethodPost, "/mcp"}, {http.MethodGet, "/mcp"},
		{http.MethodGet, "/v1/work-orders"}, {http.MethodGet, "/v1/work-orders/any/pull-request-template"}, {http.MethodPost, "/v1/work-orders/any/submit-for-review"}, {http.MethodPost, "/v1/work-orders/any/context-refresh"}, {http.MethodPost, "/v1/work-orders/any/verification/submit-verification"},
		{http.MethodGet, "/healthz"}, {http.MethodGet, "/v1/version"}, {http.MethodPost, "/v1/sign-in/password"}, {http.MethodPost, "/v1/github/webhook/alpha"}, {http.MethodGet, "/.well-known/oauth-protected-resource"}, {http.MethodPost, "/oauth/token"},
		{http.MethodGet, "/"}, {http.MethodGet, "/assets/any.js"}, {http.MethodGet, "/v1/unknown"},
		// Near matches of the signal path.
		{http.MethodPost, "/v1/feedback/signals/"}, {http.MethodPost, "/v1/feedback/signals/more"}, {http.MethodPost, "/v1/feedback/signal"}, {http.MethodPost, "/v1/feedback"}, {http.MethodPost, "/v1/feedback/%73ignals"}, {http.MethodPost, "/v1/feedback%2Fsignals"}, {http.MethodPost, "//v1/feedback/signals"}, {http.MethodPost, "/V1/feedback/signals"}, {http.MethodPost, "/v1/./feedback/signals"},
	}
	for _, path := range environmentReads {
		routes = append(routes, struct{ method, path string }{http.MethodGet, path})
	}
	for _, c := range environmentMutations(environment.ID, key.ID) {
		routes = append(routes, struct{ method, path string }{c.method, c.path})
	}
	for _, route := range routes {
		for name, prepare := range map[string]func(*http.Request){
			"bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key.Value) },
			"scheme case": func(r *http.Request) { r.Header.Set("Authorization", "bEARER   "+key.Value) },
			"bare prefix": func(r *http.Request) { r.Header.Set("Authorization", "Bearer cv_env_") },
			"first header": func(r *http.Request) {
				r.Header["Authorization"] = []string{"Bearer " + key.Value, "Bearer " + h.manager()}
			},
			"second header": func(r *http.Request) {
				r.Header["Authorization"] = []string{"Bearer " + h.manager(), "Bearer " + key.Value}
			},
			"session cookie": func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+key.Value)
				r.Header.Set("X-Conveyor-CSRF", "1")
				r.Header.Set("Origin", oauthTestIssuer)
				r.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: h.session})
			},
		} {
			request := httptest.NewRequest(route.method, "http://conveyor.example/", strings.NewReader(`{"name":"Mine","label":"mine"}`))
			parsed, err := request.URL.Parse("http://conveyor.example" + route.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(route.path, "//") || strings.Contains(route.path, "/./") {
				// Keep the raw path, which resolving a reference would clean.
				parsed.Path = route.path
			}
			request.URL = parsed
			request.Header.Set("X-Workspace-ID", "alpha")
			prepare(request)
			response := h.serve(request)
			challenge := response.Header().Get("WWW-Authenticate")
			if response.Code != http.StatusUnauthorized || strings.TrimSpace(response.Body.String()) != "unauthorized" || !strings.HasPrefix(challenge, "Bearer") {
				t.Fatalf("%s %s with %s: status=%d challenge=%q body=%s", route.method, route.path, name, response.Code, challenge, response.Body)
			}
			if named := strings.Contains(challenge, "resource_metadata"); named != (route.path == "/mcp") {
				t.Fatalf("%s %s challenge = %q", route.method, route.path, challenge)
			}
		}
	}
	// The exact signal path is not mounted, so it admits nothing.
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		request := httptest.NewRequest(method, environmentSignalPath, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+key.Value)
		if response := h.serve(request); response.Code != http.StatusNotFound {
			t.Fatalf("%s signal path: status=%d body=%s, want 404", method, response.Code, response.Body)
		}
	}
	// A key in the session cookie is no session.
	request := httptest.NewRequest(http.MethodGet, "/v1/environments", nil)
	request.Header.Set("X-Workspace-ID", "alpha")
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: key.Value})
	if response := h.serve(request); response.Code != http.StatusUnauthorized {
		t.Fatalf("key as a session: status=%d", response.Code)
	}
	if after := h.recorded(); after != before {
		t.Fatalf("a refused key changed a record or its last use:\nbefore %s\nafter  %s", before, after)
	}
	// Other credentials are untouched, in any scheme that is not a bearer.
	for _, header := range []string{"", "Basic " + key.Value, key.Value} {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		if response := h.serve(request); response.Code != http.StatusOK {
			t.Fatalf("healthz with %q scheme: status=%d", strings.Split(header, " ")[0], response.Code)
		}
	}
	// With OAuth off, /mcp answers the bare challenge.
	h.server.InvitationDelivery.PublicURL = ""
	h.handler = h.server.Handler()
	if response := h.do(key.Value, http.MethodPost, "/mcp", "{}"); response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("/mcp with OAuth off: %d %v", response.Code, response.Header())
	}
}

// TestEnvironmentKeyValueStaysOutOfLogsAndErrors issues, uses, and revokes a
// key, and sends its value where a person could paste it by mistake.
func TestEnvironmentKeyValueStaysOutOfLogsAndErrors(t *testing.T) {
	h := newEnvironmentHarness(t)
	logs := captureOAuthLogs(t)
	h.handler = h.server.Handler()
	environment := h.create(`{"name":"Production"}`)
	key := h.issue(environment.ID)
	h.do(key.Value, http.MethodGet, "/v1/environments", "")
	h.do(key.Value, http.MethodPost, environmentSignalPath, "{}")
	for _, c := range []struct{ path, body string }{
		{"/v1/environments", `{"name":"Other","` + key.Value + `":1}`},
		{"/v1/environments", `{"name":"Other","tracked_branches":{"` + key.Value + `":"main"}}`},
		{"/v1/environments", `{"name":"` + key.Value + `"}`},
	} {
		if response := h.want(http.StatusBadRequest, http.MethodPost, c.path, c.body); strings.Contains(response.Body.String(), key.Value) {
			t.Fatal("a refusal sends the key value back")
		}
	}
	if !strings.Contains(logs.String(), "/v1/environments/"+environment.ID+"/keys") {
		t.Fatalf("the access log was not captured: %s", logs.String())
	}
	if strings.Contains(logs.String(), key.Value) || strings.Contains(logs.String(), "cv_env_") {
		t.Fatal("a log line holds the key value")
	}
}

// failingEnvironmentStore fails every environment read and write.
type failingEnvironmentStore struct {
	store.Backend
	err error
}

func (f failingEnvironmentStore) CreateEnvironment(context.Context, store.EnvironmentInput) (store.EnvironmentRecord, error) {
	return store.EnvironmentRecord{}, f.err
}

func (f failingEnvironmentStore) UpdateEnvironment(context.Context, string, store.EnvironmentPatch) (store.EnvironmentRecord, error) {
	return store.EnvironmentRecord{}, f.err
}

func (f failingEnvironmentStore) ListEnvironments(context.Context, store.EnvironmentListQuery) ([]store.Environment, error) {
	return nil, f.err
}

func (f failingEnvironmentStore) ListEnvironmentRecords(context.Context, store.EnvironmentListQuery) ([]store.EnvironmentRecord, error) {
	return nil, f.err
}

func (f failingEnvironmentStore) IssueEnvironmentKey(context.Context, string, string) (store.IssuedEnvironmentKey, error) {
	return store.IssuedEnvironmentKey{}, f.err
}

func (f failingEnvironmentStore) RevokeEnvironmentKey(context.Context, string, string) error {
	return f.err
}

func (f failingEnvironmentStore) ListEnvironmentSuggestions(context.Context) ([]store.EnvironmentSuggestion, error) {
	return nil, f.err
}

func (f failingEnvironmentStore) IgnoreEnvironmentSuggestion(context.Context, string) error {
	return f.err
}

func TestEnvironmentStoreFailuresAreGeneric(t *testing.T) {
	h := newEnvironmentHarness(t)
	captureStandardLog(t)
	h.server.Store = failingEnvironmentStore{Backend: h.st, err: errors.New("driver detail cv_env_envkey_Ab3dE6gH9jK2mN5p_Ab3dE6gH9jK2mN5pQ8sT1vW4y")}
	h.handler = h.server.Handler()
	requests := environmentMutations("env-000000-000000", "envkey_missing")
	for _, path := range environmentReads {
		requests = append(requests, struct{ method, path, body string }{http.MethodGet, path, ""})
	}
	for _, c := range requests {
		response := h.want(http.StatusInternalServerError, c.method, c.path, c.body)
		if strings.TrimSpace(response.Body.String()) != "internal server error" {
			t.Fatalf("%s %s body = %s", c.method, c.path, response.Body)
		}
	}
	// A store without environments answers 501 and reads nothing.
	h.server.Store = store.NewMemory()
	h.handler = h.server.Handler()
	for _, c := range requests {
		h.want(http.StatusNotImplemented, c.method, c.path, c.body)
	}
}
