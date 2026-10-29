package dispatch

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

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
	task := core.Task{ID: "task-1", Source: "cli", GitHub: &core.GitHubLifecycle{IssueNumber: 42}}
	body := PRBody(task, core.Artifact{
		ID: "abc123", Name: "proof `shot`.png", ContentType: "image/png", SizeBytes: 42,
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
		DownloadURL: "https://private.invalid/artifact?token=secret",
	})
	if !strings.Contains(body, "Closes #42") {
		t.Fatalf("body=%q", body)
	}
	if strings.Count(body, "<!-- conveyor:verification-evidence -->") != 1 ||
		!strings.Contains(body, "abc123") || !strings.Contains(body, "proof 'shot'.png") ||
		strings.Contains(body, "private.invalid") || strings.Contains(body, "token=secret") {
		t.Fatalf("unsafe or incomplete evidence body=%q", body)
	}
}

func TestSpecApprovalQueuesSourceIssueLifecycle(t *testing.T) {
	ctx := store.WithActor(context.Background(), store.SystemActor())
	st := store.NewMemory()
	task := core.Task{ID: "spec-task", Workspace: "test", Repo: "app", Source: "github:acme/app#19", State: core.TaskRunning, CreatedAt: time.Now()}
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
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	task = gate.Task
	d := New(st, issuePublishingConfig(), nil)
	if err = d.HandleIntervention(ctx, task, job, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok || lifecycle.SpecVersion != spec.Version || lifecycle.SourceIssueNumber != 19 {
		t.Fatalf("lifecycle=%+v ok=%t err=%v", lifecycle, ok, err)
	}
}

func TestIssuePublisherRejectsLifecycleFromDifferentConfiguredRepository(t *testing.T) {
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	d := New(st, issuePublishingConfig(), nil)
	worker := &githubIssuePublicationWorker{dispatcher: d}
	err = worker.Work(ctx, testJob(queue.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID}, 1, 1, 5))
	if err == nil || !strings.Contains(err.Error(), "does not match configured") {
		t.Fatalf("error=%v", err)
	}
	lifecycle, _, _ := st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.LastError == "" || lifecycle.State != core.GitHubPublicationRetrying {
		t.Fatalf("lifecycle=%+v", lifecycle)
	}
}

func TestIssuePublisherBoundsAmbiguousRecoveryBeforeOneCreateReauthorization(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
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
	d := New(st, issuePublishingConfig(), nil)
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
	job := func(attempt int) queue.Job {
		return testJob(queue.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID}, int64(attempt), attempt, 5)
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	d := New(st, issuePublishingConfig(), nil)
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("first reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("second reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
}

// The lifecycle reconcile reads only its candidates: superseded tasks and
// approved plans without a lifecycle row. Finished tasks whose lifecycle is
// queued are never read, and the repair matches the full-list sweep's
// (component-runtime, Reconcile tick).
func TestReconcileGitHubLifecyclesSkipsPublishedTerminalHistory(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	terminal := storetest.SeedTerminalHistory(t, st, ctx, "test", "app", 40, 20)
	task := core.Task{ID: "reconcile-issue", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/reconcile-issue", State: core.TaskQueued, CreatedAt: time.Now()}
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
	recorder := storetest.NewReadRecorder(st)
	d := New(recorder, issuePublishingConfig(), nil)
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("first reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if lifecycle, ok, getErr := st.GetGitHubLifecycle(ctx, task.ID); getErr != nil || !ok || lifecycle.SpecVersion != spec.Version {
		t.Fatalf("lifecycle=%+v ok=%t err=%v", lifecycle, ok, getErr)
	}
	recorder.RequireNoTerminalReads(t, terminal)
	recorder.Reset()
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("second reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if _, read := recorder.TaskRowState(task.ID); read {
		t.Fatal("second reconcile read the repaired task")
	}
	recorder.RequireNoTerminalReads(t, terminal)
}

// issuePublishingConfig is a workspace with issue publication turned on an
// hour before the test, so every approval the test makes follows it.
func issuePublishingConfig() *config.Config {
	return &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}},
		IssuePublication: config.IssuePublication{Enabled: true, EnabledAt: time.Now().UTC().Add(-time.Hour)}}
}

// issuePublicationWorkspace seeds a memory workspace whose dispatcher reads
// the stored policy document on every call, so a setting change takes effect
// the way an operator's configuration write does.
func issuePublicationWorkspace(t *testing.T) (context.Context, store.Backend, *Dispatcher, func(bool)) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewVolatileBackend()
	deployment := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", URL: "https://github.com/acme/app.git", Base: "main", GitHub: "acme/app"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	d := New(st, nil, nil)
	d.ConfigProvider = func(ctx context.Context) (*config.Config, error) { return st.RuntimeConfig(ctx, deployment) }
	set := func(enabled bool) {
		t.Helper()
		current, err := st.WorkspaceConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next, err := st.RuntimeConfig(ctx, deployment)
		if err != nil {
			t.Fatal(err)
		}
		next.IssuePublication.Enabled = enabled
		if _, err = st.UpdateWorkspaceConfig(ctx, current.Version, next); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, st, d, set
}

// approveThroughGate files a task with a pending plan gate and approves it
// the way an operator does, so the approval path queues any publication.
func approveThroughGate(t *testing.T, ctx context.Context, st store.Store, d *Dispatcher, id string) core.Task {
	t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", Source: "github:acme/app#19", Title: id, BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: "## Intent\nApproved"}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: id + "-spec", TaskID: id, Stage: core.StageSpec, State: core.JobDone}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.HandleIntervention(ctx, gate.Task, job, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	approved, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}

// finishedApprovedTask is a merged or closed task whose plan was approved
// while publication was off, so it has no lifecycle row.
func finishedApprovedTask(t *testing.T, ctx context.Context, st store.Store, id string, state core.TaskState) core.Task {
	t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", Source: "cli", Title: id, BaseBranch: "main", Branch: "conveyor/" + id, State: state, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: id, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, id, spec.Version); err != nil {
		t.Fatal(err)
	}
	return task
}

// waitPastApproval separates approval and turn-on times beyond the
// microsecond precision both durable backends store.
func waitPastApproval(t *testing.T, ctx context.Context, st store.Store, taskID string) {
	t.Helper()
	spec, ok, err := st.GetLatestSpecVersion(ctx, taskID)
	if err != nil || !ok || spec.ApprovedAt.IsZero() {
		t.Fatalf("approval=%+v ok=%t err=%v", spec, ok, err)
	}
	for !time.Now().After(spec.ApprovedAt.Add(time.Millisecond)) {
		runtime.Gosched()
	}
}

func refuseForgeCalls(t *testing.T, d *Dispatcher) {
	d.PublishIssue = func(context.Context, githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		t.Error("issue publication called the forge")
		return githubtrigger.IssuePublicationResult{}, errors.New("forge call refused")
	}
}

// An absent setting is off: approval queues no lifecycle, reconciliation
// repairs none, and no forge call is made for the source issue
// (req-delivery-and-forge AC-8.1, AC-8.2).
func TestIssuePublicationOffQueuesNothingAtApproval(t *testing.T) {
	ctx, st, d, _ := issuePublicationWorkspace(t)
	refuseForgeCalls(t, d)
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IssuePublication.Enabled {
		t.Fatalf("absent setting=%+v, want off", cfg.IssuePublication)
	}
	task := approveThroughGate(t, ctx, st, d, "off-approval")
	if spec, _, _ := st.GetLatestSpecVersion(ctx, task.ID); !spec.Approved {
		t.Fatalf("plan not approved: %+v", spec)
	}
	if _, ok, getErr := st.GetGitHubLifecycle(ctx, task.ID); getErr != nil || ok {
		t.Fatalf("approval with publication off queued a lifecycle ok=%t err=%v", ok, getErr)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if _, ok, _ := st.GetGitHubLifecycle(ctx, task.ID); ok || task.GitHub != nil {
		t.Fatalf("reconcile queued a lifecycle; association=%+v", task.GitHub)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "github_issue.publication_queued"); countErr != nil || count != 0 {
		t.Fatalf("queued events=%d err=%v", count, countErr)
	}
}

// Turning the setting on publishes for a later approval and for no task
// approved earlier, including a finished one, across a reconcile tick
// (req-delivery-and-forge AC-8.3).
func TestIssuePublicationTurnOnPublishesOnlyLaterApprovals(t *testing.T) {
	ctx, st, d, set := issuePublicationWorkspace(t)
	earlierMerged := finishedApprovedTask(t, ctx, st, "earlier-merged", core.TaskMerged)
	earlierClosed := finishedApprovedTask(t, ctx, st, "earlier-closed", core.TaskClosed)
	waitPastApproval(t, ctx, st, earlierClosed.ID)
	set(true)
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IssuePublication.Enabled || cfg.IssuePublication.EnabledAt.IsZero() {
		t.Fatalf("turned-on setting=%+v", cfg.IssuePublication)
	}
	later := approveThroughGate(t, ctx, st, d, "later-approval")
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, later.ID)
	if err != nil || !ok || lifecycle.State != core.GitHubPublicationQueued || lifecycle.SourceIssueNumber != 19 {
		t.Fatalf("later approval lifecycle=%+v ok=%t err=%v", lifecycle, ok, err)
	}
	// A later approval whose queueing was lost before a restart is repaired.
	gap := core.Task{ID: "later-gap", Workspace: "test", Repo: "app", Source: "cli", BaseBranch: "main", Branch: "conveyor/later-gap", State: core.TaskRunning, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, gap); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: gap.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, gap.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if _, ok, _ := st.GetGitHubLifecycle(ctx, gap.ID); !ok {
		t.Fatal("later approval's lost lifecycle was not repaired")
	}
	for _, earlier := range []core.Task{earlierMerged, earlierClosed} {
		if _, ok, getErr := st.GetGitHubLifecycle(ctx, earlier.ID); getErr != nil || ok {
			t.Fatalf("task %s approved before the turn-on was queued ok=%t err=%v", earlier.ID, ok, getErr)
		}
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("repeat reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
}

// Turning the setting off ends a queued, uncreated publication skipped
// without a forge write, and leaves a published issue unchanged
// (req-delivery-and-forge AC-8.4).
func TestIssuePublicationTurnOffSkipsQueuedPublication(t *testing.T) {
	ctx, st, d, set := issuePublicationWorkspace(t)
	set(true)
	queued := approveThroughGate(t, ctx, st, d, "queued-publication")
	published := approveThroughGate(t, ctx, st, d, "published-publication")
	done, _, err := st.GetGitHubLifecycle(ctx, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	done.State, done.IssueNumber, done.IssueURL, done.Outcome = core.GitHubPublicationPublished, 7, "https://github.com/acme/app/issues/7", "created"
	if err = st.UpdateGitHubLifecycle(ctx, done); err != nil {
		t.Fatal(err)
	}
	set(false)
	refuseForgeCalls(t, d)
	worker := &githubIssuePublicationWorker{dispatcher: d}
	for i, taskID := range []string{queued.ID, published.ID} {
		if err = worker.Work(ctx, githubIssuePublicationJob(taskID, i+1)); err != nil {
			t.Fatalf("publication of %s with the setting off: %v", taskID, err)
		}
	}
	skipped, ok, err := st.GetGitHubLifecycle(ctx, queued.ID)
	if err != nil || !ok || skipped.State != core.GitHubPublicationSkipped || skipped.Outcome != "skipped" || skipped.IssueNumber != 0 || skipped.Attempts != 0 || skipped.CreateAttempts != 0 {
		t.Fatalf("queued publication after turn-off=%+v ok=%t err=%v", skipped, ok, err)
	}
	if count, countErr := st.CountEvents(ctx, queued.ID, "github_issue.publication_skipped"); countErr != nil || count != 1 {
		t.Fatalf("skipped events=%d err=%v", count, countErr)
	}
	unchanged, _, _ := st.GetGitHubLifecycle(ctx, published.ID)
	if unchanged.State != core.GitHubPublicationPublished || unchanged.IssueNumber != 7 || unchanged.Outcome != "created" {
		t.Fatalf("published issue changed: %+v", unchanged)
	}
	// Turning the setting on again never revives the skipped publication.
	set(true)
	if err = worker.Work(ctx, githubIssuePublicationJob(queued.ID, 3)); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if again, _, _ := st.GetGitHubLifecycle(ctx, queued.ID); again.State != core.GitHubPublicationSkipped {
		t.Fatalf("skipped publication reopened: %+v", again)
	}
}
