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
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func verificationHTTPFixture(t *testing.T) (*Server, context.Context, string) {
	t.Helper()
	b := store.NewVolatileBackend()
	t.Cleanup(b.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://github.com/org/repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := b.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "verification-http", Workspace: "demo", Repo: "repo", Title: "verification", Branch: "conveyor/http", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
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
	ctx = context.WithValue(ctx, workerContextKey{}, core.Worker{ID: "fixture", Workspace: "demo"})
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
		if response.Code != 200 || !reflect.DeepEqual(left, right) {
			t.Fatalf("%s parity: %d %s", name, response.Code, response.Body)
		}
		return got
	}
	contextID := snapshot.Contexts[0].ID
	obligation := parity("register_verification_obligation", map[string]any{"context_id": contextID, "obligation_id": "observe", "description": "Observe fixture", "sources": []any{map[string]any{"document_id": "req-fixture", "version": 1, "section_id": "REQ-1"}}, "observation_procedure": "Read fixture state", "contract": map[string]any{"id": "observe", "kind": "observation", "argv": []string{}, "cwd": ".", "timeout_seconds": 30, "stages": []string{"verify"}, "prerequisites": []any{}, "permissions": []any{}, "inputs": []any{}, "required_assertions": []string{}, "retry_policy": "safe_to_replay", "safety_basis": "read only", "operations": []any{}, "evidence_outputs": []any{map[string]any{"type": "state_observation", "schema_version": 1, "minimum_items": 1}}, "supports": []any{}}}).(store.VerificationReceipt)
	run := parity("start_verification_attempt", map[string]any{"context_id": contextID, "start_key": "start", "subject": core.VerificationSubject{Kind: "ordinary", ObligationID: "observe", ContractDigest: obligation.Digest}, "environment": core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, "effective_permissions": []any{}, "safe_inputs": map[string]any{}}).(store.VerificationReceipt)
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

	for _, name := range []string{"get_verification_context", "get_verification_publication", "register_verification_obligation", "start_verification_attempt", "report_verification_outcome", "submit_verification_evidence", "upload_verification_artifact", "read_verification_evidence", "prepare_verification"} {
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
			if !reflect.DeepEqual(props[field], shape) {
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
	if _, err = s.Store.(store.VerificationStore).ApplyVerification(ctx, store.VerificationCommand{Kind: store.VerificationSeal, Access: access, ContextID: sealed.Contexts[0].ID}); err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderCompleted
	if err = storetest.UpdateWorkOrder(ctx, s.Store, order, core.WorkOrderCmdSubmitVerification); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: order.TaskID + "-review-1", TaskID: order.TaskID, Stage: core.StageReview, State: core.JobPending}
	review := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: order.TaskID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: order.HeadSHA, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC()}
	if err = storetest.CreateReviewRound(ctx, s.Store, order.TaskID, []core.Job{job}, []core.WorkOrder{review}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimWorkOrder(ctx, s.Store, review.ID, core.WorkOrderClaim{WorkerID: "reviewer", ClaimantID: "reviewer", SessionID: "review-session", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: "worker:reviewer", Role: core.ActorWorker})
	ctx = context.WithValue(ctx, workerContextKey{}, core.Worker{ID: "reviewer", Workspace: "demo"})
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
