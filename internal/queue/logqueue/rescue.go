package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// discardHandlerTimeout caps the protected time of one discard attempt, its
// claim, handler, and discard append together. The context is detached from
// shutdown, as an outcome append is (component-durable-queue, The runtime).
const discardHandlerTimeout = 30 * time.Second

// errDiscardRetryWaiting reports a discard whose next retry is not due yet, so
// both callers keep their retry armed. Each retry appends a claim, so a wake
// must not start one early.
var errDiscardRetryWaiting = errors.New("discard retry is not due yet")

// pendingDiscard is a job whose discard this process must retry. The stream's
// running job is the durable intent. head is this process's view of the stream
// when the attempt ended, handled records that the handler already succeeded,
// so a retry only records the discard, and delay and nextAt space the retries.
type pendingDiscard struct {
	head    eventlog.Version
	reason  string
	handled bool
	delay   time.Duration
	nextAt  time.Time
}

// discardBudget is the protected time of one discard attempt. Half the rescue
// threshold stays beyond it, so another replica cannot take the job over until
// well after the attempt's context has ended, even with some clock skew between
// replicas.
func (rt *Runtime) discardBudget() time.Duration {
	if half := rt.opts.RescueStuckAfter / 2; half > 0 && half < discardHandlerTimeout {
		return half
	}
	return discardHandlerTimeout
}

// rescue reschedules or discards jobs whose claim is older than the
// threshold: their worker is presumed dead. The attempt counts. It also
// retries each discard this process left unfinished. One job's failure does not
// stop the others; the joined error makes both callers retry
// (req-task-lifecycle-and-queue AC-6.2; req-bounded-automatic-recovery AC-4.1).
func (rt *Runtime) rescue(ctx context.Context, ws *workspace) error {
	now := rt.opts.Now().UTC()
	type candidate struct {
		job      Job
		pending  pendingDiscard
		retrying bool
	}
	rt.mu.Lock()
	// A retry whose job moved on, as this process's tail shows, is no longer
	// this process's to make.
	for stream, pending := range ws.discards {
		job, ok := ws.jobs[stream]
		if ok && ws.running[job.Kind+"|"+string(stream)] {
			continue // Its attempt is running and will record the outcome.
		}
		if !ok || job.State != StateRunning || job.Head != pending.head {
			delete(ws.discards, stream)
		}
	}
	var stuck []candidate
	for _, job := range ws.jobs {
		if job.State != StateRunning || ws.running[job.Kind+"|"+string(job.Stream)] {
			continue
		}
		stale := rt.opts.RescueStuckAfter > 0 && !job.ClaimedAt.IsZero() && now.Sub(job.ClaimedAt) > rt.opts.RescueStuckAfter
		pending, retrying := ws.discards[job.Stream]
		if retrying || stale {
			stuck = append(stuck, candidate{job: *job, pending: pending, retrying: retrying})
		}
	}
	rt.mu.Unlock()
	var errs []error
	for _, c := range stuck {
		job := c.job
		reason := "rescued: claim exceeded " + rt.opts.RescueStuckAfter.String()
		if c.pending.reason != "" {
			reason = c.pending.reason
		}
		if job.Attempt >= job.MaxAttempts {
			if reg, ok := rt.registration(job.Kind); ok && reg.Discarded != nil {
				if c.retrying && now.Before(c.pending.nextAt) {
					errs = append(errs, errDiscardRetryWaiting)
					continue
				}
				if err := rt.rescueWithDiscardHandler(ws, reg, job, reason, c.pending.handled); err != nil {
					errs = append(errs, err)
				}
				continue
			}
		}
		payload := outcomePayload{Attempt: job.Attempt, Error: reason}
		kind := KindRescued
		if job.Attempt >= job.MaxAttempts {
			kind = KindDiscarded
		} else {
			payload.NextAt = now.Add(rt.retryDelay(job.Kind, job.Attempt))
		}
		if err := rt.appendOutcome(ctx, job, kind, payload, now); err != nil {
			if !eventlog.IsVersionConflict(err) {
				errs = append(errs, err)
			}
			continue
		}
		rt.opts.Logf("logqueue: rescued %s attempt %d/%d as %s", job.ID(), job.Attempt, job.MaxAttempts, kind)
	}
	return errors.Join(errs...)
}

// rescueWithDiscardHandler runs one discard attempt, unless this process is
// already running one for the job. The poll loop and the alarm both rescue, so
// job can be a snapshot from before the other's attempt ended. Such a snapshot
// does nothing: its claim would conflict with this process's own, and that must
// not read as losing the job to another replica. The rescue whose attempt failed
// keeps the retry armed.
func (rt *Runtime) rescueWithDiscardHandler(ws *workspace, reg queue.Registration, job Job, reason string, handled bool) error {
	key := job.Kind + "|" + string(job.Stream)
	rt.mu.Lock()
	current, ok := ws.jobs[job.Stream]
	if ws.running[key] || !ok || current.Head != job.Head {
		rt.mu.Unlock()
		return nil
	}
	ws.running[key] = true
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		delete(ws.running, key)
		rt.mu.Unlock()
	}()
	return rt.finishDiscard(ws, reg, job, reason, handled)
}

// finishDiscard makes one attempt to finalize an exhausted job. Every attempt,
// first or retry, on rescue or after a handler's last attempt, takes the same
// three steps under one budget, so ownership is never assumed from an earlier
// claim (DEC-50; component-durable-queue, The runtime).
//
//  1. An expected-version job.claimed naming the next attempt proves this
//     process's view is the stream's head and renews the claim. A replica whose
//     view is stale, including one behind a newer generation, loses that append
//     and has no effect. The job stays active, so no newer generation can start
//     while the claim holds.
//  2. The Discarded handler runs. Its context ends within the budget, which is
//     at most half the rescue threshold, so its writes stop before another
//     replica may take the job over.
//  3. job.discarded is recorded only after the handler succeeded.
//
// A failure leaves the job running, which is the durable intent, and remembers
// the retry. A restart or another replica finds the job again once its claim
// goes stale. The context is detached from shutdown, so an outcome still lands
// during a drain.
func (rt *Runtime) finishDiscard(ws *workspace, reg queue.Registration, job Job, reason string, handled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), rt.discardBudget())
	defer cancel()
	claimed, err := rt.claimDiscard(ctx, ws, job)
	if err != nil {
		if eventlog.IsVersionConflict(err) {
			rt.abandonDiscard(ws, job)
			return nil
		}
		rt.rememberDiscard(ws, job, reason, handled)
		return fmt.Errorf("claim discard of %s: %w", job.ID(), err)
	}
	job = claimed
	if !handled {
		err = reg.Discarded(ctx, queue.Job{
			WorkspaceID: ws.id, ID: job.ID(), Kind: job.Kind, Attempt: min(job.Attempt, job.MaxAttempts), MaxAttempts: job.MaxAttempts, Args: job.Args,
		}, reason)
		if err != nil {
			rt.rememberDiscard(ws, job, reason, false)
			return fmt.Errorf("discard handler for %s: %w", job.ID(), err)
		}
	}
	now := rt.opts.Now().UTC()
	if err = rt.appendOutcome(ctx, job, KindDiscarded, outcomePayload{Attempt: job.Attempt, Error: reason}, now); err != nil {
		if eventlog.IsVersionConflict(err) {
			rt.abandonDiscard(ws, job)
			return nil
		}
		rt.rememberDiscard(ws, job, reason, true)
		return fmt.Errorf("record discard of %s: %w", job.ID(), err)
	}
	rt.mu.Lock()
	delete(ws.discards, job.Stream)
	rt.mu.Unlock()
	rt.opts.Logf("logqueue: discarded %s attempt %d/%d", job.ID(), job.Attempt, job.MaxAttempts)
	signal(ws.fold)
	signal(ws.alarmReread)
	return nil
}

// claimDiscard appends this process's claim on an exhausted job and applies it
// to the index, as a worker's claim is. ClaimedAt is read before the append, so
// the claim's age never understates the time the budget has used.
func (rt *Runtime) claimDiscard(ctx context.Context, ws *workspace, job Job) (Job, error) {
	now := rt.opts.Now().UTC()
	attempt := job.Attempt + 1
	claim, err := json.Marshal(claimedPayload{Attempt: attempt, Worker: rt.opts.WorkerID, ClaimedAt: now})
	if err != nil {
		return Job{}, err
	}
	head, err := rt.log.Append(ctx, job.Workspace, job.Stream, job.Head, []eventlog.NewEvent{{
		Kind: KindClaimed, ActorID: rt.opts.WorkerID, ActorRole: "system", Payload: claim, At: now,
	}})
	if err != nil {
		return Job{}, err
	}
	rt.mu.Lock()
	if current, ok := ws.jobs[job.Stream]; ok {
		current.State, current.Attempt, current.ClaimedAt, current.ClaimedBy, current.Head = StateRunning, attempt, now, rt.opts.WorkerID, head
	}
	rt.mu.Unlock()
	job.Attempt, job.ClaimedAt, job.ClaimedBy, job.Head = attempt, now, rt.opts.WorkerID, head
	return job, nil
}

// rememberDiscard records the retry and spaces it on the bounded backoff.
func (rt *Runtime) rememberDiscard(ws *workspace, job Job, reason string, handled bool) {
	now := rt.opts.Now().UTC()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delay := nextLoadRetry(ws.discards[job.Stream].delay)
	ws.discards[job.Stream] = pendingDiscard{head: job.Head, reason: reason, handled: handled, delay: delay, nextAt: now.Add(delay)}
}

// abandonDiscard drops a discard this process no longer owns and re-reads the
// stream, so a partial history never stands in for the truth.
func (rt *Runtime) abandonDiscard(ws *workspace, job Job) {
	rt.opts.Logf("logqueue: %s moved on; another owner finishes its discard", job.ID())
	rt.mu.Lock()
	delete(ws.discards, job.Stream)
	rt.mu.Unlock()
	rt.refresh(ws, job.Stream)
}
