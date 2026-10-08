package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// GrantVerificationFixture creates real operator authority through the shared
// store boundary. It is setup for lifecycle tests, never a permission bypass.
func GrantVerificationFixture(t *testing.T, b store.Backend, ctx context.Context, taskID, orderID, contextID, key string, subject core.VerificationSubject, actions []core.VerificationPermission) string {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	operator, owner := bootstrapOwner(t, Fixture{Backend: b, Context: ctx, Workspace: ws})
	if actions == nil {
		actions = []core.VerificationPermission{}
	}
	request := store.VerificationPermissionRequest{ContextID: contextID, RequestKey: key, Subject: subject, Actions: actions}
	receipt, err := b.ApplyVerification(operator, store.VerificationCommand{Access: store.VerificationAccess{UserID: owner.ID, TaskID: taskID, WorkOrderID: orderID}, Kind: store.VerificationGrantPermissions, ContextID: contextID, Key: key, Permissions: &request})
	requireOK(t, err)
	return receipt.ID
}

func runVerificationPermissions(t *testing.T, x Fixture) {
	t.Run("GrantBindingIntersectionAndLaunchReplay", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "network", Description: "read fixture API", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "network", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only fixture", Permissions: []verification.Permission{{Kind: "network", TargetBinding: "api"}}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		subject := core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		actions := []core.VerificationPermission{{Kind: "network", Binding: "api", Target: "https://api.test:443"}}
		grant := GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "network-grant", subject, actions)
		start := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "network-start", Attempt: &store.VerificationAttempt{Subject: subject, GrantID: grant, EffectiveActions: actions, LocalActions: actions, SafeInputs: map[string]json.RawMessage{}}}
		before := verificationPublicState(t, &v)
		for _, mutate := range []func(*store.VerificationAttempt){
			func(a *store.VerificationAttempt) { a.LocalActions = nil },
			func(a *store.VerificationAttempt) { a.EffectiveActions = []core.VerificationPermission{} },
			func(a *store.VerificationAttempt) { a.Subject.ContractDigest = "foreign" },
			func(a *store.VerificationAttempt) { a.GrantID = v.snapshot(t).Attempts[0].GrantID },
		} {
			copy := *start.Attempt
			mutate(&copy)
			bad := start
			bad.Attempt = &copy
			if _, err := x.Backend.ApplyVerification(v.ctx, v.command(bad)); err == nil {
				t.Fatal("invalid permission admission succeeded")
			}
			if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
				t.Fatal("permission refusal mutated aggregate")
			}
		}
		first := v.apply(t, start)
		second := v.apply(t, start)
		if first.ID != second.ID || !first.LaunchAuthorized || second.LaunchAuthorized {
			t.Fatal("start replay authorizes duplicate child")
		}
		var run store.VerificationAttempt
		for _, a := range v.snapshot(t).Attempts {
			if a.ID == first.ID {
				run = a
			}
		}
		if run.GrantSnapshot == nil || len(run.LocalActions) != 1 || !reflect.DeepEqual(run.EffectiveActions, actions) {
			t.Fatal("intersection snapshot absent")
		}
	})
	t.Run("ExpiredClaimCannotGrantOrReconcile", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := v.snapshot(t).Obligations[0]
		obligation.ID = "expiry-" + v.access.TaskID
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "expiry-run")
		op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "expiry-op", Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		order.LeaseExpiresAt = time.Now().Add(-time.Second)
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRenew))
		operator, owner := bootstrapOwner(t, x)
		cmd := store.VerificationCommand{Access: store.VerificationAccess{UserID: owner.ID, TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID}, Kind: store.VerificationGrantPermissions, ContextID: v.contextID, Key: "expired", Permissions: &store.VerificationPermissionRequest{Subject: v.subject, Actions: []core.VerificationPermission{}}}
		if _, err = x.Backend.ApplyVerification(operator, cmd); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("expired grant: %v", err)
		}
		reconciliation := v.command(store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "unknown", Source: "worker", CapturedAt: time.Now().UTC()}})
		if _, err = x.Backend.ApplyVerification(v.ctx, reconciliation); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("expired reconciliation: %v", err)
		}
		// Internal claim-loss reconciliation still cancels abandoned attempts.
		internal := store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
		if _, err = x.Backend.ReconcileVerificationClaims(internal); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("PermissionGrantAuthorityAndRevocation", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		snapshot := v.snapshot(t)
		run := snapshot.Attempts[0]
		if run.GrantSnapshot == nil || run.GrantID == "" || run.GrantSnapshot.WorkOrderAttemptID != v.access.WorkOrderAttemptID {
			t.Fatal("grant authority not frozen")
		}
		operator, owner := bootstrapOwner(t, x)
		request := store.VerificationPermissionRequest{ContextID: v.contextID, RequestKey: "revoke", RevokeGrantID: run.GrantID, Reason: "fixture revocation"}
		c := store.VerificationCommand{Access: store.VerificationAccess{TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID, UserID: owner.ID}, ContextID: v.contextID, Kind: store.VerificationRevokePermissions, Key: "revoke", Permissions: &request}
		forged := c
		forged.Access = v.access
		forged.Authority.OperateGates = true
		if _, err := x.Backend.ApplyVerification(v.ctx, forged); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("worker granted authority: %v", err)
		}
		r, err := x.Backend.ApplyVerification(operator, c)
		requireOK(t, err)
		again, err := x.Backend.ApplyVerification(operator, c)
		requireOK(t, err)
		if r.ID != again.ID {
			t.Fatal("revocation replay changed identity")
		}
		snapshot = v.snapshot(t)
		if snapshot.Attempts[0].State != "cancelled" || len(snapshot.PermissionRevocations) != 1 {
			t.Fatal("revocation did not atomically cancel")
		}
		e := v.envelope("after-revoke", "after-revoke", "no write")
		_, err = x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}}))
		if !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("write after revocation: %v", err)
		}
		start := v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "forged-start", Attempt: &store.VerificationAttempt{Subject: v.subject, GrantID: run.GrantID, EffectiveActions: []core.VerificationPermission{}, SafeInputs: map[string]json.RawMessage{}}})
		if _, err = x.Backend.ApplyVerification(v.ctx, start); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("revoked grant starts attempt: %v", err)
		}
		start.Attempt.GrantID = "absent"
		if _, err = x.Backend.ApplyVerification(v.ctx, start); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("missing grant starts attempt: %v", err)
		}
		grantID := GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "replacement", v.subject, nil)
		start.Attempt.GrantID = grantID
		start.Attempt.EffectiveActions = []core.VerificationPermission{{Kind: "network", Binding: "api", Target: "https://example.test"}}
		if _, err = x.Backend.ApplyVerification(v.ctx, start); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("effective actions escalate grant: %v", err)
		}
	})
	// component-verification-runner: an authorized operator receives a stable reason for
	// every claim-window and request refusal, while the sentinel is unchanged.
	t.Run("GrantRefusalReasons", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		operator, owner := bootstrapOwner(t, x)
		apply := func(contextID, key string, request store.VerificationPermissionRequest) (store.VerificationReceipt, string, error) {
			request.ContextID, request.RequestKey = contextID, key
			kind := store.VerificationGrantPermissions
			if request.RevokeGrantID != "" {
				kind = store.VerificationRevokePermissions
			}
			r, err := x.Backend.ApplyVerification(operator, store.VerificationCommand{Access: store.VerificationAccess{UserID: owner.ID, TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID}, Kind: kind, ContextID: contextID, Key: key, Permissions: &request})
			reason, _, _, _ := store.VerificationRefusalDetail(err)
			return r, reason, err
		}
		grant := store.VerificationPermissionRequest{Subject: v.subject, Actions: []core.VerificationPermission{}}
		unregistered := grant
		unregistered.Subject.ContractDigest = "unregistered"
		if _, reason, err := apply(v.contextID, "unregistered", unregistered); reason != store.VerificationRefusalSubjectUnregistered || err == nil {
			t.Fatalf("unregistered subject: %s %v", reason, err)
		}
		first, _, err := apply(v.contextID, "reasons", grant)
		requireOK(t, err)
		changed := grant
		changed.Actions = []core.VerificationPermission{{Kind: "operator_interaction", Binding: "conveyor"}}
		if _, reason, err := apply(v.contextID, "reasons", changed); reason != store.VerificationRefusalRequestConflict || !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("changed request: %s %v", reason, err)
		}
		if _, reason, err := apply(v.contextID, "revoke-unknown", store.VerificationPermissionRequest{RevokeGrantID: v.contextID + ":absent", Reason: "fixture"}); reason != store.VerificationRefusalGrantUnknown || !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("unknown revocation: %s %v", reason, err)
		}
		if _, _, err := apply(v.contextID, "revoke-blank", store.VerificationPermissionRequest{RevokeGrantID: first.ID}); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("blank revocation reason: %v", err)
		}
		_, _, err = apply(v.contextID, "revoke", store.VerificationPermissionRequest{RevokeGrantID: first.ID, Reason: "fixture"})
		requireOK(t, err)
		if _, reason, err := apply(v.contextID, "reasons", grant); reason != store.VerificationRefusalGrantRevoked || err == nil {
			t.Fatalf("revoked replay: %s %v", reason, err)
		}
		requireOK(t, func() error {
			_, err := ReleaseWorkerClaim(x.Context, x.Backend, v.access.WorkOrderID, v.access.Claim.WorkerID, core.WorkOrderRelease{SessionID: v.access.Claim.SessionID, Reason: "fixture handoff"})
			return err
		}())
		if _, reason, err := apply(v.contextID, "released", grant); reason != store.VerificationRefusalNotClaimed || !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("released claim: %s %v", reason, err)
		}
		if _, err = RecoverWorkOrder(x.Context, x.Backend, v.access.WorkOrderID, "reasons-recover", time.Hour); err != nil {
			t.Fatal(err)
		}
		claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "successor", ClientToken: "successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
		claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
		if _, err = ClaimWorkOrder(x.Context, x.Backend, v.access.WorkOrderID, claim); err != nil {
			t.Fatal(err)
		}
		if _, reason, err := apply(v.contextID, "successor", grant); reason != store.VerificationRefusalContextStale || !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("predecessor context: %s %v", reason, err)
		}
	})
}
