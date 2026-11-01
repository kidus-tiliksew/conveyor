package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// The dispatcher's registrations on the memory backend, with the store as
// the deadline source, apply each recorded deadline within five seconds
// (req-quiet-idle-runtime AC-2.4; component-durable-queue, The deadline
// alarm). Every deadline is recorded while the alarm sleeps until a later
// one, an hour away, so each case also proves that an earlier deadline moves
// a sleeping alarm forward. Each test waits on task notices, never on a
// fixed sleep, and asserts 60% of the budget so a slower CI runner passes.

const deadlineBudget = 5 * time.Second

type alarmHarness struct {
	t   *testing.T
	ctx context.Context
	st  store.Backend
	hub *changes.Hub
}

func newAlarmHarness(t *testing.T) *alarmHarness {
	t.Helper()
	cfg := &config.Config{
		Workspace: "test",
		Repos:     []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}},
	}
	st := store.NewVolatileBackend()
	h := &alarmHarness{t: t, ctx: store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test"), st: st, hub: st.Changes()}
	// A far deadline: the alarm sleeps until it once the runtime starts.
	far := h.order(time.Now().UTC().Add(time.Hour))
	d := New(st, cfg, nil)
	runtime, err := testRuntime(t, st.Log(), d, &ShutdownMarker{}, []string{"test"}, map[string]*config.Config{"test": cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtime.StopAndCancel(ctx)
	})
	h.waitSleeping(runtime, far.QueueDeadline)
	return h
}

// order creates a running task with a queued implement order whose queue
// deadline is queueDeadline.
func (h *alarmHarness) order(queueDeadline time.Time) core.WorkOrder {
	h.t.Helper()
	id := core.NewTaskID()
	now := time.Now().UTC()
	if err := h.st.CreateTask(h.ctx, core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		h.t.Fatal(err)
	}
	orderID := id + "-implement-1"
	if err := h.st.CreateJob(h.ctx, core.Job{ID: orderID, TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		h.t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(h.ctx, h.st, core.WorkOrder{ID: orderID, TaskID: id, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: queueDeadline, CreatedAt: now}); err != nil {
		h.t.Fatal(err)
	}
	stored, err := h.st.GetWorkOrder(h.ctx, orderID)
	if err != nil {
		h.t.Fatal(err)
	}
	return stored
}

// waitSleeping waits until the workspace's alarm, past its start fire,
// sleeps until the far deadline.
func (h *alarmHarness) waitSleeping(runtime *logqueue.Runtime, far time.Time) {
	h.t.Helper()
	deadline := time.Now().Add(deadlineBudget)
	for time.Now().Before(deadline) {
		if target, armed := runtime.AlarmTarget("test"); armed && target.Equal(far) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	target, armed := runtime.AlarmTarget("test")
	h.t.Fatalf("alarm target=%s armed=%t, want the far deadline %s", target, armed, far)
}

// applied waits on the task's notices until done reports the deadline
// applied, and returns when it saw that.
func (h *alarmHarness) applied(taskID, what string, done func() bool) time.Time {
	h.t.Helper()
	notices := h.hub.SubscribeTask("test", taskID)
	defer notices.Close()
	timeout := time.After(deadlineBudget + 2*time.Second)
	for !done() {
		select {
		case <-notices.C():
		case <-timeout:
			h.t.Fatalf("%s never applied", what)
		}
	}
	return time.Now().UTC()
}

func (h *alarmHarness) within(what string, deadline, applied time.Time) {
	h.t.Helper()
	late := applied.Sub(deadline)
	if late < 0 {
		h.t.Fatalf("%s applied %s before its deadline", what, -late)
	}
	if late > deadlineBudget*6/10 {
		h.t.Fatalf("%s applied %s after its deadline; budget %s", what, late, deadlineBudget)
	}
	h.t.Logf("%s applied %s after its deadline", what, late.Round(time.Millisecond))
}

func (h *alarmHarness) state(id string) core.WorkOrder {
	h.t.Helper()
	order, err := h.st.GetWorkOrder(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return order
}

func TestDeadlineAlarmAppliesLeaseExpiry(t *testing.T) {
	h := newAlarmHarness(t)
	order := h.order(time.Now().UTC().Add(time.Hour))
	claimed, err := storetest.ClaimWorkOrder(h.ctx, h.st, order.ID, core.WorkOrderClaim{SessionID: "lease", ClientToken: "lease-token", ClaimantID: core.TaskRunClaimantID("usr-run"), OwnerUserID: "usr-run", Lease: 1500 * time.Millisecond, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	lease := h.state(claimed.ID).LeaseExpiresAt
	at := h.applied(order.TaskID, "lease expiry", func() bool { return h.state(order.ID).State != core.WorkOrderClaimed })
	h.within("lease expiry", lease, at)
}

func TestDeadlineAlarmAppliesQueueDeadline(t *testing.T) {
	h := newAlarmHarness(t)
	order := h.order(time.Now().UTC().Add(1500 * time.Millisecond))
	at := h.applied(order.TaskID, "queue deadline", func() bool { return h.state(order.ID).State == core.WorkOrderStale })
	h.within("queue deadline", order.QueueDeadline, at)
}

func TestDeadlineAlarmAppliesPublicationRetry(t *testing.T) {
	h := newAlarmHarness(t)
	window := config.DefaultRecoveryPolicy().Window
	taskID := core.NewTaskID()
	if err := h.st.CreateTask(h.ctx, core.Task{ID: taskID, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	reviewID := taskID + "-review"
	if err := h.st.CreateJob(h.ctx, core.Job{ID: reviewID, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.QueueReviewPublication(h.ctx, core.ReviewPublication{ReviewWorkOrderID: reviewID, TaskID: taskID, JobID: reviewID, Verdict: "changes_requested", Feedback: "keep", ReviewedCommitSHA: "0123456789abcdef0123456789abcdef01234567"}); err != nil {
		t.Fatal(err)
	}
	publication, err := h.st.GetReviewPublication(h.ctx, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	// The row failed again after an automatic re-enqueue one window ago, so
	// its next re-enqueue is due shortly.
	due := time.Now().UTC().Add(1500 * time.Millisecond)
	publication.State, publication.LastError, publication.ForgeErrorCategory = core.ReviewPublicationFailed, "review publish failed", "forge_request"
	publication.AutomaticReenqueueAt = due.Add(-window)
	if err := h.st.UpdateReviewPublication(h.ctx, publication); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := h.st.NextDeadline(h.ctx, time.Now().UTC()); err != nil || !ok || !next.Equal(due) {
		t.Fatalf("NextDeadline=%s ok=%t err=%v, want the re-enqueue %s", next, ok, err, due)
	}
	at := h.applied(taskID, "publication retry", func() bool {
		current, err := h.st.GetReviewPublication(h.ctx, reviewID)
		return err == nil && current.State == core.ReviewPublicationRetrying
	})
	h.within("publication retry", due, at)
}
