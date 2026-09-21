package storetest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
	t.Run("SealedReviewAcceptance", func(t *testing.T) { runSealedVerificationReview(t, x) })
	t.Run("OperatorRecoveryBindsSuccessorAndConsumesAuthorization", func(t *testing.T) {
		v := newVerificationFixture(t, x)
		obligation := store.VerificationObligation{ID: "recovery-" + v.access.TaskID, Description: "Recover one external mutation", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "recovery", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
		registered := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: registered.Digest}
		v.start(t, "operator-original")
		key := "recovery-operation-" + v.runID
		op := v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: key, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "provider acknowledgement lost"}})
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
	for _, mutate := range []func(*core.ReviewDecision){
		func(d *core.ReviewDecision) { d.VerificationAssessment = nil },
		func(d *core.ReviewDecision) { d.HeadSHA = strings.Repeat("b", 40) },
		func(d *core.ReviewDecision) { d.ReviewScope = "delta" },
		func(d *core.ReviewDecision) { d.BaselineSHA = "wrong" },
		func(d *core.ReviewDecision) { d.VerificationAssessment.ContextIDs = []string{"unknown"} },
		func(d *core.ReviewDecision) {
			d.VerificationAssessment.Mappings = []core.VerificationAssessmentMapping{}
		},
		func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].Version = 2 },
		func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].AcceptanceCriterionID = "AC-99.1" },
		func(d *core.ReviewDecision) { d.VerificationAssessment.Mappings[0].EvidenceIDs = []string{"foreign"} },
	} {
		var bad core.ReviewDecision
		requireOK(t, json.Unmarshal(core.JSONPayload(valid), &bad))
		mutate(&bad)
		before := verificationPublicState(t, &v)
		if err := taskops.New(x.Backend).AcceptReviewDecision(ctx, bad); err == nil {
			t.Fatal("invalid sealed assessment accepted")
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
	obligation := store.VerificationObligation{ID: "reconcile-" + v.access.TaskID, Description: "Inspect provider state", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "reconcile", Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, RetryPolicy: "reconciliation_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
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
	obligation := store.VerificationObligation{ID: "checkpoint-" + v.access.TaskID, Description: "External action", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "action", Kind: "script", Argv: []string{"action"}, TimeoutSeconds: 30, RequiredAssertions: []string{}, RetryPolicy: "operator_action_required", Operations: []verification.Operation{{ID: "step", TargetBinding: "fixture"}}}}
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	v.start(t, "action")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationPrepareOperation, Key: "checkpoint-" + v.runID, Operation: &store.VerificationOperation{StepID: "step", Target: "fixture", InputDigest: verificationSHA([]byte("{}"))}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Provider acknowledgement is unknown; operator inspection required"}})
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "Inspect provider and authorize disposition"}})
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
