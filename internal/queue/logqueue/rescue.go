package logqueue

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
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
		if err := rt.appendOutcome(ctx, job, kind, payload, now); err != nil && !eventlog.IsVersionConflict(err) {
			return err
		}
		rt.opts.Logf("logqueue: rescued %s attempt %d/%d as %s", job.ID(), job.Attempt, job.MaxAttempts, kind)
	}
	return nil
}
