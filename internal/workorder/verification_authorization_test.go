package workorder

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestVerificationRequestsRefuseCallerAuthorization pins that no verification
// request names an operator authorization. The server selects it, so a start,
// an operation write, or a reconciliation that sends replay_authorization_id
// is refused by strict decoding and writes nothing, and no request type
// declares the field (component-verification-service invariants 1, 19, and
// 25; DEC-133(3)).
func TestVerificationRequestsRefuseCallerAuthorization(t *testing.T) {
	for _, name := range []string{"start_verification_attempt", "prepare_verification_operation", "reconcile_verification_operation"} {
		request := VerificationRequestType(name)
		if request == nil {
			t.Fatalf("%s has no request type", name)
		}
		schema := core.VerificationJSONSchema(reflect.TypeOf(request))
		if _, ok := schema["properties"].(map[string]any)["replay_authorization_id"]; ok {
			t.Fatalf("%s request schema declares replay_authorization_id", name)
		}
	}
	f := newVerificationServiceFixture(t)
	snapshot := f.call(t, "prepare_verification", VerificationPrepareRequest{RequestKey: "prepare"}).(store.VerificationSnapshot)
	contextID := snapshot.Contexts[0].ID
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: strings.Repeat("b", 64)}
	environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}
	coverage := store.VerificationCoverage{ObligationIDs: []string{"ordinary"}, Justification: "Cover the fixture source", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Disposition: "covered", Explanation: "Exercise observes the requirement", Subjects: []core.VerificationSubject{subject}}}}
	// Each request is otherwise well formed, so only the removed field can
	// cause the refusal.
	requests := map[string]any{
		"start_verification_attempt":       VerificationStartRequest{Coverage: coverage, ContextID: contextID, StartKey: "caller-named", Subject: subject, GrantID: "grant", EffectiveActions: []core.VerificationPermission{}, Environment: environment, SafeInputs: map[string]json.RawMessage{}},
		"prepare_verification_operation":   VerificationOperationRequest{ContextID: contextID, RunID: "run", Action: "prepare", Key: "operation", StepID: "step", Target: "fixture", InputDigest: strings.Repeat("a", 64)},
		"reconcile_verification_operation": VerificationReconcileRequest{ContextID: contextID, OperationID: "operation", Outcome: "not_applied", Source: "provider read", CapturedAt: time.Now().UTC()},
	}
	cases := map[string]map[string]any{}
	for name, request := range requests {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		cases[name] = fields
	}
	for name, fields := range cases {
		// Control: without the field the request passes the shape check, and
		// any refusal comes from the store state.
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.s.Verification(f.ctx, f.o.ID, "session", "token", name, raw)
		var shape *store.VerificationRefusal
		if errors.As(err, &shape) && shape.Code == store.VerificationCodeRequestShape {
			t.Fatalf("control %s failed the shape check: %v", name, err)
		}
		fields["replay_authorization_id"] = "recovery:caller-named"
		raw, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		before := f.call(t, "get_verification_context", VerificationContextRequest{ContextID: contextID}).(store.VerificationSnapshot)
		_, err = f.s.Verification(f.ctx, f.o.ID, "session", "token", name, raw)
		var refusal *store.VerificationRefusal
		if !errors.Is(err, store.ErrVerificationInvalid) || !errors.As(err, &refusal) || refusal.Code != store.VerificationCodeRequestShape || refusal.Field != "replay_authorization_id" {
			t.Fatalf("%s with replay_authorization_id: %v, want %s naming the field", name, err, store.VerificationCodeRequestShape)
		}
		after := f.call(t, "get_verification_context", VerificationContextRequest{ContextID: contextID}).(store.VerificationSnapshot)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("refused %s changed the context", name)
		}
	}
}
