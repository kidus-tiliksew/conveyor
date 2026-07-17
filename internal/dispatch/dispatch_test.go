package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type reviewAcceptanceFlakyStore struct {
	store.Store
	failures int
}

func approvedMergeFixture(t *testing.T, githubRepo string) (context.Context, store.Store, core.Task, *Dispatcher) {
	t.Helper()
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "merge-task", Workspace: "test", Repo: "app", Branch: "conveyor/merge-task", State: core.TaskApproved, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: githubRepo}}}, nil)
	return ctx, st, task, d
}

func TestMergeApprovedTaskMergesOnlyAfterAuthoritativeConfirmation(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	views, merges := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "open", Mergeable: "MERGEABLE"}, nil
		}
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "closed", Merged: true}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }

	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || merges != 1 || views != 2 {
		t.Fatalf("task=%+v merges=%d views=%d err=%v", current, merges, views, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requested, confirmed int
	for _, event := range events {
		requested += boolInt(event.Kind == "merge.requested")
		confirmed += boolInt(event.Kind == "merge.confirmed")
	}
	if requested != 1 || confirmed != 1 {
		t.Fatalf("events=%+v", events)
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if merges != 1 || views != 2 {
		t.Fatalf("idempotent retry merges=%d views=%d", merges, views)
	}
}

func TestMergeApprovedTaskReconcilesAlreadyMergedPR(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "closed", Merged: true}, nil
	}
	mergeCalled := false
	d.RequestMerge = func(context.Context, string, int) error { mergeCalled = true; return nil }
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalled {
		t.Fatalf("task=%+v mergeCalled=%t", current, mergeCalled)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	found := false
	for _, event := range events {
		found = found || event.Kind == "merge.reconciled"
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}
}

func TestMergeApprovedTaskFailuresStayApprovedAndAreAudited(t *testing.T) {
	for _, test := range []struct {
		name       string
		githubRepo string
		view       func(context.Context, string, string) (githubtrigger.PullRequest, error)
		merge      func(context.Context, string, int) error
		reason     string
	}{
		{name: "non GitHub repository", reason: "unsupported_repository"},
		{name: "missing pull request", githubRepo: "acme/app", reason: "missing_pull_request", view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{}, githubtrigger.ErrPullRequestNotFound
		}},
		{name: "forge merge failure", githubRepo: "acme/app", reason: "forge_merge_failed", view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE"}, nil
		}, merge: func(context.Context, string, int) error { return errors.New("branch protection") }},
		{name: "unconfirmed result", githubRepo: "acme/app", reason: "merge_unconfirmed", view: func() func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			calls := 0
			return func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				calls++
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: false}, nil
			}
		}(), merge: func(context.Context, string, int) error { return nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixture(t, test.githubRepo)
			if test.view != nil {
				d.ViewPullRequest = test.view
			}
			if test.merge != nil {
				d.RequestMerge = test.merge
			}
			if err := d.MergeApprovedTask(ctx, task); err == nil {
				t.Fatal("expected merge error")
			}
			current, _ := st.GetTask(ctx, task.ID)
			if current.State != core.TaskApproved {
				t.Fatalf("state=%s", current.State)
			}
			events, _ := st.ListEvents(ctx, task.ID)
			last := events[len(events)-1]
			if last.Kind != "merge.failed" || !strings.Contains(string(last.Payload), test.reason) {
				t.Fatalf("last event=%+v", last)
			}
		})
	}
}

func TestMergeApprovedTaskSerializesConcurrentRequests(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	merged, mergeCalls := false, 0
	var forgeMu sync.Mutex
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: merged}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		mergeCalls++
		merged = true
		return nil
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- d.MergeApprovedTask(ctx, task)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalls != 1 {
		t.Fatalf("state=%s mergeCalls=%d", current.State, mergeCalls)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (st *reviewAcceptanceFlakyStore) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	if st.failures > 0 {
		st.failures--
		return errors.New("review acceptance unavailable")
	}
	return st.Store.AcceptReviewDecision(ctx, decision)
}

type sequenceAgent struct {
	outputs []string
	next    int
	costUSD float64
}

func (agent *sequenceAgent) Run(context.Context, string, string) (inprocess.Result, error) {
	output := agent.outputs[agent.next]
	agent.next++
	return inprocess.Result{Output: output, TokensIn: 20, TokensOut: 10, CostUSD: agent.costUSD}, nil
}

func TestHighInProcessUsageDoesNotGatePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "high-usage", Workspace: "demo", Repo: "api", Title: "Small fix", Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"chore\",\"automatability\":1,\"route\":\"implement\",\"summary\":\"Ready.\"}\n```"}, costUSD: 20_000}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	job, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || job.State != core.JobDone || job.CostUSD != 20_000 {
		t.Fatalf("job=%+v ok=%t err=%v", job, ok, err)
	}
}

func TestInProcessTriageAndSpecAdvanceToImplementWorkOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "demo", Repo: "api", Title: "Add audit export", Body: "Specify and implement it", BaseBranch: "main", Branch: "conveyor/task-task", Level: core.L2, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{
		"```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.9,\"route\":\"spec\",\"summary\":\"Needs an accepted contract.\"}\n```",
		"# Audit export\n\n## Intent\nAdd the export.\n\n## Non-goals\nNo unrelated formats.\n\n```conveyor:acceptance\n- id: AC-1\n  criterion: Export tests pass\n  verify: test\n  ref: ./...\n```",
	}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"spec":      {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
		"review":    {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle

	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("after spec task=%+v err=%v", current, err)
	}
	spec, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok || spec.AcceptanceCount != 1 || spec.Approved {
		t.Fatalf("spec=%+v ok=%v err=%v", spec, ok, err)
	}
	latest, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("latest job ok=%v err=%v", ok, err)
	}
	if err = dispatcher.HandleIntervention(ctx, current, latest, core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement || orders[0].State != core.WorkOrderQueued {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
}

func TestResolvedSpecGateIsIndependentFromManualMode(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "policy-spec", Workspace: "demo", Repo: "api", Title: "Policy", Mode: core.TaskModeManual, PolicyVersion: 1, SpecApproval: true, MergeApproval: false, Level: core.L3, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.2,\"route\":\"human\",\"summary\":\"Needs review.\"}\n```", "# Policy spec\n\n## Intent\nDefine it.\n\n## Non-goals\nNone.\n\n```conveyor:acceptance\n- id: AC-1\n  criterion: Works\n  verify: test\n  ref: ./...\n```"}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute}, "spec": {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute}, "implement": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	d := New(st, cfg, agent)
	d.Pack = bundle
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.NextStage != core.StageSpec || current.State != core.TaskQueued {
		t.Fatalf("after triage=%+v", current)
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = st.GetTask(ctx, task.ID)
	if current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("after spec=%+v", current)
	}
}

func TestExternalReviewBounceCreatesNextImplementOrderWithFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "bounce-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, Branch: "conveyor/bounce", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{
		{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone, ModelTier: "impl", StartedAt: time.Now()},
		{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP}}}}
	d := New(st, cfg, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "review"}, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "missing coverage", Feedback: "add the test"}, "review-1", "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	if _, err = st.ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "warm-implement-session", ClientToken: "warm-token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].Comment != "add the test" {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	bounces := 0
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			bounces++
		}
	}
	if err != nil || bounces != 1 {
		t.Fatalf("bounces=%d err=%v", bounces, err)
	}
}

func TestExternalReviewAtBounceCapStopsAtHumanGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "cap-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 1}, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "stop", Feedback: "human help"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskAwaiting || updated.RecoveryStage != core.StageImplement || updated.NextStage != "" {
		t.Fatalf("task=%+v err=%v", updated, err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("unexpected follow-up orders: %+v", orders)
	}
}

func TestReviewAcceptanceFailureRollsBackAndRetryCommitsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := store.NewMemory()
	task := core.Task{ID: "publication-failure", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	flaky := &reviewAcceptanceFlakyStore{Store: base, failures: 1}
	d := New(flaky, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	d.UseDurableQueue()
	err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review")
	if err == nil || !strings.Contains(err.Error(), "review acceptance unavailable") {
		t.Fatalf("error = %v", err)
	}
	updated, getErr := base.GetTask(ctx, task.ID)
	if getErr != nil || updated.State != core.TaskRunning {
		t.Fatalf("task=%+v err=%v", updated, getErr)
	}
	events, _ := base.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "review.completed" || event.Kind == "review.publication_queued" {
			t.Fatalf("partial review acceptance event persisted: %s", event.Kind)
		}
	}
	if err = d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
		t.Fatalf("recovery retry failed: %v", err)
	}
	publication, err := base.GetReviewPublication(ctx, job.ID)
	if err != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	events, _ = base.ListEvents(ctx, task.ID)
	completed := 0
	for _, event := range events {
		if event.Kind == "review.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("review.completed events = %d, want 1", completed)
	}
}

func TestReviewForRepoWithoutGitHubDoesNotCreateOrReconcilePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "non-github-review", Workspace: "test", Repo: "local", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "non-github-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "local", URL: "file:///tmp/local"}}}, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetReviewPublication(ctx, job.ID); err == nil {
		t.Fatal("non-GitHub repository created review publication")
	}
	if repaired, err := st.ReconcileReviewPublications(ctx); err != nil || repaired != 0 {
		t.Fatalf("non-GitHub reconciliation=%d err=%v", repaired, err)
	}
	if _, err := st.GetReviewPublication(ctx, job.ID); err == nil {
		t.Fatal("non-GitHub review was reconciled into publication")
	}
}

func TestExistingUnacceptedReviewEventRepairsRouting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "partial-review", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "partial-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": job.ID, "verdict": "changes_requested", "publication_eligible": true,
	})}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	d.UseDurableQueue()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "retry", Feedback: "fix it"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("repaired task=%+v err=%v", current, err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Kind]++
	}
	if counts["review.completed"] != 1 || counts["pipeline.bounced"] != 1 || counts["review.accepted"] != 1 {
		t.Fatalf("recovery event counts=%v", counts)
	}
	if publication, getErr := st.GetReviewPublication(ctx, job.ID); getErr != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("repaired publication=%+v err=%v", publication, getErr)
	}
}

func TestExternalReviewApprovePreservesLevelRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		level    core.EscalationLevel
		state    core.TaskState
		recovery core.Stage
	}{
		{name: "L0 direct approval", level: core.L0, state: core.TaskApproved},
		{name: "L2 final human gate", level: core.L2, state: core.TaskAwaiting, recovery: core.StageImplement},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			task := core.Task{ID: "approve-" + string(test.level), Workspace: "test", Repo: "app", Level: test.level, State: core.TaskRunning, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2}, nil)
			d.UseDurableQueue()
			if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
				t.Fatal(err)
			}
			updated, err := st.GetTask(ctx, task.ID)
			if err != nil || updated.State != test.state || updated.RecoveryStage != test.recovery {
				t.Fatalf("task=%+v err=%v", updated, err)
			}
		})
	}
}

func TestResolvedMergeGateControlsHumanWaitOrAutomaticMerge(t *testing.T) {
	for _, test := range []struct {
		name          string
		mergeApproval bool
		want          core.TaskState
	}{{"human merge gate", true, core.TaskAwaiting}, {"automatic merge", false, core.TaskMerged}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			st := store.NewMemory()
			task := core.Task{ID: "policy-" + test.name, Workspace: "test", Repo: "app", Branch: "conveyor/policy", Mode: core.TaskModeAuto, PolicyVersion: 1, SpecApproval: false, MergeApproval: test.mergeApproval, Level: core.LegacyLevel(core.TaskModeAuto, false, test.mergeApproval), State: core.TaskRunning, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning, StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
			d.UseDurableQueue()
			merged := false
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 7, State: map[bool]string{true: "closed", false: "open"}[merged], Merged: merged, Mergeable: "MERGEABLE"}, nil
			}
			d.RequestMerge = func(context.Context, string, int) error { merged = true; return nil }
			if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review-model"); err != nil {
				t.Fatal(err)
			}
			updated, _ := st.GetTask(ctx, task.ID)
			if updated.State != test.want {
				t.Fatalf("state=%s want=%s", updated.State, test.want)
			}
		})
	}
}

func TestUnanimousReviewPanelSurvivesRestartAndUsesResolvedMergeGate(t *testing.T) {
	for _, test := range []struct {
		name          string
		mergeApproval bool
		want          core.TaskState
	}{{"human merge gate", true, core.TaskAwaiting}, {"automatic merge", false, core.TaskMerged}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			st := store.NewMemory()
			now := time.Now().UTC()
			task := core.Task{ID: "panel-policy-" + test.name, Workspace: "test", Repo: "app", Branch: "conveyor/panel-policy", Mode: core.TaskModeAuto, PolicyVersion: 1, MergeApproval: test.mergeApproval, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			jobs := []core.Job{
				{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
				{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
			}
			orders := []core.WorkOrder{
				{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
				{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "claude-review", RequiredHarness: "claude", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
			}
			if err := st.CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
			firstDispatcher := New(st, cfg, nil)
			firstDispatcher.UseDurableQueue()
			if err := firstDispatcher.ApplyExternalReview(ctx, task, jobs[0], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat one passes", Feedback: "seat one evidence"}, orders[0].ID, "review-session-1", "gpt-review"); err != nil {
				t.Fatal(err)
			}
			if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskRunning {
				t.Fatalf("panel advanced before unanimous verdict: %+v", current)
			}

			// A new dispatcher instance represents restart recovery before the
			// second durable verdict arrives.
			restarted := New(st, cfg, nil)
			restarted.UseDurableQueue()
			merged := false
			restarted.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 7, State: map[bool]string{true: "closed", false: "open"}[merged], Merged: merged, Mergeable: "MERGEABLE"}, nil
			}
			restarted.RequestMerge = func(context.Context, string, int) error { merged = true; return nil }
			if err := restarted.ApplyExternalReview(ctx, task, jobs[1], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat two passes", Feedback: "seat two evidence"}, orders[1].ID, "review-session-2", "claude-review"); err != nil {
				t.Fatal(err)
			}
			updated, _ := st.GetTask(ctx, task.ID)
			if updated.State != test.want {
				t.Fatalf("state=%s want=%s", updated.State, test.want)
			}
			if count, err := st.CountEvents(ctx, task.ID, "review.round_completed"); err != nil || count != 1 {
				t.Fatalf("completed rounds=%d err=%v", count, err)
			}
		})
	}
}
