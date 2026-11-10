package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRiskEnforcementConformance proves on every backend that an enforcing
// task's tier sets its review seat floor with the source risk, that a tier
// raised by a seat's verdict or a person's raise adds seats to the open round
// in the same act and never to a settled round, that fail-fast settlement
// still settles and cancels the added seats, that concurrent raises and final
// verdicts serialize, and that acceptance takes the merge gate whenever one
// or more human approvals are required. Off and record tasks keep their seats
// and edges (req-risk-based-review REQ-7, AC-7.1, AC-7.2, AC-8.1, AC-8.2,
// AC-8.6, AC-1.6; req-review-gates-evidence AC-2.1, AC-2.3, AC-2.6, AC-2.7;
// feature-risk-based-review RR-5, RR-6).
func RunRiskEnforcementConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	enforce := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	type taskOptions struct {
		policy   *riskpolicy.Policy
		gate     bool
		failFast bool
	}
	fileTask := func(t *testing.T, opts taskOptions) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, MergeApproval: opts.gate, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: opts.policy, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}, FailFast: opts.failFast}}
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	rate := func(t *testing.T, taskID, head string, tier riskpolicy.Tier, approvals int) {
		t.Helper()
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule, Tier: tier,
			HeadSHA: head, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationAbsent, Reason: "path rule " + string(tier), HumanApprovals: approvals})
		requireOK(t, err)
	}
	resolve := func(t *testing.T, task core.Task, round int) store.ReviewRoundSeatResolution {
		t.Helper()
		in, err := store.LoadReviewRoundSeatInputs(ctx, st, task, store.ReviewSeatMinimum(task.SetupContract, 0), round)
		requireOK(t, err)
		return store.ResolveReviewRoundSeats(in)
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
	openRound := func(t *testing.T, task core.Task, head string, round int, seats store.ReviewRoundSeatResolution) error {
		t.Helper()
		jobs, orders := members(task.ID, head, round, seats.SeatCount)
		_, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (struct{}, error) {
			return struct{}{}, st.CreateReviewRoundCommand(ctx, lease, task.ID, seats, jobs, orders)
		})
		return err
	}
	verdict := func(task core.Task, head string, round, seat int, outcome, tier string) core.ReviewDecision {
		id := fmt.Sprintf("%s-review-%d-seat-%d", task.ID, round, seat)
		decision := core.ReviewDecision{TaskID: task.ID, JobID: id, ReviewWorkOrderID: id, ReviewRound: round, ReviewSeat: seat,
			Verdict: outcome, ReasonCode: "verified", Summary: "risk seat", MaxBounces: 3, ReviewedCommitSHA: head, HeadSHA: head,
			ReviewerModel: fmt.Sprintf("model-%d", seat), PolicyVersion: 1, MergeApproval: task.MergeApproval,
			RiskTier: tier, RiskReason: "seat states " + tier, WorkspaceReviewSeats: 1, SeatQueueTimeout: time.Hour}
		if outcome == "changes_requested" {
			decision.ReasonCode, decision.Feedback = "fix", "fix the change"
		}
		return decision
	}
	roundOrders := func(t *testing.T, taskID string, round int) []core.WorkOrder {
		t.Helper()
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		var out []core.WorkOrder
		for _, order := range orders {
			if order.Stage == core.StageReview && order.ReviewRound == round {
				out = append(out, order)
			}
		}
		return out
	}
	eventsOf := func(t *testing.T, taskID, kind string) []core.Event {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		var out []core.Event
		for _, event := range events {
			if event.Kind == kind {
				out = append(out, event)
			}
		}
		return out
	}
	record1 := func(t *testing.T, taskID string, round int) store.ReviewRoundSeats {
		t.Helper()
		records, err := st.ListReviewRoundSeats(ctx, taskID)
		requireOK(t, err)
		for _, record := range records {
			if record.ReviewRound == round {
				return record
			}
		}
		t.Fatalf("no seat record for round %d of %s: %+v", round, taskID, records)
		return store.ReviewRoundSeats{}
	}
	stateOf := func(t *testing.T, taskID string) core.Task {
		t.Helper()
		task, err := st.GetTask(ctx, taskID)
		requireOK(t, err)
		return task
	}
	accept := func(decision core.ReviewDecision) error {
		return taskops.New(st).AcceptReviewDecision(ctx, decision)
	}
	raise := func(taskID string, tier riskpolicy.Tier) error {
		_, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: taskID, Tier: tier, Reason: "operator raise to " + string(tier), ActorID: "user:operator"},
			store.RiskSeatSettings{WorkspaceReviewSeats: 1, QueueTimeout: time.Hour})
		return err
	}

	t.Run("TiersOpenTheTableSeats", func(t *testing.T) {
		custom := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.Table{
			Low: riskpolicy.Requirement{AgentReviews: 2}, Medium: riskpolicy.Requirement{AgentReviews: 3, HumanApprovals: 1}, High: riskpolicy.Requirement{AgentReviews: 4, HumanApprovals: 2}}}
		for _, tc := range []struct {
			name   string
			policy riskpolicy.Policy
			tier   riskpolicy.Tier
			seats  int
			source string
		}{
			{"DefaultLow", enforce, riskpolicy.TierLow, 1, store.ReviewSeatSourceMinimum},
			{"DefaultMedium", enforce, riskpolicy.TierMedium, 1, store.ReviewSeatSourceMinimum},
			{"DefaultHigh", enforce, riskpolicy.TierHigh, 2, store.ReviewSeatSourceRisk},
			{"CustomLow", custom, riskpolicy.TierLow, 2, store.ReviewSeatSourceRisk},
			{"CustomMedium", custom, riskpolicy.TierMedium, 3, store.ReviewSeatSourceRisk},
			{"CustomHigh", custom, riskpolicy.TierHigh, 4, store.ReviewSeatSourceRisk},
			{"RecordHighKeepsMinimum", record, riskpolicy.TierHigh, 1, store.ReviewSeatSourceMinimum},
		} {
			t.Run(tc.name, func(t *testing.T) {
				policy := tc.policy
				task := fileTask(t, taskOptions{policy: &policy})
				rate(t, task.ID, "head-t", tc.tier, 0)
				seats := resolve(t, task, 1)
				if seats.SeatCount != tc.seats || seats.Source != tc.source || seats.FloorSeatCount != tc.seats {
					t.Fatalf("resolution=%+v, want %d seats from %s", seats, tc.seats, tc.source)
				}
				if tc.seats > 1 {
					// A round below the tier's floor is refused and opens nothing
					// (AC-2.6).
					below := store.ReviewRoundSeatResolution{SeatCount: 1, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1, FloorSeatCount: 1}
					if err := openRound(t, task, "head-t", 1, below); !errors.Is(err, store.ErrReviewRoundSeatsInvalid) {
						t.Fatalf("below-floor round err=%v, want ErrReviewRoundSeatsInvalid", err)
					}
					if got := len(roundOrders(t, task.ID, 1)); got != 0 {
						t.Fatalf("refused round left %d orders", got)
					}
				}
				requireOK(t, openRound(t, task, "head-t", 1, seats))
				if got := len(roundOrders(t, task.ID, 1)); got != tc.seats {
					t.Fatalf("round opened %d seats, want %d", got, tc.seats)
				}
				if recorded := record1(t, task.ID, 1); recorded.SeatCount != tc.seats || recorded.SeatCountSource != tc.source {
					t.Fatalf("record=%+v", recorded)
				}
			})
		}
	})

	t.Run("VerdictRaisingTheTierAddsASeatBeforeSettling", func(t *testing.T) {
		task := fileTask(t, taskOptions{policy: &enforce})
		rate(t, task.ID, "head-v", riskpolicy.TierLow, 0)
		requireOK(t, openRound(t, task, "head-v", 1, resolve(t, task, 1)))
		requireOK(t, accept(verdict(task, "head-v", 1, 1, "approve", "high")))
		orders := roundOrders(t, task.ID, 1)
		if len(orders) != 2 || orders[1].ReviewSeat != 2 || orders[1].State != core.WorkOrderQueued || !orders[1].Claimable || orders[1].HeadSHA != "head-v" {
			t.Fatalf("round orders=%+v, want an added queued seat 2", orders)
		}
		if recorded := record1(t, task.ID, 1); recorded.SeatCount != 2 || recorded.SeatCountSource != store.ReviewSeatSourceRisk {
			t.Fatalf("record=%+v, want 2 seats from risk", recorded)
		}
		added := eventsOf(t, task.ID, "review.seats_added")
		if len(added) != 1 {
			t.Fatalf("review.seats_added events=%d, want 1", len(added))
		}
		var payload map[string]any
		requireOK(t, json.Unmarshal(added[0].Payload, &payload))
		if payload["source"] != "risk" || payload["risk_tier"] != "high" || payload["seat_count"] != float64(2) || payload["prior_seat_count"] != float64(1) {
			t.Fatalf("seats_added payload=%v", payload)
		}
		if len(eventsOf(t, task.ID, "review.round_completed")) != 0 || stateOf(t, task.ID).State != core.TaskRunning {
			t.Fatal("the round settled short of the added seat")
		}
		// A replayed verdict adds no second seat.
		requireOK(t, accept(verdict(task, "head-v", 1, 1, "approve", "high")))
		if got := len(roundOrders(t, task.ID, 1)); got != 2 {
			t.Fatalf("replayed verdict left %d seats, want 2", got)
		}
		requireOK(t, accept(verdict(task, "head-v", 1, 2, "approve", "high")))
		if completed := eventsOf(t, task.ID, "review.round_completed"); len(completed) != 1 {
			t.Fatalf("round_completed=%d, want 1 after both seats approved", len(completed))
		}
		// High requires two human approvals, so the gate-off task waits at
		// the merge gate (AC-8.2).
		if got := stateOf(t, task.ID); got.State != core.TaskAwaiting || got.GateKind != core.GateKindMergeApproval {
			t.Fatalf("task state=%s gate=%s, want awaiting merge approval", got.State, got.GateKind)
		}
	})

	t.Run("FailFastChangesRequestedStillSettles", func(t *testing.T) {
		task := fileTask(t, taskOptions{policy: &enforce, failFast: true})
		rate(t, task.ID, "head-f", riskpolicy.TierMedium, 0)
		requireOK(t, openRound(t, task, "head-f", 1, resolve(t, task, 1)))
		requireOK(t, accept(verdict(task, "head-f", 1, 1, "changes_requested", "high")))
		orders := roundOrders(t, task.ID, 1)
		if len(orders) != 2 || orders[1].State != core.WorkOrderCancelled {
			t.Fatalf("round orders=%+v, want an added seat cancelled by the settlement", orders)
		}
		completed := eventsOf(t, task.ID, "review.round_completed")
		if len(completed) != 1 {
			t.Fatalf("round_completed=%d, want 1", len(completed))
		}
		var aggregate struct {
			Verdict        string `json:"verdict"`
			CancelledSeats []int  `json:"cancelled_seats"`
		}
		requireOK(t, json.Unmarshal(completed[0].Payload, &aggregate))
		if aggregate.Verdict != "changes_requested" || len(aggregate.CancelledSeats) != 1 || aggregate.CancelledSeats[0] != 2 {
			t.Fatalf("aggregate=%+v", aggregate)
		}
	})

	t.Run("SettledRoundTakesNoSeats", func(t *testing.T) {
		task := fileTask(t, taskOptions{policy: &enforce})
		rate(t, task.ID, "head-s", riskpolicy.TierLow, 0)
		requireOK(t, openRound(t, task, "head-s", 1, resolve(t, task, 1)))
		requireOK(t, accept(verdict(task, "head-s", 1, 1, "approve", "low")))
		if stateOf(t, task.ID).State != core.TaskApproved {
			t.Fatal("a gate-off enforcing low task did not take review.accept")
		}
		// A raise after settlement records the tier and adds no seat
		// (AC-2.6, AC-7.2).
		requireOK(t, raise(task.ID, riskpolicy.TierHigh))
		if got := len(roundOrders(t, task.ID, 1)); got != 1 || len(eventsOf(t, task.ID, "review.seats_added")) != 0 {
			t.Fatalf("settled round has %d seats after a raise, want 1", got)
		}
	})

	t.Run("PersonRaiseAddsOnlyMissingSeats", func(t *testing.T) {
		task := fileTask(t, taskOptions{policy: &enforce})
		rate(t, task.ID, "head-p", riskpolicy.TierLow, 0)
		requireOK(t, openRound(t, task, "head-p", 1, resolve(t, task, 1)))
		requireOK(t, raise(task.ID, riskpolicy.TierMedium))
		if got := len(roundOrders(t, task.ID, 1)); got != 1 {
			t.Fatalf("medium raise left %d seats, want 1", got)
		}
		requireOK(t, raise(task.ID, riskpolicy.TierHigh))
		orders := roundOrders(t, task.ID, 1)
		if len(orders) != 2 || len(eventsOf(t, task.ID, "review.seats_added")) != 1 {
			t.Fatalf("high raise orders=%+v, want one added seat and one event", orders)
		}
		// A refused raise adds nothing.
		var notRaised *core.RiskTierNotRaisedError
		if err := raise(task.ID, riskpolicy.TierHigh); !errors.As(err, &notRaised) {
			t.Fatalf("repeated raise err=%v, want RiskTierNotRaisedError", err)
		}
		if got := len(roundOrders(t, task.ID, 1)); got != 2 {
			t.Fatalf("refused raise left %d seats", got)
		}
		requireOK(t, accept(verdict(task, "head-p", 1, 1, "approve", "low")))
		if len(eventsOf(t, task.ID, "review.round_completed")) != 0 {
			t.Fatal("the round settled before the added seat answered")
		}
		requireOK(t, accept(verdict(task, "head-p", 1, 2, "approve", "low")))
		if len(eventsOf(t, task.ID, "review.round_completed")) != 1 {
			t.Fatal("the round did not settle after every seat approved")
		}
	})

	t.Run("AcceptanceHoldsAtTheMergeGate", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			policy    *riskpolicy.Policy
			tier      riskpolicy.Tier
			approvals int
			gate      bool
			kind      string
			state     core.TaskState
		}{
			{"EnforceLowGateOffMerges", &enforce, riskpolicy.TierLow, 0, false, "", core.TaskApproved},
			{"EnforceMediumGateOffHolds", &enforce, riskpolicy.TierMedium, 0, false, "", core.TaskAwaiting},
			{"EnforcePathMinimumHolds", &enforce, riskpolicy.TierLow, 1, false, "", core.TaskAwaiting},
			{"EnforceRefreshLowStaysApproved", &enforce, riskpolicy.TierLow, 0, false, "refresh", core.TaskApproved},
			{"EnforceRefreshMediumReturnsToGate", &enforce, riskpolicy.TierMedium, 0, false, "refresh", core.TaskAwaiting},
			{"EnforceRefreshGateOnReturnsToGate", &enforce, riskpolicy.TierLow, 0, true, "refresh", core.TaskAwaiting},
			{"RecordHighGateOffMerges", &record, riskpolicy.TierHigh, 2, false, "", core.TaskApproved},
			{"RecordRefreshGateOnStaysApproved", &record, riskpolicy.TierHigh, 0, true, "refresh", core.TaskApproved},
			{"OffGateOffMerges", nil, "", 0, false, "", core.TaskApproved},
		} {
			t.Run(tc.name, func(t *testing.T) {
				task := fileTask(t, taskOptions{policy: tc.policy, gate: tc.gate})
				if tc.policy != nil {
					rate(t, task.ID, "head-a", tc.tier, tc.approvals)
				}
				requireOK(t, openRound(t, task, "head-a", 1, store.ReviewRoundSeatResolution{SeatCount: 1, Source: store.ReviewSeatSourceMinimum, MinimumSeatCount: 1, FloorSeatCount: 1}))
				decision := verdict(task, "head-a", 1, 1, "approve", string(tc.tier))
				decision.ReviewKind = tc.kind
				if tc.policy == nil {
					decision.RiskTier, decision.RiskReason = "", ""
				}
				requireOK(t, accept(decision))
				got := stateOf(t, task.ID)
				if got.State != tc.state {
					t.Fatalf("task state=%s, want %s", got.State, tc.state)
				}
				if tc.state == core.TaskAwaiting && got.GateKind != core.GateKindMergeApproval {
					t.Fatalf("gate kind=%s, want merge approval", got.GateKind)
				}
			})
		}
	})

	t.Run("ConcurrentRaiseAndFinalVerdictSerialize", func(t *testing.T) {
		for i := 0; i < 4; i++ {
			task := fileTask(t, taskOptions{policy: &enforce})
			rate(t, task.ID, "head-c", riskpolicy.TierMedium, 0)
			requireOK(t, openRound(t, task, "head-c", 1, resolve(t, task, 1)))
			var wg sync.WaitGroup
			errs := make([]error, 2)
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; errs[0] = raise(task.ID, riskpolicy.TierHigh) }()
			go func() { defer wg.Done(); <-start; errs[1] = accept(verdict(task, "head-c", 1, 1, "approve", "medium")) }()
			close(start)
			wg.Wait()
			for _, err := range errs {
				if err != nil && !errors.Is(err, store.ErrRetryable) {
					t.Fatalf("iteration %d: concurrent act err=%v", i, err)
				}
			}
			for j, err := range errs {
				if errors.Is(err, store.ErrRetryable) {
					if j == 0 {
						requireOK(t, raise(task.ID, riskpolicy.TierHigh))
					} else {
						requireOK(t, accept(verdict(task, "head-c", 1, 1, "approve", "medium")))
					}
				}
			}
			seats := len(roundOrders(t, task.ID, 1))
			settled := len(eventsOf(t, task.ID, "review.round_completed"))
			switch {
			case seats == 1 && settled == 1:
				// The verdict settled the round first; the raise added nothing.
			case seats == 2 && settled == 0:
				// The raise added seat 2 first; the round waits for it.
			default:
				t.Fatalf("iteration %d: %d seats and %d settlements, want the raise and the verdict serialized", i, seats, settled)
			}
			if added := len(eventsOf(t, task.ID, "review.seats_added")); added != seats-1 {
				t.Fatalf("iteration %d: seats_added=%d with %d seats", i, added, seats)
			}
		}
	})

	t.Run("ConcurrentVerdictsAddSeatsOnce", func(t *testing.T) {
		wide := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.Table{
			Low: riskpolicy.Requirement{AgentReviews: 1}, Medium: riskpolicy.Requirement{AgentReviews: 1, HumanApprovals: 1}, High: riskpolicy.Requirement{AgentReviews: 3, HumanApprovals: 2}}}
		task := fileTask(t, taskOptions{policy: &wide})
		rate(t, task.ID, "head-d", riskpolicy.TierLow, 0)
		carried := store.ReviewRoundSeatResolution{SeatCount: 2, Source: store.ReviewSeatSourceCarried, MinimumSeatCount: 1, FloorSeatCount: 1}
		requireOK(t, openRound(t, task, "head-d", 1, carried))
		// Both seats state high at once; high requires 3 agent reviews, so
		// exactly one seat is added however the verdicts interleave.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = accept(verdict(task, "head-d", 1, i+1, "approve", "high"))
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if errors.Is(err, store.ErrRetryable) {
				err = accept(verdict(task, "head-d", 1, i+1, "approve", "high"))
			}
			requireOK(t, err)
		}
		if got := len(roundOrders(t, task.ID, 1)); got != 3 || len(eventsOf(t, task.ID, "review.seats_added")) != 1 {
			t.Fatalf("round has %d seats and %d additions, want 3 seats from one addition", got, len(eventsOf(t, task.ID, "review.seats_added")))
		}
		if len(eventsOf(t, task.ID, "review.round_completed")) != 0 {
			t.Fatal("the round settled before the added seat answered")
		}
		requireOK(t, accept(verdict(task, "head-d", 1, 3, "approve", "high")))
		if len(eventsOf(t, task.ID, "review.round_completed")) != 1 {
			t.Fatal("the round did not settle after the added seat approved")
		}
	})

	t.Run("RecordModeAddsNoSeats", func(t *testing.T) {
		task := fileTask(t, taskOptions{policy: &record})
		rate(t, task.ID, "head-r", riskpolicy.TierLow, 0)
		requireOK(t, openRound(t, task, "head-r", 1, resolve(t, task, 1)))
		requireOK(t, accept(verdict(task, "head-r", 1, 1, "approve", "high")))
		if got := len(roundOrders(t, task.ID, 1)); got != 1 || len(eventsOf(t, task.ID, "review.round_completed")) != 1 {
			t.Fatalf("record round has %d seats, want 1 and settled", got)
		}
		if stateOf(t, task.ID).State != core.TaskApproved {
			t.Fatal("a gate-off record task did not take review.accept")
		}
	})
}
