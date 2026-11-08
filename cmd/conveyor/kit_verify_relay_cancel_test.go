package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// These tests pin which relay check failures cancel a run
// (component-verification-runner invariants 21 and 22). A source change,
// claim loss, expiry or revocation cancels the run. A child hangup or a
// transient call failure refuses the signal with 409 and leaves the outcome
// to the child's exit. The relay_hangup.py child sends its steps one at a
// time. A hangup step closes its sending side only after a check stand-in,
// running inside the relay check under test, writes to the step's FIFO gate.
// The stand-in then blocks until the hangup ends the request context. The
// hung-up step reads until the runner closes the connection, which happens
// only after the relay handler returns. The FIFO, channels and context
// cancellation order every step, so no sleep or timing budget decides a
// result.

// kitRelayCancelObservation is what a check stand-in saw on the relay's first
// call of the check under test.
type kitRelayCancelObservation struct {
	liveAtEntry bool
	err         error
}

type kitRelayCancelLogEntry struct {
	Step               int    `json:"step"`
	Mode               string `json:"mode"`
	Type               string `json:"type"`
	Status             int    `json:"status"`
	State              string `json:"state"`
	DispatchAuthorized *bool  `json:"dispatch_authorized"`
}

// kitRelayCancelExercise runs relay_hangup.py with exit status 0 and the given
// steps. A child left running fails the attempt at the exercise deadline
// instead of hanging the test.
func kitRelayCancelExercise(t *testing.T, operations bool, steps ...string) verification.Exercise {
	t.Helper()
	fixture, err := filepath.Abs("testdata/verification-kit-runner/relay_hangup.py")
	if err != nil {
		t.Fatal(err)
	}
	e := verification.Exercise{ID: "relay", Kind: "script", Argv: append([]string{"python3", fixture, "0"}, steps...), TimeoutSeconds: 60, RequiredAssertions: []verification.Assertion{}, Operations: []verification.Operation{}}
	if operations {
		e.Operations = []verification.Operation{{ID: "create", TargetBinding: "fixture"}}
	}
	return e
}

func readKitRelayCancelLog(t *testing.T, attemptRoot string) []kitRelayCancelLogEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(attemptRoot, "run", "relay-log.json"))
	if err != nil {
		t.Fatalf("child relay log: %v; child output: %s", err, kitRelayChildOutput(attemptRoot))
	}
	var entries []kitRelayCancelLogEntry
	if err = json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func kitRelayCancelStatuses(entries []kitRelayCancelLogEntry) []int {
	statuses := []int{}
	for _, entry := range entries {
		statuses = append(statuses, entry.Status)
	}
	return statuses
}

// kitRelayCancelReleaseHangup lets the child's pending hangup step close its
// sending side. Opening the FIFO waits for the child to open its read end.
func kitRelayCancelReleaseHangup(attemptRoot string) error {
	gate, err := os.OpenFile(filepath.Join(attemptRoot, "run", "hangup-gate"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer gate.Close()
	_, err = gate.Write([]byte{1})
	return err
}

// kitRelayCancelFirst returns what the stand-in observed on the relay's first
// check. launch has returned by then, so an empty channel means the relay
// never ran that check.
func kitRelayCancelFirst[T any](t *testing.T, observed chan T) T {
	t.Helper()
	select {
	case value := <-observed:
		return value
	default:
		t.Fatal("the relay never ran the check under test")
		panic("unreachable")
	}
}

// kitRelayCancelRecorded lists the relayed state_observation keys the
// fixture server recorded. The caller holds f.mu.
func kitRelayCancelRecorded(f *kitExecutionFixture) []string {
	keys := []string{}
	for _, record := range f.snapshot.Evidence {
		if record.Envelope.Type == "state_observation" {
			keys = append(keys, record.Envelope.SubmissionKey)
		}
	}
	return keys
}

// A child that hangs up while its relay is inside the checkout check does
// not cancel the run. The check fails only because the request context ended,
// and the outcome follows the child's exit.
func TestKitRelayCancelHangupDuringCheckoutCheck(t *testing.T) {
	t.Parallel()
	e := kitRelayCancelExercise(t, false, "hangup:evidence:item-1", "send:evidence:item-2")
	f := newKitExecutionFixture(t, e)
	attemptRoot := t.TempDir()
	var calls atomic.Int32
	first := make(chan kitRelayCancelObservation, 1)
	f.v.relayCheckout = func(ctx context.Context) error {
		if calls.Add(1) != 1 {
			return f.v.checkCheckout(ctx)
		}
		// The child hangs up while the relay is inside this check; the real
		// check then runs under the ended request context.
		observed := kitRelayCancelObservation{liveAtEntry: ctx.Err() == nil}
		if err := kitRelayCancelReleaseHangup(attemptRoot); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		observed.err = f.v.checkCheckout(ctx)
		first <- observed
		return observed.err
	}
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	if observed := kitRelayCancelFirst(t, first); !observed.liveAtEntry || observed.err == nil {
		t.Fatalf("relay checkout check: live at entry=%v err=%v, want a check that fails after a hangup inside it", observed.liveAtEntry, observed.err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || f.outcome != "succeeded" {
		t.Fatalf("hangup during the checkout check: outcome=%q err=%v child output: %s", f.outcome, err, kitRelayChildOutput(attemptRoot))
	}
	log := readKitRelayCancelLog(t, attemptRoot)
	if got := kitRelayCancelStatuses(log); !slices.Equal(got, []int{http.StatusConflict, http.StatusOK}) || log[1].State != "recorded" {
		t.Fatalf("relay answers %v, want the hung-up signal refused with 409 and the next recorded: %+v", got, log)
	}
	if got := kitRelayCancelRecorded(f); !slices.Equal(got, []string{"item-2"}) {
		t.Fatalf("recorded relayed evidence %v, want only item-2", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("relay checkout checks %d, want 2", calls.Load())
	}
}

// A child that hangs up during the relay's liveness check does not cancel
// the run. The liveness call fails only because the request context ended,
// records no authority loss, and the outcome follows the child's exit.
func TestKitRelayCancelHangupDuringLiveCheck(t *testing.T) {
	t.Parallel()
	e := kitRelayCancelExercise(t, true, "hangup:operation.dispatching:create", "send:operation.dispatching:create", "send:operation.completed:create")
	f := newKitExecutionFixture(t, e)
	attemptRoot := t.TempDir()
	var calls atomic.Int32
	first := make(chan kitRelayCancelObservation, 1)
	f.v.relayLive = func(ctx context.Context, grantID string) error {
		if calls.Add(1) != 1 {
			return f.v.live(ctx, grantID)
		}
		// The child hangs up while the relay is inside this check, after the
		// checkout check passed; the real liveness call then runs under the
		// ended request context.
		observed := kitRelayCancelObservation{liveAtEntry: ctx.Err() == nil}
		if err := kitRelayCancelReleaseHangup(attemptRoot); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		observed.err = f.v.live(ctx, grantID)
		first <- observed
		return observed.err
	}
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	if observed := kitRelayCancelFirst(t, first); !observed.liveAtEntry || observed.err == nil {
		t.Fatalf("relay liveness check: live at entry=%v err=%v, want a check that fails after a hangup inside it", observed.liveAtEntry, observed.err)
	}
	if lost := f.v.cachedLive(); lost != nil {
		t.Fatalf("a hangup recorded authority loss: %v", lost)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || f.outcome != "succeeded" {
		t.Fatalf("hangup during the liveness check: outcome=%q err=%v child output: %s", f.outcome, err, kitRelayChildOutput(attemptRoot))
	}
	log := readKitRelayCancelLog(t, attemptRoot)
	if got := kitRelayCancelStatuses(log); !slices.Equal(got, []int{http.StatusConflict, http.StatusOK, http.StatusOK}) || log[1].DispatchAuthorized == nil || !*log[1].DispatchAuthorized {
		t.Fatalf("relay answers %v, want the hung-up signal refused with 409, then an authorized dispatch and a completion: %+v", got, log)
	}
	if !slices.Equal(f.operations, []string{"prepare", "dispatching", "completed"}) {
		t.Fatalf("server operation record %v; the hung-up dispatch must not reach the server", f.operations)
	}
}

// A transient renew_work_order failure during an operation signal, while the
// known lease holds, refuses the signal with 409 and does not cancel the run.
func TestKitRelayCancelTransientRenewalFailure(t *testing.T) {
	t.Parallel()
	e := kitRelayCancelExercise(t, true, "send:operation.dispatching:create", "send:operation.dispatching:create", "send:operation.completed:create")
	f := newKitExecutionFixture(t, e)
	var calls atomic.Int32
	firstErr := make(chan error, 1)
	f.v.relayLive = func(ctx context.Context, grantID string) error {
		if calls.Add(1) != 1 {
			return f.v.live(ctx, grantID)
		}
		// The server answers 503 for the whole call. A monitor renewal in
		// this window fails transiently too, which the monitor tolerates.
		f.mu.Lock()
		f.offline = true
		f.mu.Unlock()
		err := f.v.live(ctx, grantID)
		f.mu.Lock()
		f.offline = false
		f.mu.Unlock()
		firstErr <- err
		return err
	}
	attemptRoot := t.TempDir()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	if liveErr := kitRelayCancelFirst(t, firstErr); !transientWorkerError(liveErr) {
		t.Fatalf("relay liveness error %v, want a transient renewal failure", liveErr)
	}
	if lost := f.v.cachedLive(); lost != nil {
		t.Fatalf("a transient renewal failure recorded authority loss: %v", lost)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || f.outcome != "succeeded" {
		t.Fatalf("transient renewal failure: outcome=%q err=%v child output: %s", f.outcome, err, kitRelayChildOutput(attemptRoot))
	}
	log := readKitRelayCancelLog(t, attemptRoot)
	if got := kitRelayCancelStatuses(log); !slices.Equal(got, []int{http.StatusConflict, http.StatusOK, http.StatusOK}) || log[1].DispatchAuthorized == nil || !*log[1].DispatchAuthorized {
		t.Fatalf("relay answers %v, want the signal refused with 409, then an authorized dispatch and a completion: %+v", got, log)
	}
	if !slices.Equal(f.operations, []string{"prepare", "dispatching", "completed"}) {
		t.Fatalf("server operation record %v; the refused dispatch must not reach the server", f.operations)
	}
}

// A real source change that the relay sees while the request context is
// live still cancels the run. The stand-in changes the checkout, runs the
// real check, and restores the checkout before the relay acts, so the
// post-exit check finds it clean and the outcome reads cancelled.
func TestKitRelayCancelSourceChange(t *testing.T) {
	t.Parallel()
	e := kitRelayCancelExercise(t, false, "send:evidence:item-1", "send:evidence:item-2")
	f := newKitExecutionFixture(t, e)
	var calls atomic.Int32
	type observation struct {
		err  error
		live bool
	}
	first := make(chan observation, 1)
	changed := filepath.Join(f.v.root, "relay-source-change")
	f.v.relayCheckout = func(ctx context.Context) error {
		if calls.Add(1) != 1 {
			return f.v.checkCheckout(ctx)
		}
		if err := os.WriteFile(changed, []byte("changed"), 0600); err != nil {
			t.Error(err)
		}
		err := f.v.checkCheckout(ctx)
		if removeErr := os.Remove(changed); removeErr != nil {
			t.Error(removeErr)
		}
		first <- observation{err: err, live: ctx.Err() == nil}
		return err
	}
	attemptRoot := t.TempDir()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	observed := kitRelayCancelFirst(t, first)
	if observed.err == nil || !strings.Contains(observed.err.Error(), "dirty") || !observed.live {
		t.Fatalf("relay check: err=%v live=%v, want a dirty checkout seen under a live request", observed.err, observed.live)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil || f.outcome != "cancelled" {
		t.Fatalf("source change seen by the relay: outcome=%q err=%v child output: %s", f.outcome, err, kitRelayChildOutput(attemptRoot))
	}
	if got := kitRelayCancelRecorded(f); len(got) != 0 {
		t.Fatalf("relayed evidence %v recorded after the source change", got)
	}
}

// A refused renewal that the relay sees still records authority loss and
// cancels the run. No outcome is reported after the loss, and the execution
// report written to the output marks the run cancelled.
func TestKitRelayCancelRefusedRenewal(t *testing.T) {
	t.Parallel()
	e := kitRelayCancelExercise(t, true, "send:operation.dispatching:create", "send:operation.completed:create")
	f := newKitExecutionFixture(t, e)
	var calls atomic.Int32
	firstErr := make(chan error, 1)
	f.v.relayLive = func(ctx context.Context, grantID string) error {
		if calls.Add(1) != 1 {
			return f.v.live(ctx, grantID)
		}
		f.mu.Lock()
		f.loseClaim = true
		f.mu.Unlock()
		err := f.v.live(ctx, grantID)
		firstErr <- err
		return err
	}
	attemptRoot := t.TempDir()
	err := f.v.launch(t.Context(), e, f.v.root, attemptRoot, "run", "grant", f.snapshot.Attempts[0].Subject, kitRelayEnvironment(), nil)
	if liveErr := kitRelayCancelFirst(t, firstErr); liveErr == nil || !strings.Contains(liveErr.Error(), "renew_work_order refused") {
		t.Fatalf("relay liveness error %v, want a refused renewal", liveErr)
	}
	if lost := f.v.cachedLive(); lost == nil {
		t.Fatal("a refused renewal did not record authority loss")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "authority loss") || f.outcome != "" {
		t.Fatalf("refused renewal seen by the relay: outcome=%q err=%v child output: %s", f.outcome, err, kitRelayChildOutput(attemptRoot))
	}
	if !strings.Contains(f.output.String(), `"cancelled":true`) {
		t.Fatalf("execution report does not mark the run cancelled: %s", f.output.String())
	}
	if !slices.Equal(f.operations, []string{"prepare"}) {
		t.Fatalf("server operation record %v; no signal may reach the server after authority loss", f.operations)
	}
}
