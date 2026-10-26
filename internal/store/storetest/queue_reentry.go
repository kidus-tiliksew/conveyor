package storetest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// legacyExecutionPins stamps the harness, model, and effort pins that legacy
// server-pinned dispatch persisted on an order; under DEC-56 the claiming
// machine's local execution setup supplies them instead.
func legacyExecutionPins(order *core.WorkOrder) {
	order.RequiredHarness, order.RequiredModel, order.RequiredEffort = "legacy-harness", "legacy-model", "high"
	order.RequiredHarnessConfig = &core.HarnessSnapshot{Name: "legacy-harness", Command: []string{"legacy", "exec", "{prompt}"}, Effort: "high", EffortArgv: []string{"--effort", "high"}}
}

func requireExecutionPins(t *testing.T, label string, order core.WorkOrder) {
	t.Helper()
	if order.RequiredHarness != "legacy-harness" || order.RequiredModel != "legacy-model" || order.RequiredEffort != "high" ||
		order.RequiredHarnessConfig == nil || order.RequiredHarnessConfig.Name != "legacy-harness" {
		t.Fatalf("%s: fixture did not persist legacy execution pins: harness=%q model=%q effort=%q snapshot=%+v",
			label, order.RequiredHarness, order.RequiredModel, order.RequiredEffort, order.RequiredHarnessConfig)
	}
}

func requireNoExecutionPins(t *testing.T, label string, order core.WorkOrder) {
	t.Helper()
	if order.RequiredHarness != "" || order.RequiredModel != "" || order.RequiredEffort != "" || order.RequiredHarnessConfig != nil {
		t.Fatalf("%s: queue re-entry kept execution pins: harness=%q model=%q effort=%q snapshot=%+v",
			label, order.RequiredHarness, order.RequiredModel, order.RequiredEffort, order.RequiredHarnessConfig)
	}
}

// requireStoredWithoutPins checks both the command result and the persisted
// row, so a backend cannot clear only its returned copy.
func requireStoredWithoutPins(t *testing.T, x Fixture, label string, returned core.WorkOrder) core.WorkOrder {
	t.Helper()
	if returned.State != core.WorkOrderQueued {
		t.Fatalf("%s: state=%s, want queued", label, returned.State)
	}
	requireNoExecutionPins(t, label+" result", returned)
	stored, err := x.Backend.GetWorkOrder(x.Context, returned.ID)
	requireOK(t, err)
	requireNoExecutionPins(t, label+" stored", stored)
	return stored
}

func newPinnedImplementOrder(t *testing.T, x Fixture) core.WorkOrder {
	t.Helper()
	task := newAggregateTask(t, x)
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	legacyExecutionPins(&order)
	created, err := CreateStageWorkOrder(x.Context, x.Backend, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("pinned order was not created")
	}
	stored, err := x.Backend.GetWorkOrder(x.Context, order.ID)
	requireOK(t, err)
	requireExecutionPins(t, "created implement order", stored)
	return stored
}

// retryableRelease returns a child-failure release whose retry delay has
// already elapsed, so the released order is immediately claimable again.
func retryableRelease(session string) core.WorkOrderRelease {
	return core.WorkOrderRelease{SessionID: session, Reason: "machine handoff", Outcome: core.WorkOrderOutcomeChildFailure, FailureDetail: "harness exited",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond, AutomaticRetryLimit: 3}
}

// runQueueReentryPins proves that every queue re-entry command clears legacy
// harness, model, and effort pins while keeping review round and seat, and
// that the next claim takes its execution identity from the claiming machine
// (req-worker AC-2.2, AC-2.3; DEC-56).
func runQueueReentryPins(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	t.Run("ReleaseThenNextMachineClaims", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "first-machine", ClientToken: "first", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		released, err := ReleaseWorkerClaim(ctx, st, order.ID, "worker-a", retryableRelease("first-machine"))
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "release", released)
		time.Sleep(time.Millisecond)
		next, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "next-machine", ClientToken: "next", ClaimantID: "worker-b", WorkerID: "worker-b", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		identity := core.WorkOrderClaimIdentity{WorkerID: "worker-b", ClaimantID: "worker-b", SessionID: "next-machine"}
		// The continuation harness falls back to the order's pinned harness
		// when the claim names no agent; a cleared pin admits the next
		// machine's own harness.
		if _, err = st.RecordWorkOrderContinuation(ctx, order.ID, identity, core.WorkOrderContinuation{SessionID: "native", AttemptID: next.AttemptID, Harness: "next-harness", LaunchEnvironment: "test"}); err != nil {
			t.Fatalf("next machine harness refused after re-entry: %v", err)
		}
	})
	t.Run("LeaseExpiry", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "expiring", ClientToken: "expiring", Lease: time.Nanosecond, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		time.Sleep(time.Millisecond)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		expired, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
			t.Fatalf("lease expiry outcome=%q", expired.LastAttemptOutcome)
		}
		requireStoredWithoutPins(t, x, "lease expiry", expired)
	})
	t.Run("StaleRedispatch", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
			return st.ApplyWorkOrderClock(ctx, lease, order.TaskID, order.QueueDeadline.Add(time.Second))
		})
		requireOK(t, err)
		stale, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if stale.State != core.WorkOrderStale {
			t.Fatalf("clock state=%s, want stale", stale.State)
		}
		redispatched, err := RedispatchWorkOrder(ctx, st, order.ID, time.Hour)
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "stale redispatch", redispatched)
	})
	t.Run("RecoveryAfterTimeout", func(t *testing.T) {
		for _, refreeze := range []*store.RecoveryRefreeze{nil, {ExecutionTimeoutText: "2h"}} {
			order := newPinnedImplementOrder(t, x)
			_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "timing-out", ClientToken: "timing-out", Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
			requireOK(t, err)
			time.Sleep(time.Millisecond)
			_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
			requireOK(t, err)
			timedOut, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			if timedOut.State != core.WorkOrderTimedOut {
				t.Fatalf("clock state=%s, want timed_out", timedOut.State)
			}
			var refreezes []*store.RecoveryRefreeze
			if refreeze != nil {
				refreezes = append(refreezes, refreeze)
			}
			recovered, err := RecoverWorkOrder(ctx, st, order.ID, "recover-"+core.NewTaskID(), time.Hour, refreezes...)
			requireOK(t, err)
			stored := requireStoredWithoutPins(t, x, "recovery", recovered)
			if refreeze != nil && stored.ExecutionTimeoutText != "2h" {
				t.Fatalf("recovery refreeze timeout=%q", stored.ExecutionTimeoutText)
			}
		}
	})
	t.Run("Preempt", func(t *testing.T) {
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "preempted", ClientToken: "preempted", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		result, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
			return st.PreemptWorkOrderCommand(ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: order.ID, RequestID: "preempt-" + core.NewTaskID(), Reason: "fixture preempt"})
		})
		requireOK(t, err)
		requireStoredWithoutPins(t, x, "preempt", result.WorkOrder)
	})
	t.Run("ReviewSeatRequeue", func(t *testing.T) {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		for i := range orders {
			legacyExecutionPins(&orders[i])
		}
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		seat := orders[1]
		stored, err := st.GetWorkOrder(ctx, seat.ID)
		requireOK(t, err)
		requireExecutionPins(t, "created review seat", stored)
		// The legacy pin is still enforced before re-entry: a worker with a
		// different model cannot claim the pinned seat.
		if _, err = ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "wrong-model", ClientToken: "wrong-model", ClaimantID: "worker-a", WorkerID: "worker-a", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour}); err == nil {
			t.Fatal("pinned review seat accepted a different worker model before re-entry")
		}
		_, err = ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "seat-two-first", ClientToken: "seat-two-first", ClaimantID: "worker-a", WorkerID: "worker-a", Model: "legacy-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		released, err := ReleaseWorkerClaim(ctx, st, seat.ID, "worker-a", retryableRelease("seat-two-first"))
		requireOK(t, err)
		requeued := requireStoredWithoutPins(t, x, "review seat release", released)
		if released.ReviewRound != 1 || released.ReviewSeat != 2 || requeued.ReviewRound != 1 || requeued.ReviewSeat != 2 {
			t.Fatalf("review seat identity changed: result=%d/%d stored=%d/%d", released.ReviewRound, released.ReviewSeat, requeued.ReviewRound, requeued.ReviewSeat)
		}
		time.Sleep(time.Millisecond)
		next, err := ClaimWorkOrder(ctx, st, seat.ID, core.WorkOrderClaim{SessionID: "seat-two-next", ClientToken: "seat-two-next", ClaimantID: "worker-b", WorkerID: "worker-b", Model: "next-model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatalf("next machine model refused after re-entry: %v", err)
		}
		if next.ReviewRound != 1 || next.ReviewSeat != 2 || next.Model != "next-model" {
			t.Fatalf("next claim seat=%d/%d model=%q", next.ReviewRound, next.ReviewSeat, next.Model)
		}
		other, err := st.GetWorkOrder(ctx, orders[0].ID)
		requireOK(t, err)
		if other.ReviewSeat != 1 || other.State != core.WorkOrderQueued {
			t.Fatalf("sibling seat changed: seat=%d state=%s", other.ReviewSeat, other.State)
		}
	})
	t.Run("InterruptedReviewRecovery", func(t *testing.T) {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		for i := range orders {
			legacyExecutionPins(&orders[i])
		}
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		first.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
		_, err = ClaimWorkOrder(ctx, st, orders[1].ID, core.WorkOrderClaim{SessionID: "seat-two", ClientToken: "two", Lease: time.Nanosecond, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		time.Sleep(time.Millisecond)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		result, err := RecoverInterruptedReviewRound(ctx, st, store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: "recover-" + core.NewTaskID(), Round: 1}, time.Hour)
		requireOK(t, err)
		if len(result.RecoveredOrders) != 1 || result.RecoveredOrders[0].ID != orders[1].ID {
			t.Fatalf("recovered orders=%+v", result.RecoveredOrders)
		}
		recovered := requireStoredWithoutPins(t, x, "interrupted review recovery", result.RecoveredOrders[0])
		if recovered.ReviewRound != 1 || recovered.ReviewSeat != 2 || recovered.RetrySuppressed {
			t.Fatalf("recovered seat=%d/%d suppressed=%v", recovered.ReviewRound, recovered.ReviewSeat, recovered.RetrySuppressed)
		}
		retained, err := st.GetWorkOrder(ctx, orders[0].ID)
		requireOK(t, err)
		if retained.State != core.WorkOrderCompleted || retained.ReviewSeat != 1 {
			t.Fatalf("completed seat changed: state=%s seat=%d", retained.State, retained.ReviewSeat)
		}
	})
	t.Run("ClaimedAttemptKeepsSnapshot", func(t *testing.T) {
		// Active-attempt freezing is unchanged: renewal does not clear the
		// pins of an attempt that is still running.
		order := newPinnedImplementOrder(t, x)
		_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "running", ClientToken: "running", ClaimantID: "worker-a", WorkerID: "worker-a", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		renewed, err := RenewWorkerClaim(ctx, st, order.ID, "worker-a", "running", 2*time.Minute)
		requireOK(t, err)
		if renewed.State != core.WorkOrderClaimed {
			t.Fatalf("renewed state=%s", renewed.State)
		}
		requireExecutionPins(t, "claimed attempt", renewed)
	})
	t.Run("VerifyAuthority", func(t *testing.T) { runVerifyAuthorityReentry(t, x) })
}

// newLateAuthorityVerifyOrder creates a verify-stage task, with no served
// requirement, and its queued verify order at the submitted head.
func newLateAuthorityVerifyOrder(t *testing.T, x Fixture) core.WorkOrder {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: strings.Repeat("c", 40), CreatedAt: time.Now().UTC()}
	task.SetupContract.VerifyStage = true
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: core.StageVerify, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	_, err := CreateStageWorkOrder(x.Context, x.Backend, job, order)
	requireOK(t, err)
	return order
}

// claimVerifyLive claims a verify order with the live authority that
// workorder.Service.Claim resolves for every verify claim.
func claimVerifyLive(t *testing.T, x Fixture, orderID, session string, lease, timeout time.Duration) core.WorkOrder {
	t.Helper()
	order, err := x.Backend.GetWorkOrder(x.Context, orderID)
	requireOK(t, err)
	task, err := x.Backend.GetTask(x.Context, order.TaskID)
	requireOK(t, err)
	served, err := store.ServedRequirementsForTask(x.Context, x.Backend, task.ID)
	requireOK(t, err)
	governance, err := store.GovernanceForTask(x.Context, x.Backend, task.ID, task.Repo)
	requireOK(t, err)
	claimed, err := ClaimWorkOrder(x.Context, x.Backend, orderID, core.WorkOrderClaim{WorkerID: "worker-verify", ClaimantID: "worker-verify", SessionID: session, ClientToken: session + "-token", Lease: lease, ExecutionTimeout: timeout,
		Requirements: append([]core.ServedRequirementContext{}, served.Requirements...), Governance: &governance})
	requireOK(t, err)
	return claimed
}

// attachConfirmedRequirement confirms two versions of a new requirement and
// attaches it to the task through a confirmed triage context proposal. It
// returns the requirement's current version.
func attachConfirmedRequirement(t *testing.T, x Fixture, taskID string) (string, int) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	id := "req-late-" + core.NewTaskID()
	version := func(statement string) core.RequirementVersion {
		return core.RequirementVersion{RequirementID: id, Content: "# " + id + "\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}, Origin: core.RequirementOriginOperator}
	}
	_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, version("Observe the late fixture."))
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, id, 1)
	requireOK(t, err)
	_, err = st.ProposeRequirementVersion(ctx, version("Observe the late fixture again."))
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, id, 2)
	requireOK(t, err)
	_, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: taskID, TargetKind: core.TaskContextProposalRequirement, TargetID: id, Source: core.TaskContextProposalTriage, Justification: "The task changes the late fixture."})
	requireOK(t, err)
	_, err = st.ConfirmTaskContextProposal(ctx, taskID, core.TaskContextProposalRequirement, id)
	requireOK(t, err)
	return id, 2
}

// verifySnapshotPins lists an order's governing pins as prepare_verification
// derives them from its snapshots.
func verifySnapshotPins(order core.WorkOrder) []core.VerificationPin {
	pins := []core.VerificationPin{}
	for _, r := range order.ServedRequirementSnapshot {
		pins = append(pins, core.VerificationPin{Kind: "requirement", DocumentID: r.ID, Version: r.Version})
	}
	if order.GovernanceSnapshot != nil {
		for _, d := range order.GovernanceSnapshot.Designs {
			pins = append(pins, core.VerificationPin{Kind: "system_design", DocumentID: d.ID, Version: d.Version})
		}
	}
	return pins
}

func requireSnapshotRequirement(t *testing.T, label string, order core.WorkOrder, id string, version int) {
	t.Helper()
	for _, r := range order.ServedRequirementSnapshot {
		if r.ID == id && r.Version == version {
			return
		}
	}
	t.Fatalf("%s: served snapshot %+v lacks %s v%d", label, order.ServedRequirementSnapshot, id, version)
}

// requireLateRequirementSelectable creates the claim's verification context
// from its snapshot pins and evaluates a kit pinning the late requirement.
func requireLateRequirementSelectable(t *testing.T, x Fixture, order core.WorkOrder, session, id string, version int) {
	t.Helper()
	revision := core.VerificationRevision{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: order.HeadSHA}
	pins := verifySnapshotPins(order)
	v := verificationFixture{x: x, ctx: store.WithActor(x.Context, store.Actor{ID: "worker:worker-verify", Role: core.ActorWorker}),
		access:    store.VerificationAccess{TaskID: order.TaskID, WorkOrderID: order.ID, WorkOrderAttemptID: order.AttemptID, ClientToken: session + "-token", Claim: core.WorkOrderClaimIdentity{WorkerID: "worker-verify", ClaimantID: "worker-verify", SessionID: session}},
		revisions: []core.VerificationRevision{revision}, pins: pins}
	discovery := []json.RawMessage{core.JSONPayload(map[string]any{"repository": revision.Repository, "revision": revision.SHA, "state": "present"})}
	created := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "late-" + session, Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: pins, Discovery: discovery}})
	snapshot, err := x.Backend.ReadVerification(v.ctx, v.access, created.ID)
	requireOK(t, err)
	found := false
	for _, p := range snapshot.Contexts[0].GoverningPins {
		found = found || (p.Kind == "requirement" && p.DocumentID == id && p.Version == version)
	}
	if !found {
		t.Fatalf("context pins %+v lack %s v%d", snapshot.Contexts[0].GoverningPins, id, version)
	}
	manifest, err := verification.Parse(strings.NewReader(fmt.Sprintf(lateKitManifest, id, version)), nil)
	requireOK(t, err)
	selectionPins := make([]verification.Pin, len(pins))
	for i, p := range pins {
		selectionPins[i] = verification.Pin{Kind: p.Kind, DocumentID: p.DocumentID, Version: p.Version}
	}
	receipt := verification.Evaluate(manifest, verification.SelectionContext{Pins: selectionPins, Stage: "verify", ManifestRevision: order.HeadSHA, SourceRevision: order.HeadSHA},
		map[string][]verification.TreeEntry{"late": {{Mode: "100755", Path: "check.sh", BlobOID: strings.Repeat("d", 40)}}})
	if len(receipt.Kits) != 1 || receipt.Kits[0].Eligibility != "eligible" {
		t.Fatalf("late kit selection = %+v", receipt)
	}
}

const lateKitManifest = `schema_version: 1
kits:
  - id: late
    name: Late kit
    version: "1"
    path: kits/late
    governing_pins:
      requirements: [{document_id: %s, version: %d}]
      system_designs: []
    exercises:
      - id: check
        stages: [verify]
        kind: script
        argv: ["./check.sh"]
        cwd: "."
        timeout_seconds: 30
        prerequisites: []
        permissions: []
        inputs: []
        required_assertions: [observed]
        retry_policy: safe_to_replay
        safety_basis: read-only
        operations: []
        evidence_outputs: []
        supports: []
`

// runVerifyAuthorityReentry proves that a verify order serves the task's
// current authority at each claim until a verification record binds its
// pins (component-work-orders, Verify authority at claim;
// req-verification-kits AC-2.1).
func runVerifyAuthorityReentry(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	tick := func(t *testing.T, at time.Time) {
		t.Helper()
		_, err := taskops.New(st).TickOrderClock(ctx, at)
		requireOK(t, err)
	}
	recoverOrder := func(t *testing.T, id string) {
		t.Helper()
		_, err := RecoverWorkOrder(ctx, st, id, "recover-"+core.NewTaskID(), time.Hour)
		requireOK(t, err)
	}
	operatorRelease := func(session string) core.WorkOrderRelease {
		return core.WorkOrderRelease{SessionID: session, Reason: "pinned authority diverged", Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}
	}
	// Each path ends a first claim, made with timeout as its execution
	// window, and leaves the order claimable.
	paths := []struct {
		name    string
		lease   time.Duration
		timeout time.Duration
		requeue func(t *testing.T, order core.WorkOrder)
	}{
		{"ReleaseThenRecover", time.Minute, time.Hour, func(t *testing.T, order core.WorkOrder) {
			_, err := ReleaseWorkerClaim(ctx, st, order.ID, "worker-verify", operatorRelease(order.SessionID))
			requireOK(t, err)
			recoverOrder(t, order.ID)
		}},
		{"LeaseExpiry", time.Nanosecond, time.Hour, func(t *testing.T, order core.WorkOrder) {
			time.Sleep(time.Millisecond)
			tick(t, time.Now().UTC())
			if expired, err := st.GetWorkOrder(ctx, order.ID); err != nil || expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
				t.Fatalf("lease expiry = %+v, %v", expired, err)
			}
			if expired, _ := st.GetWorkOrder(ctx, order.ID); !expired.Claimable {
				recoverOrder(t, order.ID)
			}
		}},
		{"TimeoutThenRecover", time.Minute, time.Nanosecond, func(t *testing.T, order core.WorkOrder) {
			time.Sleep(time.Millisecond)
			tick(t, time.Now().UTC())
			if timedOut, err := st.GetWorkOrder(ctx, order.ID); err != nil || timedOut.State != core.WorkOrderTimedOut {
				t.Fatalf("timeout = %+v, %v", timedOut, err)
			}
			recoverOrder(t, order.ID)
		}},
		{"AutomaticRecovery", time.Hour, time.Minute, func(t *testing.T, order core.WorkOrder) {
			tick(t, time.Now().UTC().Add(2*time.Minute))
			scheduled, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			if scheduled.State != core.WorkOrderTimedOut || scheduled.NextAutomaticRecoveryAt.IsZero() {
				t.Fatalf("automatic recovery not scheduled: %+v", scheduled)
			}
			tick(t, scheduled.NextAutomaticRecoveryAt)
			if recovered, err := st.GetWorkOrder(ctx, order.ID); err != nil || recovered.State != core.WorkOrderQueued || recovered.AutomaticRecoveryCount != 1 {
				t.Fatalf("automatic recovery = %+v, %v", recovered, err)
			}
		}},
		{"Preempt", time.Minute, time.Hour, func(t *testing.T, order core.WorkOrder) {
			_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
				return st.PreemptWorkOrderCommand(ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: order.ID, RequestID: "preempt-" + core.NewTaskID(), Reason: "fixture preempt"})
			})
			requireOK(t, err)
		}},
	}
	for _, path := range paths {
		t.Run("LateRequirement"+path.name, func(t *testing.T) {
			order := newLateAuthorityVerifyOrder(t, x)
			first := claimVerifyLive(t, x, order.ID, "first-"+path.name, path.lease, path.timeout)
			if first.ServedRequirementSnapshot == nil || len(first.ServedRequirementSnapshot) != 0 || first.GovernanceSnapshot == nil {
				t.Fatalf("first claim snapshots: requirements=%+v governance=%v", first.ServedRequirementSnapshot, first.GovernanceSnapshot != nil)
			}
			id, version := attachConfirmedRequirement(t, x, order.TaskID)
			path.requeue(t, first)
			second := claimVerifyLive(t, x, order.ID, "second-"+path.name, time.Hour, time.Hour)
			requireSnapshotRequirement(t, "reclaim", second, id, version)
			stored, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			requireSnapshotRequirement(t, "stored reclaim", stored, id, version)
			requireLateRequirementSelectable(t, x, second, "second-"+path.name, id, version)
		})
	}
	t.Run("SealedCheckpointKeepsSnapshot", func(t *testing.T) {
		v := newUngrantedVerificationFixture(t, x, verification.Permission{Kind: "network", TargetBinding: "fixture"})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: verificationCheckpointSubmission(verificationFixtureCoverage(v.snapshot(t)))})
		sealed, err := st.GetWorkOrder(ctx, v.access.WorkOrderID)
		requireOK(t, err)
		if sealed.State != core.WorkOrderQueued || sealed.Checkpoint == nil || sealed.Checkpoint.Verification == nil {
			t.Fatalf("checkpoint seal = %+v", sealed)
		}
		want := sealed.ServedRequirementSnapshot
		requireKept := func(t *testing.T, label string, order core.WorkOrder) {
			t.Helper()
			if len(order.ServedRequirementSnapshot) != len(want) || len(want) != 1 || order.ServedRequirementSnapshot[0].ID != want[0].ID || order.ServedRequirementSnapshot[0].Version != want[0].Version {
				t.Fatalf("%s: served snapshot %+v, want sealed %+v", label, order.ServedRequirementSnapshot, want)
			}
		}
		// The task gains a requirement after the seal; no later claim of
		// the sealed order may pin it.
		attachConfirmedRequirement(t, x, v.access.TaskID)
		recoverOrder(t, sealed.ID)
		steps := []struct {
			name           string
			lease, timeout time.Duration
			end            func(t *testing.T, order core.WorkOrder)
		}{
			{"LeaseExpiry", time.Nanosecond, time.Hour, paths[1].requeue},
			{"TimeoutThenRecover", time.Minute, time.Nanosecond, paths[2].requeue},
			{"Preempt", time.Minute, time.Hour, paths[4].requeue},
			// A session release replaces the checkpoint; the sealed
			// context still binds the pins.
			{"ReleaseReplacesCheckpoint", time.Minute, time.Hour, paths[0].requeue},
			// Automatic recovery skips an order holding a checkpoint, so
			// it applies only once the release has replaced it.
			{"AutomaticRecovery", time.Hour, time.Minute, paths[3].requeue},
		}
		for _, step := range steps {
			claimed := claimVerifyLive(t, x, sealed.ID, "sealed-"+step.name, step.lease, step.timeout)
			requireKept(t, step.name+" claim", claimed)
			step.end(t, claimed)
		}
		final := claimVerifyLive(t, x, sealed.ID, "sealed-final", time.Hour, time.Hour)
		requireKept(t, "final claim", final)
		if final.Checkpoint != nil && final.Checkpoint.Verification != nil {
			t.Fatal("session release kept the sealed checkpoint reference")
		}
	})
}
