package workorder

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type verificationServiceFixture struct {
	s   *Service
	b   store.Backend
	ctx context.Context
	o   core.WorkOrder
}

func newVerificationServiceFixture(t *testing.T) verificationServiceFixture {
	t.Helper()
	b := store.NewVolatileBackend()
	t.Cleanup(b.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://github.com/org/repo", GitHub: "org/repo", Base: "main"}}}
	if _, err := b.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "verification-service", Workspace: "demo", Repo: "repo", Title: "verify", State: core.TaskRunning, NextStage: core.StageVerify, Branch: "conveyor/test", CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	task.ReviewedHeadSHA = strings.Repeat("a", 40)
	if err := b.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: strings.Repeat("a", 40), CreatedAt: task.CreatedAt}
	if _, err := storetest.CreateStageWorkOrder(ctx, b, job, order); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaim{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session", ClientToken: "token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Observed"}}}}}}}
	order, err := storetest.ClaimWorkOrder(ctx, b, order.ID, claim)
	if err != nil {
		t.Fatal(err)
	}
	ctx = store.WithActor(ctx, store.Actor{ID: "worker:fixture", Role: core.ActorWorker})
	s := &Service{Store: b, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	return verificationServiceFixture{s, b, ctx, order}
}
func (f verificationServiceFixture) call(t *testing.T, name string, input any) any {
	t.Helper()
	if start, ok := input.(VerificationStartRequest); ok && start.GrantID == "" {
		start.GrantID = storetest.GrantVerificationFixture(t, f.b, f.ctx, f.o.TaskID, f.o.ID, start.ContextID, "grant-"+start.StartKey, start.Subject, nil)
		start.EffectiveActions = []core.VerificationPermission{}
		input = start
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.s.Verification(f.ctx, f.o.ID, "session", "token", name, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}
func TestVerificationPreparationAndClaimRefusal(t *testing.T) {
	f := newVerificationServiceFixture(t)
	first := f.call(t, "prepare_verification", VerificationPrepareRequest{RequestKey: "prepare"}).(store.VerificationSnapshot)
	second := f.call(t, "prepare_verification", VerificationPrepareRequest{RequestKey: "prepare"}).(store.VerificationSnapshot)
	if len(first.Contexts) != 1 || len(first.Selections) != 1 || first.Contexts[0].ID != second.Contexts[0].ID || first.Contexts[0].GoverningPins[0].DocumentID != "req-fixture" {
		t.Fatalf("preparation not frozen: %+v", first)
	}
	for _, name := range []string{"prepare_verification", "get_verification_context"} {
		raw := []byte(`{"request_key":"denied"}`)
		if name == "get_verification_context" {
			raw, _ = json.Marshal(VerificationContextRequest{ContextID: first.Contexts[0].ID})
		}
		for _, scope := range []struct {
			ctx                   context.Context
			order, session, token string
		}{{store.WithWorkspace(f.ctx, "foreign"), f.o.ID, "session", "token"}, {f.ctx, "foreign", "session", "token"}, {f.ctx, f.o.ID, "foreign", "token"}, {f.ctx, f.o.ID, "session", "foreign"}, {store.WithActor(f.ctx, store.Actor{ID: "worker:foreign", Role: core.ActorWorker}), f.o.ID, "session", "token"}} {
			if _, err := f.s.Verification(scope.ctx, scope.order, scope.session, scope.token, name, raw); !errors.Is(err, store.ErrVerificationAccess) {
				t.Fatalf("%s foreign scope: %v", name, err)
			}
		}
	}
	for _, raw := range []string{`{"request_key":"one","request_key":"two"}`, `{"request_key":"one","governing_pins":[]}`, `{}`, `{"request_key":42}`} {
		if _, err := f.s.Verification(f.ctx, f.o.ID, "session", "token", "prepare_verification", []byte(raw)); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("invalid input %s: %v", raw, err)
		}
	}
}
func TestVerificationPrematureSuccessRetainsEvidence(t *testing.T) {
	f := newVerificationServiceFixture(t)
	snapshot := f.call(t, "prepare_verification", VerificationPrepareRequest{RequestKey: "prepare"}).(store.VerificationSnapshot)
	vc := snapshot.Contexts[0]
	contract := verification.Exercise{ID: "ordinary", Kind: "script", Argv: []string{"check"}, TimeoutSeconds: 30, RequiredAssertions: []string{"observed"}, RetryPolicy: "safe_to_replay", SafetyBasis: "read-only", EvidenceOutputs: []verification.EvidenceOutput{{Type: "state_observation", SchemaVersion: 1, MinimumItems: 1}}}
	receipt := f.call(t, "register_verification_obligation", VerificationObligationRequest{ContextID: vc.ID, ObligationID: "ordinary", Description: "Observe state", Sources: []VerificationSource{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: contract}).(store.VerificationReceipt)
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: receipt.Digest}
	environment := core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}
	start := VerificationStartRequest{Coverage: store.VerificationCoverage{ObligationIDs: []string{"ordinary"}, Justification: "Cover the fixture source", Sources: []store.VerificationCoverageSource{{Source: store.VerificationCoverageReference{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}, Disposition: "covered", Explanation: "Exercise observes the requirement", Subjects: []core.VerificationSubject{subject}}}}, ContextID: vc.ID, StartKey: "start", Subject: subject, Environment: environment, SafeInputs: map[string]json.RawMessage{}}
	run := f.call(t, "start_verification_attempt", start).(store.VerificationReceipt)
	if again := f.call(t, "start_verification_attempt", start).(store.VerificationReceipt); again.ID != run.ID {
		t.Fatal("start replay changed identity")
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	base := core.VerificationEvidence{SchemaVersion: 1, SubmissionKey: "evidence", CapturedAt: at, ReceivedAt: at, SubmittedBy: "worker:fixture", CapturedBy: core.VerificationCaptureActor{Identity: "fixture", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: "demo", TaskID: f.o.TaskID, WorkOrderID: f.o.ID, WorkOrderAttemptID: f.o.AttemptID, ContextID: vc.ID, RunID: run.ID, Subject: subject, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins, SafeInputs: map[string]json.RawMessage{}, Environment: environment}
	observation := base
	observation.ID = "observation"
	observation.Type = "state_observation"
	observation.Payload = core.JSONPayload(core.StateObservationPayload{Target: "fixture", Method: "read", CapturedAt: at, Value: core.JSONPayload("observed")})
	zero := 0
	no := false
	report := base
	report.ID = "report"
	report.Type = "execution_report"
	report.Payload = core.JSONPayload(core.ExecutionReportPayload{Argv: []string{"check"}, Tool: "check", ToolVersion: "1", Runtime: "fixture", StartedAt: at, EndedAt: at, ExitCode: &zero, TimedOut: &no, Cancelled: &no, StdoutSHA256: strings.Repeat("a", 64), StderrSHA256: strings.Repeat("b", 64), StdoutTruncated: &no, StderrTruncated: &no})
	f.call(t, "submit_verification_evidence", VerificationEvidenceRequest{ContextID: vc.ID, RunID: run.ID, SubmissionKey: "evidence", Evidence: []json.RawMessage{core.JSONPayload(observation), core.JSONPayload(report)}})
	outcome := VerificationOutcomeRequest{ContextID: vc.ID, RunID: run.ID, State: "succeeded", ExitCode: &zero}
	raw, _ := json.Marshal(outcome)
	if _, err := f.s.Verification(f.ctx, f.o.ID, "session", "token", "report_verification_outcome", raw); !errors.Is(err, store.ErrVerificationState) {
		t.Fatalf("premature success %v", err)
	}
	snapshot = f.call(t, "get_verification_context", VerificationContextRequest{ContextID: vc.ID}).(store.VerificationSnapshot)
	if len(snapshot.Evidence) != 2 || snapshot.Attempts[0].State != "running" {
		t.Fatal("refusal discarded evidence or terminated attempt")
	}
	assertion := base
	assertion.ID = "assertion"
	assertion.SubmissionKey = "assertions"
	assertion.Type = "assertion_result"
	assertion.Payload = core.JSONPayload(core.AssertionResultPayload{AssertionID: "observed", Text: "Observed state", Expected: "observed", Actual: "observed", Outcome: "pass", Supporting: []core.VerificationReference{{EvidenceID: "observation"}}})
	f.call(t, "submit_verification_evidence", VerificationEvidenceRequest{ContextID: vc.ID, RunID: run.ID, SubmissionKey: "assertions", Evidence: []json.RawMessage{core.JSONPayload(assertion)}})
	f.call(t, "report_verification_outcome", outcome)
	if terminal := f.call(t, "report_verification_outcome", outcome).(store.VerificationReceipt); terminal.State != "succeeded" {
		t.Fatal("terminal retry changed state")
	}
}

func TestVerificationMemberAndSealedReviewReads(t *testing.T) {
	f := newVerificationServiceFixture(t)
	snapshot := f.call(t, "prepare_verification", VerificationPrepareRequest{RequestKey: "prepare"}).(store.VerificationSnapshot)
	vc := snapshot.Contexts[0]
	if _, err := f.b.BootstrapIdentity(f.ctx, config.FirstOperatorIdentity{OrganizationName: "Fixture", Email: "owner@example.test", DisplayName: "Owner"}, "read-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := f.b.VerifyPersonalAccessToken(f.ctx, "read-token")
	if err != nil {
		t.Fatal(err)
	}
	userctx := store.WithActor(f.ctx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	raw := core.JSONPayload(VerificationContextRequest{ContextID: vc.ID})
	for _, name := range []string{"get_verification_context", "get_verification_publication"} {
		if _, err = f.s.Verification(userctx, f.o.ID, "", "", name, raw); err != nil {
			t.Fatalf("member %s: %v", name, err)
		}
		foreign := store.WithActor(userctx, store.Actor{ID: "user:foreign", Role: core.ActorUser})
		if _, err = f.s.Verification(foreign, f.o.ID, "", "", name, raw); !errors.Is(err, store.ErrVerificationAccess) {
			t.Fatalf("foreign member: %v", err)
		}
	}
	access := store.VerificationAccess{TaskID: f.o.TaskID, WorkOrderID: f.o.ID, WorkOrderAttemptID: f.o.AttemptID, ClientToken: "token", Claim: core.WorkOrderClaimIdentity{WorkerID: "fixture", ClaimantID: "fixture", SessionID: "session"}}
	vc = storetest.SealEmptyVerificationFixture(t, f.ctx, f.b, access, vc, "AC-1.1")
	raw = core.JSONPayload(VerificationContextRequest{ContextID: vc.ID})
	job := core.Job{ID: f.o.TaskID + "-review-1", TaskID: f.o.TaskID, Stage: core.StageReview, State: core.JobPending}
	review := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: job.TaskID, Stage: core.StageReview, State: core.WorkOrderQueued, HeadSHA: f.o.HeadSHA, CreatedAt: time.Now().UTC()}
	review.ReviewRound, review.ReviewSeat = 1, 1
	if err = storetest.CreateReviewRound(f.ctx, f.b, f.o.TaskID, []core.Job{job}, []core.WorkOrder{review}); err != nil {
		t.Fatal(err)
	}
	review, err = storetest.ClaimWorkOrder(f.ctx, f.b, review.ID, core.WorkOrderClaim{WorkerID: "reviewer", ClaimantID: "reviewer", SessionID: "review-session", ClientToken: "review-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	reviewctx := store.WithActor(f.ctx, store.Actor{ID: "worker:reviewer", Role: core.ActorWorker})
	for _, name := range []string{"get_verification_context", "get_verification_publication"} {
		if _, err = f.s.Verification(reviewctx, review.ID, "review-session", "review-token", name, raw); err != nil {
			t.Fatalf("sealed review %s: %v", name, err)
		}
	}
	if _, err = f.s.Verification(reviewctx, review.ID, "review-session", "review-token", "prepare_verification", core.JSONPayload(VerificationPrepareRequest{RequestKey: "review-write"})); !errors.Is(err, store.ErrVerificationAccess) {
		t.Fatalf("review write: %v", err)
	}
}
