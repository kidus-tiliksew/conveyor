package workorder

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestReleaseCheckpointDecisionValidationAndCitations(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	service := &Service{Store: st}
	now := time.Now().UTC()
	task := core.Task{ID: "checkpoint-metadata", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-owner"), SessionID: "checkpoint-session"}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Checkpoints name the decision.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "The request is distinct."}}}}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-checkpoint", Title: "Checkpoint"}, core.RequirementVersion{Content: "# Checkpoint authority.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Checkpoints name the decision.\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: The request is distinct.\n```", Statements: statements, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}

	release := core.WorkOrderRelease{SessionID: claim.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}
	if _, err = service.ReleaseClaim(ctx, claim, job.ID, release); err == nil || !strings.Contains(err.Error(), "checkpoint.decision_request is required") {
		t.Fatalf("missing decision request error=%v", err)
	}
	if order, getErr := st.GetWorkOrder(ctx, job.ID); getErr != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("missing request mutated order=%+v err=%v", order, getErr)
	}
	release.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Resolve the cited conflict.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: "missing-document", CitedVersion: 1}}}
	if _, err = service.ReleaseClaim(ctx, claim, job.ID, release); err == nil || !strings.Contains(err.Error(), "does not exist in this workspace") {
		t.Fatalf("unknown document error=%v", err)
	}
	if order, getErr := st.GetWorkOrder(ctx, job.ID); getErr != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("unknown document mutated order=%+v err=%v", order, getErr)
	}
	release.Checkpoint.Citations = []core.WorkOrderAuthorityConflictCitation{
		{DocumentID: requirement.ID, CitedVersion: version.Version, StatementOrSectionID: "AC-1.1"},
		{DocumentID: requirement.ID, CitedVersion: version.Version, StatementOrSectionID: "AC-9.9"},
	}
	released, err := service.ReleaseClaim(ctx, claim, job.ID, release)
	if err != nil {
		t.Fatal(err)
	}
	if released.Checkpoint == nil || released.Checkpoint.DecisionRequest != "Resolve the cited conflict." || len(released.Checkpoint.Citations) != 2 || released.Checkpoint.Citations[0].StatementOrSectionID != "AC-1.1" || released.Checkpoint.Citations[1].StatementOrSectionID != "" {
		t.Fatalf("normalized checkpoint=%+v", released.Checkpoint)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "work_order.released" && strings.Contains(string(event.Payload), `"decision_request":"Resolve the cited conflict."`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("release event omitted checkpoint: %+v", events)
	}
}

// A System Design citation that names a code-formatted heading keeps its
// clause, with or without the backticks or as the heading's anchor; an
// unknown clause is still dropped to a document-level citation, and an
// over-limit decision request names the rune limit (component-mcp-protocol,
// Checkpoint decision requests on release; req-260820-394cac REQ-1, AC-2.1).
func TestReleaseCheckpointKeepsCodeFormattedHeadingCitations(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	service := &Service{Store: st}
	now := time.Now().UTC()
	task := core.Task{ID: "checkpoint-code-heading", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-owner"), SessionID: "code-heading-session"}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-installer", Title: "Installer", Category: "Component design"}, core.SystemDesignVersion{
		Content: "# Installer\n\n## `agentdesk context`\n\nPrints guidance.\n\n## Skill file\n\nShips the skill.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - packages/installer/**\n```\n", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}

	release := core.WorkOrderRelease{SessionID: claim.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: strings.Repeat("é", core.WorkOrderDecisionRequestLimit+1)}}
	if _, err = service.ReleaseClaim(ctx, claim, job.ID, release); err == nil || err.Error() != "checkpoint.decision_request exceeds 1024 Unicode characters (runes)" {
		t.Fatalf("over-limit decision request error=%v", err)
	}
	release.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: strings.Repeat("é", core.WorkOrderDecisionRequestLimit), Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "agentdesk context"},
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "`agentdesk context`"},
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "agentdesk-context"},
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "skill file"},
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "Standing guidance"},
		{DocumentID: design.ID, CitedVersion: version.Version, StatementOrSectionID: "``"},
	}}
	released, err := service.ReleaseClaim(ctx, claim, job.ID, release)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agentdesk context", "`agentdesk context`", "agentdesk-context", "skill file", "", ""}
	if released.Checkpoint == nil || len(released.Checkpoint.Citations) != len(want) {
		t.Fatalf("checkpoint=%+v", released.Checkpoint)
	}
	for i, clause := range want {
		if got := released.Checkpoint.Citations[i].StatementOrSectionID; got != clause {
			t.Fatalf("citation %d clause=%q, want %q", i, got, clause)
		}
	}
}

func TestTransientConnectivityFailureClassification(t *testing.T) {
	for _, detail := range []string{
		"failed to connect to websocket wss://example.invalid/responses",
		"dial tcp: lookup example.invalid: nodename nor servname provided",
		"temporary failure in name resolution",
		"read: connection reset by peer",
	} {
		if !transientConnectivityFailure(detail) {
			t.Fatalf("detail was not classified as transient connectivity: %q", detail)
		}
	}
	for _, detail := range []string{"401 unauthorized", "validation failed", "provider usage limit reached"} {
		if transientConnectivityFailure(detail) {
			t.Fatalf("detail was incorrectly classified as transient connectivity: %q", detail)
		}
	}
}

func TestFailureDetailBound(t *testing.T) {
	detail := strings.Repeat("x", failureDetailLimit+100) + " unsupported model "
	bounded := boundedFailureDetail(detail)
	if len(bounded) > failureDetailLimit || !strings.HasSuffix(bounded, "unsupported model") {
		t.Fatalf("bounded detail length=%d suffix=%q", len(bounded), bounded[len(bounded)-20:])
	}
}

func TestProviderUsageLimitMatcher(t *testing.T) {
	for _, fixture := range []struct {
		detail string
		want   bool
	}{
		{"You have reached your usage limit. Try again later.", true},
		{"HTTP 429: too many requests", true},
		{"provider quota has been exceeded", true},
		{"capacity exhausted for this account", true},
		{"harness exited before completing work order", false},
		{"the configured model is unsupported", false},
	} {
		if got := providerUsageLimit(fixture.detail); got != fixture.want {
			t.Fatalf("providerUsageLimit(%q)=%v want %v", fixture.detail, got, fixture.want)
		}
	}
}

// TestRenewClaimWithLeaseCapsAtExecutionDeadline proves the session's chosen
// lease is honored and never passes the fixed execution deadline
// (req-agent-skills AC-2.2; req-delegated-execution AC-1.1).
func TestRenewClaimWithLeaseCapsAtExecutionDeadline(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	service := &Service{Store: st}
	now := time.Now().UTC()
	task := core.Task{ID: "renew-lease", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-owner"), SessionID: "renew-session"}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.RenewClaimWithLease(ctx, core.WorkOrderClaimIdentity{ClaimantID: claim.ClaimantID}, job.ID, time.Minute); err == nil {
		t.Fatal("renewal without a session was accepted")
	}
	renewed, err := service.RenewClaimWithLease(ctx, claim, job.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.LeaseExpiresAt.After(claimed.LeaseExpiresAt.Add(20*time.Minute)) || renewed.LeaseExpiresAt.After(claimed.ExecutionDeadline) {
		t.Fatalf("renewed lease=%s claimed lease=%s deadline=%s", renewed.LeaseExpiresAt, claimed.LeaseExpiresAt, claimed.ExecutionDeadline)
	}
	capped, err := service.RenewClaimWithLease(ctx, claim, job.ID, 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if capped.LeaseExpiresAt.After(claimed.ExecutionDeadline) || !capped.ExecutionDeadline.Equal(claimed.ExecutionDeadline) {
		t.Fatalf("lease %s passed deadline %s", capped.LeaseExpiresAt, capped.ExecutionDeadline)
	}
	if _, err = service.RenewClaimWithLease(ctx, core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-other"), SessionID: claim.SessionID}, job.ID, time.Minute); err == nil {
		t.Fatal("another claimant renewed the claim")
	}
}

// TestReleaseClaimValidatesSessionOutcomeAndCause proves release refuses a
// missing or mismatched session and an unknown outcome or cause before any
// mutation.
func TestReleaseClaimValidatesSessionOutcomeAndCause(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	service := &Service{Store: st}
	now := time.Now().UTC()
	task := core.Task{ID: "release-validation", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-owner"), SessionID: "release-session"}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	for name, release := range map[string]core.WorkOrderRelease{
		"missing session":         {Reason: "done", Outcome: core.WorkOrderOutcomeReleased},
		"other session":           {SessionID: "other-session", Reason: "done", Outcome: core.WorkOrderOutcomeReleased},
		"unknown outcome":         {SessionID: claim.SessionID, Reason: "done", Outcome: "vanished"},
		"unknown cause":           {SessionID: claim.SessionID, Reason: "done", Cause: "meteor", Outcome: core.WorkOrderOutcomeReleased},
		"class without citations": {SessionID: claim.SessionID, Reason: "done", Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Decide.", Class: core.WorkOrderCheckpointClassAuthorityConflict}},
	} {
		if _, err := service.ReleaseClaim(ctx, claim, job.ID, release); err == nil {
			t.Fatalf("%s: release accepted", name)
		}
		if order, err := st.GetWorkOrder(ctx, job.ID); err != nil || order.State != core.WorkOrderClaimed {
			t.Fatalf("%s: refused release mutated order=%+v err=%v", name, order, err)
		}
	}
	released, err := service.ReleaseClaim(ctx, claim, job.ID, core.WorkOrderRelease{SessionID: claim.SessionID, Reason: "operator asked", Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	if err != nil {
		t.Fatal(err)
	}
	if released.State != core.WorkOrderQueued || released.SessionID != "" || released.WorkerID != "" {
		t.Fatalf("released order=%+v", released)
	}
}

// TestRequestPlanRevisionClaimRequiresSessionAndRationale proves the request
// refuses a blank session or rationale before any mutation.
func TestRequestPlanRevisionClaimRequiresSessionAndRationale(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	service := &Service{Store: store.NewMemory()}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-owner")}
	if _, err := service.RequestPlanRevisionClaim(ctx, claim, "missing-order", "Plan is wrong."); err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("blank session error=%v", err)
	}
	claim.SessionID = "plan-session"
	if _, err := service.RequestPlanRevisionClaim(ctx, claim, "missing-order", "  "); err == nil || !strings.Contains(err.Error(), "rationale is required") {
		t.Fatalf("blank rationale error=%v", err)
	}
}
