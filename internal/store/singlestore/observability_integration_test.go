package singlestore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

const captureIntegrationWorker = "s2-capture-worker"

// endedAttemptForCapture claims a fresh implementation order, records its
// live-tail snapshot, and ends the attempt through a child-failure release.
func endedAttemptForCapture(t *testing.T, st *Store, ctx context.Context, workspace string) core.WorkOrder {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	session := "s2-capture-" + task.ID
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{WorkerID: captureIntegrationWorker, ClaimantID: captureIntegrationWorker, SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertWorkOrderActivitySnapshot(ctx, job.ID, captureIntegrationIdentity(session), "predecessor tail"); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, captureIntegrationWorker, core.WorkOrderRelease{
		SessionID: session, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "predecessor failed",
		InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond,
	}); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func captureIntegrationIdentity(session string) core.WorkOrderClaimIdentity {
	return core.WorkOrderClaimIdentity{WorkerID: captureIntegrationWorker, ClaimantID: captureIntegrationWorker, SessionID: session}
}

func captureIntegrationInput(claimed core.WorkOrder, content string) core.WorkOrderAttemptCapture {
	return core.WorkOrderAttemptCapture{SessionID: claimed.SessionID, AttemptID: claimed.AttemptID, TerminationReason: "predecessor failed",
		Transcript: &core.WorkOrderAttemptTranscript{Content: content}}
}

// TestAttemptObservabilityConcurrentCaptureIntegration proves the native
// SingleStore capture boundary (req-260820-221be8 AC-2.1; DEC-26;
// component-work-orders; component-persistence): racing deliveries insert one
// capture, a delayed capture racing a successor claim preserves successor
// ownership and its activity snapshot, and a failed insert rolls back the
// snapshot supersession with it.
func TestAttemptObservabilityConcurrentCaptureIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "attempt-capture-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}

	t.Run("DuplicateDeliveriesInsertOnce", func(t *testing.T) {
		claimed := endedAttemptForCapture(t, st, ctx, workspace)
		const deliveries = 8
		start := make(chan struct{})
		results := make([]core.WorkOrderAttemptCaptureResult, deliveries)
		errs := make([]error, deliveries)
		var wg sync.WaitGroup
		for i := range deliveries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = st.RecordWorkOrderAttemptCapture(ctx, claimed.ID, captureIntegrationIdentity(claimed.SessionID), captureIntegrationInput(claimed, fmt.Sprintf("delivery %d", i)))
			}()
		}
		close(start)
		wg.Wait()
		created := -1
		for i := range deliveries {
			if errs[i] != nil || results[i].TerminationReason != "predecessor failed" {
				t.Fatalf("delivery %d result=%+v err=%v", i, results[i], errs[i])
			}
			if results[i].Created {
				if created >= 0 {
					t.Fatalf("deliveries %d and %d both inserted", created, i)
				}
				created = i
			}
		}
		captures, err := st.ListWorkOrderTranscriptCaptures(ctx, claimed.ID)
		if err != nil || created < 0 || len(captures) != 1 || captures[0].Content != fmt.Sprintf("delivery %d", created) || captures[0].TerminationReason != "predecessor failed" {
			t.Fatalf("captures=%+v created=%d err=%v", captures, created, err)
		}
		if _, found, err := st.GetWorkOrderActivitySnapshot(ctx, claimed.ID); err != nil || found {
			t.Fatalf("ended snapshot survived: found=%v err=%v", found, err)
		}
	})

	t.Run("SuccessorClaimVersusDelayedCapture", func(t *testing.T) {
		for round := range 4 {
			predecessor := endedAttemptForCapture(t, st, ctx, workspace)
			successorSession := fmt.Sprintf("s2-successor-%d-%s", round, core.NewTaskID())
			start := make(chan struct{})
			var successor core.WorkOrder
			var claimErr, snapshotErr, captureErr error
			var captured core.WorkOrderAttemptCaptureResult
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				successor, claimErr = storetest.For(st).ClaimWorkOrder(ctx, predecessor.ID, core.WorkOrderClaim{WorkerID: captureIntegrationWorker, ClaimantID: captureIntegrationWorker, SessionID: successorSession, ClientToken: successorSession, Lease: time.Hour, ExecutionTimeout: time.Hour})
				if claimErr == nil {
					snapshotErr = st.UpsertWorkOrderActivitySnapshot(ctx, predecessor.ID, captureIntegrationIdentity(successorSession), "successor tail")
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				captured, captureErr = st.RecordWorkOrderAttemptCapture(ctx, predecessor.ID, captureIntegrationIdentity(predecessor.SessionID), captureIntegrationInput(predecessor, "predecessor transcript"))
			}()
			close(start)
			wg.Wait()
			if claimErr != nil || snapshotErr != nil || captureErr != nil {
				t.Fatalf("round %d claim=%v snapshot=%v capture=%v", round, claimErr, snapshotErr, captureErr)
			}
			if !captured.Created || captured.TerminationReason != "predecessor failed" {
				t.Fatalf("round %d capture=%+v", round, captured)
			}
			current, err := st.GetWorkOrder(ctx, predecessor.ID)
			if err != nil || current.State != core.WorkOrderClaimed || current.AttemptID != successor.AttemptID || current.SessionID != successorSession || current.AttemptID == predecessor.AttemptID {
				t.Fatalf("round %d successor ownership=%+v err=%v", round, current, err)
			}
			snapshot, found, err := st.GetWorkOrderActivitySnapshot(ctx, predecessor.ID)
			if err != nil || !found || snapshot.AttemptID != successor.AttemptID || snapshot.Content != "successor tail" {
				t.Fatalf("round %d successor snapshot=%+v found=%v err=%v", round, snapshot, found, err)
			}
			captures, err := st.ListWorkOrderTranscriptCaptures(ctx, predecessor.ID)
			if err != nil || len(captures) != 1 || captures[0].AttemptID != predecessor.AttemptID {
				t.Fatalf("round %d captures=%+v err=%v", round, captures, err)
			}
		}
	})

	t.Run("ForcedRollbackLeavesNoPartialCapture", func(t *testing.T) {
		claimed := endedAttemptForCapture(t, st, ctx, workspace)
		injected := errors.New("attempt capture injected failure")
		attemptCaptureInsertFault = func() error { return injected }
		t.Cleanup(func() { attemptCaptureInsertFault = nil })
		_, err := st.RecordWorkOrderAttemptCapture(ctx, claimed.ID, captureIntegrationIdentity(claimed.SessionID), captureIntegrationInput(claimed, "rolled back"))
		if !errors.Is(err, injected) {
			t.Fatalf("injected failure err=%v", err)
		}
		if captures, listErr := st.ListWorkOrderTranscriptCaptures(ctx, claimed.ID); listErr != nil || len(captures) != 0 {
			t.Fatalf("partial capture=%+v err=%v", captures, listErr)
		}
		if snapshot, found, snapshotErr := st.GetWorkOrderActivitySnapshot(ctx, claimed.ID); snapshotErr != nil || !found || snapshot.AttemptID != claimed.AttemptID {
			t.Fatalf("snapshot supersession escaped rollback: %+v found=%v err=%v", snapshot, found, snapshotErr)
		}
		attemptCaptureInsertFault = nil
		result, err := st.RecordWorkOrderAttemptCapture(ctx, claimed.ID, captureIntegrationIdentity(claimed.SessionID), captureIntegrationInput(claimed, "after recovery"))
		if err != nil || !result.Created {
			t.Fatalf("retry after rollback result=%+v err=%v", result, err)
		}
	})
}
