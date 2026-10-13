package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func openForgeReconcilePostgres(t *testing.T) (context.Context, store.Store, string) {
	t.Helper()
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 30*time.Second)
	t.Cleanup(cancel)
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	suffix := core.NewTaskID()
	workspace := "forge-merge-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Forge merge "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	return store.WithWorkspace(ctx, workspace), st, workspace
}

func enterPostgresMergeGate(t *testing.T, st store.Store, ctx context.Context, workspace, reviewed string) core.Task {
	t.Helper()
	id := "gate-" + core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: workspace, Repo: "repo", Title: "Forge merge " + id,
		BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageReview,
		ReviewedHeadSHA: reviewed, MergeApproval: true, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	out, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Task.State != core.TaskAwaiting || out.Task.GateKind != core.GateKindMergeApproval || out.Task.ReviewedHeadSHA != reviewed {
		t.Fatalf("gate task=%+v", out.Task)
	}
	return out.Task
}

func openPostgresPullRequest(t *testing.T, st store.Store, ctx context.Context, taskID string, number int, head string) {
	t.Helper()
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{
		"repository": "acme/repo", "number": number, "head_sha": head,
	})}); err != nil {
		t.Fatal(err)
	}
}

func postgresMergedPull(task core.Task, number int, head string) githubtrigger.PullRequest {
	return githubtrigger.PullRequest{
		Number: number, URL: "https://github.com/acme/repo/pull/" + task.ID, State: "closed", Merged: true,
		Mergeable: "UNKNOWN", HeadSHA: head, HeadRef: task.Branch, MergeCommitSHA: "landed-" + task.ID,
		MergedBy: "github-operator", BaseSHA: "base",
	}
}

func TestObservedMergeAtMergeGateReconcilesToMergedIntegration(t *testing.T) {
	ctx, st, workspace := openForgeReconcilePostgres(t)
	task := enterPostgresMergeGate(t, st, ctx, workspace, "reviewed-head")
	openPostgresPullRequest(t, st, ctx, task.ID, 12, "reviewed-head")
	d := New(st, dispatchRaceConfig(workspace), nil)
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	pr := postgresMergedPull(task, 12, "reviewed-head")
	accepted, err := d.ReconcileObservedPullRequest(ctx, "repo", "acme/repo", task.ID, pr)
	if err != nil || !accepted {
		t.Fatalf("accepted=%t err=%v", accepted, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.GateKind != "" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconciled := 0
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		reconciled++
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["result"] != "merged_outside_conveyor" || payload["merged_by"] != "github-operator" {
			t.Fatalf("payload=%v", payload)
		}
	}
	if reconciled != 1 {
		t.Fatalf("merge.reconciled=%d", reconciled)
	}
	repeat, err := d.ReconcileObservedPullRequest(ctx, "repo", "acme/repo", task.ID, pr)
	if err != nil || !repeat {
		t.Fatalf("repeat accepted=%t err=%v", repeat, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "merge.reconciled"); countErr != nil || count != 1 {
		t.Fatalf("repeat events=%d err=%v", count, countErr)
	}
}

func TestObservedMergeAtDifferentHeadRecordsDriftIntegration(t *testing.T) {
	ctx, st, workspace := openForgeReconcilePostgres(t)
	task := enterPostgresMergeGate(t, st, ctx, workspace, "reviewed-head")
	openPostgresPullRequest(t, st, ctx, task.ID, 31, "other-head")
	d := New(st, dispatchRaceConfig(workspace), nil)
	pr := postgresMergedPull(task, 31, "other-head")
	accepted, err := d.ReconcileObservedPullRequest(ctx, "repo", "acme/repo", task.ID, pr)
	if err != nil || accepted {
		t.Fatalf("accepted=%t err=%v", accepted, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	repeat, err := d.ReconcileObservedPullRequest(ctx, "repo", "acme/repo", task.ID, pr)
	if err != nil || repeat {
		t.Fatalf("repeat accepted=%t err=%v", repeat, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "merge.failed"); countErr != nil || count != 1 {
		t.Fatalf("drift events=%d err=%v", count, countErr)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	var marker *store.ActivityMarker
	for i := range markers {
		if markers[i].TaskID == task.ID {
			marker = &markers[i]
			break
		}
	}
	if marker == nil || marker.ForgeFailure == nil || marker.ForgeFailure.Category != "merged_head_mismatch" {
		t.Fatalf("activity marker=%+v", marker)
	}
}

func TestReconcileMergeReadinessReconcilesGateOnApprovedMergedPRIntegration(t *testing.T) {
	ctx, st, workspace := openForgeReconcilePostgres(t)
	id := "approved-" + core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: workspace, Repo: "repo", Title: "Approved forge merge",
		BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskApproved, MergeApproval: true, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d := New(st, dispatchRaceConfig(workspace), nil)
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 41, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "approved-head", MergeCommitSHA: "landed", MergedBy: "github-operator"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	reconciled, err := d.ReconcileMergeReadiness(ctx)
	if err != nil || reconciled != 1 || views != 1 {
		t.Fatalf("reconciled=%d views=%d err=%v", reconciled, views, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		found = payload["result"] == "merged_outside_conveyor"
	}
	if !found {
		t.Fatal("merged_outside_conveyor result missing")
	}
}

func TestApproveAtMergeGateOnMergedPullRequestEndsMergedIntegration(t *testing.T) {
	ctx, st, workspace := openForgeReconcilePostgres(t)
	task := enterPostgresMergeGate(t, st, ctx, workspace, "reviewed-head")
	d := New(st, dispatchRaceConfig(workspace), nil)
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return postgresMergedPull(task, 111, "reviewed-head"), nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	if err := d.HandleIntervention(ctx, task, core.Job{}, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || current.ApprovedHeadSHA != "reviewed-head" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		found = payload["result"] == "merged_outside_conveyor"
	}
	if !found {
		t.Fatal("merged_outside_conveyor result missing")
	}
}
