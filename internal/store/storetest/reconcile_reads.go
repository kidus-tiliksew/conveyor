package storetest

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
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

	t.Run("VerificationClaimsReadOnlyAttemptHolders", func(t *testing.T) {
		runVerificationClaimReadScope(t, x)
	})

	t.Run("GitHubLifecycleReconcileTaskIDsSelectCandidatesInTaskOrder", func(t *testing.T) {
		epoch := time.Unix(0, 0).UTC()
		before, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx, &epoch)
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
		got, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx, &epoch)
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
	t.Run("GitHubLifecycleReconcileTaskIDsHonorPublicationCutoff", func(t *testing.T) {
		// A nil cutoff means issue publication is off: only supersession
		// candidates remain. A cutoff omits a plan approved before it
		// (req-delivery-and-forge AC-8.3; component-git-delivery).
		epoch := time.Unix(0, 0).UTC()
		before, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx, &epoch)
		requireOK(t, err)
		earlier := newTask(core.TaskMerged)
		plan(earlier.ID, true)
		earlierSpec, ok, err := st.GetLatestSpecVersion(ctx, earlier.ID)
		requireOK(t, err)
		if !ok || earlierSpec.ApprovedAt.IsZero() {
			t.Fatalf("earlier approval=%+v ok=%t", earlierSpec, ok)
		}
		cutoff := earlierSpec.ApprovedAt.Add(time.Microsecond)
		// Both durable backends stamp approval with the host clock at
		// microsecond precision; wait until that clock has passed the cutoff.
		for !time.Now().After(cutoff.Add(time.Millisecond)) {
			runtime.Gosched()
		}
		later := newTask(core.TaskQueued)
		plan(later.ID, true)
		laterSpec, _, err := st.GetLatestSpecVersion(ctx, later.ID)
		requireOK(t, err)
		if laterSpec.ApprovedAt.Before(cutoff) {
			t.Fatalf("later approval %s precedes cutoff %s", laterSpec.ApprovedAt, cutoff)
		}
		retired := newTask(core.TaskRunning)
		_, err = taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: retired.ID, RequestID: "restart-" + retired.ID, Reason: "replace"})
		requireOK(t, err)
		fresh := func(got []string) []string {
			return slices.DeleteFunc(got, func(id string) bool { return slices.Contains(before, id) })
		}
		off, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx, nil)
		requireOK(t, err)
		if got := fresh(off); !slices.Equal(got, []string{retired.ID}) {
			t.Fatalf("nil cutoff candidates=%v, want only the superseded %s (earlier %s, later %s)", got, retired.ID, earlier.ID, later.ID)
		}
		on, err := st.ListGitHubLifecycleReconcileTaskIDs(ctx, &cutoff)
		requireOK(t, err)
		if got := fresh(on); !slices.Equal(got, []string{retired.ID, later.ID}) {
			t.Fatalf("cutoff candidates=%v, want %v without the earlier approval %s", got, []string{retired.ID, later.ID}, earlier.ID)
		}
	})

	t.Run("IssuePublicationTurnOnIsStamped", func(t *testing.T) {
		// Every backend stamps the turn-on time from the stored document and
		// never keeps a client-sent time (component-persistence; AC-8.3).
		update := func(enabled bool, clientTime time.Time) config.UpdateReceipt {
			t.Helper()
			current, err := st.WorkspaceConfig(ctx)
			requireOK(t, err)
			next, err := st.RuntimeConfig(ctx, x.Config)
			requireOK(t, err)
			next.IssuePublication = config.IssuePublication{Enabled: enabled, EnabledAt: clientTime}
			receipt, err := st.UpdateWorkspaceConfig(ctx, current.Version, next)
			requireOK(t, err)
			return receipt
		}
		stored := func() config.IssuePublication {
			t.Helper()
			saved, err := st.WorkspaceConfig(ctx)
			requireOK(t, err)
			composed, err := st.RuntimeConfig(ctx, x.Config)
			requireOK(t, err)
			if saved.Document.IssuePublication.Enabled != composed.IssuePublication.Enabled || !saved.Document.IssuePublication.EnabledAt.Equal(composed.IssuePublication.EnabledAt) {
				t.Fatalf("stored %+v differs from runtime %+v", saved.Document.IssuePublication, composed.IssuePublication)
			}
			return saved.Document.IssuePublication
		}
		if initial := stored(); initial.Enabled {
			t.Fatalf("fresh workspace issue publication=%+v, want off", initial)
		}
		forged := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		start := time.Now().UTC().Truncate(time.Microsecond)
		receipt := update(true, forged)
		turnedOn := stored()
		if !turnedOn.Enabled || turnedOn.EnabledAt.Before(start) || turnedOn.EnabledAt.After(time.Now().UTC()) {
			t.Fatalf("turn-on stamped %+v, want a write time at or after %s", turnedOn, start)
		}
		if !slices.Contains(receipt.Sections, "issue_publication") || !receipt.Document.IssuePublication.EnabledAt.Equal(turnedOn.EnabledAt) {
			t.Fatalf("turn-on receipt sections=%v document=%+v", receipt.Sections, receipt.Document.IssuePublication)
		}
		update(true, forged.Add(time.Hour))
		if kept := stored(); !kept.Enabled || !kept.EnabledAt.Equal(turnedOn.EnabledAt) {
			t.Fatalf("write keeping the setting on stored %+v, want turn-on time %s", kept, turnedOn.EnabledAt)
		}
		update(false, forged)
		if off := stored(); off.Enabled || !off.EnabledAt.Equal(turnedOn.EnabledAt) {
			t.Fatalf("turn-off stored %+v, want off with last turn-on time %s", off, turnedOn.EnabledAt)
		}
	})
}

// runVerificationClaimReadScope seeds a workspace dominated by finished
// tasks with long histories beside four verification tasks: a running task
// whose attempt lost its claim, a closed task whose attempt lost its claim, a
// running task whose attempt still holds a live claim, and a running task
// whose attempt already ended. ReconcileVerificationClaims must cancel both
// lost attempts, leave the others unchanged, act again on nothing, and, on
// the durable backends, read verification rows only for the three tasks that
// hold a running or pending attempt (component-verification-service;
// component-persistence).
func runVerificationClaimReadScope(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	var reads func() []string
	if x.VerificationClaimReads != nil {
		reads = x.VerificationClaimReads(t)
	} else if st.IsDurable() {
		t.Fatal("durable conformance fixture must provide VerificationClaimReads")
	}
	terminal := SeedTerminalHistory(t, st, ctx, x.Workspace, "conveyor", 30, 15)
	loseClaim := func(v verificationFixture) {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, v.access.WorkOrderID)
		requireOK(t, err)
		order.LeaseExpiresAt = time.Now().Add(-time.Second)
		requireOK(t, UpdateWorkOrder(ctx, st, order))
	}
	lost := newVerificationFixture(t, x)
	loseClaim(lost)
	closedLost := newVerificationFixture(t, x)
	loseClaim(closedLost)
	_, err := taskops.New(st).Perform(ctx, closedLost.access.TaskID, taskops.Command{Kind: core.TaskCancel})
	requireOK(t, err)
	if closed, getErr := st.GetTask(ctx, closedLost.access.TaskID); getErr != nil || closed.State != core.TaskClosed {
		t.Fatalf("closed verification task=%+v err=%v", closed, getErr)
	}
	live := newVerificationFixture(t, x)
	ended := newVerificationFixture(t, x)
	ended.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Key: "terminate-" + ended.runID, Attempt: &store.VerificationAttempt{State: "failed"}})

	userContext, owner := bootstrapOwner(t, x)
	attemptState := func(v verificationFixture) (string, bool) {
		t.Helper()
		snapshot, readErr := st.ReadVerification(userContext, store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}, v.contextID)
		requireOK(t, readErr)
		if len(snapshot.Attempts) != 1 {
			t.Fatalf("task %s attempts=%+v", v.access.TaskID, snapshot.Attempts)
		}
		return snapshot.Attempts[0].State, snapshot.Attempts[0].EndedAt != nil
	}
	if state, ended := attemptState(closedLost); state != "running" || ended {
		t.Fatalf("closing the task ended its attempt before the sweep: state=%s ended=%t", state, ended)
	}
	before := map[string]string{}
	for _, v := range []verificationFixture{live, ended} {
		before[v.access.TaskID], _ = attemptState(v)
	}

	reconciler := store.WithActor(ctx, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem})
	count, err := st.ReconcileVerificationClaims(reconciler)
	requireOK(t, err)
	if count != 2 {
		t.Fatalf("first sweep reconciled %d attempts, want the two lost claims", count)
	}
	for _, v := range []verificationFixture{lost, closedLost} {
		if state, ended := attemptState(v); state != "cancelled" || !ended {
			t.Fatalf("lost attempt of task %s state=%s ended=%t", v.access.TaskID, state, ended)
		}
	}
	for _, v := range []verificationFixture{live, ended} {
		if state, _ := attemptState(v); state != before[v.access.TaskID] {
			t.Fatalf("task %s attempt changed from %s to %s", v.access.TaskID, before[v.access.TaskID], state)
		}
	}
	firstReads := []string(nil)
	if reads != nil {
		firstReads = reads()
		want := []string{live.access.TaskID, closedLost.access.TaskID, lost.access.TaskID}
		if !slices.Equal(firstReads, want) {
			t.Fatalf("first sweep read verification rows of %v, want the attempt holders in ListTasks order %v", firstReads, want)
		}
	}
	count, err = st.ReconcileVerificationClaims(reconciler)
	requireOK(t, err)
	if count != 0 {
		t.Fatalf("repeat sweep reconciled %d attempts", count)
	}
	if reads != nil {
		if repeat := reads()[len(firstReads):]; !slices.Equal(repeat, []string{live.access.TaskID}) {
			t.Fatalf("repeat sweep read verification rows of %v, want only the live attempt's task", repeat)
		}
		for _, id := range reads() {
			for _, task := range terminal {
				if id == task.ID {
					t.Fatalf("sweep read finished task %s", id)
				}
			}
			if id == ended.access.TaskID {
				t.Fatal("sweep read the task whose attempt already ended")
			}
		}
	}
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
