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

// RunRiskRatingConformance proves on every backend that risk ratings are
// workspace-scoped, unique per task and key, append-only with one activity
// event each, recorded atomically with a review verdict, raised only
// strictly upward under concurrency, and refused for a task whose frozen
// risk mode is off (req-risk-based-review AC-1.1, AC-5.2, AC-5.3, AC-5.5,
// AC-5.6, REQ-6, AC-6.1, AC-6.2; component-risk-review; feature-risk-based-review
// RR-3, RR-8).
func RunRiskRatingConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	record := riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}
	fileTask := func(t *testing.T, policy *riskpolicy.Policy) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: policy}
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	pathRating := func(taskID, head string, tier riskpolicy.Tier, reason string) core.RiskRating {
		return core.RiskRating{TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule, Tier: tier, HeadSHA: head,
			BaseSHA: "base-" + head, DeclarationState: riskpolicy.DeclarationAbsent, Reason: reason}
	}
	riskEvents := func(t *testing.T, taskID string) []core.Event {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		var rated []core.Event
		for _, event := range events {
			if event.Kind == core.RiskRatedEvent {
				rated = append(rated, event)
			}
		}
		return rated
	}
	ratings := func(t *testing.T, taskID string) []core.RiskRating {
		t.Helper()
		listed, err := st.ListTaskRiskRatings(ctx, taskID)
		requireOK(t, err)
		return listed
	}

	t.Run("PathRatingOncePerHeadWithEvent", func(t *testing.T) {
		task := fileTask(t, &record)
		first, created, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-1", riskpolicy.TierMedium, "matched path rules: GitHub Actions workflows (medium)"))
		requireOK(t, err)
		if !created || first.Seq != 1 || first.Tier != riskpolicy.TierMedium || first.CreatedAt.IsZero() {
			t.Fatalf("first path rating=%+v created=%t", first, created)
		}
		replay, created, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-1", riskpolicy.TierLow, "a different second read"))
		requireOK(t, err)
		if created || replay.Seq != first.Seq || replay.Tier != riskpolicy.TierMedium || replay.Reason != first.Reason {
			t.Fatalf("replayed head recorded %+v created=%t, want the first rating reused", replay, created)
		}
		events := riskEvents(t, task.ID)
		if len(events) != 1 {
			t.Fatalf("risk events=%d, want 1 for one head", len(events))
		}
		var payload map[string]any
		requireOK(t, json.Unmarshal(events[0].Payload, &payload))
		for key, want := range map[string]any{"tier": "medium", "source": "path_rule", "head_sha": "head-1", "reason": first.Reason, "base_sha": "base-head-1"} {
			if payload[key] != want {
				t.Fatalf("event payload %s=%v, want %v (payload %v)", key, payload[key], want, payload)
			}
		}
		if _, ok := payload["matched_rules"].([]any); !ok {
			t.Fatalf("event payload matched_rules=%v, want a list", payload["matched_rules"])
		}
		second, created, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-2", riskpolicy.TierLow, "no sensitive path rule matched the changed paths"))
		requireOK(t, err)
		if !created || second.Seq != 2 {
			t.Fatalf("second head rating=%+v created=%t", second, created)
		}
		listed := ratings(t, task.ID)
		if len(listed) != 2 || listed[0].Key != core.PathRiskKey("head-1") || listed[1].Key != core.PathRiskKey("head-2") {
			t.Fatalf("listed ratings=%+v", listed)
		}
		// A less sensitive later head never lowers the task's tier (AC-5.5).
		if setter, ok := core.RiskTierOf(listed); !ok || setter.Tier != riskpolicy.TierMedium || setter.HeadSHA != "head-1" {
			t.Fatalf("task tier setter=%+v ok=%t, want medium from head-1", setter, ok)
		}
	})

	t.Run("ScopedToWorkspace", func(t *testing.T) {
		task := fileTask(t, &record)
		_, _, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-scope", riskpolicy.TierLow, "no sensitive path rule matched the changed paths"))
		requireOK(t, err)
		foreign := store.WithWorkspace(ctx, x.Workspace+"-risk-foreign")
		if listed, err := st.ListTaskRiskRatings(foreign, task.ID); !errors.Is(err, store.ErrNotFound) || len(listed) != 0 {
			t.Fatalf("foreign workspace read %d ratings, err=%v; want not found", len(listed), err)
		}
		if _, _, err := st.RecordRiskRating(foreign, pathRating(task.ID, "head-foreign", riskpolicy.TierHigh, "foreign write")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign workspace path rating err=%v, want not found", err)
		}
		if _, err := st.RaiseTaskRiskTier(foreign, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "foreign raise", ActorID: "user:foreign"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign workspace raise err=%v, want not found", err)
		}
		if got := len(ratings(t, task.ID)); got != 1 {
			t.Fatalf("foreign writes changed the task's ratings: %d", got)
		}
	})

	t.Run("OffTaskRecordsNothing", func(t *testing.T) {
		task := fileTask(t, nil)
		if _, _, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-off", riskpolicy.TierHigh, "off")); !errors.Is(err, store.ErrRiskNotEnabled) {
			t.Fatalf("off task path rating err=%v, want ErrRiskNotEnabled", err)
		}
		if _, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "off", ActorID: "user:operator"}); !errors.Is(err, store.ErrRiskNotEnabled) {
			t.Fatalf("off task raise err=%v, want ErrRiskNotEnabled", err)
		}
		if len(ratings(t, task.ID)) != 0 || len(riskEvents(t, task.ID)) != 0 {
			t.Fatal("an off task recorded a rating")
		}
	})

	t.Run("RecordRiskRatingRefusesReviewerAndPersonSources", func(t *testing.T) {
		task := fileTask(t, &record)
		for _, rating := range []core.RiskRating{
			{TaskID: task.ID, Key: core.ReviewerRiskKey("order"), Source: core.RiskSourceReviewer, Tier: riskpolicy.TierLow, Reason: "self", ReviewWorkOrderID: "order"},
			{TaskID: task.ID, Key: core.PersonRiskKey(riskpolicy.TierHigh), Source: core.RiskSourcePerson, Tier: riskpolicy.TierHigh, Reason: "self", ActorID: "user:x"},
			{TaskID: task.ID, Key: "path_rule:other", Source: core.RiskSourcePathRule, Tier: riskpolicy.TierLow, HeadSHA: "head", Reason: "mismatched key"},
			{TaskID: task.ID, Key: core.PathRiskKey("head"), Source: core.RiskSourcePathRule, Tier: "critical", HeadSHA: "head", Reason: "unknown tier"},
		} {
			if _, _, err := st.RecordRiskRating(ctx, rating); err == nil {
				t.Fatalf("RecordRiskRating accepted %+v", rating)
			}
		}
		if len(ratings(t, task.ID)) != 0 {
			t.Fatal("a refused rating was recorded")
		}
	})

	t.Run("ReviewerRatingsInVerdictTransaction", func(t *testing.T) {
		task := fileTask(t, &record)
		_, _, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-r", riskpolicy.TierMedium, "matched path rules: GitHub Actions workflows (medium)"))
		requireOK(t, err)
		jobs, orders := riskReviewRound(task.ID, 3)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		decision := func(i int, tier, reason string) core.ReviewDecision {
			return core.ReviewDecision{TaskID: task.ID, JobID: orders[i].JobID, ReviewWorkOrderID: orders[i].ID, ReviewRound: 1, ReviewSeat: i + 1,
				Verdict: "approve", ReasonCode: "verified", Summary: "risk seat", MaxBounces: 3, ReviewedCommitSHA: "head-r", HeadSHA: "head-r",
				ReviewerModel: fmt.Sprintf("model-%d", i+1), PolicyVersion: 1, RiskTier: tier, RiskReason: reason}
		}
		eventsBefore, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		// A malformed stated tier refuses the whole verdict: no completion, no
		// rating, no round progress.
		if err := taskops.New(st).AcceptReviewDecision(ctx, decision(0, "critical", "not a tier")); err == nil {
			t.Fatal("a verdict with tier critical was accepted")
		}
		if err := taskops.New(st).AcceptReviewDecision(ctx, decision(0, "low", "two\nlines")); err == nil {
			t.Fatal("a verdict with a multi-line reason was accepted")
		}
		eventsAfter, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		if len(eventsAfter) != len(eventsBefore) || len(ratings(t, task.ID)) != 1 {
			t.Fatalf("refused verdicts wrote %d events and left %d ratings", len(eventsAfter)-len(eventsBefore), len(ratings(t, task.ID)))
		}
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(0, "low", "small change in one handler")))
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(1, "low", "tests cover the change")))
		// Under record a verdict without a tier is accepted and records no
		// rating (AC-5.2).
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(2, "", "")))
		listed := ratings(t, task.ID)
		if len(listed) != 3 {
			t.Fatalf("ratings=%+v, want the path rating and two seat ratings", listed)
		}
		for i, rating := range listed[1:] {
			if rating.Source != core.RiskSourceReviewer || rating.ReviewSeat != i+1 || rating.ReviewRound != 1 || rating.ReviewWorkOrderID != orders[i].ID ||
				rating.ReviewerModel != fmt.Sprintf("model-%d", i+1) || rating.HeadSHA != "head-r" || rating.Tier != riskpolicy.TierLow {
				t.Fatalf("seat rating %d=%+v", i+1, rating)
			}
		}
		// Every seat said low; the sensitive path keeps the head at medium.
		if setter, ok := core.RiskTierOf(listed); !ok || setter.Tier != riskpolicy.TierMedium || setter.Source != core.RiskSourcePathRule {
			t.Fatalf("task tier setter=%+v, want medium from the path rule", setter)
		}
		events := riskEvents(t, task.ID)
		if len(events) != 3 {
			t.Fatalf("risk events=%d, want 3", len(events))
		}
		var seat map[string]any
		requireOK(t, json.Unmarshal(events[1].Payload, &seat))
		if seat["source"] != "reviewer" || seat["review_seat"] != float64(1) || seat["reviewer_model"] != "model-1" || seat["head_sha"] != "head-r" {
			t.Fatalf("seat event payload=%v", seat)
		}
		// A replayed verdict records no second rating.
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision(0, "low", "small change in one handler")))
		if got := len(ratings(t, task.ID)); got != 3 {
			t.Fatalf("replayed verdict left %d ratings, want 3", got)
		}
	})

	t.Run("SeatsLowAndMediumGiveMedium", func(t *testing.T) {
		task := fileTask(t, &record)
		_, _, err := st.RecordRiskRating(ctx, pathRating(task.ID, "head-m", riskpolicy.TierLow, "no sensitive path rule matched the changed paths"))
		requireOK(t, err)
		jobs, orders := riskReviewRound(task.ID, 2)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		for i, tier := range []string{"low", "medium"} {
			requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: orders[i].JobID, ReviewWorkOrderID: orders[i].ID,
				ReviewRound: 1, ReviewSeat: i + 1, Verdict: "approve", ReasonCode: "verified", Summary: "seat", MaxBounces: 3, ReviewedCommitSHA: "head-m", HeadSHA: "head-m",
				ReviewerModel: "model", PolicyVersion: 1, RiskTier: tier, RiskReason: "seat " + tier}))
		}
		setter, ok := core.RiskTierOf(ratings(t, task.ID))
		if !ok || setter.Tier != riskpolicy.TierMedium || setter.Source != core.RiskSourceReviewer || setter.ReviewSeat != 2 {
			t.Fatalf("task tier setter=%+v, want medium from seat 2", setter)
		}
	})

	t.Run("OffTaskVerdictWithTierIsRefused", func(t *testing.T) {
		task := fileTask(t, nil)
		jobs, orders := riskReviewRound(task.ID, 1)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		err := taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: orders[0].JobID, ReviewWorkOrderID: orders[0].ID,
			ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "verified", Summary: "seat", MaxBounces: 3, ReviewedCommitSHA: "head-off",
			PolicyVersion: 1, RiskTier: "high", RiskReason: "off task"})
		if !errors.Is(err, store.ErrRiskNotEnabled) {
			t.Fatalf("off task verdict with a tier err=%v, want ErrRiskNotEnabled", err)
		}
		if len(ratings(t, task.ID)) != 0 {
			t.Fatal("an off task recorded a seat rating")
		}
	})

	t.Run("RaiseIsStrictlyUpward", func(t *testing.T) {
		task := fileTask(t, &record)
		before, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		var notRaised *core.RiskTierNotRaisedError
		// An unrated task's tier is low, so low is refused.
		if _, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierLow, Reason: "no change", ActorID: "user:operator"}); !errors.As(err, &notRaised) || notRaised.Current != riskpolicy.TierLow {
			t.Fatalf("raise to low err=%v, want not raised naming low", err)
		}
		for _, invalid := range []core.RiskRating{
			{TaskID: task.ID, Tier: "critical", Reason: "unknown tier", ActorID: "user:operator"},
			{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: " ", ActorID: "user:operator"},
			{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "two\nlines", ActorID: "user:operator"},
			{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "no person"},
		} {
			if _, err := st.RaiseTaskRiskTier(ctx, invalid); err == nil {
				t.Fatalf("raise %+v was accepted", invalid)
			}
		}
		raised, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierMedium, Reason: "touches billing", ActorID: "user:operator", HeadSHA: "head-p"})
		requireOK(t, err)
		if raised.Source != core.RiskSourcePerson || raised.PreviousTier != riskpolicy.TierLow || raised.Tier != riskpolicy.TierMedium || raised.ActorID != "user:operator" {
			t.Fatalf("raise=%+v", raised)
		}
		for _, tier := range []riskpolicy.Tier{riskpolicy.TierLow, riskpolicy.TierMedium} {
			if _, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: tier, Reason: "lower or equal", ActorID: "user:operator"}); !errors.As(err, &notRaised) || notRaised.Current != riskpolicy.TierMedium {
				t.Fatalf("raise to %s err=%v, want not raised naming medium", tier, err)
			}
		}
		events := riskEvents(t, task.ID)
		if len(events) != 1 {
			t.Fatalf("risk events=%d, want 1", len(events))
		}
		var payload map[string]any
		requireOK(t, json.Unmarshal(events[0].Payload, &payload))
		if payload["source"] != "person" || payload["actor_id"] != "user:operator" || payload["previous_tier"] != "low" || payload["new_tier"] != "medium" || payload["reason"] != "touches billing" {
			t.Fatalf("raise event payload=%v", payload)
		}
		// Concurrent raises to high: exactly one is recorded.
		var wg sync.WaitGroup
		results := make(chan error, 6)
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: fmt.Sprintf("raise %d", i), ActorID: "user:operator"})
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		accepted := 0
		for err := range results {
			switch {
			case err == nil:
				accepted++
			case errors.As(err, &notRaised) && notRaised.Current == riskpolicy.TierHigh:
			default:
				t.Fatalf("concurrent raise err=%v", err)
			}
		}
		if accepted != 1 {
			t.Fatalf("concurrent raises accepted %d, want 1", accepted)
		}
		listed := ratings(t, task.ID)
		if len(listed) != 2 || listed[1].PreviousTier != riskpolicy.TierMedium || listed[1].Tier != riskpolicy.TierHigh {
			t.Fatalf("ratings after raises=%+v", listed)
		}
		// A raise changes no task state, stage, plan state, or policy
		// (AC-1.6, AC-2.2, AC-2.3).
		after, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if after.State != before.State || after.NextStage != before.NextStage || after.PlanState != before.PlanState || after.GateKind != before.GateKind ||
			after.MergeApproval != before.MergeApproval || after.SetupContract.Risk == nil || *after.SetupContract.Risk != record {
			t.Fatalf("raise changed the task: before %+v after %+v", before, after)
		}
	})

	// Concurrent raises serialize on every backend. Each round starts a fresh
	// task and releases raises to medium and to high at once. Every raise is
	// recorded or refused as not raised, never a backend error; the recorded
	// raises climb strictly, each naming the tier before it, with one event
	// each and sequences 1..n (req-risk-based-review AC-6.1, AC-6.2;
	// component-risk-review invariant 4).
	t.Run("PlanRatings", func(t *testing.T) { runPlanRiskRatings(t, x) })

	t.Run("ConcurrentRaisesSerialize", func(t *testing.T) {
		const rounds, writers = 20, 8
		for round := range rounds {
			task := fileTask(t, &record)
			start := make(chan struct{})
			results := make(chan error, writers)
			var wg sync.WaitGroup
			for i := range writers {
				tier := riskpolicy.TierMedium
				if i%2 == 1 {
					tier = riskpolicy.TierHigh
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: tier, Reason: fmt.Sprintf("round %d writer %d", round, i), ActorID: "user:operator"})
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				var notRaised *core.RiskTierNotRaisedError
				switch {
				case err == nil:
					accepted++
				case errors.As(err, &notRaised):
				default:
					t.Fatalf("round %d: concurrent raise err=%v", round, err)
				}
			}
			listed := ratings(t, task.ID)
			if accepted == 0 || len(listed) != accepted || listed[len(listed)-1].Tier != riskpolicy.TierHigh {
				t.Fatalf("round %d: accepted %d, ratings %+v", round, accepted, listed)
			}
			previous := riskpolicy.TierLow
			for i, rating := range listed {
				if rating.Seq != i+1 || rating.PreviousTier != previous || rating.Tier.Rank() <= previous.Rank() {
					t.Fatalf("round %d: rating %d=%+v after tier %s", round, i, rating, previous)
				}
				previous = rating.Tier
			}
			if events := riskEvents(t, task.ID); len(events) != len(listed) {
				t.Fatalf("round %d: %d events for %d ratings", round, len(events), len(listed))
			}
		}
	})
}

// riskReviewRound builds review round 1 with the given number of seats.
func riskReviewRound(taskID string, seats int) ([]core.Job, []core.WorkOrder) {
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= seats; seat++ {
		id := core.NewTaskID()
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	return jobs, orders
}
