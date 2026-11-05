package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// PageQuery is one page of a list read that grows with workspace history. The
// handler's shared parser refuses an out-of-range value with 400, and each
// backend validates again, so no caller reaches a backend with an
// unrepresentable bound (DEC-105(4); component-http-api invariant 19).
type PageQuery struct {
	Limit  int
	Offset int
}

// Validate refuses a page the backends cannot represent identically.
func (q PageQuery) Validate() error {
	if q.Limit < 1 || q.Limit > MaxTaskOperationsLimit {
		return fmt.Errorf("page limit must be between 1 and %d", MaxTaskOperationsLimit)
	}
	if q.Offset < 0 || q.Offset > MaxTaskOperationsOffset {
		return fmt.Errorf("page offset must be between 0 and %d", MaxTaskOperationsOffset)
	}
	return nil
}

// JobPage is one page of a task's jobs in ListJobs order. Total counts every
// job of the task, so a page past the end still states it.
type JobPage struct {
	Jobs  []core.Job
	Total int
}

// EventPage is one page of a task's ledger in ListEvents order. Total counts
// every event of the task.
type EventPage struct {
	Events []core.Event
	Total  int
}

// RecoveryActQuery is one page of the workspace recovery act log at or after
// Since.
type RecoveryActQuery struct {
	Since time.Time
	Page  PageQuery
}

// RecoveryActPage is one page of recovery acts, newest first. Total counts
// every act the query selects.
type RecoveryActPage struct {
	Acts  []nextaction.Act
	Total int
}

// recoveryActKinds maps each system recovery event kind to the act kind that
// GET /v1/inbox/acts states (req-bounded-automatic-recovery AC-5.2).
var recoveryActKinds = map[string]string{
	"work_order.recovery_scheduled": "recovery_scheduled",
	"work_order.recovered":          "recovered",
	"merge.conflict_fix_dispatched": "conflict_fix_dispatched",
	"task.retry_accepted":           "retry_accepted",
}

// RecoveryActEventKinds returns the event kinds a recovery act read selects,
// sorted, for a backend's query.
func RecoveryActEventKinds() []string {
	kinds := make([]string, 0, len(recoveryActKinds))
	for kind := range recoveryActKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// RecoveryActFromEvent states one selected recovery event as an act.
func RecoveryActFromEvent(id int64, taskID, eventKind string, at time.Time) nextaction.Act {
	return nextaction.Act{ID: id, TaskID: taskID, Kind: recoveryActKinds[eventKind], At: at, Automatic: true}
}

func pageBounds(page PageQuery, total int) (int, int) {
	start := min(page.Offset, total)
	return start, min(start+page.Limit, total)
}

// memoryTaskInScope reports whether the request's workspace scope holds the
// task, so an entity of another workspace reads as a missing one
// (component-http-api invariant 13). The volatile backend keeps a task created
// without a workspace, and an unscoped request, outside this comparison.
func memoryTaskInScope(task core.Task, workspace string) bool {
	return workspace == "" || task.Workspace == "" || task.Workspace == workspace
}

// taskInScopeLocked applies memoryTaskInScope to a stored task. A task the
// backend does not hold has no rows to read in any scope.
func (m *memory) taskInScopeLocked(ctx context.Context, taskID string) bool {
	task, ok := m.tasks[taskID]
	return ok && memoryTaskInScope(task, workspaceOrDefault(ctx, ""))
}

func (m *memory) ListJobPage(ctx context.Context, taskID string, page PageQuery) (JobPage, error) {
	if err := page.Validate(); err != nil {
		return JobPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.taskInScopeLocked(ctx, taskID) {
		return JobPage{Jobs: []core.Job{}}, nil
	}
	jobs := append(make([]core.Job, 0, len(m.jobs[taskID])), m.jobs[taskID]...)
	sortJobs(jobs)
	start, end := pageBounds(page, len(jobs))
	return JobPage{Jobs: jobs[start:end], Total: len(jobs)}, nil
}

func (m *memory) ListEventPage(ctx context.Context, taskID string, page PageQuery) (EventPage, error) {
	if err := page.Validate(); err != nil {
		return EventPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.taskInScopeLocked(ctx, taskID) {
		return EventPage{Events: []core.Event{}}, nil
	}
	events := append(make([]core.Event, 0, len(m.events[taskID])), m.events[taskID]...)
	sortEventsOldest(events)
	start, end := pageBounds(page, len(events))
	return EventPage{Events: events[start:end], Total: len(events)}, nil
}

func (m *memory) ListRecoveryActPage(ctx context.Context, query RecoveryActQuery) (RecoveryActPage, error) {
	if err := query.Page.Validate(); err != nil {
		return RecoveryActPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	var selected []core.Event
	for id, task := range m.tasks {
		if !memoryTaskInScope(task, workspace) {
			continue
		}
		for _, event := range m.events[id] {
			if _, recovery := recoveryActKinds[event.Kind]; recovery && event.ActorRole == core.ActorSystem && !event.At.Before(query.Since) {
				selected = append(selected, event)
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].At.Equal(selected[j].At) {
			return selected[i].ID > selected[j].ID
		}
		return selected[i].At.After(selected[j].At)
	})
	start, end := pageBounds(query.Page, len(selected))
	acts := make([]nextaction.Act, 0, end-start)
	for _, event := range selected[start:end] {
		acts = append(acts, RecoveryActFromEvent(event.ID, event.TaskID, event.Kind, event.At))
	}
	return RecoveryActPage{Acts: acts, Total: len(selected)}, nil
}
