package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// AC-7.2 / VK-6: a task identity alone does not authorize another context's
// mutations, and a content address alone does not authorize typed evidence.
func runVerificationScope(t *testing.T, x Fixture) {
	for _, separateContext := range []bool{false, true} {
		name := "OtherRun"
		if separateContext {
			name = "OtherContext"
		}
		t.Run("Operation"+name, func(t *testing.T) {
			original := newVerificationFixture(t, x)
			original.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
			current := original
			if separateContext {
				obligation := original.snapshot(t).Obligations[0]
				current.contextID = original.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "second-context", Context: &store.VerificationContext{Revisions: original.revisions, GoverningPins: original.pins}}).ID
				registered := current.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
				current.subject.ContractDigest = registered.Digest
			}
			current.start(t, "second-run")
			op := current.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "scope-operation-" + current.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("input"))}})
			observe := store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "fixture", CapturedAt: time.Now().UTC()}}
			current.apply(t, observe)
			before := current.snapshot(t)
			events, err := x.Backend.ListEvents(current.ctx, current.access.TaskID)
			requireOK(t, err)
			for _, state := range []string{"dispatching", "completed", "outcome_unknown", "applied", "not_applied", "unknown"} {
				observe.Observation.State = state
				receipt, err := x.Backend.ApplyVerification(original.ctx, original.command(observe))
				if !errors.Is(err, store.ErrVerificationAccess) || !reflect.DeepEqual(receipt, store.VerificationReceipt{}) {
					t.Fatalf("foreign %s observation: receipt=%+v err=%v", state, receipt, err)
				}
			}
			if !reflect.DeepEqual(before, current.snapshot(t)) {
				t.Fatal("foreign observation changed operation history")
			}
			after, err := x.Backend.ListEvents(current.ctx, current.access.TaskID)
			requireOK(t, err)
			if !reflect.DeepEqual(events, after) {
				t.Fatal("foreign observation appended an event")
			}
			current.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
			observe.Observation.State = "not_applied"
			current.apply(t, observe) // Reconciliation of the original terminal run remains valid.
		})
	}
	t.Run("RetryContextOwnership", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
		other := v
		other.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "other-retry-context", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}}).ID
		ctx, owner := bootstrapOwner(t, x)
		approval := other.command(store.VerificationCommand{Kind: store.VerificationAuthorizeRetry, Attempt: &store.VerificationAttempt{Explanation: "Inspect original attempt before replay"}})
		approval.Access.UserID = owner.ID
		approval.Access.Claim = core.WorkOrderClaimIdentity{}
		approval.Access.ClientToken = ""
		before := v.snapshot(t)
		receipt, err := x.Backend.ApplyVerification(ctx, approval)
		if !errors.Is(err, store.ErrVerificationAccess) || !reflect.DeepEqual(receipt, store.VerificationReceipt{}) {
			t.Fatalf("cross-context retry: receipt=%+v err=%v", receipt, err)
		}
		if !reflect.DeepEqual(before, v.snapshot(t)) {
			t.Fatal("cross-context retry changed recovery authorizations")
		}
		approval.ContextID = v.contextID
		approval.Access.WorkOrderAttemptID = "another-attempt"
		if _, err = x.Backend.ApplyVerification(ctx, approval); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("cross-work-order-attempt retry: %v", err)
		}
		approval.Access.WorkOrderAttemptID = v.access.WorkOrderAttemptID
		first, err := x.Backend.ApplyVerification(ctx, approval)
		requireOK(t, err)
		again, err := x.Backend.ApplyVerification(ctx, approval)
		requireOK(t, err)
		if first.ID == "" || first.ID != again.ID {
			t.Fatal("authorized retry lost idempotency")
		}
	})
	for _, typedFirst := range []bool{false, true} {
		name := "ContextFirst"
		if typedFirst {
			name = "TypedFirst"
		}
		t.Run("SharedArtifact"+name, func(t *testing.T) {
			v := newVerificationFixture(t, x)
			data := []byte("shared verification content " + v.runID)
			hash := verificationSHA(data)
			createContext := func() {
				a, err := x.Backend.CreateArtifact(v.ctx, core.Artifact{TaskID: v.access.TaskID, Name: "shared.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext}, data)
				requireOK(t, err)
				if a.ID != hash {
					t.Fatal("context artifact content identity changed")
				}
			}
			if !typedFirst {
				createContext()
			}
			v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "shared-upload", Content: data}})
			e := v.envelope("shared-"+v.runID, "shared-batch", "observed")
			e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}
			v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "shared-upload", Name: "shared.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: hash}}})
			if typedFirst {
				createContext()
			}
			a, content, err := x.Backend.GetArtifact(v.ctx, hash)
			if !errors.Is(err, store.ErrVerificationAccess) || !reflect.DeepEqual(a, core.Artifact{}) || len(content) != 0 {
				t.Fatalf("generic shared-content read disclosed typed evidence: artifact=%+v err=%v", a, err)
			}
			a, content, err = x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, hash)
			requireOK(t, err)
			if a.Role != core.ArtifactRoleTypedVerificationEvidence || a.Role.ModelInputEligible() || !bytes.Equal(content, data) {
				t.Fatal("authorized typed read lost its role or content")
			}
		})
	}
}
