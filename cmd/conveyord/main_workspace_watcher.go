package main

import (
	"context"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// workspaceRegistrationWatcher converges the queue runtime's workspaces with
// the persisted ones at start and on each workspace change notice, which
// covers a workspace a peer daemon created. A failed convergence is logged at
// error level and retried after a delay that starts at five seconds and
// doubles to one minute; a later notice converges at once. It replaces the
// convergence the minute reconcile tick ran (component-runtime, Start-up
// checks and background loops; req-quiet-idle-runtime AC-4.3).
type workspaceRegistrationWatcher struct {
	notices  *changes.Subscription
	list     func(context.Context) ([]core.Workspace, error)
	converge func([]core.Workspace) error
	logf     func(string, ...any)
	// retryInitial and retryMaximum default to five seconds and one minute.
	retryInitial, retryMaximum time.Duration
	// after returns a channel that fires once after the delay; nil uses
	// time.After.
	after func(time.Duration) <-chan time.Time
}

// startWorkspaceRegistrationWatcher converges once, then watches until ctx
// ends. The returned channel closes when the watcher stops.
func startWorkspaceRegistrationWatcher(ctx context.Context, w workspaceRegistrationWatcher) <-chan struct{} {
	if w.retryInitial <= 0 {
		w.retryInitial = 5 * time.Second
	}
	if w.retryMaximum <= 0 {
		w.retryMaximum = time.Minute
	}
	if w.after == nil {
		w.after = time.After
	}
	done := make(chan struct{})
	delay := w.retryInitial
	retry := w.convergeOnce(ctx, &delay)
	go func() {
		defer close(done)
		defer w.notices.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.notices.C():
			case <-retry:
			}
			retry = w.convergeOnce(ctx, &delay)
		}
	}()
	return done
}

// convergeOnce lists and registers the workspaces. On failure it logs one
// error line and returns the retry timer, doubling the next delay; on success
// it resets the delay and returns nil.
func (w workspaceRegistrationWatcher) convergeOnce(ctx context.Context, delay *time.Duration) <-chan time.Time {
	workspaces, err := w.list(ctx)
	if err == nil {
		err = w.converge(workspaces)
	}
	if err == nil {
		*delay = w.retryInitial
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	w.logf(`level=error msg="workspace queue registration failed" retry_in=%s err=%v`, *delay, err)
	retry := w.after(*delay)
	*delay = min(2**delay, w.retryMaximum)
	return retry
}
