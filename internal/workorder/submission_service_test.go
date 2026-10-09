package workorder

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func TestSubmitForReviewReturnsSynchronousInProcessVerdict(t *testing.T) {
	t.Parallel()
	for _, recording := range []bool{true, false} {
		t.Run(fmt.Sprint(recording), func(t *testing.T) {
			ctx := store.WithActor(store.WithWorkspace(context.Background(), "test"), store.Actor{ID: "user:owner", Role: core.ActorUser})
			st := store.NewMemory()
			task := core.Task{ID: "task-sync", Workspace: "test", Repo: "app", Title: "Change", Branch: "conveyor/task-sync", BaseBranch: "main", Level: core.L0, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "base123", "head_sha": "abc123"})}); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, ModelTier: "implementer", StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
				t.Fatal(err)
			}
			claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("owner"), SessionID: "implement-session", ClientToken: "implement-token", Agent: "codex", Model: "implementer", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"review": {Model: "reviewer", Execution: config.ExecutionInProcess, Timeout: time.Minute},
			}}}
			agent := &staticAgent{output: "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"all criteria pass\",\"feedback\":\"\"}\n```"}
			dispatcher := dispatch.New(st, cfg, agent)
			dispatcher.Pack = bundle
			dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
				return "diff --git a/app.txt b/app.txt\n-v1\n+v2\n", nil
			}
			service := &Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}

			if _, err = service.Usage(ctx, claimed.ID, "implement-session", 100_000_000, 25_000_000); err != nil {
				t.Fatalf("high usage report failed: %v", err)
			}
			baseline, getErr := service.RefreshContext(ctx, claimed.ID, "implement-session", "")
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !baseline.ObservationRecorded {
				t.Fatal(baseline)
			}
			correction, attachErr := st.CreateArtifact(ctx, core.Artifact{Name: "late-correction.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("late correction"))
			if attachErr != nil {
				t.Fatal(attachErr)
			}
			if !recording {
				service.Store = &freshnessFixtureStore{Store: st, failObservation: true}
			}
			prepareSubmissionTest(service)
			result, err := service.SubmitForReview(ctx, claimed.ID, "implement-session", submissionTestHead(service))
			if err != nil {
				t.Fatal(err)
			}
			freshness, ok := result["context_freshness"].(core.ContextFreshness)
			if !ok || freshness.UnfetchedAdditions != 1 || freshness.ObservationRecorded != recording || freshness.Additions.Items[0].ArtifactID != correction.ID {
				t.Fatalf("submission freshness=%+v", result["context_freshness"])
			}
			if result["await_review"] != false || result["verdict"] != "approve" {
				t.Fatalf("result = %+v", result)
			}
			if !strings.Contains(agent.input.Prompt, "```conveyor:review") || strings.Contains(agent.input.Prompt, "submit_review_verdict") {
				t.Fatalf("in-process review prompt has the wrong terminal contract: %s", agent.input.Prompt)
			}
			if !strings.Contains(agent.input.Prompt, "diff --git a/app.txt b/app.txt") {
				t.Fatalf("in-process review prompt is missing the branch diff: %s", agent.input.Prompt)
			}
			updated, err := st.GetTask(ctx, task.ID)
			if err != nil || updated.State != core.TaskApproved {
				t.Fatalf("task = %+v err=%v", updated, err)
			}
		})
	}
}

type failingSubmissionGovernanceStore struct {
	store.Store
	err error
}

func (s failingSubmissionGovernanceStore) AttachSubmissionGovernance(context.Context, string, string, []string, store.SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	return nil, s.err
}

func TestSubmitForReviewGovernanceFailuresPrecedeReviewSideEffects(t *testing.T) {
	for _, test := range []struct {
		name       string
		pathErr    error
		attachErr  error
		wantDetail string
	}{
		{name: "diff resolution", pathErr: errors.New("diff unavailable"), wantDetail: "resolve submission diff changed paths"},
		{name: "atomic attachment", attachErr: errors.New("transaction rolled back"), wantDetail: "attach submission governance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			base := store.NewMemory()
			design, version, err := base.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-submit-failure", Title: "Submit failure", Category: "Architecture"}, core.SystemDesignVersion{
				Content: "# Submit\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = base.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
				t.Fatal(err)
			}
			task := core.Task{ID: "submission-failure-" + strings.ReplaceAll(test.name, " ", "-"), Workspace: "test", Repo: "app", Title: "Change", Branch: "conveyor/failure", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
			if err = base.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
			if err = base.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err = storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
				t.Fatal(err)
			}
			if _, err = storetest.For(base).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner", Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			dispatcher := dispatch.New(base, cfg, nil)
			dispatcher.DisableMemoryQueueForTest()
			var serviceStore store.Store = base
			if test.attachErr != nil {
				serviceStore = failingSubmissionGovernanceStore{Store: base, err: test.attachErr}
			}
			opened := 0
			service := &Service{Store: serviceStore, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
				SubmissionChangedPaths: func(context.Context, *config.Config, core.Task) ([]string, error) {
					return []string{"internal/change.go"}, test.pathErr
				},
				ReconcileSubmissionPR: func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error {
					opened++
					return errors.New("unexpected reconciliation")
				},
			}
			prepareSubmissionTest(service)
			if _, err = service.SubmitForReview(ctx, job.ID, "implementer", submissionTestHead(service)); err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("submit error=%v", err)
			}
			order, orderErr := base.GetWorkOrder(ctx, job.ID)
			current, taskErr := base.GetTask(ctx, task.ID)
			context, contextErr := store.TaskContextForTask(ctx, base, task.ID)
			if orderErr != nil || taskErr != nil || contextErr != nil || order.State != core.WorkOrderClaimed || current.NextStage != core.StageImplement || opened != 0 || len(context.Designs) != 0 {
				t.Fatalf("order=%+v task=%+v context=%+v opened=%d errs=%v/%v/%v", order, current, context, opened, orderErr, taskErr, contextErr)
			}
		})
	}
}

func TestSubmissionDerivedGovernanceEngagesTaskProposalReviewGate(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	design, confirmed, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-derived-gate", Title: "Derived gate", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Gate\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, confirmed.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "submission-derived-gate", Workspace: "test", Repo: "app", Title: "Gate change", Branch: "conveyor/derived-gate", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Proposed\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Execution: config.ExecutionMCP, Model: "reviewer", Timeout: time.Hour, TimeoutText: "1h"},
	}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	service := &Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }, SubmissionChangedPaths: func(context.Context, *config.Config, core.Task) ([]string, error) {
		return []string{"internal/workorder/service.go"}, nil
	}}
	prepareSubmissionTest(service)
	if _, err = service.SubmitForReview(ctx, job.ID, "implementer", submissionTestHead(service)); err != nil {
		t.Fatal(err)
	}
	context, err := store.TaskContextForTask(ctx, st, task.ID)
	if err != nil || len(context.Designs) != 1 || context.Designs[0].ID != design.ID {
		t.Fatalf("derived context=%+v err=%v", context, err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var review core.WorkOrder
	for _, order := range orders {
		if order.Stage == core.StageReview {
			review = order
		}
	}
	if review.ID == "" {
		t.Fatalf("review order missing: %+v", orders)
	}
	if _, err = service.Claim(ctx, review.ID, core.WorkOrderClaim{SessionID: "reviewer", ClientToken: "review-secret", ClaimantID: "reviewer", Lease: time.Minute}); err == nil || !strings.Contains(err.Error(), "waiting on task-authored System Design proposal "+design.ID+" v") {
		t.Fatalf("review proposal gate error=%v", err)
	}
}

// evidenceSubmissionFixture is one valid claimed implementation submission
// with a two-seat review panel and recorded PR body.
type evidenceSubmissionFixture struct {
	ctx       context.Context
	st        store.Store
	task      core.Task
	otherTask core.Task
	job       core.Job
	service   *Service
	openCalls *int
	prBody    *string
}

func newEvidenceSubmissionFixture(t *testing.T) evidenceSubmissionFixture {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "evidence-task", Workspace: "demo", Repo: "app", Source: "roadmap:phase-5.4",
		Title: "Evidence change", Branch: "conveyor/evidence-task", BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now(),
	}
	otherTask := core.Task{ID: "other-task", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	for _, candidate := range []core.Task{task, otherTask} {
		if err := st.CreateTask(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("usr-evidence"), OwnerUserID: "usr-evidence", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", MaxBounces: 2,
		Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{
			{Model: "reviewer-a"}, {Model: "reviewer-b"},
		}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	openCalls := 0
	prBody := ""
	service := &Service{
		Store: st, Dispatcher: dispatcher, Pack: bundle,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		ReconcileSubmissionPR: func(_ context.Context, _ string, _ githubtrigger.SubmissionPullRequest, body string) error {
			openCalls++
			prBody = body
			return nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 54, BaseSHA: "base123", HeadSHA: "abc123"}, nil
		},
	}
	return evidenceSubmissionFixture{ctx: ctx, st: st, task: task, otherTask: otherTask, job: job, service: service, openCalls: &openCalls, prBody: &prBody}
}

// submitAndClaimSeats submits the implementation, dispatches the review panel,
// and returns each seat's served verification-evidence references.
func (f evidenceSubmissionFixture) submitAndClaimSeats(t *testing.T) [][]ArtifactReference {
	t.Helper()
	prepareSubmissionTest(f.service)
	result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", submissionTestHead(f.service))
	if err != nil || result["await_review"] != true || *f.openCalls != 1 {
		t.Fatalf("submit=%+v open_calls=%d err=%v", result, *f.openCalls, err)
	}
	order, err := f.st.GetWorkOrder(f.ctx, f.job.ID)
	if err != nil || order.State != core.WorkOrderSubmitted {
		t.Fatalf("implementation order=%+v err=%v", order, err)
	}
	links, err := f.st.ListLineageLinks(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantRange := core.CommitRangeLineageID("acme/app", "base123", "abc123")
	foundPR, foundRange := false, false
	for _, link := range links {
		foundPR = foundPR || (link.Kind == "submitted_as" && link.DstID == core.PullRequestLineageID("acme/app", 54))
		foundRange = foundRange || (link.Kind == "submitted_range" && link.DstID == wantRange)
	}
	if !foundPR || !foundRange {
		t.Fatalf("submission lineage=%+v", links)
	}
	if err = f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seats [][]ArtifactReference
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		session := "review-session-" + order.ID
		claimed, claimErr := f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "review-secret-" + order.ID, ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Lease: time.Minute})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if claimed.ServedRequirementSnapshot == nil {
			t.Fatalf("seat %s did not pin an empty served-requirement snapshot", order.ID)
		}
		if len(claimed.ServedRequirementSnapshot) != 0 {
			t.Fatalf("seat %s snapshot=%+v, want empty", order.ID, claimed.ServedRequirementSnapshot)
		}
		reloaded, reloadErr := f.st.GetWorkOrder(f.ctx, order.ID)
		if reloadErr != nil {
			t.Fatal(reloadErr)
		}
		if reloaded.ServedRequirementSnapshot == nil || len(reloaded.ServedRequirementSnapshot) != 0 {
			t.Fatalf("seat %s reloaded snapshot=%+v, want non-nil empty", order.ID, reloaded.ServedRequirementSnapshot)
		}
		served, getErr := f.service.Get(f.ctx, order.ID, session)
		if getErr != nil {
			t.Fatal(getErr)
		}
		for _, reference := range served.VerificationEvidence {
			if reference.WorkOrderID != order.ID || reference.ReadTool != "read_artifact" || reference.DownloadURL != "" {
				t.Fatalf("seat %s reference=%+v", order.ID, reference)
			}
		}
		seats = append(seats, served.VerificationEvidence)
	}
	if len(seats) != 2 {
		t.Fatalf("review seats=%d orders=%+v", len(seats), orders)
	}
	return seats
}

// No workspace setting refuses a submission for missing verification
// evidence, and only eligible task-owned evidence reaches every review seat
// and the pull request (req-review-gates-evidence AC-8.1, AC-8.2, AC-8.3;
// DEC-53).
func TestSubmitForReviewAdmitsWithoutEvidenceAndPropagatesOnlyEligibleEvidence(t *testing.T) {
	addIneligible := func(t *testing.T, f evidenceSubmissionFixture) {
		t.Helper()
		if _, err := f.st.CreateArtifact(f.ctx, core.Artifact{
			Name: "wrong-role.png", ContentType: "image/png", Role: core.ArtifactRoleTaskContext, TaskID: f.task.ID,
		}, testimage.PNG("wrong role")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.CreateArtifact(f.ctx, core.Artifact{
			Name: "other.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: f.otherTask.ID,
		}, testimage.PNG("cross task")); err != nil {
			t.Fatal(err)
		}
	}
	assertNoEvidenceServed := func(t *testing.T, f evidenceSubmissionFixture, seats [][]ArtifactReference) {
		t.Helper()
		if strings.Contains(*f.prBody, "<!-- conveyor:verification-evidence -->") {
			t.Fatalf("PR body lists evidence the task does not own: %s", *f.prBody)
		}
		for i, seat := range seats {
			if len(seat) != 0 {
				t.Fatalf("seat %d evidence=%+v, want none", i+1, seat)
			}
		}
	}

	t.Run("no evidence", func(t *testing.T) {
		f := newEvidenceSubmissionFixture(t)
		assertNoEvidenceServed(t, f, f.submitAndClaimSeats(t))
	})

	t.Run("only ineligible evidence", func(t *testing.T) {
		f := newEvidenceSubmissionFixture(t)
		addIneligible(t, f)
		assertNoEvidenceServed(t, f, f.submitAndClaimSeats(t))
	})

	t.Run("eligible owned evidence", func(t *testing.T) {
		f := newEvidenceSubmissionFixture(t)
		addIneligible(t, f)
		evidence, err := f.st.CreateArtifact(f.ctx, core.Artifact{
			Name: "exercised UI `proof`.png", ContentType: "image/png; charset=binary",
			Role: core.ArtifactRoleVerificationEvidence, TaskID: f.task.ID,
			DownloadURL: "https://control-plane.invalid/private?token=secret",
		}, testimage.PNG("valid evidence"))
		if err != nil {
			t.Fatal(err)
		}
		seats := f.submitAndClaimSeats(t)
		prBody := *f.prBody
		if strings.Count(prBody, "<!-- conveyor:verification-evidence -->") != 1 ||
			!strings.Contains(prBody, evidence.ID) || !strings.Contains(prBody, "image/png") ||
			strings.Contains(prBody, "control-plane.invalid") || strings.Contains(prBody, "token=secret") {
			t.Fatalf("unsafe or incomplete PR evidence body: %s", prBody)
		}
		for i, seat := range seats {
			if len(seat) != 1 || seat[0].ID != evidence.ID {
				t.Fatalf("seat %d evidence=%+v, want only %s", i+1, seat, evidence.ID)
			}
		}
	})
}

func TestSubmitForReviewWaitsForIssueAndPassesClosingReference(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "issue-linked", Workspace: "test", Repo: "app", Title: "Linked change", Branch: "conveyor/issue-linked", BaseBranch: "main", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	lifecycle := core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: spec.Version}
	if err = st.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "issue-linked-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("usr-executor"), OwnerUserID: "usr-executor", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	opened := 0
	var body string
	service := &Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }, ReconcileSubmissionPR: func(_ context.Context, _ string, _ githubtrigger.SubmissionPullRequest, value string) error {
		opened++
		body = value
		return nil
	}, ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
		return githubtrigger.ReviewTarget{Number: 9, HeadSHA: "abc"}, nil
	}}
	prepareSubmissionTest(service)
	if _, err = service.SubmitForReview(ctx, job.ID, "implementer", submissionTestHead(service)); err == nil || !strings.Contains(err.Error(), "retry after publication") || opened != 0 {
		t.Fatalf("pending issue submit err=%v opened=%d", err, opened)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	lifecycle.State = core.GitHubPublicationPublished
	lifecycle.IssueNumber = 42
	lifecycle.IssueURL = "https://github.com/acme/app/issues/42"
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	prepareSubmissionTest(service)
	if _, err = service.SubmitForReview(ctx, job.ID, "implementer", submissionTestHead(service)); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || !strings.Contains(body, "Closes #42") {
		t.Fatalf("reconciled=%d body=%q", opened, body)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundAttribution := false
	for _, event := range events {
		foundAttribution = foundAttribution || (event.Kind == "pull_request.opened" && strings.Contains(string(event.Payload), `"forge_author_class":"executing_user"`) && strings.Contains(string(event.Payload), `"forge_author_user_id":"usr-executor"`) && !strings.Contains(string(event.Payload), "executor-forge-token"))
	}
	if !foundAttribution {
		t.Fatalf("pull request attribution missing or unsafe: %+v", events)
	}
}

func TestSubmitForReviewAdvancesStaleRefreshHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "stale-refresh", Workspace: "test", Repo: "app", Title: "Fix", Branch: "conveyor/stale-refresh", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "conflict-fix-head", config.RefreshReviewDelta, "merge-conflict"); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "stale-refresh-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "implementer", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("usr-refresh"), OwnerUserID: "usr-refresh", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	service := &Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		ReconcileSubmissionPR: func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error {
			return nil
		},
		ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
			return githubtrigger.ReviewTarget{Number: 7, HeadSHA: "panel-fix-head"}, nil
		}}
	prepareSubmissionTest(service)
	if _, err := service.SubmitForReview(ctx, job.ID, "implementer", submissionTestHead(service)); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || !updated.ApprovalStale || updated.RefreshBaselineSHA != "approved-head" || updated.RefreshHeadSHA != "panel-fix-head" {
		t.Fatalf("task = %+v err=%v", updated, err)
	}
	if n, countErr := st.CountEvents(ctx, task.ID, "review.refresh_head_advanced"); countErr != nil || n != 1 {
		t.Fatalf("advance events=%d err=%v", n, countErr)
	}
	// The next refresh round must contract the newly pushed head, not the
	// head recorded when the approval went stale (design-git-delivery).
	_, orders, err := dispatch.BuildReviewRound(cfg, updated, cfg.Routing.Stages["review"], 2)
	if err != nil || len(orders) == 0 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	for _, order := range orders {
		if order.ReviewKind != "refresh" || order.BaselineSHA != "approved-head" || order.HeadSHA != "panel-fix-head" {
			t.Fatalf("refresh order contract = %+v", order)
		}
	}
}

// conflictSubmission is a ReasonCode merge-conflict implement order claimed
// for a task with an older approved round, an unresolved conflict episode,
// its own running implement job, and a newer job that is not the order's.
type conflictSubmission struct {
	ctx        context.Context
	st         store.Store
	cfg        *config.Config
	task       core.Task
	order      core.WorkOrder
	service    *Service
	reads      int
	waits      []time.Duration
	reconciled int
	prHead     func(read int) string
	prBase     string
}

func newConflictSubmission(t *testing.T, policy config.ExecutionSetup, wrap ...func(store.Store) store.Store) *conflictSubmission {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "test")
	var st store.Store = store.NewMemory()
	for _, apply := range wrap {
		st = apply(st)
	}
	fixture := &conflictSubmission{ctx: ctx, st: st, prBase: "main", prHead: func(int) string { return "fix-head" }}
	task := core.Task{ID: "conflict-submit", Workspace: "test", Repo: "app", Title: "Conflict fix", Branch: "conveyor/conflict-submit", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, SetupContract: policy, ApprovedHeadSHA: "approved-head", ReviewedHeadSHA: "approved-head", CreatedAt: time.Now().Add(-time.Hour)}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	older := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, older); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 1, "verdict": "approve", "approved_head_sha": "approved-head"})},
		{TaskID: task.ID, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"workspace": "test", "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": "approved-head", "new_head": "conflict-head"})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict", BaselineSHA: "approved-head", CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "fixer", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("usr-fixer"), OwnerUserID: "usr-fixer", Lease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	newer := core.Job{ID: task.ID + "-verify-9", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	if err = st.CreateJob(ctx, newer); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
	d := dispatch.New(st, fixture.cfg, nil)
	d.DisableMemoryQueueForTest()
	fixture.service = &Service{Store: st, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return fixture.cfg, nil },
		SubmissionPR: func(_ context.Context, _ string, branch string) (githubtrigger.SubmissionPullRequest, error) {
			fixture.reads++
			pr := githubtrigger.SubmissionPullRequest{Number: 1098, URL: "https://github.com/acme/app/pull/1098"}
			pr.Head.Ref, pr.Head.SHA, pr.Base.Ref, pr.Base.SHA = branch, fixture.prHead(fixture.reads), fixture.prBase, "base-sha"
			return pr, nil
		},
		SubmissionPRWait: func(_ context.Context, delay time.Duration) error {
			fixture.waits = append(fixture.waits, delay)
			return nil
		},
		ReconcileSubmissionPR: func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error {
			fixture.reconciled++
			return nil
		},
	}
	prepareSubmissionTest(fixture.service)
	fixture.task, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.order = order
	return fixture
}

func (f *conflictSubmission) submit(session, head string) (map[string]any, error) {
	return f.service.SubmitForReview(f.ctx, f.order.ID, session, head)
}

func (f *conflictSubmission) jobState(t *testing.T, id string) core.JobState {
	t.Helper()
	jobs, err := f.st.ListJobs(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == id {
			return job.State
		}
	}
	t.Fatalf("job %s missing", id)
	return ""
}

func (f *conflictSubmission) eventCount(t *testing.T) int {
	t.Helper()
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

func (f *conflictSubmission) countKind(t *testing.T, kind string) int {
	t.Helper()
	count, err := f.st.CountEvents(f.ctx, f.task.ID, kind)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// assertUntouched proves a refused submission left the claim, job, task, and
// history exactly as they were.
func (f *conflictSubmission) assertUntouched(t *testing.T, events int, state core.TaskState) {
	t.Helper()
	order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
	if err != nil || order.State != core.WorkOrderClaimed || order.SessionID != "fixer" || order.HeadSHA != "" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	if got := f.jobState(t, f.order.JobID); got != core.JobRunning {
		t.Fatalf("implement job state=%s", got)
	}
	if got := f.eventCount(t); got != events {
		t.Fatalf("events before=%d after=%d", events, got)
	}
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil || task.State != state || f.reconciled != 0 {
		t.Fatalf("task=%+v reconciled=%d err=%v", task, f.reconciled, err)
	}
}

func TestConflictFixSubmissionRetriesLaggingPullRequestHead(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{})
	f.prHead = func(read int) string {
		if read < 3 {
			return "conflict-head"
		}
		return "fix-head"
	}
	result, err := f.submit("fixer", "fix-head")
	if err != nil {
		t.Fatal(err)
	}
	if f.reads != 3 || !reflect.DeepEqual(f.waits, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}) || result["pr_url"] != "https://github.com/acme/app/pull/1098" {
		t.Fatalf("reads=%d waits=%v result=%v", f.reads, f.waits, result)
	}
	order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
	if err != nil || order.State != core.WorkOrderSubmitted || order.HeadSHA != "fix-head" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	// The order's own job completes even though a newer job is the task's
	// latest; the newer job is untouched.
	if f.jobState(t, f.order.JobID) != core.JobDone || f.jobState(t, f.task.ID+"-verify-9") != core.JobPending {
		t.Fatalf("implement=%s newer=%s", f.jobState(t, f.order.JobID), f.jobState(t, f.task.ID+"-verify-9"))
	}
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil || task.State != core.TaskQueued || task.NextStage != core.StageReview || !task.ApprovalStale || task.RefreshBaselineSHA != "approved-head" || task.RefreshHeadSHA != "fix-head" || task.RefreshReviewScope != config.RefreshReviewDelta {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	if f.countKind(t, "approval.stale") != 1 || f.countKind(t, "pull_request.opened") != 1 {
		t.Fatalf("stale=%d opened=%d", f.countKind(t, "approval.stale"), f.countKind(t, "pull_request.opened"))
	}
}

func TestConflictFixSubmissionPullRequestRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		head   func(int) string
		base   string
		lookup func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error)
		wait   func(context.Context, time.Duration) error
		want   string
		reads  int
		waits  []time.Duration
	}{
		{name: "permanent mismatch", head: func(int) string { return "conflict-head" }, want: "pull_request_head_mismatch: branch conveyor/conflict-submit expected head fix-head observed head conflict-head", reads: 6, waits: submissionPRHeadBackoff},
		{name: "wrong base", base: "release", want: "pull_request_base_mismatch", reads: 1},
		{name: "wrong base and head", head: func(int) string { return "conflict-head" }, base: "release", want: "pull_request_head_mismatch", reads: 1},
		{name: "missing pull request", lookup: func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
			return githubtrigger.SubmissionPullRequest{}, nil
		}, want: "pull_request_missing", reads: 0},
		{name: "forge failure", lookup: func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
			return githubtrigger.SubmissionPullRequest{}, errors.New("forge unavailable")
		}, want: "read pull request for branch conveyor/conflict-submit expected head fix-head: forge unavailable", reads: 0},
		{name: "cancelled wait", head: func(int) string { return "conflict-head" }, wait: func(context.Context, time.Duration) error { return context.Canceled }, want: "context canceled", reads: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConflictSubmission(t, config.ExecutionSetup{})
			if test.head != nil {
				f.prHead = test.head
			}
			if test.base != "" {
				f.prBase = test.base
			}
			if test.lookup != nil {
				f.service.SubmissionPR = test.lookup
			}
			if test.wait != nil {
				f.service.SubmissionPRWait = test.wait
			}
			events := f.eventCount(t)
			_, err := f.submit("fixer", "fix-head")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want %q", err, test.want)
			}
			if f.reads != test.reads || len(f.waits) != len(test.waits) || (len(test.waits) > 0 && !reflect.DeepEqual(f.waits, test.waits)) {
				t.Fatalf("reads=%d waits=%v", f.reads, f.waits)
			}
			f.assertUntouched(t, events, core.TaskRunning)
		})
	}
	var total time.Duration
	for _, delay := range submissionPRHeadBackoff {
		total += delay
	}
	if len(submissionPRHeadBackoff)+1 != 6 || total != 3100*time.Millisecond {
		t.Fatalf("backoff=%v total=%s", submissionPRHeadBackoff, total)
	}
}

func TestConflictFixSubmissionRealWaitHonorsCancellation(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{})
	f.service.SubmissionPRWait = nil
	f.prHead = func(int) string { return "conflict-head" }
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	events := f.eventCount(t)
	if _, err := f.service.SubmitForReview(ctx, f.order.ID, "fixer", "fix-head"); !errors.Is(err, context.Canceled) || f.reads != 1 {
		t.Fatalf("err=%v reads=%d", err, f.reads)
	}
	f.assertUntouched(t, events, core.TaskRunning)
}

func TestConflictFixSubmissionRefusedBeforeSideEffectsWhenTaskLeftRunning(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{})
	// The incident: the readiness sweep moved the task running→queued while the
	// conflict-fix order was still claimed, without a refresh for this head.
	if _, err := taskops.New(f.st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskRecoverRefresh, NextStage: core.StageReview, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	events := f.eventCount(t)
	_, err := f.submit("fixer", "fix-head")
	if err == nil || !strings.Contains(err.Error(), "submission refused before side effects") || !strings.Contains(err.Error(), "stage.advance") {
		t.Fatalf("err=%v", err)
	}
	if f.reads != 0 {
		t.Fatalf("pull request read before admission: %d", f.reads)
	}
	f.assertUntouched(t, events, core.TaskQueued)
}

func TestConflictFixSubmissionRequiresItsOwnRunningJob(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{})
	job := core.Job{ID: f.order.JobID, TaskID: f.task.ID, Stage: core.StageImplement, State: core.JobFailed, EndedAt: time.Now()}
	if err := f.st.UpdateJob(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	events := f.eventCount(t)
	if _, err := f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "submission requires a running job") || f.reads != 0 {
		t.Fatalf("err=%v reads=%d", err, f.reads)
	}
	if got := f.eventCount(t); got != events || f.reconciled != 0 {
		t.Fatalf("events before=%d after=%d reconciled=%d", events, got, f.reconciled)
	}
	order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
	if err != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}

// dispatchExistingRefresh reproduces what the pre-fix sweep did while the
// order was claimed: stale approval for the pushed head, running→queued, and
// a refresh round (or verify order) contracting that head.
func (f *conflictSubmission) dispatchExistingRefresh(t *testing.T, head, scope string) {
	t.Helper()
	if _, err := f.st.MarkTaskApprovalStale(f.ctx, f.task.ID, "approved-head", head, scope, "head-changed"); err != nil {
		t.Fatal(err)
	}
	stage := core.StageReview
	if f.task.SetupContract.VerifyStage {
		stage = core.StageVerify
	}
	if _, err := taskops.New(f.st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskRecoverRefresh, NextStage: stage, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stage == core.StageVerify {
		job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: core.VerifyStageHead(task), BaselineSHA: task.RefreshBaselineSHA, ReviewScope: task.RefreshReviewScope, CreatedAt: time.Now()}
		if _, err = storetest.For(f.st).CreateStageWorkOrder(f.ctx, job, order); err != nil {
			t.Fatal(err)
		}
		return
	}
	jobs, orders, err := dispatch.BuildReviewRound(f.cfg, task, f.cfg.Routing.Stages["review"], 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(f.st).CreateReviewRound(f.ctx, task.ID, jobs, orders); err != nil {
		t.Fatal(err)
	}
}

func TestConflictFixSubmissionCompletesOnceWhenRefreshAlreadyDispatched(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy config.ExecutionSetup
		scope  string
		stage  core.Stage
	}{
		{name: "delta review", scope: config.RefreshReviewDelta, stage: core.StageReview},
		{name: "none policy raised to delta", policy: config.ExecutionSetup{RefreshReview: config.RefreshReviewNone}, scope: config.RefreshReviewDelta, stage: core.StageReview},
		{name: "full policy", policy: config.ExecutionSetup{RefreshReview: config.RefreshReviewFull}, scope: config.RefreshReviewFull, stage: core.StageReview},
		{name: "frozen verify stage", policy: config.ExecutionSetup{VerifyStage: true}, scope: config.RefreshReviewDelta, stage: core.StageVerify},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConflictSubmission(t, test.policy)
			f.dispatchExistingRefresh(t, "fix-head", test.scope)
			ordersBefore, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			staleBefore := f.countKind(t, "approval.stale")
			result, err := f.submit("fixer", "fix-head")
			if err != nil {
				t.Fatal(err)
			}
			if result["refresh_already_dispatched"] != true || f.reads != 0 || f.reconciled != 0 {
				t.Fatalf("result=%v reads=%d reconciled=%d", result, f.reads, f.reconciled)
			}
			if test.stage == core.StageVerify && result["next_stage"] != core.StageVerify {
				t.Fatalf("verify routing result=%v", result)
			}
			if test.stage == core.StageReview && result["review_round"] != 2 {
				t.Fatalf("review round result=%v", result)
			}
			order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
			if err != nil || order.State != core.WorkOrderSubmitted || order.HeadSHA != "fix-head" || f.jobState(t, f.order.JobID) != core.JobDone {
				t.Fatalf("order=%+v job=%s err=%v", order, f.jobState(t, f.order.JobID), err)
			}
			ordersAfter, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
			if err != nil || len(ordersAfter) != len(ordersBefore) {
				t.Fatalf("orders before=%d after=%d err=%v", len(ordersBefore), len(ordersAfter), err)
			}
			task, err := f.st.GetTask(f.ctx, f.task.ID)
			if err != nil || task.State != core.TaskQueued || task.NextStage != test.stage || task.RefreshHeadSHA != "fix-head" || task.RefreshReviewScope != test.scope {
				t.Fatalf("task=%+v err=%v", task, err)
			}
			if f.countKind(t, "approval.stale") != staleBefore || f.countKind(t, "pull_request.opened") != 0 || f.countKind(t, "review.refresh_round_created") != 0 {
				t.Fatalf("duplicate effects stale=%d opened=%d", f.countKind(t, "approval.stale"), f.countKind(t, "pull_request.opened"))
			}
			// The same session replays the identical head without any write.
			events := f.eventCount(t)
			replay, err := f.submit("fixer", "fix-head")
			if err != nil || replay["replayed"] != true || f.eventCount(t) != events || f.reads != 0 {
				t.Fatalf("replay=%v err=%v events before=%d after=%d", replay, err, events, f.eventCount(t))
			}
			if _, err = f.service.PullRequestTemplate(f.ctx, f.order.ID, "fixer"); err != nil {
				t.Fatalf("template reread for replay: %v", err)
			}
		})
	}
}

func TestConflictFixSubmissionRefusesUnprovenRefreshAndForeignReplays(t *testing.T) {
	t.Run("unrelated refresh head", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		f.dispatchExistingRefresh(t, "other-head", config.RefreshReviewDelta)
		events := f.eventCount(t)
		if _, err := f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "submission refused before side effects") {
			t.Fatalf("err=%v", err)
		}
		f.assertUntouched(t, events, core.TaskQueued)
	})
	t.Run("narrower than frozen full policy", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{RefreshReview: config.RefreshReviewFull})
		f.dispatchExistingRefresh(t, "fix-head", config.RefreshReviewDelta)
		events := f.eventCount(t)
		if _, err := f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "submission refused before side effects") {
			t.Fatalf("err=%v", err)
		}
		f.assertUntouched(t, events, core.TaskQueued)
	})
	t.Run("stale approval without refresh orders", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		if _, err := f.st.MarkTaskApprovalStale(f.ctx, f.task.ID, "approved-head", "fix-head", config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(f.st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskRecoverRefresh, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		events := f.eventCount(t)
		if _, err := f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "submission refused before side effects") {
			t.Fatalf("err=%v", err)
		}
		f.assertUntouched(t, events, core.TaskQueued)
	})
	t.Run("foreign session and changed head", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		if _, err := f.submit("intruder", "fix-head"); err == nil || !strings.Contains(err.Error(), "another session") {
			t.Fatalf("foreign claimed err=%v", err)
		}
		if _, err := f.submit("fixer", "fix-head"); err != nil {
			t.Fatal(err)
		}
		events := f.eventCount(t)
		for _, call := range []struct{ session, head, want string }{
			{session: "intruder", head: "fix-head", want: "another session"},
			{session: "fixer", head: "later-head", want: "already submitted at head"},
			{session: "fixer", head: "", want: "already submitted at head"},
		} {
			if _, err := f.submit(call.session, call.head); err == nil || !strings.Contains(err.Error(), call.want) {
				t.Fatalf("%+v err=%v", call, err)
			}
		}
		if f.eventCount(t) != events || f.reads != 1 {
			t.Fatalf("refused replays wrote events or read the PR: events %d→%d reads=%d", events, f.eventCount(t), f.reads)
		}
		if _, err := f.service.PullRequestTemplate(f.ctx, f.order.ID, "intruder"); err == nil {
			t.Fatal("foreign session read the submitted template")
		}
	})
	t.Run("expired and cancelled claims", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
		if err != nil {
			t.Fatal(err)
		}
		order.LeaseExpiresAt = time.Now().Add(-time.Second)
		if err = storetest.For(f.st).UpdateWorkOrder(f.ctx, order); err != nil {
			t.Fatal(err)
		}
		if _, err = f.submit("fixer", "fix-head"); err == nil || f.jobState(t, f.order.JobID) == core.JobDone {
			t.Fatalf("expired claim err=%v", err)
		}
		g := newConflictSubmission(t, config.ExecutionSetup{})
		cancelled, err := g.st.GetWorkOrder(g.ctx, g.order.ID)
		if err != nil {
			t.Fatal(err)
		}
		cancelled.State = core.WorkOrderCancelled
		if err = storetest.For(g.st).UpdateWorkOrder(g.ctx, cancelled, core.WorkOrderCmdCancel); err != nil {
			t.Fatal(err)
		}
		if _, err = g.submit("fixer", "fix-head"); !errors.Is(err, store.ErrWorkOrderCancelled) {
			t.Fatalf("cancelled claim err=%v", err)
		}
	})
	t.Run("submitted order without completed job", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Legacy partial state from the incident: submitted order, running job.
		order.State, order.HeadSHA = core.WorkOrderSubmitted, "fix-head"
		if err = storetest.For(f.st).UpdateWorkOrder(f.ctx, order, core.WorkOrderCmdSubmitForReview); err != nil {
			t.Fatal(err)
		}
		events := f.eventCount(t)
		if _, err = f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "is not complete") || f.eventCount(t) != events {
			t.Fatalf("err=%v events %d→%d", err, events, f.eventCount(t))
		}
	})
}

// barrierLockStore holds the first task side-effect lock until released, so a
// second caller provably contends for the existing lock.
type barrierLockStore struct {
	store.Store
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func (s *barrierLockStore) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	return s.Store.WithTaskSideEffectLock(ctx, taskID, func(ctx context.Context) error {
		first := false
		s.once.Do(func() {
			first = true
			close(s.held)
		})
		if first {
			<-s.release
		}
		return fn(ctx)
	})
}

func TestConflictFixSubmissionAndReadinessSweepRaceCreateOneRefreshRound(t *testing.T) {
	for _, first := range []string{"sweep", "submit"} {
		t.Run(first+" holds the lock first", func(t *testing.T) {
			barrier := &barrierLockStore{held: make(chan struct{}), release: make(chan struct{})}
			f := newConflictSubmission(t, config.ExecutionSetup{}, func(st store.Store) store.Store {
				barrier.Store = st
				return barrier
			})
			d := f.service.Dispatcher
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 1098, State: "open", Mergeable: "MERGEABLE", HeadSHA: "fix-head", URL: "https://github.com/acme/app/pull/1098"}, nil
			}
			merges := 0
			d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
			sweep := func() error {
				_, err := d.ReconcileMergeReadiness(f.ctx)
				return err
			}
			submit := func() error {
				_, err := f.submit("fixer", "fix-head")
				return err
			}
			calls := map[string]func() error{"sweep": sweep, "submit": submit}
			second := "submit"
			if first == "submit" {
				second = "sweep"
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			wg.Add(2)
			go func() { defer wg.Done(); errs <- calls[first]() }()
			<-barrier.held
			go func() { defer wg.Done(); errs <- calls[second]() }()
			time.Sleep(20 * time.Millisecond)
			close(barrier.release)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent %s/%s: %v", first, second, err)
				}
			}
			// The queue materializes the submitted refresh; later polls must
			// neither re-mark the approval nor create another round.
			if err := d.DispatchNow(f.ctx, f.task.ID); err != nil {
				t.Fatal(err)
			}
			for poll := 0; poll < 2; poll++ {
				if err := sweep(); err != nil {
					t.Fatalf("poll %d: %v", poll, err)
				}
			}
			orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			refresh := 0
			for _, order := range orders {
				if order.Stage != core.StageReview {
					continue
				}
				refresh++
				if order.ReviewRound != 1 || order.ReviewKind != "refresh" || order.HeadSHA != "fix-head" || order.BaselineSHA != "approved-head" || order.ReviewScope != config.RefreshReviewDelta {
					t.Fatalf("refresh seat contract=%+v", order)
				}
			}
			if refresh != 1 || f.countKind(t, "approval.stale") != 1 || f.countKind(t, "review.refresh_round_created") != 1 || merges != 0 {
				t.Fatalf("refresh seats=%d stale=%d rounds=%d merges=%d", refresh, f.countKind(t, "approval.stale"), f.countKind(t, "review.refresh_round_created"), merges)
			}
			if f.jobState(t, f.order.JobID) != core.JobDone {
				t.Fatalf("implement job=%s", f.jobState(t, f.order.JobID))
			}
			order, err := f.st.GetWorkOrder(f.ctx, f.order.ID)
			if err != nil || order.State != core.WorkOrderSubmitted || order.HeadSHA != "fix-head" {
				t.Fatalf("order=%+v err=%v", order, err)
			}
		})
	}
}

// handoffFaultStore fails one post-completion handoff step, after the atomic
// order/job completion has committed.
type handoffFaultStore struct {
	store.Store
	failStale, failAdvanceHead, failStageAdvance bool
}

var errInjectedHandoff = errors.New("injected handoff failure")

func (s *handoffFaultStore) MarkTaskApprovalStale(ctx context.Context, id, approved, head, scope, reason string) (bool, error) {
	if s.failStale {
		return false, errInjectedHandoff
	}
	return s.Store.MarkTaskApprovalStale(ctx, id, approved, head, scope, reason)
}

func (s *handoffFaultStore) AdvanceTaskRefreshHead(ctx context.Context, id, head string) error {
	if s.failAdvanceHead {
		return errInjectedHandoff
	}
	return s.Store.AdvanceTaskRefreshHead(ctx, id, head)
}

func (s *handoffFaultStore) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	if s.failStageAdvance && command.Kind == core.TaskStageAdvance {
		return core.Task{}, errInjectedHandoff
	}
	return s.Store.ApplyTaskCommand(ctx, lease, id, command)
}

// assertPartialSubmissionRefused proves a retry of a partial submission never
// claims the missing handoff and writes nothing.
func assertPartialSubmissionRefused(t *testing.T, st store.Store, ctx context.Context, service *Service, orderID, jobID, session, head string) {
	t.Helper()
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || order.State != core.WorkOrderSubmitted || order.HeadSHA != head {
		t.Fatalf("order/job completion is not atomic with the submitted head: %+v err=%v", order, err)
	}
	jobs, err := st.ListJobs(ctx, order.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == jobID && job.State != core.JobDone {
			t.Fatalf("submitted order beside job state %s", job.State)
		}
	}
	task, err := st.GetTask(ctx, order.TaskID)
	if err != nil || task.State != core.TaskRunning {
		t.Fatalf("partial submission advanced the task: %+v err=%v", task, err)
	}
	events, err := st.ListEvents(ctx, order.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := service.SubmitForReview(ctx, orderID, session, head)
		if err == nil || result != nil || !strings.Contains(err.Error(), "review handoff was not recorded") {
			t.Fatalf("retry %d of a partial submission result=%v err=%v", attempt, result, err)
		}
	}
	after, err := st.ListEvents(ctx, order.TaskID)
	if err != nil || len(after) != len(events) {
		t.Fatalf("refused replay wrote events %d→%d err=%v", len(events), len(after), err)
	}
}

func TestConflictFixSubmissionReplayRefusesIncompleteHandoff(t *testing.T) {
	for _, test := range []struct {
		name  string
		fault func(*handoffFaultStore)
	}{
		{name: "stale marking fails", fault: func(s *handoffFaultStore) { s.failStale = true }},
		{name: "stage advance fails", fault: func(s *handoffFaultStore) { s.failStageAdvance = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			faults := &handoffFaultStore{}
			f := newConflictSubmission(t, config.ExecutionSetup{}, func(st store.Store) store.Store {
				faults.Store = st
				return faults
			})
			test.fault(faults)
			if _, err := f.submit("fixer", "fix-head"); !errors.Is(err, errInjectedHandoff) {
				t.Fatalf("injected failure err=%v", err)
			}
			reads := f.reads
			assertPartialSubmissionRefused(t, f.st, f.ctx, f.service, f.order.ID, f.order.JobID, "fixer", "fix-head")
			if f.reads != reads || f.reconciled != 1 {
				t.Fatalf("refused replay read or wrote the PR: reads %d→%d reconciled=%d", reads, f.reads, f.reconciled)
			}
			if test.name == "stage advance fails" && f.countKind(t, "approval.stale") != 1 {
				t.Fatalf("stale mark before the failed advance=%d", f.countKind(t, "approval.stale"))
			}
		})
	}
	t.Run("refresh head advance fails", func(t *testing.T) {
		ctx := store.WithWorkspace(t.Context(), "test")
		faults := &handoffFaultStore{Store: store.NewMemory(), failAdvanceHead: true}
		task := core.Task{ID: "stale-advance", Workspace: "test", Repo: "app", Title: "Fix", Branch: "conveyor/stale-advance", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
		if err := faults.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := faults.MarkTaskApprovalStale(ctx, task.ID, "approved-head", "older-fix-head", config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: task.ID + "-implement-3", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		if err := faults.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(faults).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.For(faults).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "fixer", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("usr-fixer"), OwnerUserID: "usr-fixer", Lease: time.Hour}); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
		d := dispatch.New(faults, cfg, nil)
		d.DisableMemoryQueueForTest()
		service := &Service{Store: faults, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
		prepareSubmissionTest(service)
		if _, err := service.SubmitForReview(ctx, job.ID, "fixer", "abc123"); !errors.Is(err, errInjectedHandoff) {
			t.Fatalf("injected failure err=%v", err)
		}
		assertPartialSubmissionRefused(t, faults, ctx, service, job.ID, job.ID, "fixer", "abc123")
	})
}

func TestConflictFixSubmissionReplayAfterCompleteHandoffWritesNothing(t *testing.T) {
	f := newConflictSubmission(t, config.ExecutionSetup{})
	first, err := f.submit("fixer", "fix-head")
	if err != nil {
		t.Fatal(err)
	}
	events, reads := f.eventCount(t), f.reads
	replay, err := f.submit("fixer", "fix-head")
	if err != nil || replay["replayed"] != true || replay["pr_url"] != first["pr_url"] || replay["refresh_already_dispatched"] != nil {
		t.Fatalf("replay=%v first=%v err=%v", replay, first, err)
	}
	if f.eventCount(t) != events || f.reads != reads || f.reconciled != 1 {
		t.Fatalf("replay wrote state: events %d→%d reads %d→%d reconciled=%d", events, f.eventCount(t), reads, f.reads, f.reconciled)
	}
}

// supersedeRound records the durable setup-change lineage that
// store.CurrentReviewOrders consumes to retire review orders.
func (f *conflictSubmission) supersedeRound(t *testing.T, round int) {
	t.Helper()
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	superseded := []string{}
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == round {
			superseded = append(superseded, order.ID)
		}
	}
	if len(superseded) == 0 {
		t.Fatalf("round %d has no review orders", round)
	}
	if err = f.st.AppendEvent(f.ctx, core.Event{TaskID: f.task.ID, Kind: "task.setup.changed", Payload: core.JSONPayload(map[string]any{"request_id": "supersede-round", "review_transition": map[string]any{"superseded_work_order_ids": superseded}})}); err != nil {
		t.Fatal(err)
	}
	current := store.CurrentReviewOrders(orders, func() []core.Event { events, _ := f.st.ListEvents(f.ctx, f.task.ID); return events }())
	for _, order := range current {
		if order.Stage == core.StageReview && order.ReviewRound == round {
			t.Fatalf("round %d order %s still current after supersession", round, order.ID)
		}
	}
}

func TestConflictFixSubmissionRefusesSupersededRefreshRound(t *testing.T) {
	t.Run("submission", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		f.dispatchExistingRefresh(t, "fix-head", config.RefreshReviewDelta)
		f.supersedeRound(t, 2)
		events := f.eventCount(t)
		if _, err := f.submit("fixer", "fix-head"); err == nil || !strings.Contains(err.Error(), "submission refused before side effects") {
			t.Fatalf("err=%v", err)
		}
		if f.reads != 0 {
			t.Fatalf("superseded refresh read the PR: %d", f.reads)
		}
		f.assertUntouched(t, events, core.TaskQueued)
	})
	t.Run("replay", func(t *testing.T) {
		f := newConflictSubmission(t, config.ExecutionSetup{})
		f.dispatchExistingRefresh(t, "fix-head", config.RefreshReviewDelta)
		if result, err := f.submit("fixer", "fix-head"); err != nil || result["refresh_already_dispatched"] != true {
			t.Fatalf("result=%v err=%v", result, err)
		}
		if replay, err := f.submit("fixer", "fix-head"); err != nil || replay["replayed"] != true || replay["refresh_already_dispatched"] != true {
			t.Fatalf("current refresh replay=%v err=%v", replay, err)
		}
		f.supersedeRound(t, 2)
		events := f.eventCount(t)
		if replay, err := f.submit("fixer", "fix-head"); err == nil || replay != nil || !strings.Contains(err.Error(), "review handoff was not recorded") {
			t.Fatalf("superseded refresh replay=%v err=%v", replay, err)
		}
		if f.eventCount(t) != events || f.reads != 0 || f.reconciled != 0 {
			t.Fatalf("refused replay wrote state: events %d→%d reads=%d reconciled=%d", events, f.eventCount(t), f.reads, f.reconciled)
		}
	})
}
