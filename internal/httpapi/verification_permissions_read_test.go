package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type permissionRouteFixture struct {
	t                  *testing.T
	s                  *Server
	b                  store.Backend
	worker             context.Context
	ctx                context.Context
	id                 string
	operator, contrib  context.Context
	viewer             context.Context
	ownerID, contextID string
	subject            core.VerificationSubject
	networkSubject     core.VerificationSubject
	path               string
}

func newPermissionRouteFixture(t *testing.T) *permissionRouteFixture {
	t.Helper()
	s, worker, id := verificationHTTPFixture(t)
	s.Workspace = "demo"
	b := s.Store.(store.Backend)
	s.Workspaces = b
	f := &permissionRouteFixture{t: t, s: s, b: b, worker: worker, id: id, path: "/v1/work-orders/" + id + "/verification/permissions?workspace_id=demo"}
	f.ctx = store.WithWorkspace(t.Context(), "demo")
	if _, err := b.BootstrapIdentity(f.ctx, config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}, "permission-read-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := b.VerifyPersonalAccessToken(f.ctx, "permission-read-token")
	if err != nil {
		t.Fatal(err)
	}
	f.ownerID = owner.ID
	f.operator = f.user(owner.ID)
	// Bootstrap never writes bindings (DEC-63(4)); the fixture grants the
	// deployment owner's workspace binding explicitly.
	if _, err = b.GrantWorkspaceRole(f.operator, owner.Email, "demo", core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	for role, target := range map[core.WorkspaceRole]*context.Context{core.WorkspaceRoleContributor: &f.contrib, core.WorkspaceRoleViewer: &f.viewer} {
		member, err := b.ProvisionIdentityUser(f.operator, string(role)+"@example.test", string(role))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = b.GrantWorkspaceRole(f.operator, member.Email, "demo", role); err != nil {
			t.Fatal(err)
		}
		*target = f.user(member.ID)
	}
	snapshot := f.call("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "permission-read"}).(store.VerificationSnapshot)
	f.contextID = snapshot.Contexts[0].ID
	register := func(id string, permissions []verification.Permission) core.VerificationSubject {
		receipt := f.call("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: f.contextID, ObligationID: id, Description: "read-only fixture " + id, Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}}, Contract: verification.Exercise{ID: id, Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 1, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read only", Permissions: permissions}}).(store.VerificationReceipt)
		return core.VerificationSubject{Kind: "ordinary", ObligationID: id, ContractDigest: receipt.Digest}
	}
	f.subject = register("ordinary", nil)
	f.networkSubject = register("network", []verification.Permission{{Kind: "network", TargetBinding: "api"}})
	return f
}

func (f *permissionRouteFixture) user(id string) context.Context {
	return store.WithActor(store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "route-user-" + id, Kind: core.CredentialUser, Scope: core.CredentialScopeUser, OwnerUserID: id}), store.Actor{ID: store.UserActorID(id), Role: core.ActorUser})
}

func (f *permissionRouteFixture) call(name string, input any) any {
	f.t.Helper()
	out, err := f.s.WorkOrders.Verification(f.worker, f.id, "session", "token", name, core.JSONPayload(input))
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *permissionRouteFixture) serve(ctx context.Context, method, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Workspace-ID", "demo")
	r.Header.Set("X-Conveyor-CSRF", "1")
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	return w
}

func (f *permissionRouteFixture) view(query string) store.VerificationPermissionView {
	f.t.Helper()
	w := f.serve(f.operator, "GET", f.path+query, nil)
	if w.Code != 200 {
		f.t.Fatalf("operator projection: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		f.t.Fatal("projection is cacheable")
	}
	for _, secret := range []string{"\"token\"", "client_token", "ClientToken", "session"} {
		if strings.Contains(w.Body.String(), secret) {
			f.t.Fatalf("projection exposed %s: %s", secret, w.Body.String())
		}
	}
	var v store.VerificationPermissionView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *permissionRouteFixture) post(ctx context.Context, request store.VerificationPermissionRequest) (*httptest.ResponseRecorder, map[string]string) {
	w := f.serve(ctx, "POST", f.path, core.JSONPayload(request))
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

// component-verification-runner: the operator projection exposes exact frozen subjects, grant
// receipts and eligibility without payloads, tokens or a self-grant path.
func TestVerificationPermissionProjectionAndReceipt(t *testing.T) {
	f := newPermissionRouteFixture(t)
	events, err := f.b.(interface {
		ListEvents(context.Context, string) ([]core.Event, error)
	}).ListEvents(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	v := f.view("")
	if v.Eligibility.State != "eligible" || v.Context == nil || v.Context.ID != f.contextID || v.SubmittedHead != strings.Repeat("a", 40) || v.LeaseExpiresAt == nil || v.ExecutionDeadline == nil {
		t.Fatalf("projection header: %+v", v)
	}
	if len(v.Subjects) != 2 || v.Subjects[0].Subject != f.subject || v.Subjects[1].Subject != f.networkSubject || len(v.Grants) != 0 {
		t.Fatalf("projection subjects: %+v", v.Subjects)
	}
	if got := v.Subjects[1].Actions; len(got) != 1 || got[0] != (verification.ActionRequirement{Kind: "network", Binding: "api", Required: true}) {
		t.Fatalf("network action requirement: %+v", got)
	}
	after, _ := f.b.(interface {
		ListEvents(context.Context, string) ([]core.Event, error)
	}).ListEvents(f.ctx, "")
	if len(after) != len(events) {
		t.Fatal("projection read wrote an event")
	}
	page := f.view("&limit=1")
	if len(page.Subjects) != 1 || page.NextCursor == "" || f.view("&limit=1&cursor=" + page.NextCursor).Subjects[0].Subject != f.networkSubject {
		t.Fatal("subject pagination")
	}

	request := store.VerificationPermissionRequest{ContextID: f.contextID, RequestKey: "network-grant", Subject: f.networkSubject, Actions: []core.VerificationPermission{{Kind: "network", Binding: "api", Target: "https://api.example.test"}}}
	for _, caller := range []context.Context{f.worker, f.contrib, f.viewer} {
		if w, _ := f.post(caller, request); w.Code < 400 {
			t.Fatalf("nonoperator granted: %d", w.Code)
		}
	}
	w, _ := f.post(f.operator, request)
	var receipt store.VerificationReceipt
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if w, _ = f.post(f.operator, request); w.Code != 200 || !strings.Contains(w.Body.String(), receipt.ID) {
		t.Fatalf("identical retry: %d %s", w.Code, w.Body.String())
	}
	readBack := f.view("&grant_id=" + receipt.ID)
	if len(readBack.Grants) != 1 || len(readBack.Subjects) != 0 {
		t.Fatalf("grant read-back: %+v", readBack)
	}
	g := readBack.Grants[0]
	if g.ID != receipt.ID || g.Actor != store.UserActorID(f.ownerID) || g.WorkOrderID != f.id || g.WorkOrderAttemptID != v.Context.WorkOrderAttemptID || g.Subject != f.networkSubject || len(g.Actions) != 1 || g.Actions[0].Target != "https://api.example.test:443" || len(g.Revisions) != 1 || g.Revocation != nil {
		t.Fatalf("receipt bindings: %+v", g)
	}
	if got := f.view("").Subjects[1].GrantIDs; len(got) != 1 || got[0] != receipt.ID {
		t.Fatalf("subject grant ids: %v", got)
	}
	if w := f.serve(f.operator, "GET", f.path+"&grant_id=absent", nil); w.Code != 404 {
		t.Fatalf("absent grant read-back: %d", w.Code)
	}

	changed := request
	changed.Actions = []core.VerificationPermission{{Kind: "network", Binding: "api", Target: "https://other.example.test"}}
	if w, body := f.post(f.operator, changed); w.Code != 409 || body["reason"] != store.VerificationRefusalRequestConflict || body["recovery"] == "" {
		t.Fatalf("changed request: %d %v", w.Code, body)
	}
	unregistered := request
	unregistered.RequestKey, unregistered.Subject.ContractDigest = "unregistered", "not-registered"
	if w, body := f.post(f.operator, unregistered); body["reason"] != store.VerificationRefusalSubjectUnregistered || w.Code < 400 {
		t.Fatalf("unregistered subject: %d %v", w.Code, body)
	}
	if w, body := f.post(f.operator, store.VerificationPermissionRequest{ContextID: f.contextID, RequestKey: "revoke-unknown", RevokeGrantID: f.contextID + ":absent", Reason: "fixture"}); w.Code != 409 || body["reason"] != store.VerificationRefusalGrantUnknown {
		t.Fatalf("unknown revocation: %d %v", w.Code, body)
	}
	if w, _ := f.post(f.operator, store.VerificationPermissionRequest{ContextID: f.contextID, RequestKey: "revoke", RevokeGrantID: receipt.ID, Reason: "fixture revocation"}); w.Code != 200 {
		t.Fatalf("revocation: %d %s", w.Code, w.Body.String())
	}
	if r := f.view("&grant_id=" + receipt.ID).Grants[0].Revocation; r == nil || r.Reason != "fixture revocation" || r.Actor != store.UserActorID(f.ownerID) {
		t.Fatalf("revocation read-back: %+v", r)
	}
	if w, body := f.post(f.operator, request); w.Code != 409 || body["reason"] != store.VerificationRefusalGrantRevoked {
		t.Fatalf("revoked replay: %d %v", w.Code, body)
	}

	// Foreign and unauthorized callers receive no reason or resource detail.
	foreign := request
	foreign.RequestKey, foreign.ContextID = "foreign", "foreign"
	if w, body := f.post(f.operator, foreign); w.Code != 404 || body["reason"] != "" {
		t.Fatalf("foreign context: %d %v", w.Code, body)
	}
	agent := store.WithActor(store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "child", Kind: core.CredentialAgent, OwnerUserID: f.ownerID, RunWorkspaceID: "demo", RunWorkOrderID: f.id, RunSessionID: "session"}), store.Actor{ID: "agent:child", Role: core.ActorAgent})
	for name, caller := range map[string]context.Context{"worker": f.worker, "agent": agent, "contributor": f.contrib, "viewer": f.viewer, "anonymous": f.ctx} {
		w := f.serve(caller, "GET", f.path, nil)
		if w.Code < 400 || strings.Contains(w.Body.String(), f.contextID) {
			t.Fatalf("%s projection: %d %s", name, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"&unknown=1", "&limit=0", "&limit=51", "&context_id=a&context_id=b", "&cursor=bad"} {
		if w := f.serve(f.operator, "GET", f.path+query, nil); w.Code != 400 {
			t.Fatalf("query %s: %d", query, w.Code)
		}
	}
	if w := f.serve(f.operator, "GET", "/v1/work-orders/"+f.id+"/verification/permissions", nil); w.Code != 400 {
		t.Fatalf("implicit workspace: %d", w.Code)
	}
}

// component-verification-runner: authorized operators learn why the claim-bound window is closed.
func TestVerificationPermissionClaimWindowReasons(t *testing.T) {
	f := newPermissionRouteFixture(t)
	request := store.VerificationPermissionRequest{ContextID: f.contextID, RequestKey: "late", Subject: f.subject, Actions: []core.VerificationPermission{}}
	order, err := f.b.GetWorkOrder(f.ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	order.HeadSHA = strings.Repeat("b", 40)
	if err = storetest.UpdateWorkOrder(f.ctx, f.b, order, core.WorkOrderCmdRenew); err != nil {
		t.Fatal(err)
	}
	if v := f.view(""); v.Eligibility.State != store.VerificationRefusalHeadChanged {
		t.Fatalf("head eligibility: %+v", v.Eligibility)
	}
	if w, body := f.post(f.operator, request); w.Code != 409 || body["reason"] != store.VerificationRefusalHeadChanged {
		t.Fatalf("changed head grant: %d %v", w.Code, body)
	}
	order.HeadSHA = strings.Repeat("a", 40)
	if err = storetest.UpdateWorkOrder(f.ctx, f.b, order, core.WorkOrderCmdRenew); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ReleaseWorkerClaim(f.ctx, f.b, f.id, "fixture", core.WorkOrderRelease{SessionID: "session", Reason: "fixture handoff"}); err != nil {
		t.Fatal(err)
	}
	v := f.view("")
	if v.Eligibility.State != store.VerificationRefusalNotClaimed || v.Context == nil || v.Context.ID != f.contextID {
		t.Fatalf("released eligibility: %+v", v)
	}
	if w, body := f.post(f.operator, request); w.Code != 409 || body["reason"] != store.VerificationRefusalNotClaimed {
		t.Fatalf("released grant: %d %v", w.Code, body)
	}
}

// The memory store requeues an order once its lease is observed expired, so the
// claimed-but-lapsed window is exercised on the shared classifier directly.
func TestVerificationPermissionExpiredClaimState(t *testing.T) {
	now := time.Now().UTC()
	task := core.Task{ID: "t", ReviewedHeadSHA: strings.Repeat("a", 40)}
	order := core.WorkOrder{ID: "o", TaskID: "t", Stage: core.StageVerify, State: core.WorkOrderClaimed, HeadSHA: task.ReviewedHeadSHA, AttemptID: "attempt", LeaseExpiresAt: now.Add(-time.Second), ExecutionDeadline: now.Add(time.Hour)}
	v, err := store.BuildVerificationPermissionView(task, order, nil, store.VerificationPermissionViewRequest{}, now)
	if err != nil || v.Eligibility.State != store.VerificationRefusalClaimExpired || v.Eligibility.Recovery == "" {
		t.Fatalf("lapsed lease: %+v %v", v.Eligibility, err)
	}
	order.LeaseExpiresAt, order.ExecutionDeadline = now.Add(time.Minute), now.Add(-time.Second)
	if got := store.VerificationPermissionOrderState(task, order, now); got != store.VerificationRefusalClaimExpired {
		t.Fatalf("passed deadline: %s", got)
	}
	order.ExecutionDeadline = now.Add(time.Hour)
	if v, _ = store.BuildVerificationPermissionView(task, order, nil, store.VerificationPermissionViewRequest{}, now); v.Eligibility.State != store.VerificationRefusalContextMissing {
		t.Fatalf("unprepared context: %+v", v.Eligibility)
	}
}
