package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestVerificationPermissionRouteIsOperatorOnly(t *testing.T) {
	s, worker, id := verificationHTTPFixture(t)
	s.Workspace = "demo"
	b := s.Store.(store.Backend)
	s.Workspaces = b
	call := func(name string, input any) any {
		t.Helper()
		out, err := s.WorkOrders.Verification(worker, id, "session", "token", name, core.JSONPayload(input))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	snapshot := call("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "permission-route"}).(store.VerificationSnapshot)
	ctx := store.WithWorkspace(t.Context(), "demo")
	_, err := b.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}, "permission-route-token")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := b.VerifyPersonalAccessToken(ctx, "permission-route-token")
	if err != nil {
		t.Fatal(err)
	}
	user := func(id string) context.Context {
		return store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "route-user", Kind: core.CredentialUser, Scope: core.CredentialScopeUser, OwnerUserID: id}), store.Actor{ID: store.UserActorID(id), Role: core.ActorUser})
	}
	operator := user(owner.ID)
	member, err := b.ProvisionIdentityUser(operator, "maintainer@example.test", "Maintainer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.GrantWorkspaceRole(operator, member.Email, "demo", core.WorkspaceRoleMaintainer); err != nil {
		t.Fatal(err)
	}
	request := store.VerificationPermissionRequest{ContextID: snapshot.Contexts[0].ID, RequestKey: "grant", Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: "unregistered", ContractDigest: "not-registered"}, Actions: []core.VerificationPermission{}}
	serve := func(ctx context.Context, path string, body []byte) int {
		r := httptest.NewRequest("POST", path, bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Workspace-ID", "demo")
		r.Header.Set("X-Conveyor-CSRF", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	path := "/v1/work-orders/" + id + "/verification/permissions?workspace_id=demo"
	agent := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "child", Kind: core.CredentialAgent, OwnerUserID: owner.ID, RunWorkspaceID: "demo", RunWorkOrderID: id, RunSessionID: "session"}), store.Actor{ID: "agent:child", Role: core.ActorAgent})
	for _, caller := range []context.Context{worker, agent, user(member.ID)} {
		if code := serve(caller, path, core.JSONPayload(request)); code < 400 {
			t.Fatalf("nonoperator created grant: %d", code)
		}
	}
	if code := serve(operator, path, core.JSONPayload(request)); code < 400 {
		t.Fatal("unregistered subject accepted")
	}
	obligation := call("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: request.ContextID, ObligationID: "ordinary", Description: "read-only fixture", Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}}, Contract: verification.Exercise{ID: "ordinary", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 1, RequiredAssertions: []string{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read only"}}).(store.VerificationReceipt)
	request.Subject = core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: obligation.Digest}
	for i := 0; i < 2; i++ {
		if code := serve(operator, path, core.JSONPayload(request)); code != 200 {
			t.Fatalf("operator grant or replay: %d", code)
		}
	}
	updated := call("get_verification_context", workorder.VerificationContextRequest{ContextID: request.ContextID}).(store.VerificationSnapshot)
	if len(updated.PermissionGrants) != 1 || updated.PermissionGrants[0].WorkOrderAttemptID != snapshot.Contexts[0].WorkOrderAttemptID {
		t.Fatal("grant was not derived from locked context")
	}
	foreign := request
	foreign.ContextID = "foreign"
	if code := serve(operator, path, core.JSONPayload(foreign)); code != 404 {
		t.Fatalf("foreign context: %d", code)
	}
	foreign = request
	foreign.Subject.ContractDigest = "foreign"
	if code := serve(operator, path, core.JSONPayload(foreign)); code < 400 {
		t.Fatalf("foreign subject: %d", code)
	}
	// Strict decoding rejects caller-supplied claim/revision authority before
	// attempting any ownership lookup.
	for _, field := range []string{"work_order_attempt_id", "revisions", "actor", "workspace_id"} {
		var raw map[string]any
		_ = json.Unmarshal(core.JSONPayload(request), &raw)
		raw[field] = "forged"
		if code := serve(operator, path, core.JSONPayload(raw)); code != 400 {
			t.Fatalf("%s: %d", field, code)
		}
	}
	if code := serve(operator, "/v1/work-orders/"+id+"/verification/permissions", core.JSONPayload(request)); code != 400 {
		t.Fatalf("implicit workspace: %d", code)
	}
	for _, tool := range verificationMCPTools() {
		if tool["name"] == "grant_verification_permissions" || tool["name"] == "revoke_verification_permissions" {
			t.Fatal("operator grant exposed through MCP")
		}
	}
}
