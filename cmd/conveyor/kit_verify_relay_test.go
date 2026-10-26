package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type kitRelayLogEntry struct {
	Item   int    `json:"item"`
	Status int    `json:"status"`
	State  string `json:"state"`
}

func kitRelayExercise(t *testing.T, id string, args ...string) verification.Exercise {
	t.Helper()
	fixture, err := filepath.Abs("testdata/verification-kit-runner/relay_evidence.py")
	if err != nil {
		t.Fatal(err)
	}
	return verification.Exercise{ID: id, Kind: "script", Argv: append([]string{"python3", fixture}, args...), TimeoutSeconds: 120, RequiredAssertions: []verification.Assertion{}, Operations: []verification.Operation{}}
}

func kitRelayEnvironment() core.VerificationEnvironment {
	return core.VerificationEnvironment{Target: "fixture", OS: "unknown", Architecture: "unknown", Runtime: "fixture", Deployment: "unknown", Attributes: map[string]string{}}
}

func readKitRelayLog(t *testing.T, attemptRoot string) []kitRelayLogEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(attemptRoot, "run", "relay-log.json"))
	if err != nil {
		t.Fatalf("child relay log: %v", err)
	}
	var entries []kitRelayLogEntry
	if err = json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// A remote server that answers every call in 6–8 s must not fail an exercise
// whose child relays evidence with a 2 s per-request timeout: the relay
// answers from the spool and the attempt drains every upload before it
// reports (component-verification-runner).
func TestKitRunnerSlowServerRecordsRelayedEvidence(t *testing.T) {
	t.Parallel()
	e := kitRelayExercise(t, "relay", "3", "2", "64", "0")
	e.EvidenceOutputs = []verification.EvidenceOutput{{Type: "state_observation", SchemaVersion: 1, MinimumItems: 3}}
	f := newKitExecutionFixture(t, e)
	f.mu.Lock()
	f.order.LeaseExpiresAt = time.Now().Add(10 * time.Minute)
	f.order.ExecutionDeadline = time.Now().Add(10 * time.Minute)
	f.v.order = f.order
	f.mu.Unlock()
	f.responseDelay.Store(int64(6 * time.Second))
	f.responseJitter.Store(int64(2 * time.Second))
	attemptRoot := t.TempDir()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || f.outcome != "succeeded" {
		t.Fatalf("slow server attempt: outcome=%q err=%v", f.outcome, err)
	}
	for _, entry := range readKitRelayLog(t, attemptRoot) {
		if entry.Status != http.StatusOK || entry.State != "recorded" {
			t.Fatalf("relay not acknowledged within the child timeout: %+v", entry)
		}
	}
	recorded := map[string]bool{}
	for _, record := range f.snapshot.Evidence {
		if record.Envelope.Type == "state_observation" {
			recorded[record.Envelope.SubmissionKey] = true
		}
	}
	for _, key := range []string{"item-1", "item-2", "item-3"} {
		if !recorded[key] {
			t.Fatalf("relayed evidence %s not recorded: %v", key, recorded)
		}
	}
	entries, err := os.ReadDir(filepath.Join(attemptRoot, "run", "spool"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool not drained: %v %v", entries, err)
	}
}

// A relay acknowledged from the spool is not a success path: when the claim
// is lost before the spool drains, the attempt reports no outcome and the
// entry stays spooled for a rerun (component-verification-runner).
func TestKitRunnerSpooledRelayClaimLossFailsAttempt(t *testing.T) {
	e := kitRelayExercise(t, "relay", "1", "2", "16", "20")
	e.TimeoutSeconds = 30
	f := newKitExecutionFixture(t, e)
	attempted := make(chan struct{})
	var once sync.Once
	f.evidenceUnavailable = true
	// The first upload attempt ends the claim on the server; the runner
	// learns it from its next renewal.
	f.onEvidence = func() {
		f.loseClaim = true
		once.Do(func() { close(attempted) })
	}
	attemptRoot := t.TempDir()
	started := time.Now()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	select {
	case <-attempted:
	default:
		t.Fatal("relayed evidence upload was never attempted")
	}
	if err == nil || time.Since(started) > 15*time.Second {
		t.Fatalf("claim loss before drain: err=%v after %s", err, time.Since(started))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcome != "" || f.uploads != 0 || len(f.snapshot.Evidence) != 0 {
		t.Fatalf("outcome or evidence written after claim loss: outcome=%q uploads=%d", f.outcome, f.uploads)
	}
	log := readKitRelayLog(t, attemptRoot)
	if len(log) != 1 || log[0].Status != http.StatusOK || log[0].State != "recorded" {
		t.Fatalf("relay was not acknowledged from the spool: %+v", log)
	}
	if _, err := os.Stat(filepath.Join(attemptRoot, "run", "spool", "item-1.json")); err != nil {
		t.Fatalf("acknowledged evidence not retained after claim loss: %v", err)
	}
}

// The monitor keeps renewing the claim while relayed evidence drains, so a
// drain that outlasts the lease held at child exit still succeeds
// (component-verification-runner).
func TestKitRunnerDrainKeepsClaimRenewed(t *testing.T) {
	t.Parallel()
	e := kitRelayExercise(t, "relay", "3", "2", "16", "0")
	e.EvidenceOutputs = []verification.EvidenceOutput{{Type: "state_observation", SchemaVersion: 1, MinimumItems: 3}}
	f := newKitExecutionFixture(t, e)
	f.mu.Lock()
	f.renewExtends = 3 * time.Second
	f.order.LeaseExpiresAt = time.Now().Add(3 * time.Second)
	f.v.order = f.order
	f.mu.Unlock()
	f.evidenceDelay.Store(int64(2 * time.Second))
	attemptRoot := t.TempDir()
	started := time.Now()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || f.outcome != "succeeded" {
		t.Fatalf("drain beyond the exit-time lease: outcome=%q err=%v", f.outcome, err)
	}
	// Three relayed uploads and the execution report take at least 8 s, well
	// past the 3 s lease held when the child exited.
	if elapsed := time.Since(started); elapsed < 8*time.Second {
		t.Fatalf("drain did not outlast the lease: %s", elapsed)
	}
	if f.uploads != 4 {
		t.Fatalf("uploads %d, want three relayed items and the execution report", f.uploads)
	}
}

// A relay beyond the spool bound answers 409, so the child sees back-pressure
// instead of an acknowledgement for unrecorded evidence.
func TestKitRunnerSpoolFullRefusesRelay(t *testing.T) {
	limit, backoff := kitEvidenceSpoolLimit, kitUploadBackoff
	kitEvidenceSpoolLimit, kitUploadBackoff = 6<<10, 10*time.Millisecond
	t.Cleanup(func() { kitEvidenceSpoolLimit, kitUploadBackoff = limit, backoff })
	e := kitRelayExercise(t, "relay", "4", "2", "2048", "0")
	f := newKitExecutionFixture(t, e)
	f.evidenceUnavailable = true
	attemptRoot := t.TempDir()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	if err == nil || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("unuploaded evidence must fail the attempt: %v", err)
	}
	f.mu.Lock()
	outcome := f.outcome
	f.mu.Unlock()
	if outcome == "succeeded" {
		t.Fatal("attempt succeeded with unuploaded evidence")
	}
	log := readKitRelayLog(t, attemptRoot)
	if log[0].Status != http.StatusOK || log[len(log)-1].Status != http.StatusConflict {
		t.Fatalf("spool bound not enforced as back-pressure: %+v", log)
	}
	if _, err := os.Stat(filepath.Join(attemptRoot, "run", "spool", "item-1.json")); err != nil {
		t.Fatalf("acknowledged evidence not retained: %v", err)
	}
}

func TestKitRPCTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, 999 * time.Millisecond, 10*time.Minute + time.Second} {
		if validateKitRPCTimeout(d) == nil {
			t.Fatalf("out-of-range --rpc-timeout %s accepted", d)
		}
	}
	for _, d := range []time.Duration{time.Second, kitDefaultRPCTimeout, 10 * time.Minute} {
		if err := validateKitRPCTimeout(d); err != nil {
			t.Fatalf("--rpc-timeout %s refused: %v", d, err)
		}
	}
	if kitTeardownTimeout == kitDefaultRPCTimeout {
		t.Fatal("teardown shares the transport ceiling")
	}
	cmd := kitVerifyCmd()
	if flag := cmd.Flags().Lookup("rpc-timeout"); flag == nil || flag.DefValue != "30s" {
		t.Fatalf("default transport ceiling: %+v", flag)
	}
	cmd.SetArgs([]string{"task", "--rpc-timeout", "0s"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--rpc-timeout") {
		t.Fatalf("out-of-range flag reached the server path: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": false, "content": []map[string]string{{"type": "text", "text": "{}"}}}})
	}))
	t.Cleanup(server.Close)
	rpc := kitRPC{client: &client{base: server.URL, token: "factory-secret-fixture", workspace: "demo"}, order: "order", session: "session", claimToken: "claim-secret-fixture"}
	call := func(ctx context.Context, rpc kitRPC) (time.Duration, error) {
		started := time.Now()
		err := rpc.call(ctx, "get_work_order", nil, nil)
		return time.Since(started), err
	}
	// A zero timeout selects the default ceiling, which admits a 1.5 s call.
	if elapsed, err := call(t.Context(), rpc); err != nil || elapsed < 1500*time.Millisecond {
		t.Fatalf("default ceiling: %v after %s", err, elapsed)
	}
	rpc.timeout = 300 * time.Millisecond
	elapsed, err := call(t.Context(), rpc)
	if err == nil || elapsed > time.Second {
		t.Fatalf("configured ceiling did not bound the call: %v after %s", err, elapsed)
	}
	if !transientWorkerError(err) {
		t.Fatalf("call deadline must be transient for upload retry: %v", err)
	}
	rpc.timeout = time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if elapsed, err = call(ctx, rpc); err == nil || elapsed > time.Second {
		t.Fatalf("caller deadline shorter than the ceiling ignored: %v after %s", err, elapsed)
	}
}
