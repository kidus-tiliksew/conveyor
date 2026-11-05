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

// discardHandlerTimeout bounds one call of a registration's Discarded handler.
// It is detached from shutdown, as an outcome append is, and stays far below
// any rescue threshold, so a handler ends before another replica may take the
// job over (component-durable-queue, The runtime).
const discardHandlerTimeout = 30 * time.Second

// pendingDiscard is a job whose Discarded handler this process must retry. The
// stream's running job is the durable intent; this names the claim this
// process holds on it, at head, and the reason to report.
type pendingDiscard struct {
	head   eventlog.Version
	reason string
}

// rescue reschedules or discards jobs whose claim is older than the
// threshold: their worker is presumed dead. The attempt counts. It also
// retries each discard handler this process owns. One job's failure does not
// stop the others; the joined error makes both callers retry
// (req-task-lifecycle-and-queue AC-6.2; req-bounded-automatic-recovery AC-4.1).
func (rt *Runtime) rescue(ctx context.Context, ws *workspace) error {
	now := rt.opts.Now().UTC()
	type candidate struct {
		job     Job
		pending pendingDiscard
		owned   bool
	}
	rt.mu.Lock()
	// A retry whose job moved on, as this process's tail shows, is no longer
	// this process's to make.
	for stream, pending := range ws.discards {
		if job, ok := ws.jobs[stream]; !ok || job.State != StateRunning || job.Head != pending.head {
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
		owned := retrying && !stale
		if owned || stale {
			stuck = append(stuck, candidate{job: *job, pending: pending, owned: owned})
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
				if err := rt.rescueWithDiscardHandler(ctx, ws, reg, job, reason, c.owned); err != nil {
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

// rescueWithDiscardHandler finalizes an exhausted job through its Discarded
// handler. An expected-version job.claimed naming the next attempt makes this
// replica the only owner: a replica whose view is stale, including one behind
// a newer generation, loses that append and runs nothing. The job stays
// active while it is owned, so no newer generation can start under the
// handler (DEC-50; component-durable-queue, The runtime). A retry by the owner
// reads the stream head first, because another replica's takeover raises no
// notice.
func (rt *Runtime) rescueWithDiscardHandler(ctx context.Context, ws *workspace, reg queue.Registration, job Job, reason string, owned bool) error {
	key := job.Kind + "|" + string(job.Stream)
	rt.mu.Lock()
	if ws.running[key] {
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
	if owned {
		head, err := rt.log.Head(ctx, ws.id, job.Stream)
		if err != nil {
			return fmt.Errorf("read head of %s: %w", job.ID(), err)
		}
		if head != job.Head {
			rt.abandonDiscard(ws, job.Stream)
			return nil
		}
	} else {
		claimed, err := rt.claimDiscard(ctx, ws, job)
		if err != nil {
			if eventlog.IsVersionConflict(err) {
				rt.abandonDiscard(ws, job.Stream)
				return nil
			}
			return fmt.Errorf("claim discard of %s: %w", job.ID(), err)
		}
		job = claimed
	}
	return rt.finishDiscard(ws, reg, job, reason)
}

// claimDiscard appends this process's claim on an exhausted job and applies
// it to the index, as a worker's claim is.
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

// finishDiscard runs the Discarded handler under this process's claim, and
// records job.discarded only after it succeeds. A failed handler or append
// leaves the job running, which is the durable intent, and remembers the
// retry; a restart or another replica finds the job again once its claim goes
// stale. The context is detached from shutdown, so an outcome still lands
// during a drain (component-durable-queue, The runtime).
func (rt *Runtime) finishDiscard(ws *workspace, reg queue.Registration, job Job, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), discardHandlerTimeout)
	defer cancel()
	err := reg.Discarded(ctx, queue.Job{
		WorkspaceID: ws.id, ID: job.ID(), Kind: job.Kind, Attempt: min(job.Attempt, job.MaxAttempts), MaxAttempts: job.MaxAttempts, Args: job.Args,
	}, reason)
	if err != nil {
		rt.rememberDiscard(ws, job, reason)
		return fmt.Errorf("discard handler for %s: %w", job.ID(), err)
	}
	now := rt.opts.Now().UTC()
	if err = rt.appendOutcome(ctx, job, KindDiscarded, outcomePayload{Attempt: job.Attempt, Error: reason}, now); err != nil {
		if eventlog.IsVersionConflict(err) {
			rt.abandonDiscard(ws, job.Stream)
			return nil
		}
		rt.rememberDiscard(ws, job, reason)
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

func (rt *Runtime) rememberDiscard(ws *workspace, job Job, reason string) {
	rt.mu.Lock()
	ws.discards[job.Stream] = pendingDiscard{head: job.Head, reason: reason}
	rt.mu.Unlock()
}

// abandonDiscard drops a discard this process no longer owns and re-reads the
// stream, so a partial history never stands in for the truth.
func (rt *Runtime) abandonDiscard(ws *workspace, stream eventlog.StreamID) {
	rt.mu.Lock()
	delete(ws.discards, stream)
	rt.mu.Unlock()
	rt.refresh(ws, stream)
}
