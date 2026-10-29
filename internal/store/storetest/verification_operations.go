package storetest

import (
	"context"
	"encoding/json"
	"errors"
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
	t.Run("NextActionDispositionBinding", func(t *testing.T) { runNextActionDispositionBinding(t, x) })
	t.Run("RetryGateScopedToVerifyOrder", func(t *testing.T) { runVerificationRetryGateScope(t, x) })
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
		disposition := &store.VerificationRecoveryDisposition{ContextID: oldContext, RunID: oldRun, OperationIDs: []string{op.ID}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied", Reason: "Operator inspected provider state and authorizes one retry."}
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
		claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "successor", ClientToken: "successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
		order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
		requireOK(t, err)
		v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "worker", SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
		v.contextID, v.runID = "", ""
		contextReceipt := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}})
		v.contextID = contextReceipt.ID
		registered = v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject.ContractDigest = registered.Digest
		authorization := "recovery:operation-recovery"
		start := store.VerificationCommand{Kind: store.VerificationStartAttempt, Key: "successor-start", Attempt: &store.VerificationAttempt{Subject: v.subject, SafeInputs: map[string]json.RawMessage{}, ReplayAuthorizationID: authorization}}
		if _, err = x.Backend.ApplyVerification(v.ctx, v.command(start)); err == nil {
			t.Fatal("disposition silently replaced reconciliation")
		}
		GrantVerificationFixture(t, x.Backend, x.Context, v.access.TaskID, v.access.WorkOrderID, v.contextID, "successor-reconciliation", v.subject, nil)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationReconcileOperation, Operation: &store.VerificationOperation{ID: op.ID}, ReplayAuthorizationID: authorization, Observation: &store.VerificationOperationObservation{State: "not_applied", Source: "fresh provider read", CapturedAt: time.Now().UTC()}})
		started := v.apply(t, start)
		v.runID = started.ID
		prepared := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, ReplayAuthorizationID: authorization, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		if prepared.ID != op.ID {
			t.Fatal("successor replaced operation identity")
		}
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
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{WorkerID: "reviewer", ClaimantID: "reviewer", SessionID: "reviewer", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: verify.ServedRequirementSnapshot})
	requireOK(t, err)
	ctx := store.WithActor(x.Context, store.Actor{ID: "worker:reviewer", Role: core.ActorWorker})
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
			if body.Assessment == nil || body.Assessment.Actor != "worker:reviewer" {
				t.Fatal("review lost authenticated assessment attribution")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("accepted review was not durable")
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

// projectVerificationDisposition projects the task with row 10 selected. A
// sealed release also records the checkpoint's decision_request, which row 9
// (agent_question) outranks, so the projection reads the same stored order
// with only that agent-question marker withheld; the binding itself comes
// from the backend's verification records
// (feature-operator-next-action NA-3, Disposition binding).
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

// runNextActionDispositionBinding proves that row 10's details.disposition
// names the sealed checkpoint's context, the attempt that recorded its
// unresolved operation, that operation, and the attempt's safe-input digest,
// so a recovery built only from those fields, a disposition, and a reason
// passes PrepareVerificationRecovery on the ordinary recovery path. Row 9
// never carries the binding, the response exposes no verification record,
// and a checkpoint without an unresolved operation offers neither
// disposition (feature-operator-next-action NA-3; req-operator-next-action
// AC-1.4, AC-2.2; component-verification-service).
func runNextActionDispositionBinding(t *testing.T, x Fixture) {
	gates := map[core.Capability]bool{core.CapabilityOperateGates: true}
	v := newVerificationFixture(t, x, true)
	order, operationID := sealVerificationCheckpoint(t, &v, true)

	question, err := store.ReadNextAction(x.Context, x.Backend, order.TaskID, store.NextActionOptions{Now: time.Now().UTC(), Capabilities: gates})
	requireOK(t, err)
	if question.Kind != nextaction.KindAgentQuestion || question.Details.Disposition != nil {
		t.Fatalf("sealed checkpoint = %s disposition %+v, want agent_question without a binding", question.Kind, question.Details.Disposition)
	}

	action := projectVerificationDisposition(t, x, order.TaskID, gates)
	want := &nextaction.Disposition{ContextID: v.contextID, RunID: v.runID, OperationIDs: []string{operationID}, InputDigest: verificationSHA([]byte("{}"))}
	if !reflect.DeepEqual(action.Details.Disposition, want) {
		t.Fatalf("disposition = %+v, want %+v (refusals %+v)", action.Details.Disposition, want, action.Details.Refusals)
	}
	for _, id := range []string{"applied", "not_applied"} {
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
		if (choice.ID == "applied" || choice.ID == "not_applied") && choice.Allowed {
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
	disposition := &store.VerificationRecoveryDisposition{ContextID: want.ContextID, RunID: want.RunID, OperationIDs: want.OperationIDs, InputDigest: want.InputDigest, Disposition: "not_applied", Reason: "Operator inspected the provider; the change did not apply."}
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

	empty := newVerificationFixture(t, x, true)
	bare, _ := sealVerificationCheckpoint(t, &empty, false)
	unbound := projectVerificationDisposition(t, x, bare.TaskID, gates)
	if unbound.Details.Disposition != nil {
		t.Fatalf("checkpoint without unresolved operations bound %+v", unbound.Details.Disposition)
	}
	for _, id := range []string{"applied", "not_applied"} {
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
		disposition := &store.VerificationRecoveryDisposition{ContextID: oldContext, RunID: oldRun, OperationIDs: []string{}, InputDigest: verificationSHA([]byte("{}")), Disposition: "not_applied", Reason: "Operator restored the environment and authorizes one retry."}
		_, err = RecoverWorkOrder(store.WithVerificationRecovery(operator, disposition), x.Backend, order.ID, "gate-recovery", time.Hour)
		requireOK(t, err)
		claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "gate-successor", ClientToken: "gate-successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: order.ServedRequirementSnapshot, Governance: order.GovernanceSnapshot}
		order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
		requireOK(t, err)
		v.access = store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, Claim: core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "worker", SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
		v.contextID, v.runID = "", ""
		v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "gate-successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}}).ID
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject.ContractDigest = r.Digest
		// The successor context belongs to the same order, so the gate still
		// reads the original attempt.
		refused(t, &v, start("gated-successor-bare", "", v.subject), store.VerificationCodeRetryAuthorizationRequired)
		authorization := "recovery:gate-recovery"
		started := v.apply(t, start("gated-successor", authorization, v.subject))
		if !started.LaunchAuthorized {
			t.Fatal("recovery authorization did not admit the successor attempt")
		}
		v.runID = started.ID
		failed(t, &v)
		refused(t, &v, start("gated-successor-reuse", authorization, v.subject), store.VerificationCodeRetryAuthorizationRequired)
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
	o, err = ClaimWorkOrder(ctx, st, o.ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "resubmit", ClientToken: "resubmit-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
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
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "later-verify", ClientToken: "later-verify-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
	claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	o, err = ClaimWorkOrder(ctx, st, o.ID, claim)
	requireOK(t, err)
	next := v
	next.access = store.VerificationAccess{TaskID: o.TaskID, WorkOrderID: o.ID, WorkOrderAttemptID: o.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}}
	next.revisions = []core.VerificationRevision{{Repository: v.revisions[0].Repository, RemoteIdentity: v.revisions[0].RemoteIdentity, SHA: head}}
	next.contextID, next.runID = "", ""
	next.contextID = next.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "later-verify", Context: &store.VerificationContext{ReviewScope: o.ReviewScope, BaselineSHA: o.BaselineSHA, Revisions: next.revisions, GoverningPins: next.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": next.revisions[0].Repository, "revision": head, "state": "no_manifest"})}}}).ID
	next.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}})
	return next
}
