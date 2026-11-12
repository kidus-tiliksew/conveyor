package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	storepg "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type failingStageOrderStore struct {
	store.Store
	err error
}

type blockingDispatchStore struct {
	store.Store
	taskID  string
	started chan struct{}
	once    sync.Once
}

func (s *blockingDispatchStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	if id != s.taskID {
		return s.Store.GetTask(ctx, id)
	}
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return core.Task{}, ctx.Err()
}

func TestWorkspaceForgeOperationFailsClosedWithoutAppIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 30*time.Second)
	defer cancel()
	st, err := storepg.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	workspace := "forge-fail-closed-" + core.NewTaskID()
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(ctx, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Forge fail closed "+workspace, cfg); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	_, err = dispatcher.PublishIssue(store.WithWorkspace(ctx, workspace), githubtrigger.IssuePublication{
		Repo: "acme/repo", TaskID: "task-without-workspace-token", Title: "must fail before GitHub",
	})
	if githubtrigger.ErrorCategory(err) != githubtrigger.ForgePermission {
		t.Fatalf("category = %q, error = %v", githubtrigger.ErrorCategory(err), err)
	}
	if !strings.Contains(err.Error(), "workspace "+workspace+" GitHub App") || !strings.Contains(err.Error(), "connect the GitHub App in workspace settings") {
		t.Fatalf("permission remedy = %v", err)
	}
}

func (s *failingStageOrderStore) CreateStageWorkOrderCommand(context.Context, taskops.TaskLease, core.Job, core.WorkOrder) (bool, error) {
	return false, s.err
}

func TestReviewPublicationWorkerPostgresProjectionLifecycleIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := store.WithActor(t.Context(), store.SystemActor())
	st, err := storepg.Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := core.NewTaskID()
	workspace := "review-publication-" + suffix
	cfg := dispatchRaceConfig(workspace)
	actorCtx := store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman})
	if _, err = st.CreateWorkspace(actorCtx, workspace, "Review publication "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	task := core.Task{
		ID: "review-publication-" + suffix, Workspace: workspace, Repo: "repo",
		Title: "Publish review", Branch: "conveyor/review-publication-" + suffix,
		BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{
		TaskID: task.ID, JobID: job.ID, ReviewWorkOrderID: job.ID,
		Verdict: "approve", ReasonCode: "approved", Summary: "All criteria pass.",
		ReviewedCommitSHA: "head-1", ReviewerModel: "reviewer", ReviewerSession: "review-session",
		SameModelAsImplementer: "false", PublicationEligible: true,
		ReviewRound: 1, ReviewSeat: 1, PolicyVersion: 1, MergeApproval: true, MaxBounces: 3,
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	publication, err := st.GetReviewPublication(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var completedAt time.Time
	for _, event := range events {
		if event.Kind == "review.completed" {
			completedAt = event.At
			break
		}
	}
	if completedAt.IsZero() || !completedAt.After(publication.CreatedAt) {
		t.Fatalf("production ordering not reproduced: completed_at=%s publication_created_at=%s", completedAt, publication.CreatedAt)
	}

	dispatcher := New(st, cfg, nil)
	var requests []githubtrigger.ReviewPublication
	dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		requests = append(requests, request)
		return githubtrigger.ReviewPublicationResult{CommentID: 71, ReviewedCommitSHA: "head-1"}, nil
	}
	worker := &reviewPublicationWorker{dispatcher: dispatcher}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, job.ID, 1)); err != nil {
		t.Fatal(err)
	}
	publication, err = st.GetReviewPublication(ctx, job.ID)
	if err != nil || publication.State != core.ReviewPublicationPublished || publication.CommentID != 71 {
		t.Fatalf("single-seat publication=%+v err=%v", publication, err)
	}
	if len(requests) != 1 || len(requests[0].History) != 1 || requests[0].History[0].WorkOrderID != job.ID {
		t.Fatalf("single-seat requests=%+v", requests)
	}

	panelTask := core.Task{
		ID: "review-panel-" + suffix, Workspace: workspace, Repo: "repo", Title: "Publish panel",
		Branch: "conveyor/review-panel-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, panelTask); err != nil {
		t.Fatal(err)
	}
	panelIDs := []string{panelTask.ID + "-review-1-seat-1", panelTask.ID + "-review-1-seat-2"}
	for index, id := range panelIDs {
		if err = st.CreateJob(ctx, core.Job{ID: id, TaskID: panelTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
			ReviewWorkOrderID: id, TaskID: panelTask.ID, JobID: id, Verdict: "approve",
			ReasonCode: "approved", Summary: "Seat passes.", ReviewRound: 1, ReviewSeat: index + 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: panelTask.ID, JobID: id, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": id, "verdict": "approve", "reason_code": "approved",
			"summary": "Seat passes.", "review_round": 1, "review_seat": index + 1,
		})}); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
				requests = append(requests, request)
				return githubtrigger.ReviewPublicationResult{CommentID: 81, ReviewedCommitSHA: "panel-head"}, nil
			}
			if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, id, 1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: panelTask.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
		"review_round": 1, "verdict": "approve",
	})}); err != nil {
		t.Fatal(err)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, panelIDs[1], 1)); err != nil {
		t.Fatal(err)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, panelIDs[0], 2)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[1].StatusState != "pending" || requests[2].StatusState != "success" ||
		len(requests[2].History) != 2 {
		t.Fatalf("panel requests=%+v", requests)
	}
	for _, id := range panelIDs {
		stored, getErr := st.GetReviewPublication(ctx, id)
		if getErr != nil || stored.State != core.ReviewPublicationPublished || stored.CommentID != 81 {
			t.Fatalf("panel publication %s=%+v err=%v", id, stored, getErr)
		}
	}

	roundTask := core.Task{
		ID: "review-rounds-" + suffix, Workspace: workspace, Repo: "repo", Title: "Publish review rounds",
		Branch: "conveyor/review-rounds-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, roundTask); err != nil {
		t.Fatal(err)
	}
	roundIDs := []string{roundTask.ID + "-review-1", roundTask.ID + "-review-2"}
	roundVerdicts := []string{"changes_requested", "approve"}
	var roundRequests []githubtrigger.ReviewPublication
	dispatcher.PublishReview = func(_ context.Context, request githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		roundRequests = append(roundRequests, request)
		return githubtrigger.ReviewPublicationResult{CommentID: 121, ReviewedCommitSHA: "round-head"}, nil
	}
	for index, id := range roundIDs {
		round := index + 1
		verdict := roundVerdicts[index]
		reasonCode := "tests"
		feedback := "Add coverage."
		if verdict == "approve" {
			reasonCode = "approved"
			feedback = ""
		}
		if err = st.CreateJob(ctx, core.Job{ID: id, TaskID: roundTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
			ReviewWorkOrderID: id, TaskID: roundTask.ID, JobID: id, Verdict: verdict,
			ReasonCode: reasonCode, Summary: "Round complete.", Feedback: feedback,
			ReviewRound: round, ReviewSeat: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: roundTask.ID, JobID: id, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
			"review_work_order_id": id, "verdict": verdict, "reason_code": reasonCode,
			"summary": "Round complete.", "feedback": feedback, "review_round": round, "review_seat": 1,
		})}); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: roundTask.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
			"review_round": round, "verdict": verdict,
		})}); err != nil {
			t.Fatal(err)
		}
		if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if len(roundRequests) != 2 || len(roundRequests[0].History) != 1 ||
		roundRequests[0].History[0].ResolutionState != "unresolved" ||
		len(roundRequests[1].History) != 2 ||
		roundRequests[1].History[0].ResolutionState != "resolved" ||
		roundRequests[1].History[1].ResolutionState != "accepted" {
		t.Fatalf("later-round requests=%+v", roundRequests)
	}
	for _, id := range roundIDs {
		stored, getErr := st.GetReviewPublication(ctx, id)
		if getErr != nil || stored.State != core.ReviewPublicationPublished || stored.CommentID != 121 {
			t.Fatalf("later-round publication %s=%+v err=%v", id, stored, getErr)
		}
	}

	failureTask := core.Task{
		ID: "review-failure-" + suffix, Workspace: workspace, Repo: "repo", Title: "Retry publication",
		Branch: "conveyor/review-failure-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, failureTask); err != nil {
		t.Fatal(err)
	}
	failureID := failureTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: failureID, TaskID: failureTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
		ReviewWorkOrderID: failureID, TaskID: failureTask.ID, JobID: failureID,
		Verdict: "approve", ReasonCode: "approved", Summary: "Passes.",
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("GitHub comment unavailable")
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{}, wantErr
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, failureID, 1)); !errors.Is(err, wantErr) {
		t.Fatalf("publication failure=%v, want %v", err, wantErr)
	}
	failedPublication, err := st.GetReviewPublication(ctx, failureID)
	if err != nil || failedPublication.State != core.ReviewPublicationRetrying || failedPublication.CommentID != 0 {
		t.Fatalf("retrying publication=%+v err=%v", failedPublication, err)
	}
	authoritativeTask, err := st.GetTask(ctx, failureTask.ID)
	if err != nil || authoritativeTask.State != core.TaskRunning || authoritativeTask.NextStage != core.StageReview {
		t.Fatalf("internal review authority changed: task=%+v err=%v", authoritativeTask, err)
	}
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{CommentID: 91, ReviewedCommitSHA: "retry-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, failureID, 2)); err != nil {
		t.Fatal(err)
	}
	failedPublication, err = st.GetReviewPublication(ctx, failureID)
	if err != nil || failedPublication.State != core.ReviewPublicationPublished || failedPublication.CommentID != 91 ||
		failedPublication.Attempts != 2 {
		t.Fatalf("retried publication=%+v err=%v", failedPublication, err)
	}

	reconcileTask := core.Task{
		ID: "review-reconcile-" + suffix, Workspace: workspace, Repo: "repo", Title: "Reconcile publication",
		Branch: "conveyor/review-reconcile-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, reconcileTask); err != nil {
		t.Fatal(err)
	}
	reconcileID := reconcileTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: reconcileID, TaskID: reconcileTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: reconcileTask.ID, JobID: reconcileID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": reconcileID, "verdict": "approve", "reason_code": "approved",
		"summary": "Reconciled.", "publication_eligible": true,
	})}); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("reconciled publications=%d err=%v", repaired, reconcileErr)
	}
	reconcileCalls := 0
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		reconcileCalls++
		return githubtrigger.ReviewPublicationResult{CommentID: 101, ReviewedCommitSHA: "reconcile-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, reconcileID, 1)); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("duplicate reconciliation publications=%d err=%v", repaired, reconcileErr)
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, reconcileID, 2)); err != nil {
		t.Fatal(err)
	}
	reconciledPublication, err := st.GetReviewPublication(ctx, reconcileID)
	if err != nil || reconciledPublication.State != core.ReviewPublicationPublished ||
		reconciledPublication.CommentID != 101 || reconcileCalls != 1 {
		t.Fatalf("reconciled publication=%+v calls=%d err=%v", reconciledPublication, reconcileCalls, err)
	}

	legacyTask := core.Task{
		ID: "review-legacy-" + suffix, Workspace: workspace, Repo: "repo", Title: "Repair legacy publication",
		Branch: "conveyor/review-legacy-" + suffix, BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC(),
	}
	if err = st.CreateTask(ctx, legacyTask); err != nil {
		t.Fatal(err)
	}
	legacyID := legacyTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: legacyID, TaskID: legacyTask.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueReviewPublication(ctx, core.ReviewPublication{
		ReviewWorkOrderID: legacyID, TaskID: legacyTask.ID, JobID: legacyID,
		Verdict: "approve", ReasonCode: "approved", Summary: "Legacy projection.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Pool().Exec(ctx, `UPDATE review_publications
		SET state='published', comment_id=0 WHERE workspace_id=$1 AND review_work_order_id=$2`,
		workspace, legacyID); err != nil {
		t.Fatal(err)
	}
	if repaired, reconcileErr := st.ReconcileReviewPublications(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("legacy projection repairs=%d err=%v", repaired, reconcileErr)
	}
	legacyPublication, err := st.GetReviewPublication(ctx, legacyID)
	if err != nil || legacyPublication.State != core.ReviewPublicationRetrying || legacyPublication.CommentID != 0 {
		t.Fatalf("legacy publication after repair=%+v err=%v", legacyPublication, err)
	}
	dispatcher.PublishReview = func(context.Context, githubtrigger.ReviewPublication) (githubtrigger.ReviewPublicationResult, error) {
		return githubtrigger.ReviewPublicationResult{CommentID: 111, ReviewedCommitSHA: "legacy-head"}, nil
	}
	if err = worker.Work(ctx, reviewPublicationIntegrationJob(workspace, legacyID, 1)); err != nil {
		t.Fatal(err)
	}
	legacyPublication, err = st.GetReviewPublication(ctx, legacyID)
	if err != nil || legacyPublication.State != core.ReviewPublicationPublished || legacyPublication.CommentID != 111 {
		t.Fatalf("repaired legacy publication=%+v err=%v", legacyPublication, err)
	}
}

func reviewPublicationIntegrationJob(workspace, workOrderID string, attempt int) queue.Job {
	return testJob(queue.ReviewPublicationArgs{WorkspaceID: workspace, ReviewWorkOrderID: workOrderID}, int64(attempt), attempt, 5)
}

func dispatchIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse CONVEYOR_TEST_DATABASE_URL: %v", err)
	}
	if !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("refusing integration database %q: name must end in _test", parsed.Path)
	}
	return databaseURL
}

func dispatchRaceConfig(workspace string) *config.Config {
	return &config.Config{
		Workspace: workspace,
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo.git", GitHub: "acme/repo", Base: "main"}},
	}
}

// TestReviewDispatchReadsOnlyTaskOrdersWithoutSnapshotsPostgresIntegration is
// the PostgreSQL form of TestReviewDispatchReadsOnlyTaskOrdersWithoutSnapshots.
// It copies one template order of another task into 200 rows with one
// set-based INSERT and pins a 64 KiB governance snapshot on every order of the
// workspace, the task's own verify order included, with one set-based UPDATE.
// The review dispatch must read only the task's rows and decode no snapshot
// byte, for the refused and the admitted readiness answer
// (component-work-orders, Snapshot-free fold reads).
func TestReviewDispatchReadsOnlyTaskOrdersWithoutSnapshotsPostgresIntegration(t *testing.T) {
	databaseURL := dispatchIntegrationDatabaseURL(t)
	root := store.WithActor(t.Context(), store.SystemActor())
	st, err := storepg.Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := core.NewTaskID()
	workspace := "snapshot-free-" + suffix
	cfg := dispatchRaceConfig(workspace)
	cfg.Routing.Stages["review"] = config.StageRoute{Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}
	if _, err = st.CreateWorkspace(store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman}), workspace, "Snapshot-free reads "+suffix, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(root, workspace)
	verifyOrder := func(t *testing.T, id string) core.WorkOrder {
		t.Helper()
		task := core.Task{ID: id, Workspace: workspace, Repo: "repo", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head", CreatedAt: time.Now().UTC()}
		task.SetupContract.VerifyStage = true
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: core.StageVerify, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head", QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
		if _, err := storetest.CreateStageWorkOrder(ctx, st, job, order); err != nil {
			t.Fatal(err)
		}
		return order
	}
	template := verifyOrder(t, "foreign-"+suffix)
	task := core.Task{ID: "review-" + suffix}
	verify := verifyOrder(t, task.ID)
	if _, err = storetest.ClaimWorkOrder(ctx, st, verify.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("verifier"), SessionID: "verify-" + suffix, ClientToken: "verify-" + suffix, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}

	// Copy the template into snapshotFixtureOrders rows of the other task.
	rows, err := st.Pool().Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'work_orders' AND is_generated = 'NEVER' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	var columns, values []string
	for rows.Next() {
		var column string
		if err = rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
		if column == "id" {
			values = append(values, "template.id || '-copy-' || copy.n")
		} else {
			values = append(values, "template."+column)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Pool().Exec(ctx, `INSERT INTO work_orders (`+strings.Join(columns, ", ")+`) SELECT `+strings.Join(values, ", ")+
		` FROM work_orders template CROSS JOIN generate_series(1, $3::int) AS copy(n) WHERE template.workspace_id = $1 AND template.id = $2`,
		workspace, template.ID, snapshotFixtureOrders); err != nil {
		t.Fatal(err)
	}
	gov := snapshotFixtureGovernance()
	digest, content := store.EncodeGovernanceSnapshot(&gov)
	served, err := json.Marshal(snapshotFixtureRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Pool().Exec(ctx, `INSERT INTO governance_snapshots (workspace_id, digest, content) VALUES ($1,$2,$3::jsonb) ON CONFLICT DO NOTHING`, workspace, digest, content); err != nil {
		t.Fatal(err)
	}
	tag, err := st.Pool().Exec(ctx, `UPDATE work_orders SET governance_snapshot_digest = $2, served_requirement_snapshot = $3::jsonb WHERE workspace_id = $1`, workspace, digest, string(served))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(snapshotFixtureOrders + 2); tag.RowsAffected() != want {
		t.Fatalf("seeded snapshots on %d orders, want %d", tag.RowsAffected(), want)
	}
	full, err := st.ListWorkOrders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seeded := newWorkOrderReadRecorder(st, task.ID)
	seeded.record("seed", full)
	if seeded.foreignRows != snapshotFixtureOrders+1 || seeded.snapshotBytes < (snapshotFixtureOrders+2)*snapshotFixtureBytes {
		t.Fatalf("fixture holds %d foreign rows and %d snapshot bytes, want %d rows and at least %d bytes", seeded.foreignRows, seeded.snapshotBytes, snapshotFixtureOrders+1, (snapshotFixtureOrders+2)*snapshotFixtureBytes)
	}

	t.Run("Refused", func(t *testing.T) {
		requireReviewDispatchReadsOnlyTaskOrders(t, ctx, st, cfg, task.ID, true)
	})
	t.Run("Admitted", func(t *testing.T) {
		if _, err := st.Pool().Exec(ctx, `UPDATE work_orders SET state = 'completed', verification_context_id = 'context-fixture' WHERE workspace_id = $1 AND id = $2`, workspace, verify.ID); err != nil {
			t.Fatal(err)
		}
		if current, err := st.GetTask(ctx, task.ID); err == nil && current.State == core.TaskQueued {
			if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		if current, err := st.GetTask(ctx, task.ID); err != nil || current.NextStage != core.StageReview {
			t.Fatalf("sealed fixture task did not reach review: next=%s err=%v", current.NextStage, err)
		}
		requireReviewDispatchReadsOnlyTaskOrders(t, ctx, st, cfg, task.ID, false)
	})
}
