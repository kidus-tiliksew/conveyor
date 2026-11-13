package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// TestVerificationSurfacesRefuseCallerAuthorization pins that neither MCP nor
// REST accepts a caller-named operator authorization. The published tool
// schemas omit replay_authorization_id, and a start, an operation write, or a
// reconciliation that sends it is refused with verification_request_shape on
// that field (component-verification-service invariants 1, 19, and 25;
// DEC-133(3)).
func TestVerificationSurfacesRefuseCallerAuthorization(t *testing.T) {
	names := map[string]bool{"start_verification_attempt": true, "prepare_verification_operation": true, "reconcile_verification_operation": true}
	for _, tool := range verificationMCPTools() {
		name := tool["name"].(string)
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["replay_authorization_id"]; ok {
			t.Fatalf("%s publishes replay_authorization_id", name)
		}
		delete(names, name)
	}
	if len(names) != 0 {
		t.Fatalf("tools missing from the MCP list: %v", names)
	}

	s, ctx, id := verificationHTTPFixture(t)
	envelope := map[string]any{"workspace_id": "demo", "work_order_id": id, "session_id": "session", "client_token": "token"}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	prepare := map[string]any{"request_key": "prepare"}
	for k, v := range envelope {
		prepare[k] = v
	}
	result, err := s.callMCPTool(request, "prepare_verification", prepare)
	if err != nil {
		t.Fatal(err)
	}
	contextID := result.(store.VerificationSnapshot).Contexts[0].ID
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: strings.Repeat("b", 64)}
	environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}
	coverage := store.VerificationCoverage{ObligationIDs: []string{"ordinary"}, Justification: "Cover the fixture source", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Disposition: "covered", Explanation: "Exercise observes the requirement", Subjects: []core.VerificationSubject{subject}}}}
	requests := map[string]any{
		"start_verification_attempt":       workorder.VerificationStartRequest{Coverage: coverage, ContextID: contextID, StartKey: "caller-named", Subject: subject, GrantID: "grant", EffectiveActions: []core.VerificationPermission{}, Environment: environment, SafeInputs: map[string]json.RawMessage{}},
		"prepare_verification_operation":   workorder.VerificationOperationRequest{ContextID: contextID, RunID: "run", Action: "prepare", Key: "operation", StepID: "step", Target: "fixture", InputDigest: strings.Repeat("a", 64)},
		"reconcile_verification_operation": workorder.VerificationReconcileRequest{ContextID: contextID, OperationID: "operation", Outcome: "not_applied", Source: "provider read", CapturedAt: time.Now().UTC()},
	}
	for name, typed := range requests {
		raw, err := json.Marshal(typed)
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{}
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		for k, v := range envelope {
			args[k] = v
		}
		args["replay_authorization_id"] = "recovery:caller-named"
		_, err = s.callMCPTool(request, name, args)
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) || refusal.Code != store.VerificationCodeRequestShape || refusal.Field != "replay_authorization_id" {
			t.Fatalf("MCP %s with replay_authorization_id: %v, want %s on the field", name, err, store.VerificationCodeRequestShape)
		}
		response := verificationRESTCall(s, ctx, id, name, args)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("REST %s with replay_authorization_id: %d %s", name, response.Code, response.Body)
		}
		if !strings.Contains(response.Body.String(), store.VerificationCodeRequestShape) || !strings.Contains(response.Body.String(), "replay_authorization_id") {
			t.Fatalf("REST %s refusal does not name the field: %s", name, response.Body)
		}
	}
}
