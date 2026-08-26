package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunForgeAuthorIdentityConformance verifies that memory and PostgreSQL use
// the same three-class forge attribution contract for projections and replay.
func RunForgeAuthorIdentityConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	taskID := "forge-author-" + core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: forgeWorkspaceID(ctx), Repo: "app", Title: "Forge author conformance", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskQueued, NextStage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if err := st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: taskID, Repository: "acme/app", SpecVersion: 1}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, taskID)
	if err != nil || !ok || lifecycle.ForgeAuthorClass != core.ForgeAuthorWorkspace || lifecycle.ForgeAuthorUserID != "" {
		t.Fatalf("GitHub lifecycle=%+v ok=%t err=%v", lifecycle, ok, err)
	}
	lifecycle.State = core.GitHubPublicationRetrying
	lifecycle.LastError = "retry"
	if err = st.UpdateGitHubLifecycle(ctx, lifecycle); err != nil {
		t.Fatal(err)
	}
	assertForgeAuthorEvent(t, st, ctx, taskID, "github_issue.publication_queued", core.ForgeAuthorWorkspace, "")
	assertForgeAuthorEvent(t, st, ctx, taskID, "github_issue.publication_retry", core.ForgeAuthorWorkspace, "")

	jobID := taskID + "-review"
	if err = st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	publication := core.ReviewPublication{ReviewWorkOrderID: jobID, TaskID: taskID, JobID: jobID, Verdict: "approve"}
	if err = st.QueueReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	publication, err = st.GetReviewPublication(ctx, jobID)
	if err != nil || publication.ForgeAuthorClass != core.ForgeAuthorWorkspace || publication.ForgeAuthorUserID != "" {
		t.Fatalf("review publication=%+v err=%v", publication, err)
	}
	publication.State = core.ReviewPublicationRetrying
	publication.LastError = "retry"
	if err = st.UpdateReviewPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	assertForgeAuthorEvent(t, st, ctx, taskID, "review.publication_queued", core.ForgeAuthorWorkspace, "")
	assertForgeAuthorEvent(t, st, ctx, taskID, "review.publication_retry", core.ForgeAuthorWorkspace, "")

	replayJobID := taskID + "-replay"
	if err = st.CreateJob(ctx, core.Job{ID: replayJobID, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: replayJobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": replayJobID, "publication_eligible": true, "verdict": "approve",
	})}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReconcileReviewPublications(ctx); err != nil {
		t.Fatal(err)
	}
	replayed, err := st.GetReviewPublication(ctx, replayJobID)
	if err != nil || replayed.ForgeAuthorClass != core.ForgeAuthorWorkspace || replayed.ForgeAuthorUserID != "" {
		t.Fatalf("replayed review publication=%+v err=%v", replayed, err)
	}

	invalidTaskID := taskID + "-invalid"
	if err = st.CreateTask(ctx, core.Task{ID: invalidTaskID, Workspace: forgeWorkspaceID(ctx), Repo: "app", Title: "Invalid forge author", BaseBranch: "main", Branch: "conveyor/" + invalidTaskID, State: core.TaskQueued, NextStage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: invalidTaskID, Repository: "acme/app", SpecVersion: 1, ForgeAuthorClass: core.ForgeAuthorClass("host")})
	if err == nil || !strings.Contains(err.Error(), "unsupported forge author class") {
		t.Fatalf("retired host class err=%v", err)
	}
}

func assertForgeAuthorEvent(t *testing.T, st store.Store, ctx context.Context, taskID, kind string, class core.ForgeAuthorClass, userID string) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		var author struct {
			Class  core.ForgeAuthorClass `json:"forge_author_class"`
			UserID string                `json:"forge_author_user_id"`
		}
		if err = json.Unmarshal(event.Payload, &author); err != nil {
			t.Fatal(err)
		}
		if author.Class == class && author.UserID == userID {
			return
		}
	}
	t.Fatalf("event %s lacks forge author class %s and user %q", kind, class, userID)
}

func forgeWorkspaceID(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}
