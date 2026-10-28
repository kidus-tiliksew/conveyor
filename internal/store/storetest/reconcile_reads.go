package storetest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runReconcileReads proves the reconcile sweeps' narrow reads on every
// backend (component-persistence; component-runtime, Reconcile tick).
func runReconcileReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	created := 0
	newTask := func(state core.TaskState) core.Task {
		t.Helper()
		created++
		task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Source: "test", Title: "reconcile read", Repo: "conveyor", BaseBranch: "main",
			State: state, NextStage: core.StageImplement, CreatedAt: base.Add(time.Duration(created) * time.Minute)}
		task.Branch = "conveyor/task-" + task.ID
		requireOK(t, st.CreateTask(ctx, task))
		return task
	}
	plan := func(taskID string, approve bool) {
		t.Helper()
		spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
		requireOK(t, err)
		if approve {
			requireOK(t, st.ApproveSpecVersion(ctx, taskID, spec.Version))
		}
	}

	t.Run("AutomaticReviewEventsSelectFoldedKindsInEventOrder", func(t *testing.T) {
		folded, unrelated := newTask(core.TaskRunning), newTask(core.TaskRunning)
		for _, kind := range []string{"review.completed", "pull_request.opened", "task.setup.changed", "review.round_completed", "work_order.claimed", "review.round_retried", "review.round_recovered", "task.state_changed"} {
			requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: folded.ID, Kind: kind, Payload: core.JSONPayload(map[string]any{"review_round": 1})}))
		}
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: unrelated.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7})}))
		got, err := st.ListAutomaticReviewEvents(ctx, []string{folded.ID, unrelated.ID, "absent-task"})
		requireOK(t, err)
		var want []int64
		for _, event := range listEventsOK(t, st, ctx, folded.ID) {
			if slices.Contains(store.AutomaticReviewEventKinds, event.Kind) {
				want = append(want, event.ID)
			}
		}
		var ids []int64
		for _, event := range got[folded.ID] {
			if event.TaskID != folded.ID {
				t.Fatalf("event %d belongs to task %s", event.ID, event.TaskID)
			}
			ids = append(ids, event.ID)
		}
		if len(want) != len(store.AutomaticReviewEventKinds) || !slices.Equal(ids, want) {
			t.Fatalf("automatic review events=%v, want ListEvents order %v", ids, want)
		}
		if _, ok := got[unrelated.ID]; ok {
			t.Fatalf("task without a folded kind is present: %+v", got[unrelated.ID])
		}
		if _, ok := got["absent-task"]; ok || len(got) != 1 {
			t.Fatalf("automatic review events keys=%d", len(got))
		}
		empty, err := st.ListAutomaticReviewEvents(ctx, nil)
		requireOK(t, err)
		if len(empty) != 0 {
			t.Fatalf("empty request returned %+v", empty)
		}
	})

	t.Run("GitHubLifecycleReconcileTaskIDsSelectCandidatesInTaskOrder", func(t *testing.T) {
		before, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx)
		requireOK(t, err)
		noPlan := newTask(core.TaskQueued)
		draft := newTask(core.TaskQueued)
		plan(draft.ID, false)
		approved := newTask(core.TaskQueued)
		plan(approved.ID, true)
		mergedApproved := newTask(core.TaskMerged)
		plan(mergedApproved.ID, true)
		published := newTask(core.TaskRunning)
		plan(published.ID, true)
		requireOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: published.ID, Repository: "acme/conveyor", SpecVersion: 1}))
		revised := newTask(core.TaskRunning)
		plan(revised.ID, true)
		plan(revised.ID, false)
		retired := newTask(core.TaskRunning)
		restarted, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: retired.ID, RequestID: "restart-" + retired.ID, Reason: "replace"})
		requireOK(t, err)
		if restarted.Task.State != core.TaskClosed || restarted.Task.SupersededBy == "" {
			t.Fatalf("start-over result=%+v", restarted)
		}
		got, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx)
		requireOK(t, err)
		got = slices.DeleteFunc(got, func(id string) bool { return slices.Contains(before, id) })
		// ListTasks order: newest first. The closed retired task is a
		// candidate through its supersession, its queued successor is not.
		want := []string{retired.ID, mergedApproved.ID, approved.ID}
		if !slices.Equal(got, want) {
			t.Fatalf("lifecycle reconcile candidates=%v, want %v (no plan %s, draft %s, published %s, revised %s, successor %s)",
				got, want, noPlan.ID, draft.ID, published.ID, revised.ID, restarted.Successor.ID)
		}
		all, err := st.ListTasks(ctx)
		requireOK(t, err)
		var order []string
		for _, task := range all {
			if slices.Contains(want, task.ID) {
				order = append(order, task.ID)
			}
		}
		if !slices.Equal(order, want) {
			t.Fatalf("candidate order %v differs from ListTasks order %v", want, order)
		}
	})
}

// ReadRecorder wraps a store and records the task rows and per-task history
// reads a sweep issues, so a test proves the sweep never reads the
// workspace's full task list or a terminal task's rows (component-runtime,
// Reconcile tick).
type ReadRecorder struct {
	store.Store
	mu         sync.Mutex
	fullLists  int
	taskRows   map[string]core.TaskState
	eventReads map[string]int
	orderReads map[string]int
}

// NewReadRecorder wraps st.
func NewReadRecorder(st store.Store) *ReadRecorder {
	r := &ReadRecorder{Store: st}
	r.Reset()
	return r
}

// Reset forgets every recorded read.
func (r *ReadRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fullLists = 0
	r.taskRows = map[string]core.TaskState{}
	r.eventReads = map[string]int{}
	r.orderReads = map[string]int{}
}

func (r *ReadRecorder) rows(tasks []core.Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, task := range tasks {
		r.taskRows[task.ID] = task.State
	}
}

func (r *ReadRecorder) ListTasks(ctx context.Context) ([]core.Task, error) {
	r.mu.Lock()
	r.fullLists++
	r.mu.Unlock()
	tasks, err := r.Store.ListTasks(ctx)
	r.rows(tasks)
	return tasks, err
}

func (r *ReadRecorder) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	if !filter.Active() {
		r.mu.Lock()
		r.fullLists++
		r.mu.Unlock()
	}
	tasks, err := r.Store.ListTasksFiltered(ctx, filter)
	r.rows(tasks)
	return tasks, err
}

func (r *ReadRecorder) GetTask(ctx context.Context, id string) (core.Task, error) {
	task, err := r.Store.GetTask(ctx, id)
	if err == nil {
		r.rows([]core.Task{task})
	}
	return task, err
}

func (r *ReadRecorder) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	r.mu.Lock()
	r.eventReads[taskID]++
	r.mu.Unlock()
	return r.Store.ListEvents(ctx, taskID)
}

func (r *ReadRecorder) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	r.mu.Lock()
	r.orderReads[taskID]++
	r.mu.Unlock()
	return r.Store.ListTaskWorkOrders(ctx, taskID)
}

// TaskRowState reports the state of a task row a recorded read returned.
func (r *ReadRecorder) TaskRowState(id string) (core.TaskState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.taskRows[id]
	return state, ok
}

// RequireNoTerminalReads fails when a full task list ran, or when a merged or
// closed task's row, event history, or order list was read, other than the
// tasks in allowed.
func (r *ReadRecorder) RequireNoTerminalReads(t *testing.T, terminal []core.Task, allowed ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fullLists != 0 {
		t.Fatalf("sweep read the full task list %d time(s)", r.fullLists)
	}
	for id, state := range r.taskRows {
		if core.TaskTerminal(state) && !slices.Contains(allowed, id) {
			t.Fatalf("sweep read terminal task row %s (%s)", id, state)
		}
	}
	for _, task := range terminal {
		if slices.Contains(allowed, task.ID) {
			continue
		}
		if r.eventReads[task.ID] != 0 || r.orderReads[task.ID] != 0 {
			t.Fatalf("sweep read terminal task %s history: events=%d orders=%d", task.ID, r.eventReads[task.ID], r.orderReads[task.ID])
		}
	}
}

// SeedTerminalHistory creates count merged and closed tasks, each with an
// approved, published plan and events of every AR-5 folded kind among
// unrelated ones, modelling a workspace dominated by finished work.
func SeedTerminalHistory(t *testing.T, st store.Store, ctx context.Context, workspace, repo string, count, eventsPerTask int) []core.Task {
	t.Helper()
	created := time.Now().UTC().Add(-48 * time.Hour)
	kinds := append([]string{"task.state_changed", "pull_request.opened", "work_order.claimed"}, store.AutomaticReviewEventKinds...)
	tasks := make([]core.Task, 0, count)
	for i := range count {
		state := core.TaskMerged
		if i%2 == 1 {
			state = core.TaskClosed
		}
		task := core.Task{ID: fmt.Sprintf("terminal-%03d-%s", i, core.NewTaskID()), Workspace: workspace, Source: "test", Title: "finished work", Repo: repo, BaseBranch: "main",
			State: state, NextStage: core.StageReview, CreatedAt: created.Add(time.Duration(i) * time.Minute)}
		task.Branch = "conveyor/task-" + task.ID
		requireOK(t, st.CreateTask(ctx, task))
		spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
		requireOK(t, err)
		requireOK(t, st.ApproveSpecVersion(ctx, task.ID, spec.Version))
		requireOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/" + repo, SpecVersion: spec.Version}))
		for j := range eventsPerTask {
			payload := map[string]any{"review_round": 1, "number": 1000 + i, "review_work_order_id": task.ID + "-review-1-seat-1"}
			requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: kinds[j%len(kinds)], Payload: core.JSONPayload(payload)}))
		}
		tasks = append(tasks, task)
	}
	return tasks
}
