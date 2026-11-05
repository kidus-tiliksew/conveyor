package logqueue

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// rescue reschedules or discards jobs whose claim is older than the
// threshold: their worker is presumed dead. The attempt counts.
func (rt *Runtime) rescue(ctx context.Context, ws *workspace) error {
	if rt.opts.RescueStuckAfter <= 0 {
		return nil
	}
	now := rt.opts.Now().UTC()
	rt.mu.Lock()
	var stuck []Job
	for _, job := range ws.jobs {
		if job.State == StateRunning && !job.ClaimedAt.IsZero() && now.Sub(job.ClaimedAt) > rt.opts.RescueStuckAfter && !ws.running[job.Kind+"|"+string(job.Stream)] {
			stuck = append(stuck, *job)
		}
	}
	rt.mu.Unlock()
	for _, job := range stuck {
		payload := outcomePayload{Attempt: job.Attempt, Error: "rescued: claim exceeded " + rt.opts.RescueStuckAfter.String()}
		kind := KindRescued
		if job.Attempt >= job.MaxAttempts {
			kind = KindDiscarded
		} else {
			payload.NextAt = now.Add(rt.retryDelay(job.Kind, job.Attempt))
		}
		if err := rt.appendOutcome(ctx, job, kind, payload, now); err != nil {
			if eventlog.IsVersionConflict(err) {
				// Another replica recorded this job's outcome first and owns
				// its discard handler.
				continue
			}
			return err
		}
		rt.opts.Logf("logqueue: rescued %s attempt %d/%d as %s", job.ID(), job.Attempt, job.MaxAttempts, kind)
		if kind == KindDiscarded {
			rt.discarded(ctx, ws, job, payload.Error)
		}
	}
	return nil
}

// discarded runs the registration's discard handler for a job rescue
// discarded, so the job's owner reaches the stop its handler would have
// applied on a failed last attempt (req-task-lifecycle-and-queue AC-6.2;
// req-bounded-automatic-recovery AC-4.1). A failure is logged; the next
// daemon start's checks repair what it left.
func (rt *Runtime) discarded(ctx context.Context, ws *workspace, job Job, reason string) {
	reg, ok := rt.registration(job.Kind)
	if !ok || reg.Discarded == nil {
		return
	}
	err := reg.Discarded(ctx, queue.Job{
		WorkspaceID: ws.id, ID: job.ID(), Kind: job.Kind, Attempt: job.Attempt, MaxAttempts: job.MaxAttempts, Args: job.Args,
	}, reason)
	if err != nil {
		rt.opts.Logf("logqueue: discard handler for %s: %v", job.ID(), err)
	}
}
