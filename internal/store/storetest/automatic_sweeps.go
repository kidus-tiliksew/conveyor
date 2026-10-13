package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// automaticSweepHead is the verified PR head of every sweep fixture task.
const automaticSweepHead = "fixture-head"

// ReviewSweepFunc runs workorder.Service.SweepAutomaticReviewRecovery over st
// with cfg as the workspace configuration and head as every task's verified
// PR head. storetest cannot import workorder: workorder tests import storetest.
type ReviewSweepFunc func(ctx context.Context, st store.Store, cfg *config.Config, head string, now time.Time) (int, error)

// automaticReviewSweepConfig serves the fixture repository, two review seats,
// and the given recovery policy.
func automaticReviewSweepConfig(workspace string, policy config.RecoveryPolicy) *config.Config {
	return &config.Config{
		Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Recovery: policy,
		Repos:   []config.Repo{{Name: "conveyor", Base: "main", GitHub: "acme/conveyor"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Harness: "codex", TimeoutText: "45m"}}},
		Harnesses: []config.Harness{
			{Name: "codex", Command: []string{"codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-current", Harness: "codex"}, {Model: "gpt-second", Harness: "codex"}}},
	}
}

func requireReviewSweep(t *testing.T, sweep ReviewSweepFunc) {
	t.Helper()
	if sweep == nil {
		t.Fatal("factory must wire workorder.Service.SweepAutomaticReviewRecovery")
	}
}

// seedSweepableReviewRound is a running task whose round 1 has a verdict on
// seat 1 and an AR-3-exhausted seat 2, with its PR head recorded: one
// automatic round retry is due.
func seedSweepableReviewRound(t *testing.T, st store.Store, ctx context.Context, workspace string) (core.Task, []core.WorkOrder) {
	t.Helper()
	task, orders := seedReviewRoundWithVerdict(t, st, ctx, workspace)
	exhaustReviewSeat(t, st, ctx, orders[1].ID)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "head_sha": automaticSweepHead})}))
	return task, orders
}

// runConcurrentReviewSweeps proves D14 for the review rule on every backend:
// concurrent production sweeps over one due round create exactly one new
// round, each of its seats once, and one automatic review.round_retried
// event. The fixture task has no frozen policy, so the new round has every
// configured workspace seat.
func runConcurrentReviewSweeps(t *testing.T, st store.Store, ctx context.Context, workspace string, sweep ReviewSweepFunc) {
	requireReviewSweep(t, sweep)
	task, _ := seedSweepableReviewRound(t, st, ctx, workspace)
	cfg := automaticReviewSweepConfig(workspace, config.RecoveryPolicy{AutomaticRecoveries: 1, Window: config.DefaultRecoveryWindow, WindowText: config.DefaultRecoveryWindowText})
	now := time.Now().UTC().Add(time.Hour)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := sweep(ctx, st, cfg, automaticSweepHead, now); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Logf("concurrent review sweep error: %v", err)
	}
	if retried := automaticRoundEvents(t, st, ctx, task.ID, "review.round_retried"); len(retried) != 1 {
		t.Fatalf("automatic round_retried events=%+v", retried)
	}
	seats := reviewSeatsByRound(t, st, ctx, task.ID)
	if len(seats) != 2 || len(seats[1]) != 2 || len(seats[2]) != len(cfg.Review.Seats) {
		t.Fatalf("review seats per round=%v, want round 1 and one new round 2 of %d seats", seats, len(cfg.Review.Seats))
	}
	for seat, n := range seats[2] {
		if n != 1 {
			t.Fatalf("new round seat %d created %d times: %v", seat, n, seats)
		}
	}
}

// reviewSeatsByRound counts the task's review work orders per round and seat.
func reviewSeatsByRound(t *testing.T, st store.Store, ctx context.Context, taskID string) map[int]map[int]int {
	t.Helper()
	orders, err := st.ListTaskWorkOrders(ctx, taskID)
	requireOK(t, err)
	seats := map[int]map[int]int{}
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		if seats[order.ReviewRound] == nil {
			seats[order.ReviewRound] = map[int]int{}
		}
		seats[order.ReviewRound][order.ReviewSeat]++
	}
	return seats
}

// runNothingDueSweeps proves D15 on every backend: in a workspace where no
// publication and no review round is due, both production sweeps act on
// nothing, append no event, and leave every updated_at unchanged.
func runNothingDueSweeps(t *testing.T, st store.Backend, ctx context.Context, sweep ReviewSweepFunc) {
	requireReviewSweep(t, sweep)
	control, ok := st.(store.WorkspaceControlStore)
	if !ok {
		t.Fatal("store does not create workspaces")
	}
	workspace := "nothing-due-" + core.NewTaskID()
	repos := []config.Repo{{Name: "conveyor", Base: "main", GitHub: "acme/conveyor"}, {Name: "app", Base: "main", GitHub: "acme/app"}}
	_, err := control.CreateWorkspace(ctx, workspace, workspace, &config.Config{Workspace: workspace, Repos: repos})
	requireOK(t, err)
	ctx = store.WithWorkspace(ctx, workspace)
	setAutomaticRecoveryBound(t, st, ctx, 1)
	policy := config.RecoveryPolicy{AutomaticRecoveries: 1, Window: config.DefaultRecoveryWindow, WindowText: config.DefaultRecoveryWindowText}

	// A failed publication pair whose one automatic re-enqueue this window is
	// spent: it failed again after the re-enqueue.
	publicationTask, reviewID := seedFailedPublications(t, st, ctx, workspace, core.TaskRunning)
	now := time.Now().UTC().Truncate(time.Second)
	if n, err := store.ReconcileFailedPublications(ctx, st, now); err != nil || n != 2 {
		t.Fatalf("seeding sweep acted=%d err=%v", n, err)
	}
	completePublicationJobs(t, st, ctx, workspace, publicationTask, reviewID)
	failPublications(t, st, ctx, publicationTask, reviewID)
	// A review round with a live seat: nothing to recover.
	reviewTask, _ := seedReviewRoundWithVerdict(t, st, ctx, workspace)

	type snapshot struct {
		events      map[string]int
		updated     map[string]time.Time
		publication time.Time
		lifecycle   time.Time
	}
	take := func() snapshot {
		s := snapshot{events: map[string]int{}, updated: map[string]time.Time{}}
		for _, taskID := range []string{publicationTask, reviewTask.ID} {
			s.events[taskID] = len(listEventsOK(t, st, ctx, taskID))
			orders, err := st.ListTaskWorkOrders(ctx, taskID)
			requireOK(t, err)
			for _, order := range orders {
				s.updated["order/"+order.ID] = order.UpdatedAt
			}
		}
		lifecycle, _, err := st.GetGitHubLifecycle(ctx, publicationTask)
		requireOK(t, err)
		publication, err := st.GetReviewPublication(ctx, reviewID)
		requireOK(t, err)
		s.lifecycle, s.publication = lifecycle.UpdatedAt, publication.UpdatedAt
		return s
	}
	before := take()
	later := now.Add(time.Hour)
	if n, err := store.ReconcileFailedPublications(ctx, st, later); err != nil || n != 0 {
		t.Fatalf("publication sweep with nothing due acted=%d err=%v", n, err)
	}
	if n, err := sweep(ctx, st, automaticReviewSweepConfig(workspace, policy), automaticSweepHead, later); err != nil || n != 0 {
		t.Fatalf("review sweep with nothing due acted=%d err=%v", n, err)
	}
	after := take()
	for taskID, count := range before.events {
		if after.events[taskID] != count {
			t.Fatalf("task %s events %d -> %d", taskID, count, after.events[taskID])
		}
	}
	for key, at := range before.updated {
		if !after.updated[key].Equal(at) {
			t.Fatalf("%s updated_at %v -> %v", key, at, after.updated[key])
		}
	}
	if !after.lifecycle.Equal(before.lifecycle) || !after.publication.Equal(before.publication) {
		t.Fatalf("publication rows updated_at lifecycle %v -> %v publication %v -> %v", before.lifecycle, after.lifecycle, before.publication, after.publication)
	}
}
