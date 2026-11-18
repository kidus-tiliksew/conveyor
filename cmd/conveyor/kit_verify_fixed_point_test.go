package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// The fixed-point tests hold a long lease on a virtual clock that does not
// move, so no periodic renewal and no periodic context read is ever due. A
// renewal that still precedes the start or the outcome can only come from
// the unconditional checks of component-verification-runner invariants 17
// and 26.

// newFixedPointFixture is a polling fixture with an ordinary script subject,
// its unrevoked grant, and operator kit_permissions, primed by one periodic
// check so the last successful renewal and context read are recent.
func newFixedPointFixture(t *testing.T, script string) (*pollingFixture, store.VerificationSubjectContract) {
	t.Helper()
	p := newPollingFixture(t)
	e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", script}, TimeoutSeconds: 5, RequiredAssertions: []verification.Assertion{}, Operations: []verification.Operation{}}
	p.snapshot.Obligations[0].Contract = e
	subject := store.VerificationSubjectContract{Subject: p.snapshot.Attempts[0].Subject, Contract: e}
	p.snapshot.Attempts, p.v.snapshot.Attempts = nil, nil
	p.snapshot.PermissionGrants = []store.VerificationPermissionGrant{{ID: "grant", Subject: subject.Subject, Actions: []core.VerificationPermission{}}}
	p.v.snapshot.PermissionGrants = p.snapshot.PermissionGrants
	p.v.config = &config.Config{KitPermissions: []config.KitPermissionGrant{{Server: p.v.rpc.client.base, Workspace: "demo", Repository: "repo", Binding: "repo", Actions: []verification.VerificationPermission{}}}}
	p.v.configPath = "/operator/conveyor.yaml"
	if err := p.v.live(t.Context(), "grant"); err != nil {
		t.Fatal(err)
	}
	p.resetRPCs()
	return p, subject
}

func (p *pollingFixture) resetRPCs() {
	p.kitExecutionFixture.mu.Lock()
	defer p.kitExecutionFixture.mu.Unlock()
	p.kitExecutionFixture.rpcs = nil
}

func (p *pollingFixture) rpcLog() []string {
	p.kitExecutionFixture.mu.Lock()
	defer p.kitExecutionFixture.mu.Unlock()
	return slices.Clone(p.kitExecutionFixture.rpcs)
}

// requireRenewThenRereadBefore requires that call appears exactly once and
// that the calls immediately before it are a renewal and then a context
// reread, with at most extra further context reads between them and call.
func requireRenewThenRereadBefore(t *testing.T, rpcs []string, call string, extra int) {
	t.Helper()
	at := slices.Index(rpcs, call)
	if at < 0 || slices.Index(rpcs[at+1:], call) >= 0 {
		t.Fatalf("want exactly one %s, got %v", call, rpcs)
	}
	renew := lastIndex(rpcs[:at], "renew_work_order")
	if renew < 0 {
		t.Fatalf("no renewal precedes %s: %v", call, rpcs)
	}
	between := rpcs[renew+1 : at]
	if len(between) == 0 || len(between) > 1+extra {
		t.Fatalf("between the last renewal and %s: %v, want one to %d context reads (%v)", call, between, 1+extra, rpcs)
	}
	for _, name := range between {
		if name != "get_verification_context" {
			t.Fatalf("between the last renewal and %s: %v, want only context reads (%v)", call, between, rpcs)
		}
	}
}

func lastIndex(values []string, value string) int {
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] == value {
			return i
		}
	}
	return -1
}

func count(values []string, value string) int {
	n := 0
	for _, v := range values {
		if v == value {
			n++
		}
	}
	return n
}

// TestKitRunnerFixedPointsRenewAndRereadUnderLongLease proves a run with a
// recent renewal and a long lease still renews and rereads immediately before
// start_verification_attempt and, after the spool drains, immediately before
// report_verification_outcome, while every periodic and relay check between
// them stays on the invariant 22 cadence and renews nothing
// (component-verification-runner invariants 17, 22, and 26).
func TestKitRunnerFixedPointsRenewAndRereadUnderLongLease(t *testing.T) {
	p, subject := newFixedPointFixture(t, "exit 0")
	if err := p.v.run(t.Context(), subject, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if p.outcome != "succeeded" || p.starts != 1 {
		t.Fatalf("outcome %q starts %d, want succeeded and one start", p.outcome, p.starts)
	}
	rpcs := p.rpcLog()
	requireRenewThenRereadBefore(t, rpcs, "start_verification_attempt", 0)
	// A succeeded run reads the context once more to validate its evidence.
	requireRenewThenRereadBefore(t, rpcs, "report_verification_outcome", 1)
	// The outcome's renewal follows the drained spool: no evidence upload
	// comes after it.
	if lastIndex(rpcs, "submit_verification_evidence") > lastIndex(rpcs, "renew_work_order") || lastIndex(rpcs, "submit_verification_evidence") < 0 {
		t.Fatalf("the outcome renewal does not follow the spool drain: %v", rpcs)
	}
	// Exactly the two fixed points renewed. The monitor's periodic checks and
	// the relay barriers renewed nothing, because no lease share elapsed.
	if got := count(rpcs, "renew_work_order"); got != 2 {
		t.Fatalf("the run renewed %d times, want only the two fixed points: %v", got, rpcs)
	}
}

// TestKitRunnerObservationOutcomeRenewsAndRereads proves the observation
// path renews and rereads before its outcome under a long lease.
func TestKitRunnerObservationOutcomeRenewsAndRereads(t *testing.T) {
	p, _ := newFixedPointFixture(t, "exit 0")
	e := verification.Exercise{ID: "check", Kind: "observation", TimeoutSeconds: 30, EvidenceOutputs: []verification.EvidenceOutput{{Type: "state_observation", MinimumItems: 1}}}
	p.snapshot.Obligations[0].Contract = e
	p.snapshot.Attempts = []store.VerificationAttempt{{ID: "run", ContextID: "context", Subject: p.snapshot.PermissionGrants[0].Subject, State: "running", GrantID: "grant"}}
	p.snapshot.Evidence = append(p.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: core.VerificationEvidence{Type: "state_observation", RunID: "run", Subject: p.snapshot.PermissionGrants[0].Subject}})
	// The next periodic context read is due, the next renewal is not.
	p.advance(kitContextPollMax)
	if err := p.v.observe(t.Context(), e, "run", "grant"); err != nil {
		t.Fatal(err)
	}
	if p.outcome != "succeeded" {
		t.Fatalf("outcome %q, want succeeded", p.outcome)
	}
	rpcs := p.rpcLog()
	requireRenewThenRereadBefore(t, rpcs, "report_verification_outcome", 0)
	if got := count(rpcs, "renew_work_order"); got != 1 {
		t.Fatalf("the observation renewed %d times, want only the outcome's fixed point: %v", got, rpcs)
	}
}

// TestKitRunnerBlockedOutcomeRenewsAndRereads proves the prelaunch blocked
// outcome renews and rereads before it is reported under a long lease.
func TestKitRunnerBlockedOutcomeRenewsAndRereads(t *testing.T) {
	p, _ := newFixedPointFixture(t, "exit 0")
	dir := t.TempDir()
	err := p.v.prelaunchBlocked(t.Context(), dir, "run", "grant", "exercise executable is unavailable")
	if err == nil || p.outcome != "blocked" {
		t.Fatalf("blocked outcome %q err %v, want a reported blocked outcome", p.outcome, err)
	}
	requireRenewThenRereadBefore(t, p.rpcLog(), "report_verification_outcome", 0)
}

// TestKitRunnerFixedPointRefusalStopsStartAndOutcome proves a refused
// fixed-point renewal or a revocation that only the forced reread can see
// starts nothing and reports nothing, on every outcome path.
func TestKitRunnerFixedPointRefusalStopsStartAndOutcome(t *testing.T) {
	t.Run("renewal refused before start", func(t *testing.T) {
		p, subject := newFixedPointFixture(t, "exit 0")
		p.refuseRenewAfter = p.renewCalls
		if err := p.v.run(t.Context(), subject, t.TempDir()); err == nil {
			t.Fatal("the run started after a refused renewal")
		}
		if rpcs := p.rpcLog(); p.starts != 0 || slices.Contains(rpcs, "start_verification_attempt") || p.outcome != "" {
			t.Fatalf("refused renewal: starts %d outcome %q rpcs %v", p.starts, p.outcome, rpcs)
		}
	})
	t.Run("grant revoked before start", func(t *testing.T) {
		p, subject := newFixedPointFixture(t, "exit 0")
		// Only the server knows the revocation; the runner's own snapshot
		// still holds the grant, and no periodic read is due.
		p.kitExecutionFixture.mu.Lock()
		p.snapshot.PermissionRevocations = []store.VerificationPermissionRevocation{{GrantID: "grant"}}
		p.kitExecutionFixture.mu.Unlock()
		if err := p.v.run(t.Context(), subject, t.TempDir()); err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Fatalf("revoked grant: %v", err)
		}
		if p.starts != 0 || p.outcome != "" {
			t.Fatalf("revoked grant: starts %d outcome %q", p.starts, p.outcome)
		}
	})
	t.Run("renewal refused before the execution outcome", func(t *testing.T) {
		p, subject := newFixedPointFixture(t, "exit 0")
		// The admission renewal succeeds; the outcome renewal is refused.
		p.refuseRenewAfter = p.renewCalls + 1
		if err := p.v.run(t.Context(), subject, t.TempDir()); err == nil {
			t.Fatal("the outcome was reported after a refused renewal")
		}
		rpcs := p.rpcLog()
		if p.starts != 1 || p.outcome != "" || slices.Contains(rpcs, "report_verification_outcome") {
			t.Fatalf("refused outcome renewal: starts %d outcome %q rpcs %v", p.starts, p.outcome, rpcs)
		}
		if p.uploads != 1 {
			t.Fatalf("the execution report was not uploaded before the outcome check: %d uploads", p.uploads)
		}
	})
	t.Run("renewal refused before the observation outcome", func(t *testing.T) {
		p, _ := newFixedPointFixture(t, "exit 0")
		e := verification.Exercise{ID: "check", Kind: "observation", TimeoutSeconds: 30, EvidenceOutputs: []verification.EvidenceOutput{{Type: "state_observation", MinimumItems: 1}}}
		p.snapshot.Obligations[0].Contract = e
		p.snapshot.Attempts = []store.VerificationAttempt{{ID: "run", ContextID: "context", Subject: p.snapshot.PermissionGrants[0].Subject, State: "running", GrantID: "grant"}}
		p.snapshot.Evidence = append(p.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: core.VerificationEvidence{Type: "state_observation", RunID: "run", Subject: p.snapshot.PermissionGrants[0].Subject}})
		// The next periodic context read is due, the next renewal is not.
		p.advance(kitContextPollMax)
		p.refuseRenewAfter = p.renewCalls
		if err := p.v.observe(t.Context(), e, "run", "grant"); err == nil {
			t.Fatal("the observation reported after a refused renewal")
		}
		if rpcs := p.rpcLog(); p.outcome != "" || slices.Contains(rpcs, "report_verification_outcome") {
			t.Fatalf("refused observation renewal: outcome %q rpcs %v", p.outcome, rpcs)
		}
	})
	t.Run("renewal refused before the blocked outcome", func(t *testing.T) {
		p, _ := newFixedPointFixture(t, "exit 0")
		p.refuseRenewAfter = p.renewCalls
		dir := t.TempDir()
		err := p.v.prelaunchBlocked(t.Context(), dir, "run", "grant", "exercise executable is unavailable")
		if err == nil || !strings.Contains(err.Error(), "outcome not reported") {
			t.Fatalf("refused blocked renewal: %v", err)
		}
		if rpcs := p.rpcLog(); p.outcome != "" || slices.Contains(rpcs, "report_verification_outcome") {
			t.Fatalf("refused blocked renewal: outcome %q rpcs %v", p.outcome, rpcs)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "prelaunch.json")); !os.IsNotExist(statErr) {
			t.Fatalf("a refused check still retained the blocked diagnostic: %v", statErr)
		}
	})
}

// TestKitRunnerReplayRenewsAndRereadsBeforeRetainedUpload proves the
// retained-evidence replay passes the run-start fixed point: a first run
// whose evidence upload stays unavailable keeps its spool and reports no
// outcome; a rerun with the same start key, under a long lease with a recent
// renewal, renews and rereads before it uploads the retained evidence, and it
// neither relaunches nor reports (component-verification-runner invariants 17
// and 26).
func TestKitRunnerReplayRenewsAndRereadsBeforeRetainedUpload(t *testing.T) {
	backoff := kitUploadBackoff
	kitUploadBackoff = 10 * time.Millisecond
	t.Cleanup(func() { kitUploadBackoff = backoff })
	p, subject := newFixedPointFixture(t, "exit 0")
	root := t.TempDir()
	p.evidenceUnavailable = true
	if err := p.v.run(t.Context(), subject, root); err == nil || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("first run did not retain its evidence: %v", err)
	}
	if p.starts != 1 || p.outcome != "" {
		t.Fatalf("first run: starts %d outcome %q, want one start and no outcome", p.starts, p.outcome)
	}
	spooled := filepath.Join(root, "run", "spool", "execution.json")
	if _, err := os.Stat(spooled); err != nil {
		t.Fatalf("first run left no retained execution report: %v", err)
	}
	// The rerun reads the context the server now holds, as a rerun's
	// preparation does, so it finds the attempt that owns the start key.
	p.kitExecutionFixture.mu.Lock()
	p.evidenceUnavailable = false
	p.v.snapshot.Attempts = slices.Clone(p.snapshot.Attempts)
	p.kitExecutionFixture.mu.Unlock()
	p.resetRPCs()
	err := p.v.run(t.Context(), subject, root)
	if err == nil || !strings.Contains(err.Error(), "retained evidence retried without relaunch") {
		t.Fatalf("replay: %v", err)
	}
	rpcs := p.rpcLog()
	upload := slices.Index(rpcs, "submit_verification_evidence")
	if upload < 2 || rpcs[0] != "renew_work_order" || rpcs[1] != "get_verification_context" {
		t.Fatalf("replay did not renew and reread before its upload: %v", rpcs)
	}
	if slices.Contains(rpcs, "start_verification_attempt") || slices.Contains(rpcs, "report_verification_outcome") || p.starts != 1 || p.outcome != "" {
		t.Fatalf("replay relaunched or reported: starts %d outcome %q rpcs %v", p.starts, p.outcome, rpcs)
	}
	if count(rpcs, "renew_work_order") != 1 {
		t.Fatalf("replay renewed %d times, want only the run-start fixed point: %v", count(rpcs, "renew_work_order"), rpcs)
	}
	if _, err := os.Stat(spooled); !os.IsNotExist(err) {
		t.Fatal("the acknowledged retained report stayed in the spool")
	}
}
