package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The silence fallback resumes timed GitHub checks for a workspace with
// webhook delivery when no delivery has arrived for a while
// (component-runtime, Silence fallback; req-quiet-idle-runtime AC-3.8). It
// watches only the tasks whose next step depends on something GitHub
// reports: approved tasks with the merge gate off, which wait for checks
// and for the automatic or an external merge to be observed. Parked,
// human-gated, and agent-driven tasks never start it. Every input is a
// stored time, so a restart recomputes the silence.

// GitHubSilenceWindow is how long a watched task waits without an accepted
// delivery before the silence fallback starts.
const GitHubSilenceWindow = 15 * time.Minute

// waitingSince memoizes when a watched task began waiting: the time of its
// latest task.state_changed event to approved, and the last ledger event
// read, so the next read fetches only later events.
type waitingSince struct {
	at          time.Time
	lastEventID int64
}

// SilenceActive reports whether this daemon's silence fallback runs for
// the workspace. The workspace GitHub App API reports it as
// webhook.silence_fallback_active (component-identity-membership).
func (f *GitHubFallback) SilenceActive(workspace string) bool {
	f.mu.Lock()
	worker := f.workers[workspace]
	f.mu.Unlock()
	return worker != nil && worker.silent.Load()
}

// silenceWatched reports whether the silence fallback watches the task: an
// approved task with the merge gate off. An approved task with the merge
// gate on waits for an operator's merge act and is not watched.
func silenceWatched(task core.Task) bool {
	return task.State == core.TaskApproved && !task.MergeApproval
}

// watchedTasks lists the workspace's watched tasks.
func (f *GitHubFallback) watchedTasks(ctx context.Context) ([]core.Task, error) {
	approved, err := f.Dispatcher.Store.ListTasksFiltered(ctx, store.TaskFilter{States: []core.TaskState{core.TaskApproved}})
	if err != nil {
		return nil, err
	}
	watched := approved[:0]
	for _, task := range approved {
		if silenceWatched(task) {
			watched = append(watched, task)
		}
	}
	return watched, nil
}

// anyWatched reports whether a watched task remains. A failed read writes
// one error line and keeps the silence fallback running.
func (f *GitHubFallback) anyWatched(ctx context.Context, workspace string) bool {
	tasks, err := f.watchedTasks(ctx)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=watched_tasks err=%v`, workspace, err)
		return true
	}
	return len(tasks) > 0
}

// evaluateSilence recomputes the silence due time over every watched task
// and, when start is set, starts the silence fallback once that time has
// passed. It reports whether the silence fallback started. A failed read
// keeps the previous due time.
func (f *GitHubFallback) evaluateSilence(ctx context.Context, workspace string, st *fallbackState, now time.Time, start bool) bool {
	tasks, err := f.watchedTasks(ctx)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=watched_tasks err=%v`, workspace, err)
		return false
	}
	st.silenceDue, st.silenceSince = time.Time{}, time.Time{}
	watched := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		watched[task.ID] = true
		since, err := f.waitingSince(ctx, st, task)
		if err != nil {
			f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=waiting_since task=%s err=%v`, workspace, task.ID, err)
			continue
		}
		st.considerSilence(since)
	}
	for id := range st.since {
		if !watched[id] {
			delete(st.since, id)
		}
	}
	if !start || st.silent || st.silenceDue.IsZero() || now.Before(st.silenceDue) {
		return false
	}
	st.silent, st.waiting = true, true
	f.logf(`level=warn msg="GitHub webhook deliveries silent; timed GitHub checks started" workspace=%s last_delivery_at=%s waiting_since=%s`,
		workspace, st.lastDelivery.UTC().Format(time.RFC3339Nano), st.silenceSince.UTC().Format(time.RFC3339Nano))
	return true
}

// onSilenceTask handles a task notice for a webhook workspace whose silence
// fallback is not running: a watched task that began waiting after the last
// delivery can bring the due time forward. A notice without a task ID
// recomputes over every watched task. The timer, not the notice, starts the
// silence fallback.
func (f *GitHubFallback) onSilenceTask(ctx context.Context, workspace string, st *fallbackState, notice changes.Notice) {
	now := f.now()
	if notice.TaskID == "" {
		f.evaluateSilence(ctx, workspace, st, now, false)
		f.schedule(st, now)
		return
	}
	task, err := f.Dispatcher.Store.GetTask(ctx, notice.TaskID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		delete(st.since, notice.TaskID)
		return
	case err != nil:
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=watched_tasks err=%v`, workspace, err)
		return
	case !silenceWatched(task):
		delete(st.since, notice.TaskID)
		return
	}
	since, err := f.waitingSince(ctx, st, task)
	if err != nil {
		f.logf(`level=error msg="timed GitHub check failed" workspace=%s step=waiting_since task=%s err=%v`, workspace, task.ID, err)
		return
	}
	st.considerSilence(since)
	f.schedule(st, now)
}

// considerSilence brings the due time forward for a watched task that began
// waiting after the last accepted delivery.
func (st *fallbackState) considerSilence(since time.Time) {
	if !since.After(st.lastDelivery) {
		return
	}
	if due := since.Add(GitHubSilenceWindow); st.silenceDue.IsZero() || due.Before(st.silenceDue) {
		st.silenceDue, st.silenceSince = due, since
	}
}

// deliveryAccepted records an accepted delivery: the workspace has webhook
// delivery, a running silence fallback stops with one info line, and no
// silence is due until a watched task begins waiting after it.
func (f *GitHubFallback) deliveryAccepted(workspace string, st *fallbackState, at time.Time) {
	if st.silent {
		f.logf(`level=info msg="GitHub webhook delivery accepted; timed GitHub checks stopped" workspace=%s`, workspace)
	}
	st.webhook = true
	if at.After(st.lastDelivery) {
		st.lastDelivery = at
	}
	st.endSilence()
	st.disarm()
}

// endSilence stops the silence fallback without a line.
func (st *fallbackState) endSilence() {
	st.silent, st.waiting = false, false
	st.silenceDue, st.silenceSince = time.Time{}, time.Time{}
}

// waitingSince returns when a watched task began waiting: its latest
// task.state_changed event to approved, or its creation when the ledger
// holds none. Only events after the memoized one are read.
func (f *GitHubFallback) waitingSince(ctx context.Context, st *fallbackState, task core.Task) (time.Time, error) {
	memo, known := st.since[task.ID]
	var events []core.Event
	var err error
	if known {
		events, err = f.Dispatcher.Store.ListEventsAfter(ctx, task.ID, memo.lastEventID)
		if errors.Is(err, store.ErrEventAnchorNotFound) {
			known = false
		} else if err != nil {
			return time.Time{}, err
		}
	}
	if !known {
		memo = waitingSince{}
		if events, err = f.Dispatcher.Store.ListEvents(ctx, task.ID); err != nil {
			return time.Time{}, err
		}
	}
	for _, event := range events {
		memo.lastEventID = max(memo.lastEventID, event.ID)
		if event.Kind == "task.state_changed" && stateChangedTo(event) == core.TaskApproved && event.At.After(memo.at) {
			memo.at = event.At
		}
	}
	st.since[task.ID] = memo
	if memo.at.IsZero() {
		return task.CreatedAt, nil
	}
	return memo.at, nil
}

// stateChangedTo decodes the target state of a task.state_changed event.
func stateChangedTo(event core.Event) core.TaskState {
	var payload struct {
		To core.TaskState `json:"to"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	return payload.To
}
