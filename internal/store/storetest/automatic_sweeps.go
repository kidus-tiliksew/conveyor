package storetest

import (
	"context"
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
		Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "45m"}}},
		Review:  config.ReviewPanel{Seats: []config.ReviewSeat{{}, {}}},
	}
}

func requireReviewSweep(t *testing.T, sweep ReviewSweepFunc) {
	t.Helper()
	if sweep == nil {
		t.Fatal("factory must wire workorder.Service.SweepAutomaticReviewRecovery")
	}
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
