package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// endedCaptureAttempt claims a fresh stage order with worker and ends that
// attempt through a child-failure release, leaving a live-tail snapshot.
func endedCaptureAttempt(t *testing.T, st store.Store, ctx context.Context, worker core.Worker, stage core.Stage, release bool) core.WorkOrder {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: core.NewTaskID(), Workspace: "demo", State: core.TaskRunning, NextStage: stage, CreatedAt: now}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA, task.SetupContract.VerifyStage = "submitted-head", true
	}
	job := core.Job{ID: task.ID + "-" + string(stage) + "-1", TaskID: task.ID, Stage: stage, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	session := "session-" + task.ID
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertWorkOrderActivitySnapshot(ctx, job.ID, core.WorkOrderClaimIdentity{WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: session}, "live tail"); err != nil {
		t.Fatal(err)
	}
	if release {
		if _, err = storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, worker.ID, core.WorkOrderRelease{
			SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited with status 2",
			InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return claimed
}

// TestAttemptObservabilityRedactionBoundAndFailure proves the worker service
// redacts known credentials and secret patterns, keeps the newest
// AttemptTranscriptLimit bytes at a rune boundary, fails closed when the
// secret source fails, and never changes the attempt's lifecycle outcome
// (req-260820-221be8 AC-2.1, AC-2.2; DEC-26; component-work-orders).
func TestAttemptObservabilityRedactionBoundAndFailure(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	seedWorkerOwners(t, st)
	worker := core.Worker{ID: "capture-worker", Workspace: "demo"}
	exact := "stored-app-key-value-0123456789"
	pattern := "ghp_" + strings.Repeat("A1b2", 9)
	service := &Service{Store: st, RedactionSecrets: heartbeatSecretSource{secrets: []string{exact}}}
	capture := func(order core.WorkOrder, reason, content string, truncated bool) core.WorkOrderAttemptCapture {
		return core.WorkOrderAttemptCapture{SessionID: " " + order.SessionID + " ", AttemptID: " " + order.AttemptID + " ", TerminationReason: reason,
			Transcript: &core.WorkOrderAttemptTranscript{Content: content, Truncated: truncated}}
	}
	single := func(t *testing.T, orderID string) core.WorkOrderTranscriptCapture {
		t.Helper()
		captures, err := st.ListWorkOrderTranscriptCaptures(ctx, orderID)
		if err != nil || len(captures) != 1 {
			t.Fatalf("captures=%+v err=%v", captures, err)
		}
		return captures[0]
	}

	t.Run("RedactsBeforePersistence", func(t *testing.T) {
		for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
			order := endedCaptureAttempt(t, st, ctx, worker, stage, true)
			result, err := service.CaptureWorkerAttempt(ctx, worker, order.ID, capture(order, "", "auth "+pattern+" and key "+exact+" done", false))
			if err != nil || !result.Created || result.TerminationReason != "harness exited with status 2" {
				t.Fatalf("%s: result=%+v err=%v", stage, result, err)
			}
			got := single(t, order.ID)
			if strings.Contains(got.Content, pattern) || strings.Contains(got.Content, exact) || !strings.Contains(got.Content, "[REDACTED:") ||
				!strings.HasPrefix(got.Content, "auth ") || !strings.HasSuffix(got.Content, " done") || got.Truncated {
				t.Fatalf("%s: persisted capture=%+v", stage, got)
			}
		}
	})

	t.Run("RetainsNewestBoundedTailAtRuneBoundary", func(t *testing.T) {
		order := endedCaptureAttempt(t, st, ctx, worker, core.StageImplement, true)
		newest := "\nnewest line"
		body := strings.Repeat("y", AttemptTranscriptLimit-1-len(newest)) + newest
		// "€" is three bytes; the newest-limit cut lands on its final byte.
		content := strings.Repeat("old ", 64) + "€" + body
		if _, err := service.CaptureWorkerAttempt(ctx, worker, order.ID, capture(order, "harness exited with status 2", content, false)); err != nil {
			t.Fatal(err)
		}
		got := single(t, order.ID)
		if !got.Truncated || len(got.Content) > AttemptTranscriptLimit || !utf8.ValidString(got.Content) || got.Content != body {
			t.Fatalf("bounded capture length=%d truncated=%v valid=%v", len(got.Content), got.Truncated, utf8.ValidString(got.Content))
		}

		// A launcher-side truncation flag survives a short transcript.
		short := endedCaptureAttempt(t, st, ctx, worker, core.StageReview, true)
		if _, err := service.CaptureWorkerAttempt(ctx, worker, short.ID, capture(short, "", "spooled tail", true)); err != nil {
			t.Fatal(err)
		}
		if got := single(t, short.ID); got.Content != "spooled tail" || !got.Truncated {
			t.Fatalf("launcher truncation flag lost: %+v", got)
		}
	})

	t.Run("SecretSourceFailureDropsTranscript", func(t *testing.T) {
		failing := &Service{Store: st, RedactionSecrets: heartbeatSecretSource{err: errors.New("key store unavailable")}}
		order := endedCaptureAttempt(t, st, ctx, worker, core.StageVerify, true)
		before, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.ListEvents(ctx, order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := failing.CaptureWorkerAttempt(ctx, worker, order.ID, capture(order, "harness exited with status 2", "key "+exact, false))
		if err != nil || result.Created || result.TerminationReason != "harness exited with status 2" {
			t.Fatalf("fail-closed capture result=%+v err=%v", result, err)
		}
		if captures, listErr := st.ListWorkOrderTranscriptCaptures(ctx, order.ID); listErr != nil || len(captures) != 0 {
			t.Fatalf("unredacted transcript persisted: %+v err=%v", captures, listErr)
		}
		if _, found, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, order.ID); snapshotErr != nil || found {
			t.Fatalf("ended attempt snapshot survived: found=%v err=%v", found, snapshotErr)
		}
		after, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil {
			t.Fatal(err)
		}
		afterEvents, err := st.ListEvents(ctx, order.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != before.State || after.LastAttemptID != before.LastAttemptID || after.LastAttemptOutcome != before.LastAttemptOutcome ||
			after.LastFailureMessage != before.LastFailureMessage || after.AutomaticRetryCount != before.AutomaticRetryCount || len(afterEvents) != len(events) {
			t.Fatalf("failed redaction changed the outcome: before=%+v after=%+v events %d→%d", before, after, len(events), len(afterEvents))
		}
	})

	t.Run("ImpossibleCapturePersistsNothing", func(t *testing.T) {
		active := endedCaptureAttempt(t, st, ctx, worker, core.StageSpec, false)
		if _, err := service.CaptureWorkerAttempt(ctx, worker, active.ID, capture(active, "", "premature", false)); !errors.Is(err, store.ErrAttemptCaptureUnverified) {
			t.Fatalf("active attempt err=%v", err)
		}
		if _, err := service.CaptureWorkerAttempt(ctx, core.Worker{ID: "other-worker", Workspace: "demo"}, active.ID, capture(active, "", "foreign", false)); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
			t.Fatalf("foreign worker err=%v", err)
		}
		for _, missing := range []core.WorkOrderAttemptCapture{{AttemptID: active.AttemptID}, {SessionID: active.SessionID}, {SessionID: " ", AttemptID: " "}} {
			if _, err := service.CaptureWorkerAttempt(ctx, worker, active.ID, missing); err == nil || err.Error() != "session_id and attempt_id are required" {
				t.Fatalf("missing identity err=%v", err)
			}
		}
		if captures, err := st.ListWorkOrderTranscriptCaptures(ctx, active.ID); err != nil || len(captures) != 0 {
			t.Fatalf("refused captures persisted: %+v err=%v", captures, err)
		}
		if snapshot, found, err := st.GetWorkOrderActivitySnapshot(ctx, active.ID); err != nil || !found || snapshot.Content != "live tail" {
			t.Fatalf("refused capture changed the live snapshot: %+v found=%v err=%v", snapshot, found, err)
		}
		current, err := st.GetWorkOrder(ctx, active.ID)
		if err != nil || current.State != core.WorkOrderClaimed || current.AttemptID != active.AttemptID {
			t.Fatalf("refused capture changed the live claim: %+v err=%v", current, err)
		}
	})

	t.Run("RunPlaneIdentity", func(t *testing.T) {
		order := endedCaptureAttempt(t, st, ctx, worker, core.StageImplement, true)
		run := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID("usr-capture"), SessionID: order.SessionID}
		if _, err := service.CaptureAttempt(ctx, run, order.ID, capture(order, "", "run", false)); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
			t.Fatalf("run identity on a worker claim err=%v", err)
		}
	})
}
