package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// pollingFixture drives a kit verifier's polling schedule on a virtual clock,
// so a ten-minute observation proves its server call rate without waiting
// (component-verification-runner invariant 22).
type pollingFixture struct {
	*kitExecutionFixture
	mu    sync.Mutex
	clock time.Time
}

func newPollingFixture(t *testing.T) *pollingFixture {
	t.Helper()
	e := verification.Exercise{ID: "check", Kind: "script", Argv: []string{"sh", "-c", "true"}, TimeoutSeconds: 5}
	f := newKitExecutionFixture(t, e)
	// The fixture's own lease is a minute long; a virtual ten-minute run needs
	// bounds the real clock still honours.
	bound := time.Now().Add(2 * time.Hour)
	f.v.order.LeaseExpiresAt = bound
	f.v.order.ExecutionDeadline = bound
	f.order.LeaseExpiresAt = bound
	f.renewExtends = time.Hour
	p := &pollingFixture{kitExecutionFixture: f, clock: time.Now()}
	f.v.now = p.now
	return p
}

func (p *pollingFixture) now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clock
}

func (p *pollingFixture) advance(d time.Duration) {
	p.mu.Lock()
	p.clock = p.clock.Add(d)
	p.mu.Unlock()
}

func (p *pollingFixture) calls() (renew, context int) {
	p.kitExecutionFixture.mu.Lock()
	defer p.kitExecutionFixture.mu.Unlock()
	return p.kitExecutionFixture.renewCalls, p.kitExecutionFixture.contextCalls
}

// TestKitLivePollingKeepsServerCallRateBounded proves a ten-minute observation
// issues one renewal per capped lease share and one context read per 5-15 s
// band instead of two calls per second (component-verification-runner invariant
// 22).
func TestKitLivePollingKeepsServerCallRateBounded(t *testing.T) {
	p := newPollingFixture(t)
	ctx := t.Context()
	if err := p.v.live(ctx, "grant"); err != nil {
		t.Fatal(err)
	}
	const seconds = 600
	for range seconds {
		p.advance(time.Second)
		if err := p.v.live(ctx, "grant"); err != nil {
			t.Fatal(err)
		}
	}
	renew, contexts := p.calls()
	maxRenew := 1 + seconds/int(kitRenewIntervalCeiling/time.Second)
	if renew > maxRenew {
		t.Fatalf("ten minutes issued %d renewals, want at most %d", renew, maxRenew)
	}
	if renew < 1 {
		t.Fatal("the observation never renewed its claim")
	}
	// The first check reads the context, then the interval doubles up to the
	// maximum.
	if want := seconds / int(kitContextPollMax/time.Second); contexts < want {
		t.Fatalf("ten minutes issued %d context reads, want at least %d to poll every %s", contexts, want, kitContextPollMax)
	}
	if limit := seconds / 5; contexts > limit {
		t.Fatalf("ten minutes issued %d context reads, want at most %d", contexts, limit)
	}
	if contexts >= seconds {
		t.Fatalf("the observation still reads the context every tick: %d reads", contexts)
	}
}

func TestKitLiveContextBackoffResetsOnChange(t *testing.T) {
	p := newPollingFixture(t)
	ctx := t.Context()
	check := func(advance time.Duration, want int) {
		t.Helper()
		p.advance(advance)
		if err := p.v.live(ctx, "grant"); err != nil {
			t.Fatal(err)
		}
		_, got := p.calls()
		if got != want {
			t.Fatalf("after %s: got %d context reads, want %d", advance, got, want)
		}
	}
	check(0, 1)
	check(4*time.Second, 1)
	check(time.Second, 2)
	check(9*time.Second, 2)
	check(time.Second, 3)
	p.kitExecutionFixture.mu.Lock()
	p.kitExecutionFixture.snapshot.Contexts[0].ID = "changed"
	p.kitExecutionFixture.mu.Unlock()
	check(15*time.Second, 4)
	check(4*time.Second, 4)
	check(time.Second, 5)
}

// TestKitLiveNoticesLostClaimWithinOneContextInterval proves a claim the server
// no longer holds stops the run inside one context interval, even though the
// next renewal is a whole lease share away (component-verification-runner
// invariant 22).
func TestKitLiveNoticesLostClaimWithinOneContextInterval(t *testing.T) {
	p := newPollingFixture(t)
	ctx := t.Context()
	if err := p.v.live(ctx, "grant"); err != nil {
		t.Fatal(err)
	}
	p.kitExecutionFixture.mu.Lock()
	p.kitExecutionFixture.contextFails = true
	p.kitExecutionFixture.mu.Unlock()
	start := p.now()
	detected := time.Duration(-1)
	for range int(kitContextPollMax/time.Second) + 1 {
		p.advance(time.Second)
		if err := p.v.live(ctx, "grant"); err != nil {
			detected = p.now().Sub(start)
			break
		}
	}
	if detected < 0 || detected > kitContextPollMax {
		t.Fatalf("lost claim noticed after %s, want at most %s", detected, kitContextPollMax)
	}
}

// TestKitLiveNoticesExpiredLeaseWithoutAServerCall proves the cached lease bound
// still stops the run, so a slow control plane cannot extend a dead claim
// (component-verification-runner).
func TestKitLiveNoticesExpiredLeaseWithoutAServerCall(t *testing.T) {
	p := newPollingFixture(t)
	ctx := t.Context()
	if err := p.v.live(ctx, "grant"); err != nil {
		t.Fatal(err)
	}
	renew, contexts := p.calls()
	p.kitExecutionFixture.mu.Lock()
	p.kitExecutionFixture.order.LeaseExpiresAt = time.Now().Add(-time.Second)
	p.kitExecutionFixture.mu.Unlock()
	p.v.mu.Lock()
	p.v.order.LeaseExpiresAt = time.Now().Add(-time.Second)
	p.v.mu.Unlock()
	if err := p.v.live(ctx, "grant"); err == nil {
		t.Fatal("live accepted an expired lease")
	}
	afterRenew, afterContexts := p.calls()
	if afterRenew != renew || afterContexts != contexts {
		t.Fatalf("the expired lease made %d renewals and %d context reads, want none", afterRenew-renew, afterContexts-contexts)
	}
}

func TestKitObservationAcceptsEvidenceAtDeadline(t *testing.T) {
	e := verification.Exercise{ID: "observe", Kind: "observation", TimeoutSeconds: 1, EvidenceOutputs: []verification.EvidenceOutput{{Type: "state_observation", MinimumItems: 1}}}
	f := newKitExecutionFixture(t, e)
	f.onContext = func(calls int) {
		if calls == 2 {
			f.snapshot.Evidence = append(f.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: core.VerificationEvidence{Type: "state_observation", RunID: "run", Subject: f.snapshot.Attempts[0].Subject}})
		}
	}
	if err := f.v.observe(t.Context(), e, "run", "grant"); err != nil {
		t.Fatal(err)
	}
	if f.outcome != "succeeded" {
		t.Fatalf("outcome %q, want succeeded", f.outcome)
	}
}

func TestKitObservationPollingKeepsProgressAndCallsBounded(t *testing.T) {
	p := newPollingFixture(t)
	e := verification.Exercise{ID: "check", Kind: "observation", TimeoutSeconds: 30, EvidenceOutputs: []verification.EvidenceOutput{{Type: "state_observation", MinimumItems: 1}}}
	p.snapshot.Obligations[0].Contract = e
	p.output.WriteString("existing progress\n")
	var readTimes []time.Time
	p.onContext = func(int) { readTimes = append(readTimes, p.now()) }
	ticks := 0
	p.v.observationWait = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.advance(time.Second)
		ticks++
		if ticks == 600 {
			p.kitExecutionFixture.mu.Lock()
			p.snapshot.Evidence = append(p.snapshot.Evidence, store.VerificationEvidenceRecord{Envelope: core.VerificationEvidence{Type: "state_observation", RunID: "run", Subject: p.snapshot.Attempts[0].Subject}})
			p.kitExecutionFixture.mu.Unlock()
		}
		if ticks > 620 {
			t.Fatal("observation missed completion evidence")
		}
		return nil
	}
	if err := p.v.observe(t.Context(), e, "run", "grant"); err != nil {
		t.Fatal(err)
	}
	renew, contexts := p.calls()
	// The periodic schedule renews at most three times in ten minutes. The
	// outcome adds exactly one fixed-point renewal and reread
	// (component-verification-runner invariants 22 and 26).
	rpcs := p.rpcLog()
	requireRenewThenRereadBefore(t, rpcs, "report_verification_outcome", 0)
	if periodic := renew - 1; periodic > 3 || contexts < 40 || contexts > 122 {
		t.Fatalf("ten-minute observation: %d periodic renewals, %d context reads", periodic, contexts)
	}
	for i := 1; i < len(readTimes)-1; i++ {
		if d := readTimes[i].Sub(readTimes[i-1]); d < 5*time.Second || d > 15*time.Second {
			t.Fatalf("context read interval %s outside 5–15 seconds", d)
		}
	}
	if got := p.output.String(); got != "existing progress\n" {
		t.Fatalf("observation changed progress output: %q", got)
	}
}
