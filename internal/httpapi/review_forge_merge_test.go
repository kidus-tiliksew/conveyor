package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func TestReviewApproveReconcilesMergedPullRequestAtReviewedHead(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "review-merged", Workspace: "demo", Repo: "api", BaseBranch: "main", Branch: "conveyor/review-merged",
		State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", MergeApproval: true, CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", GitHub: "acme/api"}}}, nil)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{
			Number: 1608, State: "closed", Merged: true, HeadSHA: "reviewed-head", HeadRef: task.Branch,
			MergeCommitSHA: "landed", MergedBy: "github-operator", URL: "https://github.com/acme/api/pull/1608",
		}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnIntervention = d.HandleIntervention
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", strings.NewReader(`{"action":"approve","reason_code":"approved"}`))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Conveyor-Actor", "operator")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"state":"merged"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("task=%+v err=%v", current, err)
	}
}

func TestReviewApproveRacingForgeMergeAnswersConflict(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "review-race", Workspace: "demo", Repo: "api", BaseBranch: "main", Branch: "conveyor/review-race",
		State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", MergeApproval: true, CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", GitHub: "acme/api"}}}, nil)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{
			Number: 1609, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "reviewed-head", HeadRef: task.Branch,
			MergeCommitSHA: "landed", MergedBy: "github-operator", URL: "https://github.com/acme/api/pull/1609",
		}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	s := NewServer(st)
	s.BearerToken = "token"
	// The forge merge reconciles after the handler's transition check and
	// before the approve takes the task lock.
	s.OnIntervention = func(ctx context.Context, stale core.Task, job core.Job, intervention core.Intervention) error {
		if _, err := d.ReadMergeReadiness(ctx, stale); err != nil {
			t.Fatal(err)
		}
		return d.HandleIntervention(ctx, stale, job, intervention)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", strings.NewReader(`{"action":"approve","reason_code":"approved"}`))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Conveyor-Actor", "operator")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.ApprovedHeadSHA != "" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	for _, kind := range []string{"intervention.approve", "approval.bound"} {
		if count, _ := st.CountEvents(ctx, task.ID, kind); count != 0 {
			t.Fatalf("%s events=%d", kind, count)
		}
	}
}
