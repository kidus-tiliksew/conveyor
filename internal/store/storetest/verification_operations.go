package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func verificationPublicState(t *testing.T, v *verificationFixture) any {
	t.Helper()
	task, err := v.x.Backend.GetTask(v.ctx, v.access.TaskID)
	requireOK(t, err)
	order, err := v.x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	jobs, err := v.x.Backend.ListJobs(v.ctx, v.access.TaskID)
	requireOK(t, err)
	events, err := v.x.Backend.ListEvents(v.ctx, v.access.TaskID)
	requireOK(t, err)
	snapshot := v.snapshot(t)
	orders, err := v.x.Backend.ListTaskWorkOrders(v.ctx, v.access.TaskID)
	requireOK(t, err)
	return struct {
		Orders   []core.WorkOrder
		Task     core.Task
		Order    core.WorkOrder
		Jobs     []core.Job
		Events   []core.Event
		Snapshot store.VerificationSnapshot
	}{orders, task, order, jobs, events, snapshot}
}

func runVerificationOperations(t *testing.T, x Fixture) {
	t.Run("AppliedAndUnknownReconciliation", func(t *testing.T) { runVerificationReconciliation(t, x) })
	t.Run("OperatorCheckpoint", func(t *testing.T) { runVerificationCheckpoint(t, x) })
	t.Run("Checkpoints", func(t *testing.T) { runVerificationCheckpoints(t, x) })
	t.Run("SealedReviewAcceptance", func(t *testing.T) { runSealedVerificationReview(t, x) })
	t.Run("SealedReviewAuthorityDrift", func(t *testing.T) { runSealedVerificationReviewDrift(t, x) })
	t.Run("NextActionDispositionBinding", func(t *testing.T) { runNextActionDispositionBinding(t, x) })
	t.Run("NextActionGatedAttemptBinding", func(t *testing.T) { runNextActionGatedAttemptBinding(t, x) })
	t.Run("RetryGateScopedToVerifyOrder", func(t *testing.T) { runVerificationRetryGateScope(t, x) })
	t.Run("OneRecoveryDisposesEveryGatedSubject", func(t *testing.T) { runRecoveryDisposesEveryGatedSubject(t, x) })
	t.Run("OneRecoveryDisposesAttemptsAcrossContexts", func(t *testing.T) { runRecoveryDisposesAttemptsAcrossContexts(t, x) })
	t.Run("ServerSelectsRecoveryAuthorizations", func(t *testing.T) { runServerSelectsRecoveryAuthorizations(t, x) })
	t.Run("OperationAuthorizationServesOneReprepare", func(t *testing.T) { runOperationAuthorizationServesOneReprepare(t, x) })
	t.Run("OperatorRecoveryBindsSuccessorAndConsumesAuthorization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "recovery-" + v.access.TaskID, Description: "Recover one external mutation", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "recovery", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "operator-original")
		key := "recovery-operation-" + v.runID
		op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "provider acknowledgement lost"}})
		// Revoked grants remain historical evidence. They cannot block the
		// separate operator recovery command after claim loss (v4 recovery split).
		originalGrant := v.snapshot(t).Attempts
		for _, run := range originalGrant {
			if run.ID != v.runID {
				continue
			}
			operator, owner := bootstrapOwner(t, x)
			_, err := x.Backend.ApplyVerification(operator, store.VerificationCommand{Access: store.VerificationAccess{UserID: owner.ID, TaskID: v.access.TaskID, WorkOrderID: v.access.WorkOrderID}, Kind: store.VerificationRevokePermissions, ContextID: v.contextID, Key: "revoke-before-recovery", Permissions: &store.VerificationPermissionRequest{RevokeGrantID: run.GrantID, Reason: "stop execution"}})
			requireOK(t, err)
		}
		oldAccess, oldContext, oldRun := v.access, v.contextID, v.runID
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
		order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
		order.LeaseExpiresAt = time.Time{}
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
		disposition := &store.VerificationRecovery{Reason: "Operator inspected provider state and authorizes one retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: oldContext, RunID: oldRun, OperationIDs: []string{op.ID}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied"}}}
		operator, owner := bootstrapOwner(t, x)
		operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
		observer := v
		observer.ctx, observer.access.UserID = operator, owner.ID
		beforeRecovery := verificationPublicState(t, &observer)
		if _, err = RecoverWorkOrder(store.WithVerificationRecovery(v.ctx, disposition), x.Backend, order.ID, "operation-recovery", time.Hour); err == nil {
			t.Fatal("worker issued operator disposition")
		}
		if !reflect.DeepEqual(beforeRecovery, verificationPublicState(t, &observer)) {
			t.Fatal("worker recovery refusal changed aggregate")
		}
		recoveryContext := store.WithVerificationRecovery(operator, disposition)
		faultContext := store.WithVerificationFault(recoveryContext, func(step string) error {
			if step == "recovery" {
				return store.ErrVerificationState
			}
			return nil
		})
		if _, err = RecoverWorkOrder(faultContext, x.Backend, order.ID, "operation-recovery", time.Hour); err == nil {
			t.Fatal("recovery fault committed")
		}
		if !reflect.DeepEqual(beforeRecovery, verificationPublicState(t, &observer)) {
			t.Fatal("recovery fault partially committed")
		}
		_, err = RecoverWorkOrder(recoveryContext, x.Backend, order.ID, "operation-recovery", time.Hour)
		requireOK(t, err)
		_, err = RecoverWorkOrder(recoveryContext, x.Backend, order.ID, "operation-recovery", time.Hour)
		requireOK(t, err)
		changed := *disposition
		changed.Reason = "different disposition"
		beforeRecovery = verificationPublicState(t, &observer)
		if _, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, &changed), x.Backend, order.ID, "operation-recovery", time.Hour); err == nil {
			t.Fatal("recovery request replay changed inputs")
		}
		if !reflect.DeepEqual(beforeRecovery, verificationPublicState(t, &observer)) {
			t.Fatal("conflicting recovery request changed aggregate")
		}
		claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "successor", ClientToken: "successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
		order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
		requireOK(t, err)
		v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
		v.contextID, v.runID = "", ""
		contextReceipt := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}})
		v.contextID = contextReceipt.ID
		registered = v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject.ContractDigest = registered.Digest
		start := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "successor-start", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}}
		if _, err = x.Backend.ApplyVerification(v.ctx, v.command(start)); err == nil {
			t.Fatal("disposition silently replaced reconciliation")
		}
		GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "successor-reconciliation", v.subject, nil)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "not_applied", Source: "fresh provider read", CapturedAt: time.Now().UTC()}})
		assertOperationAuthorization(t, &observer, oldContext, op.ID, "")
		started := v.apply(t, start)
		v.runID = started.ID
		prepared := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		if prepared.ID != op.ID {
			t.Fatal("successor replaced operation identity")
		}
		assertOperationAuthorization(t, &observer, oldContext, op.ID, started.ID)
		before := verificationPublicState(t, &v)
		stale := store.VerificationCommand{Access: oldAccess, ContextID: oldContext, RunID: oldRun, Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "stale", CapturedAt: time.Now().UTC()}}
		if _, err = x.Backend.ApplyVerification(v.ctx, stale); err == nil {
			t.Fatal("stale claim dispatched")
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
			t.Fatal("stale refusal changed public state")
		}
		observation := &store.VerificationOperationObservation{State: "dispatching", Source: "successor runner", CapturedAt: time.Now().UTC()}
		c := store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: observation}
		if !v.apply(t, c).DispatchAuthorized || v.apply(t, c).DispatchAuthorized {
			t.Fatal("retry authorization was not one use")
		}
		refuseConsumedReconciliation(t, x, &v, &observer, op.ID)
		observation.State = "completed"
		observation.CapturedAt = time.Now().UTC()
		observation.ProviderReference = "provider-resource-42"
		v.apply(t, c)
		snapshot := v.snapshot(t)
		if len(snapshot.Operations) != 1 || snapshot.Operations[0].Original.ContextID != oldContext || snapshot.Operations[0].Original.RunID != oldRun || len(snapshot.Operations[0].Successors) != 1 {
			t.Fatal("successor lost original provenance")
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "a later safe step failed"}})
		start.Key = "replayed-authorization"
		if _, err = x.Backend.ApplyVerification(v.ctx, v.command(start)); err == nil {
			t.Fatal("used recovery authorization started another attempt")
		}
	})
	t.Run("OperationReceiptsAndSanitation", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		c := store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "op-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}}
		op := v.apply(t, c)
		if op.State != "registered" || op.DispatchAuthorized {
			t.Fatal("registration authorized dispatch")
		}
		c.Kind = store.VerificationObserveOperation
		c.Operation = &store.VerificationOperation{ID: op.ID}
		c.Observation = &store.VerificationOperationObservation{State: "dispatching", Source: "runner relay", CapturedAt: time.Now().UTC()}
		first := v.apply(t, c)
		retry := v.apply(t, c)
		if !first.DispatchAuthorized || retry.DispatchAuthorized {
			t.Fatal("dispatch receipt grants are not one use")
		}
		c.Observation = &store.VerificationOperationObservation{State: "completed", Source: "runner relay", CapturedAt: time.Now().UTC(), ProviderReference: "https://user:password@provider.invalid/resource/1?token=private#private"}
		v.apply(t, c)
		before := verificationPublicState(t, &v)
		c.Observation.State = "dispatching"
		if _, err := x.Backend.ApplyVerification(v.ctx, v.command(c)); err == nil {
			t.Fatal("completed action redispatched")
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
			t.Fatal("refused redispatch changed public state")
		}
		s := v.snapshot(t)
		data := string(core.JSONPayload(s.Operations))
		if strings.Contains(data, "password") || strings.Contains(data, "private") {
			t.Fatal("provider secret persisted")
		}
	})
	t.Run("NoKitSealingAndRefusalAtomicity", func(t *testing.T) {
		v := newVerificationFixture(t, x, true)
		// A fresh context explicitly assesses an empty ordinary-obligation set.
		v.contextID = ""
		v.runID = ""
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "no-kit", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": v.revisions[0].SHA, "state": "no_manifest"})}}})
		v.contextID = r.ID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}})
		valid := &store.VerificationSubmission{Outcome: "succeeded", Coverage: verificationFixtureCoverage(v.snapshot(t))}
		for _, bad := range []*store.VerificationSubmission{nil, {Outcome: "succeeded"}, {Outcome: "succeeded", Coverage: store.VerificationCoverage{ObligationIDs: []string{}}}, {Outcome: "succeeded", Coverage: store.VerificationCoverage{ObligationIDs: []string{"missing"}, Justification: "unregistered"}}} {
			before := verificationPublicState(t, &v)
			if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: bad})); err == nil {
				t.Fatal("invalid coverage sealed")
			}
			if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
				t.Fatal("coverage refusal changed public state")
			}
		}
		for _, step := range []string{"artifact", "evidence", "event", "queue"} {
			before := verificationPublicState(t, &v)
			ctx := store.WithVerificationFault(v.ctx, func(at string) error {
				if at == step {
					return store.ErrVerificationState
				}
				return nil
			})
			if _, err := x.Backend.ApplyVerification(ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: valid})); err == nil {
				t.Fatal("fault committed")
			}
			if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
				t.Fatalf("%s fault partially committed", step)
			}
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: valid})
		task, err := x.Backend.GetTask(v.ctx, v.access.TaskID)
		requireOK(t, err)
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		if task.NextStage != core.StageReview || order.State != core.WorkOrderCompleted || order.VerificationContextID != v.contextID {
			t.Fatal("seal did not atomically advance to review")
		}
		snapshot := v.snapshot(t)
		if snapshot.Contexts[0].Result.Disposition != "no_selected_kits" {
			t.Fatal("empty selection was represented as exercise success")
		}
	})
	// component-verification-service v6: discovery and selection validity gate
	// succeeded only; feedback and checkpoints stay reachable with no eligible kit.
	t.Run("NoEligibleKitOutcomes", func(t *testing.T) {
		prepare := func(t *testing.T, state string, kits []verification.KitReceipt, diagnostics []verification.Diagnostic) verificationFixture {
			v := newVerificationFixture(t, x, true)
			v.contextID, v.runID = "", ""
			r := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "no-eligible-" + state, Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": v.revisions[0].SHA, "state": state})}}})
			v.contextID = r.ID
			v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: kits, Diagnostics: diagnostics}, Subjects: []store.VerificationSubjectContract{}}})
			return v
		}
		refuse := func(t *testing.T, v *verificationFixture, submission store.VerificationSubmission, code string) {
			t.Helper()
			before := verificationPublicState(t, v)
			_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: &submission}))
			var refusal *store.VerificationRefusal
			if !errors.As(err, &refusal) || refusal.Code != code || refusal.Field == "" || refusal.Rule == "" {
				t.Fatalf("%s submission: %v, want %s", submission.Outcome, err, code)
			}
			if !reflect.DeepEqual(before, verificationPublicState(t, v)) {
				t.Fatalf("%s refusal changed public state", code)
			}
		}
		feedbackText := func(t *testing.T, v *verificationFixture) string {
			t.Helper()
			task, err := x.Backend.GetTask(v.ctx, v.access.TaskID)
			requireOK(t, err)
			if task.NextStage == core.StageVerify || task.NextStage == core.StageReview {
				t.Fatalf("feedback left next stage %s", task.NextStage)
			}
			events, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
			requireOK(t, err)
			text := ""
			for _, e := range events {
				if strings.HasPrefix(e.Kind, "intervention.") {
					text += string(e.Payload)
				}
			}
			return text
		}
		t.Run("MalformedDeclaration", func(t *testing.T) {
			invalid := []verification.KitReceipt{{KitID: "integrations", Pins: []verification.Pin{}, Eligibility: "invalid", Reasons: []verification.SelectionReason{{Code: "invalid_entry", Path: "manifest.kits[0].exercises[0].supports[0]", Message: "support must name an AC within a declared requirement pin"}}}}
			v := prepare(t, "malformed", invalid, []verification.Diagnostic{{Path: "discovery.conveyor", Message: "malformed"}, {Path: "discovery.conveyor.manifest.kits[0].exercises[0].supports[0]", Message: ".conveyor/kits/manifest.yaml: kit integrations, exercise go-test: support must name an AC within a declared requirement pin"}})
			coverage := verificationFixtureCoverage(v.snapshot(t))
			refuse(t, &v, store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeDiscoveryNotValid)
			refuse(t, &v, store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "declaration", RequiredAction: "fix"}, store.VerificationCodeCheckpointUngrounded)
			v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "feedback", Coverage: coverage, Feedback: "The kit declaration is invalid."}})
			if text := feedbackText(t, &v); !strings.Contains(text, "exercise go-test") || !strings.Contains(text, "supports[0]") {
				t.Fatalf("delivered feedback lost the declaration diagnostic: %s", text)
			}
			result := v.snapshot(t).Contexts[0].Result
			if result.Disposition != "discovery_failed" || result.Feedback == nil || result.Feedback.Grounds[0].Kind != store.VerificationFeedbackDeclarationDefect {
				t.Fatalf("feedback seal = %+v", result)
			}
		})
		for _, discovery := range []struct {
			state string
			kits  []verification.KitReceipt
		}{
			{"no_manifest", []verification.KitReceipt{}},
			{"present", []verification.KitReceipt{{KitID: "other", Pins: []verification.Pin{{Kind: "requirement", DocumentID: "req-other", Version: 1}}, StageMatch: true, Eligibility: "ineligible", Reasons: []verification.SelectionReason{{Code: "pin_not_authoritative"}}}}},
		} {
			t.Run("PlanFinding/"+discovery.state, func(t *testing.T) {
				v := prepare(t, discovery.state, discovery.kits, []verification.Diagnostic{})
				coverage := verificationFixtureCoverage(v.snapshot(t))
				refuse(t, &v, store.VerificationSubmission{Outcome: "feedback", Coverage: coverage, Feedback: "No failure"}, store.VerificationCodeFeedbackUngrounded)
				finding := store.VerificationFinding{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Statement: "The observation is not retained."}
				refuse(t, &v, store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage, Findings: []store.VerificationFinding{finding}}, store.VerificationCodeFindingInvalid)
				v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "feedback", Coverage: coverage, Feedback: "AC-1.1 is unmet.", Findings: []store.VerificationFinding{finding}}})
				if text := feedbackText(t, &v); !strings.Contains(text, "req-fixture v1 AC-1.1: The observation is not retained.") {
					t.Fatalf("delivered feedback lost the finding: %s", text)
				}
				snapshot := v.snapshot(t)
				if len(snapshot.Attempts) != 0 || snapshot.Contexts[0].Result.Disposition != "no_selected_kits" {
					t.Fatal("a finding fabricated an attempt or a disposition")
				}
			})
		}
		t.Run("UnknownContextIsGeneric", func(t *testing.T) {
			v := prepare(t, "no_manifest", []verification.KitReceipt{}, []verification.Diagnostic{})
			coverage := verificationFixtureCoverage(v.snapshot(t))
			own := v.contextID
			v.contextID = "0123456789abcdef0123456789abcdef"
			for _, submission := range []store.VerificationSubmission{
				{Outcome: "succeeded", Coverage: store.VerificationCoverage{ObligationIDs: []string{}, Justification: "x"}},
				{Outcome: "succeeded", Coverage: store.VerificationCoverage{ObligationIDs: []string{}, Justification: "x", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "missing"}, Disposition: "not_applicable", Explanation: "x", Subjects: []core.VerificationSubject{}}}}},
				{Outcome: "blocked", Coverage: coverage},
			} {
				submission := submission
				_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: &submission}))
				var refusal *store.VerificationRefusal
				if !errors.Is(err, store.ErrVerificationAccess) || errors.As(err, &refusal) {
					t.Fatalf("unknown context returned detail: %v", err)
				}
			}
			v.contextID = own
		})
		t.Run("DiscoveryUnavailable", func(t *testing.T) {
			v := prepare(t, "permission", []verification.KitReceipt{}, []verification.Diagnostic{{Path: "discovery.conveyor", Message: "permission"}})
			coverage := verificationFixtureCoverage(v.snapshot(t))
			refuse(t, &v, store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}, store.VerificationCodeDiscoveryNotValid)
			refuse(t, &v, store.VerificationSubmission{Outcome: "feedback", Coverage: coverage, Feedback: "unavailable"}, store.VerificationCodeFeedbackUngrounded)
			v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "Discovery has no access.", RequiredAction: "Grant the GitHub App access, then recover the verify order."}})
			order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
			requireOK(t, err)
			task, err := x.Backend.GetTask(v.ctx, v.access.TaskID)
			requireOK(t, err)
			if task.NextStage != core.StageVerify || !order.RetrySuppressed || order.VerificationContextID != "" || order.Checkpoint == nil || order.Checkpoint.Verification == nil || len(order.Checkpoint.Verification.Grounds) != 1 || order.Checkpoint.Verification.Grounds[0].Kind != store.VerificationGroundDiscoveryUnavailable || !strings.Contains(order.Checkpoint.Verification.Grounds[0].Explanation, "permission") {
				t.Fatalf("unavailable discovery checkpoint = %+v", order.Checkpoint)
			}
		})
	})
	t.Run("SupersededClaimIsRevoked", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		_, err := x.Backend.MarkTaskApprovalStale(v.ctx, v.access.TaskID, v.revisions[0].SHA, strings.Repeat("b", 40), "full", "source changed")
		requireOK(t, err)
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		if order.State != core.WorkOrderCancelled || order.SessionID != "" || !order.LeaseExpiresAt.IsZero() {
			t.Fatal("obsolete verification claim survived")
		}
		if _, err = x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed"}})); err == nil {
			t.Fatal("superseded claim mutated verification")
		}
	})
}

func verificationFixtureCoverage(snapshot store.VerificationSnapshot) store.VerificationCoverage {
	c := store.VerificationCoverage{ObligationIDs: []string{}, Justification: "Assessment of pinned fixture source.", Sources: []store.VerificationCoverageSource{}}
	subjects := []core.VerificationSubject{}
	for _, o := range snapshot.Obligations {
		c.ObligationIDs = append(c.ObligationIDs, o.ID)
		subjects = append(subjects, core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest})
	}
	for _, selection := range snapshot.Selections {
		for _, s := range selection.Subjects {
			subjects = append(subjects, s.Subject)
		}
	}
	disposition := "covered"
	if len(subjects) == 0 {
		disposition = "not_applicable"
	}
	for _, p := range snapshot.Contexts[0].GoverningPins {
		c.Sources = append(c.Sources, store.VerificationCoverageSource{Source: store.VerificationCoverageReference{DocumentID: p.DocumentID, Version: p.Version, SectionID: "AC-1.1"}, Disposition: disposition, Explanation: "The registered fixture checks cover this source; an empty fixture has no executable requirement.", Subjects: subjects})
	}
	return c
}

// SealEmptyVerificationFixture records truthful fixture discovery and coverage
// before exercising the lifecycle; production discovery is tested separately.
func SealEmptyVerificationFixture(t *testing.T, ctx context.Context, b store.VerificationStore, access store.VerificationAccess, vc store.VerificationContext, section string) store.VerificationContext {
	t.Helper()
	vc.Discovery = []json.RawMessage{}
	pins := []verification.Pin{}
	for _, p := range vc.GoverningPins {
		pins = append(pins, verification.Pin{Kind: p.Kind, DocumentID: p.DocumentID, Version: p.Version})
	}
	for _, r := range vc.Revisions {
		vc.Discovery = append(vc.Discovery, core.JSONPayload(map[string]any{"repository": r.Repository, "revision": r.SHA, "state": "no_manifest"}))
	}
	selection := &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: pins, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}
	receipt, err := b.ApplyVerification(ctx, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "empty-fixture", Access: access, Context: &vc, Selection: selection})
	requireOK(t, err)
	vc.ID = receipt.ID
	coverage := store.VerificationCoverage{ObligationIDs: []string{}, Justification: "Read-only fixture has no applicable checks", Sources: []store.VerificationCoverageSource{}}
	for _, p := range vc.GoverningPins {
		coverage.Sources = append(coverage.Sources, store.VerificationCoverageSource{Source: store.VerificationCoverageReference{DocumentID: p.DocumentID, Version: p.Version, SectionID: section}, Disposition: "not_applicable", Explanation: "No executable checks apply to this fixture", Subjects: []core.VerificationSubject{}})
	}
	_, err = b.ApplyVerification(ctx, store.VerificationCommand{Kind: store.VerificationSeal, Access: access, ContextID: vc.ID, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}})
	requireOK(t, err)
	return vc
}

func runSealedVerificationReview(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	coverage := verificationFixtureCoverage(v.snapshot(t))
	submission := store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}}
	v.apply(t, submission)
	before := verificationPublicState(t, &v)
	v.apply(t, submission)
	if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
		t.Fatal("sealed receipt repeated lifecycle writes")
	}
	verify, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	job := core.Job{ID: verify.TaskID + "-review-1", TaskID: verify.TaskID, Stage: core.StageReview, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: job.TaskID, Stage: core.StageReview, State: core.WorkOrderQueued, HeadSHA: verify.HeadSHA, ReviewRound: 1, ReviewSeat: 1, CreatedAt: time.Now().UTC()}
	requireOK(t, CreateReviewRound(x.Context, x.Backend, order.TaskID, []core.Job{job}, []core.WorkOrder{order}))
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("reviewer"), SessionID: "reviewer", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: verify.ServedRequirementSnapshot})
	requireOK(t, err)
	ctx := store.WithActor(x.Context, store.Actor{ID: store.UserActorID("reviewer"), Role: core.ActorUser})
	valid := core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ClaimSession: "reviewer", ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "verified", Summary: "Sealed empty fixture assessed", ReviewedCommitSHA: verify.HeadSHA, HeadSHA: verify.HeadSHA, Reviewer: "forged actor", MaxBounces: 3, VerificationAssessment: &core.VerificationAssessment{ContextIDs: []string{v.contextID}, RunIDs: []string{}, EvidenceIDs: []string{}, Mappings: []core.VerificationAssessmentMapping{{DocumentID: "req-fixture", Version: 1, AcceptanceCriterionID: "AC-1.1", EvidenceIDs: []string{}, Assessment: "Explicit empty assessment is appropriate for this fixture"}}, Actor: "forged actor"}}
	// Each backend's acceptance transaction returns the same correctable
	// refusal as the preflight; a stale binding or an unknown context keeps
	// the generic refusal (component-verification-service "Correctable refusals").
	for _, tc := range []struct {
		code   string
		mutate func(*core.ReviewDecision)
	}{
		{store.VerificationCodeFieldRequired, func(d *core.ReviewDecision) { d.VerificationAssessment = nil }},
		{"", func(d *core.ReviewDecision) { d.HeadSHA = strings.Repeat("b", 40) }},
		{"", func(d *core.ReviewDecision) { d.ReviewScope = "delta" }},
		{"", func(d *core.ReviewDecision) { d.BaselineSHA = "wrong" }},
		{"", func(d *core.ReviewDecision) { d.VerificationAssessment.ContextIDs = []string{"unknown"} }},
		{store.VerificationCodeReviewContextCount, func(d *core.ReviewDecision) { d.VerificationAssessment.ContextIDs = []string{} }},
		{store.VerificationCodeReviewPinUnmapped, func(d *core.ReviewDecision) {
			d.VerificationAssessment.Mappings = []core.VerificationAssessmentMapping{}
		}},
		{store.VerificationCodeReviewMappingUnpinned, func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].Version = 2 }},
		{store.VerificationCodeReviewCriterionUnknown, func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].AcceptanceCriterionID = "AC-99.1" }},
		{store.VerificationCodeReviewEvidenceUnknown, func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].EvidenceIDs = []string{"foreign"} }},
		{store.VerificationCodeReviewRunUnknown, func(d *core.ReviewDecision) { d.VerificationAssessment.RunIDs = []string{"foreign"} }},
	} {
		var bad core.ReviewDecision
		requireOK(t, json.Unmarshal(core.JSONPayload(valid), &bad))
		tc.mutate(&bad)
		before := verificationPublicState(t, &v)
		err := taskops.New(x.Backend).AcceptReviewDecision(ctx, bad)
		if err == nil {
			t.Fatal("invalid sealed assessment accepted")
		}
		var refusal *store.VerificationRefusal
		if tc.code == "" {
			if errors.As(err, &refusal) {
				t.Fatalf("stale or unknown binding carried refusal detail: %v", err)
			}
		} else if !errors.As(err, &refusal) || refusal.Code != tc.code {
			t.Fatalf("acceptance refusal = %v, want code %s", err, tc.code)
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
			t.Fatal("review refusal changed aggregate")
		}
	}
	requireOK(t, taskops.New(x.Backend).AcceptReviewDecision(ctx, valid))
	events, err := x.Backend.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	found := false
	for _, e := range events {
		if e.Kind == "review.completed" {
			var body struct {
				Assessment *core.VerificationAssessment `json:"verification_assessment"`
			}
			requireOK(t, json.Unmarshal(e.Payload, &body))
			if body.Assessment == nil || body.Assessment.Actor != "user:reviewer" {
				t.Fatal("review lost authenticated assessment attribution")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("accepted review was not durable")
	}
}

// runSealedVerificationReviewDrift claims the review seat after a newer
// version of the context's governing requirement is confirmed, so the seat
// pins v2 while the context was sealed under v1. Each backend's acceptance
// transaction refuses approve with verification_review_authority_drift and
// writes nothing, then accepts changes_requested citing the same context and
// bounces the task to implement (component-verification-service "Authority
// drift between seal and review").
func runSealedVerificationReviewDrift(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: verificationFixtureCoverage(v.snapshot(t))}})
	verify, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	job := core.Job{ID: verify.TaskID + "-review-1", TaskID: verify.TaskID, Stage: core.StageReview, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: job.TaskID, Stage: core.StageReview, State: core.WorkOrderQueued, HeadSHA: verify.HeadSHA, ReviewRound: 1, ReviewSeat: 1, CreatedAt: time.Now().UTC()}
	requireOK(t, CreateReviewRound(x.Context, x.Backend, order.TaskID, []core.Job{job}, []core.WorkOrder{order}))
	drifted := append([]core.ServedRequirementContext{}, verify.ServedRequirementSnapshot...)
	if len(drifted) != 1 || drifted[0].ID != "req-fixture" || drifted[0].Version != 1 {
		t.Fatalf("verify order pins %+v, want req-fixture v1", drifted)
	}
	drifted[0].Version = 2
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("reviewer"), SessionID: "reviewer", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: drifted})
	requireOK(t, err)
	ctx := store.WithActor(x.Context, store.Actor{ID: store.UserActorID("reviewer"), Role: core.ActorUser})
	decision := core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ClaimSession: "reviewer", ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "verified", Summary: "Sealed empty fixture assessed", ReviewedCommitSHA: verify.HeadSHA, HeadSHA: verify.HeadSHA, Reviewer: "user:reviewer", MaxBounces: 3, VerificationAssessment: &core.VerificationAssessment{ContextIDs: []string{v.contextID}, RunIDs: []string{}, EvidenceIDs: []string{}, Mappings: []core.VerificationAssessmentMapping{{DocumentID: "req-fixture", Version: 1, AcceptanceCriterionID: "AC-1.1", EvidenceIDs: []string{}, Assessment: "The context was sealed under v1; v2 is unverified"}}}}
	before := verificationPublicState(t, &v)
	err = taskops.New(x.Backend).AcceptReviewDecision(ctx, decision)
	var refusal *store.VerificationRefusal
	if !errors.As(err, &refusal) || refusal.Code != store.VerificationCodeReviewAuthorityDrift || !errors.Is(err, store.ErrVerificationAccess) || !strings.Contains(refusal.Rule, "req-fixture v1") || !strings.Contains(refusal.Rule, "v2") {
		t.Fatalf("approve under drift = %v, want %s naming both versions", err, store.VerificationCodeReviewAuthorityDrift)
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
		t.Fatal("refused approval changed aggregate")
	}
	decision.Verdict, decision.ReasonCode = "changes_requested", "reverify"
	decision.Feedback = "req-fixture v2 was confirmed after the seal; submit the same head again for re-verification."
	requireOK(t, taskops.New(x.Backend).AcceptReviewDecision(ctx, decision))
	task, err := x.Backend.GetTask(ctx, order.TaskID)
	requireOK(t, err)
	if task.NextStage != core.StageImplement || core.TaskTerminal(task.State) {
		t.Fatalf("task after changes_requested = state %s next_stage %s, want a live bounce to implement", task.State, task.NextStage)
	}
}

func runVerificationReconciliation(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	obligation := store.VerificationObligation{ID: "reconcile-" + v.access.TaskID, Description: "Inspect provider state", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "reconcile", Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "reconciliation_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
	v.start(t, "original")
	prepare := store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "logical-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}}
	op := v.apply(t, prepare)
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "uncertain provider state"}})
	obs := store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "unknown", Source: "provider inspection", CapturedAt: time.Now().UTC()}}
	v.apply(t, obs)
	start := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "successor", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}}}
	before := verificationPublicState(t, &v)
	if _, err := x.Backend.ApplyVerification(v.ctx, v.command(start)); err == nil {
		t.Fatal("unknown result admitted replay")
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
		t.Fatal("unknown refusal mutated state")
	}
	obs.Observation.State = "applied"
	obs.Observation.CapturedAt = time.Now().UTC()
	v.apply(t, obs)
	v.runID = v.apply(t, start).ID
	if receipt := v.apply(t, prepare); receipt.ID != op.ID || receipt.State != "applied" || receipt.DispatchAuthorized {
		t.Fatal("applied reconciliation failed to retain operation")
	}
	dispatch := store.VerificationCommand{Kind: store.VerificationObserveOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "dispatching", Source: "runner", CapturedAt: time.Now().UTC()}}
	before = verificationPublicState(t, &v)
	if _, err := x.Backend.ApplyVerification(v.ctx, v.command(dispatch)); err == nil {
		t.Fatal("applied operation redispatched")
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
		t.Fatal("applied refusal mutated state")
	}
	exit := 0
	if _, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "succeeded", ExitCode: &exit}})); err == nil {
		t.Fatal("reconciliation fabricated current execution evidence")
	}
}

func runVerificationCheckpoint(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	obligation := store.VerificationObligation{ID: "checkpoint-" + v.access.TaskID, Description: "External action", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "action", Kind: "script", Argv: []string{"action"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	v.start(t, "action")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "checkpoint-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Provider acknowledgement is unknown; operator inspection required"}})
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "Provider acknowledgement is unknown", RequiredAction: "Inspect provider and authorize disposition"}})
	task, err := x.Backend.GetTask(v.ctx, v.access.TaskID)
	requireOK(t, err)
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	if task.NextStage != core.StageVerify || order.State != core.WorkOrderQueued || !order.RetrySuppressed || order.AttemptID != "" || order.SessionID != "" || !order.ExecutionDeadline.IsZero() || order.Checkpoint == nil {
		t.Fatalf("checkpoint lifecycle not atomic: %+v", order)
	}
	jobs, err := x.Backend.ListJobs(v.ctx, v.access.TaskID)
	requireOK(t, err)
	if len(jobs) != 1 || jobs[0].State != core.JobPending || !jobs[0].StartedAt.IsZero() || !jobs[0].EndedAt.IsZero() {
		t.Fatal("checkpoint did not reset job for recovery")
	}
	events, err := x.Backend.ListEvents(v.ctx, v.access.TaskID)
	requireOK(t, err)
	found := false
	for _, event := range events {
		if event.Kind == "work_order.released" {
			var payload struct {
				AttemptID  string                    `json:"attempt_id"`
				SessionID  string                    `json:"session_id"`
				Reason     string                    `json:"reason"`
				Checkpoint *core.WorkOrderCheckpoint `json:"checkpoint"`
			}
			requireOK(t, json.Unmarshal(event.Payload, &payload))
			found = payload.AttemptID == v.access.WorkOrderAttemptID && payload.SessionID == v.access.Claim.SessionID && payload.Reason == core.WorkOrderReleaseReasonOperatorCheckpointReached && payload.Checkpoint != nil
		}
	}
	if !found {
		t.Fatal("checkpoint release lost original claim identity")
	}
	if _, err = x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}})); err == nil {
		t.Fatal("released checkpoint claim sealed success")
	}
}

// sealVerificationCheckpoint seals an operator_action_required checkpoint
// whose blocked attempt leaves one prepared operation unresolved, or none
// when withOperation is false, and returns the released verify order.
func sealVerificationCheckpoint(t *testing.T, v *verificationFixture, withOperation bool) (core.WorkOrder, string) {
	t.Helper()
	obligation := store.VerificationObligation{ID: "disposition-" + v.access.TaskID, Description: "External action", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "action", Kind: "script", Argv: []string{"action"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	v.start(t, "disposition")
	operationID := ""
	if withOperation {
		operationID = v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "disposition-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}}).ID
	}
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Provider acknowledgement is unknown"}})
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "Provider acknowledgement is unknown", RequiredAction: "Inspect provider and authorize disposition"}})
	order, err := v.x.Backend.GetWorkOrder(v.x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	if order.State != core.WorkOrderQueued || order.Checkpoint == nil || order.Checkpoint.Verification == nil {
		t.Fatalf("sealed checkpoint order = %+v", order)
	}
	return order, operationID
}

// projectVerificationDisposition projects the task with row 11 selected. A
// sealed release also records the checkpoint's decision_request, which row 10
// (agent_question) outranks, so the projection reads the same stored order
// with only that agent-question marker withheld; the disposition set itself
// comes from the backend's verification records (component-next-action
// invariant 15).
func projectVerificationDisposition(t *testing.T, x Fixture, taskID string, caps map[core.Capability]bool) nextaction.NextAction {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	orders, err := x.Backend.ListWorkOrdersForTasks(x.Context, []string{taskID})
	requireOK(t, err)
	for i := range orders {
		if orders[i].Checkpoint != nil && orders[i].Checkpoint.Verification != nil {
			checkpoint := *orders[i].Checkpoint
			checkpoint.DecisionRequest = ""
			orders[i].Checkpoint = &checkpoint
		}
	}
	projected, err := store.ReadNextActions(x.Context, x.Backend, []core.Task{task}, store.NextActionOptions{Now: time.Now().UTC(), Capabilities: caps, OrdersLoaded: true, Orders: orders})
	requireOK(t, err)
	action := projected[taskID]
	if action.Kind != nextaction.KindVerificationDisposition {
		t.Fatalf("next action = %s, want verification_disposition", action.Kind)
	}
	return action
}

// runNextActionDispositionBinding proves that row 11's details.dispositions
// names the sealed checkpoint's context, the attempt that recorded its
// unresolved operation, that operation, the attempt's safe-input digest,
// subject, and state, so a recovery built only from those fields, a
// disposition, and a reason passes PrepareVerificationRecovery on the
// ordinary recovery path. Row 10 never carries the set, the response exposes
// no verification record, and a checkpoint without an unresolved operation
// or a retry-gated attempt withholds record_dispositions with a refusal
// (component-next-action invariants 7, 8, and 15;
// component-verification-service invariant 45).
func runNextActionDispositionBinding(t *testing.T, x Fixture) {
	gates := map[core.Capability]bool{core.CapabilityOperateGates: true}
	v := newVerificationFixture(t, x, true)
	order, operationID := sealVerificationCheckpoint(t, &v, true)

	question, err := store.ReadNextAction(x.Context, x.Backend, order.TaskID, store.NextActionOptions{Now: time.Now().UTC(), Capabilities: gates})
	requireOK(t, err)
	if question.Kind != nextaction.KindAgentQuestion || question.Details.Dispositions != nil {
		t.Fatalf("sealed checkpoint = %s dispositions %+v, want agent_question without a set", question.Kind, question.Details.Dispositions)
	}

	action := projectVerificationDisposition(t, x, order.TaskID, gates)
	want := []nextaction.Disposition{{ContextID: v.contextID, RunID: v.runID, OperationIDs: []string{operationID}, InputDigest: verificationSHA([]byte("{}")), Subject: "ordinary:disposition-" + order.TaskID, State: "blocked"}}
	if !reflect.DeepEqual(action.Details.Dispositions, want) {
		t.Fatalf("dispositions = %+v, want %+v (refusals %+v)", action.Details.Dispositions, want, action.Details.Refusals)
	}
	if len(action.Choices) != 1 {
		t.Fatalf("choices = %+v, want record_dispositions only", action.Choices)
	}
	for _, id := range []string{"record_dispositions"} {
		found := false
		for _, choice := range action.Choices {
			if choice.ID == id {
				found = choice.Capability == string(core.CapabilityOperateGates) && choice.Allowed && choice.Path == "/v1/work-orders/"+order.ID+"/recover"
			}
		}
		if !found {
			t.Fatalf("choice %s missing or not bound to operate_gates: %+v", id, action.Choices)
		}
	}
	recoverOnly := projectVerificationDisposition(t, x, order.TaskID, map[core.Capability]bool{core.CapabilityRecoverWork: true})
	for _, choice := range recoverOnly.Choices {
		if choice.ID == "record_dispositions" && choice.Allowed {
			t.Fatalf("recover_work alone allowed %s", choice.ID)
		}
	}
	raw, err := json.Marshal(action)
	requireOK(t, err)
	for _, leaked := range []string{"safe_inputs", "client_token", "token_hash", "verification-fixture-token"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("next action exposes %s: %s", leaked, raw)
		}
	}

	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	disposition := &store.VerificationRecovery{Reason: "Operator inspected the provider; the change did not apply.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: want[0].ContextID, RunID: want[0].RunID, OperationIDs: want[0].OperationIDs, InputDigest: want[0].InputDigest, Disposition: "not_applied"}}}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, disposition), x.Backend, order.ID, "disposition-recovery-"+order.ID, time.Hour)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, order.TaskID)
	requireOK(t, err)
	authorized := false
	for _, event := range events {
		authorized = authorized || event.Kind == "verification.recovery_authorized"
	}
	if !authorized {
		t.Fatal("recovery built from the binding recorded no verification authorization")
	}

	// A checkpoint without an unresolved operation binds the attempt the
	// operator_action_required retry gate would refuse to retry, with no
	// operations.
	empty := newVerificationFixture(t, x, true)
	bare, _ := sealVerificationCheckpoint(t, &empty, false)
	gated := projectVerificationDisposition(t, x, bare.TaskID, gates)
	wantGated := []nextaction.Disposition{{ContextID: empty.contextID, RunID: empty.runID, OperationIDs: []string{}, InputDigest: verificationSHA([]byte("{}")), Subject: "ordinary:disposition-" + bare.TaskID, State: "blocked"}}
	if !reflect.DeepEqual(gated.Details.Dispositions, wantGated) {
		t.Fatalf("gated dispositions = %+v, want %+v (refusals %+v)", gated.Details.Dispositions, wantGated, gated.Details.Refusals)
	}

	// A checkpoint with neither an unresolved operation nor a retry-gated
	// attempt offers neither disposition.
	replayable := newVerificationFixture(t, x, true)
	obligation := store.VerificationObligation{ID: "replayable-" + replayable.access.TaskID, Description: "Read-only observation", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "observe", Kind: "script", Argv: []string{"observe"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
	r := replayable.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	replayable.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	replayable.start(t, "replayable")
	replayable.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Fixture account locked"}})
	replayable.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(replayable.snapshot(t)), Feedback: "Fixture account locked", RequiredAction: "Unlock the fixture account"}})
	unbound := projectVerificationDisposition(t, x, replayable.access.TaskID, gates)
	if unbound.Details.Dispositions != nil {
		t.Fatalf("checkpoint without unresolved operations or a gated attempt stated %+v", unbound.Details.Dispositions)
	}
	for _, id := range []string{"record_dispositions"} {
		for _, choice := range unbound.Choices {
			if choice.ID == id {
				t.Fatalf("choice %s offered without an unresolved operation", id)
			}
		}
		refused := false
		for _, refusal := range unbound.Details.Refusals {
			refused = refused || (refusal.Choice == id && strings.Contains(refusal.Text, "no unresolved operations"))
		}
		if !refused {
			t.Fatalf("refusals = %+v, want %s refused for missing operations", unbound.Details.Refusals, id)
		}
	}
}

// runNextActionGatedAttemptBinding reproduces the FunnelFlux shape: an
// ordinary operator_action_required obligation succeeds in context A, the
// claim is lost with A unsealed, and a later context B of the same verify
// order seals operator_action_required with no attempt and no operation.
// Row 11 states A's run with no operations and the run's input digest, and a
// recovery built from that entry admits exactly one successor start
// (component-next-action invariant 15; component-verification-service
// invariant 45; req-verification-kits AC-3.4).
func runNextActionGatedAttemptBinding(t *testing.T, x Fixture) {
	gates := map[core.Capability]bool{core.CapabilityOperateGates: true}
	v := newVerificationFixture(t, x, true)
	obligation := store.VerificationObligation{ID: "queue-snapshot-tests", Description: "Queue snapshot tests", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "queue-snapshot-tests", Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required"}}
	register := func(v *verificationFixture) {
		t.Helper()
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	}
	start := func(key, authorization string) store.VerificationCommand {
		return store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: key, Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}, ReplayAuthorizationID: authorization, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}}
	}
	refused := func(c store.VerificationCommand) {
		t.Helper()
		c.Attempt.GrantID = GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "grant-"+c.Key, c.Attempt.Subject, nil)
		c.Attempt.EffectiveActions = []core.VerificationPermission{}
		_, err := x.Backend.ApplyVerification(v.ctx, v.command(c))
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) || refusal.Code != store.VerificationCodeRetryAuthorizationRequired {
			t.Fatalf("start %s: %v, want %s", c.Key, err, store.VerificationCodeRetryAuthorizationRequired)
		}
	}
	reclaim := func(session string) {
		t.Helper()
		order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
		requireOK(t, err)
		claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
		order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
		requireOK(t, err)
		v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: session}, ClientToken: claim.ClientToken}
		v.contextID, v.runID = "", ""
		discovery := []json.RawMessage{core.JSONPayload(map[string]any{"repository": v.revisions[0].Repository, "revision": v.revisions[0].SHA, "state": "no_manifest"})}
		v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: session, Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: discovery}}).ID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{}}, Subjects: []store.VerificationSubjectContract{}}})
		register(&v)
	}

	// Context A: the obligation succeeds, then the claim is lost unsealed.
	register(&v)
	v.start(t, "snapshot-original")
	contextA, runA := v.contextID, v.runID
	zero, no := 0, false
	at := time.Now().UTC().Format(time.RFC3339Nano)
	report := v.envelope("snapshot-report", "snapshot-report", "")
	report.Type = "execution_report"
	report.Payload = core.JSONPayload(core.ExecutionReportPayload{Argv: []string{"check"}, Tool: "check", ToolVersion: "1", Runtime: "fixture", StartedAt: at, EndedAt: at, ExitCode: &zero, TimedOut: &no, Cancelled: &no, StdoutSHA256: strings.Repeat("a", 64), StderrSHA256: strings.Repeat("b", 64), StdoutTruncated: &no, StderrTruncated: &no})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: report.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(report)}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "succeeded", ExitCode: &zero}})
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	order.State, order.LastAttemptID, order.LastAttemptOutcome = core.WorkOrderQueued, order.AttemptID, core.WorkOrderOutcomeExpired
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))

	// Context B: the successor claim is refused a retry and seals
	// operator_action_required with no attempt and no operation.
	reclaim("snapshot-successor")
	refused(start("snapshot-successor-bare", ""))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Retry needs an operator authorization", RequiredAction: "Authorize one retry of queue-snapshot-tests"}})
	order, err = x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	if order.State != core.WorkOrderQueued || order.Checkpoint == nil || order.Checkpoint.Verification == nil || order.Checkpoint.Verification.ContextID != v.contextID || len(order.Checkpoint.Verification.OperationIDs) != 0 {
		t.Fatalf("context B checkpoint order = %+v", order)
	}

	action := projectVerificationDisposition(t, x, order.TaskID, gates)
	want := []nextaction.Disposition{{ContextID: contextA, RunID: runA, OperationIDs: []string{}, InputDigest: verificationSHA([]byte("{}")), Subject: "ordinary:queue-snapshot-tests", State: "succeeded"}}
	if !reflect.DeepEqual(action.Details.Dispositions, want) {
		t.Fatalf("dispositions = %+v, want %+v (refusals %+v)", action.Details.Dispositions, want, action.Details.Refusals)
	}
	raw, err := json.Marshal(action.Details.Dispositions)
	requireOK(t, err)
	if !strings.Contains(string(raw), `"operation_ids":[]`) {
		t.Fatalf("binding JSON %s does not encode operation_ids as []", raw)
	}

	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	disposition := &store.VerificationRecovery{Reason: "The obligation succeeded before the claim was lost; authorize one retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: want[0].ContextID, RunID: want[0].RunID, OperationIDs: want[0].OperationIDs, InputDigest: want[0].InputDigest, Disposition: "applied"}}}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, disposition), x.Backend, order.ID, "snapshot-recovery", time.Hour)
	requireOK(t, err)

	// Context C: the authorization admits exactly one successor start.
	reclaim("snapshot-recovered")
	// The server selects the authorization, so the start names none
	// (component-verification-service invariant 19; DEC-133(3)).
	started := v.apply(t, start("snapshot-recovered", ""))
	if !started.LaunchAuthorized {
		t.Fatal("recovery authorization did not admit the successor attempt")
	}
	v.runID = started.ID
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "snapshot mismatch"}})
	refused(start("snapshot-recovered-reuse", ""))
}

// runVerificationRetryGateScope pins the order scope of the
// operator_action_required retry gate on every backend: the gate reads the
// latest attempt of the subject within the starting verify order, while an
// unresolved operation still blocks the subject anywhere in the task
// (component-verification-service "Attempts and outcome evaluation";
// req-verification-kits AC-3.4).
func runVerificationRetryGateScope(t *testing.T, x Fixture) {
	scope := []core.VerificationRevision{{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: strings.Repeat("a", 40)}}
	bounces := func(task *core.Task) { task.SetupContract.MaxBounces = 10 }
	register := func(t *testing.T, v *verificationFixture, operations ...verification.Operation) store.VerificationObligation {
		t.Helper()
		obligation := store.VerificationObligation{ID: "gated-" + v.access.TaskID, Description: "Exercise retried only on operator authority", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "gated", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required", Operations: operations}}
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
		return obligation
	}
	start := func(key, authorization string, subject core.VerificationSubject) store.VerificationCommand {
		return store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: key, Attempt: &store.VerificationAttempt{Subject: subject, SafeInputs: map[string]json.RawMessage{}, ReplayAuthorizationID: authorization, Environment: core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}}}
	}
	refused := func(t *testing.T, v *verificationFixture, c store.VerificationCommand, code string) {
		t.Helper()
		c.Attempt.GrantID = GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "grant-"+c.Key, c.Attempt.Subject, nil)
		c.Attempt.EffectiveActions = []core.VerificationPermission{}
		before := len(v.snapshot(t).Attempts)
		_, err := x.Backend.ApplyVerification(v.ctx, v.command(c))
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) || refusal.Code != code {
			t.Fatalf("start %s: %v, want %s", c.Key, err, code)
		}
		if len(v.snapshot(t).Attempts) != before {
			t.Fatalf("refused start %s wrote an attempt", c.Key)
		}
	}
	failed := func(t *testing.T, v *verificationFixture) {
		t.Helper()
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "environment unavailable"}})
	}

	t.Run("NewVerifyOrderStartsClean", func(t *testing.T) {
		v := newVerificationFixtureTask(t, x, scope, bounces, true)
		register(t, &v)
		v.start(t, "gated-first-order")
		failed(t, &v)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "feedback", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "The gated exercise failed."}})
		next := verifyOrderAfterResubmission(t, x, v, strings.Repeat("b", 40))
		register(t, &next)
		r := next.apply(t, start("gated-next-order", "", next.subject))
		if !r.LaunchAuthorized {
			t.Fatal("a later verify order's first start was not authorized")
		}
		next.runID = r.ID
		snapshot := next.snapshot(t)
		if len(snapshot.Attempts) != 1 || snapshot.Attempts[0].ID != r.ID || snapshot.Attempts[0].Ordinal != 1 || snapshot.Attempts[0].ReplayAuthorizationID != "" {
			t.Fatalf("later order attempt = %+v", snapshot.Attempts)
		}
	})

	t.Run("SameOrderRetryNeedsAuthorization", func(t *testing.T) {
		v := newVerificationFixtureTask(t, x, scope, nil, true)
		obligation := register(t, &v)
		v.start(t, "gated-original")
		failed(t, &v)
		// A retry in the same context is gated before any recovery.
		refused(t, &v, start("gated-same-context", "", v.subject), store.VerificationCodeRetryAuthorizationRequired)
		oldContext, oldRun := v.contextID, v.runID
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
		order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
		order.LeaseExpiresAt = time.Time{}
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
		operator, owner := bootstrapOwner(t, x)
		operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
		disposition := &store.VerificationRecovery{Reason: "Operator restored the environment and authorizes one retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: oldContext, RunID: oldRun, OperationIDs: []string{}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied"}}}
		_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, disposition), x.Backend, order.ID, "gate-recovery", time.Hour)
		requireOK(t, err)
		claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "gate-successor", ClientToken: "gate-successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
		order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
		requireOK(t, err)
		v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
		v.contextID, v.runID = "", ""
		v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "gate-successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}}).ID
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject.ContractDigest = r.Digest
		// The successor context belongs to the same order, so the gate still
		// reads the original attempt.
		// The server selects the operator's authorization on that attempt.
		started := v.apply(t, start("gated-successor", "", v.subject))
		if !started.LaunchAuthorized {
			t.Fatal("recovery authorization did not admit the successor attempt")
		}
		v.runID = started.ID
		failed(t, &v)
		refused(t, &v, start("gated-successor-reuse", "", v.subject), store.VerificationCodeRetryAuthorizationRequired)
		observer := v
		observer.ctx, observer.access = operator, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
		for _, attempt := range observer.snapshotFor(t, oldContext).Attempts {
			if attempt.ID == oldRun && (len(attempt.Recovery) != 1 || attempt.Recovery[0].Successor == nil || attempt.Recovery[0].Successor.RunID != started.ID) {
				t.Fatalf("authorization did not bind the successor once: %+v", attempt.Recovery)
			}
		}
	})

	t.Run("UnresolvedOperationBlocksLaterOrder", func(t *testing.T) {
		v := newVerificationFixtureTask(t, x, scope, nil, true)
		register(t, &v, verification.Operation{ID: "step", TargetBinding: "fixture"})
		v.start(t, "gated-operation")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "gated-operation-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		// The terminal report moves the registered operation to outcome_unknown.
		failed(t, &v)
		order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
		requireOK(t, err)
		order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
		order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
		order.LeaseExpiresAt = time.Time{}
		requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
		head := strings.Repeat("c", 40)
		changed, err := x.Backend.MarkTaskApprovalStale(x.Context, v.access.TaskID, v.revisions[0].SHA, head, "full", "source changed during verification")
		requireOK(t, err)
		if !changed {
			t.Fatal("head advance did not move the verify head")
		}
		next := verifyOrderAtHead(t, x, v, head)
		register(t, &next, verification.Operation{ID: "step", TargetBinding: "fixture"})
		refused(t, &next, start("gated-operation-next-order", "", next.subject), store.VerificationCodeOperationUnresolved)
	})

	t.Run("UngatedStartsLeaveAuthorizationUnused", func(t *testing.T) {
		m := newRetryGateMixedFixture(t, x, scope, register, start, failed, 3)
		// A first start under operator_action_required is ungated, so the
		// incidental ID it carried is not recorded.
		for _, attempt := range m.observer.snapshotFor(t, m.oldContext).Attempts {
			if attempt.ReplayAuthorizationID != "" {
				t.Fatalf("first start %s recorded authorization %q", attempt.ID, attempt.ReplayAuthorizationID)
			}
		}
		// The kit runner sends the operator's ID on every subject of one run.
		// Each start that the gate does not gate is admitted, records no ID,
		// and leaves the gated attempt's authorization unused.
		for i, subject := range m.safe {
			key := fmt.Sprintf("mixed-safe-%d", i)
			r := m.v.apply(t, start(key, m.authorization, subject))
			if !r.LaunchAuthorized {
				t.Fatalf("ungated start %s was not authorized", key)
			}
			m.assertStored(t, r.ID, "")
			m.assertUnused(t)
			// A start-key replay launches nothing, with or without the ID.
			for _, id := range []string{m.authorization, ""} {
				replay := m.v.apply(t, start(key, id, subject))
				if replay.ID != r.ID || replay.LaunchAuthorized {
					t.Fatalf("replay of %s with %q = %+v, want %s without launch", key, id, replay, r.ID)
				}
			}
			m.assertUnused(t)
		}
		started := m.v.apply(t, start("mixed-gated", m.authorization, m.gated))
		if !started.LaunchAuthorized {
			t.Fatal("the gated start was refused after the ungated starts")
		}
		m.assertStored(t, started.ID, m.authorization)
		m.assertSuccessor(t, started.ID)
		replay := m.v.apply(t, start("mixed-gated", m.authorization, m.gated))
		if replay.ID != started.ID || replay.LaunchAuthorized {
			t.Fatalf("gated replay = %+v, want %s without launch", replay, started.ID)
		}
		m.assertSuccessor(t, started.ID)
		m.v.runID = started.ID
		failed(t, &m.v)
		refused(t, &m.v, start("mixed-gated-reuse", m.authorization, m.gated), store.VerificationCodeRetryAuthorizationRequired)
		m.assertSuccessor(t, started.ID)
	})

	t.Run("LegacyAttemptIDDoesNotRefuseGatedStart", func(t *testing.T) {
		if x.SeedVerificationAttemptAuthorization == nil {
			t.Fatal("conformance fixture must provide SeedVerificationAttemptAuthorization for the retry gate legacy-row case (component-verification-service invariant 19)")
		}
		m := newRetryGateMixedFixture(t, x, scope, register, start, failed, 1)
		safe := m.v.apply(t, start("legacy-safe", m.authorization, m.safe[0]))
		m.v.runID = safe.ID
		failed(t, &m.v)
		// A row written before this change holds the operator's ID on an
		// attempt of another subject.
		x.SeedVerificationAttemptAuthorization(t, x.Context, safe.ID, m.authorization)
		m.assertStored(t, safe.ID, m.authorization)
		m.assertUnused(t)
		// The historical row still answers its own start-key replay.
		for _, id := range []string{m.authorization, ""} {
			if replay := m.v.apply(t, start("legacy-safe", id, m.safe[0])); replay.ID != safe.ID || replay.LaunchAuthorized {
				t.Fatalf("legacy replay with %q = %+v", id, replay)
			}
		}
		// The server selects the gated attempt's own authorization, so the
		// start names none (component-verification-service invariant 19).
		started := m.v.apply(t, start("legacy-gated", "", m.gated))
		if !started.LaunchAuthorized {
			t.Fatal("a historical attempt of another subject refused the gated start")
		}
		m.assertStored(t, started.ID, m.authorization)
		m.assertSuccessor(t, started.ID)
		m.assertStored(t, safe.ID, m.authorization)
		m.v.runID = started.ID
		failed(t, &m.v)
		refused(t, &m.v, start("legacy-gated-reuse", m.authorization, m.gated), store.VerificationCodeRetryAuthorizationRequired)
	})
}

// retryGateMixedFixture is one verify order whose first context ended one
// operator_action_required attempt and several safe_to_replay attempts. An
// operator recovered the gated attempt with a disposition, and the same
// order was claimed again with a successor context.
type retryGateMixedFixture struct {
	v, observer        verificationFixture
	gated              core.VerificationSubject
	safe               []core.VerificationSubject
	oldContext, oldRun string
	authorization      string
}

func newRetryGateMixedFixture(t *testing.T, x Fixture, scope []core.VerificationRevision, register func(*testing.T, *verificationFixture, ...verification.Operation) store.VerificationObligation, start func(string, string, core.VerificationSubject) store.VerificationCommand, failed func(*testing.T, *verificationFixture), safeCount int) retryGateMixedFixture {
	t.Helper()
	m := retryGateMixedFixture{v: newVerificationFixtureTask(t, x, scope, nil, true), authorization: "recovery:mixed-recovery"}
	v := &m.v
	gatedObligation := register(t, v)
	m.gated = v.subject
	var obligations []store.VerificationObligation
	for i := 0; i < safeCount; i++ {
		obligation := store.VerificationObligation{ID: fmt.Sprintf("safe-%d-%s", i, v.access.TaskID), Description: "Exercise that is safe to replay", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: fmt.Sprintf("safe-%d", i), Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		obligations = append(obligations, obligation)
		m.safe = append(m.safe, core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest})
	}
	// Every first start carries the ID, as one runner invocation sends it.
	for i, subject := range append([]core.VerificationSubject{m.gated}, m.safe...) {
		r := v.apply(t, start(fmt.Sprintf("mixed-first-%d", i), m.authorization, subject))
		if !r.LaunchAuthorized {
			t.Fatalf("first start of %s was not authorized", subject.ObligationID)
		}
		v.runID = r.ID
		if subject == m.gated {
			m.oldRun = r.ID
		}
		failed(t, v)
	}
	m.oldContext = v.contextID
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, "released", true
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	disposition := &store.VerificationRecovery{Reason: "Operator restored the environment and authorizes one retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: m.oldContext, RunID: m.oldRun, OperationIDs: []string{}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied"}}}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, disposition), x.Backend, order.ID, "mixed-recovery", time.Hour)
	requireOK(t, err)
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "mixed-successor", ClientToken: "mixed-successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
	requireOK(t, err)
	v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
	v.contextID, v.runID = "", ""
	v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "mixed-successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}}).ID
	m.gated.ContractDigest = v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &gatedObligation}).Digest
	for i := range obligations {
		m.safe[i].ContractDigest = v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligations[i]}).Digest
	}
	m.observer = m.v
	m.observer.ctx, m.observer.access = operator, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
	m.assertUnused(t)
	return m
}

// recovered returns the recovered gated attempt and its one authorization.
func (m *retryGateMixedFixture) recovered(t *testing.T) store.VerificationReplayAuthorization {
	t.Helper()
	for _, attempt := range m.observer.snapshotFor(t, m.oldContext).Attempts {
		if attempt.ID == m.oldRun {
			if len(attempt.Recovery) != 1 || attempt.Recovery[0].ID != m.authorization {
				t.Fatalf("recovered attempt authorizations = %+v", attempt.Recovery)
			}
			return attempt.Recovery[0]
		}
	}
	t.Fatalf("recovered attempt %s is missing", m.oldRun)
	return store.VerificationReplayAuthorization{}
}

func (m *retryGateMixedFixture) assertUnused(t *testing.T) {
	t.Helper()
	if a := m.recovered(t); a.Successor != nil {
		t.Fatalf("authorization consumed by %+v", a.Successor)
	}
}

func (m *retryGateMixedFixture) assertSuccessor(t *testing.T, runID string) {
	t.Helper()
	if a := m.recovered(t); a.Successor == nil || a.Successor.RunID != runID {
		t.Fatalf("authorization successor = %+v, want run %s", a.Successor, runID)
	}
}

// assertStored checks the authorization ID that attempt runID records.
func (m *retryGateMixedFixture) assertStored(t *testing.T, runID, want string) {
	t.Helper()
	for _, attempt := range m.observer.snapshotFor(t, m.v.contextID).Attempts {
		if attempt.ID == runID {
			if attempt.ReplayAuthorizationID != want {
				t.Fatalf("attempt %s records authorization %q, want %q", runID, attempt.ReplayAuthorizationID, want)
			}
			return
		}
	}
	t.Fatalf("attempt %s is missing", runID)
}

// verifyOrderAfterResubmission follows a feedback seal: it claims the
// successor implement order, submits it at head, and returns the fixture of
// the verify order that the submission routes to.
func verifyOrderAfterResubmission(t *testing.T, x Fixture, v verificationFixture, head string) verificationFixture {
	t.Helper()
	st, ctx := x.Backend, x.Context
	task, err := st.GetTask(ctx, v.access.TaskID)
	requireOK(t, err)
	if task.NextStage != core.StageImplement {
		t.Fatalf("feedback did not return the task to implement: next=%s state=%s", task.NextStage, task.State)
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	o := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
	_, err = CreateStageWorkOrder(ctx, st, job, o)
	requireOK(t, err)
	o, err = ClaimWorkOrder(ctx, st, o.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "resubmit", ClientToken: "resubmit-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	o.State, o.HeadSHA = core.WorkOrderSubmitted, head
	requireOK(t, UpdateWorkOrder(ctx, st, o, core.WorkOrderCmdSubmitForReview))
	_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	requireOK(t, err)
	return verifyOrderAtHead(t, x, v, head)
}

// verifyOrderAtHead mints and claims the task's second verify order at head,
// which must be the task's verify head, and prepares one no-kit context there.
func verifyOrderAtHead(t *testing.T, x Fixture, v verificationFixture, head string) verificationFixture {
	t.Helper()
	st, ctx := x.Backend, x.Context
	task, err := st.GetTask(ctx, v.access.TaskID)
	requireOK(t, err)
	if task.State == core.TaskQueued {
		_, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart})
		requireOK(t, err)
		task, err = st.GetTask(ctx, task.ID)
		requireOK(t, err)
	}
	if task.NextStage != core.StageVerify || core.VerifyStageHead(task) != head {
		t.Fatalf("task did not reach verify at the new head: next=%s head=%s", task.NextStage, core.VerifyStageHead(task))
	}
	job := core.Job{ID: task.ID + "-verify-2", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	o := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, ReviewScope: task.RefreshReviewScope, BaselineSHA: task.RefreshBaselineSHA, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
	_, err = CreateStageWorkOrder(ctx, st, job, o)
	requireOK(t, err)
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "later-verify", ClientToken: "later-verify-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
	claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	o, err = ClaimWorkOrder(ctx, st, o.ID, claim)
	requireOK(t, err)
	next := v
	next.access = store.VerificationAccess{TaskID: o.TaskID, WorkOrderID: o.ID, WorkOrderAttemptID: o.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}}
	next.revisions = []core.VerificationRevision{{Repository: v.revisions[0].Repository, RemoteIdentity: v.revisions[0].RemoteIdentity, SHA: head}}
	next.contextID, next.runID = "", ""
	next.contextID = next.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "later-verify", Context: &store.VerificationContext{ReviewScope: o.ReviewScope, BaselineSHA: o.BaselineSHA, Revisions: next.revisions, GoverningPins: next.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": next.revisions[0].Repository, "revision": head, "state": "no_manifest"})}}}).ID
	next.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}})
	return next
}

// recoveryFixtureEnvironment is the environment every recovery fixture start
// reports.
var recoveryFixtureEnvironment = core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}

// recoveryFixtureStart starts subject under key, carrying authorization the
// way the kit runner sends it today.
func recoveryFixtureStart(key, authorization string, subject core.VerificationSubject) store.VerificationCommand {
	return store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: key, Attempt: &store.VerificationAttempt{Subject: subject, SafeInputs: map[string]json.RawMessage{}, ReplayAuthorizationID: authorization, Environment: recoveryFixtureEnvironment}}
}

// recoveryFixtureObligation is an ordinary obligation with the retry policy
// and, when step is not empty, one external operation.
func recoveryFixtureObligation(id, policy, step string) store.VerificationObligation {
	exercise := verification.Exercise{ID: id, Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: policy}
	if policy == "safe_to_replay" {
		exercise.SafetyBasis = "read-only observation"
	}
	if step != "" {
		exercise.Operations = []verification.Operation{{ID: step, TargetBinding: "fixture"}}
	}
	return store.VerificationObligation{ID: id, Description: "Recovery fixture " + id, Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: exercise}
}

// recoveryFixtureRegister registers the obligation in the fixture's current
// context and returns its subject.
func recoveryFixtureRegister(t *testing.T, v *verificationFixture, obligation store.VerificationObligation) core.VerificationSubject {
	t.Helper()
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	return core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
}

// recoveryFixtureReclaim claims the released verify order under session and
// creates a successor context in the fixture.
func recoveryFixtureReclaim(t *testing.T, x Fixture, v *verificationFixture, session string) {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
	requireOK(t, err)
	v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: session}, ClientToken: claim.ClientToken}
	v.contextID, v.runID = "", ""
	v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: session, Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}}).ID
}

// recoveryFixtureLoseClaim releases the claimed verify order as a lost claim
// does, with retry suppressed so that only an operator recovery requeues it.
func recoveryFixtureLoseClaim(t *testing.T, x Fixture, v *verificationFixture) {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	order.State, order.LastAttemptID, order.LastAttemptOutcome, order.RetrySuppressed = core.WorkOrderQueued, order.AttemptID, core.WorkOrderOutcomeReleased, true
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))
}

// recoveryFixtureRequest builds one recovery from disposition entries, with
// one disposition per entry in entry order.
func recoveryFixtureRequest(reason string, entries []nextaction.Disposition, dispositions ...string) *store.VerificationRecovery {
	r := &store.VerificationRecovery{Reason: reason, Dispositions: []store.VerificationRecoveryDisposition{}}
	for i, e := range entries {
		r.Dispositions = append(r.Dispositions, store.VerificationRecoveryDisposition{ContextID: e.ContextID, RunID: e.RunID, OperationIDs: e.OperationIDs, InputDigest: e.InputDigest, Disposition: dispositions[i]})
	}
	return r
}

// recoveryFixtureAuthorized counts the task's verification.recovery_authorized
// events.
func recoveryFixtureAuthorized(t *testing.T, x Fixture, taskID string) int {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	n := 0
	for _, event := range events {
		if event.Kind == "verification.recovery_authorized" {
			n++
		}
	}
	return n
}

// runRecoveryDisposesEveryGatedSubject reproduces the reported defect: a
// verify order ends two operator_action_required subjects and three
// safe_to_replay subjects and seals an operator checkpoint. Row 11 states
// both gated attempts, and one recovery built from that statement records an
// authorization on both attempts and appends two events, or records nothing
// when it omits a member or names one attempt twice. In the successor
// context both gated starts sent with recovery:<request_id> launch, and each
// original attempt names its own successor (component-verification-service
// invariants 19 and 45; component-next-action invariant 15; DEC-133(1), (2);
// req-verification-kits AC-3.4).
func runRecoveryDisposesEveryGatedSubject(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	digest := verificationSHA([]byte("{}"))
	var obligations []store.VerificationObligation
	for _, name := range []string{"login", "checkout"} {
		obligations = append(obligations, recoveryFixtureObligation(name+"-"+v.access.TaskID, "operator_action_required", ""))
	}
	for i := 0; i < 3; i++ {
		obligations = append(obligations, recoveryFixtureObligation(fmt.Sprintf("read-%d-%s", i, v.access.TaskID), "safe_to_replay", ""))
	}
	subjects := make([]core.VerificationSubject, len(obligations))
	originals := make([]string, len(obligations))
	for i := range obligations {
		subjects[i] = recoveryFixtureRegister(t, &v, obligations[i])
	}
	// The gated subjects block on the fixture environment; the safe ones fail.
	for i := range obligations {
		r := v.apply(t, recoveryFixtureStart(fmt.Sprintf("first-%d", i), "", subjects[i]))
		v.runID, originals[i] = r.ID, r.ID
		state := "failed"
		if i < 2 {
			state = "blocked"
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: state, Explanation: "fixture environment unavailable"}})
	}
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Two exercises need an operator", RequiredAction: "Restore the fixtures and authorize one retry of each"}})
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	if order.State != core.WorkOrderQueued || order.Checkpoint == nil || order.Checkpoint.Verification == nil {
		t.Fatalf("sealed checkpoint order = %+v", order)
	}
	oldContext := v.contextID

	action := projectVerificationDisposition(t, x, order.TaskID, map[core.Capability]bool{core.CapabilityOperateGates: true})
	want := []nextaction.Disposition{
		{ContextID: oldContext, RunID: originals[0], OperationIDs: []string{}, InputDigest: digest, Subject: "ordinary:" + obligations[0].ID, State: "blocked"},
		{ContextID: oldContext, RunID: originals[1], OperationIDs: []string{}, InputDigest: digest, Subject: "ordinary:" + obligations[1].ID, State: "blocked"},
	}
	if !reflect.DeepEqual(action.Details.Dispositions, want) {
		t.Fatalf("dispositions = %+v, want both gated attempts %+v (refusals %+v)", action.Details.Dispositions, want, action.Details.Refusals)
	}

	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	reason := "Operator restored both fixtures and authorizes one retry of each."
	refused := map[string]*store.VerificationRecovery{
		"omitted member":    recoveryFixtureRequest(reason, want[:1], "applied"),
		"duplicate attempt": recoveryFixtureRequest(reason, []nextaction.Disposition{want[0], want[0], want[1]}, "applied", "applied", "not_applied"),
	}
	for name, request := range refused {
		before := verificationPublicState(t, &observer)
		if _, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, request), x.Backend, order.ID, "refused-"+strings.ReplaceAll(name, " ", "-"), time.Hour); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("%s: recover err = %v, want an invalid recovery refusal", name, err)
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &observer)) {
			t.Fatalf("%s: refused recovery changed the aggregate", name)
		}
	}

	requestID := "batch-recovery"
	request := recoveryFixtureRequest(reason, action.Details.Dispositions, "applied", "not_applied")
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, request), x.Backend, order.ID, requestID, time.Hour)
	requireOK(t, err)
	if n := recoveryFixtureAuthorized(t, x, order.TaskID); n != 2 {
		t.Fatalf("recovery appended %d recovery_authorized events, want 2", n)
	}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, request), x.Backend, order.ID, requestID, time.Hour)
	requireOK(t, err)
	if n := recoveryFixtureAuthorized(t, x, order.TaskID); n != 2 {
		t.Fatalf("replay appended events: %d recovery_authorized events, want 2", n)
	}
	changed := recoveryFixtureRequest(reason, action.Details.Dispositions, "not_applied", "not_applied")
	if _, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, changed), x.Backend, order.ID, requestID, time.Hour); err == nil {
		t.Fatal("a replay with a changed disposition was accepted")
	}

	recoveryFixtureReclaim(t, x, &v, "batch-successor")
	for i := range obligations {
		subjects[i].ContractDigest = recoveryFixtureRegister(t, &v, obligations[i]).ContractDigest
	}
	authorization := "recovery:" + requestID
	successors := map[string]string{}
	// The runner starts every subject with the ID; the safe_to_replay starts
	// are not gated and leave both authorizations unused.
	for i := len(obligations) - 1; i >= 0; i-- {
		r := v.apply(t, recoveryFixtureStart(fmt.Sprintf("successor-%d", i), authorization, subjects[i]))
		if !r.LaunchAuthorized {
			t.Fatalf("successor start of %s was not authorized", obligations[i].ID)
		}
		successors[originals[i]] = r.ID
		v.runID = r.ID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "fixture assertion failed"}})
	}
	for _, attempt := range observer.snapshotFor(t, oldContext).Attempts {
		for i, entry := range want {
			if attempt.ID != entry.RunID {
				continue
			}
			if len(attempt.Recovery) != 1 || attempt.Recovery[0].ID != authorization || attempt.Recovery[0].Disposition != request.Dispositions[i].Disposition || attempt.Recovery[0].Successor == nil || attempt.Recovery[0].Successor.RunID != successors[attempt.ID] {
				t.Fatalf("attempt %s authorizations = %+v, want one bound to its own successor %s", attempt.ID, attempt.Recovery, successors[attempt.ID])
			}
		}
	}
}

// runRecoveryDisposesAttemptsAcrossContexts covers a disposition set that
// spans two contexts of one verify order: a gated attempt that ended in the
// first context, and a still-running attempt with an unresolved operation in
// the second. One recovery with mixed dispositions authorizes both, cancels
// the running attempt, authorizes its operation, and resolves no operation
// (component-verification-service invariant 45; DEC-133(1), (2)).
func runRecoveryDisposesAttemptsAcrossContexts(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	digest := verificationSHA([]byte("{}"))
	gated := recoveryFixtureObligation("gated-"+v.access.TaskID, "operator_action_required", "")
	mutating := recoveryFixtureObligation("mutating-"+v.access.TaskID, "operator_action_required", "step")
	gatedSubject := recoveryFixtureRegister(t, &v, gated)
	r := v.apply(t, recoveryFixtureStart("context-a", "", gatedSubject))
	v.runID = r.ID
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "fixture environment unavailable"}})
	contextA, runA := v.contextID, r.ID
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	order.State, order.LastAttemptID, order.LastAttemptOutcome = core.WorkOrderQueued, order.AttemptID, core.WorkOrderOutcomeExpired
	order.SessionID, order.AttemptID, order.ClaimantID, order.WorkerID, order.ClientTokenHash = "", "", "", "", ""
	order.LeaseExpiresAt = time.Time{}
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, order, core.WorkOrderCmdRelease))

	recoveryFixtureReclaim(t, x, &v, "cross-context")
	recoveryFixtureRegister(t, &v, gated)
	mutatingSubject := recoveryFixtureRegister(t, &v, mutating)
	r = v.apply(t, recoveryFixtureStart("context-b", "", mutatingSubject))
	v.runID = r.ID
	op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "context-b-step", Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: digest}})
	contextB, runB := v.contextID, r.ID
	recoveryFixtureLoseClaim(t, x, &v)

	entries := []nextaction.Disposition{
		{ContextID: contextA, RunID: runA, OperationIDs: []string{}, InputDigest: digest},
		{ContextID: contextB, RunID: runB, OperationIDs: []string{op.ID}, InputDigest: digest},
	}
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	reason := "Operator inspected the provider and the fixture environment."
	before := verificationPublicState(t, &observer)
	if _, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, recoveryFixtureRequest(reason, entries[1:], "not_applied")), x.Backend, order.ID, "cross-omitted", time.Hour); !errors.Is(err, store.ErrVerificationInvalid) {
		t.Fatalf("recovery without the earlier context's attempt: %v, want an invalid recovery refusal", err)
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, &observer)) {
		t.Fatal("refused cross-context recovery changed the aggregate")
	}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, recoveryFixtureRequest(reason, entries, "applied", "not_applied")), x.Backend, order.ID, "cross-recovery", time.Hour)
	requireOK(t, err)
	if n := recoveryFixtureAuthorized(t, x, v.access.TaskID); n != 2 {
		t.Fatalf("cross-context recovery appended %d recovery_authorized events, want 2", n)
	}
	for _, attempt := range observer.snapshotFor(t, contextA).Attempts {
		if attempt.ID == runA && (len(attempt.Recovery) != 1 || attempt.Recovery[0].Disposition != "applied" || attempt.Recovery[0].Original.ContextID != contextA) {
			t.Fatalf("context A attempt authorizations = %+v", attempt.Recovery)
		}
	}
	snapshot := observer.snapshotFor(t, contextB)
	for _, attempt := range snapshot.Attempts {
		if attempt.ID == runB && (attempt.State != "cancelled" || attempt.EndedAt == nil || len(attempt.Recovery) != 1 || attempt.Recovery[0].Disposition != "not_applied") {
			t.Fatalf("running attempt after recovery = state %s, authorizations %+v; want cancelled with one authorization", attempt.State, attempt.Recovery)
		}
	}
	for _, operation := range snapshot.Operations {
		if operation.ID == op.ID && (len(operation.Recovery) != 1 || operation.Recovery[0].ID != "recovery:cross-recovery" || operation.Recovery[0].Disposition != "not_applied") {
			t.Fatalf("operation authorizations = %+v", operation.Recovery)
		}
	}
}

// serverSelectedStart starts subject under key with no authorization in the
// command. The server selects the authorization that a gated start consumes
// (component-verification-service invariant 19; DEC-133(3)).
func serverSelectedStart(key string, subject core.VerificationSubject) store.VerificationCommand {
	return store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: key, Attempt: &store.VerificationAttempt{Subject: subject, SafeInputs: map[string]json.RawMessage{}, Environment: recoveryFixtureEnvironment}}
}

// operationAuthorizationSuccessor returns the Successor of the one operator
// authorization that operation opID records. The observer reads the
// operation's original context with operator access.
func operationAuthorizationSuccessor(t *testing.T, observer *verificationFixture, contextID, opID string) *core.VerificationBinding {
	t.Helper()
	for _, op := range observer.snapshotFor(t, contextID).Operations {
		if op.ID == opID {
			if len(op.Recovery) != 1 {
				t.Fatalf("operation %s authorizations = %+v, want one", opID, op.Recovery)
			}
			return op.Recovery[0].Successor
		}
	}
	t.Fatalf("operation %s is missing from context %s", opID, contextID)
	return nil
}

// assertOperationAuthorization checks the operation's one authorization: a
// reconciliation leaves it unused (runID ""), and a re-preparation binds it to
// the run that re-prepared (component-verification-service invariants 25 and
// 45).
func assertOperationAuthorization(t *testing.T, observer *verificationFixture, contextID, opID, runID string) {
	t.Helper()
	successor := operationAuthorizationSuccessor(t, observer, contextID, opID)
	if runID == "" {
		if successor != nil {
			t.Fatalf("operation %s authorization was consumed by %+v, want it unused", opID, successor)
		}
		return
	}
	if successor == nil || successor.RunID != runID {
		t.Fatalf("operation %s authorization successor = %+v, want run %s", opID, successor, runID)
	}
}

// refuseConsumedReconciliation checks that a reconciliation after the
// re-preparation consumed the operation's authorization is refused and leaves
// every record unchanged (component-verification-service invariant 25).
func refuseConsumedReconciliation(t *testing.T, x Fixture, v, observer *verificationFixture, opID string) {
	t.Helper()
	before := verificationPublicState(t, observer)
	_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: opID}, Observation: &store.VerificationOperationObservation{State: "not_applied", Source: "late provider read", CapturedAt: time.Now().UTC()}}))
	if !errors.Is(err, store.ErrVerificationState) {
		t.Fatalf("reconciliation under a consumed authorization: %v, want %v", err, store.ErrVerificationState)
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, observer)) {
		t.Fatal("refused reconciliation changed the aggregate")
	}
}

// runServerSelectsRecoveryAuthorizations covers the verifier side of one
// recovery with two dispositions. No start names an authorization. In the
// successor context the runner starts safe_to_replay subjects before, between,
// and after the two operator_action_required subjects. Each gated start is
// admitted, records recovery:<request_id>, and binds only its own subject's
// authorization, although both authorizations share that ID. A safe start and
// a receipt replay consume nothing. A second gated start without a fresh
// authorization is refused and changes nothing (component-verification-service
// invariants 19 and 45; DEC-133(3); req-verification-kits AC-3.4).
func runServerSelectsRecoveryAuthorizations(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	gated := []store.VerificationObligation{
		recoveryFixtureObligation("login-"+v.access.TaskID, "operator_action_required", ""),
		recoveryFixtureObligation("checkout-"+v.access.TaskID, "operator_action_required", ""),
	}
	var safe []store.VerificationObligation
	for i := 0; i < 3; i++ {
		safe = append(safe, recoveryFixtureObligation(fmt.Sprintf("read-%d-%s", i, v.access.TaskID), "safe_to_replay", ""))
	}
	gatedSubjects := make([]core.VerificationSubject, len(gated))
	safeSubjects := make([]core.VerificationSubject, len(safe))
	register := func() {
		for i := range gated {
			gatedSubjects[i] = recoveryFixtureRegister(t, &v, gated[i])
		}
		for i := range safe {
			safeSubjects[i] = recoveryFixtureRegister(t, &v, safe[i])
		}
	}
	register()
	originals := make([]string, len(gated))
	for i, subject := range append(append([]core.VerificationSubject{}, gatedSubjects...), safeSubjects...) {
		r := v.apply(t, serverSelectedStart(fmt.Sprintf("first-%d", i), subject))
		if !r.LaunchAuthorized {
			t.Fatalf("first start %d was not authorized", i)
		}
		v.runID = r.ID
		state := "failed"
		if i < len(gated) {
			originals[i], state = r.ID, "blocked"
		}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: state, Explanation: "fixture environment unavailable"}})
	}
	for _, attempt := range v.snapshot(t).Attempts {
		if attempt.ReplayAuthorizationID != "" {
			t.Fatalf("first start %s records authorization %q", attempt.ID, attempt.ReplayAuthorizationID)
		}
	}
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: verificationFixtureCoverage(v.snapshot(t)), Feedback: "Two exercises need an operator", RequiredAction: "Restore the fixtures and authorize one retry of each"}})
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	oldContext := v.contextID
	action := projectVerificationDisposition(t, x, order.TaskID, map[core.Capability]bool{core.CapabilityOperateGates: true})
	if len(action.Details.Dispositions) != 2 {
		t.Fatalf("dispositions = %+v, want both gated attempts", action.Details.Dispositions)
	}
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	requestID := "selected-recovery"
	request := recoveryFixtureRequest("Operator restored both fixtures and authorizes one retry of each.", action.Details.Dispositions, "applied", "not_applied")
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, request), x.Backend, order.ID, requestID, time.Hour)
	requireOK(t, err)
	authorization := "recovery:" + requestID

	recoveryFixtureReclaim(t, x, &v, "selected-successor")
	register()
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	successors := make([]string, len(gated))
	// assertBindings checks each gated attempt's one authorization against the
	// successors started so far.
	assertBindings := func() {
		t.Helper()
		for _, attempt := range observer.snapshotFor(t, oldContext).Attempts {
			for i, original := range originals {
				if attempt.ID != original {
					continue
				}
				if len(attempt.Recovery) != 1 || attempt.Recovery[0].ID != authorization {
					t.Fatalf("attempt %s authorizations = %+v, want one %s", original, attempt.Recovery, authorization)
				}
				got := attempt.Recovery[0].Successor
				if successors[i] == "" && got != nil {
					t.Fatalf("authorization of %s was consumed by %+v before its subject started", original, got)
				}
				if successors[i] != "" && (got == nil || got.RunID != successors[i]) {
					t.Fatalf("authorization of %s successor = %+v, want its own successor %s", original, got, successors[i])
				}
			}
		}
	}
	// stored returns the authorization ID that attempt runID records.
	stored := func(runID string) string {
		t.Helper()
		for _, attempt := range v.snapshot(t).Attempts {
			if attempt.ID == runID {
				return attempt.ReplayAuthorizationID
			}
		}
		t.Fatalf("attempt %s is missing", runID)
		return ""
	}
	type step struct {
		key     string
		subject core.VerificationSubject
		gated   int
	}
	steps := []step{
		{"selected-safe-0", safeSubjects[0], -1},
		{"selected-gated-0", gatedSubjects[0], 0},
		{"selected-safe-1", safeSubjects[1], -1},
		{"selected-gated-1", gatedSubjects[1], 1},
		{"selected-safe-2", safeSubjects[2], -1},
	}
	for _, s := range steps {
		r := v.apply(t, serverSelectedStart(s.key, s.subject))
		if !r.LaunchAuthorized {
			t.Fatalf("start %s was not authorized", s.key)
		}
		want := ""
		if s.gated >= 0 {
			successors[s.gated], want = r.ID, authorization
		}
		if got := stored(r.ID); got != want {
			t.Fatalf("start %s records authorization %q, want %q", s.key, got, want)
		}
		assertBindings()
		replay := v.apply(t, serverSelectedStart(s.key, s.subject))
		if replay.ID != r.ID || replay.LaunchAuthorized {
			t.Fatalf("replay of %s = %+v, want %s without a launch", s.key, replay, r.ID)
		}
		assertBindings()
		v.runID = r.ID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "fixture assertion failed"}})
	}
	for i, subject := range gatedSubjects {
		c := serverSelectedStart(fmt.Sprintf("selected-gated-again-%d", i), subject)
		before := verificationPublicState(t, &observer)
		_, err = x.Backend.ApplyVerification(v.ctx, v.command(c))
		var refusal *store.VerificationRefusal
		if !errors.As(err, &refusal) || refusal.Code != store.VerificationCodeRetryAuthorizationRequired || refusal.Field != "subject" {
			t.Fatalf("second gated start %d: %v, want %s on field subject", i, err, store.VerificationCodeRetryAuthorizationRequired)
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &observer)) {
			t.Fatalf("refused second gated start %d changed the aggregate", i)
		}
	}
	again := v.apply(t, serverSelectedStart("selected-safe-again", safeSubjects[0]))
	if !again.LaunchAuthorized || stored(again.ID) != "" {
		t.Fatal("a safe start after both gated retries was refused or recorded an authorization")
	}
	assertBindings()
}

// runOperationAuthorizationServesOneReprepare covers the operation side with
// no caller ID. A reconciliation selects the operation's authorization and
// leaves it unused. The re-preparation that follows consumes it. A later
// recovery that names only the attempt admits a fresh start, but that start
// cannot re-prepare the operation, because its one authorization is consumed
// (component-verification-service invariants 25 and 45; DEC-133(3)).
func runOperationAuthorizationServesOneReprepare(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	digest := verificationSHA([]byte("{}"))
	obligation := recoveryFixtureObligation("provider-"+v.access.TaskID, "operator_action_required", "step")
	v.subject = recoveryFixtureRegister(t, &v, obligation)
	v.start(t, "provider-original")
	key := "provider-operation-" + v.runID
	prepare := store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: digest}}
	op := v.apply(t, prepare)
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "provider acknowledgement lost"}})
	oldContext, oldRun := v.contextID, v.runID
	recoveryFixtureLoseClaim(t, x, &v)
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	first := &store.VerificationRecovery{Reason: "Operator read the provider and found no change.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: oldContext, RunID: oldRun, OperationIDs: []string{op.ID}, InputDigest: digest, Disposition: "not_applied"}}}
	_, err := RecoverWorkOrder(store.WithVerificationRecovery(operator, first), x.Backend, v.access.WorkOrderID, "provider-recovery", time.Hour)
	requireOK(t, err)
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID

	recoveryFixtureReclaim(t, x, &v, "provider-successor")
	v.subject.ContractDigest = recoveryFixtureRegister(t, &v, obligation).ContractDigest
	GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "provider-reconciliation", v.subject, nil)
	reconcile := store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, Observation: &store.VerificationOperationObservation{State: "not_applied", Source: "fresh provider read", CapturedAt: time.Now().UTC()}}
	v.apply(t, reconcile)
	assertOperationAuthorization(t, &observer, oldContext, op.ID, "")
	// An identical reconciliation replay appends nothing and consumes nothing.
	v.apply(t, reconcile)
	assertOperationAuthorization(t, &observer, oldContext, op.ID, "")
	started := v.apply(t, serverSelectedStart("provider-successor", v.subject))
	if !started.LaunchAuthorized {
		t.Fatal("the attempt authorization did not admit the successor start")
	}
	v.runID = started.ID
	if prepared := v.apply(t, prepare); prepared.ID != op.ID {
		t.Fatal("the re-preparation replaced the operation identity")
	}
	assertOperationAuthorization(t, &observer, oldContext, op.ID, started.ID)
	// The same run's receipt replay consumes nothing more.
	v.apply(t, prepare)
	assertOperationAuthorization(t, &observer, oldContext, op.ID, started.ID)
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "the provider call was not sent"}})

	// The operation is resolved as not_applied, so a second recovery names
	// only the successor attempt and issues no operation authorization.
	recoveryFixtureLoseClaim(t, x, &v)
	second := &store.VerificationRecovery{Reason: "Operator authorizes one more retry.", Dispositions: []store.VerificationRecoveryDisposition{{ContextID: v.contextID, RunID: started.ID, OperationIDs: []string{}, InputDigest: digest, Disposition: "not_applied"}}}
	_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, second), x.Backend, v.access.WorkOrderID, "provider-second-recovery", time.Hour)
	requireOK(t, err)
	recoveryFixtureReclaim(t, x, &v, "provider-third")
	v.subject.ContractDigest = recoveryFixtureRegister(t, &v, obligation).ContractDigest
	third := v.apply(t, serverSelectedStart("provider-third", v.subject))
	if !third.LaunchAuthorized {
		t.Fatal("the second attempt authorization did not admit a start")
	}
	v.runID = third.ID
	before := verificationPublicState(t, &observer)
	if _, err = x.Backend.ApplyVerification(v.ctx, v.command(prepare)); !errors.Is(err, store.ErrVerificationState) {
		t.Fatalf("a fresh second re-preparation under a consumed authorization: %v, want %v", err, store.ErrVerificationState)
	}
	if !reflect.DeepEqual(before, verificationPublicState(t, &observer)) {
		t.Fatal("the refused re-preparation changed the aggregate")
	}
	assertOperationAuthorization(t, &observer, oldContext, op.ID, started.ID)
}
