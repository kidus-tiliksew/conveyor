package dispatch

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func deltaOpened(scope, baseline string) core.Event {
	return deltaOpenedAt("base", scope, baseline)
}

// deltaOpenedAt records a submission whose pull request base was base.
func deltaOpenedAt(base, scope, baseline string) core.Event {
	payload := map[string]string{"base_sha": base, "head_sha": "head"}
	if scope != "" {
		payload["review_scope"], payload["review_baseline_sha"] = scope, baseline
	}
	return core.Event{Kind: store.SubmissionComparisonEventKind, Payload: core.JSONPayload(payload)}
}

// TestDeltaReviewRecordedReviewComparison proves the recorded comparison a
// review input reads: a record without a scope and a full record compare the
// base commit with the head, a delta record compares the judged head with the
// head, and a stale approval keeps the refresh rules whatever the record says
// (feature-delta-review-rounds DR-4; req-review-gates-evidence AC-12.4,
// AC-12.6).
func TestDeltaReviewRecordedReviewComparison(t *testing.T) {
	stale := core.Task{ApprovalStale: true, RefreshBaselineSHA: "approved", RefreshHeadSHA: "head", RefreshReviewScope: config.RefreshReviewDelta}
	staleFull := core.Task{ApprovalStale: true, RefreshBaselineSHA: "approved", RefreshHeadSHA: "head", RefreshReviewScope: config.RefreshReviewFull}
	for _, tc := range []struct {
		name                  string
		task                  core.Task
		event                 core.Event
		baseline, head, scope string
	}{
		{name: "Legacy", event: deltaOpened("", ""), baseline: "base", head: "head", scope: config.RefreshReviewFull},
		{name: "Full", event: deltaOpened(store.ReviewScopeFull, "base"), baseline: "base", head: "head", scope: config.RefreshReviewFull},
		{name: "Delta", event: deltaOpened(store.ReviewScopeDelta, "judged"), baseline: "judged", head: "head", scope: store.ReviewScopeDelta},
		{name: "FullAfterBaseMoved", event: deltaOpenedAt("moved-base", store.ReviewScopeFull, "base"), baseline: "base", head: "head", scope: config.RefreshReviewFull},
		{name: "StaleDeltaRefresh", task: stale, event: deltaOpened(store.ReviewScopeDelta, "judged"), baseline: "approved", head: "head", scope: config.RefreshReviewDelta},
		{name: "StaleFullRefresh", task: staleFull, event: deltaOpened(store.ReviewScopeDelta, "judged"), baseline: "base", head: "head", scope: config.RefreshReviewFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comparison, scope, err := RecordedReviewComparison(tc.task, []core.Event{tc.event})
			if err != nil || comparison.BaseBranch != tc.baseline || comparison.ReviewedHeadSHA != tc.head || scope != tc.scope {
				t.Fatalf("comparison=(%q,%q,%q) err=%v, want (%q,%q,%q)", comparison.BaseBranch, comparison.ReviewedHeadSHA, scope, err, tc.baseline, tc.head, tc.scope)
			}
			context, err := RecordedReviewComparisonContext(tc.task, []core.Event{tc.event})
			if err != nil || context.BaselineSHA != tc.baseline || context.Scope != tc.scope {
				t.Fatalf("context=%+v err=%v", context, err)
			}
		})
	}
}

// TestDeltaReviewDispatchCarriesRecordedPair proves that dispatch gives a
// delta head's verify order and review round the recorded delta scope and the
// judged head, keeps an empty review kind on the round, and leaves a full
// head's orders as they are today (feature-delta-review-rounds DR-4;
// component-verification-service invariants 11 and 48).
func TestDeltaReviewDispatchCarriesRecordedPair(t *testing.T) {
	route := config.StageRoute{Execution: config.ExecutionMCP, TimeoutText: "1h"}
	for _, tc := range []struct {
		name, base, scope, baseline, wantScope, wantBaseline string
	}{
		{name: "FullAfterBaseMoved", base: "moved-base", scope: store.ReviewScopeFull, baseline: "base", wantScope: "", wantBaseline: "base"},
		{name: "Delta", scope: store.ReviewScopeDelta, baseline: "judged", wantScope: store.ReviewScopeDelta, wantBaseline: "judged"},
		{name: "Full", scope: store.ReviewScopeFull, baseline: "base", wantScope: "", wantBaseline: "base"},
		{name: "Legacy", wantScope: "", wantBaseline: "base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
				st := store.NewVolatileBackend()
				defer st.Close()
				task := core.Task{ID: core.NewTaskID(), Workspace: "demo", Repo: "repo", State: core.TaskQueued, NextStage: stage, ReviewedHeadSHA: "head"}
				task.SetupContract.VerifyStage = stage == core.StageVerify
				task.SetupContract.Review.Seats = make([]config.ReviewSeat, 2)
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				base := tc.base
				if base == "" {
					base = "base"
				}
				event := deltaOpenedAt(base, tc.scope, tc.baseline)
				event.TaskID = task.ID
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Workspace: "demo", Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}}
				d := New(st, cfg, nil)
				var err error
				if stage == core.StageVerify {
					err = d.createWorkOrder(ctx, cfg, task, route, "")
				} else {
					err = d.createReviewRound(ctx, cfg, task, route)
				}
				if err != nil {
					t.Fatal(err)
				}
				orders, err := st.ListTaskWorkOrders(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(orders) == 0 {
					t.Fatalf("%s created no order", stage)
				}
				for _, order := range orders {
					if order.Stage != stage || order.HeadSHA != "head" || order.BaselineSHA != tc.wantBaseline || order.ReviewScope != tc.wantScope || order.ReviewKind != "" {
						t.Fatalf("%s order=%+v, want scope=%q baseline=%q", stage, order, tc.wantScope, tc.wantBaseline)
					}
				}
			}
		})
	}
}
