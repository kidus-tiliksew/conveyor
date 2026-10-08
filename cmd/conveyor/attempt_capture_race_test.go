package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// captureFIFO is a named pipe the helper child blocks on. release opens it for
// writing, which returns once the child has opened it for reading, and closes
// it so the child continues. It orders events without timers.
type captureFIFO string

func newCaptureFIFO(t *testing.T, env string) captureFIFO {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signal.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, path)
	return captureFIFO(path)
}

func (f captureFIFO) release(t *testing.T) {
	pipe, err := os.OpenFile(string(f), os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	_ = pipe.Close()
}

// raceHooks installs the launcher boundary hooks for one test and restores
// them afterwards.
func raceHooks(t *testing.T) (childWaited chan struct{}, activity chan struct{}) {
	t.Helper()
	childWaited = make(chan struct{}, 1)
	activity = make(chan struct{}, 64)
	workerChildWaitedTestHook = func() { childWaited <- struct{}{} }
	workerActivityObservedTestHook = func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		workerChildWaitedTestHook, workerActivityObservedTestHook = nil, nil
		workerFirstActivityDeadlineTestHook, workerStallDeadlineTestHook = nil, nil
	})
	return childWaited, activity
}

func assertSingleCapture(t *testing.T, fixture *captureFixture, reason string) core.WorkOrderAttemptCapture {
	t.Helper()
	releases, captures, _, session := fixture.snapshot()
	if len(captures) != 1 {
		t.Fatalf("captures=%+v want exactly one", captures)
	}
	capture := captures[0]
	if capture.AttemptID != captureFixtureAttempt || capture.SessionID != session || capture.TerminationReason != reason {
		t.Fatalf("capture=%+v session=%q want reason %q", capture, session, reason)
	}
	if len(releases) > 1 {
		t.Fatalf("duplicate releases: %+v", releases)
	}
	for _, release := range releases {
		if reason != "" && release.Reason != reason {
			t.Fatalf("release reason %q differs from capture reason %q", release.Reason, reason)
		}
	}
	return capture
}

// TestAttemptCaptureChildExitWinsFirstActivityTimeout selects the
// first-activity deadline, then makes the child exit and proves its status is
// queued before the launcher re-checks: the exit wins, the release carries the
// exit reason, and exactly one capture carries the same reason.
func TestAttemptCaptureChildExitWinsFirstActivityTimeout(t *testing.T) {
	childWaited, _ := raceHooks(t)
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = time.Hour
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	exit := newCaptureFIFO(t, "CONVEYOR_CAPTURE_FIFO")
	var once sync.Once
	workerFirstActivityDeadlineTestHook = func() {
		once.Do(func() {
			exit.release(t)
			<-childWaited
		})
	}
	for _, dispatch := range []string{"worker", "run"} {
		t.Run(dispatch, func(t *testing.T) {
			once = sync.Once{}
			fixture := &captureFixture{t: t, stage: core.StageReview, dispatch: dispatch}
			server := fixture.server()
			defer server.Close()
			var stdout, stderr lockedCaptureBuffer
			err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("capture-await-exit", ""), 50*time.Millisecond, &stdout, &stderr)
			const reason = "harness exited without terminal verdict submission"
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("err=%v stderr=%q", err, stderr.String())
			}
			releases, _, _, _ := fixture.snapshot()
			if len(releases) != 1 || releases[0].Outcome != core.WorkOrderOutcomeChildFailure {
				t.Fatalf("releases=%+v", releases)
			}
			assertSingleCapture(t, fixture, reason)
		})
	}
}

// TestAttemptCaptureOutputWinsStallDeadline selects the stall deadline, then
// makes the child emit output and proves the launcher observed it before the
// generation check: no stall release happens, the later child exit is the one
// ending, and exactly one capture carries its reason.
func TestAttemptCaptureOutputWinsStallDeadline(t *testing.T) {
	childWaited, activity := raceHooks(t)
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = time.Hour
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	emit := newCaptureFIFO(t, "CONVEYOR_CAPTURE_FIFO")
	finish := captureFIFO(filepath.Join(t.TempDir(), "finish.fifo"))
	if err := syscall.Mkfifo(string(finish), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONVEYOR_CAPTURE_FIFO_2", string(finish))
	var once sync.Once
	workerStallDeadlineTestHook = func() {
		once.Do(func() {
			for drained := false; !drained; {
				select {
				case <-activity:
				default:
					drained = true
				}
			}
			emit.release(t)
			<-activity
			finish.release(t)
			<-childWaited
		})
	}
	fixture := &captureFixture{t: t, stage: core.StageImplement, dispatch: "worker"}
	server := fixture.server()
	defer server.Close()
	var stdout, stderr lockedCaptureBuffer
	err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("capture-output-await-emit-exit", "100ms"), time.Second, &stdout, &stderr)
	const reason = "harness exited before completing work order"
	if err == nil || !strings.Contains(err.Error(), reason) || strings.Contains(err.Error(), workerStallTimeoutReason) {
		t.Fatalf("err=%v", err)
	}
	releases, _, _, _ := fixture.snapshot()
	if len(releases) != 1 || releases[0].Outcome != core.WorkOrderOutcomeChildFailure {
		t.Fatalf("releases=%+v", releases)
	}
	capture := assertSingleCapture(t, fixture, reason)
	if capture.Transcript == nil || !strings.Contains(capture.Transcript.Content, "deadline activity") {
		t.Fatalf("capture missed the racing output: %+v", capture.Transcript)
	}
}

// TestAttemptCaptureRenewalSubmissionBeforeReviewChildExit has renewal observe
// the durable review verdict first and only then lets the review child exit
// nonzero: the verdict stands, nothing is released, and the one capture is
// bound to the completed handoff.
func TestAttemptCaptureRenewalSubmissionBeforeReviewChildExit(t *testing.T) {
	_, _ = raceHooks(t)
	previousRenew, previousGrace := workerClaimRenewInterval, workerRunTerminalChildGrace
	workerClaimRenewInterval, workerRunTerminalChildGrace = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { workerClaimRenewInterval, workerRunTerminalChildGrace = previousRenew, previousGrace })
	t.Setenv("CONVEYOR_CAPTURE_EXIT", "3")
	exit := newCaptureFIFO(t, "CONVEYOR_CAPTURE_FIFO")
	for _, dispatch := range []string{"worker", "run"} {
		t.Run(dispatch, func(t *testing.T) {
			fixture := &captureFixture{t: t, stage: core.StageReview, dispatch: dispatch, gate: make(chan struct{})}
			var gateOnce, exitOnce sync.Once
			workerActivityObservedTestHook = func() { gateOnce.Do(func() { close(fixture.gate) }) }
			fixture.renew = func(int) (core.WorkOrder, int, string) {
				exitOnce.Do(func() { go exit.release(t) })
				return core.WorkOrder{ID: "capture-order", State: core.WorkOrderCompleted, AttemptID: captureFixtureAttempt}, 0, ""
			}
			fixture.reconcile = func(int) (workerservice.ClaimReconciliation, int, string) {
				return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderCompleted, AttemptID: captureFixtureAttempt}}, 0, ""
			}
			server := fixture.server()
			defer server.Close()
			var stdout, stderr lockedCaptureBuffer
			if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("capture-output-await-exit", ""), time.Second, &stdout, &stderr); err != nil {
				t.Fatalf("review verdict was not preserved: %v", err)
			}
			if releases, _, _, _ := fixture.snapshot(); len(releases) != 0 {
				t.Fatalf("submitted review was released: %+v", releases)
			}
			assertSingleCapture(t, fixture, core.AttemptHandoffTerminationReason(core.WorkOrderCompleted))
		})
	}
}

// TestAttemptCaptureSelfReleaseRacingRenewal has renewal observe the agent's
// typed checkpoint release first and only then lets the child exit: the
// launcher never issues a second release and the one capture carries the
// persisted plan-revision reason.
func TestAttemptCaptureSelfReleaseRacingRenewal(t *testing.T) {
	_, _ = raceHooks(t)
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = 20 * time.Millisecond
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	exit := newCaptureFIFO(t, "CONVEYOR_CAPTURE_FIFO")
	for _, dispatch := range []string{"worker", "run"} {
		t.Run(dispatch, func(t *testing.T) {
			fixture := &captureFixture{t: t, stage: core.StageImplement, dispatch: dispatch, gate: make(chan struct{})}
			var gateOnce, exitOnce sync.Once
			workerActivityObservedTestHook = func() { gateOnce.Do(func() { close(fixture.gate) }) }
			fixture.renew = func(int) (core.WorkOrder, int, string) {
				exitOnce.Do(func() { go exit.release(t) })
				return core.WorkOrder{}, http.StatusConflict, "work_order_released_checkpoint"
			}
			fixture.reconcile = func(int) (workerservice.ClaimReconciliation, int, string) {
				return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued,
					LastAttemptID: captureFixtureAttempt, LastFailureMessage: core.WorkOrderReleaseReasonPlanRevisionRequested}, ReleasedAtCheckpoint: true}, 0, ""
			}
			server := fixture.server()
			defer server.Close()
			var stdout, stderr lockedCaptureBuffer
			if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("capture-output-await-exit", ""), time.Second, &stdout, &stderr); err != nil {
				t.Fatalf("self-release was not a successful handoff: %v", err)
			}
			if releases, _, _, _ := fixture.snapshot(); len(releases) != 0 {
				t.Fatalf("launcher released an already released claim: %+v", releases)
			}
			assertSingleCapture(t, fixture, core.WorkOrderReleaseReasonPlanRevisionRequested)
		})
	}
}

// TestAttemptCaptureTranscriptBoundAndTruncation floods more than 4 MiB of
// output and proves the capture keeps exactly the newest 4 MiB with the
// truncation flag on both planes.
func TestAttemptCaptureTranscriptBoundAndTruncation(t *testing.T) {
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = time.Hour
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	for _, dispatch := range []string{"worker", "run"} {
		t.Run(dispatch, func(t *testing.T) {
			fixture := &captureFixture{t: t, stage: core.StageImplement, dispatch: dispatch}
			fixture.reconcile = func(int) (workerservice.ClaimReconciliation, int, string) {
				return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderSubmitted, AttemptID: captureFixtureAttempt}}, 0, ""
			}
			server := fixture.server()
			defer server.Close()
			var stdout, stderr lockedCaptureBuffer
			if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("capture-flood", ""), time.Second, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			capture := assertSingleCapture(t, fixture, core.AttemptHandoffTerminationReason(core.WorkOrderSubmitted))
			if capture.Transcript == nil || !capture.Transcript.Truncated || len(capture.Transcript.Content) != workerAttemptTranscriptLimit || !strings.HasSuffix(capture.Transcript.Content, "newest tail marker\n") {
				t.Fatalf("transcript truncated=%v bytes=%d", capture.Transcript != nil && capture.Transcript.Truncated, len(capture.Transcript.Content))
			}
		})
	}
}

// TestAttemptCaptureFailuresPreserveSuccessfulHandoff injects spool creation,
// write, and read failures and a capture delivery failure into a successful
// submission: each still ends as the same successful handoff with no release,
// and the capture is delivered without a transcript or only warned about.
func TestAttemptCaptureFailuresPreserveSuccessfulHandoff(t *testing.T) {
	previousRenew, previousSpool := workerClaimRenewInterval, newAttemptTranscriptSpool
	workerClaimRenewInterval = time.Hour
	t.Cleanup(func() { workerClaimRenewInterval, newAttemptTranscriptSpool = previousRenew, previousSpool })
	for _, test := range []struct {
		name           string
		spool          func(string, int) (*boundedTranscriptSpool, error)
		captureStatus  int
		wantDelivered  bool
		wantTranscript bool
		wantWarning    string
	}{
		{name: "spool creation failure", spool: func(string, int) (*boundedTranscriptSpool, error) {
			return nil, os.ErrPermission
		}, wantDelivered: true, wantWarning: "create attempt transcript spool"},
		{name: "spool write failure", spool: func(directory string, limit int) (*boundedTranscriptSpool, error) {
			spool, err := newBoundedTranscriptSpool(directory, limit)
			if err == nil {
				err = spool.file.Close()
			}
			return spool, err
		}, wantDelivered: true, wantWarning: "read attempt transcript spool"},
		{name: "spool read failure", spool: func(directory string, limit int) (*boundedTranscriptSpool, error) {
			file, err := os.OpenFile(filepath.Join(directory, "write-only-spool"), os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			return &boundedTranscriptSpool{file: file, path: file.Name(), limit: limit}, nil
		}, wantDelivered: true, wantWarning: "read attempt transcript spool"},
		{name: "capture delivery failure", spool: newBoundedTranscriptSpool, captureStatus: http.StatusInternalServerError, wantDelivered: true, wantTranscript: true, wantWarning: "deliver attempt transcript capture"},
	} {
		for _, dispatch := range []string{"worker", "run"} {
			t.Run(test.name+"/"+dispatch, func(t *testing.T) {
				newAttemptTranscriptSpool = test.spool
				fixture := &captureFixture{t: t, stage: core.StageImplement, dispatch: dispatch, captureErr: test.captureStatus}
				fixture.reconcile = func(int) (workerservice.ClaimReconciliation, int, string) {
					return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderSubmitted, AttemptID: captureFixtureAttempt}}, 0, ""
				}
				server := fixture.server()
				defer server.Close()
				var stdout, stderr lockedCaptureBuffer
				if err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("early-output", ""), time.Second, &stdout, &stderr); err != nil {
					t.Fatalf("successful submission changed by %s: %v", test.name, err)
				}
				if releases, _, _, _ := fixture.snapshot(); len(releases) != 0 {
					t.Fatalf("submitted attempt was released: %+v", releases)
				}
				capture := assertSingleCapture(t, fixture, core.AttemptHandoffTerminationReason(core.WorkOrderSubmitted))
				if (capture.Transcript != nil) != test.wantTranscript {
					t.Fatalf("transcript=%+v want present=%v", capture.Transcript, test.wantTranscript)
				}
				if !strings.Contains(stderr.String(), test.wantWarning) {
					t.Fatalf("stderr=%q want %q", stderr.String(), test.wantWarning)
				}
			})
		}
	}
}
