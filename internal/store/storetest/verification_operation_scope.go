package storetest

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runVerificationOperationKeyScope proves the narrowed operation reads keep the
// workspace-wide uniqueness rule: a logical key another task owns is still
// refused, a changed definition of the same key is still refused, and neither
// task's read carries the other task's rows (component-verification-service;
// component-persistence, Database read accounting).
func runVerificationOperationKeyScope(t *testing.T, x Fixture) {
	t.Run("OperationKeyScope", func(t *testing.T) {
		owner := newVerificationFixture(t, x)
		other := newVerificationFixture(t, x)
		if owner.access.TaskID == other.access.TaskID || owner.contextID == other.contextID {
			t.Fatal("fixtures share a task or context")
		}
		owner.start(t, "owner-start")
		other.start(t, "other-start")
		key := "shared-operation-key"
		definition := func(digest string) *store.VerificationOperation {
			return &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte(digest))}
		}
		owner.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: definition("owner definition")})

		// Another task's key is refused as an access failure, whichever
		// definition it carries.
		for _, digest := range []string{"other definition", "owner definition"} {
			_, err := other.x.Backend.ApplyVerification(other.ctx, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: definition(digest)})
			if !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("another task's key with the %q definition = %v, want the access refusal", digest, err)
			}
		}
		// The owning task's key with a changed definition is a conflict.
		_, err := owner.x.Backend.ApplyVerification(owner.ctx, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: definition("changed definition")})
		if !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("changed definition of the task's own key = %v, want the conflict refusal", err)
		}
		// The owning task still reads its own operation and the other task
		// reads none of it.
		if got := owner.snapshot(t).Operations; len(got) != 1 || got[0].Key != key {
			t.Fatalf("owning task operations = %+v, want the single %q operation", got, key)
		}
		if got := other.snapshot(t).Operations; len(got) != 0 {
			t.Fatalf("other task read %d operations of another task", len(got))
		}
	})
}
