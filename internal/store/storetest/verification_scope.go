package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// AC-7.2 / VK-6: a task identity alone does not authorize another context's
// mutations, and a content address alone does not authorize typed evidence.
func runVerificationScope(t *testing.T, x Fixture) {
	t.Run("BoundedReadAndObservation", func(t *testing.T) { RunVerificationRead(t, x) })
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
			op := current.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "scope-operation-" + current.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
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
			observe.Kind = store.VerificationReconcileOperation
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
		if _, err := x.Backend.ApplyVerification(ctx, approval); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("unbound retry bypassed recovery: %v", err)
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

func runVerificationFinalization(t *testing.T, x Fixture) {
	t.Run("StandaloneFinalization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		data := []byte("standalone sanitized capture")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "standalone", Content: data}})
		input := store.VerificationArtifactInput{UploadID: "standalone", Name: "capture.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: verificationSHA(data)}
		command := store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: "finalize", Artifacts: []store.VerificationArtifactInput{input}}
		first := v.apply(t, command)
		again := v.apply(t, command)
		if first.ID != again.ID || len(first.ArtifactIDs) != 1 {
			t.Fatal("finalization retry changed receipt")
		}
		if _, _, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, "missing", first.ArtifactIDs[0]); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("unlinked read: %v", err)
		}
		command.Artifacts[0].Name = "changed"
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(command)); !errors.Is(err, store.ErrVerificationConflict) {
			t.Fatalf("finalization conflict: %v", err)
		}
		e := v.envelope("standalone-evidence", "attach", "capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: first.ArtifactIDs[0], SHA256: first.ArtifactIDs[0], MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		_, got, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, first.ArtifactIDs[0])
		requireOK(t, err)
		if string(got) != string(data) {
			t.Fatal("retained bytes changed")
		}
	})
}

func runVerificationClaimLoss(t *testing.T, x Fixture) {
	for _, state := range []string{"live", "expired", "deadline", "revoked", "replaced", "terminal"} {
		t.Run("ClaimLoss/"+state, func(t *testing.T) {
			v := newVerificationFixture(t, x)
			e := v.envelope("preserved-"+v.runID, "preserved", "observation")
			v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
			command := v.command(store.VerificationCommand{Kind: store.VerificationReconcileClaimLoss})
			if _, err := x.Backend.ApplyVerification(v.ctx, command); !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("ordinary caller: %v", err)
			}
			if state == "terminal" {
				v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
			}
			o, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
			requireOK(t, err)
			switch state {
			case "expired", "terminal":
				o.LeaseExpiresAt = time.Now().Add(-time.Second)
			case "deadline":
				o.ExecutionDeadline = time.Now().Add(-time.Second)
			case "revoked":
				o.State = core.WorkOrderCancelled
			case "replaced":
				o.AttemptID = "replacement-attempt"
			}
			if state == "revoked" {
				requireOK(t, UpdateWorkOrder(x.Context, x.Backend, o, core.WorkOrderCmdCancel))
			} else if state != "live" {
				requireOK(t, UpdateWorkOrder(x.Context, x.Backend, o))
			}
			ctx := store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
			if state != "live" {
				_, sweepErr := x.Backend.ReconcileVerificationClaims(ctx)
				requireOK(t, sweepErr)
			}
			first, err := x.Backend.ApplyVerification(ctx, command)
			if state == "live" {
				if !errors.Is(err, store.ErrVerificationState) {
					t.Fatalf("live claim: %v", err)
				}
				return
			}
			requireOK(t, err)
			again, err := x.Backend.ApplyVerification(ctx, command)
			requireOK(t, err)
			if !reflect.DeepEqual(first, again) {
				t.Fatal("reconciliation changed terminal receipt")
			}
			expected := "cancelled"
			if state == "terminal" {
				expected = "failed"
			}
			if first.State != expected {
				t.Fatalf("state %s", first.State)
			}
			userContext, owner := bootstrapOwner(t, x)
			retained, err := x.Backend.ReadVerification(userContext, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
			requireOK(t, err)
			if len(retained.Evidence) != 1 || retained.Evidence[0].Envelope.ID != e.ID {
				t.Fatal("claim loss discarded accepted evidence")
			}

			events, err := x.Backend.ListEvents(x.Context, v.access.TaskID)
			requireOK(t, err)
			count := 0
			for _, event := range events {
				if event.Kind == "verification.attempt.terminate" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("terminal events %d", count)
			}
			wrong := command
			wrong.Access.WorkOrderAttemptID = "foreign"
			if _, err := x.Backend.ApplyVerification(ctx, wrong); !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("foreign attempt: %v", err)
			}
		})
	}
}

func runVerificationLimits(t *testing.T, x Fixture) {
	t.Run("EvidenceAndChunkLimits", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "large", Content: make([]byte, (512<<10)+1)}})); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("oversized chunk: %v", err)
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "boundary", Content: bytes.Repeat([]byte("a"), 512<<10)}})
		var items []json.RawMessage
		for i := 0; i < 100; i++ {
			e := v.envelope(fmt.Sprintf("limit-%s-%d", v.runID, i), "hundred", "observed")
			items = append(items, verificationBytes(e))
		}
		receipt := v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "hundred", Evidence: items})
		if len(receipt.EvidenceIDs) != 100 {
			t.Fatal("100-item boundary lost evidence")
		}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "over", Evidence: append(items, items[0])})); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("101 items: %v", err)
		}
		items = nil
		for i := 0; i < 40; i++ {
			e := v.envelope(fmt.Sprintf("bytes-%d", i), "bytes", strings.Repeat("a", 220<<10))
			items = append(items, verificationBytes(e))
		}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "bytes", Evidence: items})); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("8 MiB limit: %v", err)
		}
		if len(v.snapshot(t).Evidence) != 100 {
			t.Fatal("rejected batch persisted partial evidence")
		}
	})
	t.Run("FinalizationSanitationAndIntegrity", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		var data bytes.Buffer
		requireOK(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))))
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "binary", Content: data.Bytes()}})
		input := store.VerificationArtifactInput{UploadID: "binary", Name: "capture.png", ContentType: "image/png", SHA256: verificationSHA(data.Bytes()), SizeBytes: int64(data.Len())}
		c := store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: "binary", Artifacts: []store.VerificationArtifactInput{input}}
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(c)); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("missing attestation: %v", err)
		}
		input.SanitationRecord = "capture sanitized"
		input.MaskingAttestation = "sensitive regions masked"
		input.SizeBytes++
		c.Artifacts[0] = input
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(c)); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("wrong size: %v", err)
		}
		input.SizeBytes--
		c.Artifacts[0] = input
		v.apply(t, c)
	})
	t.Run("ClaimLossOperationHistory", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "unique-" + v.access.TaskID, Description: "Observe claim loss", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "claim-loss", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "unique-start")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "operation-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		o, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
		requireOK(t, err)
		o.LeaseExpiresAt = time.Now().Add(-time.Second)
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, o))
		ctx := store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
		_, err = x.Backend.ReconcileVerificationClaims(ctx)
		requireOK(t, err)
		_, err = x.Backend.ReconcileVerificationClaims(ctx)
		requireOK(t, err)
		userContext, owner := bootstrapOwner(t, x)
		snapshot, err := x.Backend.ReadVerification(userContext, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
		requireOK(t, err)
		if len(snapshot.Operations) != 1 || len(snapshot.Operations[0].History) != 2 || snapshot.Operations[0].History[1].State != "outcome_unknown" {
			t.Fatalf("lost operation history: %+v", snapshot.Operations)
		}
	})
}

func runVerificationTerminalRace(t *testing.T, x Fixture) {
	t.Run("TerminalReportClaimLossRace", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		o, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
		requireOK(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}}))
			done <- err
		}()
		o.LeaseExpiresAt = time.Now().Add(-time.Second)
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, o))
		ctx := store.WithActor(x.Context, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
		_, err = x.Backend.ReconcileVerificationClaims(ctx)
		requireOK(t, err)
		if err = <-done; err != nil && !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("terminal report: %v", err)
		}
		userContext, owner := bootstrapOwner(t, x)
		snapshot, err := x.Backend.ReadVerification(userContext, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
		requireOK(t, err)
		if len(snapshot.Attempts) != 1 || snapshot.Attempts[0].EndedAt == nil || (snapshot.Attempts[0].State != "failed" && snapshot.Attempts[0].State != "cancelled") {
			t.Fatalf("race result: %+v", snapshot.Attempts)
		}
		events, err := x.Backend.ListEvents(x.Context, v.access.TaskID)
		requireOK(t, err)
		count := 0
		for _, e := range events {
			if e.Kind == "verification.attempt.terminate" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("terminal events %d", count)
		}
	})
}
