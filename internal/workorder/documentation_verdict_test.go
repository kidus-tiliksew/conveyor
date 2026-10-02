package workorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestSubmitVerdictDocumentationUnresolvedRemainsRetryable(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	policy := core.DocumentationPolicy{Enabled: true, BaseSHA: strings.Repeat("a", 40), ContentHash: "sha256:" + strings.Repeat("b", 64), Paths: []string{"docs/**"}, NoneStatement: "docs: none", ReasonRequired: true}
	task := core.Task{ID: "documentation-review", Workspace: "test", Repo: "app", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PinTaskDocumentationPolicy(ctx, task.ID, policy); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDocumentationGateEvidence(ctx, core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: "reviewed-head", MatchedPaths: []string{"docs/guide.md"}}); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: storetest.ReviewDoneCriteriaPlan})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: &core.GovernanceSnapshot{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app"}}}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	service := &Service{Store: st, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	review := pipeline.Review{
		Verdict: "approve", ReasonCode: "approved", Summary: "focused checks pass",
		DoneCriteriaCoverage:    &core.DoneCriteriaAssessment{Applicable: true, Summary: "no outstanding criteria", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}},
		DocumentationAssessment: &core.DocumentationAssessment{Applicable: true, Summary: "docs reviewed", UpdatedPaths: []string{"docs/guide.md"}, Unresolved: []string{"docs edit does not cover the behavior change"}, Conflicts: []string{}},
	}
	before, _ := st.ListEvents(ctx, task.ID)
	if _, err = service.SubmitVerdict(ctx, job.ID, "review-session", review); err == nil || !errors.Is(err, store.ErrDocumentationUnresolved) || !strings.Contains(err.Error(), "blocks approve: unresolved findings in unresolved") {
		t.Fatalf("error=%v", err)
	}
	order, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderClaimed || order.SessionID != "review-session" {
		t.Fatalf("claim consumed: %+v err=%v", order, err)
	}
	after, _ := st.ListEvents(ctx, task.ID)
	if string(core.JSONPayload(before)) != string(core.JSONPayload(after)) {
		t.Fatal("refusal appended events")
	}
	review.Verdict, review.ReasonCode, review.Feedback = "changes_requested", "validation", "Update the declared docs"
	if _, err = service.SubmitVerdict(ctx, job.ID, "review-session", review); err != nil {
		t.Fatal(err)
	}
	order, _ = st.GetWorkOrder(ctx, job.ID)
	bounced, _ := st.GetTask(ctx, task.ID)
	if order.State != core.WorkOrderCompleted || bounced.State != core.TaskQueued || bounced.NextStage != core.StageImplement || bounced.ApprovedHeadSHA != "" {
		t.Fatalf("truthful correction failed: order=%+v task=%+v", order, bounced)
	}
}
