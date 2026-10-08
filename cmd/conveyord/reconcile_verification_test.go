package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeVerificationReconciler struct {
	calls            []string
	claimErr, expErr error
	expired, limit   int
	deadline         time.Time
	hasDeadline      bool
}

func (f *fakeVerificationReconciler) ReconcileVerificationClaims(context.Context) (int, error) {
	f.calls = append(f.calls, "claims")
	return 0, f.claimErr
}

func (f *fakeVerificationReconciler) ExpireVerificationChunks(ctx context.Context, limit int) (int, error) {
	f.calls = append(f.calls, "expiry")
	f.limit = limit
	f.deadline, f.hasDeadline = ctx.Deadline()
	return f.expired, f.expErr
}

// TestReconcileWorkspaceVerification pins the reconcile-tick wiring of chunk
// expiry: claim reconciliation runs first, expiry receives the 100-row limit
// under a deadline of at most five seconds, a claim error still runs expiry,
// and errors are logged with the workspace while the helper returns so the
// tick continues (component-runtime; component-verification-evidence).
func TestReconcileWorkspaceVerification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fake     fakeVerificationReconciler
		wantLogs []string
	}{
		{"expired rows logged", fakeVerificationReconciler{expired: 3}, []string{"expired 3 verification upload chunk(s) in workspace demo"}},
		{"nothing expired", fakeVerificationReconciler{}, nil},
		{"claim error still expires", fakeVerificationReconciler{claimErr: errors.New("claims failed"), expired: 1}, []string{"reconcile verification claims in workspace demo: claims failed", "expired 1 verification upload chunk(s) in workspace demo"}},
		{"expiry error logged", fakeVerificationReconciler{expErr: errors.New("expiry failed")}, []string{"expire verification upload chunks in workspace demo: expiry failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			var logs []string
			reconcileWorkspaceVerification(t.Context(), &fake, "demo", func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})
			finished := time.Now()
			if strings.Join(fake.calls, ",") != "claims,expiry" {
				t.Fatalf("calls = %v", fake.calls)
			}
			if fake.limit != 100 {
				t.Fatalf("expiry limit = %d", fake.limit)
			}
			if !fake.hasDeadline || fake.deadline.After(finished.Add(store.VerificationChunkExpiryBudget)) || store.VerificationChunkExpiryBudget != 5*time.Second {
				t.Fatalf("expiry deadline = %v (set=%t)", fake.deadline, fake.hasDeadline)
			}
			if strings.Join(logs, "\n") != strings.Join(tc.wantLogs, "\n") {
				t.Fatalf("logs = %q, want %q", logs, tc.wantLogs)
			}
		})
	}
}
