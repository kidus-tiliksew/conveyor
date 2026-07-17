package dispatch

import (
	"bytes"
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
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"gopkg.in/yaml.v3"
)

type capturingInputAgent struct {
	input inprocess.Input
	calls int
}

func (agent *capturingInputAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.calls++
	agent.input = input
	return inprocess.Result{Output: "```conveyor:triage\n{\"class\":\"feature\",\"automatability\":0.8,\"route\":\"implement\",\"summary\":\"context received\"}\n```"}, nil
}

type artifactContextFailureStore struct {
	store.Store
	listErr bool
	readErr bool
}

func (st *artifactContextFailureStore) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	if st.listErr {
		return nil, errors.New("artifact list unavailable")
	}
	return st.Store.ListArtifacts(ctx)
}

func (st *artifactContextFailureStore) GetArtifactForContext(ctx context.Context, id, taskID, featureID string) (core.Artifact, []byte, error) {
	if st.readErr {
		return core.Artifact{}, nil, errors.New("artifact read unavailable")
	}
	return st.Store.GetArtifactForContext(ctx, id, taskID, featureID)
}

func TestPipelinePreparesTextImageDocumentAndAudioArtifactInputs(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "artifact-context", Workspace: "demo", Repo: "api", Title: "Use attachments", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	largeText := bytes.Repeat([]byte("a"), (1<<20)+17)
	for _, item := range []struct {
		name, contentType string
		content           []byte
	}{
		{name: "large.txt", contentType: "text/plain", content: largeText},
		{name: "design.png", contentType: "image/png", content: []byte("png")},
		{name: "requirements.pdf", contentType: "application/pdf", content: []byte("pdf")},
		{name: "interview.mp3", contentType: "audio/mpeg", content: []byte("mp3")},
	} {
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: item.name, ContentType: item.contentType, TaskID: task.ID}, item.content); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || len(agent.input.Attachments) != 4 {
		t.Fatalf("calls=%d attachments=%+v", agent.calls, agent.input.Attachments)
	}
	kinds := map[inprocess.AttachmentKind]int{}
	foundLargeText := false
	for _, attachment := range agent.input.Attachments {
		kinds[attachment.Kind]++
		if attachment.Name == "large.txt" {
			foundLargeText = len(attachment.Content) == len(largeText)
		}
	}
	if !foundLargeText || kinds[inprocess.AttachmentDocument] != 2 || kinds[inprocess.AttachmentImage] != 1 || kinds[inprocess.AttachmentAudio] != 1 {
		t.Fatalf("kinds=%+v foundLargeText=%t", kinds, foundLargeText)
	}
}

func TestPipelineArtifactContextFailuresStopBeforeModelExecution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		contentType string
		listErr     bool
		readErr     bool
		want        string
	}{
		{name: "unsupported", contentType: "application/zip", want: "unsupported context artifact"},
		{name: "list failure", contentType: "text/plain", listErr: true, want: "artifact list unavailable"},
		{name: "read failure", contentType: "text/plain", readErr: true, want: "artifact read unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(context.Background(), "demo")
			base := store.NewMemory()
			task := core.Task{ID: "failure-" + strings.ReplaceAll(test.name, " ", "-"), Workspace: "demo", Repo: "api", Title: "Context failure", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
			if err := base.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, err := base.CreateArtifact(ctx, core.Artifact{Name: "context.bin", ContentType: test.contentType, TaskID: task.ID}, []byte("content")); err != nil {
				t.Fatal(err)
			}
			wrapped := &artifactContextFailureStore{Store: base, listErr: test.listErr, readErr: test.readErr}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			agent := &capturingInputAgent{}
			dispatcher := New(wrapped, &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute}}}}, agent)
			dispatcher.Pack = bundle
			err = dispatcher.DispatchNow(ctx, task.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) || agent.calls != 0 {
				t.Fatalf("error=%v calls=%d", err, agent.calls)
			}
			current, getErr := base.GetTask(ctx, task.ID)
			events, eventErr := base.ListEvents(ctx, task.ID)
			contextFailures := 0
			for _, event := range events {
				if event.Kind == "artifact.context_failed" {
					contextFailures++
				}
			}
			if getErr != nil || eventErr != nil || current.State != core.TaskQueued || contextFailures != 1 {
				t.Fatalf("task=%+v events=%+v errors=%v/%v", current, events, getErr, eventErr)
			}
		})
	}
}

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

func TestSourceIssueNumberEnforcesConfiguredRepository(t *testing.T) {
	for _, source := range []string{"github:acme/api#42", "https://github.com/acme/api/issues/42"} {
		number, err := sourceIssueNumber("acme/api", source)
		if err != nil || number != 42 {
			t.Fatalf("source=%q number=%d err=%v", source, number, err)
		}
	}
	if _, err := sourceIssueNumber("acme/api", "github:other/api#42"); err == nil {
		t.Fatal("cross-repository source issue was accepted")
	}
}

func TestPRBodyClosesDurablyAssociatedIssue(t *testing.T) {
	body := PRBody(core.Task{ID: "task-1", Source: "cli", GitHub: &core.GitHubLifecycle{IssueNumber: 42}})
	if !strings.Contains(body, "Closes #42") {
		t.Fatalf("body=%q", body)
	}
}

func TestSpecApprovalQueuesSourceIssueLifecycle(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "spec-task", Workspace: "test", Repo: "app", Source: "github:acme/app#19", State: core.TaskAwaiting, RecoveryStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Intent\nApproved"})
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "spec-job", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	if err = d.HandleIntervention(ctx, task, job, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok || lifecycle.SpecVersion != spec.Version || lifecycle.SourceIssueNumber != 19 {
		t.Fatalf("lifecycle=%+v ok=%t err=%v", lifecycle, ok, err)
	}
}

func TestIssuePublisherRejectsLifecycleFromDifferentConfiguredRepository(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "repo-boundary", Workspace: "test", Repo: "app", CreatedAt: time.Now()}
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
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "other/app", SpecVersion: spec.Version}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	worker := &githubIssuePublicationWorker{dispatcher: d}
	err = worker.Work(ctx, &river.Job[queueargs.GitHubIssuePublicationArgs]{JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 5}, Args: queueargs.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID}})
	if err == nil || !strings.Contains(err.Error(), "does not match configured") {
		t.Fatalf("error=%v", err)
	}
	lifecycle, _, _ := st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.LastError == "" || lifecycle.State != core.GitHubPublicationRetrying {
		t.Fatalf("lifecycle=%+v", lifecycle)
	}
}

func TestIssuePublisherBoundsAmbiguousRecoveryBeforeOneCreateReauthorization(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "lost-ack", Workspace: "test", Repo: "app", Title: "Lost ack", CreatedAt: time.Now()}
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
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: spec.Version}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	publishCalls := 0
	authorizedCreates := 0
	d.PublishIssue = func(publishCtx context.Context, publication githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		publishCalls++
		switch publishCalls {
		case 1:
			if !publication.AllowCreate {
				t.Fatal("first publication did not permit create")
			}
			if err := publication.BeforeCreate(publishCtx); err != nil {
				t.Fatal(err)
			}
			authorizedCreates++
			return githubtrigger.IssuePublicationResult{}, errors.New("GitHub rejected create before accepting it")
		case 2, 3:
			if publication.AllowCreate {
				t.Fatalf("recovery miss %d permitted an early create", publishCalls-1)
			}
			return githubtrigger.IssuePublicationResult{}, githubtrigger.ErrIssueReconciliationPending
		case 4:
			if !publication.AllowCreate {
				t.Fatal("bounded reconciliation did not reauthorize create")
			}
			if err := publication.BeforeCreate(publishCtx); err != nil {
				t.Fatal(err)
			}
			authorizedCreates++
			return githubtrigger.IssuePublicationResult{Number: 42, URL: "https://github.com/acme/app/issues/42"}, nil
		default:
			t.Fatalf("unexpected publication call %d", publishCalls)
			return githubtrigger.IssuePublicationResult{}, nil
		}
	}
	worker := &githubIssuePublicationWorker{dispatcher: d}
	job := func(attempt int) *river.Job[queueargs.GitHubIssuePublicationArgs] {
		return &river.Job[queueargs.GitHubIssuePublicationArgs]{
			JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt, MaxAttempts: 5},
			Args:   queueargs.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID},
		}
	}
	if err = worker.Work(ctx, job(1)); err == nil {
		t.Fatal("first ambiguous publication succeeded")
	}
	lifecycle, _, _ := st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateState != core.GitHubCreateReconciling || lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != 0 || lifecycle.Attempts != 1 {
		t.Fatalf("lifecycle after failed create=%+v", lifecycle)
	}
	if err = worker.Work(ctx, job(2)); !errors.Is(err, githubtrigger.ErrIssueReconciliationPending) {
		t.Fatalf("first reconciliation error=%v", err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != 1 || authorizedCreates != 1 {
		t.Fatalf("lifecycle after first reconciliation miss=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
	if err = worker.Work(ctx, job(3)); !errors.Is(err, githubtrigger.ErrIssueReconciliationPending) {
		t.Fatalf("second reconciliation error=%v", err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != githubIssueReconciliationMissesBeforeCreate || authorizedCreates != 1 {
		t.Fatalf("lifecycle after bounded reconciliation=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
	if err = worker.Work(ctx, job(4)); err != nil {
		t.Fatal(err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.State != core.GitHubPublicationPublished || lifecycle.CreateState != core.GitHubCreateConfirmed || lifecycle.CreateAttempts != 2 || lifecycle.ReconcileMisses != 0 || lifecycle.IssueNumber != 42 || authorizedCreates != 2 {
		t.Fatalf("lifecycle after reauthorized create=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
}

func TestReconcileGitHubLifecyclesRepairsApprovalOutboxGapOnce(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "reconcile-issue", Workspace: "test", Repo: "app", CreatedAt: time.Now()}
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
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("first reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("second reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
}

func TestReviewHistoryKeepsRequestedChangesAndResolution(t *testing.T) {
	events := []core.Event{
		{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "review_round": 1, "review_seat": 1, "verdict": "changes_requested", "reason_code": "tests", "feedback": "Add coverage."})},
		{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-2", "review_round": 2, "review_seat": 1, "verdict": "approve", "reason_code": "approved"})},
		{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 2, "verdict": "approve"})},
	}
	history := reviewHistory(events)
	if len(history) != 2 || history[0].ResolutionState != "resolved" || history[1].ResolutionState != "accepted" {
		t.Fatalf("history=%+v", history)
	}
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

func (agent *sequenceAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
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

func TestImplementationDispatchSnapshotsNormalizedHarnessAndModel(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "snapshot-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	harness := config.Harness{Name: "codex", MCPTransport: config.MCPTransportTOMLOverride, Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Model: "gpt-5", ModelPolicy: config.ModelPolicyExplicit, EffectiveModel: "gpt-5", Harness: "codex", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP},
	}}}
	dispatcher := New(st, cfg, nil)
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	order := orders[0]
	if order.RequiredModel != "gpt-5" || order.RequiredHarness != "codex" || order.RequiredHarnessConfig == nil || order.RequiredHarnessConfig.Name != "codex" || order.RequiredHarnessConfig.MCPTransport != config.MCPTransportTOMLOverride {
		t.Fatalf("snapshotted order=%+v", order)
	}
}

func TestImplementationDispatchNeverSnapshotsUndeclaredExplicitSymbol(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "reject-symbolic-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	document := implementationModelDocument(config.ModelPolicyExplicit, "subscription", nil)
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.ParseWorkspaceDocument(raw, &config.Config{Workspace: "demo", PackDir: "."}, "symbolic dispatch test"); err == nil || !strings.Contains(err.Error(), `symbolic model "subscription" requires harness_default model policy`) {
		t.Fatalf("explicit symbolic model error=%v", err)
	}
	orders, listErr := st.ListTaskWorkOrders(ctx, task.ID)
	if listErr != nil || len(orders) != 0 {
		t.Fatalf("invalid symbolic model created work orders=%+v err=%v", orders, listErr)
	}
}

func TestImplementationDispatchSnapshotsDeclaredDefaultSentinel(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "declared-symbolic-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	document := implementationModelDocument(config.ModelPolicyHarnessDefault, "subscription", []string{"subscription"})
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseWorkspaceDocument(raw, &config.Config{Workspace: "demo", PackDir: "."}, "declared sentinel dispatch test")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].RequiredModel != "subscription" {
		t.Fatalf("declared sentinel orders=%+v err=%v", orders, err)
	}
}

func implementationModelDocument(policy, model string, sentinels []string) config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2,
		ExecutionSettings: &config.ContextualExecutionSettings{
			ControlPlane: config.ControlPlaneSettings{
				Triage: config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "20m"},
				Spec:   config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "30m"},
			},
			Implementation: config.ImplementationSettings{Harness: "codex", Model: model, ModelPolicy: policy, TimeoutText: "1h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h", FallbackModel: "gpt-review", FallbackHarness: "codex"},
		},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", TimeoutText: "20m", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt", TimeoutText: "30m", Execution: config.ExecutionInProcess},
			"implement": {Model: model, ModelPolicy: policy, Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
			"review":    {Model: "gpt-review", Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Harnesses: []config.Harness{{
			Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"},
			DefaultModelSentinels: sentinels, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s",
		}},
		Repos: []config.Repo{{Name: "api", URL: "https://example.test/api", Base: "main"}},
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

func TestBounceWindowResetsAfterHumanIntervention(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "window-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.UseDurableQueue()

	// Two agent bounces exhaust the unsupervised window and park the task.
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round one"); err != nil {
		t.Fatal(err)
	}
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round two"); err != nil {
		t.Fatal(err)
	}
	parked, err := st.GetTask(ctx, task.ID)
	if err != nil || parked.State != core.TaskAwaiting {
		t.Fatalf("parked task=%+v err=%v", parked, err)
	}
	if limits, _ := st.CountEvents(ctx, task.ID, "pipeline.bounce_limit"); limits != 1 {
		t.Fatalf("bounce_limit events=%d", limits)
	}

	// A human redirect grants a fresh window (spec §21.17): the next bounce
	// re-queues instead of re-parking.
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, JobID: job.ID, ActorID: "operator", ActorRole: core.ActorHuman, Action: core.InterventionRedirect, ReasonCode: "changes-requested", Comment: "keep going"}); err != nil {
		t.Fatal(err)
	}
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round three"); err != nil {
		t.Fatal(err)
	}
	resumed, err := st.GetTask(ctx, task.ID)
	if err != nil || resumed.State != core.TaskQueued || resumed.NextStage != core.StageImplement {
		t.Fatalf("resumed task=%+v err=%v", resumed, err)
	}
	if limits, _ := st.CountEvents(ctx, task.ID, "pipeline.bounce_limit"); limits != 1 {
		t.Fatalf("bounce_limit events after reset=%d", limits)
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
