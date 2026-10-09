package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	ctx := context.Background()
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
