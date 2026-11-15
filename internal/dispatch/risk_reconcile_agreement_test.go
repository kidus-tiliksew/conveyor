package dispatch

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TestReviewRoundHoldAgreesWithReconcile proves that dispatch and durable
// reconcile hold the same task for the same head: the submitted head, or the
// refresh head while the approval is stale. While that head's risk order is
// open, dispatch builds no round and reconcile reports a hold. Once it ends,
// or for another head's order or a record task, dispatch opens the round of
// that head and reconcile reports none (component-review invariant 41;
// req-risk-based-review AC-1.6, AC-5.10; component-risk-review invariant 13).
func TestReviewRoundHoldAgreesWithReconcile(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewVolatileBackend()
	defer st.Close()
	cfg := &config.Config{Workspace: "demo"}
	route := config.StageRoute{Execution: config.ExecutionMCP, TimeoutText: "1h"}
	d := New(st, cfg, nil)

	file := func(t *testing.T, mode riskpolicy.Mode) string {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: "demo", Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning,
			NextStage: core.StageImplement, PolicyVersion: 1, CreatedAt: time.Now().UTC(),
			SetupContract: config.PolicyContract{MaxBounces: 3, Risk: riskPolicyFor(mode), Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)}}}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// submitted records the head's pull request as the submission does.
	submitted := func(t *testing.T, taskID, head string) {
		t.Helper()
		if err := st.RecordVerificationPullRequest(ctx, core.Event{TaskID: taskID, Kind: store.SubmissionComparisonEventKind,
			Payload: core.JSONPayload(map[string]any{"base_sha": "base-" + head, "head_sha": head, "repository": "acme/repo", "number": 1})}); err != nil {
			t.Fatal(err)
		}
	}
	ensure := func(t *testing.T, taskID, head string, paths ...string) core.WorkOrder {
		t.Helper()
		result, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.HeadRiskOrderResult, error) {
			return st.EnsureHeadRiskOrderCommand(ctx, lease, store.HeadRiskOrderRequest{TaskID: taskID,
				Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base-" + head}, ChangedPaths: paths, QueueTimeout: time.Hour})
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Order
	}
	queue := func(t *testing.T, taskID string) {
		t.Helper()
		if _, err := taskops.New(st).Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
	}
	cancel := func(t *testing.T, taskID, orderID string) {
		t.Helper()
		if _, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, _, err := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: orderID, Reason: "agreement test"})
			return order, err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// agree asks reconcile's hold through public reads, then runs dispatch's
	// round builder, and requires both to answer held, or neither with a
	// round of head.
	agree := func(t *testing.T, taskID string, held bool, head string) {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		_, reconcileHeld, err := store.QueuedReviewRiskHold(task,
			func() ([]core.Event, error) { return st.ListCompactEvents(ctx, taskID) },
			func() ([]core.RiskOrder, error) { return st.ListTaskRiskOrders(ctx, taskID) })
		if err != nil {
			t.Fatal(err)
		}
		if reconcileHeld != held {
			t.Fatalf("reconcile held=%t, want %t", reconcileHeld, held)
		}
		if err := d.createReviewRound(ctx, cfg, task, route); err != nil {
			t.Fatal(err)
		}
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		var review []core.WorkOrder
		for _, order := range orders {
			if order.Stage == core.StageReview {
				review = append(review, order)
			}
		}
		if held {
			if len(review) != 0 {
				t.Fatalf("dispatch opened a round while reconcile holds: %+v", review)
			}
			return
		}
		if len(review) == 0 || review[0].HeadSHA != head {
			t.Fatalf("dispatch round=%+v, want a round of %s", review, head)
		}
	}

	t.Run("SubmittedHead", func(t *testing.T) {
		id := file(t, riskpolicy.ModeEnforce)
		submitted(t, id, "h1")
		order := ensure(t, id, "h1", "a.go")
		queue(t, id)
		agree(t, id, true, "h1")
		cancel(t, id, order.ID)
		agree(t, id, false, "h1")
	})

	t.Run("RefreshHead", func(t *testing.T) {
		id := file(t, riskpolicy.ModeEnforce)
		submitted(t, id, "h1")
		ensure(t, id, "h1", "a.go")
		queue(t, id)
		// The refresh creates the new head's order, which cancels the
		// submitted head's order, then marks the approval stale.
		refresh := ensure(t, id, "h2", "a.go", "b.go")
		if _, err := st.MarkTaskApprovalStale(ctx, id, "h1", "h2", config.RefreshReviewFull, "merge-conflict"); err != nil {
			t.Fatal(err)
		}
		agree(t, id, true, "h2")
		cancel(t, id, refresh.ID)
		agree(t, id, false, "h2")
	})

	t.Run("OtherHeadOrder", func(t *testing.T) {
		id := file(t, riskpolicy.ModeEnforce)
		ensure(t, id, "h0", "a.go")
		submitted(t, id, "h1")
		queue(t, id)
		agree(t, id, false, "h1")
	})

	t.Run("RecordTask", func(t *testing.T) {
		id := file(t, riskpolicy.ModeRecord)
		submitted(t, id, "h1")
		ensure(t, id, "h1", "a.go")
		queue(t, id)
		agree(t, id, false, "h1")
	})
}
