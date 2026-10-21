package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func verificationHTTPFixture(t *testing.T) (*Server, context.Context, string) {
	t.Helper()
	b := store.NewVolatileBackend()
	t.Cleanup(b.Close)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://github.com/org/repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := b.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "verification-http", Workspace: "demo", Repo: "repo", Title: "verification", Branch: "conveyor/http", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	task.SetupContract.MaxBounces = 10
	if err := b.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	o := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, CreatedAt: task.CreatedAt}
	if _, err := storetest.CreateStageWorkOrder(ctx, b, job, o); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimWorkOrder(ctx, b, o.ID, core.WorkOrderClaim{Requirements: []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe"}}}}, WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session", ClientToken: "token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: "worker:fixture", Role: core.ActorWorker})
	ctx = withWorkerActor(ctx, core.Worker{ID: "fixture", Workspace: "demo"})
	s := NewServer(b)
	s.WorkOrders = &workorder.Service{Store: b, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	return s, ctx, o.ID
}
func verificationRESTCall(s *Server, ctx context.Context, id, name string, args map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(args)
	r := httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+id+"/verification/"+name, bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("X-Workspace-ID", "demo")
	route := chi.NewRouteContext()
	route.URLParams.Add("id", id)
	route.URLParams.Add("operation", name)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
	w := httptest.NewRecorder()
	s.verificationOrder(w, r)
	return w
}
func TestVerificationMCPRESTParityAndScope(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	base := map[string]any{"workspace_id": "demo", "work_order_id": id, "session_id": "session", "client_token": "token", "request_key": "prepare"}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	result, err := s.callMCPTool(request, "prepare_verification", base)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result.(store.VerificationSnapshot)
	response := verificationRESTCall(s, ctx, id, "prepare_verification", base)
	if response.Code != 200 {
		t.Fatalf("REST: %d %s", response.Code, response.Body)
	}
	var rest store.VerificationSnapshot
	if err = json.Unmarshal(response.Body.Bytes(), &rest); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, rest) {
		t.Fatal("REST/MCP preparation returned different snapshots")
	}
	// Each transport retries the same immutable command and must return the
	// same receipt, including chunks and standalone artifact finalization.
	parity := func(name string, payload map[string]any) any {
		t.Helper()
		payload["workspace_id"], payload["work_order_id"], payload["session_id"], payload["client_token"] = "demo", id, "session", "token"
		got, err := s.callMCPTool(request, name, payload)
		if err != nil {
			t.Fatalf("MCP %s: %v", name, err)
		}
		response := verificationRESTCall(s, ctx, id, name, payload)
		encoded, _ := json.Marshal(got)
		var left, right any
		json.Unmarshal(encoded, &left)
		json.Unmarshal(response.Body.Bytes(), &right)
		if name == "start_verification_attempt" && response.Code == 200 {
			l, r := left.(map[string]any), right.(map[string]any)
			if l["LaunchAuthorized"] != true || r["LaunchAuthorized"] != false {
				t.Fatal("start receipt replay authorized another launch")
			}
			l["LaunchAuthorized"] = false
		}
		if response.Code != 200 || !reflect.DeepEqual(left, right) {
			t.Fatalf("%s parity: %d %s", name, response.Code, response.Body)
		}
		return got
	}
	contextID := snapshot.Contexts[0].ID
	obligation := parity("register_verification_obligation", map[string]any{"context_id": contextID, "obligation_id": "observe", "description": "Observe fixture", "sources": []any{map[string]any{"document_id": "req-fixture", "version": 1, "section_id": "REQ-1"}}, "observation_procedure": "Read fixture state", "contract": map[string]any{"id": "observe", "kind": "observation", "argv": []string{}, "cwd": ".", "timeout_seconds": 30, "stages": []string{"verify"}, "prerequisites": []any{}, "permissions": []any{}, "inputs": []any{}, "required_assertions": []string{}, "retry_policy": "safe_to_replay", "safety_basis": "read only", "operations": []any{}, "evidence_outputs": []any{map[string]any{"type": "state_observation", "schema_version": 1, "minimum_items": 1}}, "supports": []any{}}}).(store.VerificationReceipt)
	grantID := storetest.GrantVerificationFixture(t, s.Store.(store.Backend), ctx, "verification-http", id, contextID, "grant", core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, nil)
	run := parity("start_verification_attempt", map[string]any{"grant_id": grantID, "effective_actions": []core.VerificationPermission{}, "coverage": store.VerificationCoverage{ObligationIDs: []string{"observe"}, Justification: "Fixture coverage", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "Current observation covers the fixture", Subjects: []core.VerificationSubject{{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}}}}}, "context_id": contextID, "start_key": "start", "subject": core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, "environment": core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, "effective_permissions": []any{}, "safe_inputs": map[string]any{}}).(store.VerificationReceipt)
	content := []byte("captured fixture output")
	parity("upload_verification_artifact", map[string]any{"context_id": contextID, "run_id": run.ID, "upload_id": "output", "index": 0, "content": content})
	parity("upload_verification_artifact", map[string]any{"context_id": contextID, "run_id": run.ID, "upload_id": "output", "finalize": map[string]any{"name": "output.txt", "content_type": "text/plain", "size_bytes": len(content), "sha256": fmt.Sprintf("%x", sha256.Sum256(content))}})
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	envelope := core.VerificationEvidence{SchemaVersion: 1, ID: "captured-state", SubmissionKey: "captured", Type: "state_observation", CapturedAt: at, CapturedBy: core.VerificationCaptureActor{Identity: "fixture", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: "demo", TaskID: order.TaskID, WorkOrderID: id, WorkOrderAttemptID: order.AttemptID, ContextID: contextID, RunID: run.ID, Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, Revisions: snapshot.Contexts[0].Revisions, GoverningPins: snapshot.Contexts[0].GoverningPins, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, Artifacts: []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}, Payload: core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: at, Value: core.JSONPayload("observed")})}
	parity("submit_verification_evidence", map[string]any{"context_id": contextID, "run_id": run.ID, "submission_key": "captured", "evidence": []core.VerificationEvidence{envelope}})
	parity("read_verification_evidence", map[string]any{"context_id": contextID, "evidence_id": envelope.ID})
	parity("read_verification_evidence", map[string]any{"context_id": contextID, "evidence_id": envelope.ID, "artifact_id": hash})
	parity("get_evidence_schemas", map[string]any{})

	parity("report_verification_outcome", map[string]any{"context_id": contextID, "run_id": run.ID, "state": "failed", "explanation": "Fixture stopped before observation"})
	parity("get_verification_context", map[string]any{"context_id": contextID})
	parity("get_verification_publication", map[string]any{"context_id": contextID})

	for _, name := range []string{"prepare_verification_operation", "reconcile_verification_operation", "submit_verification", "get_verification_context", "get_verification_publication", "register_verification_obligation", "start_verification_attempt", "report_verification_outcome", "submit_verification_evidence", "upload_verification_artifact", "read_verification_evidence", "prepare_verification"} {
		t.Run(name, func(t *testing.T) {
			typed := workorder.VerificationRequestType(name)
			raw, _ := json.Marshal(typed)
			args := map[string]any{}
			json.Unmarshal(raw, &args)
			// Supply the required shape while testing that scope refusal precedes any
			// inspection or mutation of the named verification record.
			args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = "demo", id, "session", "token"
			if name == "prepare_verification" {
				args["request_key"] = "prepare"
			}
			if _, ok := args["context_id"]; ok {
				args["context_id"] = snapshot.Contexts[0].ID
			}
			for _, field := range []string{"workspace_id", "work_order_id", "session_id", "client_token"} {
				invalid := map[string]any{}
				for k, v := range args {
					invalid[k] = v
				}
				invalid[field] = "foreign"
				_, err := s.callMCPTool(request, name, invalid)
				// Foreign requests always fail without returning record content.
				if !errors.Is(err, store.ErrVerificationAccess) {
					t.Fatalf("%s: %v", field, err)
				}
				response := verificationRESTCall(s, ctx, id, name, invalid)
				if response.Code < 400 || strings.Contains(response.Body.String(), snapshot.Contexts[0].ID) {
					t.Fatalf("REST %s disclosed content: %d %s", field, response.Code, response.Body)
				}
			}
		})
	}
	unknown := map[string]any{}
	for k, v := range base {
		unknown[k] = v
	}
	unknown["authority"] = true
	if _, err = s.callMCPTool(request, "prepare_verification", unknown); !errors.Is(err, store.ErrVerificationInvalid) {
		t.Fatalf("unknown field: %v", err)
	}
}
func TestVerificationToolSchemasMatchRequestTypes(t *testing.T) {
	for _, tool := range verificationMCPTools() {
		name := tool["name"].(string)
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		expected := core.VerificationJSONSchema(reflect.TypeOf(workorder.VerificationRequestType(name)))["properties"].(map[string]any)
		for field, shape := range expected {
			published := props[field]
			// Descriptions annotate the published mapping (component-verification-service); shape must match.
			if annotated, ok := published.(map[string]any); ok {
				published = map[string]any{}
				for k, v := range annotated {
					if k != "description" {
						published.(map[string]any)[k] = v
					}
				}
			}
			if !reflect.DeepEqual(published, shape) {
				t.Fatalf("%s field %s schema drift", name, field)
			}
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s accepts unknown fields", name)
		}
	}
}

func TestVerificationSealedReviewSurfaceReads(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	prepare := func(key string) store.VerificationSnapshot {
		result, err := s.callMCPTool(request, "prepare_verification", map[string]any{"workspace_id": "demo", "work_order_id": id, "session_id": "session", "client_token": "token", "request_key": key})
		if err != nil {
			t.Fatal(err)
		}
		return result.(store.VerificationSnapshot)
	}
	sealed, unsealed := prepare("sealed"), prepare("unsealed")
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	access := store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: id, WorkOrderAttemptID: order.AttemptID, ClientToken: "token", Claim: core.WorkOrderClaimIdentity{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session"}}
	sealed.Contexts[0] = storetest.SealEmptyVerificationFixture(t, ctx, s.Store.(store.VerificationStore), access, sealed.Contexts[0], "REQ-1")
	job := core.Job{ID: order.TaskID + "-review-1", TaskID: order.TaskID, Stage: core.StageReview, State: core.JobPending}
	review := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: order.TaskID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: order.HeadSHA, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}
	if err = storetest.CreateReviewRound(ctx, s.Store, order.TaskID, []core.Job{job}, []core.WorkOrder{review}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimWorkOrder(ctx, s.Store, review.ID, core.WorkOrderClaim{WorkerID: "reviewer", ClaimantID: "reviewer", SessionID: "review-session", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: "worker:reviewer", Role: core.ActorWorker})
	ctx = withWorkerActor(ctx, core.Worker{ID: "reviewer", Workspace: "demo"})
	request = request.WithContext(ctx)
	for _, vc := range []store.VerificationContext{sealed.Contexts[0], unsealed.Contexts[0]} {
		for _, name := range []string{"get_verification_context", "get_verification_publication"} {
			args := map[string]any{"workspace_id": "demo", "work_order_id": review.ID, "session_id": "review-session", "client_token": "review-token", "context_id": vc.ID}
			_, err = s.callMCPTool(request, name, args)
			response := verificationRESTCall(s, ctx, review.ID, name, args)
			if vc.ID == sealed.Contexts[0].ID {
				if err != nil || response.Code != 200 {
					t.Fatalf("sealed %s: %v, HTTP %d", name, err, response.Code)
				}
			} else if !errors.Is(err, store.ErrVerificationAccess) || response.Code != 404 || strings.Contains(response.Body.String(), vc.ID) {
				t.Fatalf("unsealed %s disclosed context: %v HTTP %d", name, err, response.Code)
			}
		}
	}
}

func TestVerificationOperationToolsAndSealing(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	invoke := func(name string, input any) (any, map[string]any, error) {
		args := map[string]any{}
		if err := json.Unmarshal(core.JSONPayload(input), &args); err != nil {
			t.Fatal(err)
		}
		args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = "demo", id, "session", "token"
		out, err := s.callMCPTool(request, name, args)
		return out, args, err
	}
	out, _, err := invoke("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "operation"})
	if err != nil {
		t.Fatal(err)
	}
	vc := out.(store.VerificationSnapshot).Contexts[0]
	contract := verification.Exercise{ID: "mutate", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "reconciliation_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}
	out, _, err = invoke("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: vc.ID, ObligationID: "mutate", Description: "Fixture action", Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}}, Contract: contract})
	if err != nil {
		t.Fatal(err)
	}
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "mutate", ContractDigest: out.(store.VerificationReceipt).Digest}
	coverage := store.VerificationCoverage{ObligationIDs: []string{"mutate"}, Justification: "Fixture source is covered", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "Declared fixture action", Subjects: []core.VerificationSubject{subject}}}}
	grantID := storetest.GrantVerificationFixture(t, s.Store.(store.Backend), ctx, "verification-http", id, vc.ID, "grant", subject, nil)
	out, _, err = invoke("start_verification_attempt", workorder.VerificationStartRequest{GrantID: grantID, EffectiveActions: []core.VerificationPermission{}, ContextID: vc.ID, StartKey: "operation-run", Subject: subject, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, EffectivePermissions: []verification.Permission{}, SafeInputs: map[string]json.RawMessage{}, Coverage: coverage})
	if err != nil {
		t.Fatal(err)
	}
	run := out.(store.VerificationReceipt).ID
	operation := workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: run, Action: "prepare", Key: "logical-action", StepID: "step", Target: "fixture", InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("{}")))}
	out, args, err := invoke("prepare_verification_operation", operation)
	if err != nil {
		t.Fatal(err)
	}
	op := out.(store.VerificationReceipt)
	if response := verificationRESTCall(s, ctx, id, "prepare_verification_operation", args); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	operation = workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: run, Action: "dispatching", OperationID: op.ID, Source: "runner", CapturedAt: time.Now().UTC()}
	out, args, err = invoke("prepare_verification_operation", operation)
	if err != nil || !out.(store.VerificationReceipt).DispatchAuthorized {
		t.Fatalf("dispatch: %v", err)
	}
	response := verificationRESTCall(s, ctx, id, "prepare_verification_operation", args)
	var receipt store.VerificationReceipt
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.DispatchAuthorized {
		t.Fatal("REST replay authorized another provider call")
	}
	if _, _, err = invoke("report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: vc.ID, RunID: run, State: "failed", Explanation: "Provider acknowledgement lost"}); err != nil {
		t.Fatal(err)
	}
	reconcile := workorder.VerificationReconcileRequest{ContextID: vc.ID, OperationID: op.ID, Outcome: "applied", Source: "provider inspection", CapturedAt: time.Now().UTC(), ProviderReference: "https://user:password@provider.test/resource?token=private"}
	out, args, err = invoke("reconcile_verification_operation", reconcile)
	if err != nil || out.(store.VerificationReceipt).DispatchAuthorized {
		t.Fatalf("reconciliation: %v", err)
	}
	if response = verificationRESTCall(s, ctx, id, "reconcile_verification_operation", args); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	_, args, err = invoke("submit_verification", workorder.VerificationSubmitRequest{ContextID: vc.ID, Outcome: "succeeded", Coverage: coverage})
	if !errors.Is(err, store.ErrVerificationState) {
		t.Fatalf("incomplete result sealed: %v", err)
	}
	if response = verificationRESTCall(s, ctx, id, "submit_verification", args); response.Code == 200 {
		t.Fatal("REST accepted incomplete result")
	}
	for _, name := range []string{"prepare_verification_operation", "reconcile_verification_operation", "submit_verification"} {
		if mcpCapabilities[name] != core.CapabilityClaimWork {
			t.Fatalf("%s gained operator capability", name)
		}
		_, _, err = invoke(name, map[string]any{"context_id": vc.ID, "actor": "user:forged", "operate_gates": true})
		if !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("%s accepted forged fields: %v", name, err)
		}
	}
}

func TestVerificationNoKitSubmissionSurface(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	access := store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: id, WorkOrderAttemptID: order.AttemptID, ClientToken: "token", Claim: core.WorkOrderClaimIdentity{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session"}}
	vc := store.VerificationContext{Revisions: []core.VerificationRevision{{Repository: "repo", RemoteIdentity: "https://github.com/org/repo", SHA: order.HeadSHA}}, GoverningPins: []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "repo", "revision": order.HeadSHA, "state": "no_manifest"})}}
	selected := &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}
	created, err := s.Store.(store.VerificationStore).ApplyVerification(ctx, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "no-kit-surface", Access: access, Context: &vc, Selection: selected})
	if err != nil {
		t.Fatal(err)
	}
	coverage := store.VerificationCoverage{ObligationIDs: []string{}, Justification: "Empty fixture scope", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "not_applicable", Explanation: "No check applies to this fixture", Subjects: []core.VerificationSubject{}}}}
	args := map[string]any{"workspace_id": "demo", "work_order_id": id, "session_id": "session", "client_token": "token", "context_id": created.ID, "outcome": "succeeded", "coverage": coverage}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	if _, err = s.callMCPTool(request, "submit_verification", args); err != nil {
		t.Fatal(err)
	}
	if response := verificationRESTCall(s, ctx, id, "submit_verification", args); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil || task.NextStage != core.StageReview {
		t.Fatal("submission did not advance to review")
	}
}

func TestVerificationRecoveryStrictAndWorkerRefusal(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	before, err := s.Store.ListEvents(ctx, "verification-http")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"request_id":"r","verification":{"actor":"user:forged"}}`, 400},
		{`{"request_id":"r","request_id":"other"}`, 400},
		{`{"request_id":"r"} {"direction":"other"}`, 400},
		{`{"request_id":"r","verification":{"context_id":"context","run_id":"run","operation_ids":[],"input_digest":"digest","disposition":"not_applied","reason":"operator claim"}}`, 409},
	} {
		route := chi.NewRouteContext()
		route.URLParams.Add("id", id)
		request := httptest.NewRequest(http.MethodPost, "/v1/work-orders/"+id+"/recover", strings.NewReader(tc.body)).WithContext(context.WithValue(ctx, chi.RouteCtxKey, route))
		response := httptest.NewRecorder()
		s.recoverWorkOrder(response, request)
		if response.Code != tc.status {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	after, err := s.Store.ListEvents(ctx, "verification-http")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused recovery changed events")
	}
}

// component-verification-service: one remedy over REST and MCP, then a pre-execution checkpoint.
func TestVerificationCheckpointRemedyAndMissingGrant(t *testing.T) {
	s, ctx, id := verificationHTTPFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	invoke := func(name string, input any) (any, map[string]any, error) {
		args := map[string]any{}
		if err := json.Unmarshal(core.JSONPayload(input), &args); err != nil {
			t.Fatal(err)
		}
		args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = "demo", id, "session", "token"
		out, err := s.callMCPTool(request, name, args)
		return out, args, err
	}
	for _, tool := range verificationMCPTools() {
		if tool["name"] != "submit_verification" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if !strings.Contains(tool["description"].(string), store.VerificationOutcomeMapping) || props["required_action"] == nil || props["outcome"].(map[string]any)["description"] != store.VerificationOutcomeMapping {
			t.Fatal("submit_verification does not publish the outcome mapping")
		}
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	access := store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: id, WorkOrderAttemptID: order.AttemptID, ClientToken: "token", Claim: core.WorkOrderClaimIdentity{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session"}}
	vc := store.VerificationContext{Revisions: []core.VerificationRevision{{Repository: "repo", RemoteIdentity: "https://github.com/org/repo", SHA: order.HeadSHA}}, GoverningPins: []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "repo", "revision": order.HeadSHA, "state": "no_manifest"})}}
	selected := &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}
	created, err := s.Store.(store.VerificationStore).ApplyVerification(ctx, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "checkpoint-surface", Access: access, Context: &vc, Selection: selected})
	if err != nil {
		t.Fatal(err)
	}
	vc.ID = created.ID
	contract := verification.Exercise{ID: "network", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only", Permissions: []verification.Permission{{Kind: "network", TargetBinding: "fixture"}}}
	out, _, err := invoke("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: vc.ID, ObligationID: "network", Description: "Needs network", Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}}, Contract: contract})
	if err != nil {
		t.Fatal(err)
	}
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "network", ContractDigest: out.(store.VerificationReceipt).Digest}
	coverage := store.VerificationCoverage{ObligationIDs: []string{"network"}, Justification: "Fixture source is covered", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "covered", Explanation: "Declared fixture check", Subjects: []core.VerificationSubject{subject}}}}
	events, err := s.Store.ListEvents(ctx, "verification-http")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"blocked", "waiting", "failed"} {
		_, args, err := invoke("submit_verification", workorder.VerificationSubmitRequest{ContextID: vc.ID, Outcome: outcome, Coverage: coverage, Feedback: "missing grant", RequiredAction: "grant network"})
		var remedy *store.VerificationRemedyError
		if !errors.As(err, &remedy) || remedy.Code != "verification_outcome_unsupported" || !strings.Contains(err.Error(), "report_verification_outcome.state") {
			t.Fatalf("MCP %s: %v", outcome, err)
		}
		response := verificationRESTCall(s, ctx, id, "submit_verification", args)
		var body map[string]string
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["code"] != "verification_outcome_unsupported" || body["remedy"] != store.VerificationOutcomeRemedy || strings.Contains(response.Body.String(), vc.ID) {
			t.Fatalf("REST %s: %d %s", outcome, response.Code, response.Body)
		}
		args["session_id"] = "foreign"
		if response = verificationRESTCall(s, ctx, id, "submit_verification", args); response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "verification_outcome_unsupported") {
			t.Fatalf("foreign scope received the remedy: %d %s", response.Code, response.Body)
		}
	}
	_, args, err := invoke("submit_verification", workorder.VerificationSubmitRequest{ContextID: vc.ID, Outcome: "operator_action_required", Coverage: coverage, Feedback: "missing grant"})
	var remedy *store.VerificationRemedyError
	if !errors.As(err, &remedy) || remedy.Code != "verification_checkpoint_incomplete" {
		t.Fatalf("incomplete checkpoint: %v", err)
	}
	after, err := s.Store.ListEvents(ctx, "verification-http")
	if err != nil || len(after) != len(events) {
		t.Fatal("refused outcomes wrote events")
	}
	args["required_action"] = "Recover the verify order, then grant network access for the next context."
	response := verificationRESTCall(s, ctx, id, "submit_verification", args)
	var receipt store.VerificationReceipt
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.State != "operator_action_required" {
		t.Fatalf("REST checkpoint: %d %s", response.Code, response.Body)
	}
	order, err = s.Store.GetWorkOrder(ctx, id)
	if err != nil || order.State != core.WorkOrderQueued || !order.RetrySuppressed || order.Checkpoint == nil || order.Checkpoint.Verification == nil || order.Checkpoint.Verification.ContextID != vc.ID || len(order.Checkpoint.Verification.Grounds) != 1 || order.Checkpoint.Verification.Grounds[0].Kind != store.VerificationGroundMissingGrant || order.VerificationContextID != "" {
		t.Fatalf("checkpoint order = %+v", order)
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil || task.NextStage != core.StageVerify {
		t.Fatal("checkpoint left verify")
	}
	if receipt.NextStage != "verify" || len(receipt.Grounds) != 1 {
		t.Fatalf("checkpoint receipt = %+v", receipt)
	}
	// component-verification-service: the task activity projection keeps the context reference.
	views, err := s.checkpointWorkOrderViews(ctx, []core.WorkOrder{order}, nil)
	if err != nil {
		t.Fatal(err)
	}
	projected, _ := json.Marshal(views[0])
	if !strings.Contains(string(projected), `"verification":{"context_id":"`+vc.ID+`"`) || !strings.Contains(string(projected), `"kind":"missing_grant"`) {
		t.Fatalf("activity projection dropped the checkpoint reference: %s", projected)
	}
	// component-verification-service: the retained claim replays after release.
	events, err = s.Store.ListEvents(ctx, "verification-http")
	if err != nil {
		t.Fatal(err)
	}
	out, err = s.callMCPTool(request, "submit_verification", args)
	if err != nil || !reflect.DeepEqual(out, receipt) {
		t.Fatalf("MCP replay = %+v, %v", out, err)
	}
	if response = verificationRESTCall(s, ctx, id, "submit_verification", args); response.Code != http.StatusOK {
		t.Fatalf("REST replay: %d %s", response.Code, response.Body)
	}
	for name, mutate := range map[string]func(map[string]any){
		"changed":       func(a map[string]any) { a["required_action"] = "different act" },
		"stale session": func(a map[string]any) { a["session_id"] = "stale" },
		"wrong token":   func(a map[string]any) { a["client_token"] = "wrong" },
		"unsupported":   func(a map[string]any) { a["outcome"] = "blocked" },
	} {
		changed := map[string]any{}
		for k, v := range args {
			changed[k] = v
		}
		mutate(changed)
		response = verificationRESTCall(s, ctx, id, "submit_verification", changed)
		if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "verification_outcome_unsupported") {
			t.Fatalf("%s replay: %d %s", name, response.Code, response.Body)
		}
	}
	after, err = s.Store.ListEvents(ctx, "verification-http")
	if err != nil || len(after) != len(events) {
		t.Fatal("checkpoint replays wrote events")
	}
}

// noEligibleKitFixture is a claimed verify order with an approved plan whose
// done criterion DC-1 a verifier can cite (component-verification-service v6).
type noEligibleKitFixture struct {
	t     *testing.T
	s     *Server
	ctx   context.Context
	id    string
	order core.WorkOrder
	plan  core.SpecVersion
}

func newNoEligibleKitFixture(t *testing.T) *noEligibleKitFixture {
	t.Helper()
	s, ctx, id := verificationHTTPFixture(t)
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b := s.Store.(store.Backend)
	plan, err := b.CreateSpecVersion(ctx, core.SpecVersion{TaskID: order.TaskID, Content: "## Done criteria\n\n1. **DC-1 — Fixture.** The fixture records the operator decision.\n", Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.ApproveSpecVersionAndMaterialize(ctx, order.TaskID, plan.Version); err != nil {
		t.Fatal(err)
	}
	return &noEligibleKitFixture{t: t, s: s, ctx: ctx, id: id, order: order, plan: plan}
}

func (f *noEligibleKitFixture) access() store.VerificationAccess {
	return store.VerificationAccess{TaskID: f.order.TaskID, WorkOrderID: f.id, WorkOrderAttemptID: f.order.AttemptID, ClientToken: "token", Claim: core.WorkOrderClaimIdentity{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session"}}
}

// context records discovery as the service would after GitHub App discovery.
func (f *noEligibleKitFixture) context(key, state string, kits []verification.KitReceipt, diagnostics []verification.Diagnostic) string {
	f.t.Helper()
	vc := store.VerificationContext{Revisions: []core.VerificationRevision{{Repository: "repo", RemoteIdentity: "https://github.com/org/repo", SHA: f.order.HeadSHA}}, GoverningPins: []core.VerificationPin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "repo", "revision": f.order.HeadSHA, "state": state})}}
	selected := &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: kits, Diagnostics: diagnostics}, Subjects: []store.VerificationSubjectContract{}}
	created, err := f.s.Store.(store.VerificationStore).ApplyVerification(f.ctx, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: key, Access: f.access(), Context: &vc, Selection: selected})
	if err != nil {
		f.t.Fatalf("context %s: %v", key, err)
	}
	return created.ID
}

// call invokes one tool over MCP and the same arguments over REST.
func (f *noEligibleKitFixture) call(name string, input any, rest bool) (map[string]any, error, *httptest.ResponseRecorder) {
	f.t.Helper()
	args := map[string]any{}
	if err := json.Unmarshal(core.JSONPayload(input), &args); err != nil {
		f.t.Fatal(err)
	}
	args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = "demo", f.id, "session", "token"
	_, err := f.s.callMCPTool(httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(f.ctx), name, args)
	var response *httptest.ResponseRecorder
	if rest {
		response = verificationRESTCall(f.s, f.ctx, f.id, name, args)
	}
	return args, err, response
}

// refused requires the same correctable refusal over MCP and REST with no write.
func (f *noEligibleKitFixture) refused(name string, input any, code, field string, status int) {
	f.t.Helper()
	before, err := f.s.Store.ListEvents(f.ctx, f.order.TaskID)
	if err != nil {
		f.t.Fatal(err)
	}
	_, err, response := f.call(name, input, true)
	var refusal *store.VerificationRefusal
	if !errors.As(err, &refusal) || refusal.Code != code || refusal.Field != field || refusal.Rule == "" || refusal.Remedy == "" || !strings.Contains(err.Error(), code+" at "+field+": ") {
		f.t.Fatalf("%s MCP: %v, want %s at %s", name, err, code, field)
	}
	var body map[string]string
	if response.Code != status || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["code"] != code || body["field"] != field || body["rule"] != refusal.Rule || body["remedy"] != refusal.Remedy {
		f.t.Fatalf("%s REST: %d %s, want %d %s", name, response.Code, response.Body, status, code)
	}
	after, err := f.s.Store.ListEvents(f.ctx, f.order.TaskID)
	if err != nil || !reflect.DeepEqual(before, after) {
		f.t.Fatalf("%s refusal wrote events", code)
	}
}

func (f *noEligibleKitFixture) planSource() store.VerificationCoverageReference {
	return store.VerificationCoverageReference{DocumentID: "approved_plan", Version: f.plan.Version, SectionID: "DC-1"}
}

// coverage maps the approved plan to subjects and marks the requirement not applicable.
func (f *noEligibleKitFixture) coverage(subjects ...core.VerificationSubject) store.VerificationCoverage {
	ids := []string{}
	plan := store.VerificationCoverageSource{Source: f.planSource(), Disposition: "not_applicable", Explanation: "Plan finding", Subjects: []core.VerificationSubject{}}
	for _, subject := range subjects {
		ids = append(ids, subject.ObligationID)
		plan.Disposition, plan.Explanation = "covered", "Plan check"
		plan.Subjects = append(plan.Subjects, subject)
	}
	return store.VerificationCoverage{ObligationIDs: ids, Justification: "The approved plan's done criterion is assessed", Sources: []store.VerificationCoverageSource{plan, {Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "REQ-1"}, Disposition: "not_applicable", Explanation: "Not exercised", Subjects: []core.VerificationSubject{}}}}
}

func (f *noEligibleKitFixture) register(contextID, obligation string) core.VerificationSubject {
	f.t.Helper()
	contract := verification.Exercise{ID: obligation, Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required"}
	if _, err, _ := f.call("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: contextID, ObligationID: obligation, Description: "Plan check", Sources: []workorder.VerificationSource{{DocumentID: "approved_plan", Version: f.plan.Version, SectionID: "DC-1"}}, Contract: contract}, false); err != nil {
		f.t.Fatal(err)
	}
	snapshot, err := f.s.Store.(store.VerificationStore).ReadVerification(f.ctx, f.access(), contextID)
	if err != nil {
		f.t.Fatal(err)
	}
	return core.VerificationSubject{Kind: "ordinary", ObligationID: obligation, ContractDigest: snapshot.Obligations[0].Digest}
}

func (f *noEligibleKitFixture) start(contextID, key, grantID string, subject core.VerificationSubject, coverage store.VerificationCoverage) workorder.VerificationStartRequest {
	return workorder.VerificationStartRequest{ContextID: contextID, StartKey: key, GrantID: grantID, Subject: subject, EffectiveActions: []core.VerificationPermission{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, EffectivePermissions: []verification.Permission{}, SafeInputs: map[string]json.RawMessage{}, Coverage: coverage}
}

func (f *noEligibleKitFixture) intervention() string {
	f.t.Helper()
	task, err := f.s.Store.GetTask(f.ctx, f.order.TaskID)
	if err != nil || task.NextStage != core.StageImplement {
		f.t.Fatalf("feedback did not reopen implement: %+v %v", task.NextStage, err)
	}
	interventions, err := f.s.Store.ListInterventions(f.ctx, f.order.TaskID)
	if err != nil || len(interventions) != 1 {
		f.t.Fatalf("interventions = %+v %v", interventions, err)
	}
	return interventions[0].Comment
}

var malformedIntegrationsKit = []verification.KitReceipt{{KitID: "integrations", Pins: []verification.Pin{}, Eligibility: "invalid", Reasons: []verification.SelectionReason{{Code: "invalid_entry", Path: "manifest.kits[0].exercises[0].supports[0]", Message: `support must name an AC within a declared requirement pin: document "req-x" version 2 matches no requirement pin this kit declares`}}}}
var malformedIntegrationsDiagnostics = []verification.Diagnostic{{Path: "discovery.repo", Message: "malformed"}, {Path: "discovery.repo.manifest.kits[0].exercises[0].supports[0]", Message: `.conveyor/kits/manifest.yaml: kit integrations, exercise go-test: support must name an AC within a declared requirement pin: document "req-x" version 2 matches no requirement pin this kit declares`}}

// Task 261020-cadfba: the funnelflux-pro incident shapes of 2026-10-20 and
// both no-kit paths (component-verification-service v6; req-verification-kits
// AC-1.2, AC-1.3, AC-3.2, AC-8.3).
func TestVerificationNoEligibleKitOutcomes(t *testing.T) {
	t.Run("MalformedDeclarationFeedback", func(t *testing.T) {
		f := newNoEligibleKitFixture(t)
		contextID := f.context("malformed", "malformed", malformedIntegrationsKit, malformedIntegrationsDiagnostics)
		subject := f.register(contextID, "plan-drift")
		coverage := f.coverage(subject)
		// The incident's attempt 1: a start without a grant names the grant rule.
		f.refused("start_verification_attempt", f.start(contextID, "plan-drift-1", "", subject, coverage), store.VerificationCodeGrantMissing, "grant_id", http.StatusConflict)
		// Success is never admitted over a malformed declaration (AC-1.3).
		f.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeDiscoveryNotValid, "outcome", http.StatusConflict)
		finding := store.VerificationFinding{Source: f.planSource(), Statement: "The approved plan omits the operator decision."}
		args, err, response := f.call("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: coverage, Feedback: "Return to implementation.", Findings: []store.VerificationFinding{finding}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if response = verificationRESTCall(f.s, f.ctx, f.id, "submit_verification", args); response.Code != http.StatusOK {
			t.Fatalf("REST replay: %d %s", response.Code, response.Body)
		}
		text := f.intervention()
		for _, want := range []string{"Return to implementation.", "approved_plan v1 DC-1: The approved plan omits the operator decision.", "declaration defect in repo: .conveyor/kits/manifest.yaml: kit integrations, exercise go-test", "supports[0]"} {
			if !strings.Contains(text, want) {
				t.Fatalf("delivered feedback lacks %q: %s", want, text)
			}
		}
		snapshot, err := f.s.Store.(store.VerificationStore).ReadVerification(f.ctx, f.access(), contextID)
		if err != nil || len(snapshot.Attempts) != 0 || snapshot.Contexts[0].Result.Disposition != "discovery_failed" || len(snapshot.Contexts[0].Result.Feedback.MissingSubjects) != 1 {
			t.Fatalf("feedback seal = %+v %v", snapshot.Contexts[0].Result, err)
		}
	})
	t.Run("MalformedDeclarationCheckpoint", func(t *testing.T) {
		f := newNoEligibleKitFixture(t)
		contextID := f.context("malformed", "malformed", malformedIntegrationsKit, malformedIntegrationsDiagnostics)
		subject := f.register(contextID, "plan-drift")
		_, err, response := f.call("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "operator_action_required", Coverage: f.coverage(subject), Feedback: "The plan check needs an operator grant.", RequiredAction: "Grant ordinary obligation plan-drift."}, true)
		if err != nil || response.Code != http.StatusOK {
			t.Fatalf("checkpoint: %v %d %s", err, response.Code, response.Body)
		}
		order, err := f.s.Store.GetWorkOrder(f.ctx, f.id)
		if err != nil || !order.RetrySuppressed || order.VerificationContextID != "" || order.Checkpoint.Verification.Grounds[0].Kind != store.VerificationGroundMissingGrant {
			t.Fatalf("checkpoint order = %+v %v", order.Checkpoint, err)
		}
	})
	for _, discovery := range []struct {
		name, state string
		kits        []verification.KitReceipt
	}{
		{"no_manifest", "no_manifest", []verification.KitReceipt{}},
		{"ineligible", "present", []verification.KitReceipt{{KitID: "other", Pins: []verification.Pin{{Kind: "requirement", DocumentID: "req-other", Version: 1}}, StageMatch: true, Eligibility: "ineligible", Reasons: []verification.SelectionReason{{Code: "pin_not_authoritative"}}}}},
	} {
		t.Run("PlanFeedback/"+discovery.name, func(t *testing.T) {
			f := newNoEligibleKitFixture(t)
			contextID := f.context("plan-"+discovery.name, discovery.state, discovery.kits, []verification.Diagnostic{})
			coverage := f.coverage()
			f.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: coverage, Feedback: "No finding"}, store.VerificationCodeFeedbackUngrounded, "outcome", http.StatusConflict)
			finding := store.VerificationFinding{Source: f.planSource(), Statement: "The operator decision is missing."}
			if _, err, _ := f.call("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: coverage, Feedback: "DC-1 is unmet.", Findings: []store.VerificationFinding{finding}}, false); err != nil {
				t.Fatal(err)
			}
			if text := f.intervention(); !strings.Contains(text, "approved_plan v1 DC-1: The operator decision is missing.") {
				t.Fatalf("delivered feedback lost the finding: %s", text)
			}
		})
		t.Run("Success/"+discovery.name, func(t *testing.T) {
			f := newNoEligibleKitFixture(t)
			contextID := f.context("success-"+discovery.name, discovery.state, discovery.kits, []verification.Diagnostic{})
			subject := f.register(contextID, "plan-check")
			coverage := f.coverage(subject)
			grantID := storetest.GrantVerificationFixture(t, f.s.Store.(store.Backend), f.ctx, f.order.TaskID, f.id, contextID, "grant", subject, nil)
			f.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeSubjectUnstarted, "outcome", http.StatusConflict)
			if _, err, _ := f.call("start_verification_attempt", f.start(contextID, "plan-check-1", grantID, subject, coverage), false); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.s.Store.(store.VerificationStore).ReadVerification(f.ctx, f.access(), contextID)
			if err != nil {
				t.Fatal(err)
			}
			run, vc := snapshot.Attempts[0].ID, snapshot.Contexts[0]
			f.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeAttemptRunning, "context_id", http.StatusConflict)
			at := time.Now().UTC().Format(time.RFC3339Nano)
			zero, no := 0, false
			report := core.VerificationEvidence{ID: "report", Type: "execution_report", SchemaVersion: 1, SubmissionKey: "evidence", CapturedAt: at, ReceivedAt: at, SubmittedBy: "worker:fixture", CapturedBy: core.VerificationCaptureActor{Identity: "fixture", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: "demo", TaskID: f.order.TaskID, WorkOrderID: f.id, WorkOrderAttemptID: f.order.AttemptID, ContextID: contextID, RunID: run, Subject: subject, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}
			report.Payload = core.JSONPayload(core.ExecutionReportPayload{Argv: []string{"check"}, Tool: "check", ToolVersion: "1", Runtime: "fixture", StartedAt: at, EndedAt: at, ExitCode: &zero, TimedOut: &no, Cancelled: &no, StdoutSHA256: strings.Repeat("a", 64), StderrSHA256: strings.Repeat("b", 64), StdoutTruncated: &no, StderrTruncated: &no})
			if _, err, _ = f.call("submit_verification_evidence", workorder.VerificationEvidenceRequest{ContextID: contextID, RunID: run, SubmissionKey: "evidence", Evidence: []json.RawMessage{core.JSONPayload(report)}}, false); err != nil {
				t.Fatal(err)
			}
			if _, err, _ = f.call("report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: contextID, RunID: run, State: "succeeded", ExitCode: &zero}, false); err != nil {
				t.Fatal(err)
			}
			_, err, response := f.call("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: coverage}, true)
			if err != nil || response.Code != http.StatusOK {
				t.Fatalf("success: %v %d %s", err, response.Code, response.Body)
			}
			task, err := f.s.Store.GetTask(f.ctx, f.order.TaskID)
			order, orderErr := f.s.Store.GetWorkOrder(f.ctx, f.id)
			if err != nil || orderErr != nil || task.NextStage != core.StageReview || order.VerificationContextID != contextID || order.HeadSHA != f.order.HeadSHA {
				t.Fatalf("success did not reach review: %s %+v", task.NextStage, order)
			}
		})
	}
	t.Run("DiscoveryUnavailableCheckpoint", func(t *testing.T) {
		f := newNoEligibleKitFixture(t)
		contextID := f.context("unavailable", "permission", []verification.KitReceipt{}, []verification.Diagnostic{{Path: "discovery.repo", Message: "permission"}})
		coverage := f.coverage()
		f.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeDiscoveryNotValid, "outcome", http.StatusConflict)
		_, err, response := f.call("submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "operator_action_required", Coverage: coverage, Feedback: "Discovery has no repository access.", RequiredAction: "Grant the GitHub App access, then recover the verify order."}, true)
		if err != nil || response.Code != http.StatusOK {
			t.Fatalf("checkpoint: %v %d %s", err, response.Code, response.Body)
		}
		order, err := f.s.Store.GetWorkOrder(f.ctx, f.id)
		if err != nil || !order.RetrySuppressed || order.Checkpoint.Verification.Grounds[0].Kind != store.VerificationGroundDiscoveryUnavailable {
			t.Fatalf("checkpoint order = %+v %v", order.Checkpoint, err)
		}
		task, err := f.s.Store.GetTask(f.ctx, f.order.TaskID)
		if err != nil || task.NextStage != core.StageVerify {
			t.Fatal("checkpoint left verify")
		}
	})
}

// component-verification-service v6 "Correctable refusals": each family names
// its code and field over MCP and REST, after scope authentication only.
func TestVerificationCorrectableRefusals(t *testing.T) {
	f := newNoEligibleKitFixture(t)
	contextID := f.context("refusals", "no_manifest", []verification.KitReceipt{}, []verification.Diagnostic{})
	subject := f.register(contextID, "plan-check")
	valid := f.coverage(subject)
	submit := func(mutate func(*store.VerificationCoverage)) workorder.VerificationSubmitRequest {
		c := valid
		c.Sources = append([]store.VerificationCoverageSource{}, valid.Sources...)
		mutate(&c)
		return workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: c, Feedback: "finding"}
	}
	cases := []struct {
		name, code, field string
		status            int
		input             any
	}{
		{"sources null", store.VerificationCodeFieldRequired, "coverage.sources", 400, submit(func(c *store.VerificationCoverage) { c.Sources = nil })},
		{"sources empty", store.VerificationCodePlanMissing, "coverage.sources", 400, submit(func(c *store.VerificationCoverage) { c.Sources = []store.VerificationCoverageSource{} })},
		{"obligation ids null", store.VerificationCodeFieldRequired, "coverage.obligation_ids", 400, submit(func(c *store.VerificationCoverage) { c.ObligationIDs = nil })},
		{"subjects null", store.VerificationCodeFieldRequired, "coverage.sources[1].subjects", 400, submit(func(c *store.VerificationCoverage) {
			c.Sources[1] = store.VerificationCoverageSource{Source: c.Sources[1].Source, Disposition: "not_applicable", Explanation: "Not exercised"}
		})},
		{"pin without section", store.VerificationCodeCitationUnresolved, "coverage.sources[1].source", 400, submit(func(c *store.VerificationCoverage) {
			c.Sources[1] = store.VerificationCoverageSource{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1}, Disposition: "not_applicable", Explanation: "Not exercised", Subjects: []core.VerificationSubject{}}
		})},
		{"approved plan omitted", store.VerificationCodePlanMissing, "coverage.sources", 400, submit(func(c *store.VerificationCoverage) { c.Sources = c.Sources[1:] })},
		{"stale subject digest", store.VerificationCodeSubjectUnknown, "coverage.sources[0].subjects[0]", 400, submit(func(c *store.VerificationCoverage) {
			stale := subject
			stale.ContractDigest = strings.Repeat("0", 64)
			c.Sources[0] = store.VerificationCoverageSource{Source: c.Sources[0].Source, Disposition: "covered", Explanation: "Plan check", Subjects: []core.VerificationSubject{stale}}
		})},
		{"obligation omitted", store.VerificationCodeObligationsMismatch, "coverage.obligation_ids", 409, submit(func(c *store.VerificationCoverage) { c.ObligationIDs = []string{} })},
		{"feedback ungrounded", store.VerificationCodeFeedbackUngrounded, "outcome", 409, submit(func(*store.VerificationCoverage) {})},
		{"finding on success", store.VerificationCodeFindingInvalid, "findings", 400, workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: valid, Findings: []store.VerificationFinding{{Source: f.planSource(), Statement: "x"}}}},
		{"finding unmapped", store.VerificationCodeCitationUnresolved, "findings[0].source", 400, workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: valid, Feedback: "x", Findings: []store.VerificationFinding{{Source: store.VerificationCoverageReference{DocumentID: "approved_plan", Version: f.plan.Version, SectionID: "DC-9"}, Statement: "x"}}}},
		{"feedback too long", store.VerificationCodeFeedbackTooLong, "feedback", 400, workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: valid, Feedback: strings.Repeat("x", store.VerificationCheckpointTextLimit+1)}},
		{"unstarted on success", store.VerificationCodeSubjectUnstarted, "outcome", 409, workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "succeeded", Coverage: valid}},
		{"checkpoint ungrounded", store.VerificationCodeCheckpointUngrounded, "outcome", 409, workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "operator_action_required", Coverage: store.VerificationCoverage{}, Feedback: "x", RequiredAction: "y"}},
		{"grant missing", store.VerificationCodeGrantMissing, "grant_id", 409, f.start(contextID, "start-1", "", subject, valid)},
		{"start key missing", store.VerificationCodeFieldRequired, "start_key", 400, f.start(contextID, "", "", subject, valid)},
	}
	for _, tc := range cases {
		if tc.name == "checkpoint ungrounded" {
			// A checkpoint over the registered, unstarted obligation is grounded by
			// missing_grant, so this case seals no subject: use a fresh context.
			g := newNoEligibleKitFixture(t)
			fresh := g.context("ungrounded", "no_manifest", []verification.KitReceipt{}, []verification.Diagnostic{})
			g.refused("submit_verification", workorder.VerificationSubmitRequest{ContextID: fresh, Outcome: "operator_action_required", Coverage: g.coverage(), Feedback: "x", RequiredAction: "y"}, tc.code, tc.field, tc.status)
			continue
		}
		name := "submit_verification"
		if _, ok := tc.input.(workorder.VerificationStartRequest); ok {
			name = "start_verification_attempt"
		}
		t.Run(tc.name, func(t *testing.T) { f.t = t; f.refused(name, tc.input, tc.code, tc.field, tc.status) })
	}
	f.t = t
	// Foreign scope keeps the one generic refusal and no detail.
	args, _, _ := f.call("submit_verification", submit(func(c *store.VerificationCoverage) { c.Sources = nil }), false)
	args["session_id"] = "foreign"
	if response := verificationRESTCall(f.s, f.ctx, f.id, "submit_verification", args); response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "verification_") || strings.Contains(response.Body.String(), "coverage") {
		t.Fatalf("foreign scope received detail: %d %s", response.Code, response.Body)
	}
	// An unknown context of an authenticated claim is generic too, whatever is
	// wrong with the coverage or outcome (component-verification-service v6).
	unknown := "0123456789abcdef0123456789abcdef"
	for name, input := range map[string]workorder.VerificationSubmitRequest{
		"null sources":      submit(func(c *store.VerificationCoverage) { c.Sources = nil }),
		"plan omitted":      submit(func(c *store.VerificationCoverage) { c.Sources = c.Sources[1:] }),
		"unresolved source": submit(func(c *store.VerificationCoverage) { c.Sources[1].Source.SectionID = "missing" }),
		"unsupported":       {Coverage: valid, Outcome: "blocked"},
		"incomplete":        {Coverage: valid, Outcome: "operator_action_required"},
	} {
		input.ContextID = unknown
		for _, obligation := range []bool{false, true} {
			tool := "submit_verification"
			var payload any = input
			if obligation {
				tool = "register_verification_obligation"
				payload = workorder.VerificationObligationRequest{ContextID: unknown, ObligationID: "other", Description: "x", Sources: []workorder.VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "missing"}}, Contract: verification.Exercise{ID: "other", Kind: "script", Argv: []string{"x"}, TimeoutSeconds: 1, RequiredAssertions: []verification.Assertion{}}}
			}
			_, err, response := f.call(tool, payload, true)
			var refusal *store.VerificationRefusal
			if !errors.Is(err, store.ErrVerificationAccess) || errors.As(err, &refusal) || response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "verification_") {
				t.Fatalf("unknown context %s via %s: MCP %v, REST %d %s", name, tool, err, response.Code, response.Body)
			}
		}
	}
	// Refusal rules never echo caller text, so neither a recognized token nor a
	// configured or arbitrary secret value can leave in a rule.
	for _, secret := range []string{"sk-abcdefghijklmnopqrstuvwxyz012345", "configured-secret-value-6f1c"} {
		inputs := map[string]struct {
			tool  string
			input any
		}{
			"coverage": {"submit_verification", submit(func(c *store.VerificationCoverage) { c.Sources[1].Source.SectionID = secret })},
			"finding": {"submit_verification", workorder.VerificationSubmitRequest{ContextID: contextID, Outcome: "feedback", Coverage: valid, Feedback: "x", Findings: []store.VerificationFinding{{Source: store.VerificationCoverageReference{DocumentID: "approved_plan", Version: f.plan.Version, SectionID: secret}, Statement: "x"}}}},
			"obligation": {"register_verification_obligation", workorder.VerificationObligationRequest{ContextID: contextID, ObligationID: "other", Description: "x", Sources: []workorder.VerificationSource{{DocumentID: secret, Version: 1, SectionID: secret}}, Contract: verification.Exercise{ID: "other", Kind: "script", Argv: []string{"x"}, TimeoutSeconds: 1, RequiredAssertions: []verification.Assertion{}}}},
			"subject": {"submit_verification", submit(func(c *store.VerificationCoverage) {
				c.Sources[0].Subjects = []core.VerificationSubject{{Kind: "ordinary", ObligationID: secret, ContractDigest: strings.Repeat("0", 64)}}
			})},
			"obligation id": {"submit_verification", submit(func(c *store.VerificationCoverage) { c.ObligationIDs = []string{secret} })},
		}
		for name, tc := range inputs {
			_, err, response := f.call(tc.tool, tc.input, true)
			var refusal *store.VerificationRefusal
			if !errors.As(err, &refusal) || strings.Contains(err.Error(), secret) || strings.Contains(response.Body.String(), secret) || response.Code == http.StatusOK {
				t.Fatalf("%s refusal echoed caller text: MCP %v, REST %d %s", name, err, response.Code, response.Body)
			}
		}
	}
}
