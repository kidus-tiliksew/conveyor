package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// kitUploadAttempts bounds the transient retries of one spooled entry, and
// kitUploadBackoff is the first retry delay, doubled after each failure
// (component-verification-runner).
const kitUploadAttempts = 5

var kitUploadBackoff = time.Second

// kitEvidenceSpoolLimit bounds one attempt's evidence spool; a relay beyond
// it is refused so the child sees back-pressure.
var kitEvidenceSpoolLimit int64 = 8 << 20

// uploadEvidence submits one sanitized spool entry. A transient failure (a
// 5xx answer, a network error or a call deadline) is retried while the cached
// claim state holds; the server returns the first receipt for an identical
// batch, so a retry after a lost response records nothing twice.
func (v *kitVerifier) uploadEvidence(ctx context.Context, data []byte) error {
	clean, _, err := v.redactor.RedactJSON(data)
	if err != nil {
		return err
	}
	delay := kitUploadBackoff
	for attempt := 1; ; attempt++ {
		var receipt store.VerificationReceipt
		err = v.rpc.call(ctx, "submit_verification_evidence", json.RawMessage(clean), &receipt)
		if err == nil || attempt == kitUploadAttempts || ctx.Err() != nil || !transientWorkerError(err) {
			return err
		}
		if e := v.cachedLive(); e != nil {
			return e
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
}

// kitEvidenceUploader uploads relayed evidence in relay order while the child
// runs. The relay answers the child once an entry is durably spooled; drain
// waits for every queued upload before the attempt reports an outcome
// (component-verification-runner).
type kitEvidenceUploader struct {
	spool   *verification.EvidenceSpool
	upload  func(context.Context, []byte) error
	mu      sync.Mutex
	pending []string
	queued  map[string]bool
	closed  bool
	err     error
	wake    chan struct{}
	done    chan struct{}
}

func startKitEvidenceUploader(ctx context.Context, spool *verification.EvidenceSpool, upload func(context.Context, []byte) error) *kitEvidenceUploader {
	u := &kitEvidenceUploader{spool: spool, upload: upload, queued: map[string]bool{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go u.run(ctx)
	return u
}

// enqueue queues a spooled key once. After the uploader stops, the entry stays
// spooled and the drain-time flush retries it.
func (u *kitEvidenceUploader) enqueue(key string) {
	u.mu.Lock()
	if !u.queued[key] {
		u.queued[key] = true
		u.pending = append(u.pending, key)
	}
	u.mu.Unlock()
	u.signal()
}

func (u *kitEvidenceUploader) signal() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *kitEvidenceUploader) run(ctx context.Context) {
	defer close(u.done)
	for {
		u.mu.Lock()
		if len(u.pending) == 0 {
			closed := u.closed
			u.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-u.wake:
			case <-ctx.Done():
				return
			}
			continue
		}
		key := u.pending[0]
		u.mu.Unlock()
		err := u.spool.Upload(ctx, key, u.upload)
		u.mu.Lock()
		if err != nil {
			// Stop at the first failure so later entries never overtake it.
			u.err = err
			u.mu.Unlock()
			return
		}
		u.pending = u.pending[1:]
		delete(u.queued, key)
		u.mu.Unlock()
	}
}

// drain closes the queue and waits for the uploader to finish or stop.
func (u *kitEvidenceUploader) drain() error {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.signal()
	<-u.done
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// monitor rechecks the checkout and the claim once per second until ctx ends.
// It runs apart from the loop that waits for the child, so a slow call never
// delays observing child exit, cancellation or the deadline. A source change,
// claim loss, expiry or revocation cancels the run; a transient call failure
// does not, and the cached lease bound still applies on the next tick
// (component-verification-runner).
func (v *kitVerifier) monitor(ctx context.Context, grantID string, cancelRun context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := v.checkCheckout(ctx); err != nil {
			if ctx.Err() == nil {
				cancelRun()
			}
			return
		}
		if err := v.live(ctx, grantID); err != nil {
			if ctx.Err() != nil {
				return
			}
			if v.cachedLive() != nil {
				cancelRun()
				return
			}
		}
	}
}
