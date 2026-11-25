package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// feedbackHarness drives the signal route through the full handler over the
// volatile backend, with a clock the test moves.
type feedbackHarness struct {
	*environmentHarness
	environment store.EnvironmentRecord
	key         store.IssuedEnvironmentKey

	mu  sync.Mutex
	now time.Time
}

const feedbackMinimalSignal = `{"next_signal_seconds":60,"services":[{"name":"api","versions":[{"version":"1.0"}]}]}`

func newFeedbackHarness(t *testing.T) *feedbackHarness {
	t.Helper()
	h := &feedbackHarness{environmentHarness: newEnvironmentHarness(t), now: time.Date(2026, 11, 25, 12, 0, 0, 123456789, time.UTC)}
	h.server.feedbackClock = h.clock
	h.environment = h.create(`{"name":"Production"}`)
	h.key = h.issue(h.environment.ID)
	return h
}

func (h *feedbackHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *feedbackHarness) advance(by time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(by)
}

// signal sends one body under a key value.
func (h *feedbackHarness) signal(value, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(value, http.MethodPost, environmentSignalPath, body)
}

// accept sends one body after the key's window has passed and requires 202.
func (h *feedbackHarness) accept(body string) *httptest.ResponseRecorder {
	h.t.Helper()
	h.advance(feedbackSignalWindow)
	response := h.signal(h.key.Value, body)
	if response.Code != http.StatusAccepted {
		h.t.Fatalf("signal status=%d body=%s, want 202", response.Code, response.Body)
	}
	return response
}

func (h *feedbackHarness) deploys() []store.FeedbackDeploy {
	h.t.Helper()
	deploys, err := h.st.ListFeedbackDeploys(h.ctx, h.environment.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return deploys
}

// stored renders everything a refused signal must leave unchanged: the
// environments with each key's last use, the events, and the feedback
// records of both workspaces.
func (h *feedbackHarness) stored() string {
	h.t.Helper()
	parts := []any{h.recorded()}
	for _, ctx := range []context.Context{h.ctx, h.betaCtx} {
		records, err := h.st.ListEnvironments(ctx, store.EnvironmentListQuery{})
		if err != nil {
			h.t.Fatal(err)
		}
		for _, record := range records {
			signals, err := h.st.ListFeedbackSignals(ctx, []string{record.ID})
			if err != nil {
				h.t.Fatal(err)
			}
			running, err := h.st.ListFeedbackRunningVersions(ctx, record.ID)
			if err != nil {
				h.t.Fatal(err)
			}
			deploys, err := h.st.ListFeedbackDeploys(ctx, record.ID)
			if err != nil {
				h.t.Fatal(err)
			}
			parts = append(parts, signals, running, deploys)
		}
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func (h *feedbackHarness) requireUnauthorized(response *httptest.ResponseRecorder, what string) {
	h.t.Helper()
	if response.Code != http.StatusUnauthorized || strings.TrimSpace(response.Body.String()) != "unauthorized" || response.Header().Get("WWW-Authenticate") != "Bearer" {
		h.t.Fatalf("%s: status=%d headers=%v body=%s, want the one 401", what, response.Code, response.Header(), response.Body)
	}
}

func feedbackJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// feedbackServices renders count services with distinct names.
func feedbackServices(t *testing.T, count int) string {
	var services []map[string]any
	for i := range count {
		services = append(services, map[string]any{"name": fmt.Sprintf("service-%d", i), "versions": []map[string]string{{"version": "1"}}})
	}
	return feedbackJSON(t, services)
}

func feedbackVersions(t *testing.T, count int) string {
	var versions []map[string]string
	for i := range count {
		versions = append(versions, map[string]string{"version": fmt.Sprintf("v%d", i)})
	}
	return feedbackJSON(t, versions)
}

func feedbackBody(interval, services string) string {
	return `{"next_signal_seconds":` + interval + `,"services":` + services + `}`
}

func feedbackService(members string) string { return feedbackBody("60", "[{"+members+"}]") }

func TestFeedbackSignalAccepted(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.advance(0)
	response := h.signal(h.key.Value, `{"next_signal_seconds":60,"services":[{"name":"api","repository":"app","versions":[{"commit":"ABCDEF1","version":"1.0"},{"commit":"abcdef1"}]},{"name":"web","versions":[{"version":" 2 "}]}]}`)
	if response.Code != http.StatusAccepted || !slices.Equal(environmentBodyKeys(t, response.Body.Bytes()), []string{"accepted_at"}) {
		t.Fatalf("status=%d body=%s, want 202 with accepted_at only", response.Code, response.Body)
	}
	var answer struct {
		AcceptedAt time.Time `json:"accepted_at"`
	}
	decodeEnvironmentBody(t, response, &answer)
	arrived := store.EnvironmentTime(h.clock())
	if !answer.AcceptedAt.Equal(arrived) {
		t.Fatalf("accepted_at=%v, want the arrival %v", answer.AcceptedAt, arrived)
	}
	origin := store.FeedbackOrigin{Source: "signal", Label: "deploy", Actor: core.SystemActorID}
	deploys := h.deploys()
	if len(deploys) != 3 {
		t.Fatalf("deploys=%+v, want three", deploys)
	}
	for i, want := range []store.FeedbackVersion{{Repository: "app", Commit: "abcdef1"}, {Repository: "app", Commit: "ABCDEF1", Version: "1.0"}, {Version: " 2 "}} {
		if deploys[i].FeedbackVersion != want || !deploys[i].DeployedAt.Equal(arrived) || deploys[i].Origin != origin || deploys[i].Resolved || deploys[i].StoppedAt != nil {
			t.Fatalf("deploy %d=%+v, want %+v at %v from %+v", i, deploys[i], want, arrived, origin)
		}
	}
	// The key's last use is recorded with the signal.
	records, err := h.st.ListEnvironmentRecords(h.ctx, store.EnvironmentListQuery{})
	if err != nil || records[0].Keys[0].LastUsedAt == nil {
		t.Fatalf("records=%+v err=%v, want a last use", records, err)
	}
	// A dropped version stops, at the arrival of the signal that drops it.
	h.accept(feedbackService(`"name":"api","repository":"app","versions":[{"commit":"abcdef1"}]`))
	deploys = h.deploys()
	if deploys[0].StoppedAt != nil || deploys[2].StoppedAt != nil || deploys[1].StoppedAt == nil || !deploys[1].StoppedAt.Equal(store.EnvironmentTime(h.clock())) || *deploys[1].StopOrigin != origin {
		t.Fatalf("deploys after a drop=%+v", deploys)
	}
}

// TestFeedbackSignalBounds covers each field and list bound of
// component-production-feedback invariant 3 on both sides. A refused signal
// changes no record and does not use the key's window.
func TestFeedbackSignalBounds(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	for name, body := range map[string]string{
		"least interval":          feedbackBody("10", feedbackServices(t, 1)),
		"greatest interval":       feedbackBody("86400", feedbackServices(t, 1)),
		"most services":           feedbackBody("60", feedbackServices(t, 200)),
		"shortest name":           feedbackService(`"name":"a","versions":[{"version":"1"}]`),
		"longest name":            feedbackService(`"name":"` + strings.Repeat("é", 100) + `","versions":[{"version":"1"}]`),
		"names that differ":       feedbackBody("60", `[{"name":"api","versions":[{"version":"1"}]},{"name":"API","versions":[{"version":"1"}]},{"name":"api ","versions":[{"version":"1"}]}]`),
		"most versions":           feedbackService(`"name":"a","versions":` + feedbackVersions(t, 10)),
		"shortest commit":         feedbackService(`"name":"a","versions":[{"commit":"abcdef0"}]`),
		"longest commit":          feedbackService(`"name":"a","versions":[{"commit":"` + strings.Repeat("aF", 20) + `"}]`),
		"shortest version":        feedbackService(`"name":"a","versions":[{"version":"1"}]`),
		"longest version":         feedbackService(`"name":"a","versions":[{"version":"` + strings.Repeat("é", 200) + `"}]`),
		"a digest as the version": feedbackService(`"name":"a","versions":[{"version":"sha256:` + strings.Repeat("ab", 32) + `"}]`),
		"a repository":            feedbackService(`"name":"a","repository":"app","versions":[{"commit":"abcdef0","version":"1"}]`),
		"escaped text":            feedbackService(`"name":"\ud83d\ude80\\ud800\u00e9\"","versions":[{"version":"\ufffd"}]`),
		"trailing whitespace":     feedbackMinimalSignal + " \n",
	} {
		t.Run(name, func(t *testing.T) {
			h.t = t
			h.accept(body)
		})
	}
	h.t = t

	h.advance(feedbackSignalWindow)
	before := h.stored()
	two := `"versions":[{"version":"1"}]`
	for _, c := range []struct{ name, field, body string }{
		{"no interval", "next_signal_seconds", `{"services":` + feedbackServices(t, 1) + `}`},
		{"null interval", "next_signal_seconds", feedbackBody("null", feedbackServices(t, 1))},
		{"low interval", "next_signal_seconds", feedbackBody("9", feedbackServices(t, 1))},
		{"high interval", "next_signal_seconds", feedbackBody("86401", feedbackServices(t, 1))},
		{"negative interval", "next_signal_seconds", feedbackBody("-60", feedbackServices(t, 1))},
		{"fractional interval", "next_signal_seconds", feedbackBody("60.5", feedbackServices(t, 1))},
		{"huge interval", "next_signal_seconds", feedbackBody("1e400", feedbackServices(t, 1))},
		{"text interval", "next_signal_seconds", feedbackBody(`"60"`, feedbackServices(t, 1))},
		{"no services", "services", `{"next_signal_seconds":60}`},
		{"null services", "services", feedbackBody("60", "null")},
		{"empty services", "services", feedbackBody("60", "[]")},
		{"services object", "services", feedbackBody("60", "{}")},
		{"too many services", "services", feedbackBody("60", feedbackServices(t, 201))},
		{"service text", "services[0]", feedbackBody("60", `["api"]`)},
		{"null service", "services[0]", feedbackBody("60", `[null]`)},
		{"no name", "services[0].name", feedbackService(two)},
		{"null name", "services[0].name", feedbackService(`"name":null,` + two)},
		{"empty name", "services[0].name", feedbackService(`"name":"",` + two)},
		{"number name", "services[0].name", feedbackService(`"name":7,` + two)},
		{"long name", "services[0].name", feedbackService(`"name":"` + strings.Repeat("é", 101) + `",` + two)},
		{"NUL name", "services[0].name", feedbackService(`"name":"a\u0000",` + two)},
		{"repeated service", "services[1].name", feedbackBody("60", `[{"name":"api",`+two+`},{"name":"api",`+two+`}]`)},
		{"null repository", "services[0].repository", feedbackService(`"name":"a","repository":null,` + two)},
		{"empty repository", "services[0].repository", feedbackService(`"name":"a","repository":"",` + two)},
		{"unknown repository", "services[0].repository", feedbackService(`"name":"a","repository":"ghost",` + two)},
		{"repository case", "services[0].repository", feedbackService(`"name":"a","repository":"App",` + two)},
		{"repository of another workspace", "services[0].repository", feedbackService(`"name":"a","repository":"elsewhere",` + two)},
		{"no versions", "services[0].versions", feedbackService(`"name":"a"`)},
		{"null versions", "services[0].versions", feedbackService(`"name":"a","versions":null`)},
		{"empty versions", "services[0].versions", feedbackService(`"name":"a","versions":[]`)},
		{"too many versions", "services[0].versions", feedbackService(`"name":"a","versions":` + feedbackVersions(t, 11))},
		{"version text", "services[0].versions[0]", feedbackService(`"name":"a","versions":["1"]`)},
		{"null version entry", "services[0].versions[0]", feedbackService(`"name":"a","versions":[null]`)},
		{"neither commit nor version", "services[0].versions[0]", feedbackService(`"name":"a","versions":[{}]`)},
		{"null commit", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":null,"version":"1"}]`)},
		{"empty commit", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":"","version":"1"}]`)},
		{"short commit", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":"abcdef"}]`)},
		{"long commit", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":"` + strings.Repeat("a", 41) + `"}]`)},
		{"commit that is not hexadecimal", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":"abcdefg"}]`)},
		{"number commit", "services[0].versions[0].commit", feedbackService(`"name":"a","versions":[{"commit":1234567}]`)},
		{"null version", "services[0].versions[0].version", feedbackService(`"name":"a","versions":[{"commit":"abcdef0","version":null}]`)},
		{"empty version", "services[0].versions[0].version", feedbackService(`"name":"a","versions":[{"commit":"abcdef0","version":""}]`)},
		{"long version", "services[0].versions[0].version", feedbackService(`"name":"a","versions":[{"version":"` + strings.Repeat("a", 201) + `"}]`)},
		{"later version entry", "services[0].versions[1].commit", feedbackService(`"name":"a","versions":[{"version":"1"},{"commit":"x"}]`)},
		{"extra field", "body", `{"accepted_at":"2020-01-01T00:00:00Z","next_signal_seconds":60,"services":` + feedbackServices(t, 1) + `}`},
		{"environment in the body", "body", `{"environment":"env","next_signal_seconds":60,"services":` + feedbackServices(t, 1) + `}`},
		{"field in another case", "body", `{"Next_signal_seconds":60,"services":` + feedbackServices(t, 1) + `}`},
		{"extra service field", "services[0]", feedbackService(`"name":"a","at":"now",` + two)},
		{"extra version field", "services[0].versions[0]", feedbackService(`"name":"a","versions":[{"version":"1","at":"now"}]`)},
		{"repeated field", "body", `{"next_signal_seconds":60,"next_signal_seconds":60,"services":` + feedbackServices(t, 1) + `}`},
		{"trailing JSON", "body", feedbackMinimalSignal + "{}"},
		{"trailing text", "body", feedbackMinimalSignal + "x"},
		{"empty body", "body", ""},
		{"null body", "body", "null"},
		{"array body", "body", "[]"},
		{"broken JSON", "body", `{"next_signal_seconds":`},
		{"half a surrogate pair in a name", "services[0].name", feedbackService(`"name":"a\ud800",` + two)},
		{"low half first in a version", "services[0].versions[0].version", feedbackService(`"name":"a","versions":[{"version":"\udc00\ud800"}]`)},
		{"half a pair before text", "services[0].versions[0].version", feedbackService(`"name":"a","versions":[{"version":"\ud83dx"}]`)},
		{"invalid UTF-8", "body", feedbackService("\"name\":\"a\xff\"," + two)},
	} {
		t.Run(c.name, func(t *testing.T) {
			response := h.signal(h.key.Value, c.body)
			var refusal map[string]string
			decodeEnvironmentBody(t, response, &refusal)
			if response.Code != http.StatusBadRequest || refusal["error"] != "invalid_signal" || refusal["field"] != c.field || refusal["message"] == "" || len(refusal) != 3 {
				t.Fatalf("status=%d body=%s, want invalid_signal naming %s", response.Code, response.Body, c.field)
			}
		})
	}
	if after := h.stored(); after != before {
		t.Fatalf("a refused signal changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// No refusal used the window, so the next signal is accepted at once.
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("after refusals: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackSignalRefusesSecrets proves that text shaped like a credential
// is refused, and that no refusal or log line repeats a submitted value
// (req-security-boundaries REQ-2, REQ-3).
func TestFeedbackSignalRefusesSecrets(t *testing.T) {
	h := newFeedbackHarness(t)
	logs := captureOAuthLogs(t)
	h.handler = h.server.Handler()
	before := h.stored()
	const token = "ghp_0123456789abcdefghij0123456789abcdefghij"
	for field, body := range map[string]string{
		"services[0].name":                feedbackService(`"name":"` + token + `","versions":[{"version":"1"}]`),
		"services[0].repository":          feedbackService(`"name":"a","repository":"` + token + `","versions":[{"version":"1"}]`),
		"services[0].versions[0].version": feedbackService(`"name":"a","versions":[{"version":"` + h.key.Value + `"}]`),
		"services[0].versions[0].commit":  feedbackService(`"name":"a","versions":[{"commit":"` + token + `"}]`),
		"services[0]":                     feedbackService(`"name":"a","` + token + `":1,"versions":[{"version":"1"}]`),
		"body":                            `{"` + token + `":1}`,
	} {
		response := h.signal(h.key.Value, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"`+field+`"`) {
			t.Fatalf("%s: status=%d body=%s", field, response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), token) || strings.Contains(response.Body.String(), h.key.Value) {
			t.Fatalf("the refusal of %s repeats the value", field)
		}
	}
	if h.stored() != before {
		t.Fatal("a refused secret changed a record")
	}
	if text := logs.String(); !strings.Contains(text, environmentSignalPath) || strings.Contains(text, token) || strings.Contains(text, h.key.Value) {
		t.Fatal("the access log is missing or holds a submitted value")
	}
}

func TestFeedbackSignalBodyLimit(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	fit := feedbackMinimalSignal + strings.Repeat(" ", feedbackSignalBodyLimit-len(feedbackMinimalSignal))
	over := fit + " "
	send := func(body io.Reader, length int64) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, environmentSignalPath, body)
		request.ContentLength = length
		request.Header.Set("Authorization", "Bearer "+h.key.Value)
		return h.serve(request)
	}
	before := h.stored()
	for name, length := range map[string]int64{"known length": int64(len(over)), "unknown length": -1} {
		// The wrapper hides the reader's length from the request.
		if response := send(io.MultiReader(strings.NewReader(over)), length); response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s over the limit: status=%d body=%s", name, response.Code, response.Body)
		}
	}
	if h.stored() != before {
		t.Fatal("a body over the limit changed a record")
	}
	for name, length := range map[string]int64{"known length": int64(len(fit)), "unknown length": -1} {
		if response := send(io.MultiReader(strings.NewReader(fit)), length); response.Code != http.StatusAccepted {
			t.Fatalf("%s at the limit: status=%d body=%s", name, response.Code, response.Body)
		}
		h.advance(feedbackSignalWindow)
	}
}

// TestFeedbackSignalCredentials proves component-production-feedback
// invariant 2 and req-security-boundaries AC-1.5: one live key of an
// unarchived environment is the only credential, and every other one gets one
// answer and records nothing.
func TestFeedbackSignalCredentials(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	revoked := h.issue(h.environment.ID)
	h.want(http.StatusNoContent, http.MethodDelete, "/v1/environments/"+h.environment.ID+"/keys/"+revoked.ID, "")
	old := h.create(`{"name":"Old"}`)
	archived := h.issue(old.ID)
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+old.ID, `{"archived":true}`)
	before := h.stored()
	for name, headers := range map[string][]string{
		"no credential":             nil,
		"operator token":            {"Bearer " + h.manager()},
		"viewer token":              {"Bearer " + h.tokens[core.WorkspaceRoleViewer]},
		"unknown token":             {"Bearer unknown"},
		"bare prefix":               {"Bearer cv_env_"},
		"unknown key":               {"Bearer cv_env_envkey_unknown_unknown"},
		"altered key":               {"Bearer " + h.key.Value + "x"},
		"truncated key":             {"Bearer " + h.key.Value[:len(h.key.Value)-1]},
		"key ID":                    {"Bearer " + h.key.ID},
		"revoked key":               {"Bearer " + revoked.Value},
		"key of an archived record": {"Bearer " + archived.Value},
		"another scheme":            {"Basic " + h.key.Value},
		"no scheme":                 {h.key.Value},
		"key beside a user token":   {"Bearer " + h.key.Value, "Bearer " + h.manager()},
		"user token beside a key":   {"Bearer " + h.manager(), "Bearer " + h.key.Value},
		"OAuth token":               {"Bearer cv_oat_0123456789abcdef_0123456789abcdef"},
	} {
		for _, cookie := range []bool{false, true} {
			request := httptest.NewRequest(http.MethodPost, "http://conveyor.example"+environmentSignalPath, strings.NewReader(feedbackMinimalSignal))
			request.Header["Authorization"] = headers
			request.Header.Set("X-Workspace-ID", "alpha")
			if cookie {
				request.Header.Set("X-Conveyor-CSRF", "1")
				request.Header.Set("Origin", oauthTestIssuer)
				request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: h.session})
			}
			response := h.serve(request)
			if response.Code != http.StatusUnauthorized || strings.TrimSpace(response.Body.String()) != "unauthorized" {
				t.Fatalf("%s cookie=%v: status=%d body=%s, want 401", name, cookie, response.Code, response.Body)
			}
		}
	}
	if after := h.stored(); after != before {
		t.Fatalf("a refused credential changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// A restored environment's key is admitted again.
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+old.ID, `{"archived":false}`)
	if response := h.signal(archived.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("restored key: status=%d body=%s", response.Code, response.Body)
	}
	// The route takes POST only. The key is refused before routing on every
	// other method (TestEnvironmentKeyIsRefusedOnEveryRoute).
	if response := h.do(h.manager(), http.MethodGet, environmentSignalPath, ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET with a user token: status=%d", response.Code)
	}
}

// TestFeedbackSignalScopeIsTheKeys proves that no header, query, or body
// chooses the workspace or the environment.
func TestFeedbackSignalScopeIsTheKeys(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	other := h.create(`{"name":"Staging"}`)
	beta, err := h.st.CreateEnvironment(h.betaCtx, store.EnvironmentInput{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, environmentSignalPath+"?workspace=beta&workspace_id=beta&environment="+other.ID+"&environment_id="+other.ID, strings.NewReader(feedbackMinimalSignal))
	request.Header.Set("Authorization", "Bearer "+h.key.Value)
	for _, name := range []string{"X-Workspace-ID", "X-Conveyor-Workspace", "X-Environment-ID"} {
		request.Header.Set(name, "beta")
	}
	if response := h.serve(request); response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if deploys := h.deploys(); len(deploys) != 1 {
		t.Fatalf("deploys of the key's environment=%+v", deploys)
	}
	for ctx, id := range map[context.Context]string{h.ctx: other.ID, h.betaCtx: beta.ID} {
		if deploys, err := h.st.ListFeedbackDeploys(ctx, id); err != nil || len(deploys) != 0 {
			t.Fatalf("deploys of %s=%+v err=%v", id, deploys, err)
		}
	}
	// Each workspace's key sees its own repositories.
	issued, err := h.st.IssueEnvironmentKey(h.betaCtx, beta.ID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if response := h.signal(issued.Value, feedbackService(`"name":"a","repository":"app","versions":[{"version":"1"}]`)); response.Code != http.StatusBadRequest {
		t.Fatalf("foreign repository: status=%d", response.Code)
	}
	if response := h.signal(issued.Value, feedbackService(`"name":"a","repository":"elsewhere","versions":[{"version":"1"}]`)); response.Code != http.StatusAccepted {
		t.Fatalf("own repository: status=%d body=%s", response.Code, response.Body)
	}
}

func TestFeedbackSignalWindow(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	second := h.issue(h.environment.ID)
	if second.Label != h.key.Label {
		t.Fatalf("labels %q and %q, want one label", h.key.Label, second.Label)
	}
	h.accept(feedbackMinimalSignal)
	before := h.stored()
	for _, c := range []struct {
		after time.Duration
		retry string
	}{{0, "5"}, {time.Millisecond, "5"}, {999 * time.Millisecond, "4"}, {3500 * time.Millisecond, "1"}, {499 * time.Millisecond, "1"}} {
		h.advance(c.after)
		response := h.signal(h.key.Value, feedbackService(`"name":"new","versions":[{"version":"1"}]`))
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != c.retry {
			t.Fatalf("after %v: status=%d Retry-After=%q, want 429 and %s", c.after, response.Code, response.Header().Get("Retry-After"), c.retry)
		}
	}
	if after := h.stored(); after != before {
		t.Fatalf("a refused signal changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// Another key with the same label has its own window.
	if response := h.signal(second.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("second key: status=%d body=%s", response.Code, response.Body)
	}
	// The window ends at exactly five seconds, and a refusal never moved it.
	h.advance(time.Millisecond)
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("at the boundary: status=%d body=%s", response.Code, response.Body)
	}
}

// feedbackSignalStore replaces the signal write and the repository check, each
// when it is set.
type feedbackSignalStore struct {
	store.Backend
	record func(context.Context, store.FeedbackSignal) error
	check  func(context.Context, store.FeedbackSignal) error
}

func (f feedbackSignalStore) CheckFeedbackRepositories(ctx context.Context, signal store.FeedbackSignal) error {
	if f.check == nil {
		return f.Backend.CheckFeedbackRepositories(ctx, signal)
	}
	return f.check(ctx, signal)
}

func (f feedbackSignalStore) RecordFeedbackSignal(ctx context.Context, signal store.FeedbackSignal) error {
	if f.record == nil {
		return f.Backend.RecordFeedbackSignal(ctx, signal)
	}
	return f.record(ctx, signal)
}

const (
	feedbackGhostSignal = `{"next_signal_seconds":60,"services":[{"name":"api","versions":[{"version":"1"}]},{"name":"ghost","repository":"ghost","versions":[{"version":"1"}]}]}`
	feedbackAppSignal   = `{"next_signal_seconds":60,"services":[{"name":"api","repository":"app","versions":[{"version":"1"}]}]}`
)

// use puts a store in front of the harness's backend.
func (h *feedbackHarness) use(st store.Backend) {
	h.server.Store = st
	h.handler = h.server.Handler()
}

// send serves one signal of the harness's key and can run in any goroutine.
func (h *feedbackHarness) send(body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, environmentSignalPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+h.key.Value)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func (h *feedbackHarness) requireGhostRefusal(response *httptest.ResponseRecorder, what string) {
	h.t.Helper()
	var refusal map[string]string
	if response.Code != http.StatusBadRequest {
		h.t.Fatalf("%s: status=%d body=%s, want 400", what, response.Code, response.Body)
	}
	decodeEnvironmentBody(h.t, response, &refusal)
	if refusal["error"] != "invalid_signal" || refusal["field"] != "services[1].repository" || response.Header().Get("Retry-After") != "" || strings.Contains(response.Body.String(), "ghost") {
		h.t.Fatalf("%s: headers=%v body=%s, want invalid_signal naming the repository", what, response.Header(), response.Body)
	}
}

// TestFeedbackSignalRepositoryInsideWindow proves that a repository outside
// the workspace is refused as invalid inside a live window, beside a full
// limiter, and without a change to either (component-production-feedback
// invariant 3).
func TestFeedbackSignalRepositoryInsideWindow(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.server.feedbackLimiter = newFeedbackLimiter(1)
	second := h.issue(h.environment.ID)
	h.accept(feedbackMinimalSignal)
	before := h.stored()
	for _, after := range []time.Duration{0, time.Second, 3999 * time.Millisecond} {
		h.advance(after)
		h.requireGhostRefusal(h.send(feedbackGhostSignal), "inside a live window")
		if response := h.send(feedbackAppSignal); response.Code != http.StatusTooManyRequests {
			t.Fatalf("a valid signal inside the window: status=%d", response.Code)
		}
		h.requireGhostRefusal(h.signal(second.Value, feedbackGhostSignal), "beside a full limiter")
	}
	if after := h.stored(); after != before {
		t.Fatalf("a refused signal changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// The refusals did not move the end of the window.
	h.advance(time.Millisecond)
	if response := h.send(feedbackAppSignal); response.Code != http.StatusAccepted {
		t.Fatalf("at the boundary: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackSignalRepositoryBesideValidSignal holds an invalid signal and a
// valid one of the same key open at once, in both orders. Neither answer
// depends on the other.
func TestFeedbackSignalRepositoryBesideValidSignal(t *testing.T) {
	t.Parallel()
	t.Run("invalid first", func(t *testing.T) {
		t.Parallel()
		h := newFeedbackHarness(t)
		entered, release := make(chan struct{}), make(chan struct{})
		h.use(feedbackSignalStore{Backend: h.st, check: func(ctx context.Context, signal store.FeedbackSignal) error {
			err := h.st.CheckFeedbackRepositories(ctx, signal)
			if err != nil {
				close(entered)
				<-release
			}
			return err
		}})
		invalid := make(chan *httptest.ResponseRecorder, 1)
		go func() { invalid <- h.send(feedbackGhostSignal) }()
		select {
		case <-entered:
		case response := <-invalid:
			t.Fatalf("the invalid signal was answered before its check: status=%d body=%s", response.Code, response.Body)
		}
		valid := h.send(feedbackAppSignal)
		close(release)
		h.requireGhostRefusal(<-invalid, "beside a valid signal")
		if valid.Code != http.StatusAccepted {
			t.Fatalf("valid signal beside an invalid one: status=%d body=%s", valid.Code, valid.Body)
		}
		if deploys := h.deploys(); len(deploys) != 1 || deploys[0].Repository != "app" {
			t.Fatalf("deploys=%+v, want the valid signal's", deploys)
		}
	})
	t.Run("valid first", func(t *testing.T) {
		t.Parallel()
		h := newFeedbackHarness(t)
		entered, release := make(chan struct{}), make(chan struct{})
		h.use(feedbackSignalStore{Backend: h.st, record: func(ctx context.Context, signal store.FeedbackSignal) error {
			close(entered)
			<-release
			return h.st.RecordFeedbackSignal(ctx, signal)
		}})
		valid := make(chan *httptest.ResponseRecorder, 1)
		go func() { valid <- h.send(feedbackAppSignal) }()
		<-entered
		invalid := h.send(feedbackGhostSignal)
		close(release)
		if response := <-valid; response.Code != http.StatusAccepted {
			t.Fatalf("valid signal: status=%d body=%s", response.Code, response.Body)
		}
		h.requireGhostRefusal(invalid, "while a valid signal is in the store")
	})
}

// TestFeedbackSignalRepositoryRemovedAfterCheck proves that the store still
// refuses a repository that leaves the configuration after the check, and
// that neither that refusal nor a failed check opens a window.
func TestFeedbackSignalRepositoryRemovedAfterCheck(t *testing.T) {
	h := newFeedbackHarness(t)
	captureStandardLog(t)
	before := h.stored()
	h.use(feedbackSignalStore{Backend: h.st, check: func(context.Context, store.FeedbackSignal) error { return nil }})
	h.requireGhostRefusal(h.send(feedbackGhostSignal), "after the check")
	h.use(feedbackSignalStore{Backend: h.st, check: func(context.Context, store.FeedbackSignal) error { return errors.New("driver detail") }})
	if response := h.send(feedbackAppSignal); response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
		t.Fatalf("failed check: status=%d body=%s", response.Code, response.Body)
	}
	if h.stored() != before {
		t.Fatal("a refused signal changed a record")
	}
	h.use(h.st)
	if response := h.send(feedbackAppSignal); response.Code != http.StatusAccepted {
		t.Fatalf("after the refusals: status=%d body=%s", response.Code, response.Body)
	}
}

func TestFeedbackSignalStoreFailure(t *testing.T) {
	h := newFeedbackHarness(t)
	logs := captureStandardLog(t)
	before := h.stored()
	failure := errors.New("driver detail " + h.key.Value)
	h.server.Store = feedbackSignalStore{Backend: h.st, record: func(context.Context, store.FeedbackSignal) error { return failure }}
	h.handler = h.server.Handler()
	for range 2 {
		response := h.signal(h.key.Value, feedbackMinimalSignal)
		if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
			t.Fatalf("status=%d body=%s, want the generic 500", response.Code, response.Body)
		}
	}
	if strings.Contains(logs.String(), h.key.Value) {
		t.Fatal("the log holds the key value")
	}
	// A key that loses its validity after the check gets the one refusal.
	failure = store.ErrInvalidEnvironmentKey
	h.requireUnauthorized(h.signal(h.key.Value, feedbackMinimalSignal), "a key revoked during the request")
	if h.stored() != before {
		t.Fatal("a failed signal changed a record")
	}
	// A failure opens no window, so the retry at the same time is accepted.
	h.server.Store = h.st
	h.handler = h.server.Handler()
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("retry: status=%d body=%s", response.Code, response.Body)
	}
	// A failure inside a later window keeps the earlier window's record.
	h.advance(feedbackSignalWindow)
	h.server.Store = feedbackSignalStore{Backend: h.st, record: func(context.Context, store.FeedbackSignal) error { return failure }}
	h.handler = h.server.Handler()
	h.requireUnauthorized(h.signal(h.key.Value, feedbackMinimalSignal), "a later failure")
	h.server.Store = h.st
	h.handler = h.server.Handler()
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("later retry: status=%d body=%s", response.Code, response.Body)
	}
}

type failingFeedbackKeyStore struct{ store.Backend }

func (failingFeedbackKeyStore) CheckEnvironmentKey(context.Context, string) (store.VerifiedEnvironmentKey, error) {
	return store.VerifiedEnvironmentKey{}, errors.New("driver detail")
}

func (failingFeedbackKeyStore) ListFeedbackSignals(context.Context, []string) (map[string]store.FeedbackSignalState, error) {
	return nil, errors.New("driver detail")
}

func TestFeedbackReadFailuresAreGeneric(t *testing.T) {
	h := newFeedbackHarness(t)
	captureStandardLog(t)
	h.server.Store = failingFeedbackKeyStore{Backend: h.st}
	h.handler = h.server.Handler()
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "credential verification failed" {
		t.Fatalf("verification failure: status=%d body=%s", response.Code, response.Body)
	}
	if response := h.do(h.manager(), http.MethodGet, "/v1/environments", ""); response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
		t.Fatalf("list failure: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackSignalConcurrentAdmission holds one signal in the store while
// others of the same key arrive. Exactly one is accepted.
func TestFeedbackSignalConcurrentAdmission(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h.server.Store = feedbackSignalStore{Backend: h.st, record: func(ctx context.Context, signal store.FeedbackSignal) error {
		close(entered)
		<-release
		return h.st.RecordFeedbackSignal(ctx, signal)
	}}
	h.handler = h.server.Handler()
	first := make(chan *httptest.ResponseRecorder, 1)
	send := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, environmentSignalPath, strings.NewReader(feedbackMinimalSignal))
		request.Header.Set("Authorization", "Bearer "+h.key.Value)
		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, request)
		return response
	}
	go func() { first <- send() }()
	<-entered
	others := make(chan *httptest.ResponseRecorder, 8)
	for range cap(others) {
		go func() { others <- send() }()
	}
	for range cap(others) {
		if response := <-others; response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "5" {
			t.Errorf("concurrent signal: status=%d Retry-After=%q", response.Code, response.Header().Get("Retry-After"))
		}
	}
	close(release)
	if response := <-first; response.Code != http.StatusAccepted {
		t.Fatalf("first signal: status=%d body=%s", response.Code, response.Body)
	}
	if deploys := h.deploys(); len(deploys) != 1 {
		t.Fatalf("deploys=%+v, want one", deploys)
	}
}

func TestFeedbackLimiter(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	key := func(id string) feedbackLimiterKey { return feedbackLimiterKey{workspace: "alpha", keyID: id} }
	l := newFeedbackLimiter(2)
	admit := func(id string, arrived time.Time) {
		t.Helper()
		if wait, full := l.reserve(key(id), arrived); wait != 0 || full {
			t.Fatalf("%s at %v: wait=%v full=%v", id, arrived, wait, full)
		}
	}
	admit("one", at)
	admit("two", at)
	// Pending entries are never forgotten, whatever the time.
	if _, full := l.reserve(key("three"), at.Add(time.Hour)); !full {
		t.Fatal("a pending entry was forgotten")
	}
	l.settle(key("one"), at, true)
	l.settle(key("two"), at, true)
	// Live windows are never forgotten.
	if _, full := l.reserve(key("three"), at.Add(feedbackSignalWindow-time.Nanosecond)); !full || len(l.entries) != 2 {
		t.Fatalf("a live window was forgotten: full=%v entries=%v", full, l.entries)
	}
	// A held key is judged by its own window while the limiter is full.
	if wait, full := l.reserve(key("one"), at.Add(time.Second)); wait != 4*time.Second || full {
		t.Fatalf("held key: wait=%v full=%v", wait, full)
	}
	// A signal that arrived before the accepted one waits at most the window.
	if wait, _ := l.reserve(key("one"), at.Add(-time.Minute)); wait != feedbackSignalWindow {
		t.Fatalf("earlier arrival: wait=%v", wait)
	}
	// Ended windows make room.
	admit("three", at.Add(feedbackSignalWindow))
	if len(l.entries) != 1 {
		t.Fatalf("entries=%v, want the new key only", l.entries)
	}
	// A failed first signal leaves nothing.
	l.settle(key("three"), at.Add(feedbackSignalWindow), false)
	if len(l.entries) != 0 {
		t.Fatalf("entries=%v, want none", l.entries)
	}
	// The same key ID of another workspace is another key.
	admit("one", at)
	if wait, full := l.reserve(feedbackLimiterKey{workspace: "beta", keyID: "one"}, at); wait != 0 || full {
		t.Fatalf("another workspace: wait=%v full=%v", wait, full)
	}
	// The bound holds under concurrent reservations.
	l = newFeedbackLimiter(8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if wait, full := l.reserve(key(fmt.Sprint(i)), at); wait == 0 && !full {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 8 || len(l.entries) != 8 {
		t.Fatalf("admitted=%d entries=%d, want the bound 8", admitted, len(l.entries))
	}
}

// TestFeedbackLimiterFullAnswers proves the route's answer when every held
// key is inside its window, and that room returns when a window ends.
func TestFeedbackLimiterFullAnswers(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.server.feedbackLimiter = newFeedbackLimiter(1)
	second := h.issue(h.environment.ID)
	h.accept(feedbackMinimalSignal)
	before := h.stored()
	response := h.signal(second.Value, feedbackMinimalSignal)
	if response.Code != http.StatusServiceUnavailable || strings.TrimSpace(response.Body.String()) != "temporarily unavailable" || h.stored() != before {
		t.Fatalf("full limiter: status=%d body=%s", response.Code, response.Body)
	}
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusTooManyRequests {
		t.Fatalf("the live window: status=%d", response.Code)
	}
	h.advance(feedbackSignalWindow)
	if response := h.signal(second.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("after the window: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackListening covers component-production-feedback invariant 13 on
// GET /v1/environments (req-production-feedback AC-2.1).
func TestFeedbackListening(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	quiet := h.create(`{"name":"Quiet"}`)
	type row struct {
		ID           string     `json:"id"`
		Listening    *bool      `json:"listening"`
		LastSignalAt *time.Time `json:"last_signal_at"`
	}
	list := func(query string) map[string]row {
		t.Helper()
		response := h.want(http.StatusOK, http.MethodGet, "/v1/environments"+query, "")
		var raw []json.RawMessage
		decodeEnvironmentBody(t, response, &raw)
		rows := map[string]row{}
		want := slices.Sorted(slices.Values(append([]string{"last_signal_at", "listening"}, environmentRowFields...)))
		for _, entry := range raw {
			if got := environmentBodyKeys(t, entry); !slices.Equal(got, want) {
				t.Fatalf("row fields=%v, want %v", got, want)
			}
			var r row
			if err := json.Unmarshal(entry, &r); err != nil || r.Listening == nil {
				t.Fatalf("row %s: %v", entry, err)
			}
			rows[r.ID] = r
		}
		return rows
	}
	require := func(listening bool, last *time.Time, what string) {
		t.Helper()
		r := list("")[h.environment.ID]
		if *r.Listening != listening || (r.LastSignalAt == nil) != (last == nil) || (last != nil && !r.LastSignalAt.Equal(*last)) {
			t.Fatalf("%s: listening=%v last=%v, want %v %v", what, *r.Listening, r.LastSignalAt, listening, last)
		}
	}
	require(false, nil, "before the first signal")
	h.accept(feedbackBody("10", feedbackServices(t, 1)))
	last := store.EnvironmentTime(h.clock())
	require(true, &last, "at the signal")
	if r := list("")[quiet.ID]; *r.Listening || r.LastSignalAt != nil {
		t.Fatalf("another environment=%+v", r)
	}
	h.advance(20*time.Second - time.Microsecond)
	require(true, &last, "just before twice the interval")
	h.advance(time.Microsecond)
	require(false, &last, "at twice the interval")
	// A refused signal restores nothing.
	h.signal(h.key.Value, `{}`)
	require(false, &last, "after a refused signal")
	// A later signal sets its own interval.
	h.accept(feedbackBody("600", feedbackServices(t, 1)))
	last = store.EnvironmentTime(h.clock())
	h.advance(1199 * time.Second)
	require(true, &last, "inside a longer interval")
	h.accept(feedbackBody("10", feedbackServices(t, 1)))
	last = store.EnvironmentTime(h.clock())
	h.advance(20 * time.Second)
	require(false, &last, "after a shorter interval")
	// The archive filter and the page are unchanged, and an archived
	// environment keeps its last signal.
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+h.environment.ID, `{"archived":true}`)
	if rows := list(""); len(rows) != 1 || rows[quiet.ID].ID == "" {
		t.Fatalf("unarchived rows=%v", rows)
	}
	if rows := list("?archived=true"); len(rows) != 1 || !rows[h.environment.ID].LastSignalAt.Equal(last) {
		t.Fatalf("archived rows=%v", rows)
	}
	if rows := list("?limit=1&offset=1"); len(rows) != 0 {
		t.Fatalf("page=%v", rows)
	}
	// The other environment answers keep their fields.
	if got := environmentBodyKeys(t, h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+quiet.ID, `{}`).Body.Bytes()); !slices.Equal(got, environmentRowFields) {
		t.Fatalf("PATCH row fields=%v", got)
	}
}

// TestFeedbackSignalObservesOnly proves component-production-feedback
// invariant 1 (req-production-feedback REQ-11; DEC-149(1)): a signal changes
// no task, work order, event, Inbox item, or attention count.
func TestFeedbackSignalObservesOnly(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	if err := h.st.CreateTask(h.ctx, core.Task{ID: "feedback-task", Workspace: "alpha", Repo: "app", Title: "Task", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	observe := func() string {
		t.Helper()
		var parts []string
		for _, path := range []string{"/v1/tasks", "/v1/tasks/feedback-task", "/v1/tasks/feedback-task/events", "/v1/work-orders", "/v1/attention/tasks", "/v1/pending-proposals", "/v1/inbox", "/v1/next-actions"} {
			response := h.want(http.StatusOK, http.MethodGet, path, "")
			parts = append(parts, path+" "+response.Body.String())
		}
		events, err := h.st.ListEvents(h.ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return feedbackJSON(t, []any{parts, events})
	}
	before := observe()
	h.accept(feedbackService(`"name":"api","repository":"app","versions":[{"commit":"abcdef0"}]`))
	h.accept(feedbackService(`"name":"api","repository":"app","versions":[{"commit":"abcdef1"}]`))
	if len(h.deploys()) != 2 {
		t.Fatal("the signals recorded no deploy")
	}
	if after := observe(); after != before {
		t.Fatalf("a signal changed an observed read:\nbefore %s\nafter  %s", before, after)
	}
}
