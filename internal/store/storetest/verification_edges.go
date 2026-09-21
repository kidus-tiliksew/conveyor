package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func runVerificationEdges(t *testing.T, v *verificationFixture) {
	t.Run("LinksAndDuplicateJSON", func(t *testing.T) {
		a := v.envelope("cycle-a", "cycle", "unused")
		b := v.envelope("cycle-b", "cycle", "unused")
		assertion := func(e *core.VerificationEvidence, to string) {
			e.Type = "assertion_result"
			e.Payload = core.JSONPayload(core.AssertionResultPayload{AssertionID: e.ID, Text: "Observed value", Expected: "yes", Actual: "yes", Outcome: "pass", Supporting: []core.VerificationReference{{EvidenceID: to}}})
		}
		assertion(&a, b.ID)
		assertion(&b, a.ID)
		command := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "cycle", Evidence: []json.RawMessage{verificationBytes(a), verificationBytes(b)}})
		before := v.snapshot(t)
		if _, err := v.x.Backend.ApplyVerification(v.ctx, command); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("cycle: %v", err)
		}
		if !reflect.DeepEqual(before, v.snapshot(t)) {
			t.Fatal("cycle partially persisted")
		}
		a.ID = "link-foreign"
		a.SubmissionKey = "foreign-link"
		assertion(&a, "other-task-evidence")
		command = v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: a.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(a)}})
		if _, err := v.x.Backend.ApplyVerification(v.ctx, command); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("foreign link: %v", err)
		}
		a.ID = "link-valid"
		a.SubmissionKey = "valid-link"
		assertion(&a, "evidence-one")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: a.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(a)}})
		if len(v.snapshot(t).Links) != 1 {
			t.Fatal("supporting link not retained")
		}
		raw := verificationBytes(a)
		raw = append(raw[:len(raw)-1:len(raw)-1], []byte(`,"payload":{}}`)...)
		if _, err := v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "duplicate", Evidence: []json.RawMessage{raw}})); err == nil {
			t.Fatal("duplicate JSON fields survived redaction")
		}
	})
	t.Run("ChunkExpiry", func(t *testing.T) {
		chunk := store.VerificationUploadChunk{UploadID: "expiring", Content: []byte("temporary")}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})
		expired := store.WithVerificationClockForTest(v.ctx, func() time.Time { return time.Now().UTC().Add(25 * time.Hour) })
		if _, err := v.x.Backend.ApplyVerification(expired, v.command(store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("expired upload replay: %v", err)
		}
		_, err := v.x.Backend.ApplyVerification(expired, v.command(store.VerificationCommand{Kind: store.VerificationExpireChunks}))
		requireOK(t, err)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &chunk})
		// Expiry removes incomplete staging only, never accepted evidence/artifacts.
		if len(v.snapshot(t).Evidence) < 2 {
			t.Fatal("expiry removed durable evidence")
		}
	})
	t.Run("SecretSourceAndArtifactRedaction", func(t *testing.T) {
		key := bytes.Repeat([]byte{41}, 32)
		secret := "verification-private-credential-fixture"
		v.x.Backend.ConfigureForgeTokenEncryptionKey(key)
		_, err := v.x.Backend.StoreWorkspaceGitHubApp(v.ctx, v.x.Workspace, core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "fixture", ClientID: "fixture"}, PrivateKey: secret})
		requireOK(t, err)
		e := v.envelope("secret-evidence", "secret-batch", secret)
		c := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		before := v.snapshot(t)
		events, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		v.x.Backend.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{42}, 32))
		_, err = v.x.Backend.ApplyVerification(v.ctx, c)
		if !errors.Is(err, store.ErrForgeTokenDecrypt) {
			t.Fatalf("secret source did not fail closed: %v", err)
		}
		if !reflect.DeepEqual(before, v.snapshot(t)) {
			t.Fatal("secret failure changed evidence")
		}
		after, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		if len(events) != len(after) {
			t.Fatal("secret failure appended event")
		}
		v.x.Backend.ConfigureForgeTokenEncryptionKey(key)
		v.apply(t, c)
		data := []byte("capture " + secret)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "redacted-upload", Content: data}})
		e = v.envelope("redacted-artifact-evidence", "redacted-artifact-batch", "observed")
		original := verificationSHA(data)
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: original, SHA256: original, MediaType: "text/plain"}}
		receipt := v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "redacted-upload", Name: "capture.txt", ContentType: "text/plain", SHA256: original, SizeBytes: int64(len(data))}}})
		if len(receipt.ArtifactIDs) != 1 || receipt.ArtifactIDs[0] == original {
			t.Fatal("retained artifact used pre-redaction hash")
		}
		_, content, err := v.x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, receipt.ArtifactIDs[0])
		requireOK(t, err)
		if strings.Contains(string(content), secret) || verificationSHA(content) != receipt.ArtifactIDs[0] {
			t.Fatal("artifact redaction/integrity mismatch")
		}

		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "different-transport", Content: data}})
		replay := v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "different-transport", Name: "capture.txt", ContentType: "text/plain", SHA256: original, SizeBytes: int64(len(data))}}})
		if !reflect.DeepEqual(receipt, replay) {
			t.Fatal("upload transport identity changed sanitized submission receipt")
		}
		if _, _, err = v.x.Backend.GetArtifact(v.ctx, receipt.ArtifactIDs[0]); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("legacy hash-only artifact read: %v", err)
		}
		if _, err = v.x.Backend.CreateArtifact(v.ctx, core.Artifact{TaskID: v.access.TaskID, Role: core.ArtifactRoleTypedVerificationEvidence, ContentType: "text/plain"}, []byte("unclaimed")); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("unclaimed typed artifact write: %v", err)
		}
	})
}

func runVerificationRetryAndSeal(t *testing.T, v *verificationFixture) {
	t.Run("ReconciliationAndDistinctRuns", func(t *testing.T) {
		snapshot := v.snapshot(t)
		op := snapshot.Operations[0]
		observation := store.VerificationOperationObservation{State: "not_applied", Source: "read-only provider lookup", CapturedAt: time.Now().UTC()}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &observation})
		oldRun := v.runID
		v.start(t, "start-second")
		if oldRun == v.runID {
			t.Fatal("retry relabeled original run")
		}
		data := []byte("sanitized fixture artifact")
		hash := verificationSHA(data)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "second-upload", Content: data}})
		e := v.envelope("second-run-evidence", "artifact-batch", "same bytes, new capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "second-upload", Name: "capture.txt", ContentType: "text/plain", SHA256: hash, SizeBytes: int64(len(data))}}})
		snapshot = v.snapshot(t)
		runs := map[string]bool{}
		for _, record := range snapshot.Evidence {
			for _, a := range record.Envelope.Artifacts {
				if a.ArtifactID == hash {
					runs[record.Envelope.RunID] = true
				}
			}
		}
		if len(runs) != 2 {
			t.Fatal("content deduplication combined capture histories")
		}
		before := v.runID
		v.start(t, "start-second")
		if v.runID != before {
			t.Fatal("start replay created another run")
		}
	})
	t.Run("OperatorObservation", func(t *testing.T) {
		e := v.envelope("forged-operator", "forged-operator", "unused")
		e.Type = "operator_observation"
		e.CapturedBy = core.VerificationCaptureActor{Identity: "worker:worker", Kind: "operator", Version: "1", Attribution: "authenticated_operator"}
		e.Payload = core.JSONPayload(core.OperatorObservationPayload{OperatorID: e.CapturedBy.Identity, Fact: "observed", CapturedAt: e.CapturedAt, Supporting: []core.VerificationReference{{EvidenceID: "second-run-evidence"}}})
		c := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Authority: core.VerificationEvidenceAuthority{OperateGates: true}})
		if _, err := v.x.Backend.ApplyVerification(v.ctx, c); err == nil {
			t.Fatal("worker invented operator capability")
		}
		ctx, owner := bootstrapOwner(t, v.x)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
		e.ID = "operator-evidence"
		e.SubmissionKey = "operator-batch"
		e.SubmittedBy = store.UserActorID(owner.ID)
		e.CapturedBy.Identity = e.SubmittedBy
		e.Payload = core.JSONPayload(core.OperatorObservationPayload{OperatorID: e.SubmittedBy, Fact: "observed independently", CapturedAt: e.CapturedAt, Supporting: []core.VerificationReference{{EvidenceID: "second-run-evidence"}}})
		c = v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		c.Access.UserID = owner.ID
		c.Access.Claim = core.WorkOrderClaimIdentity{}
		c.Access.ClientToken = ""
		_, err := v.x.Backend.ApplyVerification(ctx, c)
		requireOK(t, err)
		_, err = v.x.Backend.ReadVerification(ctx, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
		requireOK(t, err)
	})

	t.Run("OperatorAuthorizedRetry", func(t *testing.T) {
		savedSubject, savedRun := v.subject, v.runID
		defer func() { v.subject = savedSubject; v.runID = savedRun }()
		obligation := store.VerificationObligation{ID: "operator-required", Description: "An external observation requiring recovery approval", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "operator-required", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []string{}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "operator-first")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "interrupted"}})
		retry := v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "operator-retry", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}})
		if _, err := v.x.Backend.ApplyVerification(v.ctx, retry); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("unapproved retry: %v", err)
		}
		approval := v.command(store.VerificationCommand{Kind: store.VerificationAuthorizeRetry, Attempt: &store.VerificationAttempt{Explanation: "Provider state inspected; authorize one retry"}})
		if _, err := v.x.Backend.ApplyVerification(v.ctx, approval); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("worker authorized itself: %v", err)
		}
		ctx, owner := bootstrapOwner(t, v.x)
		approval.Access.UserID = owner.ID
		approval.Access.Claim = core.WorkOrderClaimIdentity{}
		approval.Access.ClientToken = ""
		receipt, err := v.x.Backend.ApplyVerification(ctx, approval)
		requireOK(t, err)
		retry.Attempt.ReplayAuthorizationID = receipt.ID
		started, err := v.x.Backend.ApplyVerification(v.ctx, retry)
		requireOK(t, err)
		v.runID = started.ID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "second interruption"}})
		retry.Key = "operator-third"
		retry.RunID = v.runID
		if _, err = v.x.Backend.ApplyVerification(v.ctx, retry); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("authorization reused: %v", err)
		}
	})
	t.Run("SealAndLegacyProvenance", func(t *testing.T) {
		legacy, err := v.x.Backend.CreateArtifact(v.ctx, core.Artifact{TaskID: v.access.TaskID, Name: "legacy.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext}, []byte("legacy context"))
		requireOK(t, err)
		before := v.snapshot(t)
		if _, err = v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("sealed running attempt: %v", err)
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "fixture ends without acceptance"}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal})
		if _, err = v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "late-start", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("sealed context accepted mutation: %v", err)
		}
		after := v.snapshot(t)
		if after.Contexts[0].SealedAt == nil || len(after.Evidence) != len(before.Evidence) {
			t.Fatal("seal lost evidence")
		}
		original, _, err := v.x.Backend.GetArtifact(v.ctx, legacy.ID)
		requireOK(t, err)
		if original.Role != legacy.Role {
			t.Fatal("legacy role rewritten")
		}
		for _, record := range after.Evidence {
			for _, a := range record.Envelope.Artifacts {
				if a.ArtifactID == legacy.ID {
					t.Fatal("legacy artifact acquired invented run provenance")
				}
			}
		}
	})
}
