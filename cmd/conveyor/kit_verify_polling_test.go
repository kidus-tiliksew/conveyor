package main

import (
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// pollingFixture drives a kit verifier's polling schedule on a virtual clock,
// so a ten-minute observation proves its server call rate without waiting
// (component-verification-runner, Polling cadence).
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
// band instead of two calls per second (component-verification-runner, Polling
// cadence).
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
// next renewal is a whole lease share away (component-verification-runner,
// Polling cadence).
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
