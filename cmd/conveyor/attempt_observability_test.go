package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// captureFixture is a fake parent plane (worker or task run) that records
// releases and attempt-observability deliveries for one launched attempt.
type captureFixture struct {
	t        *testing.T
	stage    core.Stage
	dispatch string
	// renew and reconcile decide the server's view of the attempt.
	renew      func(call int) (core.WorkOrder, int, string)
	reconcile  func(call int) (workerservice.ClaimReconciliation, int, string)
	captureErr int
	specOrigin string
	// claim, when set, adjusts the claimed order (for example continuation
	// metadata) before it is returned.
	claim func(*core.WorkOrder)
	// gate, when set, holds renewal at "claimed" until the child's first
	// output, so terminal and authority-loss responses can never reach the
	// pre-start renewal (deterministic ordering, no timers).
	gate chan struct{}

	mu         sync.Mutex
	sessionID  string
	renewCalls int
	recCalls   int
	releases   []core.WorkOrderRelease
	captures   []core.WorkOrderAttemptCapture
	capturePth []string
	issued     int
	revoked    int
}

const captureFixtureAttempt = "attempt-current"

func (f *captureFixture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveDirectTaskGET(w, r) {
			return
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/agent-credential"):
			f.mu.Lock()
			f.issued++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"credential_id": "run-agent", "credential": "run-agent-secret"})
		case r.Method == http.MethodDelete && strings.HasSuffix(path, "/agent-credential"):
			f.mu.Lock()
			f.revoked++
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/claim"):
			var request struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			f.mu.Lock()
			f.sessionID = request.SessionID
			f.mu.Unlock()
			order := core.WorkOrder{ID: "capture-order", TaskID: "capture-task", Stage: f.stage, State: core.WorkOrderClaimed,
				AttemptID: captureFixtureAttempt, LastAttemptID: "attempt-predecessor", LeaseExpiresAt: time.Now().Add(time.Minute)}
			if f.claim != nil {
				f.claim(&order)
			}
			if f.dispatch == "run" {
				_ = json.NewEncoder(w).Encode(order)
				return
			}
			_ = json.NewEncoder(w).Encode(workerservice.ClaimDelivery{WorkOrder: order})
		case strings.HasSuffix(path, "/renew"):
			f.mu.Lock()
			f.renewCalls++
			call := f.renewCalls
			f.mu.Unlock()
			order, status, code := core.WorkOrder{ID: "capture-order", State: core.WorkOrderClaimed, AttemptID: captureFixtureAttempt, LeaseExpiresAt: time.Now().Add(time.Minute)}, 0, ""
			gated := false
			if f.gate != nil {
				select {
				case <-f.gate:
				default:
					gated = true
				}
			}
			if f.renew != nil && !gated {
				order, status, code = f.renew(call)
			}
			if status != 0 {
				if code != "" {
					w.Header().Set("X-Conveyor-Error-Code", code)
				}
				http.Error(w, code, status)
				return
			}
			_ = json.NewEncoder(w).Encode(order)
		case strings.HasSuffix(path, "/reconcile"):
			f.mu.Lock()
			f.recCalls++
			call := f.recCalls
			f.mu.Unlock()
			result, status, code := workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderClaimed}, Authorized: true}, 0, ""
			if f.reconcile != nil {
				result, status, code = f.reconcile(call)
			}
			if status != 0 {
				if code != "" {
					w.Header().Set("X-Conveyor-Error-Code", code)
				}
				http.Error(w, code, status)
				return
			}
			_ = json.NewEncoder(w).Encode(result)
		case strings.HasSuffix(path, "/release"):
			var release core.WorkOrderRelease
			var wire struct {
				Reason  string `json:"reason"`
				Outcome string `json:"outcome"`
			}
			_ = json.NewDecoder(r.Body).Decode(&wire)
			release.Reason, release.Outcome = wire.Reason, wire.Outcome
			f.mu.Lock()
			f.releases = append(f.releases, release)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued})
		case strings.HasSuffix(path, "/attempt-observability"):
			var capture core.WorkOrderAttemptCapture
			if err := json.NewDecoder(r.Body).Decode(&capture); err != nil {
				f.t.Errorf("decode capture: %v", err)
			}
			f.mu.Lock()
			f.captures = append(f.captures, capture)
			f.capturePth = append(f.capturePth, path)
			f.mu.Unlock()
			if f.captureErr != 0 {
				if f.captureErr == http.StatusConflict {
					w.Header().Set("X-Conveyor-Error-Code", "attempt_capture_unverified")
					http.Error(w, "attempt ending unverified", f.captureErr)
					return
				}
				http.Error(w, "capture refused", f.captureErr)
				return
			}
			_ = json.NewEncoder(w).Encode(core.WorkOrderAttemptCaptureResult{Created: true, TerminationReason: capture.TerminationReason})
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *captureFixture) item(mode, stall string) workerservice.DispatchOrder {
	item := workerservice.DispatchOrder{
		Order:    core.WorkOrder{ID: "capture-order", TaskID: "capture-task", Stage: f.stage},
		Dispatch: f.dispatch,
		Harness: config.Harness{
			MCPTransport: config.MCPTransportTOMLOverride, Name: "helper", Command: []string{os.Args[0], "-test.run=TestWorkerLifecycleHelper", "--", mode},
			StallTimeoutText: stall,
		},
	}
	if f.dispatch == "run" {
		item.Task = core.Task{ID: "capture-task", Branch: "conveyor/capture"}
	}
	if f.stage == core.StageSpec {
		// Spec launches materialize a read-only clone of the assigned base
		// before the child starts.
		item.Task.Repo, item.Task.BaseBranch = "conveyor", "main"
		item.Repository = config.Repo{Name: "conveyor", URL: f.specOrigin}
	}
	return item
}

func (f *captureFixture) snapshot() ([]core.WorkOrderRelease, []core.WorkOrderAttemptCapture, []string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.WorkOrderRelease(nil), f.releases...), append([]core.WorkOrderAttemptCapture(nil), f.captures...), append([]string(nil), f.capturePth...), f.sessionID
}

// TestAttemptCaptureAllStageEndings drives the shared launcher through every
// mediated ending for every stage and asserts one capture of the exact
// current attempt bound to the same termination reason as the ending, with
// the lifecycle result unchanged (req-260820-221be8 AC-2.1; DEC-26).
func TestAttemptCaptureAllStageEndings(t *testing.T) {
	previousRenew, previousGrace := workerClaimRenewInterval, workerRunTerminalChildGrace
	t.Cleanup(func() { workerClaimRenewInterval, workerRunTerminalChildGrace = previousRenew, previousGrace })
	workerRunTerminalChildGrace = 50 * time.Millisecond

	type ending struct {
		name           string
		mode           string
		stall          string
		firstActivity  time.Duration
		renewEvery     time.Duration
		cancelAfter    bool
		renew          func(core.Stage) func(int) (core.WorkOrder, int, string)
		reconcile      func(core.Stage) func(int) (workerservice.ClaimReconciliation, int, string)
		wantReason     func(core.Stage) string
		wantErr        string
		wantNoCapture  bool
		wantTranscript bool
		dispatches     []string
		// gateOnOutput holds renew responses at "claimed" until the child's
		// first output is observed by the launcher.
		gateOnOutput bool
		// captureStatus makes the fake server refuse the capture.
		captureStatus int
	}
	handoffState := func(stage core.Stage) core.WorkOrderState {
		if stage == core.StageImplement {
			return core.WorkOrderSubmitted
		}
		return core.WorkOrderCompleted
	}
	terminal := func(stage core.Stage) func(int) (workerservice.ClaimReconciliation, int, string) {
		return func(int) (workerservice.ClaimReconciliation, int, string) {
			return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: handoffState(stage), AttemptID: captureFixtureAttempt}}, 0, ""
		}
	}
	endings := []ending{
		{
			name: "successful handoff", mode: "early-output", firstActivity: time.Second, renewEvery: time.Minute,
			reconcile:      terminal,
			wantReason:     func(stage core.Stage) string { return core.AttemptHandoffTerminationReason(handoffState(stage)) },
			wantTranscript: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "nonzero exit without handoff", mode: "exit", firstActivity: time.Second, renewEvery: time.Minute,
			wantReason: func(stage core.Stage) string {
				if stage == core.StageReview {
					return "harness exited without terminal verdict submission"
				}
				return "harness exited: exit status 7"
			},
			wantErr: "exit", wantTranscript: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "zero exit without handoff", mode: "early-output", firstActivity: time.Second, renewEvery: time.Minute,
			wantReason: func(stage core.Stage) string {
				if stage == core.StageReview {
					return "harness exited without terminal verdict submission"
				}
				return "harness exited before completing work order"
			},
			wantErr: "harness exited", wantTranscript: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "plan revision self release", mode: "early-output", firstActivity: time.Second, renewEvery: time.Minute,
			reconcile: func(core.Stage) func(int) (workerservice.ClaimReconciliation, int, string) {
				return func(int) (workerservice.ClaimReconciliation, int, string) {
					return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued,
						LastAttemptID: captureFixtureAttempt, LastFailureMessage: core.WorkOrderReleaseReasonPlanRevisionRequested}, ReleasedAtCheckpoint: true}, 0, ""
				}
			},
			wantReason: func(core.Stage) string { return core.WorkOrderReleaseReasonPlanRevisionRequested },
			dispatches: []string{"worker", "run"},
		},
		{
			name: "operator checkpoint self release", mode: "early-output", firstActivity: time.Second, renewEvery: time.Minute,
			reconcile: func(core.Stage) func(int) (workerservice.ClaimReconciliation, int, string) {
				return func(int) (workerservice.ClaimReconciliation, int, string) {
					return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued,
						LastAttemptID: captureFixtureAttempt, LastFailureMessage: core.WorkOrderReleaseReasonOperatorCheckpointReached}, ReleasedAtCheckpoint: true}, 0, ""
				}
			},
			wantReason: func(core.Stage) string { return core.WorkOrderReleaseReasonOperatorCheckpointReached },
			dispatches: []string{"worker", "run"},
		},
		{
			// An ordinary agent release_work_order: the launcher did not commit
			// the ending, so Conveyor binds the capture to the persisted reason.
			name: "ordinary agent self release", mode: "early-output", firstActivity: time.Second, renewEvery: time.Minute,
			reconcile: func(core.Stage) func(int) (workerservice.ClaimReconciliation, int, string) {
				return func(int) (workerservice.ClaimReconciliation, int, string) {
					return workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued,
						LastAttemptID: captureFixtureAttempt, LastFailureMessage: "agent released: blocked on missing credential"}, Reason: "session released the claim"}, 0, ""
				}
			},
			wantReason: func(core.Stage) string { return "" },
			wantErr:    "server reports queued", wantTranscript: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "first activity timeout", mode: "silent", firstActivity: 100 * time.Millisecond, renewEvery: time.Minute,
			wantReason: func(core.Stage) string { return workerFirstActivityTimeoutReason },
			wantErr:    workerFirstActivityTimeoutReason, dispatches: []string{"worker", "run"},
		},
		{
			name: "stall timeout", mode: "early-then-silent", stall: "150ms", firstActivity: time.Second, renewEvery: time.Minute,
			wantReason: func(core.Stage) string { return workerStallTimeoutReason },
			wantErr:    workerStallTimeoutReason, wantTranscript: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "authority loss reported by renewal", mode: "early-then-silent", firstActivity: time.Second, renewEvery: 30 * time.Millisecond,
			renew: func(core.Stage) func(int) (core.WorkOrder, int, string) {
				return func(int) (core.WorkOrder, int, string) {
					return core.WorkOrder{ID: "capture-order", State: core.WorkOrderQueued}, 0, ""
				}
			},
			wantReason: func(core.Stage) string { return "" },
			wantErr:    "claim authority lost", wantTranscript: true, gateOnOutput: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "preemption", mode: "early-then-silent", firstActivity: time.Second, renewEvery: 30 * time.Millisecond,
			renew: func(core.Stage) func(int) (core.WorkOrder, int, string) {
				return func(int) (core.WorkOrder, int, string) {
					return core.WorkOrder{}, http.StatusConflict, "work_order_preempted"
				}
			},
			wantReason: func(core.Stage) string { return "" },
			wantErr:    "preempted", wantTranscript: true, gateOnOutput: true, dispatches: []string{"worker", "run"},
		},
		{
			// Server-side cancellation: the row cannot name the ending, so the
			// capture is delivered without a declaration and refused as
			// unverified; the cancellation result is unchanged.
			name: "server cancellation", mode: "early-then-silent", firstActivity: time.Second, renewEvery: 30 * time.Millisecond,
			renew: func(core.Stage) func(int) (core.WorkOrder, int, string) {
				return func(int) (core.WorkOrder, int, string) {
					return core.WorkOrder{}, http.StatusConflict, "work order was cancelled"
				}
			},
			wantReason: func(core.Stage) string { return "" },
			wantErr:    errWorkerOrderCancelled.Error(), wantTranscript: true, gateOnOutput: true, captureStatus: http.StatusConflict, dispatches: []string{"worker", "run"},
		},
		{
			name: "worker shutdown", mode: "early-then-silent", firstActivity: time.Second, renewEvery: time.Minute, cancelAfter: true,
			wantReason: func(core.Stage) string { return "worker shutting down" },
			wantErr:    context.Canceled.Error(), wantTranscript: true, dispatches: []string{"worker"},
		},
		{
			name: "terminal grace reaps lingering child", mode: "early-then-silent", firstActivity: time.Second, renewEvery: 30 * time.Millisecond,
			renew: func(stage core.Stage) func(int) (core.WorkOrder, int, string) {
				return func(int) (core.WorkOrder, int, string) {
					return core.WorkOrder{ID: "capture-order", State: handoffState(stage), AttemptID: captureFixtureAttempt}, 0, ""
				}
			},
			wantReason:     func(stage core.Stage) string { return core.AttemptHandoffTerminationReason(handoffState(stage)) },
			wantTranscript: true, gateOnOutput: true, dispatches: []string{"worker", "run"},
		},
		{
			name: "explicit run interruption leaves no capture", mode: "early-then-silent", firstActivity: time.Second, renewEvery: time.Minute, cancelAfter: true,
			wantErr: context.Canceled.Error(), wantNoCapture: true, dispatches: []string{"run"},
		},
	}
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		for _, test := range endings {
			for _, dispatch := range test.dispatches {
				t.Run(fmt.Sprintf("%s/%s/%s", stage, dispatch, test.name), func(t *testing.T) {
					workerClaimRenewInterval = test.renewEvery
					fixture := &captureFixture{t: t, stage: stage, dispatch: dispatch, captureErr: test.captureStatus}
					if test.gateOnOutput {
						fixture.gate = make(chan struct{})
						var once sync.Once
						workerActivityObservedTestHook = func() { once.Do(func() { close(fixture.gate) }) }
						t.Cleanup(func() { workerActivityObservedTestHook = nil })
					}
					if stage == core.StageSpec {
						fixture.specOrigin = newGitFixture(t).origin
					}
					if test.renew != nil {
						fixture.renew = test.renew(stage)
					}
					if test.reconcile != nil {
						fixture.reconcile = test.reconcile(stage)
					}
					server := fixture.server()
					defer server.Close()
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					// The run reap notice is written while the lingering child
					// may still write, so the destination is synchronized.
					var stdout, stderr lockedCaptureBuffer
					var err error
					if test.cancelAfter {
						activity := &signalWriter{signal: make(chan struct{})}
						go func() {
							<-activity.signal
							cancel()
						}()
						err = runHarnessChildWithFirstActivityTimeoutAndOutput(ctx, &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item(test.mode, test.stall), test.firstActivity, activity, &stderr)
					} else {
						err = runHarnessChildWithFirstActivityTimeoutAndOutput(ctx, &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item(test.mode, test.stall), test.firstActivity, &stdout, &stderr)
					}
					if test.wantErr == "" && err != nil {
						t.Fatalf("ending changed: err=%v stderr=%q", err, stderr.String())
					}
					if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
						t.Fatalf("err=%v want containing %q", err, test.wantErr)
					}
					releases, captures, paths, session := fixture.snapshot()
					if test.wantNoCapture {
						if len(captures) != 0 || len(releases) != 0 {
							t.Fatalf("interrupted run released=%+v captured=%+v", releases, captures)
						}
						return
					}
					if len(captures) != 1 {
						t.Fatalf("captures=%d (%+v) want exactly one; stderr=%q", len(captures), captures, stderr.String())
					}
					capture := captures[0]
					if capture.AttemptID != captureFixtureAttempt || capture.SessionID == "" || capture.SessionID != session {
						t.Fatalf("capture identity=%+v session=%q", capture, session)
					}
					if want := test.wantReason(stage); capture.TerminationReason != want {
						t.Fatalf("capture reason=%q want %q", capture.TerminationReason, want)
					}
					for _, release := range releases {
						if test.wantReason(stage) != "" && release.Reason != capture.TerminationReason {
							t.Fatalf("release reason %q differs from capture reason %q", release.Reason, capture.TerminationReason)
						}
					}
					if len(releases) > 1 {
						t.Fatalf("duplicate releases: %+v", releases)
					}
					if test.captureStatus != 0 && !strings.Contains(stderr.String(), "deliver attempt transcript capture") {
						t.Fatalf("refused capture left no warning: %q", stderr.String())
					}
					wantPath := "/v1/worker/work-orders/capture-order/attempt-observability"
					if dispatch == "run" {
						wantPath = "/v1/tasks/capture-task/run-orders/capture-order/attempt-observability"
					}
					if paths[0] != wantPath {
						t.Fatalf("capture path=%q want %q", paths[0], wantPath)
					}
					want := "activity"
					if test.mode == "exit" {
						// The exit helper prints only credentials, which must
						// arrive redacted.
						want = "token=[REDACTED:exact]"
					}
					if test.wantTranscript && (capture.Transcript == nil || !strings.Contains(capture.Transcript.Content, want)) {
						t.Fatalf("capture transcript=%+v", capture.Transcript)
					}
					if capture.Transcript != nil && (strings.Contains(capture.Transcript.Content, "parent-credential") || strings.Contains(capture.Transcript.Content, "run-agent-secret")) {
						t.Fatal("capture transcript carried a credential")
					}
				})
			}
		}
	}
}

// signalWriter closes signal on the first write so a test can cancel only
// after the child produced output, without sleeping.
type signalWriter struct {
	once   sync.Once
	signal chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.signal) })
	return len(p), nil
}

// TestAttemptCaptureRacesFinalizeOnce proves racing endings (child exit,
// timeout, renewal observing submission, self-release) arm and deliver at
// most one capture, and the first recorded ending wins deterministically.
func TestAttemptCaptureRacesFinalizeOnce(t *testing.T) {
	var mu sync.Mutex
	var delivered []core.WorkOrderAttemptCapture
	finalizer := newAttemptCaptureFinalizer("session", "attempt", func(_ context.Context, capture core.WorkOrderAttemptCapture) error {
		mu.Lock()
		delivered = append(delivered, capture)
		mu.Unlock()
		return nil
	}, nil)
	finalizer.arm("child exit")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, reason := range []string{workerFirstActivityTimeoutReason, workerStallTimeoutReason, core.AttemptHandoffTerminationReason(core.WorkOrderSubmitted), core.WorkOrderReleaseReasonPlanRevisionRequested} {
		wg.Add(2)
		go func(reason string) {
			defer wg.Done()
			<-start
			finalizer.arm(reason)
		}(reason)
		go func() {
			defer wg.Done()
			<-start
			finalizer.finish()
		}()
	}
	close(start)
	wg.Wait()
	finalizer.finish()
	if len(delivered) != 1 || delivered[0].TerminationReason != "child exit" || delivered[0].AttemptID != "attempt" || delivered[0].SessionID != "session" {
		t.Fatalf("delivered=%+v want one first-armed capture", delivered)
	}

	// An unarmed attempt (no mediated ending) never delivers.
	unarmedCalls := 0
	unarmed := newAttemptCaptureFinalizer("session", "attempt", func(context.Context, core.WorkOrderAttemptCapture) error {
		unarmedCalls++
		return nil
	}, nil)
	unarmed.finish()
	unarmed.arm("late")
	unarmed.finish()
	if unarmedCalls != 0 {
		t.Fatalf("unarmed finalizer delivered %d captures", unarmedCalls)
	}
}

// TestAttemptCaptureFailuresAreObservational proves spool and transport
// failures only warn: the ending keeps its release and result.
func TestAttemptCaptureFailuresAreObservational(t *testing.T) {
	t.Run("spool read failure delivers the ending without transcript", func(t *testing.T) {
		var got []core.WorkOrderAttemptCapture
		var warnings bytes.Buffer
		finalizer := newAttemptCaptureFinalizer("session", "attempt", func(_ context.Context, capture core.WorkOrderAttemptCapture) error {
			got = append(got, capture)
			return nil
		}, &warnings)
		finalizer.setSnapshot(func() (string, bool, error) { return "", false, errors.New("spool unreadable") })
		finalizer.arm("stalled")
		finalizer.finish()
		if len(got) != 1 || got[0].Transcript != nil || got[0].TerminationReason != "stalled" || !strings.Contains(warnings.String(), "spool unreadable") {
			t.Fatalf("captures=%+v warnings=%q", got, warnings.String())
		}
	})
	t.Run("transport failure warns only", func(t *testing.T) {
		var warnings bytes.Buffer
		finalizer := newAttemptCaptureFinalizer("session", "attempt", func(context.Context, core.WorkOrderAttemptCapture) error {
			return errors.New("connection reset")
		}, &warnings)
		finalizer.arm("released")
		finalizer.finish()
		if !strings.Contains(warnings.String(), "connection reset") || !strings.Contains(warnings.String(), "best-effort") {
			t.Fatalf("warnings=%q", warnings.String())
		}
	})
	for _, status := range []int{http.StatusInternalServerError, http.StatusConflict, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprintf("server refusal %d keeps child-failure ending", status), func(t *testing.T) {
			previousRenew := workerClaimRenewInterval
			workerClaimRenewInterval = time.Minute
			t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
			fixture := &captureFixture{t: t, stage: core.StageReview, dispatch: "worker", captureErr: status}
			server := fixture.server()
			defer server.Close()
			var stdout, stderr bytes.Buffer
			err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("exit", ""), time.Second, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "harness exited without terminal verdict submission") {
				t.Fatalf("ending changed by capture refusal: %v", err)
			}
			releases, captures, _, _ := fixture.snapshot()
			if len(releases) != 1 || releases[0].Outcome != core.WorkOrderOutcomeChildFailure || len(captures) != 1 {
				t.Fatalf("releases=%+v captures=%d", releases, len(captures))
			}
			if !strings.Contains(stderr.String(), "deliver attempt transcript capture") {
				t.Fatalf("stderr=%q want capture warning", stderr.String())
			}
		})
	}
}

// TestAttemptCaptureUsesCurrentAttemptNotPredecessorProducer proves the
// launcher labels its spool with its own claimed attempt even when the claim
// names a predecessor attempt (component-attempt-checkpoints).
func TestAttemptCaptureUsesCurrentAttemptNotPredecessorProducer(t *testing.T) {
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = time.Minute
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	fixture := &captureFixture{t: t, stage: core.StageReview, dispatch: "worker"}
	server := fixture.server()
	defer server.Close()
	var stdout, stderr bytes.Buffer
	_ = runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", fixture.item("early-output", ""), time.Second, &stdout, &stderr)
	_, captures, _, _ := fixture.snapshot()
	if len(captures) != 1 || captures[0].AttemptID != captureFixtureAttempt {
		t.Fatalf("captures=%+v want the current attempt %q, never attempt-predecessor", captures, captureFixtureAttempt)
	}
}

// TestUnmediatedEndingHasNoSyntheticCapture proves a pre-launch failure, an
// interrupted run, and an attempt without a claimed attempt ID deliver no
// capture (req-260820-221be8 AC-2.3).
func TestUnmediatedEndingHasNoSyntheticCapture(t *testing.T) {
	previousRenew := workerClaimRenewInterval
	workerClaimRenewInterval = time.Minute
	t.Cleanup(func() { workerClaimRenewInterval = previousRenew })
	t.Run("pre-launch failure", func(t *testing.T) {
		fixture := &captureFixture{t: t, stage: core.StageReview, dispatch: "worker"}
		server := fixture.server()
		defer server.Close()
		item := fixture.item("exit", "")
		item.Harness.Command = []string{"/nonexistent/conveyor-harness-binary"}
		var stdout, stderr bytes.Buffer
		err := runHarnessChildWithFirstActivityTimeoutAndOutput(t.Context(), &client{base: server.URL, workspace: "demo"}, "parent-credential", item, time.Second, &stdout, &stderr)
		if err == nil {
			t.Fatal("launch of a missing binary succeeded")
		}
		releases, captures, _, _ := fixture.snapshot()
		if len(captures) != 0 || len(releases) != 1 || !strings.HasPrefix(releases[0].Reason, "harness launch failed") {
			t.Fatalf("releases=%+v captures=%+v", releases, captures)
		}
	})
	t.Run("launcher killed then lease recovery", runLauncherCrashLeavesNoSyntheticCaptureAndRecoversLease)
	t.Run("missing attempt identity", func(t *testing.T) {
		calls := 0
		finalizer := newAttemptCaptureFinalizer("session", "", func(context.Context, core.WorkOrderAttemptCapture) error {
			calls++
			return nil
		}, nil)
		finalizer.arm("released")
		finalizer.finish()
		if calls != 0 {
			t.Fatal("capture without attempt identity was delivered")
		}
	})
}

// lockedCaptureBuffer is a goroutine-safe output destination.
type lockedCaptureBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedCaptureBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedCaptureBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
