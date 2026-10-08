package storetest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// captureWorker is the worker-plane identity used by attempt-capture cases.
const captureWorker = "capture-worker"

// attemptEnding ends one claimed attempt through an existing lifecycle path
// and returns the reason the capture must be bound to.
type attemptEnding struct {
	name string
	end  func(t *testing.T, x Fixture, claimed core.WorkOrder) string
}

// runAttemptObservability is the shared stage-independent attempt-ending
// capture contract (req-260820-221be8 AC-2.1–AC-2.3; DEC-26;
// component-work-orders): exact immutable claim ownership, a reason derived
// only from the attempt's persisted ending, one capture per attempt, a
// successor's activity snapshot preserved, and captured content kept out of
// every execution-facing read.
func runAttemptObservability(t *testing.T, x Fixture) {
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		t.Run(string(stage), func(t *testing.T) {
			for _, ending := range attemptEndings(stage) {
				t.Run(ending.name, func(t *testing.T) { runAttemptEndingCapture(t, x, stage, ending) })
			}
			t.Run("DelayedCaptureAfterSuccessor", func(t *testing.T) { runDelayedAttemptCapture(t, x, stage) })
			t.Run("WrongIdentityRefused", func(t *testing.T) { runAttemptCaptureIdentityRefusals(t, x, stage) })
			t.Run("NoSyntheticCapture", func(t *testing.T) { runAttemptCaptureUnverified(t, x, stage) })
		})
	}
	t.Run("RunPlaneIdentity", func(t *testing.T) { runRunPlaneAttemptCapture(t, x) })
	t.Run("PlanRevisionRelease", func(t *testing.T) { runPlanRevisionAttemptCapture(t, x) })
	t.Run("CrashedLauncherLeaseRecovery", func(t *testing.T) { runCrashedLauncherLeaseRecovery(t, x) })
}

// runCrashedLauncherLeaseRecovery models a launcher killed abruptly mid
// attempt: it renewed with an activity snapshot and then never released or
// captured. Ordinary lease-expiry recovery must treat that attempt exactly
// like an attempt that never had a launcher (same recovered row, same
// recoverability and fresh claim), and no capture may exist or be accepted for
// it afterwards (req-260820-221be8 AC-2.3; component-work-orders).
func runCrashedLauncherLeaseRecovery(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	for _, plane := range []struct {
		name     string
		claim    func(session string) core.WorkOrderClaim
		identity func(session string) core.WorkOrderClaimIdentity
	}{
		{name: "worker", claim: func(session string) core.WorkOrderClaim {
			return core.WorkOrderClaim{WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: session, ClientToken: session, Lease: time.Nanosecond, ExecutionTimeout: 24 * time.Hour}
		}, identity: workerCaptureIdentity},
		{name: "run", claim: func(session string) core.WorkOrderClaim {
			return core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("usr-crash"), OwnerUserID: "usr-crash", SessionID: session, ClientToken: session, Lease: time.Nanosecond, ExecutionTimeout: 24 * time.Hour}
		}, identity: func(session string) core.WorkOrderClaimIdentity {
			return core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-crash"), SessionID: session}
		}},
	} {
		t.Run(plane.name, func(t *testing.T) {
			control := newCaptureOrder(t, x, core.StageImplement)
			crashed := newCaptureOrder(t, x, core.StageImplement)
			controlClaim, err := ClaimWorkOrder(ctx, st, control.ID, plane.claim("control-"+core.NewTaskID()))
			requireOK(t, err)
			crashedClaim, err := ClaimWorkOrder(ctx, st, crashed.ID, plane.claim("crashed-"+core.NewTaskID()))
			requireOK(t, err)
			// The crashed launcher's last observable act was an activity
			// snapshot; it then died without release or capture.
			if err = st.UpsertWorkOrderActivitySnapshot(ctx, crashed.ID, plane.identity(crashedClaim.SessionID), "tail before crash"); err != nil && !errors.Is(err, store.ErrWorkOrderClaimLost) {
				t.Fatal(err)
			}
			_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
			requireOK(t, err)
			recoveredControl, err := st.GetWorkOrder(ctx, control.ID)
			requireOK(t, err)
			recoveredCrash, err := st.GetWorkOrder(ctx, crashed.ID)
			requireOK(t, err)
			type recoveredShape struct {
				State                  core.WorkOrderState
				Claimable              bool
				RetrySuppressed        bool
				RetrySuppressionReason string
				LastAttemptOutcome     string
				LastFailureMessage     string
				AutomaticRetryCount    int
				NextRetryZero          bool
				ActiveAttempt          string
				Session                string
			}
			shape := func(o core.WorkOrder) recoveredShape {
				return recoveredShape{o.State, o.Claimable, o.RetrySuppressed, o.RetrySuppressionReason, o.LastAttemptOutcome, o.LastFailureMessage, o.AutomaticRetryCount, o.NextRetryAt.IsZero(), o.AttemptID, o.SessionID}
			}
			if shape(recoveredCrash) != shape(recoveredControl) || recoveredCrash.LastAttemptOutcome != core.WorkOrderOutcomeExpired ||
				recoveredCrash.LastAttemptID != crashedClaim.AttemptID || recoveredControl.LastAttemptID != controlClaim.AttemptID {
				t.Fatalf("crashed recovery %+v differs from control %+v", recoveredCrash, recoveredControl)
			}
			requireAttemptCaptures(t, x, crashed.ID)
			requireAttemptCaptures(t, x, control.ID)
			// A late capture for the crashed attempt is never accepted.
			_, err = st.RecordWorkOrderAttemptCapture(ctx, crashed.ID, plane.identity(crashedClaim.SessionID), captureOf(crashedClaim, "", "fabricated after crash"))
			requireCaptureError(t, "crashed attempt after expiry", err, store.ErrAttemptCaptureUnverified)
			// Operator recovery and a fresh claim behave exactly as for the
			// control order, and still fabricate no capture.
			for _, order := range []struct {
				id      string
				attempt string
			}{{control.ID, controlClaim.AttemptID}, {crashed.ID, crashedClaim.AttemptID}} {
				recovered, err := RecoverWorkOrder(ctx, st, order.id, "crash-recover-"+core.NewTaskID(), time.Hour)
				requireOK(t, err)
				if recovered.State != core.WorkOrderQueued {
					t.Fatalf("recovery state=%s", recovered.State)
				}
				fresh, err := ClaimWorkOrder(ctx, st, order.id, core.WorkOrderClaim{WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: "fresh-" + core.NewTaskID(), ClientToken: "fresh", Lease: time.Hour, ExecutionTimeout: time.Hour})
				requireOK(t, err)
				if fresh.AttemptID == "" || fresh.AttemptID == order.attempt {
					t.Fatalf("fresh claim attempt=%q (crashed %q)", fresh.AttemptID, order.attempt)
				}
				requireAttemptCaptures(t, x, order.id)
			}
		})
	}
}

func attemptEndings(stage core.Stage) []attemptEnding {
	release := func(name, outcome, reason string) attemptEnding {
		return attemptEnding{name: name, end: func(t *testing.T, x Fixture, claimed core.WorkOrder) string {
			_, err := ReleaseWorkerClaim(x.Context, x.Backend, claimed.ID, captureWorker, core.WorkOrderRelease{
				SessionID: claimed.SessionID, Outcome: outcome, Reason: reason,
				InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
			})
			requireOK(t, err)
			if reason == "" {
				return outcome
			}
			return reason
		}}
	}
	return []attemptEnding{
		{name: "SuccessfulHandoff", end: func(t *testing.T, x Fixture, claimed core.WorkOrder) string {
			return handOffAttempt(t, x, claimed)
		}},
		release("ChildFailureRelease", core.WorkOrderOutcomeChildFailure, "harness exited with status 1"),
		release("StallRelease", core.WorkOrderOutcomeStalled, "no output for 10m0s"),
		release("CheckpointRelease", core.WorkOrderOutcomeReleased, core.WorkOrderReleaseReasonOperatorCheckpointReached),
		release("WorkerShutdownRelease", core.WorkOrderOutcomeCancelled, "worker shutting down"),
		release("ReleaseWithoutReason", core.WorkOrderOutcomeReleased, ""),
		{name: "Preemption", end: func(t *testing.T, x Fixture, claimed core.WorkOrder) string {
			result, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, claimed.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
				return x.Backend.PreemptWorkOrderCommand(x.Context, lease, store.WorkOrderPreemptRequest{WorkOrderID: claimed.ID, RequestID: "capture-preempt-" + core.NewTaskID(), Reason: "capture preempt"})
			})
			requireOK(t, err)
			if result.RevokedAttemptID != claimed.AttemptID {
				t.Fatalf("preempt revoked %q, want %q", result.RevokedAttemptID, claimed.AttemptID)
			}
			return core.WorkOrderOutcomePreempted
		}},
	}
}

// newCaptureOrder creates a queued order for stage in a fresh task.
func newCaptureOrder(t *testing.T, x Fixture, stage core.Stage) core.WorkOrder {
	t.Helper()
	if stage != core.StageReview {
		return newAggregateOrder(t, x, stage)
	}
	task := newAggregateTask(t, x)
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(x.Context, x.Backend, task.ID, jobs, orders))
	order, err := x.Backend.GetWorkOrder(x.Context, orders[0].ID)
	requireOK(t, err)
	return order
}

func claimForCapture(t *testing.T, x Fixture, orderID, session string) core.WorkOrder {
	t.Helper()
	claimed, err := ClaimWorkOrder(x.Context, x.Backend, orderID, core.WorkOrderClaim{
		WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: session, ClientToken: session,
		Lease: time.Hour, ExecutionTimeout: time.Hour,
	})
	requireOK(t, err)
	if claimed.State != core.WorkOrderClaimed || claimed.AttemptID == "" {
		t.Fatalf("claim did not establish an attempt: %+v", claimed)
	}
	return claimed
}

// handOffAttempt completes the attempt through its stage's terminal handoff.
func handOffAttempt(t *testing.T, x Fixture, claimed core.WorkOrder) string {
	t.Helper()
	if claimed.Stage == core.StageImplement {
		_, err := submitImplementation(x, core.WorkOrderCmdSubmitForReview, claimed, "capture-head")
		requireOK(t, err)
		return core.AttemptHandoffTerminationReason(core.WorkOrderSubmitted)
	}
	command := map[core.Stage]core.WorkOrderCommand{
		core.StageSpec: core.WorkOrderCmdSubmitSpec, core.StageReview: core.WorkOrderCmdSubmitReviewVerdict,
		core.StageVerify: core.WorkOrderCmdSubmitVerification,
	}[claimed.Stage]
	claimed.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, claimed, command))
	return core.AttemptHandoffTerminationReason(core.WorkOrderCompleted)
}

func workerCaptureIdentity(session string) core.WorkOrderClaimIdentity {
	return core.WorkOrderClaimIdentity{WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: session}
}

func captureOf(claimed core.WorkOrder, reason, content string) core.WorkOrderAttemptCapture {
	return core.WorkOrderAttemptCapture{
		SessionID: claimed.SessionID, AttemptID: claimed.AttemptID, TerminationReason: reason,
		Transcript: &core.WorkOrderAttemptTranscript{Content: content, Truncated: true},
	}
}

func requireCaptureError(t *testing.T, label string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err=%v, want %v", label, err, want)
	}
}

func requireAttemptCaptures(t *testing.T, x Fixture, orderID string, want ...core.WorkOrderTranscriptCapture) {
	t.Helper()
	captures, err := x.Backend.ListWorkOrderTranscriptCaptures(x.Context, orderID)
	requireOK(t, err)
	if len(captures) != len(want) {
		t.Fatalf("captures=%+v, want %d", captures, len(want))
	}
	for i := range want {
		got := captures[i]
		if got.AttemptID != want[i].AttemptID || got.Content != want[i].Content || got.TerminationReason != want[i].TerminationReason || got.Truncated != want[i].Truncated || got.CapturedAt.IsZero() {
			t.Fatalf("capture %d=%+v, want %+v", i, got, want[i])
		}
	}
}

func taskEventCount(t *testing.T, x Fixture, taskID string) int {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	return len(events)
}

// requireCaptureAbsentFromExecutionReads proves captured content never
// reaches get_work_order, list, claim, or task-context inputs.
func requireCaptureAbsentFromExecutionReads(t *testing.T, x Fixture, order core.WorkOrder, content string, claimed ...core.WorkOrder) {
	t.Helper()
	current, err := x.Backend.GetWorkOrder(x.Context, order.ID)
	requireOK(t, err)
	snapshot, err := x.Backend.ListTaskWorkOrdersSnapshot(x.Context, order.TaskID)
	requireOK(t, err)
	listed, err := x.Backend.ListWorkOrders(x.Context)
	requireOK(t, err)
	taskContext, err := store.TaskContextForTask(x.Context, x.Backend, order.TaskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, order.TaskID)
	requireOK(t, err)
	for label, value := range map[string]any{"get_work_order": current, "task_work_orders": snapshot, "list_work_orders": listed, "task_context": taskContext, "events": events, "claim": claimed} {
		encoded, marshalErr := json.Marshal(value)
		requireOK(t, marshalErr)
		if strings.Contains(string(encoded), content) || strings.Contains(string(encoded), "transcript_captures") || strings.Contains(string(encoded), "activity_snapshot") {
			t.Fatalf("%s exposes observational capture data: %s", label, encoded)
		}
	}
}

func runAttemptEndingCapture(t *testing.T, x Fixture, stage core.Stage, ending attemptEnding) {
	st, ctx := x.Backend, x.Context
	order := newCaptureOrder(t, x, stage)
	claimed := claimForCapture(t, x, order.ID, "ending-"+core.NewTaskID())
	identity := workerCaptureIdentity(claimed.SessionID)
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "running tail"))
	reason := ending.end(t, x, claimed)
	content := "ending transcript " + core.NewTaskID()
	events := taskEventCount(t, x, order.TaskID)
	before, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)

	// A conflicting declaration is refused without any write.
	_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "invented reason", content))
	requireCaptureError(t, "conflicting reason", err, store.ErrAttemptCaptureReasonConflict)
	requireAttemptCaptures(t, x, order.ID)
	if current, found, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, order.ID); snapshotErr != nil || !found || current.AttemptID != claimed.AttemptID {
		t.Fatalf("refused capture changed the snapshot: %+v found=%v err=%v", current, found, snapshotErr)
	}

	result, err := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "  "+reason+"  ", content))
	requireOK(t, err)
	if !result.Created || result.TerminationReason != reason {
		t.Fatalf("capture result=%+v, want created with %q", result, reason)
	}
	want := core.WorkOrderTranscriptCapture{AttemptID: claimed.AttemptID, Content: content, TerminationReason: reason, Truncated: true}
	requireAttemptCaptures(t, x, order.ID, want)
	if _, found, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, order.ID); snapshotErr != nil || found {
		t.Fatalf("ended attempt snapshot survived capture: found=%v err=%v", found, snapshotErr)
	}

	// A duplicate delivery, with or without a declared reason, never
	// rewrites content or reason; an undeclared reason uses the derived one.
	for _, declared := range []string{reason, ""} {
		duplicate, dupErr := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, declared, "replacement content"))
		requireOK(t, dupErr)
		if duplicate.Created || duplicate.TerminationReason != reason {
			t.Fatalf("duplicate result=%+v", duplicate)
		}
	}
	_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "rewritten reason", "replacement content"))
	requireCaptureError(t, "duplicate with conflicting reason", err, store.ErrAttemptCaptureReasonConflict)
	withoutTranscript := captureOf(claimed, reason, "")
	withoutTranscript.Transcript = nil
	if again, againErr := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, withoutTranscript); againErr != nil || again.Created || again.TerminationReason != reason {
		t.Fatalf("transcript-free duplicate=%+v err=%v", again, againErr)
	}
	requireAttemptCaptures(t, x, order.ID, want)

	// Capture is observational: no lifecycle event or order field changes.
	after, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if got := taskEventCount(t, x, order.TaskID); got != events {
		t.Fatalf("capture appended %d task events", got-events)
	}
	if after.State != before.State || after.AttemptID != before.AttemptID || after.SessionID != before.SessionID ||
		after.LastAttemptID != before.LastAttemptID || after.LastAttemptOutcome != before.LastAttemptOutcome ||
		after.LastFailureMessage != before.LastFailureMessage || after.AutomaticRetryCount != before.AutomaticRetryCount ||
		after.RetrySuppressed != before.RetrySuppressed {
		t.Fatalf("capture changed lifecycle state: before=%+v after=%+v", before, after)
	}
	requireCaptureAbsentFromExecutionReads(t, x, order, content)
}

func runDelayedAttemptCapture(t *testing.T, x Fixture, stage core.Stage) {
	st, ctx := x.Backend, x.Context
	order := newCaptureOrder(t, x, stage)
	first := claimForCapture(t, x, order.ID, "first-"+core.NewTaskID())
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, workerCaptureIdentity(first.SessionID), "first tail"))
	_, err := ReleaseWorkerClaim(ctx, st, order.ID, captureWorker, core.WorkOrderRelease{
		SessionID: first.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "first attempt failed",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
	})
	requireOK(t, err)
	successor := claimForCapture(t, x, order.ID, "successor-"+core.NewTaskID())
	if successor.AttemptID == first.AttemptID {
		t.Fatal("successor reused the predecessor attempt")
	}
	successorIdentity := workerCaptureIdentity(successor.SessionID)
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, successorIdentity, "successor tail"))

	content := "delayed predecessor transcript " + core.NewTaskID()
	result, err := st.RecordWorkOrderAttemptCapture(ctx, order.ID, workerCaptureIdentity(first.SessionID), captureOf(first, "first attempt failed", content))
	requireOK(t, err)
	if !result.Created || result.TerminationReason != "first attempt failed" {
		t.Fatalf("delayed capture=%+v", result)
	}
	snapshot, found, err := st.GetWorkOrderActivitySnapshot(ctx, order.ID)
	requireOK(t, err)
	if !found || snapshot.AttemptID != successor.AttemptID || snapshot.Content != "successor tail" {
		t.Fatalf("delayed predecessor capture disturbed the successor snapshot: %+v found=%v", snapshot, found)
	}
	current, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if current.State != core.WorkOrderClaimed || current.AttemptID != successor.AttemptID || current.SessionID != successor.SessionID ||
		current.WorkerID != captureWorker || !current.LeaseExpiresAt.Equal(successor.LeaseExpiresAt) {
		t.Fatalf("delayed capture changed successor ownership: %+v", current)
	}
	// The successor's own still-active attempt cannot be captured, and its
	// claim session cannot capture the predecessor attempt.
	_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, successorIdentity, captureOf(successor, "", "premature"))
	requireCaptureError(t, "active successor", err, store.ErrAttemptCaptureUnverified)
	crossed := captureOf(first, "", "crossed")
	crossed.SessionID = successor.SessionID
	_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, successorIdentity, crossed)
	requireCaptureError(t, "successor session naming predecessor attempt", err, store.ErrWorkOrderClaimUnauthorized)
	requireAttemptCaptures(t, x, order.ID, core.WorkOrderTranscriptCapture{AttemptID: first.AttemptID, Content: content, TerminationReason: "first attempt failed", Truncated: true})
	requireCaptureAbsentFromExecutionReads(t, x, order, content, successor)
}

func runAttemptCaptureIdentityRefusals(t *testing.T, x Fixture, stage core.Stage) {
	st, ctx := x.Backend, x.Context
	order := newCaptureOrder(t, x, stage)
	claimed := claimForCapture(t, x, order.ID, "identity-"+core.NewTaskID())
	identity := workerCaptureIdentity(claimed.SessionID)
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "identity tail"))
	_, err := ReleaseWorkerClaim(ctx, st, order.ID, captureWorker, core.WorkOrderRelease{
		SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeStalled, Reason: "stalled",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
	})
	requireOK(t, err)
	foreign := x.Workspace + "-capture-other"
	foreignCtx := store.WithWorkspace(ctx, foreign)
	_, err = st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	valid := captureOf(claimed, "stalled", "identity transcript")
	for _, tc := range []struct {
		name    string
		input   func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture)
		foreign bool
	}{
		{name: "wrong worker", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, core.WorkOrderClaimIdentity{WorkerID: "other-worker", ClaimantID: captureWorker, SessionID: claimed.SessionID}, valid
		}},
		{name: "wrong claimant", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, core.WorkOrderClaimIdentity{WorkerID: captureWorker, ClaimantID: "other-worker", SessionID: claimed.SessionID}, valid
		}},
		{name: "other worker entirely", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, core.WorkOrderClaimIdentity{WorkerID: "other-worker", ClaimantID: "other-worker", SessionID: claimed.SessionID}, valid
		}},
		{name: "run plane identity", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-capture"), SessionID: claimed.SessionID}, valid
		}},
		{name: "empty identity", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, core.WorkOrderClaimIdentity{}, valid
		}},
		{name: "wrong session", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			capture := valid
			capture.SessionID = "other-session"
			return order.ID, workerCaptureIdentity("other-session"), capture
		}},
		{name: "claim session differs from capture", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, workerCaptureIdentity("other-session"), valid
		}},
		{name: "wrong attempt", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			capture := valid
			capture.AttemptID = "att-unknown"
			return order.ID, identity, capture
		}},
		{name: "missing attempt", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			capture := valid
			capture.AttemptID = " "
			return order.ID, identity, capture
		}},
		{name: "other order", input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return "missing-" + core.NewTaskID(), identity, valid
		}},
		{name: "other workspace", foreign: true, input: func() (string, core.WorkOrderClaimIdentity, core.WorkOrderAttemptCapture) {
			return order.ID, identity, valid
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, claim, capture := tc.input()
			callCtx := ctx
			if tc.foreign {
				callCtx = foreignCtx
			}
			_, err := st.RecordWorkOrderAttemptCapture(callCtx, id, claim, capture)
			requireCaptureError(t, tc.name, err, store.ErrWorkOrderClaimUnauthorized)
			requireAttemptCaptures(t, x, order.ID)
			if snapshot, found, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, order.ID); snapshotErr != nil || !found || snapshot.AttemptID != claimed.AttemptID {
				t.Fatalf("refused capture removed the snapshot: %+v found=%v err=%v", snapshot, found, snapshotErr)
			}
		})
	}
	result, err := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, valid)
	requireOK(t, err)
	if !result.Created {
		t.Fatal("authentic capture was not recorded after refusals")
	}
}

func runAttemptCaptureUnverified(t *testing.T, x Fixture, stage core.Stage) {
	st, ctx := x.Backend, x.Context
	tick := func(t *testing.T) {
		t.Helper()
		_, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
	}
	refused := func(t *testing.T, order, claimed core.WorkOrder, label string) {
		t.Helper()
		identity := workerCaptureIdentity(claimed.SessionID)
		_, err := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "", "unverified "+label))
		requireCaptureError(t, label, err, store.ErrAttemptCaptureUnverified)
		_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, core.WorkOrderOutcomeExpired, "unverified "+label))
		requireCaptureError(t, label+" with declared reason", err, store.ErrAttemptCaptureUnverified)
		requireAttemptCaptures(t, x, order.ID)
	}

	// Still active: the attempt has no ending yet.
	active := newCaptureOrder(t, x, stage)
	activeClaim := claimForCapture(t, x, active.ID, "active-"+core.NewTaskID())
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, active.ID, workerCaptureIdentity(activeClaim.SessionID), "live tail"))
	refused(t, active, activeClaim, "still active")
	if snapshot, found, err := st.GetWorkOrderActivitySnapshot(ctx, active.ID); err != nil || !found || snapshot.Content != "live tail" {
		t.Fatalf("refused capture removed the live snapshot: %+v found=%v err=%v", snapshot, found, err)
	}

	// Unmediated lease expiry, after an earlier release left a message the
	// expired attempt must not inherit (AC-2.3).
	expired := newCaptureOrder(t, x, stage)
	earlier := claimForCapture(t, x, expired.ID, "earlier-"+core.NewTaskID())
	_, err := ReleaseWorkerClaim(ctx, st, expired.ID, captureWorker, core.WorkOrderRelease{
		SessionID: earlier.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "earlier failure",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
	})
	requireOK(t, err)
	lapsed, err := ClaimWorkOrder(ctx, st, expired.ID, core.WorkOrderClaim{
		WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: "lapsed-" + core.NewTaskID(), ClientToken: "lapsed",
		Lease: time.Nanosecond, ExecutionTimeout: time.Hour,
	})
	requireOK(t, err)
	refused(t, expired, lapsed, "lease lapsed before recovery")
	tick(t)
	recovered, err := st.GetWorkOrder(ctx, expired.ID)
	requireOK(t, err)
	if recovered.LastAttemptID != lapsed.AttemptID || recovered.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
		t.Fatalf("lease expiry fixture=%+v", recovered)
	}
	refused(t, expired, lapsed, "expired lease")

	// Execution-clock timeout and task cancellation leave an earlier
	// attempt's reason on the row; neither yields a synthetic capture.
	timedOut := newCaptureOrder(t, x, stage)
	timedClaim, err := ClaimWorkOrder(ctx, st, timedOut.ID, core.WorkOrderClaim{
		WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: "timeout-" + core.NewTaskID(), ClientToken: "timeout",
		Lease: time.Hour, ExecutionTimeout: time.Nanosecond,
	})
	requireOK(t, err)
	tick(t)
	refused(t, timedOut, timedClaim, "execution timeout")

	cancelled := newCaptureOrder(t, x, stage)
	cancelClaim := claimForCapture(t, x, cancelled.ID, "cancel-"+core.NewTaskID())
	_, err = taskops.New(st).Cancel(ctx, core.Intervention{TaskID: cancelled.TaskID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	requireOK(t, err)
	refused(t, cancelled, cancelClaim, "task cancellation")
}

func runRunPlaneAttemptCapture(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newCaptureOrder(t, x, core.StageImplement)
	claimant := core.TaskRunClaimantID("usr-capture")
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{
		ClaimantID: claimant, OwnerUserID: "usr-capture", SessionID: "run-" + core.NewTaskID(), ClientToken: "run",
		Lease: time.Hour, ExecutionTimeout: time.Hour,
	})
	requireOK(t, err)
	identity := core.WorkOrderClaimIdentity{ClaimantID: claimant, SessionID: claimed.SessionID}
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "run tail"))
	_, err = taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdRelease, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		return st.ReleaseWorkerClaimCommand(ctx, lease, order.ID, identity, core.WorkOrderRelease{
			SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "run child failed",
			InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
		})
	})
	requireOK(t, err)
	for _, wrong := range []core.WorkOrderClaimIdentity{
		{ClaimantID: core.TaskRunClaimantID("usr-other"), SessionID: claimed.SessionID},
		{WorkerID: claimant, ClaimantID: claimant, SessionID: claimed.SessionID},
		{WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: claimed.SessionID},
	} {
		_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, wrong, captureOf(claimed, "", "run transcript"))
		requireCaptureError(t, "run plane "+wrong.ClaimantID, err, store.ErrWorkOrderClaimUnauthorized)
	}
	result, err := st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "", "run transcript"))
	requireOK(t, err)
	if !result.Created || result.TerminationReason != "run child failed" {
		t.Fatalf("run-plane capture=%+v", result)
	}
	if _, found, err := st.GetWorkOrderActivitySnapshot(ctx, order.ID); err != nil || found {
		t.Fatalf("run-plane capture left the ended snapshot: found=%v err=%v", found, err)
	}
	requireAttemptCaptures(t, x, order.ID, core.WorkOrderTranscriptCapture{AttemptID: claimed.AttemptID, Content: "run transcript", TerminationReason: "run child failed", Truncated: true})
}

// runPlanRevisionAttemptCapture covers the implement-only agent
// self-release through request_plan_revision.
func runPlanRevisionAttemptCapture(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	fixture := newPlanRevisionFixture(t, st, ctx, core.StageImplement, true, false)
	claimed := claimForCapture(t, x, fixture.order.ID, "revision-"+core.NewTaskID())
	identity := workerCaptureIdentity(claimed.SessionID)
	_, err := requestPlanRevision(ctx, st, claimed.TaskID, claimed.ID, captureWorker, claimed.SessionID, "the API moved")
	requireOK(t, err)
	result, err := st.RecordWorkOrderAttemptCapture(ctx, claimed.ID, identity, captureOf(claimed, core.WorkOrderReleaseReasonPlanRevisionRequested, "plan revision transcript"))
	requireOK(t, err)
	if !result.Created || result.TerminationReason != core.WorkOrderReleaseReasonPlanRevisionRequested {
		t.Fatalf("plan revision capture=%+v", result)
	}
	requireAttemptCaptures(t, x, claimed.ID, core.WorkOrderTranscriptCapture{AttemptID: claimed.AttemptID, Content: "plan revision transcript", TerminationReason: core.WorkOrderReleaseReasonPlanRevisionRequested, Truncated: true})
}
