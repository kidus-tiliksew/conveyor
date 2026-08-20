package worker

import (
	"context"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type failingObservabilityStore struct{ store.Store }

func (failingObservabilityStore) UpsertWorkOrderActivitySnapshot(context.Context, string, core.WorkOrderClaimIdentity, string) error {
	return errors.New("snapshot unavailable")
}

func (failingObservabilityStore) FinalizeWorkOrderAttemptObservability(context.Context, string, string, core.WorkOrderAttemptCheckpoint) error {
	return errors.New("transcript unavailable")
}

func TestPairingHeartbeatHealthAndWorkerClaimLifecycle(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "operator-token", OwnerUserID: "usr-assigned", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithWorkspace(store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID("usr-assigned"), Role: core.ActorUser}), "demo")
	token, pairing, err := service.IssuePairing(operatorCtx, time.Minute)
	if err != nil || token == "" || pairing.TokenHash == token {
		t.Fatalf("pairing=%+v token=%q err=%v", pairing, token, err)
	}
	enrollment, err := service.Enroll(t.Context(), token, "laptop")
	if err != nil || enrollment.Credential == "" || enrollment.Worker.CredentialHash == enrollment.Credential || enrollment.Worker.OwnerUserID != "usr-assigned" {
		t.Fatalf("enrollment=%+v err=%v", enrollment, err)
	}
	if _, err = service.Enroll(t.Context(), token, "again"); !errors.Is(err, store.ErrPairingInvalid) {
		t.Fatalf("pairing reuse err=%v", err)
	}
	if _, _, err = service.Authenticate(t.Context(), enrollment.Credential, "other"); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("cross-workspace auth err=%v", err)
	}
	workerCtx, worker, err := service.Authenticate(t.Context(), enrollment.Credential, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if worker.OwnerUserID != "usr-assigned" {
		t.Fatalf("authenticated worker owner=%q", worker.OwnerUserID)
	}
	worker, err = service.Heartbeat(workerCtx, worker, []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}})
	if err != nil || !worker.Live(now) {
		t.Fatalf("worker=%+v err=%v", worker, err)
	}
	if status := service.Serviceability(workerCtx, cfg); !status.Available {
		t.Fatalf("worker unavailable: %s", status.Reason)
	}

	createOrder := func(taskID string, hold bool) {
		task := core.Task{ID: taskID, Workspace: "demo", Hold: hold, SpecApproval: true, MergeApproval: true, PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
		if err := st.CreateTask(workerCtx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(workerCtx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(workerCtx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	createOrder("auto-task", false)
	createOrder("manual-task", true)
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-assigned", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(operatorCtx, "auto-task", "usr-assigned"); err != nil {
		t.Fatal(err)
	}
	listed, err := service.ListClaimable(workerCtx, worker)
	if err != nil || len(listed) != 1 || listed[0].Task.ID != "auto-task" || listed[0].HarnessSelection != "local" || listed[0].Confinement != "none" || listed[0].Auth != "byoa" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	// Held tasks are rejected at claim time (DEC-5), and a
	// cleared hold releases the order back to worker claimability.
	if _, claimErr := service.ClaimForWorker(workerCtx, worker, "manual-task-implement-1", core.WorkOrderClaim{SessionID: "session-held", ClientToken: "held-token"}); claimErr == nil || !strings.Contains(claimErr.Error(), "held") {
		t.Fatalf("held claim err=%v", claimErr)
	}
	if _, holdErr := st.SetTaskHold(workerCtx, "manual-task", false); holdErr != nil {
		t.Fatal(holdErr)
	}
	if relisted, relistErr := service.ListClaimable(workerCtx, worker); relistErr != nil || len(relisted) != 2 {
		t.Fatalf("relisted=%+v err=%v", relisted, relistErr)
	}
	if _, holdErr := st.SetTaskHold(workerCtx, "manual-task", true); holdErr != nil {
		t.Fatal(holdErr)
	}
	claimed, err := service.ClaimForWorker(workerCtx, worker, listed[0].Order.ID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "child-token"})
	if err != nil || claimed.WorkerID != worker.ID {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if DefaultClaimLease != 5*time.Minute || claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt) != DefaultClaimLease {
		t.Fatalf("claim lease=%s default=%s, want 5m", claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt), DefaultClaimLease)
	}
	if DefaultLivenessLease != 15*time.Second {
		t.Fatalf("worker liveness lease changed to %s", DefaultLivenessLease)
	}
	deadline := claimed.ExecutionDeadline
	now = now.Add(10 * time.Second)
	renewed, err := service.Renew(workerCtx, worker, claimed.ID, "session-a")
	if err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	activity := strings.Repeat("x", ActivitySnapshotLimit+100) + " token=ghp_abcdefghijklmnopqrstuvwxyz123456"
	if renewed, err = service.Renew(workerCtx, worker, claimed.ID, "session-a", &core.WorkOrderActivitySnapshotInput{Content: activity}); err != nil || !renewed.ExecutionDeadline.Equal(deadline) {
		t.Fatalf("snapshot renewal=%+v err=%v", renewed, err)
	}
	if snapshot, exists, snapshotErr := st.GetWorkOrderActivitySnapshot(workerCtx, claimed.ID); snapshotErr != nil || !exists || len(snapshot.Content) > ActivitySnapshotLimit || strings.Contains(snapshot.Content, "ghp_") || snapshot.AttemptID != claimed.AttemptID {
		t.Fatalf("snapshot=%+v exists=%v err=%v", snapshot, exists, snapshotErr)
	}
	released, err := service.Release(workerCtx, worker, claimed.ID, core.WorkOrderRelease{SessionID: "session-a", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Choose whether to proceed."}, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	if err != nil || released.State != core.WorkOrderQueued || !released.ExecutionDeadline.IsZero() || !released.ExecutionStartedAt.IsZero() || !released.RetrySuppressed {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	if released.LastAttemptOutcome != core.WorkOrderOutcomeReleased || released.AutomaticRetryCount != 0 {
		t.Fatalf("checkpoint release outcome=%q retries=%d", released.LastAttemptOutcome, released.AutomaticRetryCount)
	}
	events, err := st.ListEvents(workerCtx, claimed.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var checkpointEvent core.Event
	for _, event := range events {
		if event.Kind == "work_order.released" && event.JobID == claimed.JobID {
			checkpointEvent = event
		}
		if (event.Kind == "work_order.child_failed" || event.Kind == "work_order.stalled") && event.JobID == claimed.JobID {
			t.Fatalf("checkpoint handoff emitted recovery-shaped event %q", event.Kind)
		}
	}
	if checkpointEvent.Kind == "" || !strings.Contains(string(checkpointEvent.Payload), `"reason":"`+core.WorkOrderReleaseReasonOperatorCheckpointReached+`"`) || !strings.Contains(string(checkpointEvent.Payload), `"release_cause":"`+core.WorkOrderReleaseCauseOperatorAction+`"`) || !strings.Contains(string(checkpointEvent.Payload), `"outcome":"released"`) || !strings.Contains(string(checkpointEvent.Payload), `"retry_suppressed":true`) {
		t.Fatalf("checkpoint release event=%+v", checkpointEvent)
	}
	if _, err = service.Release(workerCtx, worker, claimed.ID, core.WorkOrderRelease{SessionID: "session-a", Cause: "uncontrolled"}); err == nil || !strings.Contains(err.Error(), "invalid work-order release cause") {
		t.Fatalf("invalid release cause err=%v", err)
	}

	createOrder("submitted-task", false)
	submittedClaim, err := service.ClaimForWorker(workerCtx, worker, "submitted-task-implement-1", core.WorkOrderClaim{SessionID: "session-b", ClientToken: "submitted-token"})
	if err != nil {
		t.Fatal(err)
	}
	submittedClaim.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(workerCtx, submittedClaim); err != nil {
		t.Fatal(err)
	}
	if submitted, renewErr := service.Renew(workerCtx, worker, submittedClaim.ID, "session-b"); renewErr != nil || submitted.State != core.WorkOrderSubmitted || !submitted.ExecutionDeadline.Equal(submittedClaim.ExecutionDeadline) {
		t.Fatalf("submitted renew=%+v err=%v", submitted, renewErr)
	}
}

func TestReleaseCheckpointDecisionValidationAndCitations(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	claim := core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "worker", SessionID: "checkpoint-session"}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID, ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Checkpoints name the decision.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "The request is distinct."}}}}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-checkpoint", Title: "Checkpoint"}, core.RequirementVersion{Content: "Checkpoint authority.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Checkpoints name the decision.\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: The request is distinct.\n```", Statements: statements, Origin: core.RequirementOriginOperator})
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

func TestWorkerClaimUsesEnrollmentOwnerForAssignmentEligibility(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	createOrder := func(taskID string) core.WorkOrder {
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	assigned := createOrder("assigned-worker-owner")
	unassigned := createOrder("unassigned-legacy-worker")
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-u", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, assigned.TaskID, "usr-u"); err != nil {
		t.Fatal(err)
	}
	worker := func(id, owner string) core.Worker {
		return core.Worker{ID: id, Workspace: "demo", OwnerUserID: owner, LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}}
	}
	for name, candidate := range map[string]core.Worker{"different owner": worker("worker-v", "usr-v"), "ownerless legacy": worker("worker-legacy", "")} {
		if _, err := service.ClaimForWorker(ctx, candidate, assigned.ID, core.WorkOrderClaim{SessionID: name, ClientToken: name}); err == nil || !strings.Contains(err.Error(), "usr-u") {
			t.Fatalf("%s claim error=%v", name, err)
		}
	}
	claimed, err := service.ClaimForWorker(ctx, worker("worker-u", "usr-u"), assigned.ID, core.WorkOrderClaim{SessionID: "owned", ClientToken: "owned"})
	if err != nil || claimed.WorkerID != "worker-u" {
		t.Fatalf("owned claim=%+v err=%v", claimed, err)
	}
	legacyClaim, err := service.ClaimForWorker(ctx, worker("worker-legacy", ""), unassigned.ID, core.WorkOrderClaim{SessionID: "legacy", ClientToken: "legacy", OwnerUserID: "forged"})
	if err != nil || legacyClaim.WorkerID != "worker-legacy" {
		t.Fatalf("legacy unassigned claim=%+v err=%v", legacyClaim, err)
	}
}

func TestAttemptCheckpointIsAttemptScopedAndIdempotent(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "checkpoint-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: "checkpoint-task-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement,
		State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	worker := core.Worker{ID: "checkpoint-worker", Workspace: "demo"}
	service := &Service{Store: st}
	first, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "session-first", ClientToken: "token-first", WorkerID: worker.ID, ClaimantID: worker.ID,
		Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := core.WorkOrderAttemptCheckpoint{
		SessionID: "session-first", AttemptID: first.AttemptID, TerminationReason: "harness exited",
		CommitSHA: "1111111111111111111111111111111111111111", PushResult: "pushed",
		Transcript: &core.WorkOrderAttemptTranscript{Content: "before token=ghp_abcdefghijklmnopqrstuvwxyz123456 after"},
	}
	if _, err = service.Renew(ctx, worker, first.ID, "session-first", &core.WorkOrderActivitySnapshotInput{Content: "still running"}); err != nil {
		t.Fatal(err)
	}
	created, err := service.CheckpointAttempt(ctx, worker, first.ID, checkpoint)
	if err != nil || !created {
		t.Fatalf("first checkpoint created=%v err=%v", created, err)
	}
	if created, err = service.CheckpointAttempt(ctx, worker, first.ID, checkpoint); err != nil || created {
		t.Fatalf("duplicate checkpoint created=%v err=%v", created, err)
	}
	if _, exists, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, first.ID); snapshotErr != nil || exists {
		t.Fatalf("checkpoint did not supersede snapshot exists=%v err=%v", exists, snapshotErr)
	}
	if captures, captureErr := st.ListWorkOrderTranscriptCaptures(ctx, first.ID); captureErr != nil || len(captures) != 1 || captures[0].AttemptID != first.AttemptID || captures[0].TerminationReason != "harness exited" || strings.Contains(captures[0].Content, "ghp_") {
		t.Fatalf("captures=%+v err=%v", captures, captureErr)
	}
	wrong := checkpoint
	wrong.SessionID = "different-session"
	if _, err = service.CheckpointAttempt(ctx, worker, first.ID, wrong); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-session checkpoint err=%v", err)
	}

	if _, err = storetest.For(st).ReleaseWorkerClaim(ctx, first.ID, worker.ID, core.WorkOrderRelease{
		SessionID: "session-first", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
	}); err != nil {
		t.Fatal(err)
	}
	late := checkpoint
	late.CommitSHA = "4444444444444444444444444444444444444444"
	late.TerminationReason = "claim authority lost: server reports queued"
	if created, err = service.CheckpointAttempt(ctx, worker, first.ID, late); err != nil || !created {
		t.Fatalf("late authority-loss checkpoint created=%v err=%v", created, err)
	}
	late.SessionID = "different-session"
	late.CommitSHA = "5555555555555555555555555555555555555555"
	if _, err = service.CheckpointAttempt(ctx, worker, first.ID, late); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("unattributable late checkpoint err=%v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := storetest.For(st).ClaimWorkOrder(ctx, first.ID, core.WorkOrderClaim{
		SessionID: "session-second", ClientToken: "token-second", WorkerID: worker.ID,
		Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	predecessor := core.WorkOrderAttemptCheckpoint{
		SessionID: "session-second", AttemptID: first.AttemptID, TerminationReason: "successor adopted dirty predecessor work",
		CommitSHA: "2222222222222222222222222222222222222222", PushResult: "pushed",
	}
	if created, err = service.CheckpointAttempt(ctx, worker, second.ID, predecessor); err != nil || !created {
		t.Fatalf("predecessor checkpoint created=%v err=%v", created, err)
	}
	predecessor.AttemptID = "attempt-unrelated"
	predecessor.CommitSHA = "3333333333333333333333333333333333333333"
	if _, err = service.CheckpointAttempt(ctx, worker, second.ID, predecessor); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("unattributable checkpoint err=%v", err)
	}

	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints int
	for _, event := range events {
		if event.Kind == "work_order.attempt_checkpointed" {
			checkpoints++
			if !strings.Contains(string(event.Payload), `"push_result":"pushed"`) || !strings.Contains(string(event.Payload), `"work_order_id":"`+job.ID+`"`) {
				t.Fatalf("checkpoint payload=%s", event.Payload)
			}
		}
	}
	if checkpoints != 3 {
		t.Fatalf("checkpoint events=%d, want 3", checkpoints)
	}
}

func TestListClaimableOrdersByQueueEntryWithReviewPreference(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }, RetryDelay: time.Nanosecond, RetryMaximum: time.Nanosecond}
	worker := core.Worker{ID: "claim-order-worker", Workspace: "demo", Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}, LeaseExpiresAt: now.Add(time.Minute)}

	createOrder := func(id string, stage core.Stage, queueEnteredAt, createdAt time.Time) {
		task := core.Task{ID: id, Workspace: "demo", State: core.TaskRunning, NextStage: stage, CreatedAt: createdAt}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: queueEnteredAt, QueueDeadline: queueEnteredAt.Add(24 * time.Hour), CreatedAt: createdAt}); err != nil {
			t.Fatal(err)
		}
	}

	createOrder("newer-entry", core.StageImplement, now.Add(-time.Hour), now.Add(-3*time.Hour))
	createOrder("older-entry-z", core.StageImplement, now.Add(-2*time.Hour), now)
	createOrder("older-entry-a", core.StageImplement, now.Add(-2*time.Hour), now.Add(time.Hour))
	listed, err := service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("claimable orders = %+v, want 3", listed)
	}
	got := []string{listed[0].Order.ID, listed[1].Order.ID, listed[2].Order.ID}
	want := []string{"older-entry-a-implement-1", "older-entry-z-implement-1", "newer-entry-implement-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO claim order = %v, want %v", got, want)
	}

	claimed, err := service.ClaimForWorker(ctx, worker, "older-entry-a-implement-1", core.WorkOrderClaim{SessionID: "release-session", ClientToken: "release-token"})
	if err != nil {
		t.Fatal(err)
	}
	released, err := service.Release(ctx, worker, claimed.ID, core.WorkOrderRelease{SessionID: "release-session", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "test retry", FailureDetail: "provider usage limit reached"})
	if err != nil {
		t.Fatal(err)
	}
	if !released.QueueEnteredAt.After(now.Add(-time.Minute)) {
		t.Fatalf("released queue entry was not refreshed: %s", released.QueueEnteredAt)
	}
	if released.LastFailureCategory != core.WorkOrderFailureProviderUsageLimit || released.LastAttemptID != claimed.AttemptID {
		t.Fatalf("released category/attempt=%+v", released)
	}
	listed, err = service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("claimable orders after release = %+v, want 3", listed)
	}
	got = []string{listed[0].Order.ID, listed[1].Order.ID, listed[2].Order.ID}
	want = []string{"older-entry-z-implement-1", "newer-entry-implement-1", "older-entry-a-implement-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claim order after release = %v, want %v", got, want)
	}

	createOrder("old-review", core.StageReview, now.Add(-30*time.Minute), now.Add(2*time.Hour))
	listed, err = service.ListClaimable(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 4 {
		t.Fatalf("claimable orders with review = %+v, want 4", listed)
	}
	if listed[0].Order.Stage != core.StageReview || listed[0].Order.ID != "old-review-review-1" {
		t.Fatalf("review preference did not override queue age: first=%+v", listed[0].Order)
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

func TestFailureDetailBoundAndProviderModelRejectionMatcher(t *testing.T) {
	detail := strings.Repeat("x", FailureDetailLimit+100) + " unsupported model "
	bounded := boundedFailureDetail(detail)
	if len(bounded) > FailureDetailLimit || !strings.HasSuffix(bounded, "unsupported model") {
		t.Fatalf("bounded detail length=%d suffix=%q", len(bounded), bounded[len(bounded)-20:])
	}
	for _, fixture := range []struct {
		detail string
		want   bool
	}{
		{"The model is not supported when using this account", true},
		{"unsupported model provider/foo", true},
		{"provider returned status 400", false},
		{"harness exited with status 1", false},
	} {
		if got := providerModelRejection(fixture.detail); got != fixture.want {
			t.Fatalf("providerModelRejection(%q)=%v want %v", fixture.detail, got, fixture.want)
		}
	}
}

func TestObservabilityBoundsPreserveNewestValidUTF8(t *testing.T) {
	prefix := strings.Repeat("x", ActivitySnapshotLimit) + "€"
	bounded, truncated := boundedObservabilityContent(prefix+"newest", ActivitySnapshotLimit)
	if !truncated || len(bounded) > ActivitySnapshotLimit || !strings.HasSuffix(bounded, "newest") || strings.ToValidUTF8(bounded, "") != bounded {
		t.Fatalf("bounded length=%d truncated=%v suffix=%q", len(bounded), truncated, bounded[len(bounded)-16:])
	}
	transcript, transcriptTruncated := boundedObservabilityContent("short", AttemptTranscriptLimit, true)
	if transcript != "short" || !transcriptTruncated {
		t.Fatalf("transcript=%q truncated=%v", transcript, transcriptTruncated)
	}
}

func TestObservabilityPersistenceFailuresDoNotChangeLifecycleResults(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "observability-failure", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	worker := core.Worker{ID: "worker", Workspace: "demo"}
	claimed, err := storetest.For(base).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: "session", ClientToken: "token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: failingObservabilityStore{Store: base}}
	if renewed, renewErr := service.Renew(ctx, worker, job.ID, "session", &core.WorkOrderActivitySnapshotInput{Content: "tail"}); renewErr != nil || renewed.State != core.WorkOrderClaimed {
		t.Fatalf("renewed=%+v err=%v", renewed, renewErr)
	}
	checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: "session", AttemptID: claimed.AttemptID, TerminationReason: "harness exited", CommitSHA: strings.Repeat("a", 40), PushResult: "pushed", Transcript: &core.WorkOrderAttemptTranscript{Content: "transcript"}}
	if created, checkpointErr := service.CheckpointAttempt(ctx, worker, job.ID, checkpoint); checkpointErr != nil || !created {
		t.Fatalf("checkpoint created=%v err=%v", created, checkpointErr)
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

func TestTaskAvailabilityReportsWorkerLivenessAndQueueContext(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	worker := core.Worker{ID: "worker-status", Workspace: "demo", Name: "status", CredentialHash: "hash", CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), []core.HarnessProbe{{Harness: "claude", Healthy: false, CheckedAt: now}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "claude"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "claude", Execution: config.ExecutionMCP}, "review": {Harness: "claude", Execution: config.ExecutionMCP}}}}
	service := &Service{Store: st, Now: func() time.Time { return now }}
	task := core.Task{ID: "worker-status-task", Workspace: "demo", NextStage: core.StageReview}
	orders := []core.WorkOrder{{ID: "seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.WorkOrderQueued, RequiredHarness: "claude", RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired}}
	status := service.TaskAvailability(ctx, cfg, task, orders)
	if !status.Available || status.QueueContext != "interrupted" || status.LastHeartbeatAge != "0s" || len(status.RequiredHarnesses) != 0 {
		t.Fatalf("status=%+v", status)
	}
	if _, err := st.HeartbeatWorker(ctx, worker.ID, now.Add(DefaultLivenessLease), []core.HarnessProbe{{Harness: "claude", Healthy: true, CheckedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if healthy := service.TaskAvailability(ctx, cfg, task, orders); !healthy.Available {
		t.Fatalf("healthy status=%+v", healthy)
	}
}

func TestTaskAvailabilityUsesWorkerLivenessNotHarnessProbes(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	worker := core.Worker{ID: "stale-probe-worker", Workspace: "demo", Name: "stale", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: false}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st, Now: func() time.Time { return now }}
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}}
	task := core.Task{ID: "claimed-health", Workspace: "demo", State: core.TaskRunning}
	claimed := core.WorkOrder{ID: "claimed-health-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.WorkOrderClaimed, RequiredHarness: "codex", LeaseExpiresAt: now.Add(time.Minute)}
	status := service.TaskAvailability(ctx, cfg, task, []core.WorkOrder{claimed})
	if status == nil || !status.Available || status.Reason != "active claimed order is being served" {
		t.Fatalf("live claim status=%+v", status)
	}
	claimed.LeaseExpiresAt = now.Add(-time.Second)
	status = service.TaskAvailability(ctx, cfg, task, []core.WorkOrder{claimed})
	if status == nil || !status.Available {
		t.Fatalf("expired claim status=%+v", status)
	}
	claimed.State = core.WorkOrderCancelled
	if status = service.TaskAvailability(ctx, cfg, task, []core.WorkOrder{claimed}); status != nil {
		t.Fatalf("revoked claim status=%+v", status)
	}
}

func TestTaskAvailabilityOmitsWarningWithoutEnrolledWorkers(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	service := &Service{Store: st, Now: func() time.Time { return now }}
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "codex"}}}}
	task := core.Task{ID: "pull-only-task", Workspace: "demo"}
	orders := []core.WorkOrder{{ID: "pull-only-order", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredHarness: "codex"}}
	status := service.TaskAvailability(ctx, cfg, task, orders)
	if status != nil {
		t.Fatalf("status=%+v, want neutral pull-only state", status)
	}
	revoked := core.Worker{ID: "revoked", Workspace: "demo", Name: "revoked", CreatedAt: now}
	if err := st.CreateWorker(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeWorker(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if status = service.TaskAvailability(ctx, cfg, task, orders); status != nil {
		t.Fatalf("revoked-only status=%+v, want neutral pull-only state", status)
	}
	stale := core.Worker{ID: "stale", Workspace: "demo", Name: "stale-codex", LastSeenAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(-time.Second), CreatedAt: now.Add(time.Second)}
	if err := st.CreateWorker(ctx, stale); err != nil {
		t.Fatal(err)
	}
	status = service.TaskAvailability(ctx, cfg, task, orders)
	if status == nil || status.Available || !strings.Contains(status.Reason, "no live enrolled worker") || strings.Contains(status.Reason, "codex") {
		t.Fatalf("stale status=%+v", status)
	}
}

func TestTaskAvailabilityOmitsStatusWithoutActionableTaskOrder(t *testing.T) {
	status := (&Service{Store: store.NewMemory()}).TaskAvailability(
		store.WithWorkspace(t.Context(), "demo"),
		&config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}},
		core.Task{ID: "reviewed-task", Workspace: "demo", State: core.TaskAwaiting},
		[]core.WorkOrder{{ID: "completed-review", TaskID: "reviewed-task", Stage: core.StageReview, State: core.WorkOrderCompleted, RequiredHarness: "codex"}},
	)
	if status != nil {
		t.Fatalf("status=%+v, want nil without queued or claimed task-owned work", status)
	}
}

func TestBlockedImplementationIsWorkerVisibleButUnclaimable(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	workOrders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: workOrders, ConfigProvider: workOrders.ConfigProvider, Now: func() time.Time { return now }}
	worker := core.Worker{
		ID: "worker", Workspace: "demo", LeaseExpiresAt: now.Add(time.Minute),
		Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}},
	}
	dependency := core.Task{ID: "dependency", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	dependent := core.Task{ID: "dependent", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTaskWithDependencies(ctx, dependent, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "dependent-implement-1", TaskID: dependent.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: job.ID, TaskID: dependent.ID, JobID: job.ID, Stage: job.Stage,
		QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if claimable, err := service.ListClaimable(ctx, worker); err != nil || len(claimable) != 0 {
		t.Fatalf("claimable=%+v err=%v", claimable, err)
	}
	visible, err := service.ListVisibleOrders(ctx, worker)
	if err != nil || len(visible) != 1 {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	if visible[0].ID != job.ID || visible[0].Claimable || len(visible[0].BlockingTaskIDs) != 1 ||
		visible[0].BlockingTaskIDs[0] != dependency.ID {
		t.Fatalf("blocked visibility=%+v", visible[0])
	}
	if _, err = service.ClaimForWorker(ctx, worker, job.ID, core.WorkOrderClaim{
		SessionID: "blocked", ClientToken: "secret",
	}); err == nil || !strings.Contains(err.Error(), dependency.ID) {
		t.Fatalf("blocked worker claim error=%v", err)
	}
}

func TestWorkerDispatchDoesNotFilterClientLocalHarnesses(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.Harnesses = append(cfg.Harnesses, config.Harness{Name: "reviewer", Command: []string{"reviewer", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"reviewer", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second})
	review := cfg.Routing.Stages["review"]
	review.Harness = "reviewer"
	cfg.Routing.Stages["review"] = review
	ctx := store.WithWorkspace(t.Context(), "demo")
	orders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: orders, ConfigProvider: orders.ConfigProvider, Now: func() time.Time { return now }}
	worker := core.Worker{ID: "partial-worker", Workspace: "demo", Name: "partial", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "reviewer", Healthy: false}}, CreatedAt: now}
	if err := st.CreateWorker(ctx, worker); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "route-health", Workspace: "demo", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "route-health-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if listed, err := service.ListClaimable(ctx, worker); err != nil || len(listed) != 1 || listed[0].Harness.Name != "" || listed[0].Model != "" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if claimed, err := service.ClaimForWorker(ctx, worker, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token"}); err != nil || claimed.WorkerID != worker.ID || claimed.Agent != "worker" {
		t.Fatalf("claimed=%+v error=%v", claimed, err)
	}
}

func workerTestConfig() *config.Config {
	harness := config.Harness{Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s", ProbeTimeout: 5 * time.Second}
	return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}, "review": {Model: "gpt", Harness: "codex", Timeout: time.Hour, Execution: config.ExecutionMCP}}}}
}

func TestHarnessFingerprintCanonicalizesEmptyArguments(t *testing.T) {
	base := config.Harness{Name: "codex", Command: []string{"codex"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}
	withEmpty := base
	withEmpty.ModelArgs = []string{}
	if HarnessFingerprint(base) != HarnessFingerprint(withEmpty) {
		t.Fatal("nil and empty optional arguments produced different harness fingerprints")
	}
	changed := base
	changed.Command = []string{"codex-next"}
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different harness commands produced the same fingerprint")
	}
	changed = base
	changed.MCPTransport = config.MCPTransportTOMLOverride
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different MCP transports produced the same fingerprint")
	}
	changed = base
	changed.MCPAttachment = "conveyor"
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different MCP attachment identities produced the same fingerprint")
	}
	changed = base
	changed.StallTimeoutText = "2m"
	if HarnessFingerprint(base) == HarnessFingerprint(changed) {
		t.Fatal("different stall timeouts produced the same fingerprint")
	}
}

func TestLegacyHarnessSnapshotDefaultsToJSONFileTransport(t *testing.T) {
	harness := harnessFromSnapshot(&core.HarnessSnapshot{Name: "legacy", ProbeTimeoutText: "5s"})
	if harness.MCPTransport != config.MCPTransportJSONFile {
		t.Fatalf("legacy transport=%q want=%q", harness.MCPTransport, config.MCPTransportJSONFile)
	}
}

func TestImplementationDispatchLeavesExecutionToClientLocalSetup(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	cfg := workerTestConfig()
	cfg.Harnesses[0].EffortArgs["high"] = []string{"--config", `model_reasoning_effort="low"`}
	cfg.Repos = []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}
	orders := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	service := &Service{Store: st, WorkOrders: orders, ConfigProvider: orders.ConfigProvider, Now: func() time.Time { return now }}
	task := core.Task{ID: "implementation-effort", Workspace: "demo", Repo: "app", BaseBranch: "main", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "implementation-effort-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	snapshot := &core.HarnessSnapshot{
		Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"},
		EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, Effort: "high",
		EffortArgv: []string{"--config", `model_reasoning_effort="high"`}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s",
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, RequiredModel: "gpt", RequiredHarness: "codex", RequiredEffort: "high", RequiredHarnessConfig: snapshot, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	snapshotHarness := harnessFromSnapshot(snapshot)
	worker := core.Worker{ID: "implementation-worker", Workspace: "demo", CredentialHash: "hash", LeaseExpiresAt: now.Add(time.Minute), Probes: []core.HarnessProbe{{Harness: "codex", Fingerprint: HarnessFingerprint(snapshotHarness), Healthy: true}}, CreatedAt: now}
	listed, err := service.ListClaimable(ctx, worker)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	item := listed[0]
	if item.Model != "" || item.Effort != "" || item.Harness.Name != "" || len(item.EffortArgv) != 0 || !reflect.DeepEqual(item.Repository, cfg.Repos[0]) {
		t.Fatalf("server leaked execution selection into worker dispatch: %+v", item)
	}
}

// Simulate a restart after hot reload removes one harness, changes the
// other's command, and replaces the next round's panel. The existing round
// must still accept its old probe and dispatch both original definitions.
