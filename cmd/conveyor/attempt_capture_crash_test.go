package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// TestCrashLauncherHelper is the launcher process that
// runLauncherCrashLeavesNoSyntheticCaptureAndRecoversLease kills. It runs
// the real shared launcher against the fixture server and never returns on
// its own: the test SIGKILLs it while its harness child is running.
func TestCrashLauncherHelper(t *testing.T) {
	address := os.Getenv("CONVEYOR_CRASH_LAUNCHER_SERVER")
	if address == "" {
		return
	}
	item := workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: os.Getenv("CONVEYOR_CRASH_ORDER"), Stage: core.StageImplement},
		Harness: config.Harness{
			MCPTransport: config.MCPTransportTOMLOverride, Name: "helper", Command: []string{os.Args[0], "-test.run=^TestWorkerLifecycleHelper$", "--", "crash-child"},
		},
	}
	_ = runHarnessChildWithFirstActivityTimeoutAndOutput(store.WithActor(context.Background(), store.SystemActor()), &client{base: address, workspace: "demo"}, "crash-worker-credential", item, time.Minute, os.Stdout, os.Stderr)
	t.Fatal("launcher returned before it was killed")
}

// crashFixtureServer routes the worker plane to a real memory store and worker
// service, recording every request path.
type crashFixtureServer struct {
	mu    sync.Mutex
	paths []string
}

func (f *crashFixtureServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// runLauncherCrashLeavesNoSyntheticCaptureAndRecoversLease starts the real
// launcher in its own process with a running harness, waits on the harness's
// readiness pipe (no timers), and SIGKILLs the launcher so no deferred
// mediation runs. No capture request reaches the server and no capture row
// exists. Ordinary lease-expiry recovery, driven by an explicit clock tick,
// then recovers the order exactly like an attempt that never had a launcher,
// operator recovery and a fresh claim work, and a late capture is refused
// (req-260820-221be8 AC-2.3; req-local-task-runs AC-5.1).
func runLauncherCrashLeavesNoSyntheticCaptureAndRecoversLease(t *testing.T) {
	now := time.Now().UTC()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP, Timeout: 24 * time.Hour},
	}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	workers := &workerservice.Service{Store: st, WorkOrders: &workorder.Service{Store: st, ConfigProvider: provider}, ConfigProvider: provider}
	const workerID = "crash-worker"
	identity := func(session string) core.WorkOrderClaimIdentity {
		return core.WorkOrderClaimIdentity{WorkerID: workerID, ClaimantID: workerID, SessionID: session}
	}
	claimFor := func(session string) core.WorkOrderClaim {
		return core.WorkOrderClaim{WorkerID: workerID, ClaimantID: workerID, SessionID: session, ClientToken: session, Lease: time.Minute, ExecutionTimeout: 24 * time.Hour}
	}
	newOrder := func(taskID string) core.WorkOrder {
		task := core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", Branch: "conveyor/" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
		job := core.Job{ID: taskID + "-implement-1", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour)}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	crashed, control := newOrder("crash-launched"), newOrder("crash-control")

	fixture := &crashFixtureServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.paths = append(fixture.paths, r.URL.Path)
		fixture.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || strings.Join(parts[:3], "/") != "v1/worker/work-orders" {
			http.NotFound(w, r)
			return
		}
		id, action := parts[3], parts[4]
		var input struct {
			SessionID string `json:"session_id"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&input)
		}
		var response any
		var err error
		switch action {
		case "claim":
			var claimed core.WorkOrder
			claimed, err = storetest.ClaimWorkOrder(ctx, st, id, claimFor(input.SessionID))
			response = workerservice.ClaimDelivery{WorkOrder: claimed}
		case "renew":
			response, err = workers.RenewClaim(ctx, identity(input.SessionID), id)
		case "reconcile":
			response, err = workers.ReconcileClaim(ctx, identity(r.URL.Query().Get("session_id")), id)
		default:
			// A release or any other ending would be mediation; record and refuse.
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	directory := t.TempDir()
	ready := filepath.Join(directory, "ready.fifo")
	block := filepath.Join(directory, "block.fifo")
	for _, path := range []string{ready, block} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	launcher := exec.Command(os.Args[0], "-test.run=^TestCrashLauncherHelper$")
	launcher.Env = append(os.Environ(),
		"CONVEYOR_CRASH_LAUNCHER_SERVER="+server.URL, "CONVEYOR_CRASH_ORDER="+crashed.ID,
		"CONVEYOR_CRASH_READY="+ready, "CONVEYOR_CAPTURE_FIFO="+block,
	)
	var launcherOutput strings.Builder
	launcher.Stdout, launcher.Stderr = &launcherOutput, &launcherOutput
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	// The harness opens the readiness pipe only after the launcher claimed the
	// order and started it, so this read is the "attempt is running" barrier.
	readiness, err := os.Open(ready)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(readiness).ReadString('\n')
	_ = readiness.Close()
	harnessPID, convErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || convErr != nil || harnessPID <= 0 {
		_ = launcher.Process.Kill()
		_ = launcher.Wait()
		t.Fatalf("harness readiness=%q err=%v %v launcher output:\n%s", line, err, convErr, launcherOutput.String())
	}
	running, err := st.GetWorkOrder(ctx, crashed.ID)
	if err != nil || running.State != core.WorkOrderClaimed || running.AttemptID == "" {
		t.Fatalf("attempt not running before crash: %+v err=%v", running, err)
	}

	// Abrupt launcher death: SIGKILL runs no defer, release, or capture.
	if err = launcher.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := launcher.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("launcher did not die by SIGKILL: %v\n%s", waitErr, launcherOutput.String())
	}
	// Clean up only the fixture's own orphaned harness process group.
	if err = syscall.Kill(-harnessPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	// Closing the server waits for any in-flight handler, so the recorded
	// request list is final.
	server.Close()
	for _, path := range fixture.seen() {
		if strings.HasSuffix(path, "/attempt-observability") || strings.HasSuffix(path, "/release") {
			t.Fatalf("killed launcher mediated its ending: %s (all requests %v)", path, fixture.seen())
		}
	}
	if seen := fixture.seen(); len(seen) == 0 || !strings.HasSuffix(seen[0], "/claim") {
		t.Fatalf("launcher never claimed through the fixture: %v", seen)
	}
	t.Logf("requests before the crash: %v", fixture.seen())
	captures, err := st.ListWorkOrderTranscriptCaptures(ctx, crashed.ID)
	if err != nil || len(captures) != 0 {
		t.Fatalf("crash produced captures=%+v err=%v", captures, err)
	}

	// The control attempt never had a launcher. Claim it the same way and
	// recover both through ordinary lease-expiry recovery at an explicit time
	// past both leases and before both execution deadlines.
	controlClaim, err := storetest.ClaimWorkOrder(ctx, st, control.ID, claimFor("control-session"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	recoveredCrash, err := st.GetWorkOrder(ctx, crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveredControl, err := st.GetWorkOrder(ctx, control.ID)
	if err != nil {
		t.Fatal(err)
	}
	shape := func(o core.WorkOrder) string {
		return strings.Join([]string{string(o.State), strconv.FormatBool(o.Claimable), strconv.FormatBool(o.RetrySuppressed), o.RetrySuppressionReason,
			o.LastAttemptOutcome, o.LastFailureMessage, strconv.Itoa(o.AutomaticRetryCount), strconv.FormatBool(o.NextRetryAt.IsZero()), o.AttemptID, o.SessionID}, "|")
	}
	if shape(recoveredCrash) != shape(recoveredControl) || recoveredCrash.LastAttemptOutcome != core.WorkOrderOutcomeExpired ||
		recoveredCrash.LastAttemptID != running.AttemptID || recoveredControl.LastAttemptID != controlClaim.AttemptID {
		t.Fatalf("crashed recovery %s differs from control %s", shape(recoveredCrash), shape(recoveredControl))
	}
	if _, err = workers.CaptureWorkerAttempt(ctx, core.Worker{ID: workerID}, crashed.ID, core.WorkOrderAttemptCapture{
		SessionID: running.SessionID, AttemptID: running.AttemptID, Transcript: &core.WorkOrderAttemptTranscript{Content: "fabricated after crash"},
	}); !errors.Is(err, store.ErrAttemptCaptureUnverified) {
		t.Fatalf("late capture for the crashed attempt err=%v", err)
	}
	for _, order := range []core.WorkOrder{control, crashed} {
		recovered, err := storetest.RecoverWorkOrder(ctx, st, order.ID, "crash-recover-"+order.ID, time.Hour)
		if err != nil || recovered.State != core.WorkOrderQueued {
			t.Fatalf("operator recovery of %s: %+v err=%v", order.ID, recovered, err)
		}
		fresh, err := storetest.ClaimWorkOrder(ctx, st, order.ID, claimFor("fresh-"+order.ID))
		if err != nil || fresh.AttemptID == "" || fresh.AttemptID == running.AttemptID || fresh.AttemptID == controlClaim.AttemptID {
			t.Fatalf("fresh claim of %s: %+v err=%v", order.ID, fresh, err)
		}
		if captures, err := st.ListWorkOrderTranscriptCaptures(ctx, order.ID); err != nil || len(captures) != 0 {
			t.Fatalf("recovery fabricated captures for %s: %+v err=%v", order.ID, captures, err)
		}
	}
}
