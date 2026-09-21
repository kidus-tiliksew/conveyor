package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskVerificationReadAndObservationBoundary(t *testing.T) {
	s, worker, id := verificationHTTPFixture(t)
	s.Workspace = "demo"
	b := s.Store.(store.Backend)
	request := httptest.NewRequest("POST", "/mcp", nil).WithContext(worker)
	call := func(name string, args map[string]any) any {
		t.Helper()
		args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = "demo", id, "session", "token"
		value, err := s.callMCPTool(request, name, args)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	snapshot := call("prepare_verification", map[string]any{"request_key": "prepare"}).(store.VerificationSnapshot)
	contextID := snapshot.Contexts[0].ID
	obligation := call("register_verification_obligation", map[string]any{"context_id": contextID, "obligation_id": "observe", "description": "Observe fixture", "sources": []any{map[string]any{"document_id": "req-fixture", "version": 1, "section_id": "REQ-1"}}, "observation_procedure": "Read fixture state", "contract": map[string]any{"id": "observe", "kind": "observation", "argv": []string{}, "cwd": ".", "timeout_seconds": 30, "stages": []string{"verify"}, "prerequisites": []any{}, "permissions": []any{}, "inputs": []any{}, "required_assertions": []string{}, "retry_policy": "safe_to_replay", "safety_basis": "read only", "operations": []any{}, "evidence_outputs": []any{map[string]any{"type": "operator_observation", "schema_version": 1, "minimum_items": 1}}, "supports": []any{}}}).(store.VerificationReceipt)
	run := call("start_verification_attempt", map[string]any{"coverage": store.VerificationCoverage{ObligationIDs: []string{"observe"}, Justification: "Fixture coverage", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "Current observation", Subjects: []core.VerificationSubject{{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}}}}}, "context_id": contextID, "start_key": "start", "subject": core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, "environment": core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, "effective_permissions": []any{}, "safe_inputs": map[string]any{}}).(store.VerificationReceipt)
	ctx := store.WithWorkspace(t.Context(), "demo")
	if _, err := b.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}, "read-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := b.VerifyPersonalAccessToken(ctx, "read-token")
	if err != nil {
		t.Fatal(err)
	}
	user := func(id string) context.Context {
		return store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "test", OwnerUserID: id, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), store.Actor{ID: store.UserActorID(id), Role: core.ActorUser})
	}
	operator := user(owner.ID)
	viewer, err := b.ProvisionIdentityUser(operator, "viewer@example.test", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.GrantWorkspaceRole(operator, viewer.Email, "demo", core.WorkspaceRoleViewer); err != nil {
		t.Fatal(err)
	}
	base := "/v1/tasks/verification-http/verification"
	serve := func(ctx context.Context, method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Workspace-ID", "demo")
		r.Header.Set("X-Conveyor-CSRF", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	content := []byte("private linked artifact")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	call("upload_verification_artifact", map[string]any{"context_id": contextID, "run_id": run.ID, "upload_id": "capture", "index": 0, "content": content})
	call("upload_verification_artifact", map[string]any{"context_id": contextID, "run_id": run.ID, "upload_id": "capture", "finalize": map[string]any{"name": "capture.txt", "content_type": "text/plain", "size_bytes": len(content), "sha256": hash}})
	order, err := s.Store.GetWorkOrder(worker, id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	envelope := core.VerificationEvidence{SchemaVersion: 1, ID: "support", SubmissionKey: "support", Type: "state_observation", CapturedAt: at, CapturedBy: core.VerificationCaptureActor{Identity: "fixture", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: "demo", TaskID: order.TaskID, WorkOrderID: id, WorkOrderAttemptID: order.AttemptID, ContextID: contextID, RunID: run.ID, Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, Revisions: snapshot.Contexts[0].Revisions, GoverningPins: snapshot.Contexts[0].GoverningPins, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, Artifacts: []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}, Payload: core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: at, Value: core.JSONPayload("observed")})}
	call("submit_verification_evidence", map[string]any{"context_id": contextID, "run_id": run.ID, "submission_key": "support", "evidence": []core.VerificationEvidence{envelope}})
	input := map[string]any{"context_id": contextID, "run_id": run.ID, "idempotency_key": "human-observation", "fact": "Unique private observation", "supporting": []any{map[string]any{"evidence_id": "support"}}}
	raw, _ := json.Marshal(input)
	if w := serve(user(viewer.ID), "GET", base, nil); w.Code != 200 {
		t.Fatalf("viewer summary: %d %s", w.Code, w.Body)
	}
	if w := serve(user(viewer.ID), "POST", base+"/observations", raw); w.Code < 400 {
		t.Fatalf("viewer wrote observation: %d", w.Code)
	}
	for _, field := range []string{"actor", "captured_at", "work_order_id", "submitted_by", "workspace_id"} {
		input[field] = "forged"
		bad, _ := json.Marshal(input)
		w := serve(operator, "POST", base+"/observations", bad)
		if w.Code != 400 {
			t.Fatalf("client provenance %s: %d %s", field, w.Code, w.Body)
		}
		delete(input, field)
	}
	w := serve(operator, "POST", base+"/observations", raw)
	if w.Code != 200 {
		t.Fatalf("observation: %d %s", w.Code, w.Body)
	}
	var receipt store.VerificationReceipt
	if err = json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	replay := serve(operator, "POST", base+"/observations", raw)
	if replay.Code != 200 || replay.Body.String() != w.Body.String() {
		t.Fatal("HTTP replay changed receipt")
	}
	input["fact"] = "Changed fact"
	changed, _ := json.Marshal(input)
	if w = serve(operator, "POST", base+"/observations", changed); w.Code != 409 {
		t.Fatalf("changed fact: %d %s", w.Code, w.Body)
	}
	for _, suffix := range []string{"", "/contexts/" + contextID + "/attempts", "/contexts/" + contextID + "/evidence"} {
		w = serve(operator, "GET", base+suffix, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "Unique private observation") || strings.Contains(w.Body.String(), "\"payload\"") {
			t.Fatalf("summary leaked detail: %d %s", w.Code, w.Body)
		}
	}
	detail := base + "/contexts/" + contextID + "/evidence/" + receipt.EvidenceIDs[0]
	w = serve(user(viewer.ID), "GET", detail, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Unique private observation") {
		t.Fatalf("authorized detail: %d %s", w.Code, w.Body)
	}
	artifactPath := base + "/contexts/" + contextID + "/evidence/support/artifacts/" + hash
	artifact := serve(user(viewer.ID), "GET", artifactPath, nil)
	if artifact.Code != 200 || !bytes.Equal(artifact.Body.Bytes(), content) || artifact.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("authorized artifact: %d %s", artifact.Code, artifact.Body)
	}
	for _, path := range []string{strings.Replace(detail, contextID, "foreign", 1), strings.Replace(detail, "verification-http", "foreign", 1), detail + "/artifacts/foreign"} {
		w = serve(operator, "GET", path, nil)
		if w.Code != 404 || strings.Contains(w.Body.String(), "Unique private") {
			t.Fatalf("foreign content: %d %s", w.Code, w.Body)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=51", "?cursor=invalid", "?limit=1&limit=2", "?unexpected=x"} {
		w = serve(operator, "GET", base+query, nil)
		if w.Code != 400 {
			t.Fatalf("bad pagination %s: %d", query, w.Code)
		}
	}
}
