package storetest

import (
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

// feature-verification-kit-execution VK-13; req-verification-kits AC-3.2,
// AC-3.3, AC-3.4 and AC-4.3: grounded operator checkpoints on every backend.
func runVerificationCheckpoints(t *testing.T, x Fixture) {
	t.Run("MissingGrantBeforeExecution", func(t *testing.T) { runVerificationMissingGrantCheckpoint(t, x) })
	t.Run("TaskRepositoryHead", func(t *testing.T) { runVerificationCheckpointTaskRepositoryHead(t, x) })
	t.Run("WaitingAttempt", func(t *testing.T) { runVerificationWaitingCheckpoint(t, x) })
	t.Run("RetriedAttemptIsNotAGround", func(t *testing.T) { runVerificationRetriedAttemptGround(t, x) })
	t.Run("UnsupportedOutcomeLeavesStateUnchanged", func(t *testing.T) { runVerificationUnsupportedOutcome(t, x) })
}

// newUngrantedVerificationFixture registers one ordinary obligation without
// the fixture helper's automatic grant, so no attempt can be admitted.
func newUngrantedVerificationFixture(t *testing.T, x Fixture, permissions ...verification.Permission) verificationFixture {
	t.Helper()
	return registerUngrantedObligation(t, newVerificationFixture(t, x, true), permissions...)
}

func registerUngrantedObligation(t *testing.T, v verificationFixture, permissions ...verification.Permission) verificationFixture {
	t.Helper()
	obligation := store.VerificationObligation{ID: "ungranted-" + v.access.TaskID, Description: "Exercise needing operator permission", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "ungranted", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation", Permissions: permissions}}
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
	v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: obligation.ID, ContractDigest: r.Digest}
	return v
}

func verificationCheckpointSubmission(coverage store.VerificationCoverage) *store.VerificationSubmission {
	return &store.VerificationSubmission{Outcome: "operator_action_required", Coverage: coverage, Feedback: "No grant covers the network action after the grant wait.", RequiredAction: "Recover the verify order, then grant network access for the next claim's context."}
}

func runVerificationMissingGrantCheckpoint(t *testing.T, x Fixture) {
	v := newUngrantedVerificationFixture(t, x, verification.Permission{Kind: "network", TargetBinding: "fixture"})
	coverage := verificationFixtureCoverage(v.snapshot(t))
	if _, err := v.x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}})); !errors.Is(err, store.ErrVerificationState) {
		t.Fatalf("unstarted subject sealed success: %v", err)
	}
	seal := v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: verificationCheckpointSubmission(coverage)})
	before := verificationPublicState(t, &v)
	for _, point := range []string{"artifact", "evidence", "event", "queue"} {
		injected := errors.New("injected " + point)
		_, err := x.Backend.ApplyVerification(store.WithVerificationFault(v.ctx, func(step string) error {
			if step == point {
				return injected
			}
			return nil
		}), seal)
		if !errors.Is(err, injected) {
			t.Fatalf("%s: %v", point, err)
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
			t.Fatalf("%s fault partially committed the checkpoint", point)
		}
	}
	snapshot := v.snapshot(t)
	receipt := v.apply(t, seal)
	if receipt.ID != v.contextID || receipt.State != "operator_action_required" || receipt.NextStage != string(core.StageVerify) || len(receipt.Grounds) != 1 || receipt.Grounds[0].Kind != store.VerificationGroundMissingGrant {
		t.Fatalf("checkpoint receipt = %+v", receipt)
	}
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	after := observer.snapshot(t)
	if len(after.Attempts) != 0 || len(after.PermissionGrants) != len(snapshot.PermissionGrants) || len(after.PermissionGrants) != 0 {
		t.Fatal("checkpoint fabricated an attempt or grant")
	}
	vc := after.Contexts[0]
	if vc.SealedAt == nil || vc.Result == nil || vc.Result.Checkpoint == nil {
		t.Fatal("checkpoint record not sealed")
	}
	cp := vc.Result.Checkpoint
	if len(cp.Grounds) != 1 || cp.Grounds[0].Kind != store.VerificationGroundMissingGrant || !cp.Grounds[0].ServerVerified || cp.Grounds[0].Subject != v.subject || len(cp.Grounds[0].Permissions) != 1 || len(cp.MissingSubjects) != 1 || cp.RequiredAction == "" || cp.SessionID != v.access.Claim.SessionID || vc.Result.Binding.WorkOrderAttemptID != v.access.WorkOrderAttemptID {
		t.Fatalf("checkpoint grounds = %+v", cp)
	}
	reader := x.Backend.(store.VerificationReader)
	page, err := reader.ReadVerificationPage(operator, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, store.VerificationPageRequest{Kind: "contexts", ContextID: v.contextID})
	requireOK(t, err)
	var header map[string]string
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0].Metadata, &header) != nil || header["outcome"] != "operator_action_required" || header["reason"] != cp.Reason || header["required_action"] != cp.RequiredAction || !strings.Contains(header["checkpoint_grounds"], store.VerificationGroundMissingGrant) || !strings.Contains(header["checkpoint_grounds"], "evidence missing") || header["checkpoint_head"] != v.revisions[0].SHA || header["checkpoint_attempt"] != v.access.WorkOrderAttemptID || strings.Contains(string(page.Items[0].Metadata), "client_token") {
		t.Fatalf("checkpoint projection = %+v", header)
	}
	task, err := x.Backend.GetTask(v.ctx, v.access.TaskID)
	requireOK(t, err)
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	if task.NextStage != core.StageVerify || task.RecoveryStage != "" || order.State != core.WorkOrderQueued || !order.RetrySuppressed || order.SessionID != "" || !order.LeaseExpiresAt.IsZero() || !order.ExecutionDeadline.IsZero() || order.VerificationContextID != "" {
		t.Fatalf("checkpoint lifecycle = task %+v order %+v", task, order)
	}
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil || order.Checkpoint.Verification.ContextID != v.contextID || order.Checkpoint.Verification.HeadSHA != order.HeadSHA || len(order.Checkpoint.Verification.Grounds) != 1 || order.Checkpoint.Verification.Grounds[0].Kind != store.VerificationGroundMissingGrant {
		t.Fatalf("checkpoint reference = %+v", order.Checkpoint)
	}
	// VK-13.5: the authenticated reference names the declared permission an
	// operator grants, and the summary carries it for historical contexts.
	if ground := order.Checkpoint.Verification.Grounds[0]; !reflect.DeepEqual(ground.Permissions, []core.WorkOrderVerificationCheckpointPermission{{Kind: "network", TargetBinding: "fixture"}}) || len(ground.EvidenceIDs) != 0 || ground.AttemptID != "" || ground.Truncated {
		t.Fatalf("missing-grant reference ground = %+v", ground)
	}
	if !strings.Contains(header["checkpoint_grounds"], "requires network:fixture") {
		t.Fatalf("checkpoint summary lacks permission: %q", header["checkpoint_grounds"])
	}
	orders, err := x.Backend.ListTaskWorkOrders(v.ctx, v.access.TaskID)
	requireOK(t, err)
	for _, o := range orders {
		if o.Stage == core.StageReview {
			t.Fatal("checkpoint admitted review")
		}
	}
	if cp.Claim.ClientTokenHash != "" || cp.Claim.SessionID != v.access.Claim.SessionID || cp.HeadSHA != v.revisions[0].SHA || cp.WorkOrderAttemptID != v.access.WorkOrderAttemptID {
		t.Fatalf("retained claim exposed or incomplete: %+v", cp.Claim)
	}
	// VK-STORE-16: the exact retained claim replays the identical submission
	// and receives the original receipt without another lifecycle event.
	state := verificationPublicState(t, &observer)
	replayed, err := x.Backend.ApplyVerification(v.ctx, seal)
	requireOK(t, err)
	if !reflect.DeepEqual(replayed, receipt) {
		t.Fatalf("replay receipt = %+v, want %+v", replayed, receipt)
	}
	if !reflect.DeepEqual(state, verificationPublicState(t, &observer)) {
		t.Fatal("identical replay changed the aggregate")
	}
	changed := seal
	changedSubmission := *seal.Submission
	changedSubmission.RequiredAction = "A different operator act"
	changed.Submission = &changedSubmission
	wrongToken := seal
	wrongToken.Access.ClientToken = "not-the-claim-token"
	staleSession := seal
	staleSession.Access.Claim.SessionID = "stale-session"
	success := seal
	success.Submission = &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}
	for name, c := range map[string]store.VerificationCommand{"changed": changed, "wrong token": wrongToken, "stale session": staleSession, "success": success} {
		if _, err = x.Backend.ApplyVerification(v.ctx, c); err == nil {
			t.Fatalf("%s replay accepted", name)
		}
		if !reflect.DeepEqual(state, verificationPublicState(t, &observer)) {
			t.Fatalf("%s replay changed the aggregate", name)
		}
	}
	// Recovery without disposition returns to verify at the same head; the
	// successor claim prepares a distinct context and the checkpoint stays.
	recovered, err := RecoverWorkOrder(operator, x.Backend, order.ID, "checkpoint-recovery", time.Hour)
	requireOK(t, err)
	if recovered.State != core.WorkOrderQueued || recovered.RetrySuppressed || recovered.HeadSHA != order.HeadSHA || recovered.Stage != core.StageVerify {
		t.Fatalf("recovered order = %+v", recovered)
	}
	recoveryReplay, err := RecoverWorkOrder(operator, x.Backend, order.ID, "checkpoint-recovery", time.Hour)
	requireOK(t, err)
	if recoveryReplay.State != recovered.State || recoveryReplay.RetrySuppressed {
		t.Fatal("recovery replay changed the order")
	}
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "checkpoint-successor", ClientToken: "checkpoint-successor-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: recovered.ServedRequirementSnapshot, Governance: recovered.GovernanceSnapshot}
	successor, err := ClaimWorkOrder(x.Context, x.Backend, order.ID, claim)
	requireOK(t, err)
	if successor.AttemptID == v.access.WorkOrderAttemptID {
		t.Fatal("successor reused the checkpoint attempt")
	}
	oldContext := v.contextID
	v.access = store.VerificationAccess{TaskID: successor.TaskID, WorkOrderID: successor.ID, WorkOrderAttemptID: successor.AttemptID, Claim: core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "worker", SessionID: claim.SessionID}, ClientToken: claim.ClientToken}
	v.contextID = ""
	next := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "checkpoint-successor", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins}})
	if next.ID == "" || next.ID == oldContext {
		t.Fatal("successor reused the checkpoint context")
	}
	if _, err = x.Backend.ApplyVerification(v.ctx, seal); err == nil {
		t.Fatal("retained claim replayed after a successor claim")
	}
	history := observer.snapshotFor(t, oldContext)
	if history.Contexts[0].Result == nil || history.Contexts[0].Result.Checkpoint == nil {
		t.Fatal("recovery relabelled the retained checkpoint")
	}
	orders, err = x.Backend.ListTaskWorkOrders(v.ctx, v.access.TaskID)
	requireOK(t, err)
	for _, o := range orders {
		if o.Stage == core.StageReview {
			t.Fatal("recovery admitted review")
		}
	}
}

// runVerificationCheckpointTaskRepositoryHead retains the task repository's
// submitted head when an additional repository sorts first in the scope
// (VK-13.3; req-verification-evidence REQ-1/AC-1.2).
func runVerificationCheckpointTaskRepositoryHead(t *testing.T, x Fixture) {
	// Both repositories are registered by the conformance factory; the
	// additional repository "app" sorts before the task repository "conveyor".
	primary := core.VerificationRevision{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: strings.Repeat("a", 40)}
	secondary := core.VerificationRevision{Repository: "app", RemoteIdentity: "https://example.test/app", SHA: strings.Repeat("b", 40)}
	v := registerUngrantedObligation(t, newVerificationFixtureIn(t, x, []core.VerificationRevision{primary, secondary}, true))
	if v.revisions[0] != secondary {
		t.Fatalf("fixture scope order = %+v", v.revisions)
	}
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: verificationCheckpointSubmission(coverage)})
	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	cp := observer.snapshot(t).Contexts[0].Result.Checkpoint
	if cp == nil || cp.HeadSHA != primary.SHA {
		t.Fatalf("checkpoint head = %+v, want %s", cp, primary.SHA)
	}
	page, err := x.Backend.(store.VerificationReader).ReadVerificationPage(operator, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, store.VerificationPageRequest{Kind: "contexts", ContextID: v.contextID})
	requireOK(t, err)
	var header map[string]string
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0].Metadata, &header) != nil || header["checkpoint_head"] != primary.SHA {
		t.Fatalf("checkpoint projection head = %+v", header)
	}
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil || order.Checkpoint.Verification.HeadSHA != primary.SHA {
		t.Fatalf("checkpoint reference = %+v", order.Checkpoint)
	}
}

func runVerificationWaitingCheckpoint(t *testing.T, x Fixture) {
	v := newUngrantedVerificationFixture(t, x)
	v.start(t, "waiting")
	evidence := v.envelope("waiting-evidence", "waiting-batch", "operator prompt shown")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: evidence.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(evidence)}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "waiting", Explanation: "Operator interaction required"}})
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: verificationCheckpointSubmission(coverage)})
	operator, owner := bootstrapOwner(t, x)
	observer := v
	observer.ctx, observer.access.UserID = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser}), owner.ID
	cp := observer.snapshot(t).Contexts[0].Result.Checkpoint
	if cp == nil || len(cp.Grounds) != 1 || cp.Grounds[0].Kind != store.VerificationGroundAttemptWaiting || cp.Grounds[0].AttemptID != v.runID || cp.Grounds[0].Explanation != "Operator interaction required" || len(cp.Grounds[0].EvidenceIDs) != 1 || len(cp.EvidenceIDs) != 1 || len(cp.MissingSubjects) != 0 {
		t.Fatalf("waiting checkpoint = %+v", cp)
	}
	// VK-13.5: the reference links the waiting attempt and its retained evidence.
	order, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil || len(order.Checkpoint.Verification.Grounds) != 1 {
		t.Fatalf("waiting reference = %+v", order.Checkpoint)
	}
	if ground := order.Checkpoint.Verification.Grounds[0]; ground.AttemptID != v.runID || !reflect.DeepEqual(ground.EvidenceIDs, cp.Grounds[0].EvidenceIDs) || ground.Explanation != "Operator interaction required" || !ground.ServerVerified {
		t.Fatalf("waiting reference ground = %+v", ground)
	}
}

// runVerificationRetriedAttemptGround seals a checkpoint after one subject's
// blocked attempt was retried to success while another subject waits. Only
// the waiting subject's latest attempt is a ground, the context header names
// exactly that attempt, and both login attempts stay readable as history
// (feature-verification-kit-execution VK-13.2/VK-13.5; component-web-dashboard
// VK-WEB-6; req-verification-kits REQ-3/AC-3.4; req-verification-evidence
// REQ-1/AC-1.4).
func runVerificationRetriedAttemptGround(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x, true)
	register := func(id string) core.VerificationSubject {
		obligation := store.VerificationObligation{ID: id, Description: "Exercise " + id, Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: id, Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only observation"}}
		r := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &obligation})
		return core.VerificationSubject{Kind: "ordinary", ObligationID: id, ContractDigest: r.Digest}
	}
	login, approve := register("login"), register("approve")
	v.subject = login
	v.start(t, "login-blocked")
	blocked := v.runID
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "blocked", Explanation: "Login refused the fixture account"}})
	v.start(t, "login-retry")
	succeeded := v.runID
	zero, no := 0, false
	at := time.Now().UTC().Format(time.RFC3339Nano)
	report := v.envelope("login-report", "login-report", "")
	report.Type = "execution_report"
	report.Payload = core.JSONPayload(core.ExecutionReportPayload{Argv: []string{"check"}, Tool: "check", ToolVersion: "1", Runtime: "fixture", StartedAt: at, EndedAt: at, ExitCode: &zero, TimedOut: &no, Cancelled: &no, StdoutSHA256: strings.Repeat("a", 64), StderrSHA256: strings.Repeat("b", 64), StdoutTruncated: &no, StderrTruncated: &no})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: report.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(report)}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "succeeded", ExitCode: &zero}})
	v.subject = approve
	v.start(t, "approve-waiting")
	waiting := v.runID
	v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "waiting", Explanation: "Operator approval required"}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: verificationCheckpointSubmission(verificationFixtureCoverage(v.snapshot(t)))})

	operator, owner := bootstrapOwner(t, x)
	operator = store.WithActor(operator, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	observer := v
	observer.ctx, observer.access.UserID = operator, owner.ID
	cp := observer.snapshot(t).Contexts[0].Result.Checkpoint
	if cp == nil || len(cp.Grounds) != 1 || cp.Grounds[0].AttemptID != waiting || cp.Grounds[0].Kind != store.VerificationGroundAttemptWaiting || cp.AttemptGrounds != waiting {
		t.Fatalf("retried checkpoint = %+v", cp)
	}
	reader := x.Backend.(store.VerificationReader)
	read := store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
	page, err := reader.ReadVerificationPage(operator, read, store.VerificationPageRequest{Kind: "contexts", ContextID: v.contextID})
	requireOK(t, err)
	var header map[string]string
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0].Metadata, &header) != nil || header["checkpoint_attempt_grounds"] != waiting || header["truncated"] != "false" {
		t.Fatalf("checkpoint attempt grounds projection = %+v", header)
	}
	attempts, err := reader.ReadVerificationPage(operator, read, store.VerificationPageRequest{Kind: "attempts", ContextID: v.contextID, Limit: store.VerificationPageLimit})
	requireOK(t, err)
	states := map[string]string{}
	for _, item := range attempts.Items {
		states[item.ID] = item.State
	}
	if states[blocked] != "blocked" || states[succeeded] != "succeeded" || states[waiting] != "waiting" {
		t.Fatalf("attempt history = %+v", states)
	}
}

func runVerificationUnsupportedOutcome(t *testing.T, x Fixture) {
	v := newUngrantedVerificationFixture(t, x)
	coverage := verificationFixtureCoverage(v.snapshot(t))
	before := verificationPublicState(t, &v)
	cases := map[string]*store.VerificationSubmission{
		"blocked":            {Outcome: "blocked", Coverage: coverage, Feedback: "missing grant", RequiredAction: "grant"},
		"waiting":            {Outcome: "waiting", Coverage: coverage, Feedback: "missing grant", RequiredAction: "grant"},
		"failed":             {Outcome: "failed", Coverage: coverage, Feedback: "failure"},
		"empty":              {Outcome: "", Coverage: coverage},
		"no required action": {Outcome: "operator_action_required", Coverage: coverage, Feedback: "missing grant"},
		"no reason":          {Outcome: "operator_action_required", Coverage: coverage, RequiredAction: "grant"},
	}
	for name, submission := range cases {
		_, err := x.Backend.ApplyVerification(v.ctx, v.command(store.VerificationCommand{Kind: store.VerificationSeal, Submission: submission}))
		var remedy *store.VerificationRemedyError
		if !errors.Is(err, store.ErrVerificationInvalid) || !errors.As(err, &remedy) || remedy.Remedy != store.VerificationOutcomeRemedy {
			t.Fatalf("%s: %v", name, err)
		}
		want := "verification_outcome_unsupported"
		if submission.Outcome == "operator_action_required" {
			want = "verification_checkpoint_incomplete"
		}
		if remedy.Code != want {
			t.Fatalf("%s code = %s", name, remedy.Code)
		}
		if !reflect.DeepEqual(before, verificationPublicState(t, &v)) {
			t.Fatalf("%s changed the aggregate", name)
		}
	}
}

func (v *verificationFixture) snapshotFor(t *testing.T, contextID string) store.VerificationSnapshot {
	t.Helper()
	s, err := v.x.Backend.ReadVerification(v.ctx, v.access, contextID)
	requireOK(t, err)
	return s
}
