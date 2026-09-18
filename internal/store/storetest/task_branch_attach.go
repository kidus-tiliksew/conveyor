package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runTaskBranchAttach(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	open := newAggregateTask(t, x)

	assertUnchanged := func(taskID, branch string, err error, sentinel error) {
		t.Helper()
		if err == nil || !errors.Is(err, sentinel) {
			t.Fatalf("error=%v want %v", err, sentinel)
		}
		current, getErr := st.GetTask(ctx, taskID)
		requireOK(t, getErr)
		if current.Branch != branch {
			t.Fatalf("branch changed to %s", current.Branch)
		}
	}

	for _, name := range []string{"", "HEAD", "@", "foo..bar", "-foo", "foo.lock", "refs/heads/foo", "a b"} {
		_, err := st.AttachTaskBranch(ctx, open.ID, name)
		assertUnchanged(open.ID, open.Branch, err, store.ErrInvalidBranch)
	}

	cancelled, err := taskops.New(st).Cancel(ctx, core.Intervention{
		TaskID: open.ID, Action: core.InterventionCancel, ReasonCode: "operator_cancel", Comment: "terminal attach",
	})
	requireOK(t, err)
	_, err = st.AttachTaskBranch(ctx, cancelled.ID, cancelled.Branch)
	assertUnchanged(cancelled.ID, cancelled.Branch, err, store.ErrTaskTerminal)
	_, err = st.AttachTaskBranch(ctx, cancelled.ID, "feature/terminal")
	assertUnchanged(cancelled.ID, cancelled.Branch, err, store.ErrTaskTerminal)

	task := newAggregateTask(t, x)
	before, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	same, err := st.AttachTaskBranch(ctx, task.ID, task.Branch)
	requireOK(t, err)
	if same.Branch != task.Branch {
		t.Fatalf("same-name branch=%s", same.Branch)
	}
	after, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("same-name attach appended an event")
	}

	_, err = st.AttachTaskBranch(ctx, task.ID, task.BaseBranch)
	assertUnchanged(task.ID, task.Branch, err, store.ErrBranchNotAttachable)
	other := newAggregateTask(t, x)
	_, err = st.AttachTaskBranch(ctx, task.ID, gitx.BranchName(other.ID))
	assertUnchanged(task.ID, task.Branch, err, store.ErrBranchNotAttachable)

	queued := newAggregateTask(t, x)
	now := time.Now().UTC()
	queuedJob := queued.ID + "-queued"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: queuedJob, TaskID: queued.ID, Stage: core.StageImplement, State: core.JobPending}))
	requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{
		ID: queuedJob, JobID: queuedJob, TaskID: queued.ID, Stage: core.StageImplement,
		QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}))
	remapped, err := st.AttachTaskBranch(ctx, queued.ID, "feature/queued-ok")
	requireOK(t, err)
	if remapped.Branch != "feature/queued-ok" {
		t.Fatalf("queued remap branch=%s", remapped.Branch)
	}

	claimedTask := newAggregateTask(t, x)
	claimImplementOrder(t, x, claimedTask.ID)
	before, err = st.ListEvents(ctx, claimedTask.ID)
	requireOK(t, err)
	same, err = st.AttachTaskBranch(ctx, claimedTask.ID, claimedTask.Branch)
	requireOK(t, err)
	if same.Branch != claimedTask.Branch {
		t.Fatalf("claimed same-name branch=%s", same.Branch)
	}
	after, err = st.ListEvents(ctx, claimedTask.ID)
	requireOK(t, err)
	if len(after) != len(before) {
		t.Fatal("claimed same-name appended an event")
	}
	_, err = st.AttachTaskBranch(ctx, claimedTask.ID, "feature/claimed")
	assertUnchanged(claimedTask.ID, claimedTask.Branch, err, store.ErrWorkOrderClaimed)

	prTask := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: prTask.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 1})}))
	before, err = st.ListEvents(ctx, prTask.ID)
	requireOK(t, err)
	same, err = st.AttachTaskBranch(ctx, prTask.ID, prTask.Branch)
	requireOK(t, err)
	after, err = st.ListEvents(ctx, prTask.ID)
	requireOK(t, err)
	if same.Branch != prTask.Branch || len(after) != len(before) {
		t.Fatal("recorded-PR same-name mutated assignment")
	}
	_, err = st.AttachTaskBranch(ctx, prTask.ID, "feature/pr")
	assertUnchanged(prTask.ID, prTask.Branch, err, store.ErrPullRequestRecorded)

	holder := newAggregateTask(t, x)
	holder, err = st.AttachTaskBranch(ctx, holder.ID, "feature/held")
	requireOK(t, err)
	collider := newAggregateTask(t, x)
	_, err = st.AttachTaskBranch(ctx, collider.ID, "feature/held")
	if err == nil || !errors.Is(err, store.ErrTaskBranchConflict) {
		t.Fatalf("occupancy error=%v", err)
	}
	var inUse *store.BranchInUseError
	if !errors.As(err, &inUse) || inUse.OtherTaskID != holder.ID {
		t.Fatalf("occupancy typed error=%v", err)
	}
	current, err := st.GetTask(ctx, collider.ID)
	requireOK(t, err)
	if current.Branch != collider.Branch {
		t.Fatalf("collider branch=%s", current.Branch)
	}

	free := newAggregateTask(t, x)
	previous := free.Branch
	before, err = st.ListEvents(ctx, free.ID)
	requireOK(t, err)
	got, err := st.AttachTaskBranch(ctx, free.ID, "feature/demo")
	requireOK(t, err)
	if got.ID != free.ID || got.Branch != "feature/demo" {
		t.Fatalf("remap=%+v", got)
	}
	events, err := st.ListEvents(ctx, free.ID)
	requireOK(t, err)
	if len(events) != len(before)+1 || events[len(events)-1].Kind != "task.branch_attached" {
		t.Fatal("remap lacks task.branch_attached")
	}
	back, err := st.AttachTaskBranch(ctx, free.ID, gitx.BranchName(free.ID))
	requireOK(t, err)
	if back.Branch != previous {
		t.Fatalf("restore default branch=%s", back.Branch)
	}
}

func claimImplementOrder(t *testing.T, x Fixture, taskID string) core.WorkOrder {
	t.Helper()
	now := time.Now().UTC()
	jobID := taskID + "-implement"
	requireOK(t, x.Backend.CreateJob(x.Context, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
	requireOK(t, For(x.Backend).CreateWorkOrder(x.Context, core.WorkOrder{
		ID: jobID, JobID: jobID, TaskID: taskID, Stage: core.StageImplement,
		QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now,
	}))
	order, err := For(x.Backend).ClaimWorkOrder(x.Context, jobID, core.WorkOrderClaim{
		SessionID: taskID + "-session", ClientToken: taskID + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour,
	})
	requireOK(t, err)
	if order.State != core.WorkOrderClaimed {
		t.Fatalf("claim state=%s", order.State)
	}
	return order
}
