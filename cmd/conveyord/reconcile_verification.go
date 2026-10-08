package main

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// workspaceVerificationReconciler is the work-order service surface the
// reconcile tick uses for verification housekeeping.
type workspaceVerificationReconciler interface {
	ReconcileVerificationClaims(context.Context) (int, error)
	ExpireVerificationChunks(context.Context, int) (int, error)
}

// reconcileWorkspaceVerification runs the verification steps of one
// workspace's reconcile tick: claim-loss reconciliation, then expiry of stale
// upload chunks. Each error is logged with the workspace and never ends the
// tick, and a claim error does not skip expiry. Expiry is bounded to
// store.VerificationChunkExpiryLimit rows under
// store.VerificationChunkExpiryBudget, so a backlog drains over later ticks
// (component-runtime; component-verification-evidence).
func reconcileWorkspaceVerification(ctx context.Context, r workspaceVerificationReconciler, workspaceID string, logf func(string, ...any)) {
	if _, err := r.ReconcileVerificationClaims(ctx); err != nil {
		logf("reconcile verification claims in workspace %s: %v", workspaceID, err)
	}
	expiryCtx, cancel := context.WithTimeout(ctx, store.VerificationChunkExpiryBudget)
	defer cancel()
	expired, err := r.ExpireVerificationChunks(expiryCtx, store.VerificationChunkExpiryLimit)
	if err != nil {
		logf("expire verification upload chunks in workspace %s: %v", workspaceID, err)
	}
	if expired != 0 {
		logf("expired %d verification upload chunk(s) in workspace %s", expired, workspaceID)
	}
}
