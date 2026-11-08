package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
		v.x.Backend.ConfigureGitHubAppKeyEncryptionKey(key)
		_, err := v.x.Backend.StoreWorkspaceGitHubApp(v.ctx, v.x.Workspace, core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "fixture", ClientID: "fixture"}, PrivateKey: secret})
		requireOK(t, err)
		e := v.envelope("secret-evidence", "secret-batch", secret)
		c := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		before := v.snapshot(t)
		events, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		v.x.Backend.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{42}, 32))
		_, err = v.x.Backend.ApplyVerification(v.ctx, c)
		if !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
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
		v.x.Backend.ConfigureGitHubAppKeyEncryptionKey(key)
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
		v.apply(t, store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &observation})
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
		e.CapturedBy = core.VerificationCaptureActor{Identity: "user:worker", Kind: "operator", Version: "1", Attribution: "authenticated_operator"}
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
		obligation := store.VerificationObligation{ID: "operator-required", Description: "An external observation requiring recovery approval", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "operator-required", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}}}
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
		if _, err := v.x.Backend.ApplyVerification(ctx, approval); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("unbound authorization bypassed recovery: %v", err)
		}

	})
	t.Run("SealAndLegacyProvenance", func(t *testing.T) {
		legacy, err := v.x.Backend.CreateArtifact(v.ctx, core.Artifact{TaskID: v.access.TaskID, Name: "legacy.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext}, []byte("legacy context"))
		requireOK(t, err)
		before := v.snapshot(t)
		submission := &store.VerificationSubmission{Outcome: "feedback", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Failed fixture requires code correction."}
		if _, err = v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: submission})); !errors.Is(err, store.ErrVerificationState) {
			t.Fatalf("sealed running attempt: %v", err)
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "fixture ends without acceptance"}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: submission})
		if _, err = v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "late-start", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}})); err == nil {
			t.Fatalf("sealed context accepted mutation: %v", err)
		}
		ctx, owner := bootstrapOwner(t, v.x)
		after, err := v.x.Backend.ReadVerification(ctx, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
		requireOK(t, err)
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

// runVerificationStartRefusals pins the correctable attempt-start refusals of
// an authenticated claim on every backend: each names its code and field and
// writes no attempt (component-verification-service "Correctable refusals").
func runVerificationStartRefusals(t *testing.T, x Fixture) {
	t.Run("VerificationStartRefusals", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}
		stale := v.subject
		stale.ContractDigest = strings.Repeat("0", 64)
		kit := core.VerificationSubject{Kind: "kit", KitID: "absent", KitVersion: "1", ExerciseID: "check", ContentDigest: strings.Repeat("b", 64)}
		before := len(v.snapshot(t).Attempts)
		for _, tc := range []struct {
			name, key, code, field string
			class                  error
			subject                core.VerificationSubject
		}{
			{"start key over 128 bytes", strings.Repeat("k", 129), store.VerificationCodeRequestShape, "start_key", store.ErrVerificationInvalid, v.subject},
			{"start key with NUL", "start\x00key", store.VerificationCodeRequestShape, "start_key", store.ErrVerificationInvalid, v.subject},
			{"stale contract digest", "stale-digest", store.VerificationCodeSubjectUnregistered, "subject", store.ErrVerificationState, stale},
			{"unselected kit subject", "unselected-kit", store.VerificationCodeSubjectUnregistered, "subject", store.ErrVerificationState, kit},
		} {
			command := v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: tc.key, Attempt: &store.VerificationAttempt{Subject: tc.subject, SafeInputs: map[string]json.RawMessage{}, Environment: environment}})
			_, err := x.Backend.ApplyVerification(v.ctx, command)
			var refusal *store.VerificationRefusal
			if !errors.As(err, &refusal) || refusal.Code != tc.code || refusal.Field != tc.field || !errors.Is(err, tc.class) {
				t.Fatalf("%s: got %v, want %s at %s", tc.name, err, tc.code, tc.field)
			}
			if strings.Contains(refusal.Rule, tc.key) {
				t.Fatalf("%s: rule echoed the start key: %q", tc.name, refusal.Rule)
			}
			if after := len(v.snapshot(t).Attempts); after != before {
				t.Fatalf("%s: refused start wrote an attempt (%d -> %d)", tc.name, before, after)
			}
		}
	})
}

// runVerificationEvidenceBinding: an authenticated claim holder whose
// envelope differs from its context or attempt gets a coded access refusal
// naming the first mismatched field, and an unknown evidence run is named
// without revealing whether it exists in another context
// (component-verification-service "Correctable refusals" and invariant 29;
// req-verification-evidence AC-1.3; req-verification-kits AC-7.2, AC-8.3).
func runVerificationEvidenceBinding(t *testing.T, x Fixture) {
	// The codes are spelled out so the suite pins the stable wire values.
	const (
		sentinel        = "binding-sentinel-7f3a"
		bindingMismatch = "verification_evidence_binding_mismatch"
		runUnknown      = "verification_evidence_run_unknown"
	)
	refused := func(t *testing.T, v *verificationFixture, c store.VerificationCommand, code, field string, hidden ...string) {
		t.Helper()
		before := v.snapshot(t)
		events, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		receipt, err := x.Backend.ApplyVerification(v.ctx, c)
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) || refusal.Code != code || refusal.Field != field || refusal.Rule == "" || refusal.Remedy == "" {
			t.Fatalf("refusal = %v, want %s at %s", err, code, field)
		}
		if !errors.Is(err, store.ErrVerificationAccess) || !reflect.DeepEqual(receipt, store.VerificationReceipt{}) {
			t.Fatalf("refusal class or receipt: receipt=%+v err=%v", receipt, err)
		}
		for _, value := range append(hidden, sentinel) {
			if value != "" && strings.Contains(err.Error(), value) {
				t.Fatalf("refusal disclosed value %q: %v", value, err)
			}
		}
		if !reflect.DeepEqual(before, v.snapshot(t)) {
			t.Fatal("refused evidence changed the verification snapshot")
		}
		after, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		if len(after) != len(events) {
			t.Fatal("refused evidence appended an event")
		}
	}
	v := newVerificationFixture(t, x)
	// The mismatched envelope is the batch's second item, so the field names its index.
	batch := func(key string, mutate func(*core.VerificationEvidence)) store.VerificationCommand {
		valid := v.envelope("binding-valid-"+key, key, "observed")
		e := v.envelope("binding-"+key, key, "observed")
		mutate(&e)
		return v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: key, Evidence: []json.RawMessage{verificationBytes(valid), verificationBytes(e)}})
	}
	expected := []string{v.x.Workspace, v.access.TaskID, v.access.WorkOrderID, v.access.WorkOrderAttemptID, v.contextID, v.runID, v.subject.ContractDigest, v.revisions[0].SHA}
	cases := []struct {
		name, field string
		mutate      func(*core.VerificationEvidence)
	}{
		{"workspace", "workspace_id", func(e *core.VerificationEvidence) { e.WorkspaceID = sentinel }},
		{"workspace omitted", "workspace_id", func(e *core.VerificationEvidence) { e.WorkspaceID = "" }},
		{"task", "task_id", func(e *core.VerificationEvidence) { e.TaskID = sentinel }},
		{"work order", "work_order_id", func(e *core.VerificationEvidence) { e.WorkOrderID = sentinel }},
		{"work-order attempt", "work_order_attempt_id", func(e *core.VerificationEvidence) { e.WorkOrderAttemptID = sentinel }},
		{"work-order attempt omitted", "work_order_attempt_id", func(e *core.VerificationEvidence) { e.WorkOrderAttemptID = "" }},
		{"context", "context_id", func(e *core.VerificationEvidence) { e.ContextID = sentinel }},
		{"run", "run_id", func(e *core.VerificationEvidence) { e.RunID = sentinel }},
		{"run is the work-order attempt", "run_id", func(e *core.VerificationEvidence) { e.RunID = v.access.WorkOrderAttemptID }},
		{"subject", "subject", func(e *core.VerificationEvidence) { e.Subject.ObligationID = sentinel }},
		{"subject omitted", "subject", func(e *core.VerificationEvidence) { e.Subject = core.VerificationSubject{} }},
		{"revisions", "revisions", func(e *core.VerificationEvidence) {
			e.Revisions = []core.VerificationRevision{{Repository: v.revisions[0].Repository, RemoteIdentity: v.revisions[0].RemoteIdentity, SHA: sentinel}}
		}},
		{"revisions omitted", "revisions", func(e *core.VerificationEvidence) { e.Revisions = nil }},
		{"governing pins", "governing_pins", func(e *core.VerificationEvidence) {
			e.GoverningPins = []core.VerificationPin{{Kind: "requirement", DocumentID: sentinel, Version: 1}}
		}},
		{"governing pins omitted", "governing_pins", func(e *core.VerificationEvidence) { e.GoverningPins = nil }},
		// Several mismatches name the first one in the binding order.
		{"first of several", "work_order_id", func(e *core.VerificationEvidence) {
			e.WorkOrderID, e.ContextID, e.GoverningPins = sentinel, sentinel, nil
		}},
	}
	for i, tc := range cases {
		t.Run("BindingMismatch/"+tc.name, func(t *testing.T) {
			c := batch(fmt.Sprintf("binding-%d", i), tc.mutate)
			refused(t, &v, c, bindingMismatch, "evidence[1]."+tc.field, expected...)
		})
	}
	t.Run("BindingExact", func(t *testing.T) {
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "binding-exact", Evidence: []json.RawMessage{verificationBytes(v.envelope("binding-exact", "binding-exact", "observed"))}})
	})
	// An absent run and a run of another context of the same claim give the
	// identical answer, so the refusal reveals nothing about other contexts.
	// The claim may open a second context only after its first attempt ends.
	w := newVerificationFixture(t, x)
	w.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
	other := w
	obligation := w.snapshot(t).Obligations[0]
	other.contextID = w.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "binding-other-context", Context: &store.VerificationContext{Revisions: w.revisions, GoverningPins: w.pins}}).ID
	registered := other.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	other.subject.ContractDigest = registered.Digest
	other.start(t, "binding-other-run")
	var answers []string
	for _, run := range []struct{ name, id string }{{"absent", sentinel}, {"work-order attempt", w.access.WorkOrderAttemptID}, {"other context", other.runID}} {
		t.Run("RunUnknown/"+run.name, func(t *testing.T) {
			e := w.envelope("binding-run-"+run.name, "binding-run", "observed")
			e.RunID = run.id
			c := w.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
			c.RunID = run.id
			refused(t, &w, c, runUnknown, "run_id", run.id, other.contextID)
			_, err := x.Backend.ApplyVerification(w.ctx, c)
			answers = append(answers, err.Error())
		})
	}
	if len(answers) != 3 || answers[0] != answers[1] || answers[1] != answers[2] {
		t.Fatalf("unknown-run answers differ: %q", answers)
	}
	t.Run("RunUnknownOtherKindsUnchanged", func(t *testing.T) {
		c := w.command(store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})
		c.RunID = other.runID
		_, err := x.Backend.ApplyVerification(w.ctx, c)
		var refusal *store.VerificationRefusal
		if !errors.Is(err, store.ErrVerificationAccess) || errors.As(err, &refusal) {
			t.Fatalf("outcome for another context's run: %v", err)
		}
	})
	t.Run("ForeignScopeStaysGeneric", func(t *testing.T) {
		e := v.envelope("binding-foreign", "binding-foreign", "observed")
		for name, mutate := range map[string]func(*store.VerificationCommand){
			"task":          func(c *store.VerificationCommand) { c.Access.TaskID = sentinel },
			"order":         func(c *store.VerificationCommand) { c.Access.WorkOrderID = sentinel },
			"context":       func(c *store.VerificationCommand) { c.ContextID = sentinel },
			"stale session": func(c *store.VerificationCommand) { c.Access.Claim.SessionID = sentinel },
			"wrong token":   func(c *store.VerificationCommand) { c.Access.ClientToken = sentinel },
		} {
			c := v.command(store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
			mutate(&c)
			c.RunID = sentinel
			_, err := x.Backend.ApplyVerification(v.ctx, c)
			var refusal *store.VerificationRefusal
			if !errors.Is(err, store.ErrVerificationAccess) || errors.As(err, &refusal) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
}
