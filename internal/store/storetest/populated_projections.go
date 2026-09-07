package storetest

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// DEC-38, DEC-39: startup-only empty cases concealed populated-domain stubs.
// Every assertion uses public backend methods; PostgreSQL is the reference.
func runPopulatedProjections(t *testing.T, x Fixture) {
	ctx, owner := bootstrapOwner(t, x)
	x.Context = ctx
	st := x.Backend
	order := newAggregateOrder(t, x)
	_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session", ClientToken: "fixture", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	_, err = ReleaseWorkerClaim(ctx, st, order.ID, "worker", core.WorkOrderRelease{SessionID: "session", Reason: "operator checkpoint reached", Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the fixture proposal"}})
	requireOK(t, err)
	_, err = taskops.New(st).SetAssignee(ctx, order.TaskID, owner.ID)
	requireOK(t, err)
	candidates, err := st.ListCheckpointContextCandidates(ctx, "unattached")
	requireOK(t, err)
	if len(candidates) != 1 || candidates[0].ID != order.TaskID {
		t.Fatalf("checkpoint candidates=%+v", candidates)
	}
	// Released checkpoint orders contribute a stalled marker to caller attention.
	page, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: owner.ID, Limit: 1})
	requireOK(t, err)
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].ID != order.TaskID {
		t.Fatalf("caller attention=%+v", page)
	}
	page, err = st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: owner.ID, Limit: 1, Offset: 1})
	requireOK(t, err)
	if page.Total != 1 || len(page.Tasks) != 0 {
		t.Fatalf("attention offset=%+v", page)
	}
	page, err = st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: "other", Limit: 10})
	requireOK(t, err)
	if page.Total != 0 {
		t.Fatal("caller attention leaked an assignee")
	}
	requireOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: order.TaskID, Repository: "acme/conveyor", SpecVersion: 1}))
	wantPending := 0
	if st.IsDurable() {
		wantPending = 1
	}
	for range 2 {
		n, err := st.ReconcileGitHubLifecycles(ctx)
		requireOK(t, err)
		if n != wantPending {
			t.Fatalf("pending forge count=%d", n)
		}
	}
	events, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	queued := 0
	for _, e := range events {
		if e.Kind == "github_issue.publication_queued" {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("reconcile duplicated publication event: %d", queued)
	}
	lifecycle, found, err := st.GetGitHubLifecycle(ctx, order.TaskID)
	requireOK(t, err)
	if !found {
		t.Fatal("missing lifecycle")
	}
	lifecycle.State = core.GitHubPublicationPublished
	lifecycle.IssueNumber = 42
	lifecycle.IssueURL = "https://example.test/42"
	requireOK(t, st.UpdateGitHubLifecycle(ctx, lifecycle))
	n, err := st.ReconcileGitHubLifecycles(ctx)
	requireOK(t, err)
	if n != 0 {
		t.Fatalf("published forge count=%d", n)
	}
	parentID := core.NewTaskID()
	parent := core.Task{ID: parentID, Workspace: x.Workspace, Repo: "conveyor", Title: "Blueprint", BaseBranch: "main", Branch: "conveyor/task-" + parentID, State: core.TaskQueued, NextStage: core.StageImplement}
	requireOK(t, st.CreateTask(ctx, parent))
	child := parent
	child.ID = core.NewTaskID()
	child.Branch = "conveyor/task-" + child.ID
	child.ParentTaskID = parent.ID
	child.State = core.TaskClosed
	requireOK(t, st.CreateTask(ctx, child))
	n, err = st.ReconcileBlueprintClosures(ctx)
	requireOK(t, err)
	if n != 1 {
		t.Fatalf("blueprint closures=%d", n)
	}
	closed, err := st.GetTask(ctx, parent.ID)
	requireOK(t, err)
	if closed.State != core.TaskClosed {
		t.Fatalf("parent state=%s", closed.State)
	}
	n, err = st.ReconcileBlueprintClosures(ctx)
	requireOK(t, err)
	if n != 0 {
		t.Fatal("blueprint closed twice")
	}
	// The common populated case has no missing dispatch. Durable backends also
	// repair a consumed dispatch while the task remains queued.
	n, err = st.ReconcileQueuedTasks(ctx)
	requireOK(t, err)
	if n != 0 {
		t.Fatalf("unexpected queue repair=%d", n)
	}
	if st.IsDurable() {
		task := parent
		task.ID = core.NewTaskID()
		task.Branch = "conveyor/task-" + task.ID
		requireOK(t, st.CreateTask(ctx, task))
		stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), task.ID)
		job, err := logqueue.Load(ctx, st.Log(), x.Workspace, stream)
		requireOK(t, err)
		_, err = st.Log().Append(ctx, x.Workspace, stream, job.Head, []eventlog.NewEvent{{Kind: logqueue.KindCompleted, Payload: []byte(`{}`), At: time.Now().UTC()}})
		requireOK(t, err)
		n, err = st.ReconcileQueuedTasks(ctx)
		requireOK(t, err)
		if n != 1 {
			t.Fatalf("missing dispatch repairs=%d", n)
		}
		n, err = st.ReconcileQueuedTasks(ctx)
		requireOK(t, err)
		if n != 0 {
			t.Fatal("active dispatch enqueued twice")
		}
	}
}
