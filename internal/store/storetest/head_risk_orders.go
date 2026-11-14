package storetest

import (
	"errors"
	"fmt"
	"slices"
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
// close cancel the task's open risk orders (feature-risk-based-review RR-12,
// RR-15; req-risk-based-review AC-1.6, AC-5.8 to AC-5.11, AC-12.3;
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
}
