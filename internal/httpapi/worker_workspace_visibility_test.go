package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// These tests bring the three agent work-order routes (pull-request-template,
// submit-for-review, context-refresh) under the workspace not-found rule: a
// workspace the caller holds no binding in answers exactly what a nonexistent
// workspace answers, never 403 or 409 (req-accounts-and-membership AC-4.2;
// DEC-19; component-http-api "Workspace resolution and not-found semantics").

const (
	visibilityOrder   = "vis-implement"
	visibilitySession = "session"
	visibilityHead    = "named-head"
	missingWorkspace  = "does-not-exist"
)

type visibilityFixture struct {
	st                store.Store
	handler           http.Handler
	server            *Server
	members           *membershipFixture
	workerCredential  string
	reads, writes     int
	submissionPRCalls int
}

// newVisibilityFixture claims one implement order in workspace alpha. The
// claimant is the named user's task-run claimant, or an enrolled alpha worker
// when claimant is "worker". usr_member and usr_viewer are bound only to
// alpha; usr_owner, who owns the run child, is bound to alpha and beta.
func newVisibilityFixture(t *testing.T, claimant string) *visibilityFixture {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "alpha")
	st := store.NewMemory()
	task := core.Task{ID: "vis", Workspace: "alpha", Repo: "app", Title: "Visibility", Branch: "conveyor/task-vis", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: visibilityOrder, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	f := &visibilityFixture{st: st}
	cfg := &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: "app", URL: "https://github.com/acme/app.git", GitHub: "acme/app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	orders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		SubmissionPR: func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
			f.submissionPRCalls++
			pr := githubtrigger.SubmissionPullRequest{Number: 7, URL: "https://github.com/acme/app/pull/7"}
			pr.Head.SHA, pr.Head.Ref = visibilityHead, task.Branch
			pr.Base.SHA, pr.Base.Ref = "base-sha", "main"
			return pr, nil
		},
		SubmissionChangedPaths: func(context.Context, *config.Config, core.Task) ([]string, error) {
			f.reads++
			return []string{"internal/change.go"}, nil
		},
		ReviewDiffBetween: func(context.Context, string, string, string) (string, error) { return "diff", nil },
		ReconcileSubmissionPR: func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error {
			f.writes++
			return nil
		},
		SubmissionPRWait: func(context.Context, time.Duration) error { return nil },
	}
	workers := &workerservice.Service{Store: st, WorkOrders: orders}
	claim := core.WorkOrderClaim{SessionID: visibilitySession, ClientToken: "claim-token", ClaimantID: core.TaskRunClaimantID(claimant), OwnerUserID: claimant, Lease: time.Hour}
	if claimant == "worker" {
		pairing, _, err := workers.IssuePairing(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "member-pat", OwnerUserID: "usr_member", Kind: core.CredentialUser}), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		enrollment, err := workers.Enroll(t.Context(), pairing, "worker")
		if err != nil {
			t.Fatal(err)
		}
		f.workerCredential = enrollment.Credential
		claim.ClaimantID, claim.WorkerID, claim.OwnerUserID = enrollment.Worker.ID, enrollment.Worker.ID, "usr_member"
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, claim); err != nil {
		t.Fatal(err)
	}
	f.members = &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}, {ID: "beta", Name: "Beta"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"usr_member": {"alpha": core.WorkspaceRoleExecutor},
			"usr_viewer": {"alpha": core.WorkspaceRoleViewer},
			"usr_owner":  {"alpha": core.WorkspaceRoleExecutor, "beta": core.WorkspaceRoleExecutor},
		},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = f.members, f.members
	server.WorkOrders = orders
	server.Workers = workers
	server.Credentials = staticCredentialVerifier{
		"member-pat": {ID: "pat_member", OwnerUserID: "usr_member", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"viewer-pat": {ID: "pat_viewer", OwnerUserID: "usr_viewer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"child-token": {ID: "agt_child", OwnerUserID: "usr_owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser,
			RunWorkspaceID: "alpha", RunWorkOrderID: visibilityOrder, RunSessionID: visibilitySession},
	}
	f.server = server
	f.handler = server.Handler()
	return f
}

type visibilityRoute struct {
	name string
	call func(prefix, workspace string) *http.Request
}

func visibilityRoutes(session, head string) []visibilityRoute {
	post := func(path string, body map[string]string) *http.Request {
		raw, _ := json.Marshal(body)
		return httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	}
	return []visibilityRoute{
		{"pull-request-template", func(prefix, workspace string) *http.Request {
			return httptest.NewRequest(http.MethodGet, prefix+visibilityOrder+"/pull-request-template?workspace_id="+url.QueryEscape(workspace)+"&session_id="+url.QueryEscape(session), nil)
		}},
		{"submit-for-review", func(prefix, workspace string) *http.Request {
			return post(prefix+visibilityOrder+"/submit-for-review", map[string]string{"workspace_id": workspace, "session_id": session, "head_sha": head})
		}},
		{"context-refresh", func(prefix, workspace string) *http.Request {
			return post(prefix+visibilityOrder+"/context-refresh", map[string]string{"workspace_id": workspace, "session_id": session})
		}},
	}
}

// visibilitySuccessRoutes orders the success controls so the read and the
// refresh run while the claim is live, before submission ends it.
func visibilitySuccessRoutes() []visibilityRoute {
	routes := visibilityRoutes(visibilitySession, visibilityHead)
	return []visibilityRoute{routes[0], routes[2], routes[1]}
}

func (f *visibilityFixture) serve(token string, request *http.Request) *httptest.ResponseRecorder {
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

type visibilityEffects struct {
	order                            core.WorkOrder
	task                             core.Task
	events, reads, writes, prObserve int
}

func (f *visibilityFixture) effects(t *testing.T) visibilityEffects {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "alpha")
	order, err := f.st.GetWorkOrder(ctx, visibilityOrder)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.st.GetTask(ctx, "vis")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.st.ListEvents(ctx, "vis")
	if err != nil {
		t.Fatal(err)
	}
	return visibilityEffects{order: order, task: task, events: len(events), reads: f.reads, writes: f.writes, prObserve: f.submissionPRCalls}
}

func assertNoVisibilityEffects(t *testing.T, label string, before, after visibilityEffects) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("%s wrote state: before=%+v after=%+v", label, before, after)
	}
}

// assertIdenticalNotFound requires both responses to be the canonical 404
// with byte-identical bodies and identical headers, so the pair discloses
// nothing about which workspace exists.
func assertIdenticalNotFound(t *testing.T, label string, unbound, missing *httptest.ResponseRecorder) {
	t.Helper()
	canonical := canonicalWorkspaceNotFoundBody()
	for _, item := range []struct {
		kind     string
		response *httptest.ResponseRecorder
	}{{"unbound", unbound}, {"missing", missing}} {
		if item.response.Code != http.StatusNotFound || item.response.Body.String() != canonical || item.response.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s %s status=%d content-type=%q body=%q", label, item.kind, item.response.Code, item.response.Header().Get("Content-Type"), item.response.Body.String())
		}
	}
	if !bytes.Equal(unbound.Body.Bytes(), missing.Body.Bytes()) || !reflect.DeepEqual(unbound.Header(), missing.Header()) {
		t.Fatalf("%s distinguishes workspaces: unbound=%v %q missing=%v %q", label, unbound.Header(), unbound.Body.String(), missing.Header(), missing.Body.String())
	}
}

func TestWorkerRoutesWorkspaceVisibilityNotFound(t *testing.T) {
	f := newVisibilityFixture(t, "usr_member")
	for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
		t.Run(route.name, func(t *testing.T) {
			before := f.effects(t)
			unbound := f.serve("member-pat", route.call("/v1/work-orders/", "beta"))
			missing := f.serve("member-pat", route.call("/v1/work-orders/", missingWorkspace))
			assertIdenticalNotFound(t, route.name, unbound, missing)
			assertNoVisibilityEffects(t, route.name, before, f.effects(t))
		})
	}
	// The same caller in its own workspace succeeds on every route, so the
	// refusals above come from visibility and not from a broken fixture.
	for _, route := range visibilitySuccessRoutes() {
		if response := f.serve("member-pat", route.call("/v1/work-orders/", "alpha")); response.Code != http.StatusOK {
			t.Fatalf("visible %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
	}
	if after := f.effects(t); after.order.State != core.WorkOrderSubmitted || after.reads != 1 || after.writes != 1 {
		t.Fatalf("visible submission not recorded: %+v", after)
	}
}

func TestWorkerRoutesRunChildWorkspaceVisibilityNotFound(t *testing.T) {
	f := newVisibilityFixture(t, "usr_owner")
	for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
		t.Run(route.name, func(t *testing.T) {
			before := f.effects(t)
			// The child's owner is a beta member, so beta resolves; only the
			// child's alpha binding refuses it.
			foreign := f.serve("child-token", route.call("/v1/work-orders/", "beta"))
			missing := f.serve("child-token", route.call("/v1/work-orders/", missingWorkspace))
			assertIdenticalNotFound(t, route.name, foreign, missing)
			assertNoVisibilityEffects(t, route.name, before, f.effects(t))
		})
	}
	for _, route := range visibilitySuccessRoutes() {
		if response := f.serve("child-token", route.call("/v1/work-orders/", "alpha")); response.Code != http.StatusOK {
			t.Fatalf("bound child %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
	}
}

func TestWorkerRoutesWorkerWorkspaceVisibilityNotFound(t *testing.T) {
	f := newVisibilityFixture(t, "worker")
	for _, prefix := range []string{"/v1/worker/work-orders/", "/v1/work-orders/"} {
		for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
			t.Run(prefix+route.name, func(t *testing.T) {
				before := f.effects(t)
				foreign := f.serve(f.workerCredential, route.call(prefix, "beta"))
				missing := f.serve(f.workerCredential, route.call(prefix, missingWorkspace))
				assertIdenticalNotFound(t, prefix+route.name, foreign, missing)
				assertNoVisibilityEffects(t, prefix+route.name, before, f.effects(t))
			})
		}
	}
	for _, route := range visibilitySuccessRoutes() {
		if response := f.serve(f.workerCredential, route.call("/v1/worker/work-orders/", "alpha")); response.Code != http.StatusOK {
			t.Fatalf("enrolled worker %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
	}
}

func TestWorkerRoutesCapabilityRefusalNotFound(t *testing.T) {
	f := newVisibilityFixture(t, "usr_viewer")
	// A viewer sees alpha but lacks claim_work: the refusal is the same 404 a
	// nonexistent workspace gets, with no downstream effect.
	for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
		t.Run("viewer/"+route.name, func(t *testing.T) {
			before := f.effects(t)
			refused := f.serve("viewer-pat", route.call("/v1/work-orders/", "alpha"))
			missing := f.serve("viewer-pat", route.call("/v1/work-orders/", missingWorkspace))
			assertIdenticalNotFound(t, route.name, refused, missing)
			assertNoVisibilityEffects(t, route.name, before, f.effects(t))
		})
	}

	direct := func(t *testing.T, server *Server, ctx context.Context) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/work-orders/"+visibilityOrder+"/pull-request-template?workspace_id=alpha&session_id=session", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", visibilityOrder)
		request = request.WithContext(context.WithValue(ctx, chi.RouteCtxKey, route))
		response := httptest.NewRecorder()
		server.getSubmissionTemplate(response, request)
		return response
	}
	memberCredential := core.AuthenticatedCredential{ID: "pat_member", OwnerUserID: "usr_member", Kind: core.CredentialUser}
	canonical := canonicalWorkspaceNotFoundBody()

	t.Run("template without credential", func(t *testing.T) {
		g := newVisibilityFixture(t, "usr_member")
		if response := direct(t, g.server, t.Context()); response.Code != http.StatusNotFound || response.Body.String() != canonical {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
	t.Run("template without membership authority", func(t *testing.T) {
		g := newVisibilityFixture(t, "usr_member")
		g.server.Memberships = nil
		if response := direct(t, g.server, store.WithCredential(t.Context(), memberCredential)); response.Code != http.StatusNotFound || response.Body.String() != canonical {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
	t.Run("injected claim_work refusal", func(t *testing.T) {
		g := newVisibilityFixture(t, "usr_member")
		// alpha stays listed for usr_member, but the authorizer now refuses
		// claim_work on every route.
		g.members.roles["usr_member"]["alpha"] = core.WorkspaceRoleViewer
		for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
			before := g.effects(t)
			refused := g.serve("member-pat", route.call("/v1/work-orders/", "alpha"))
			missing := g.serve("member-pat", route.call("/v1/work-orders/", missingWorkspace))
			assertIdenticalNotFound(t, route.name, refused, missing)
			assertNoVisibilityEffects(t, route.name, before, g.effects(t))
		}
	})
	t.Run("template authorization store failure is opaque 500", func(t *testing.T) {
		g := newVisibilityFixture(t, "usr_member")
		g.members.authorizeErrs = map[core.Capability]error{core.CapabilityClaimWork: errors.New("raw membership backend detail")}
		response := g.serve("member-pat", visibilityRoutes(visibilitySession, visibilityHead)[0].call("/v1/work-orders/", "alpha"))
		if response.Code != http.StatusInternalServerError || response.Body.String() != "internal server error\n" {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
}

func TestWorkOrderToolErrorClassification(t *testing.T) {
	canonical := canonicalWorkspaceNotFoundBody()
	for _, item := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"visibility", errWorkspaceNotVisible, http.StatusNotFound, canonical},
		{"wrapped visibility", fmt.Errorf("resolve: %w", errWorkspaceNotVisible), http.StatusNotFound, canonical},
		{"invalid argument", invalidToolArgument(errors.New("head_sha is required")), http.StatusBadRequest, "head_sha is required\n"},
		{"wrapped invalid argument", fmt.Errorf("refresh: %w", invalidToolArgument(errors.New("invalid prior_revision"))), http.StatusBadRequest, "refresh: invalid prior_revision\n"},
		{"claim lost", store.ErrWorkOrderClaimLost, http.StatusConflict, store.ErrWorkOrderClaimLost.Error() + "\n"},
		{"claim unauthorized", store.ErrWorkOrderClaimUnauthorized, http.StatusConflict, store.ErrWorkOrderClaimUnauthorized.Error() + "\n"},
		{"pull request lifecycle", errors.New("pull_request_head_mismatch: observed head differs"), http.StatusConflict, "pull_request_head_mismatch: observed head differs\n"},
		// Text alone never classifies: an untyped error that merely reads
		// like a visibility refusal or an argument error keeps 409.
		{"untyped visibility text", errors.New("workspace_not_found: workspace not found"), http.StatusConflict, "workspace_not_found: workspace not found\n"},
		{"untyped argument text", errors.New("head_sha is required"), http.StatusConflict, "head_sha is required\n"},
	} {
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeWorkOrderToolError(response, item.err)
			if response.Code != item.status || response.Body.String() != item.body {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
	if invalidToolArgument(nil) != nil {
		t.Fatal("nil cause must stay nil")
	}
}

func TestWorkerRoutesVisibleWorkspaceLifecycleConflicts(t *testing.T) {
	f := newVisibilityFixture(t, "usr_member")
	for _, route := range visibilityRoutes("another-session", visibilityHead) {
		before := f.effects(t)
		if response := f.serve("member-pat", route.call("/v1/work-orders/", "alpha")); response.Code != http.StatusConflict {
			t.Fatalf("wrong session %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
		assertNoVisibilityEffects(t, "wrong session "+route.name, before, f.effects(t))
	}
	for _, route := range visibilityRoutes(visibilitySession, visibilityHead) {
		request := route.call("/v1/work-orders/", "alpha")
		request.URL.Path = strings.Replace(request.URL.Path, visibilityOrder, "other-order", 1)
		if response := f.serve("member-pat", request); response.Code != http.StatusConflict {
			t.Fatalf("wrong order %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
	}

	// A run child on its own alpha workspace but naming a foreign session is a
	// confinement failure, not a visibility refusal.
	g := newVisibilityFixture(t, "usr_owner")
	for _, route := range visibilityRoutes("another-session", visibilityHead) {
		if response := g.serve("child-token", route.call("/v1/work-orders/", "alpha")); response.Code != http.StatusConflict {
			t.Fatalf("child wrong session %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
	}

	expired := newVisibilityFixture(t, "usr_member")
	ctx := store.WithWorkspace(t.Context(), "alpha")
	order, err := expired.st.GetWorkOrder(ctx, visibilityOrder)
	if err != nil {
		t.Fatal(err)
	}
	order.LeaseExpiresAt = time.Now().Add(-time.Minute)
	if err = storetest.For(expired.st).UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	for _, route := range visibilityRoutes(visibilitySession, visibilityHead)[1:] {
		before := expired.effects(t)
		if response := expired.serve("member-pat", route.call("/v1/work-orders/", "alpha")); response.Code != http.StatusConflict {
			t.Fatalf("expired claim %s status=%d body=%s", route.name, response.Code, response.Body.String())
		}
		assertNoVisibilityEffects(t, "expired "+route.name, before, expired.effects(t))
	}

	// Remaining controls: unauthenticated 401 and template-unavailable 503.
	if response := f.serve("unknown-token", visibilityRoutes(visibilitySession, visibilityHead)[0].call("/v1/work-orders/", "alpha")); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
	f.server.WorkOrders = nil
	if response := f.serve("member-pat", visibilityRoutes(visibilitySession, visibilityHead)[0].call("/v1/work-orders/", "alpha")); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable template status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerRoutesArgumentErrors(t *testing.T) {
	f := newVisibilityFixture(t, "usr_member")
	post := func(path, body string) *httptest.ResponseRecorder {
		return f.serve("member-pat", httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	}
	submit := "/v1/work-orders/" + visibilityOrder + "/submit-for-review"
	refresh := "/v1/work-orders/" + visibilityOrder + "/context-refresh"
	for _, item := range []struct {
		name, path, body string
	}{
		{"submit malformed json", submit, `{"workspace_id":`},
		{"submit missing head", submit, `{"workspace_id":"alpha","session_id":"session"}`},
		{"submit blank head", submit, `{"workspace_id":"alpha","session_id":"session","head_sha":"   "}`},
		{"refresh malformed json", refresh, `{"workspace_id":`},
		{"refresh missing session", refresh, `{"workspace_id":"alpha"}`},
		{"refresh blank session", refresh, `{"workspace_id":"alpha","session_id":"  "}`},
		{"refresh missing workspace", refresh, `{"session_id":"session"}`},
		{"refresh invalid prior", refresh, `{"workspace_id":"alpha","session_id":"session","prior_revision":"bad"}`},
		{"refresh unknown field", refresh, `{"workspace_id":"alpha","session_id":"session","actor":"injected"}`},
		{"refresh duplicate field", refresh, `{"workspace_id":"alpha","workspace_id":"beta","session_id":"session"}`},
		{"refresh oversized", refresh, `{"workspace_id":"alpha","session_id":"session"` + strings.Repeat(" ", 8193) + `}`},
	} {
		t.Run(item.name, func(t *testing.T) {
			before := f.effects(t)
			if response := post(item.path, item.body); response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			assertNoVisibilityEffects(t, item.name, before, f.effects(t))
		})
	}
	// A missing head is classified only after workspace and claimant
	// admission: an invisible workspace still answers 404 and a foreign
	// session still answers 409.
	if response := post(submit, `{"workspace_id":"beta","session_id":"session"}`); response.Code != http.StatusNotFound || response.Body.String() != canonicalWorkspaceNotFoundBody() {
		t.Fatalf("invisible missing head status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(submit, `{"workspace_id":"alpha","session_id":"another-session"}`); response.Code != http.StatusConflict {
		t.Fatalf("foreign-session missing head status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestContextRefreshRESTArgumentBoundaries(t *testing.T) {
	f := newVisibilityFixture(t, "usr_member")
	refresh := func(body string) *httptest.ResponseRecorder {
		return f.serve("member-pat", httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+visibilityOrder+"/context-refresh", strings.NewReader(body)))
	}
	padded := func(size int) string {
		base := `{"workspace_id":"alpha","session_id":"session"`
		return base + strings.Repeat(" ", size-len(base)-1) + `}`
	}
	if body := padded(8192); len(body) != 8192 {
		t.Fatalf("at-limit body is %d bytes", len(body))
	} else if response := refresh(body); response.Code != http.StatusOK {
		t.Fatalf("8192-byte body status=%d body=%s", response.Code, response.Body.String())
	}
	if body := padded(8193); len(body) != 8193 {
		t.Fatalf("over-limit body is %d bytes", len(body))
	} else if response := refresh(body); response.Code != http.StatusBadRequest {
		t.Fatalf("8193-byte body status=%d body=%s", response.Code, response.Body.String())
	}

	// A 256-byte session passes argument admission and then fails as a
	// lifecycle conflict on the unknown session; 257 bytes is an argument
	// error.
	before := f.effects(t)
	atLimit := refresh(`{"workspace_id":"alpha","session_id":"` + strings.Repeat("s", 256) + `"}`)
	if atLimit.Code != http.StatusConflict || atLimit.Body.String() != store.ErrWorkOrderClaimLost.Error()+"\n" {
		t.Fatalf("256-byte session status=%d body=%q", atLimit.Code, atLimit.Body.String())
	}
	over := refresh(`{"workspace_id":"alpha","session_id":"` + strings.Repeat("s", 257) + `"}`)
	if over.Code != http.StatusBadRequest {
		t.Fatalf("257-byte session status=%d body=%q", over.Code, over.Body.String())
	}
	assertNoVisibilityEffects(t, "string bounds", before, f.effects(t))

	valid := strings.Repeat("0123456789abcdef", 4)
	if response := refresh(`{"workspace_id":"alpha","session_id":"session","prior_revision":"` + valid + `"}`); response.Code != http.StatusOK {
		t.Fatalf("lowercase prior status=%d body=%s", response.Code, response.Body.String())
	}
	for _, prior := range []string{strings.ToUpper(valid), valid[:63], valid + "0", strings.Repeat("g", 64)} {
		before := f.effects(t)
		if response := refresh(`{"workspace_id":"alpha","session_id":"session","prior_revision":"` + prior + `"}`); response.Code != http.StatusBadRequest || response.Body.String() != "invalid prior_revision\n" {
			t.Fatalf("prior %q status=%d body=%q", prior, response.Code, response.Body.String())
		}
		assertNoVisibilityEffects(t, "prior "+prior, before, f.effects(t))
	}
}

// TestWorkOrderToolErrorsPreserveMCPText proves the typed categories change
// only REST status codes: MCP still returns the same in-band error text.
func TestWorkOrderToolErrorsPreserveMCPText(t *testing.T) {
	f := newVisibilityFixture(t, "usr_member")
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		response := f.serve("member-pat", httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw)))
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result.Content) != 1 {
			t.Fatalf("mcp response status=%d body=%s", response.Code, response.Body.String())
		}
		return envelope.Result.Content[0].Text, envelope.Result.IsError
	}
	for _, item := range []struct {
		name string
		tool string
		args map[string]any
		text string
	}{
		{"submit foreign workspace", "submit_for_review", map[string]any{"workspace_id": "beta", "work_order_id": visibilityOrder, "session_id": visibilitySession, "head_sha": visibilityHead}, "workspace_not_found: workspace not found"},
		{"submit missing workspace", "submit_for_review", map[string]any{"workspace_id": missingWorkspace, "work_order_id": visibilityOrder, "session_id": visibilitySession, "head_sha": visibilityHead}, "workspace_not_found: workspace not found"},
		{"submit blank head", "submit_for_review", map[string]any{"workspace_id": "alpha", "work_order_id": visibilityOrder, "session_id": visibilitySession, "head_sha": ""}, "head_sha is required"},
		{"refresh foreign workspace", "refresh_work_order_context", map[string]any{"workspace_id": "beta", "work_order_id": visibilityOrder, "session_id": visibilitySession}, "workspace_not_found: workspace not found"},
		{"refresh invalid prior", "refresh_work_order_context", map[string]any{"workspace_id": "alpha", "work_order_id": visibilityOrder, "session_id": visibilitySession, "prior_revision": "bad"}, "invalid prior_revision"},
		{"refresh missing session", "refresh_work_order_context", map[string]any{"workspace_id": "alpha", "work_order_id": visibilityOrder}, "session_id is required"},
	} {
		t.Run(item.name, func(t *testing.T) {
			text, isError := call(item.tool, item.args)
			if !isError || text != item.text {
				t.Fatalf("isError=%v text=%q", isError, text)
			}
		})
	}
}
