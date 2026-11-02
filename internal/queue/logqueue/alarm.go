package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// The deadline alarm replaces the five-second order clock: one alarm per
// workspace sleeps until the earliest recorded deadline and fires the
// order_clock handler, rescue, and the worker wakes then (DEC-99(4);
// component-durable-queue, The deadline alarm; req-quiet-idle-runtime
// AC-2.4). A fire writes nothing to the log: a tick is not a fact.

// DeadlineSource returns the earliest stored deadline strictly after after
// for the workspace, and false when there is none. The daemon binds it to
// store.DeadlineReader.NextDeadline.
type DeadlineSource func(ctx context.Context, workspace string, after time.Time) (time.Time, bool, error)

const (
	// AlarmSpacing is the least time between the starts of two fires. A fire
	// requested sooner waits, which bounds a burst of notices and the
	// notices a fire's own writes raise.
	AlarmSpacing = time.Second
	// alarmRetryMax caps the retry delay after a fire whose handler or
	// deadline read failed.
	alarmRetryMax = time.Minute
	// rescueMargin moves a foreign claim's rescue time just past the
	// threshold, because rescue acts only on a claim strictly older than it.
	rescueMargin = time.Millisecond
)

// alarmRetryMin is the first retry delay after a fire whose handler or
// deadline read failed; it doubles to alarmRetryMax. Tests shorten it.
var alarmRetryMin = 5 * time.Second

// alarmLoop runs one workspace's deadline alarm.
func (rt *Runtime) alarmLoop(ws *workspace) {
	defer rt.wg.Done()
	// Subscribe before the first read, so a deadline recorded while the
	// first fire runs waits in the subscription.
	notices := rt.hub.Subscribe(ws.id, changes.Deadline)
	defer notices.Close()
	var (
		lastFire   time.Time
		stored     time.Time
		pending    time.Time
		retryAt    time.Time
		retryDelay time.Duration
		ticks      int
	)
	// scheduleRetry keeps due work that a failed fire left undone: the after
	// bound drops times at or before lastFire only once a fire succeeds.
	scheduleRetry := func() {
		retryDelay = min(max(retryDelay*2, alarmRetryMin), alarmRetryMax)
		retryAt = rt.opts.Now().UTC().Add(retryDelay)
	}
	fire := func() {
		ticks++
		start := rt.opts.Now().UTC()
		lastFire = start
		failed := false
		if err := rt.runClock(ws, ticks); err != nil {
			failed = true
		}
		// A failed rescue keeps a retry target: lastFire moves past the stuck
		// claim's rescue time, so nothing else would bring the alarm back.
		if err := rt.rescue(rt.loopCtx, ws); err != nil && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: workspace %s: rescue: %v", ws.id, err)
			failed = true
		}
		rt.wakeWorkers(ws)
		next, err := rt.readDeadline(ws, start)
		if err != nil {
			failed = true
		}
		stored = next
		// A notice consumed before the fire and due by its start is served;
		// a later one keeps its time.
		if !pending.IsZero() && !pending.After(start) {
			pending = time.Time{}
		}
		if failed {
			scheduleRetry()
		} else {
			retryDelay, retryAt = 0, time.Time{}
		}
	}
	fire()
	for {
		target := earliestTime(stored, pending, retryAt, rt.indexDeadline(ws, lastFire))
		var timer *time.Timer
		var due <-chan time.Time
		if !target.IsZero() {
			if spaced := lastFire.Add(AlarmSpacing); target.Before(spaced) {
				target = spaced
			}
			timer = time.NewTimer(max(target.Sub(rt.opts.Now()), 0))
			due = timer.C
		}
		rt.setAlarmTarget(ws, target)
		select {
		case <-rt.loopCtx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-due:
			fire()
		case n := <-notices.C():
			// A notice without a time, from a peer, a listener reconnect, or a
			// coalesced pair, fires at once; so does a past one.
			now := rt.opts.Now().UTC()
			at := n.At
			if at.IsZero() || at.Before(now) {
				at = now
			}
			pending = earliestTime(pending, at)
		case <-ws.alarmIndex:
			// The index changed; the next target reads it without a statement.
		case <-ws.alarmReread:
			// This process recorded a job outcome; a handler may have recorded
			// a retry time while the job ran.
			if next, err := rt.readDeadline(ws, lastFire); err == nil {
				stored = next
			} else if retryAt.IsZero() {
				scheduleRetry()
			}
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// runClock runs the order_clock registration's handler, when one is
// registered.
func (rt *Runtime) runClock(ws *workspace, tick int) error {
	reg, ok := rt.registration(rt.clockKind)
	if !ok {
		return nil
	}
	args, err := json.Marshal(queue.OrderClockArgs{WorkspaceID: ws.id})
	if err != nil {
		return err
	}
	job := queue.Job{WorkspaceID: ws.id, ID: fmt.Sprintf("clock/%s@%d", ws.id, tick), Kind: rt.clockKind, Attempt: 1, MaxAttempts: 1, Args: args}
	if err := reg.Handle(rt.runCtx, job); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		rt.opts.Logf("logqueue: order clock %s: %v", ws.id, err)
		return err
	}
	return nil
}

// readDeadline reads the stored deadline after after; with no source there
// is none.
func (rt *Runtime) readDeadline(ws *workspace, after time.Time) (time.Time, error) {
	if rt.opts.Deadlines == nil {
		return time.Time{}, nil
	}
	next, ok, err := rt.opts.Deadlines(rt.loopCtx, ws.id, after)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: workspace %s: next deadline: %v", ws.id, err)
		}
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, nil
	}
	return next.UTC(), nil
}

// indexDeadline is the earliest time after after that the in-memory index
// records: a scheduled job's time, or the rescue time of a job claimed by
// another process. It sends no statement.
func (rt *Runtime) indexDeadline(ws *workspace, after time.Time) time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var earliest time.Time
	for _, job := range ws.jobs {
		var at time.Time
		switch {
		case job.State == StateScheduled:
			at = job.ScheduledAt
		case job.State == StateRunning && rt.opts.RescueStuckAfter > 0 && !job.ClaimedAt.IsZero() && !ws.running[job.Kind+"|"+string(job.Stream)]:
			at = job.ClaimedAt.Add(rt.opts.RescueStuckAfter + rescueMargin)
		default:
			continue
		}
		if at.After(after) {
			earliest = earliestTime(earliest, at)
		}
	}
	return earliest
}

func (rt *Runtime) setAlarmTarget(ws *workspace, target time.Time) {
	rt.mu.Lock()
	ws.alarmTarget = target
	rt.mu.Unlock()
}

// AlarmTarget is the time the workspace's deadline alarm sleeps until, and
// false when it arms no timer; for tests and diagnostics.
func (rt *Runtime) AlarmTarget(workspaceID string) (time.Time, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ws, ok := rt.spaces[workspaceID]
	if !ok {
		return time.Time{}, false
	}
	return ws.alarmTarget, !ws.alarmTarget.IsZero()
}

// signal sends on a channel buffered for one signal without blocking.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// earliestTime is the earliest non-zero time, or zero.
func earliestTime(times ...time.Time) time.Time {
	var earliest time.Time
	for _, at := range times {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	return earliest
}
