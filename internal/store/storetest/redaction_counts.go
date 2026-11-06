package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// countFixtureSecret is obviously fake and matches the GitHub token pattern.
const countFixtureSecret = "ghp_countfixturecountfixturecountfixture1"

func taskEventsOfKind(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) []core.Event {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	var matched []core.Event
	for _, event := range events {
		if event.Kind == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

func payloadRedactions(t *testing.T, payload []byte) (core.RedactionStats, bool) {
	t.Helper()
	var decoded struct {
		Redactions *core.RedactionStats `json:"redactions"`
	}
	requireOK(t, json.Unmarshal(payload, &decoded))
	if decoded.Redactions == nil {
		return core.RedactionStats{}, false
	}
	return *decoded.Redactions, true
}

// requireWriteRecorded proves the task's latest event of kind records exactly
// want, and that no event of the task holds the value. It reports kinds and
// counts and never a stored value.
func requireWriteRecorded(t *testing.T, st store.Store, ctx context.Context, taskID, kind string, want core.RedactionStats) {
	t.Helper()
	matched := taskEventsOfKind(t, st, ctx, taskID, kind)
	if len(matched) == 0 {
		t.Fatalf("no %s event", kind)
	}
	got, present := payloadRedactions(t, matched[len(matched)-1].Payload)
	if want.Total() == 0 {
		if present {
			t.Fatalf("%s carries redactions %+v for a write that removed nothing", kind, got)
		}
	} else if !present || got != want {
		t.Fatalf("%s redactions = %+v (present %v), want %+v", kind, got, present, want)
	}
	events, err := st.ListEvents(ctx, taskID)
	requireOK(t, err)
	for _, event := range events {
		if strings.Contains(string(event.Payload), countFixtureSecret) {
			t.Fatalf("event %s holds the secret", event.Kind)
		}
	}
}

// requireReplayRecordsNothing proves a replay of a recorded write appends no
// second count.
func requireReplayRecordsNothing(t *testing.T, v *verificationFixture, c store.VerificationCommand) {
	t.Helper()
	before, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
	requireOK(t, err)
	v.apply(t, c)
	after, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("a replay appended an event")
	}
}

// runRedactionCounts proves each stored-text site records what it removed on
// the event or record the same write appends, by count and class and never by
// value, and that a site whose write appends nothing keeps its behavior
// (req-security-boundaries AC-3.1).
func runRedactionCounts(t *testing.T, x Fixture) {
	writeEvidence := func(t *testing.T, v *verificationFixture, id string, value any) store.VerificationCommand {
		t.Helper()
		e := v.envelope(id, "batch-"+id, "placeholder")
		e.Payload = core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: e.CapturedAt, Value: core.JSONPayload(value)})
		c := store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}}
		v.apply(t, c)
		return c
	}
	t.Run("EvidenceSanitizeJSON", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		c := writeEvidence(t, &v, "counted", "observed "+countFixtureSecret)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationWriteEvidence, core.RedactionStats{Pattern: 1})
		requireReplayRecordsNothing(t, &v, c)
		if s := v.snapshot(t); strings.Contains(string(s.Evidence[0].Envelope.Payload), countFixtureSecret) {
			t.Fatal("the stored evidence holds the secret")
		}
		// Control: a write that removes nothing records no count.
		writeEvidence(t, &v, "clean", "plain observation")
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationWriteEvidence, core.RedactionStats{})
	})
	t.Run("EvidenceSanitizeJSONCountsNamedFields", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		writeEvidence(t, &v, "named", map[string]any{
			"password": "short-fixture-value",
			"url":      "https://user:fixture-value@example.test/path?token=fixture-query-value",
		})
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationWriteEvidence, core.RedactionStats{Pattern: 3})
		payload := string(v.snapshot(t).Evidence[0].Envelope.Payload)
		for _, value := range []string{"short-fixture-value", "fixture-query-value", "user:fixture-value"} {
			if strings.Contains(payload, value) {
				t.Fatal("the stored evidence holds a named value")
			}
		}
	})
	t.Run("ArtifactMetadataAndText", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		data := []byte("captured " + countFixtureSecret)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "counted", Content: data}})
		c := store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: "finalize-counted", Artifacts: []store.VerificationArtifactInput{{
			UploadID: "counted", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: verificationSHA(data),
			Name: countFixtureSecret + ".txt", SanitationRecord: "scrubbed " + countFixtureSecret, MaskingAttestation: "masked " + countFixtureSecret,
		}}}
		first := v.apply(t, c)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationWriteEvidence, core.RedactionStats{Pattern: 4})
		requireReplayRecordsNothing(t, &v, c)
		e := v.envelope("artifact-evidence", "attach-counted", "capture")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: first.ArtifactIDs[0], SHA256: first.ArtifactIDs[0], MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		artifact, content, err := x.Backend.ReadVerificationArtifact(v.ctx, v.access, e.ID, first.ArtifactIDs[0])
		requireOK(t, err)
		if strings.Contains(string(content), countFixtureSecret) || strings.Contains(artifact.Name, countFixtureSecret) {
			t.Fatal("the retained artifact holds the secret")
		}
	})
	t.Run("ArtifactJSON", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		data := core.JSONPayload(map[string]string{"observed": countFixtureSecret})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "counted-json", Content: data}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationFinalizeArtifact, Key: "finalize-json", Artifacts: []store.VerificationArtifactInput{{UploadID: "counted-json", Name: "capture.json", ContentType: "application/json", SizeBytes: int64(len(data)), SHA256: verificationSHA(data)}}})
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationWriteEvidence, core.RedactionStats{Pattern: 1})
	})
	t.Run("ObligationSanitizeJSON", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "counted-obligation", Description: "Observe " + countFixtureSecret, Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "counted-obligation", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
		c := store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation}
		v.apply(t, c)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationRegisterObligation, core.RedactionStats{Pattern: 1})
		requireReplayRecordsNothing(t, &v, c)
	})
	t.Run("AttemptStartSanitizeJSON", func(t *testing.T) {
		// Safe inputs and coverage reach the same start write, and each removal counts.
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "counted-start-" + v.access.TaskID, Description: "Start with inputs", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "counted-start", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		coverage := verificationFixtureCoverage(v.snapshot(t))
		coverage.Justification = "Covered by " + countFixtureSecret
		c := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "counted-start", Coverage: &coverage, Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{"note": core.JSONPayload("input " + countFixtureSecret)}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}}
		v.runID = v.apply(t, c).ID
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationStartAttempt, core.RedactionStats{Pattern: 2})
		// Control: a start that stores no new secret records no count.
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "plain"}})
		plain := verificationFixtureCoverage(v.snapshot(t))
		v.runID = v.apply(t, store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "plain-start", Coverage: &plain, Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}}).ID
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationStartAttempt, core.RedactionStats{})
	})
	t.Run("PermissionSanitizeJSON", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		var grant string
		for _, attempt := range v.snapshot(t).Attempts {
			if attempt.ID == v.runID {
				grant = attempt.GrantID
			}
		}
		operator, owner := bootstrapOwner(t, x)
		_, err := x.Backend.ApplyVerification(operator, store.VerificationCommand{Access: store.VerificationAccess{UserID: owner.ID, TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID}, Kind: store.VerificationRevokePermissions, ContextID: v.contextID, Key: "counted-revoke", Permissions: &store.VerificationPermissionRequest{RevokeGrantID: grant, Reason: "stopped for " + countFixtureSecret}})
		requireOK(t, err)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationRevokePermissions, core.RedactionStats{Pattern: 1})
	})
	t.Run("SealSanitizeJSON", func(t *testing.T) {
		v := newVerificationFixture(t, x, true)
		coverage := verificationFixtureCoverage(v.snapshot(t))
		coverage.Justification = "Sealed by " + countFixtureSecret
		c := store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}}
		v.apply(t, c)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationSeal, core.RedactionStats{Pattern: 1})
		requireReplayRecordsNothing(t, &v, c)
		if result := v.snapshot(t).Contexts[0].Result; result == nil || strings.Contains(result.Submission.Coverage.Justification, countFixtureSecret) {
			t.Fatal("the seal has no result or stores the secret")
		}
	})
	t.Run("AttemptExplanation", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		c := store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "stopped by " + countFixtureSecret}}
		v.apply(t, c)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationTerminateAttempt, core.RedactionStats{Pattern: 1})
		requireReplayRecordsNothing(t, &v, c)
		for _, attempt := range v.snapshot(t).Attempts {
			if strings.Contains(attempt.Explanation, countFixtureSecret) {
				t.Fatal("the stored explanation holds the secret")
			}
		}
	})
	t.Run("OperationScopeAndObservation", func(t *testing.T) {
		// The operation's own subject, so its state never holds another fixture's attempt.
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "counted-operation-" + v.access.TaskID, Description: "Apply one external mutation", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "counted-operation", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "fixture mutation", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "counted-operation-start")
		prepare := store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "counted-operation-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}")), IdempotencyScope: "scope " + countFixtureSecret, IdempotencyValidUntil: time.Now().UTC().Add(time.Hour)}}
		op := v.apply(t, prepare)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationPrepareOperation, core.RedactionStats{Pattern: 1})
		requireReplayRecordsNothing(t, &v, prepare)

		observe := store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "runner " + countFixtureSecret, ProviderReference: "reference " + countFixtureSecret, CapturedAt: time.Now().UTC()}}
		v.apply(t, observe)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationObserveOperation, core.RedactionStats{Pattern: 2})
		requireReplayRecordsNothing(t, &v, observe)
		// Reconciliation appends through the same observation write, and a clean
		// observation records no count.
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "uncertain provider state"}})
		reconcile := store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "unknown", Source: "inspection " + countFixtureSecret, CapturedAt: time.Now().UTC()}}
		v.apply(t, reconcile)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationReconcileOperation, core.RedactionStats{Pattern: 1})
		reconcile.Observation.State, reconcile.Observation.Source = "applied", "clean inspection"
		v.apply(t, reconcile)
		requireWriteRecorded(t, x.Backend, v.ctx, v.access.TaskID, "verification."+store.VerificationReconcileOperation, core.RedactionStats{})
		for _, stored := range v.snapshot(t).Operations {
			if strings.Contains(stored.IdempotencyScope, countFixtureSecret) {
				t.Fatal("the stored scope holds the secret")
			}
			for _, observation := range stored.History {
				if strings.Contains(observation.Source, countFixtureSecret) || strings.Contains(observation.ProviderReference, countFixtureSecret) {
					t.Fatal("the stored observation holds the secret")
				}
			}
		}
	})
	t.Run("RecoveryReason", func(t *testing.T) { runRecoveryReasonCount(t, x) })
	t.Run("RefusalAppendsNothing", func(t *testing.T) {
		// verification_records.go redacts a refusal's text for the caller. The
		// refused write is discarded, so no event or row can carry a count.
		v := newVerificationFixture(t, x)
		before, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		state := v.snapshot(t)
		_, err = x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "bad\x00key", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}}))
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("command was not refused: %v", err)
		}
		after, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
		requireOK(t, err)
		if len(after) != len(before) {
			t.Fatal("a refusal appended an event")
		}
		if got := v.snapshot(t); len(got.Attempts) != len(state.Attempts) || len(got.Evidence) != len(state.Evidence) {
			t.Fatal("a refusal changed the stored records")
		}
	})
	t.Run("MonitorRetryAudit", func(t *testing.T) { runMonitorRetryCount(t, x) })
	t.Run("MonitorPassFailureAppendsNothing", func(t *testing.T) { runMonitorPassFailureAppendsNothing(t, x) })
	t.Run("TranscriptRecord", func(t *testing.T) {
		// internal/inprocess adds a provider message's removals to the counts a
		// job's transcript record carries, so this record must keep them.
		task := newAggregateTask(t, x)
		ctx := x.Context
		job := core.Job{ID: task.ID + "-counted", TaskID: task.ID, Stage: core.StageTriage, State: core.JobPending}
		requireOK(t, x.Backend.CreateJob(ctx, job))
		want := core.RedactionStats{Exact: 1, Encoded: 2, Pattern: 3, Entropy: 4}
		requireOK(t, x.Backend.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "artifact://counted", RedactionStats: want}))
		got, err := x.Backend.GetTranscript(ctx, job.ID)
		requireOK(t, err)
		if got.RedactionStats != want {
			t.Fatalf("transcript counts = %+v, want %+v", got.RedactionStats, want)
		}
	})
}

// runRecoveryReasonCount records the operator's recovery reason on the one
// event its authorization appends, on every backend's way through sanitation.
func runRecoveryReasonCount(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	obligation := store.VerificationObligation{ID: "counted-recovery", Description: "Recover one external mutation", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "counted-recovery", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
	v.start(t, "counted-recovery-start")
	op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "counted-recovery-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "provider acknowledgement lost"}})
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	disposition := &store.VerificationRecoveryDisposition{ContextID: v.contextID, RunID: v.runID, OperationIDs: []string{op.ID}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied", Reason: "Operator inspected " + countFixtureSecret}
	recovery := store.WithVerificationRecovery(operator, disposition)
	_, err = RecoverWorkOrder(recovery, x.Backend, order.ID, "counted-recovery", time.Hour)
	requireOK(t, err)
	requireWriteRecorded(t, x.Backend, operator, v.access.TaskID, "verification.recovery_authorized", core.RedactionStats{Pattern: 1})
	_, err = RecoverWorkOrder(recovery, x.Backend, order.ID, "counted-recovery", time.Hour)
	requireOK(t, err)
	if matched := taskEventsOfKind(t, x.Backend, operator, v.access.TaskID, "verification.recovery_authorized"); len(matched) != 1 {
		t.Fatalf("recovery events after a replay = %d", len(matched))
	}
}

type monitorCountSource func(context.Context, time.Time) ([]monitor.Observation, error)

func (f monitorCountSource) Observations(ctx context.Context, since time.Time) ([]monitor.Observation, error) {
	return f(ctx, since)
}

func monitorCountService(x Fixture) *monitor.Service {
	return &monitor.Service{
		Store: x.Backend, WorkspaceID: x.Workspace, Enabled: true, RedactionSecrets: x.Backend,
		Repositories: map[string]struct{}{"conveyor": {}},
		Intake: func(context.Context, monitor.TaskRequest) (monitor.IntakeResult, error) {
			return monitor.IntakeResult{}, errors.New("intake refused " + countFixtureSecret)
		},
	}
}

func activityPayload(t *testing.T, activity monitor.Activity) []byte {
	t.Helper()
	payload, err := json.Marshal(activity.Payload)
	requireOK(t, err)
	return payload
}

// runMonitorRetryCount records a failed read's removals on monitor.retry and
// monitor.terminal_failure, the audit each attempt appends.
func runMonitorRetryCount(t *testing.T, x Fixture) {
	ctx := x.Context
	service := monitorCountService(x)
	failures := 0
	poller := monitor.Poller{Service: service, StartupWindow: time.Hour, Attempts: 2, RetryInitial: time.Millisecond, RetryMaximum: time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { return nil },
		Source: monitorCountSource(func(context.Context, time.Time) ([]monitor.Observation, error) {
			failures++
			if failures > 2 {
				return nil, nil
			}
			return nil, errors.New("read failed with " + countFixtureSecret)
		}),
	}
	err := poller.Poll(ctx)
	if err == nil || strings.Contains(err.Error(), countFixtureSecret) {
		t.Fatal("the failed poll returned no error or an unredacted one")
	}
	status, err := x.Backend.MonitorStatus(ctx, true, time.Now().UTC())
	requireOK(t, err)
	if strings.Contains(status.CurrentError, countFixtureSecret) {
		t.Fatal("the status error holds the secret")
	}
	seen := map[string]bool{}
	for _, activity := range status.Activity {
		payload := activityPayload(t, activity)
		if bytes.Contains(payload, []byte(countFixtureSecret)) {
			t.Fatalf("activity %s holds the secret", activity.Kind)
		}
		if activity.Kind != "monitor.retry" && activity.Kind != "monitor.terminal_failure" {
			continue
		}
		seen[activity.Kind] = true
		if got, present := payloadRedactions(t, payload); !present || got != (core.RedactionStats{Pattern: 1}) {
			t.Fatalf("%s redactions = %+v (present %v)", activity.Kind, got, present)
		}
	}
	if !seen["monitor.retry"] || !seen["monitor.terminal_failure"] {
		t.Fatalf("audits seen = %v", seen)
	}
	// Control: a failed read that removes nothing records no count.
	poller.Attempts = 1
	failures = 0
	poller.Source = monitorCountSource(func(context.Context, time.Time) ([]monitor.Observation, error) {
		return nil, errors.New("read failed plainly")
	})
	if err := poller.Poll(ctx); err == nil {
		t.Fatal("the failed poll returned no error")
	}
	status, err = x.Backend.MonitorStatus(ctx, true, time.Now().UTC())
	requireOK(t, err)
	last := status.Activity[len(status.Activity)-1]
	if _, present := payloadRedactions(t, activityPayload(t, last)); present || last.Kind != "monitor.terminal_failure" {
		t.Fatalf("a clean failure recorded a count on %s", last.Kind)
	}
}

// runMonitorPassFailureAppendsNothing pins why poller.go's pass failure keeps
// its behavior: its one write replaces the monitor_status row and appends
// nothing that could carry a count, and the text it stores is still redacted.
func runMonitorPassFailureAppendsNothing(t *testing.T, x Fixture) {
	ctx := x.Context
	activity := func() int {
		status, err := x.Backend.MonitorStatus(ctx, true, time.Now().UTC())
		requireOK(t, err)
		return len(status.Activity)
	}
	before := activity()
	requireOK(t, x.Backend.RecordMonitorFailure(ctx, "", "pass failed", time.Now().UTC().Add(time.Minute)))
	if activity() != before {
		t.Fatal("RecordMonitorFailure appended activity")
	}
	service := monitorCountService(x)
	poller := monitor.Poller{Service: service, StartupWindow: time.Hour, Source: monitorCountSource(func(context.Context, time.Time) ([]monitor.Observation, error) {
		return []monitor.Observation{{Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "counted-pass", CommitSHA: "counted-pass", SourceURL: "https://example.test/commit/counted-pass"}}, nil
	})}
	err := poller.Poll(ctx)
	if err == nil || strings.Contains(err.Error(), countFixtureSecret) {
		t.Fatal("the failed pass returned no error or an unredacted one")
	}
	status, err := x.Backend.MonitorStatus(ctx, true, time.Now().UTC())
	requireOK(t, err)
	if status.CurrentError == "" || strings.Contains(status.CurrentError, countFixtureSecret) {
		t.Fatal("the status error is empty or holds the secret")
	}
	// Only what this pass appended: earlier failed reads in the workspace counted.
	for _, entry := range status.Activity[before:] {
		payload := activityPayload(t, entry)
		if bytes.Contains(payload, []byte(countFixtureSecret)) {
			t.Fatalf("activity %s holds the secret", entry.Kind)
		}
		if _, present := payloadRedactions(t, payload); present {
			t.Fatalf("activity %s carries a count from a pass failure", entry.Kind)
		}
	}
}
