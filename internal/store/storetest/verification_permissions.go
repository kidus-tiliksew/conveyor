package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
}
