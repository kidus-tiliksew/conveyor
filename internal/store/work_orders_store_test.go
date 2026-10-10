package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func eventAttemptID(t *testing.T, events []core.Event, kind, jobID string) string {
	t.Helper()
	for _, event := range events {
		if event.Kind != kind || event.JobID != jobID {
			continue
		}
		var payload struct {
			AttemptID string `json:"attempt_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", kind, err)
		}
		return payload.AttemptID
	}
	t.Fatalf("%s event for job %s not found", kind, jobID)
	return ""
}

func eventAttemptIDs(t *testing.T, events []core.Event, kind, jobID string) []string {
	t.Helper()
	var ids []string
	for _, event := range events {
		if event.Kind != kind || event.JobID != jobID {
			continue
		}
		var payload struct {
			AttemptID string `json:"attempt_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", kind, err)
		}
		ids = append(ids, payload.AttemptID)
	}
	return ids
}

func TestMemoryWorkerHeartbeatUpdatesLivenessWithoutEvent(t *testing.T) {
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	worker := core.Worker{
		ID:             "worker-heartbeat",
		Workspace:      "demo",
		OwnerUserID:    "usr-heartbeat",
		Name:           "heartbeat",
		CredentialHash: "credential-heartbeat",
		CreatedAt:      now,
	}
	if err := SetMemoryWorkspaceMember(st, "demo", worker.OwnerUserID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	firstLease := now.Add(time.Minute)
	firstProbes := []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}}
	first, err := st.HeartbeatWorker(ctx, worker.ID, firstLease, firstProbes)
	if err != nil {
		t.Fatal(err)
	}
	secondLease := now.Add(2 * time.Minute)
	secondProbes := []core.HarnessProbe{{Harness: "codex", Healthy: false, Message: "unavailable", CheckedAt: now.Add(time.Second)}}
	second, err := st.HeartbeatWorker(ctx, worker.ID, secondLease, secondProbes)
	if err != nil {
		t.Fatal(err)
	}
	if !first.LeaseExpiresAt.Equal(firstLease) || first.LastSeenAt.IsZero() || len(first.Probes) != 1 || !first.Probes[0].Healthy {
		t.Fatalf("first heartbeat worker = %+v", first)
	}
	if !second.LeaseExpiresAt.Equal(secondLease) || second.LastSeenAt.Before(first.LastSeenAt) || len(second.Probes) != 1 || second.Probes[0].Healthy || second.Probes[0].Message != "unavailable" {
		t.Fatalf("second heartbeat worker = %+v", second)
	}
	after, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("heartbeat event count changed from %d to %d: %+v", len(before), len(after), after)
	}
}

func TestTaskAssigneeConstrainsClaimsAndClearRestoresEligibility(t *testing.T) {
	ctx := WithActor(WithWorkspace(t.Context(), "demo"), Actor{ID: UserActorID("operator"), Role: core.ActorUser})
	st := NewMemory()
	now := time.Now().UTC()
	create := func(id string) core.WorkOrder {
		task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, CreatedAt: now}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: id + "-implement", TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id + "-implement", TaskID: id, JobID: id + "-implement", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err := storetestFor(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	assigned := create("assigned")
	if err := SetMemoryWorkspaceMember(st, "demo", "usr-alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, assigned.TaskID, "usr-alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, assigned.ID, core.WorkOrderClaim{SessionID: "bob", ClientToken: "bob", OwnerUserID: "usr-bob"}); err == nil || !strings.Contains(err.Error(), "usr-alice") {
		t.Fatalf("non-assignee claim error = %v", err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, assigned.ID, core.WorkOrderClaim{SessionID: "alice", ClientToken: "alice", OwnerUserID: "usr-alice"}); err != nil {
		t.Fatalf("assignee claim: %v", err)
	}

	cleared := create("cleared")
	if _, err := taskops.New(st).SetAssignee(ctx, cleared.TaskID, "usr-alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, cleared.TaskID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, cleared.ID, core.WorkOrderClaim{SessionID: "first-come", ClientToken: "first-come", OwnerUserID: "usr-bob"}); err != nil {
		t.Fatalf("claim after clear: %v", err)
	}

	events, err := st.ListEvents(ctx, assigned.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var assignmentEvents int
	for _, event := range events {
		if event.Kind == "task.assignee.set" {
			assignmentEvents++
			if event.ActorID != UserActorID("operator") || event.ActorRole != core.ActorUser {
				t.Fatalf("assignment actor = %s/%s", event.ActorID, event.ActorRole)
			}
		}
	}
	if assignmentEvents != 1 {
		t.Fatalf("assignment events = %d, want 1", assignmentEvents)
	}
}

func TestMemoryAttemptIdentityIsFreshAcrossSameSessionReclaims(t *testing.T) {
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "attempt-identity-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "attempt-identity-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim := core.WorkOrderClaim{SessionID: "warm-session", ClientToken: "warm-token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute}
	first, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, claim)
	if err != nil || first.AttemptID == "" {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	firstClosed, err := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: claim.SessionID, Outcome: core.WorkOrderOutcomeCancelled})
	if err != nil || firstClosed.LastAttemptID != first.AttemptID {
		t.Fatalf("first close=%+v err=%v", firstClosed, err)
	}
	if _, err = storetestFor(st).RecoverWorkOrder(ctx, job.ID, "same-session-reclaim", time.Hour); err != nil {
		t.Fatal(err)
	}
	second, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, claim)
	if err != nil || second.AttemptID == "" || second.AttemptID == first.AttemptID {
		t.Fatalf("second claim=%+v first_attempt=%q err=%v", second, first.AttemptID, err)
	}
	secondClosed, err := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: claim.SessionID, Outcome: core.WorkOrderOutcomeCancelled})
	if err != nil || secondClosed.LastAttemptID != second.AttemptID {
		t.Fatalf("second close=%+v err=%v", secondClosed, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimIDs := eventAttemptIDs(t, events, "work_order.claimed", job.ID)
	closeIDs := eventAttemptIDs(t, events, "work_order.released", job.ID)
	if len(claimIDs) != 2 || len(closeIDs) != 2 || claimIDs[0] != first.AttemptID || claimIDs[1] != second.AttemptID || closeIDs[0] != first.AttemptID || closeIDs[1] != second.AttemptID {
		t.Fatalf("attempt event groups claims=%v closes=%v", claimIDs, closeIDs)
	}
}

func TestMemoryCheckpointContextCandidatesRequireOpenPausedUnattachedTask(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	seed := func(id, reason string) {
		t.Helper()
		task := core.Task{ID: id, Workspace: "demo", Title: id, State: core.TaskRunning, CreatedAt: now}
		job := core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{
			ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued,
			LastAttemptOutcome: core.WorkOrderOutcomeReleased, LastFailureMessage: reason,
			QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("eligible", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	seed("other-release", "worker stopped")
	seed("attached", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	if err := st.AppendEvent(ctx, core.Event{TaskID: "attached", Kind: TaskContextRequirementAdded,
		Payload: core.JSONPayload(map[string]any{"id": "req-confirmed"}), At: now}); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListCheckpointContextCandidates(ctx, "req-confirmed")
	if err != nil || len(got) != 1 || got[0].ID != "eligible" {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
}

func TestMemoryReviewRequirementSnapshotSurvivesReload(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "snapshot-memory", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	snapshot := []core.ServedRequirementContext{{ID: "req-memory", Version: 2, Statements: []core.RequirementStatement{{ID: "REQ-1"}}}}
	governance := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "DESIGN-memory", Version: 3}}, Decisions: []core.Decision{}}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "snapshot-session", ClientToken: "secret", Lease: time.Minute, Requirements: snapshot, Governance: governance}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || len(reloaded.ServedRequirementSnapshot) != 1 || reloaded.ServedRequirementSnapshot[0].Version != 2 {
		t.Fatalf("reloaded=%+v err=%v", reloaded.ServedRequirementSnapshot, err)
	}
	if reloaded.GovernanceSnapshot == nil || len(reloaded.GovernanceSnapshot.Designs) != 1 || reloaded.GovernanceSnapshot.Designs[0].Version != 3 {
		t.Fatalf("reloaded governance=%+v", reloaded.GovernanceSnapshot)
	}
}

func TestMemoryCreateWorkOrderRejectsExplicitNonCreateStates(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "create-order-state", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.WorkOrderState{core.WorkOrderCompleted, core.WorkOrderCancelled, core.WorkOrderStale, "unsupported"} {
		id := job.ID + "-" + string(state)
		err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: state})
		var transitionErr *core.ErrInvalidTransition
		if !errors.As(err, &transitionErr) || transitionErr.Space != core.WorkOrderLifecycle || transitionErr.From != "" || transitionErr.Command != string(core.WorkOrderCmdCreate) {
			t.Fatalf("state %q error = %v", state, err)
		}
		if _, getErr := st.GetWorkOrder(ctx, id); getErr == nil {
			t.Fatalf("state %q was persisted", state)
		}
	}
}

func TestMemoryReviewRequeueRecordsStageAdvance(t *testing.T) {
	t.Parallel()
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "review-requeue-command", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, ReviewRound: 1, ReviewSeat: 1}); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: job.ID, ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "tests", Summary: "revise", Feedback: "fix it", MaxBounces: 3}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetTask(ctx, task.ID)
	if err != nil || persisted.State != core.TaskQueued || persisted.NextStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", persisted, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "task.state_changed" && strings.Contains(string(event.Payload), `"command":"stage.advance"`) {
			return
		}
	}
	t.Fatal("review requeue did not record stage.advance")
}

func TestMemoryAcceptedReviewClearsSubmittedImplementationContinuation(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "review-clears-continuation", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	implementJob := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, implementJob); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: implementJob.ID, TaskID: task.ID, JobID: implementJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	implement, err := storetestFor(st).ClaimWorkOrder(ctx, implementJob.ID, core.WorkOrderClaim{SessionID: "implement-session", ClientToken: "implement-token", ClaimantID: "run:implementer", WorkerID: "worker-implement", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	identity := core.WorkOrderClaimIdentity{WorkerID: implement.WorkerID, ClaimantID: implement.ClaimantID, SessionID: implement.SessionID}
	if _, err = st.RecordWorkOrderContinuation(ctx, implement.ID, identity, core.WorkOrderContinuation{SessionID: "native-session", AttemptID: implement.AttemptID, Harness: "codex", LaunchEnvironment: "worker-implement/env"}); err != nil {
		t.Fatal(err)
	}
	implement.State = core.WorkOrderSubmitted
	if err = storetestFor(st).UpdateWorkOrder(ctx, implement, core.WorkOrderCmdSubmitForReview); err != nil {
		t.Fatal(err)
	}

	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	reviewOrder := core.WorkOrder{ID: reviewJob.ID, TaskID: task.ID, JobID: reviewJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err = st.CreateJob(ctx, reviewJob); err != nil {
		t.Fatal(err)
	}
	if err = storetestFor(st).CreateWorkOrder(ctx, reviewOrder); err != nil {
		t.Fatal(err)
	}
	claimedReview, err := storetestFor(st).ClaimWorkOrder(ctx, reviewOrder.ID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "review-token", ClaimantID: "run:reviewer", WorkerID: "worker-review", Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = storetestFor(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: reviewJob.ID, ReviewWorkOrderID: reviewOrder.ID, ClaimSession: claimedReview.SessionID, ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "approved", Summary: "accepted", PolicyVersion: 1, MergeApproval: false}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetWorkOrder(ctx, implement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ContinuationSessionID != "" || persisted.ContinuationAttemptID != "" || persisted.ContinuationHarness != "" || persisted.ContinuationLaunchEnvironment != "" {
		t.Fatalf("submitted implementation continuation was not cleared: %+v", persisted)
	}
}

func TestReviewVerdictDiagnosticsDistinguishClaimedExpiredAndReleased(t *testing.T) {
	now := time.Now().UTC()
	claimed := core.WorkOrder{
		ID: "review-claimed", JobID: "job-claimed", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 1, ExecutionStartedAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(time.Minute),
	}
	expiredClaim := core.WorkOrder{
		ID: "review-expired", JobID: "job-expired", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 2, LeaseExpiresAt: now.Add(-time.Minute),
	}
	expired := expiredClaim
	expired.State, expired.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	releasedClaim := core.WorkOrder{
		ID: "review-released", JobID: "job-released", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 3, LeaseExpiresAt: now.Add(-time.Minute),
	}
	released := releasedClaim
	released.State, released.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	terminalClaim := core.WorkOrder{
		ID: "review-terminal", JobID: "job-terminal", Stage: core.StageReview, State: core.WorkOrderClaimed,
		ReviewRound: 2, ReviewSeat: 4, LeaseExpiresAt: now.Add(-time.Minute),
	}
	terminal := terminalClaim
	terminal.State, terminal.LeaseExpiresAt = core.WorkOrderQueued, time.Time{}
	events := []core.Event{
		{JobID: expired.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(expiredClaim), At: now.Add(-2 * time.Minute)},
		{JobID: released.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(releasedClaim), At: now.Add(-2 * time.Minute)},
		{JobID: released.JobID, Kind: "work_order.released", Payload: core.JSONPayload(map[string]string{"reason": "harness exited without terminal verdict submission"}), At: now.Add(-30 * time.Second)},
		{JobID: terminal.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(terminalClaim), At: now.Add(-2 * time.Minute)},
		{JobID: terminal.JobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]string{"review_work_order_id": terminal.ID}), At: now.Add(-30 * time.Second)},
	}
	diagnostics := ReviewVerdictDiagnostics([]core.WorkOrder{claimed, expired, released, terminal}, events, now)
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if diagnostics[0].WorkOrderID != expired.ID || diagnostics[0].Status != ReviewExpiredWithoutVerdict || diagnostics[0].ReviewSeat != 2 {
		t.Fatalf("expired diagnostic=%+v", diagnostics[0])
	}
	if diagnostics[1].WorkOrderID != claimed.ID || diagnostics[1].Status != ReviewClaimedWithoutVerdict || diagnostics[1].ReviewSeat != 1 {
		t.Fatalf("claimed diagnostic=%+v", diagnostics[1])
	}
}

func TestMemoryWorkOrderRejectsSelfReview(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), SystemActor())
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{{ID: "implement", TaskID: "task", Stage: core.StageImplement}, {ID: "review", TaskID: "task", Stage: core.StageReview}} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []core.WorkOrder{{ID: "implement", TaskID: "task", JobID: "implement", Stage: core.StageImplement, State: core.WorkOrderQueued}, {ID: "review", TaskID: "task", JobID: "review", Stage: core.StageReview, State: core.WorkOrderQueued}} {
		if err := storetestFor(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	claim := core.WorkOrderClaim{SessionID: "session-a", ClientToken: "token-a", Agent: "codex", Model: "gpt"}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, "implement", claim); err != nil {
		t.Fatal(err)
	}
	if _, err := storetestFor(st).ClaimWorkOrder(ctx, "review", claim); err == nil || !strings.Contains(err.Error(), "self-review forbidden") {
		t.Fatalf("self review error=%v", err)
	}
	claim.SessionID = "session-b"
	claim.ClientToken = "token-b"
	review, err := storetestFor(st).ClaimWorkOrder(ctx, "review", claim)
	if err != nil {
		t.Fatalf("fresh review claim: %v", err)
	}
	if review.ModelEnforcement != "self-reported" {
		t.Fatalf("manual review enforcement=%q", review.ModelEnforcement)
	}
}

func TestMemoryWorkOrderRequiresLinkedTaskJobAndWorkspace(t *testing.T) {
	t.Parallel()
	st := NewMemory()
	ctxA := WithWorkspace(WithActor(context.Background(), SystemActor()), "alpha")
	ctxB := WithWorkspace(WithActor(context.Background(), SystemActor()), "beta")
	for _, task := range []core.Task{{ID: "task-a", Workspace: "alpha"}, {ID: "task-b", Workspace: "beta"}} {
		ctx := ctxA
		if task.Workspace == "beta" {
			ctx = ctxB
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: "job-" + task.Workspace, TaskID: task.ID, Stage: core.StageImplement}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storetestFor(st).CreateWorkOrder(ctxB, core.WorkOrder{ID: "wrong-workspace", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageImplement}); err == nil {
		t.Fatal("cross-workspace work order succeeded")
	}
	if err := storetestFor(st).CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-task", TaskID: "task-a", JobID: "job-beta", Stage: core.StageImplement}); err == nil {
		t.Fatal("work order linked a task to another task's job")
	}
	if err := storetestFor(st).CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-stage", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageReview}); err == nil {
		t.Fatal("work order linked a job at the wrong stage")
	}
	if err := storetestFor(st).CreateWorkOrder(ctxA, core.WorkOrder{ID: "valid", TaskID: "task-a", JobID: "job-alpha", Stage: core.StageImplement}); err != nil {
		t.Fatalf("valid work order: %v", err)
	}
}

func TestMemoryTimedOutWorkOrderRejectsStaleUpdate(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), SystemActor())
	st := NewMemory()
	task := core.Task{ID: "timeout-task", State: core.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "timeout-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage}); err != nil {
		t.Fatal(err)
	}
	stale, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := stale.AttemptID
	expired := stale
	deadline := time.Now().Add(-time.Second)
	expired.ExecutionDeadline = deadline
	if err = storetestFor(st).UpdateWorkOrder(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetWorkOrder(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	stale.State = core.WorkOrderSubmitted
	if err = storetestFor(st).UpdateWorkOrder(ctx, stale); !errors.Is(err, ErrWorkOrderTimedOut) {
		t.Fatalf("stale update error = %v", err)
	}
	current, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || current.State != core.WorkOrderTimedOut {
		t.Fatalf("current = %+v, err = %v", current, err)
	}
	if attemptID == "" || current.AttemptID != "" || current.LastAttemptID != attemptID || current.SessionID != "" {
		t.Fatalf("timed-out attempt identity current=%+v want last_attempt_id=%q", current, attemptID)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil || eventAttemptID(t, events, "work_order.timed_out", job.ID) != attemptID {
		t.Fatalf("timed-out event identity err=%v want=%q", err, attemptID)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || !jobs[0].EndedAt.Equal(deadline) {
		t.Fatalf("timeout job=%+v err=%v want ended_at=%s", jobs, err, deadline)
	}
}

func TestMemoryCreateSpecVersionAlwaysStartsUnapproved(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), SystemActor())
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "spec-task", State: core.TaskAwaiting}); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: "spec-task", Content: "# Spec", Approved: true, ApprovedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Approved || !created.ApprovedAt.IsZero() {
		t.Fatalf("created spec bypassed approval gate: %+v", created)
	}
}

func TestMemoryWorkerFailureBackoffSuppressionAndRecovery(t *testing.T) {
	ctx := WithWorkspace(WithActor(context.Background(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	task := core.Task{ID: "retry-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "retry-job", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	workerID := "worker-1"
	policy := core.WorkOrderRelease{Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited: status 1", FailureCategory: core.WorkOrderFailureProviderUsageLimit, InitialRetryDelay: time.Second, MaximumRetryDelay: 4 * time.Second, AutomaticRetryLimit: 3}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	var priorStart time.Time
	for attempt := 0; attempt < 4; attempt++ {
		sessionID := fmt.Sprintf("session-%d", attempt)
		claimed, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: sessionID, ClientToken: fmt.Sprintf("token-%d", attempt), ClaimantID: workerID, WorkerID: workerID, Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		if claimed.AttemptID == "" {
			t.Fatalf("claim %d has empty attempt id", attempt)
		}
		if !priorStart.IsZero() && !claimed.ExecutionStartedAt.After(priorStart) {
			t.Fatalf("attempt %d reused execution start %v", attempt, claimed.ExecutionStartedAt)
		}
		priorStart = claimed.ExecutionStartedAt
		if attempt == 1 {
			if _, staleErr := storetestFor(st).RenewWorkerClaim(ctx, job.ID, workerID, "session-0", time.Minute); !errors.Is(staleErr, ErrWorkOrderClaimLost) {
				t.Fatalf("stale renew error=%v", staleErr)
			}
			if _, staleErr := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, workerID, core.WorkOrderRelease{SessionID: "session-0", Outcome: core.WorkOrderOutcomeCancelled}); !errors.Is(staleErr, ErrWorkOrderClaimLost) {
				t.Fatalf("stale release error=%v", staleErr)
			}
		}
		exit := 1
		policy.SessionID = sessionID
		policy.ExitStatus = &exit
		released, err := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, workerID, policy)
		if err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		if released.WorkerID != "" || !released.ExecutionStartedAt.IsZero() || !released.ExecutionDeadline.IsZero() {
			t.Fatalf("release %d retained active attempt: %+v", attempt, released)
		}
		if released.AttemptID != "" || released.LastAttemptID != claimed.AttemptID || released.LastFailureCategory != core.WorkOrderFailureProviderUsageLimit {
			t.Fatalf("release %d attempt projection: %+v", attempt, released)
		}
		if attempt < len(wantDelays) {
			if released.RetrySuppressed || released.AutomaticRetryCount != attempt+1 || released.NextRetryAt.Sub(released.LastFailureAt) != wantDelays[attempt] {
				t.Fatalf("release %d retry state: %+v", attempt, released)
			}
			if _, claimErr := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "too-soon", ClientToken: "too-soon"}); claimErr == nil || !strings.Contains(claimErr.Error(), "backoff") {
				t.Fatalf("release %d immediate claim error=%v", attempt, claimErr)
			}
			released.NextRetryAt = time.Now().Add(-time.Millisecond)
			if err = storetestFor(st).UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		} else if !released.RetrySuppressed || !released.NextRetryAt.IsZero() || released.AutomaticRetryCount != 3 {
			t.Fatalf("final suppression: %+v", released)
		}
	}
	if _, err := storetestFor(st).RecoverWorkOrder(WithWorkspace(ctx, "other"), job.ID, "recover-1", time.Hour); err == nil {
		t.Fatal("cross-workspace recovery succeeded")
	}
	recovered, err := storetestFor(st).RecoverWorkOrder(ctx, job.ID, "recover-1", time.Hour)
	if err != nil || !recovered.Claimable || recovered.RetrySuppressed || recovered.AutomaticRetryCount != 0 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	duplicate, err := storetestFor(st).RecoverWorkOrder(ctx, job.ID, "recover-1", time.Hour)
	if err != nil || duplicate.RedispatchCount != recovered.RedispatchCount {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	events, _ := st.CountEvents(ctx, task.ID, "work_order.redispatched")
	if events != 1 {
		t.Fatalf("redispatch events=%d", events)
	}
}

func TestMemoryTransientConnectivityBackoffResetAndObservability(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), Actor{ID: "operator", Role: core.ActorHuman}), "demo")
	st := NewMemory()
	task := core.Task{ID: "connectivity-retry-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "connectivity-retry-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}

	workerID := "connectivity-worker"
	release := func(attempt int, category string, progress bool) core.WorkOrder {
		t.Helper()
		sessionID := fmt.Sprintf("connectivity-session-%d", attempt)
		_, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: sessionID, ClientToken: sessionID, WorkerID: workerID, Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		if progress {
			if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "work_order.progress_reported", Payload: core.JSONPayload(map[string]any{"message": "made progress"}), At: time.Now().UTC()}); err != nil {
				t.Fatalf("progress %d: %v", attempt, err)
			}
		}
		result, err := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, workerID, core.WorkOrderRelease{
			SessionID: sessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited",
			FailureCategory: category, FailureDetail: fmt.Sprintf("failure-%d", attempt), AutomaticRetryLimit: 10,
			InitialRetryDelay: time.Second, MaximumRetryDelay: 4 * time.Second,
		})
		if err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		return result
	}
	advance := func(order core.WorkOrder) {
		t.Helper()
		order.NextRetryAt = time.Now().Add(-time.Millisecond)
		if err := storetestFor(st).UpdateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}

	for index, want := range []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute, 8 * time.Minute} {
		result := release(index+1, core.WorkOrderFailureTransientConnectivity, false)
		if got := result.NextRetryAt.Sub(result.LastFailureAt); got != want {
			t.Fatalf("transient attempt %d delay=%s want=%s", index+1, got, want)
		}
		wantCount := index + 1
		if !strings.Contains(result.LastFailureDetail, fmt.Sprintf("consecutive_transient_failures=%d", wantCount)) || !strings.Contains(result.LastFailureDetail, "next_attempt_at=") {
			t.Fatalf("transient attempt %d detail=%q", index+1, result.LastFailureDetail)
		}
		advance(result)
	}

	nonTransient := release(5, "contract_violation", false)
	if got := nonTransient.NextRetryAt.Sub(nonTransient.LastFailureAt); got != 4*time.Second {
		t.Fatalf("non-transient delay=%s want=%s", got, 4*time.Second)
	}
	advance(nonTransient)
	afterCategoryBreak := release(6, core.WorkOrderFailureTransientConnectivity, false)
	if got := afterCategoryBreak.NextRetryAt.Sub(afterCategoryBreak.LastFailureAt); got != 30*time.Second {
		t.Fatalf("category-break delay=%s want=30s", got)
	}
	advance(afterCategoryBreak)
	afterProgress := release(7, core.WorkOrderFailureTransientConnectivity, true)
	if got := afterProgress.NextRetryAt.Sub(afterProgress.LastFailureAt); got != 30*time.Second {
		t.Fatalf("progress-reset delay=%s want=30s", got)
	}
	advance(afterProgress)
	var exhausted core.WorkOrder
	for attempt := 8; attempt <= 11; attempt++ {
		exhausted = release(attempt, core.WorkOrderFailureTransientConnectivity, false)
		if attempt < 11 {
			advance(exhausted)
		}
	}
	if !exhausted.RetrySuppressed || exhausted.AutomaticRetryCount != 10 || !exhausted.NextRetryAt.IsZero() || !strings.Contains(exhausted.LastFailureDetail, "next_attempt_at=none") {
		t.Fatalf("transient exhaustion=%+v", exhausted)
	}
	if _, err := storetestFor(st).RecoverWorkOrder(ctx, job.ID, "transient-recovery", time.Hour); err != nil {
		t.Fatal(err)
	}

	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	var redispatchPayload map[string]any
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case "work_order.redispatched":
			if redispatchPayload == nil {
				if err = json.Unmarshal(events[i].Payload, &redispatchPayload); err != nil {
					t.Fatal(err)
				}
			}
		case "work_order.child_failed":
			if payload != nil {
				continue
			}
			if err = json.Unmarshal(events[i].Payload, &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if payload["failure_category"] != core.WorkOrderFailureTransientConnectivity || payload["consecutive_transient_failures"] != float64(5) || payload["next_retry_at"] == nil {
		t.Fatalf("failure payload=%v", payload)
	}
	if redispatchPayload["failure_category"] != core.WorkOrderFailureTransientConnectivity || redispatchPayload["consecutive_transient_failures"] != float64(5) || redispatchPayload["next_retry_at"] == nil {
		t.Fatalf("redispatch payload=%v", redispatchPayload)
	}
}

func TestMemoryStalledOutcomeConsumesRetryAndReachesNeedsOperator(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "stalled-retry-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	job := core.Job{ID: "stalled-retry-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	var released core.WorkOrder
	for attempt := 0; attempt < 4; attempt++ {
		session := fmt.Sprintf("stall-session-%d", attempt)
		if _, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute}); err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		var err error
		released, err = storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{
			SessionID: session, Outcome: core.WorkOrderOutcomeStalled, Reason: "no child output",
			AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond, MaximumRetryDelay: 4 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		if attempt < 3 {
			if released.RetrySuppressed || released.AutomaticRetryCount != attempt+1 {
				t.Fatalf("retry %d state=%+v", attempt, released)
			}
			released.NextRetryAt = time.Now().Add(-time.Millisecond)
			if err = storetestFor(st).UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !released.RetrySuppressed || released.LastAttemptOutcome != core.WorkOrderOutcomeStalled || released.AutomaticRetryCount != 3 {
		t.Fatalf("stalled exhaustion=%+v", released)
	}
	if stalled := StalledTask([]core.WorkOrder{released}); stalled == nil || !strings.Contains(stalled.Reason, "retry") {
		t.Fatalf("needs-operator projection=%+v", stalled)
	}
	if events, err := st.CountEvents(ctx, task.ID, "work_order.stalled"); err != nil || events != 4 {
		t.Fatalf("stalled events=%d err=%v", events, err)
	}
}

func TestMemoryFirstActivityTimeoutUsesExistingRetryAuditAndStallEvidence(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "first-activity-timeout-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "first-activity-timeout-order", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	const reason = "harness produced no output before first_activity_timeout"
	var released core.WorkOrder
	for attempt := 1; attempt <= 2; attempt++ {
		session := fmt.Sprintf("first-activity-timeout-%d", attempt)
		if _, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		var err error
		released, err = storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{
			SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: reason,
			AutomaticRetryLimit: 1, InitialRetryDelay: time.Millisecond, MaximumRetryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 1 {
			if released.RetrySuppressed || released.NextRetryAt.IsZero() {
				t.Fatalf("first timeout did not enter bounded retry: %+v", released)
			}
			released.NextRetryAt = now.Add(-time.Second)
			if err = storetestFor(st).UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !released.RetrySuppressed || released.LastFailureMessage != reason || released.State != core.WorkOrderQueued {
		t.Fatalf("suppressed timeout=%+v", released)
	}
	stalled := StalledTask([]core.WorkOrder{released})
	if stalled == nil || stalled.LastFailure != reason || stalled.WorkOrder.ID != released.ID {
		t.Fatalf("stalled evidence=%+v", stalled)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	childFailures := 0
	for _, event := range events {
		if event.Kind != "work_order.child_failed" {
			continue
		}
		childFailures++
		if !strings.Contains(string(event.Payload), reason) {
			t.Fatalf("timeout event omitted stable reason: %s", event.Payload)
		}
	}
	if childFailures != 2 || !strings.Contains(string(events[len(events)-1].Payload), `"retry_suppressed":true`) {
		t.Fatalf("child failure events=%d final=%s", childFailures, events[len(events)-1].Payload)
	}
}

func TestMemorySuppressesSecondIdenticalNonEmptyChildFailure(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "identical-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "identical-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		session := fmt.Sprintf("identical-%d", attempt)
		if _, err := storetestFor(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		released, err := storetestFor(st).ReleaseWorkerClaim(ctx, job.ID, "worker", core.WorkOrderRelease{SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "exit status 1", FailureDetail: "  provider rejected model  ", AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 1 {
			if released.RetrySuppressed || released.LastFailureDetail != "provider rejected model" {
				t.Fatalf("first failure=%+v", released)
			}
			released.NextRetryAt = now.Add(-time.Second)
			if err = storetestFor(st).UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !released.RetrySuppressed || !released.NextRetryAt.IsZero() || released.AutomaticRetryCount != 2 || released.RetrySuppressionReason != core.IdenticalFailureSuppressionReason {
			t.Fatalf("second failure=%+v", released)
		}
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if payload := string(events[len(events)-1].Payload); !strings.Contains(payload, `"detail":"provider rejected model"`) || !strings.Contains(payload, core.IdenticalFailureSuppressionReason) {
		t.Fatalf("event payload=%s", payload)
	}

	differentJob := core.Job{ID: "different-order", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, differentJob); err != nil {
		t.Fatal(err)
	}
	if err = storetestFor(st).CreateWorkOrder(ctx, core.WorkOrder{ID: differentJob.ID, TaskID: task.ID, JobID: differentJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	for attempt, detail := range []string{"first output", "different output"} {
		session := fmt.Sprintf("different-%d", attempt)
		if _, err = storetestFor(st).ClaimWorkOrder(ctx, differentJob.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, WorkerID: "worker", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		var released core.WorkOrder
		released, err = storetestFor(st).ReleaseWorkerClaim(ctx, differentJob.ID, "worker", core.WorkOrderRelease{SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, FailureDetail: detail, AutomaticRetryLimit: 3, InitialRetryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if released.RetrySuppressed || released.NextRetryAt.IsZero() {
			t.Fatalf("different failure %d=%+v", attempt, released)
		}
		if attempt == 0 {
			released.NextRetryAt = time.Now().Add(-time.Second)
			if err = storetestFor(st).UpdateWorkOrder(ctx, released); err != nil {
				t.Fatal(err)
			}
		}
	}
}
