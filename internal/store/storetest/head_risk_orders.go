package storetest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunHeadRiskOrderConformance proves on every backend the head risk order
// lifecycle: the act that completes an implement order, or a refresh's head
// act, creates the head's risk order and cancels an earlier head's open risk
// order in one transaction, and a refused submission writes neither; a task
// whose mode is off, whose tier is high, or whose head changes no path outside
// the latest completed head risk order's paths gets none; an enforcing task's
// round of that head is refused without any write while its risk order is
// queued or claimed, and opens once the order completes, while a record task's
// round and an older head's open order never hold it; completing or cancelling
// an enforcing task's risk order enqueues the queued task's dispatch on a
// durable backend; a claimed risk order blocks no policy change; and merge and
// close cancel the task's open risk orders; a later risk order leaves a
// timed-out review seat or verify order holding its stage; and a path minimum
// that brings the tier to high cancels the task's open risk orders and
// enqueues nothing (component-submission-merge invariants 67 to 69;
// component-review invariant 41; component-work-orders invariants 2, 62, and
// 66; req-risk-based-review AC-1.6, AC-5.8 to AC-5.11, AC-12.3;
// component-risk-review invariants 12 and 13).
func RunHeadRiskOrderConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	fileTaskSeats := func(t *testing.T, policy *riskpolicy.Policy, state core.TaskState, next core.Stage, seats int) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: state, NextStage: next, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: policy, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats)},
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	fileTask := func(t *testing.T, policy *riskpolicy.Policy, state core.TaskState, next core.Stage) core.Task {
		t.Helper()
		return fileTaskSeats(t, policy, state, next, 1)
	}
	request := func(taskID, head string, paths ...string) store.HeadRiskOrderRequest {
		return store.HeadRiskOrderRequest{TaskID: taskID, Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: "base-" + head},
			ChangedPaths: paths, QueueTimeout: time.Hour}
	}
	ensure := func(t *testing.T, r store.HeadRiskOrderRequest) store.HeadRiskOrderResult {
		t.Helper()
		result, err := taskops.ExecuteWorkOrder(ctx, st, r.TaskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.HeadRiskOrderResult, error) {
			return st.EnsureHeadRiskOrderCommand(ctx, lease, r)
		})
		requireOK(t, err)
		return result
	}
	riskOrders := func(t *testing.T, taskID string) []core.RiskOrder {
		t.Helper()
		orders, err := st.ListTaskRiskOrders(ctx, taskID)
		requireOK(t, err)
		return orders
	}
	orderState := func(t *testing.T, id string) core.WorkOrderState {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		return order.State
	}
	countEvents := func(t *testing.T, taskID, kind string) int {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		n := 0
		for _, event := range events {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	claimRisk := func(t *testing.T, id, session string) {
		t.Helper()
		_, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "run:risk-agent", SessionID: session, ClientToken: session + "-token",
			Agent: "claude-code", Model: "model-" + session, Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
	}
	completeRisk := func(t *testing.T, taskID, id, session string, tier riskpolicy.Tier) {
		t.Helper()
		_, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
			return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: id,
				Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:risk-agent", SessionID: session}, Tier: tier, Reason: "rated " + string(tier)})
		})
		requireOK(t, err)
	}
	cancelRisk := func(t *testing.T, taskID, id string) {
		t.Helper()
		_, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, _, err := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: id, Reason: "storetest cancel"})
			return order, err
		})
		requireOK(t, err)
	}
	rateHigh := func(t *testing.T, taskID, head string) {
		t.Helper()
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule, Tier: riskpolicy.TierHigh,
			HeadSHA: head, BaseSHA: "base-" + head, DeclarationState: riskpolicy.DeclarationUnknown, Reason: "declaration unreadable"})
		requireOK(t, err)
	}
	claimedImplement := func(t *testing.T, taskID string) core.WorkOrder {
		t.Helper()
		id := fmt.Sprintf("%s-implement-%d", taskID, time.Now().UnixNano())
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
		now := time.Now().UTC()
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
		order, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "run:implementer", SessionID: "implementer-" + id, ClientToken: "implementer-token-" + id,
			Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		return order
	}
	submit := func(t *testing.T, order core.WorkOrder, head string, risk *store.HeadRiskOrderRequest) error {
		t.Helper()
		order.State, order.HeadSHA = core.WorkOrderSubmitted, head
		_, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdSubmitForReview, func(lease taskops.TaskLease) (core.Job, error) {
			return st.SubmitImplementationCommand(ctx, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC(), HeadRisk: risk})
		})
		return err
	}
	members := func(taskID, head string, round, count int) ([]core.Job, []core.WorkOrder) {
		var jobs []core.Job
		var orders []core.WorkOrder
		now := time.Now().UTC()
		for seat := 1; seat <= count; seat++ {
			id := fmt.Sprintf("%s-review-%d-seat-%d", taskID, round, seat)
			jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
			orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat,
				HeadSHA: head, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
		}
		return jobs, orders
	}
	openRound := func(t *testing.T, taskID, head string, round int) error {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		in, err := store.LoadReviewRoundSeatInputs(ctx, st, task, store.ReviewSeatMinimum(task.SetupContract, 0), round)
		requireOK(t, err)
		seats := store.ResolveReviewRoundSeats(in)
		jobs, orders := members(taskID, head, round, seats.SeatCount)
		_, err = taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
			return struct{}{}, st.CreateReviewRoundCommand(ctx, lease, taskID, seats, jobs, orders)
		})
		return err
	}
	reviewOrders := func(t *testing.T, taskID string) int {
		t.Helper()
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		requireOK(t, err)
		n := 0
		for _, order := range orders {
			if order.Stage == core.StageReview {
				n++
			}
		}
		return n
	}
	seatRecords := func(t *testing.T, taskID string) []store.ReviewRoundSeats {
		t.Helper()
		records, err := st.ListReviewRoundSeats(ctx, taskID)
		requireOK(t, err)
		return records
	}
	dispatchJob := func(t *testing.T, taskID string) (logqueue.Job, eventlog.StreamID) {
		t.Helper()
		stream := logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), taskID)
		job, err := logqueue.Load(ctx, st.Log(), x.Workspace, stream)
		requireOK(t, err)
		return job, stream
	}
	finishDispatch := func(t *testing.T, taskID string) {
		t.Helper()
		job, stream := dispatchJob(t, taskID)
		if !job.Active() {
			return
		}
		_, err := st.Log().Append(ctx, x.Workspace, stream, job.Head, []eventlog.NewEvent{{Kind: logqueue.KindCompleted, Payload: []byte(`{}`), At: time.Now().UTC()}})
		requireOK(t, err)
	}
	queueTask := func(t *testing.T, taskID string) {
		t.Helper()
		_, err := taskops.New(st).Perform(ctx, taskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
		requireOK(t, err)
		if st.IsDurable() {
			finishDispatch(t, taskID)
		}
	}

	for _, mode := range []*riskpolicy.Policy{&record, &enforce} {
		t.Run("SubmissionCreatesTheHeadOrderAndCancelsEarlierHeads/"+string(mode.Mode), func(t *testing.T) {
			task := fileTask(t, mode, core.TaskRunning, core.StageImplement)
			earlier := ensure(t, request(task.ID, "h0", "a.go"))
			if !earlier.Created || earlier.Order.Stage != core.StageRisk {
				t.Fatalf("earlier head order=%+v", earlier)
			}
			implement := claimedImplement(t, task.ID)
			risk := request(task.ID, "h1", "a.go", "b.go")
			requireOK(t, submit(t, implement, "h1", &risk))
			if got := orderState(t, earlier.Order.ID); got != core.WorkOrderCancelled {
				t.Fatalf("earlier head order state=%s, want cancelled", got)
			}
			orders := riskOrders(t, task.ID)
			current, ok := store.HeadRiskOrderFor(orders, "h1")
			if !ok || current.Order.State != core.WorkOrderQueued || current.Subject.BaseSHA != "base-h1" || !slices.Equal(current.ChangedPaths, []string{"a.go", "b.go"}) {
				t.Fatalf("submitted head order=%+v ok=%t", current, ok)
			}
			submitted, err := st.GetWorkOrder(ctx, implement.ID)
			requireOK(t, err)
			if submitted.State != core.WorkOrderSubmitted {
				t.Fatalf("implement order state=%s", submitted.State)
			}
			// A replay of the same head returns the same order and writes
			// nothing again.
			again := ensure(t, risk)
			if again.Created || again.Order.ID != current.Order.ID || len(again.Cancelled) != 0 || len(riskOrders(t, task.ID)) != 2 {
				t.Fatalf("replay=%+v orders=%+v", again, riskOrders(t, task.ID))
			}
		})
	}

	t.Run("RefusedSubmissionWritesNoRiskEffect", func(t *testing.T) {
		task := fileTask(t, &record, core.TaskRunning, core.StageImplement)
		earlier := ensure(t, request(task.ID, "h0", "a.go"))
		implement := claimedImplement(t, task.ID)
		requireOK(t, st.UpdateJob(ctx, core.Job{ID: implement.JobID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobFailed, EndedAt: time.Now().UTC()}))
		risk := request(task.ID, "h1", "a.go")
		if err := submit(t, implement, "h1", &risk); err == nil {
			t.Fatal("a submission with a failed job was accepted")
		}
		if got := orderState(t, earlier.Order.ID); got != core.WorkOrderQueued {
			t.Fatalf("earlier head order state=%s after a refused submission", got)
		}
		if orders := riskOrders(t, task.ID); len(orders) != 1 {
			t.Fatalf("refused submission left risk orders %+v", orders)
		}
	})

	t.Run("OffHighAndCoveredHeadsGetNoOrder", func(t *testing.T) {
		off := fileTask(t, nil, core.TaskRunning, core.StageImplement)
		if result := ensure(t, request(off.ID, "h1", "a.go")); result.Created || result.Order.ID != "" || len(riskOrders(t, off.ID)) != 0 {
			t.Fatalf("off task result=%+v", result)
		}

		high := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		rateHigh(t, high.ID, "h1")
		if result := ensure(t, request(high.ID, "h1", "a.go")); result.Created || len(riskOrders(t, high.ID)) != 0 {
			t.Fatalf("high task result=%+v", result)
		}

		covered := fileTask(t, &record, core.TaskRunning, core.StageImplement)
		first := ensure(t, request(covered.ID, "h1", "a.go", "b.go"))
		claimRisk(t, first.Order.ID, "risk-covered")
		completeRisk(t, covered.ID, first.Order.ID, "risk-covered", riskpolicy.TierLow)
		// Every changed path of h2 was rated with h1: the earlier rating
		// carries and no order is created.
		if result := ensure(t, request(covered.ID, "h2", "b.go")); result.Created || result.Order.ID != "" {
			t.Fatalf("covered head result=%+v", result)
		}
		// A new path needs a rating.
		if result := ensure(t, request(covered.ID, "h3", "b.go", "c.go")); !result.Created {
			t.Fatalf("uncovered head result=%+v", result)
		}
		// An open order covers nothing: h4 repeats h3's paths while h3's
		// order is still queued, so h4 gets its own order and h3's is
		// cancelled.
		h3, _ := store.HeadRiskOrderFor(riskOrders(t, covered.ID), "h3")
		h4 := ensure(t, request(covered.ID, "h4", "c.go"))
		if !h4.Created || !slices.Equal(h4.Cancelled, []string{h3.Order.ID}) {
			t.Fatalf("h4 result=%+v, want h3 %s cancelled", h4, h3.Order.ID)
		}
	})

	t.Run("EnforcingRoundWaitsForItsHeadRiskOrder", func(t *testing.T) {
		task := fileTask(t, &enforce, core.TaskRunning, core.StageReview)
		created := ensure(t, request(task.ID, "h1", "a.go"))
		eventsBefore := countEvents(t, task.ID, "review.round_created")
		err := openRound(t, task.ID, "h1", 1)
		if !errors.Is(err, store.ErrReviewRoundHeldByRiskOrder) {
			t.Fatalf("held round err=%v", err)
		}
		if reviewOrders(t, task.ID) != 0 || len(seatRecords(t, task.ID)) != 0 || countEvents(t, task.ID, "review.round_created") != eventsBefore {
			t.Fatalf("a held round wrote state: orders=%d records=%+v", reviewOrders(t, task.ID), seatRecords(t, task.ID))
		}
		claimRisk(t, created.Order.ID, "risk-held")
		if err = openRound(t, task.ID, "h1", 1); !errors.Is(err, store.ErrReviewRoundHeldByRiskOrder) {
			t.Fatalf("claimed risk order round err=%v", err)
		}
		completeRisk(t, task.ID, created.Order.ID, "risk-held", riskpolicy.TierMedium)
		requireOK(t, openRound(t, task.ID, "h1", 1))
		if reviewOrders(t, task.ID) != 1 || countEvents(t, task.ID, "review.round_created") != eventsBefore+1 {
			t.Fatalf("released round orders=%d", reviewOrders(t, task.ID))
		}
	})

	t.Run("EnforcingRetryWaitsForItsHeadRiskOrder", func(t *testing.T) {
		task := fileTaskSeats(t, &enforce, core.TaskRunning, core.StageReview, 2)
		jobs, orders := reviewRound(task.ID, 1)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "retry-seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		first.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
		_, err = ClaimWorkOrder(ctx, st, orders[1].ID, core.WorkOrderClaim{SessionID: "retry-seat-two", ClientToken: "two", Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
		requireOK(t, err)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		created := ensure(t, request(task.ID, "fixture-head", "a.go"))
		jobs2, orders2 := reviewRound(task.ID, 2)
		retry := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "risk-retry-" + task.ID, PriorRound: 1, Reason: "seat timed out", PRHead: "fixture-head"}
		before := reviewOrders(t, task.ID)
		if _, err = RetryReviewRound(ctx, st, retry, jobs2, orders2); !errors.Is(err, store.ErrReviewRoundHeldByRiskOrder) || !errors.Is(err, store.ErrReviewRetryConflict) {
			t.Fatalf("held retry err=%v", err)
		}
		if reviewOrders(t, task.ID) != before {
			t.Fatalf("a held retry wrote seats: %d, want %d", reviewOrders(t, task.ID), before)
		}
		cancelRisk(t, task.ID, created.Order.ID)
		result, err := RetryReviewRound(ctx, st, retry, jobs2, orders2)
		requireOK(t, err)
		if result.NewRound != 2 || len(result.WorkOrders) != 2 {
			t.Fatalf("released retry=%+v", result)
		}
	})

	t.Run("OlderHeadsAndRecordTasksHoldNoRound", func(t *testing.T) {
		older := fileTask(t, &enforce, core.TaskRunning, core.StageReview)
		ensure(t, request(older.ID, "h0", "a.go"))
		// The h0 order is open, but the round reviews h1.
		requireOK(t, openRound(t, older.ID, "h1", 1))

		recorded := fileTask(t, &record, core.TaskRunning, core.StageReview)
		ensure(t, request(recorded.ID, "h1", "a.go"))
		requireOK(t, openRound(t, recorded.ID, "h1", 1))
	})

	t.Run("RiskOrderEndEnqueuesTheEnforcingTask", func(t *testing.T) {
		for _, scenario := range []string{"complete", "cancel", "record"} {
			t.Run(scenario, func(t *testing.T) {
				mode := &enforce
				if scenario == "record" {
					mode = &record
				}
				task := fileTask(t, mode, core.TaskRunning, core.StageImplement)
				created := ensure(t, request(task.ID, "h1", "a.go"))
				queueTask(t, task.ID)
				if scenario == "cancel" {
					cancelRisk(t, task.ID, created.Order.ID)
				} else {
					claimRisk(t, created.Order.ID, "risk-wake-"+scenario)
					completeRisk(t, task.ID, created.Order.ID, "risk-wake-"+scenario, riskpolicy.TierLow)
				}
				if !st.IsDurable() {
					return
				}
				job, _ := dispatchJob(t, task.ID)
				if want := scenario != "record"; job.Active() != want {
					t.Fatalf("dispatch job active=%t, want %t: %+v", job.Active(), want, job)
				}
			})
		}
	})

	t.Run("ClaimedRiskOrderBlocksNoPolicyChange", func(t *testing.T) {
		task := fileTask(t, &enforce, core.TaskQueued, core.StageReview)
		created := ensure(t, request(task.ID, "h1", "a.go"))
		claimRisk(t, created.Order.ID, "risk-policy")
		on := true
		_, err := taskops.ExecuteSetupChange(ctx, st, task.ID, func(lease taskops.TaskLease) (store.SetupChangeResult, error) {
			return st.ChangeTaskPolicyCommand(ctx, lease, store.SetupChangeRequest{TaskID: task.ID, RequestID: "risk-policy-" + task.ID, Reason: "storetest",
				Policy: &store.TaskPolicyChange{ReviewDistinctModels: &on}})
		})
		requireOK(t, err)
		if got := orderState(t, created.Order.ID); got != core.WorkOrderClaimed {
			t.Fatalf("policy change moved the risk order to %s", got)
		}
	})

	t.Run("MergeAndCloseCancelOpenRiskOrders", func(t *testing.T) {
		merged := fileTask(t, &record, core.TaskQueued, core.StageSpec)
		queued := ensure(t, request(merged.ID, "h1", "a.go")).Order.ID
		// merge.recover is the edge an observed forge merge takes.
		_, err := taskops.New(st).Perform(ctx, merged.ID, taskops.Command{Kind: core.TaskMergeRecover})
		requireOK(t, err)
		if got := orderState(t, queued); got != core.WorkOrderCancelled {
			t.Fatalf("merged task risk order state=%s", got)
		}

		closed := fileTask(t, &enforce, core.TaskQueued, core.StageImplement)
		claimed := ensure(t, request(closed.ID, "h1", "a.go")).Order.ID
		claimRisk(t, claimed, "risk-close")
		var cancelled core.Task
		cancelled, err = taskops.New(st).Cancel(ctx, core.Intervention{TaskID: closed.ID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
		requireOK(t, err)
		if cancelled.State != core.TaskClosed || orderState(t, claimed) != core.WorkOrderCancelled {
			t.Fatalf("closed task=%s risk order=%s", cancelled.State, orderState(t, claimed))
		}
	})

	// A risk order is created beside the task's other orders and never counts
	// as the later order. A timed-out review seat or verify order followed by
	// a risk order still holds its stage: the dispatcher's read finds the
	// stopped order as the holder, stage creation mints no second order, the
	// held task names the stopped order, and durable reconcile enqueues
	// nothing for it (component-work-orders invariants 2 and 62;
	// DEC-135(10); component-durable-queue "Reconciliation between tasks and
	// jobs").
	timeOut := func(t *testing.T, id string) {
		t.Helper()
		_, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{SessionID: "timeout-" + id, ClientToken: "timeout-token-" + id, Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
		requireOK(t, err)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		if got := orderState(t, id); got != core.WorkOrderTimedOut {
			t.Fatalf("order %s state=%s, want timed_out", id, got)
		}
	}
	requireStageHeld := func(t *testing.T, taskID string, stage core.Stage, holder string) {
		t.Helper()
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		if latest := orders[len(orders)-1]; latest.Stage != core.StageRisk {
			t.Fatalf("latest order %s is at stage %s, want a risk order", latest.ID, latest.Stage)
		}
		if got, held := core.WorkOrderBlocksStageMint(orders, stage); !held || got.ID != holder {
			t.Fatalf("%s holder=%q held=%t, want %s", stage, got.ID, held, holder)
		}
		if err = st.EnsureTaskEnqueued(ctx, taskID); !errors.Is(err, store.ErrTaskHeldByWorkOrder) || !strings.Contains(err.Error(), holder) {
			t.Fatalf("enqueue err=%v, want a hold by %s", err, holder)
		}
		if !st.IsDurable() {
			return
		}
		reconcileCtx, repairs := countReconciled(ctx, st)
		for range 3 {
			_, err = st.ReconcileQueuedTasks(reconcileCtx)
			requireOK(t, err)
		}
		got, err := repairs(taskID)
		requireOK(t, err)
		if job, _ := dispatchJob(t, taskID); got != 0 || job.Active() {
			t.Fatalf("reconcile repaired a held task: repairs=%d job=%+v", got, job)
		}
	}

	t.Run("LaterRiskOrderKeepsATimedOutReviewSeatHeld", func(t *testing.T) {
		task := fileTask(t, &record, core.TaskRunning, core.StageReview)
		requireOK(t, openRound(t, task.ID, "h1", 1))
		seat := task.ID + "-review-1-seat-1"
		timeOut(t, seat)
		if created := ensure(t, request(task.ID, "h1", "a.go")); !created.Created {
			t.Fatalf("head risk order=%+v", created)
		}
		queueTask(t, task.ID)
		requireStageHeld(t, task.ID, core.StageReview, seat)
		if n := reviewOrders(t, task.ID); n != 1 || orderState(t, seat) != core.WorkOrderTimedOut {
			t.Fatalf("review orders=%d seat state=%s, want the one timed-out seat", n, orderState(t, seat))
		}
	})

	t.Run("LaterRiskOrderKeepsATimedOutVerifyOrderHeld", func(t *testing.T) {
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "h1", PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, VerifyStage: true, Risk: &record, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 1)},
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
		requireOK(t, st.CreateTask(ctx, task))
		verifyOrder := func(n int) (core.Job, core.WorkOrder) {
			orderID := fmt.Sprintf("%s-verify-%d", id, n)
			now := time.Now().UTC()
			return core.Job{ID: orderID, TaskID: id, Stage: core.StageVerify, State: core.JobPending},
				core.WorkOrder{ID: orderID, TaskID: id, JobID: orderID, Stage: core.StageVerify, HeadSHA: "h1", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
		}
		job, order := verifyOrder(1)
		made, err := CreateStageWorkOrder(ctx, st, job, order)
		requireOK(t, err)
		if !made {
			t.Fatal("the first verify order was not created")
		}
		timeOut(t, order.ID)
		if created := ensure(t, request(id, "h1", "a.go")); !created.Created {
			t.Fatalf("head risk order=%+v", created)
		}
		job2, order2 := verifyOrder(2)
		made, err = CreateStageWorkOrder(ctx, st, job2, order2)
		requireOK(t, err)
		if made {
			t.Fatal("a second verify order was minted beside the timed-out one")
		}
		if _, err = st.GetWorkOrder(ctx, order2.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second verify order read err=%v, want not found", err)
		}
		_, err = taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageVerify, ProjectStages: true})
		requireOK(t, err)
		if st.IsDurable() {
			finishDispatch(t, id)
		}
		requireStageHeld(t, id, core.StageVerify, order.ID)
	})

	// A path minimum that brings the tier to high cancels every open risk
	// order of the task, a live claim included, in the act that records it.
	// The cancellation records no rating of its own and enqueues nothing, so
	// a queued task's dispatch stays as it was. A lower path minimum, and a
	// replay that records no rating, cancel nothing (component-work-orders
	// invariant 66; component-submission-merge invariants 67 to 69;
	// req-risk-based-review AC-12.3).
	pathRating := func(t *testing.T, taskID, head string, tier riskpolicy.Tier) (core.RiskRating, bool) {
		t.Helper()
		rating, created, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule, Tier: tier,
			HeadSHA: head, DeclarationState: riskpolicy.DeclarationUnknown, Reason: "path minimum " + string(tier)})
		requireOK(t, err)
		return rating, created
	}
	planRiskOrder := func(t *testing.T, taskID string, version int) core.WorkOrder {
		t.Helper()
		order, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
			order, made, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: taskID, Subject: core.RiskOrderSubject{PlanVersion: version}, QueueTimeout: time.Hour})
			if err == nil && !made {
				err = fmt.Errorf("plan risk order for version %d was not created", version)
			}
			return order, err
		})
		requireOK(t, err)
		return order
	}

	for _, mode := range []*riskpolicy.Policy{&record, &enforce} {
		t.Run("PathMinimumToHighCancelsOpenRiskOrders/"+string(mode.Mode), func(t *testing.T) {
			task := fileTask(t, mode, core.TaskRunning, core.StageImplement)
			headOrder := ensure(t, request(task.ID, "h0", "a.go")).Order
			plan := planRiskOrder(t, task.ID, 1)
			session := "path-high-" + task.ID
			claimRisk(t, plan.ID, session)
			queueTask(t, task.ID)
			ratingsBefore, err := st.ListTaskRiskRatings(ctx, task.ID)
			requireOK(t, err)
			if _, created := pathRating(t, task.ID, "h1", riskpolicy.TierHigh); !created {
				t.Fatal("the high path minimum was not recorded")
			}
			for _, id := range []string{headOrder.ID, plan.ID} {
				got, err := st.GetWorkOrder(ctx, id)
				requireOK(t, err)
				if got.State != core.WorkOrderCancelled || got.SessionID != "" {
					t.Fatalf("risk order %s after a high path minimum=%+v", id, got)
				}
			}
			if got := countEvents(t, task.ID, "work_order.cancelled"); got != 2 {
				t.Fatalf("work_order.cancelled events=%d, want 2", got)
			}
			events, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)
			reasons := 0
			for _, event := range events {
				if event.Kind == "work_order.cancelled" && strings.Contains(string(event.Payload), store.RiskTierHighCancelReason) {
					reasons++
				}
			}
			if reasons != 2 {
				t.Fatalf("cancellations naming the high-tier reason=%d, want 2", reasons)
			}
			ratingsAfter, err := st.ListTaskRiskRatings(ctx, task.ID)
			requireOK(t, err)
			if len(ratingsAfter) != len(ratingsBefore)+1 || ratingsAfter[len(ratingsAfter)-1].Source != core.RiskSourcePathRule {
				t.Fatalf("ratings after the path minimum=%+v, want only the path rating added", ratingsAfter)
			}
			// The claimed order's late completion is refused.
			_, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
				return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: plan.ID,
					Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:risk-agent", SessionID: session}, Tier: riskpolicy.TierLow, Reason: "late"})
			})
			if !errors.Is(err, store.ErrWorkOrderCancelled) {
				t.Fatalf("late completion after the high path minimum err=%v", err)
			}
			if st.IsDurable() {
				if job, _ := dispatchJob(t, task.ID); job.Active() {
					t.Fatalf("the path minimum cancellation enqueued the task: %+v", job)
				}
			}
		})
	}

	t.Run("LowerPathMinimumsAndReplaysCancelNothing", func(t *testing.T) {
		task := fileTask(t, &record, core.TaskRunning, core.StageImplement)
		if _, created := pathRating(t, task.ID, "h1", riskpolicy.TierMedium); !created {
			t.Fatal("the medium path minimum was not recorded")
		}
		open := ensure(t, request(task.ID, "h2", "a.go")).Order
		plan := planRiskOrder(t, task.ID, 1)
		// A replay of h1 with a high read records no rating, so the tier
		// stays medium and nothing is cancelled.
		if replay, created := pathRating(t, task.ID, "h1", riskpolicy.TierHigh); created || replay.Tier != riskpolicy.TierMedium {
			t.Fatalf("replayed path minimum=%+v created=%t", replay, created)
		}
		if _, created := pathRating(t, task.ID, "h2", riskpolicy.TierLow); !created {
			t.Fatal("the low path minimum was not recorded")
		}
		for _, id := range []string{open.ID, plan.ID} {
			if got := orderState(t, id); got != core.WorkOrderQueued {
				t.Fatalf("risk order %s state=%s, want queued", id, got)
			}
		}
		if got := countEvents(t, task.ID, "work_order.cancelled"); got != 0 {
			t.Fatalf("work_order.cancelled events=%d, want 0", got)
		}
	})

	// Durable start-up reconcile applies the dispatch hold: an enforcing
	// queued review task whose head risk order is queued or claimed is not
	// drift, so reconcile enqueues nothing and reports no repair for it. Once
	// the order ends, or for record and off tasks, other heads, a missing
	// order, and plan orders, reconcile repairs a lost dispatch as before
	// (component-review invariant 41; req-risk-based-review AC-1.6,
	// AC-5.10; component-risk-review invariant 13; component-durable-queue
	// "Reconciliation between tasks and jobs"). The memory backend keeps no
	// durable queue to reconcile.
	if !st.IsDurable() {
		return
	}
	reconcileCtx, repairs := countReconciled(ctx, st)
	reconcile := func(t *testing.T, ticks int) {
		t.Helper()
		for range ticks {
			_, err := st.ReconcileQueuedTasks(reconcileCtx)
			requireOK(t, err)
		}
	}
	requireRepairs := func(t *testing.T, taskID string, want int) {
		t.Helper()
		got, err := repairs(taskID)
		requireOK(t, err)
		if got != want {
			t.Fatalf("task %s repairs=%d, want %d", taskID, got, want)
		}
	}
	requireDispatch := func(t *testing.T, taskID string, active bool) {
		t.Helper()
		if job, _ := dispatchJob(t, taskID); job.Active() != active {
			t.Fatalf("task %s dispatch job active=%t, want %t: %+v", taskID, job.Active(), active, job)
		}
	}
	// submitHead records the head's pull request and submits it, as the
	// implementation submission does, then moves the task to a queued review
	// whose dispatch job already ran and wrote nothing.
	submitHead := func(t *testing.T, task core.Task, head string, withRisk bool, paths ...string) {
		t.Helper()
		implement := claimedImplement(t, task.ID)
		requireOK(t, st.RecordVerificationPullRequest(ctx, core.Event{TaskID: task.ID, JobID: implement.JobID, Kind: store.SubmissionComparisonEventKind,
			Payload: core.JSONPayload(map[string]any{"url": "https://github.com/fixture/reconcile/pull/1", "number": 1, "base_sha": "base-" + head, "head_sha": head,
				"repository": "fixture/reconcile", "work_order_id": implement.ID})}))
		var risk *store.HeadRiskOrderRequest
		if withRisk {
			if len(paths) == 0 {
				paths = []string{"a.go"}
			}
			r := request(task.ID, head, paths...)
			risk = &r
		}
		requireOK(t, submit(t, implement, head, risk))
		queueTask(t, task.ID)
	}
	headOrder := func(t *testing.T, taskID, head string) core.RiskOrder {
		t.Helper()
		order, ok := store.HeadRiskOrderFor(riskOrders(t, taskID), head)
		if !ok {
			t.Fatalf("task %s has no risk order for head %s", taskID, head)
		}
		return order
	}

	t.Run("ReconcileLeavesARiskHeldReviewTaskAlone", func(t *testing.T) {
		for _, scenario := range []string{"queued-then-cancelled", "claimed-then-completed"} {
			t.Run(scenario, func(t *testing.T) {
				task := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
				submitHead(t, task, "h1", true)
				order := headOrder(t, task.ID, "h1")
				if scenario == "claimed-then-completed" {
					claimRisk(t, order.Order.ID, "reconcile-held-"+task.ID)
				}
				reconcile(t, 3)
				requireRepairs(t, task.ID, 0)
				requireDispatch(t, task.ID, false)
				if reviewOrders(t, task.ID) != 0 {
					t.Fatalf("a held task got review orders: %d", reviewOrders(t, task.ID))
				}
				if scenario == "claimed-then-completed" {
					completeRisk(t, task.ID, order.Order.ID, "reconcile-held-"+task.ID, riskpolicy.TierLow)
				} else {
					cancelRisk(t, task.ID, order.Order.ID)
				}
				// The end of the risk order wakes the task in its own act.
				requireDispatch(t, task.ID, true)
				reconcile(t, 2)
				requireRepairs(t, task.ID, 0)
				// A wake whose dispatch ended without a round is real drift
				// now, and reconcile repairs it once.
				finishDispatch(t, task.ID)
				reconcile(t, 3)
				requireRepairs(t, task.ID, 1)
				requireDispatch(t, task.ID, true)
				requireOK(t, openRound(t, task.ID, "h1", 1))
			})
		}
	})

	t.Run("ReconcileRepairsReviewTasksNoRiskOrderHolds", func(t *testing.T) {
		missing := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		submitHead(t, missing, "h1", false)
		if orders := riskOrders(t, missing.ID); len(orders) != 0 {
			t.Fatalf("missing-order task has risk orders %+v", orders)
		}
		otherHead := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		older := ensure(t, request(otherHead.ID, "h0", "a.go"))
		submitHead(t, otherHead, "h1", false)
		recordQueued := fileTask(t, &record, core.TaskRunning, core.StageImplement)
		submitHead(t, recordQueued, "h1", true)
		recordClaimed := fileTask(t, &record, core.TaskRunning, core.StageImplement)
		submitHead(t, recordClaimed, "h1", true)
		recordClaimedOrder := headOrder(t, recordClaimed.ID, "h1")
		claimRisk(t, recordClaimedOrder.Order.ID, "reconcile-record-"+recordClaimed.ID)
		off := fileTask(t, nil, core.TaskRunning, core.StageImplement)
		submitHead(t, off, "h1", true)
		// A plan risk order holds a plan approval, never a review round: a
		// task that took a plan at triage and then advanced to review with
		// its plan order still open is repaired.
		planAtReview := fileTask(t, &enforce, core.TaskQueued, core.StageTriage)
		_, err := taskops.ExecuteSetupChange(ctx, st, planAtReview.ID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: planAtReview.ID, RequestID: "reconcile-plan-review-" + planAtReview.ID, Reason: "operator plan", Plan: planRiskPlan("", "")})
		})
		requireOK(t, err)
		var planAtReviewOrder core.RiskOrder
		for _, order := range riskOrders(t, planAtReview.ID) {
			if order.Subject.PlanVersion > 0 && order.Open() {
				planAtReviewOrder = order
			}
		}
		if planAtReviewOrder.Order.ID == "" {
			t.Fatalf("plan-at-review task has no open plan risk order: %+v", riskOrders(t, planAtReview.ID))
		}
		requireOK(t, st.RecordVerificationPullRequest(ctx, core.Event{TaskID: planAtReview.ID, Kind: store.SubmissionComparisonEventKind,
			Payload: core.JSONPayload(map[string]any{"base_sha": "base-h1", "head_sha": "h1", "repository": "fixture/reconcile", "number": 2})}))
		_, err = taskops.New(st).Perform(ctx, planAtReview.ID, taskops.Command{Kind: core.TaskDispatchStart})
		requireOK(t, err)
		_, err = taskops.New(st).Perform(ctx, planAtReview.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
		requireOK(t, err)
		finishDispatch(t, planAtReview.ID)
		if current, err := st.GetTask(ctx, planAtReview.ID); err != nil || current.State != core.TaskQueued || current.NextStage != core.StageReview {
			t.Fatalf("plan-at-review task=%+v err=%v, want queued at review", current, err)
		}
		// An ordinary order hold stays: the round of h1 is open.
		roundOpen := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		submitHead(t, roundOpen, "h1", true)
		roundOrder := headOrder(t, roundOpen.ID, "h1")
		cancelRisk(t, roundOpen.ID, roundOrder.Order.ID)
		finishDispatch(t, roundOpen.ID)
		requireOK(t, openRound(t, roundOpen.ID, "h1", 1))
		finishDispatch(t, roundOpen.ID)

		reconcile(t, 3)
		for _, id := range []string{missing.ID, otherHead.ID, recordQueued.ID, recordClaimed.ID, off.ID, planAtReview.ID} {
			requireRepairs(t, id, 1)
			requireDispatch(t, id, true)
		}
		requireRepairs(t, roundOpen.ID, 0)
		requireDispatch(t, roundOpen.ID, false)
		// Reconcile ends no risk order.
		if got := orderState(t, older.Order.ID); got != core.WorkOrderQueued {
			t.Fatalf("other-head order state=%s, want queued", got)
		}
		if got := orderState(t, headOrder(t, recordQueued.ID, "h1").Order.ID); got != core.WorkOrderQueued {
			t.Fatalf("record order state=%s, want queued", got)
		}
		if got := orderState(t, recordClaimedOrder.Order.ID); got != core.WorkOrderClaimed {
			t.Fatalf("record claimed order state=%s, want claimed", got)
		}
		if got := orderState(t, planAtReviewOrder.Order.ID); got != core.WorkOrderQueued {
			t.Fatalf("plan order state=%s, want queued", got)
		}
	})

	t.Run("ReconcileHoldsTheRefreshHead", func(t *testing.T) {
		task := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		submitHead(t, task, "h1", true)
		first := headOrder(t, task.ID, "h1")
		claimRisk(t, first.Order.ID, "reconcile-refresh-"+task.ID)
		completeRisk(t, task.ID, first.Order.ID, "reconcile-refresh-"+task.ID, riskpolicy.TierLow)
		finishDispatch(t, task.ID)
		// The refresh creates the new head's risk order, then marks the
		// approval stale, as dispatch's refresh does. The submitted head's
		// order is completed, so only the refresh head can hold the round.
		refreshed := ensure(t, request(task.ID, "h2", "a.go", "b.go"))
		if !refreshed.Created {
			t.Fatalf("refresh head order=%+v", refreshed)
		}
		changed, err := st.MarkTaskApprovalStale(ctx, task.ID, "h1", "h2", config.RefreshReviewDelta, "merge-conflict")
		requireOK(t, err)
		if !changed {
			t.Fatal("the approval did not become stale")
		}
		finishDispatch(t, task.ID)
		reconcile(t, 3)
		requireRepairs(t, task.ID, 0)
		requireDispatch(t, task.ID, false)
		cancelRisk(t, task.ID, refreshed.Order.ID)
		requireDispatch(t, task.ID, true)
		finishDispatch(t, task.ID)
		reconcile(t, 3)
		requireRepairs(t, task.ID, 1)
	})

	t.Run("ReconcileGivesPlanRiskOrdersNoDispatchHold", func(t *testing.T) {
		planning := func(t *testing.T, state core.TaskState, next core.Stage) core.Task {
			t.Helper()
			id := core.NewTaskID()
			task := core.Task{ID: id, Workspace: x.Workspace, Source: "api", Repo: "conveyor", Title: id, Body: "Reconcile plan risk " + id, BaseBranch: "main",
				Branch: "conveyor/task-" + id, State: state, NextStage: next, PlanState: core.PlanNeeded, PolicyVersion: 1,
				CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
			task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}}
			requireOK(t, st.CreateTask(ctx, task))
			return task
		}
		planOrder := func(t *testing.T, taskID string) core.RiskOrder {
			t.Helper()
			for _, order := range riskOrders(t, taskID) {
				if order.Subject.PlanVersion > 0 && order.Open() {
					return order
				}
			}
			t.Fatalf("task %s has no open plan risk order", taskID)
			return core.RiskOrder{}
		}
		// A plan that waits for approval leaves the queue: the task waits
		// for a person, so reconcile neither holds nor repairs it, and the
		// approval still waits for the plan's risk order.
		awaiting := planning(t, core.TaskRunning, core.StageSpec)
		_, err := taskops.New(st).Perform(ctx, awaiting.ID, taskops.Command{
			Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
			PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
			PlanVersion: &core.SpecVersion{Content: planRiskPlan("", ""), Acceptance: []byte(`[]`), Decomposition: []byte(`[]`), Agent: "codex", Model: "planner"},
		})
		requireOK(t, err)
		awaitingOrder := planOrder(t, awaiting.ID)
		// A plan attached at triage keeps the task queued with an open plan
		// risk order. The order holds the plan approval, not the dispatch.
		triage := planning(t, core.TaskQueued, core.StageTriage)
		_, err = taskops.ExecuteSetupChange(ctx, st, triage.ID, func(lease taskops.TaskLease) (store.AttachPlanResult, error) {
			return st.AttachTaskPlanCommand(ctx, lease, store.AttachPlanRequest{TaskID: triage.ID, RequestID: "reconcile-attach-" + triage.ID, Reason: "operator plan", Plan: planRiskPlan("", "")})
		})
		requireOK(t, err)
		triageOrder := planOrder(t, triage.ID)
		current, err := st.GetTask(ctx, triage.ID)
		requireOK(t, err)
		if current.State != core.TaskQueued || current.NextStage != core.StageTriage {
			t.Fatalf("attached triage task=%s/%s, want queued at triage", current.State, current.NextStage)
		}
		finishDispatch(t, triage.ID)

		reconcile(t, 3)
		requireRepairs(t, awaiting.ID, 0)
		requireRepairs(t, triage.ID, 1)
		requireDispatch(t, triage.ID, true)
		var refusal *store.PlanRiskRatingOpenError
		if err := st.ApproveSpecVersion(ctx, awaiting.ID, 1); !errors.As(err, &refusal) || refusal.WorkOrderID != awaitingOrder.Order.ID {
			t.Fatalf("approval with an open plan risk order: %v", err)
		}
		for _, id := range []string{awaitingOrder.Order.ID, triageOrder.Order.ID} {
			if got := orderState(t, id); got != core.WorkOrderQueued {
				t.Fatalf("plan risk order %s state=%s, want queued", id, got)
			}
		}
	})

	t.Run("ReconcileRacesTheRiskOrderEnd", func(t *testing.T) {
		task := fileTask(t, &enforce, core.TaskRunning, core.StageImplement)
		submitHead(t, task, "h1", true)
		order := headOrder(t, task.ID, "h1")
		session := "reconcile-race-" + task.ID
		claimRisk(t, order.Order.ID, session)
		const reconcilers = 6
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, reconcilers+1)
		for range reconcilers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := st.ReconcileQueuedTasks(reconcileCtx); err != nil {
					errs <- err
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
				return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: order.Order.ID,
					Claim: core.WorkOrderClaimIdentity{ClaimantID: "run:risk-agent", SessionID: session}, Tier: riskpolicy.TierLow, Reason: "rated low"})
			})
			if err != nil {
				errs <- err
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent reconcile and risk completion: %v", err)
		}
		// Whichever commits first, the task is dispatched once: by the wake
		// of the completion, never by a reconcile repair beside it.
		requireDispatch(t, task.ID, true)
		reconcile(t, 3)
		requireRepairs(t, task.ID, 0)
		if got := orderState(t, order.Order.ID); got != core.WorkOrderCompleted {
			t.Fatalf("risk order state=%s, want completed", got)
		}
	})
}
