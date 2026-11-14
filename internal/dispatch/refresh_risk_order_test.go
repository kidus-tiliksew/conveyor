package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// TestRefreshCreatesTheHeadRiskOrder proves that a refresh creates the
// observed head's risk order, bound to the head and the pull request's base
// commit, and cancels the open risk order of the earlier head before the
// approval is marked stale; that an enforcing refresh opens no round until the
// risk order ends, while a record refresh opens its round at once; and that a
// repeated refresh observation creates no second order
// (req-risk-based-review AC-1.6, AC-5.8, AC-5.10, AC-5.11;
// feature-risk-based-review RR-12).
func TestRefreshCreatesTheHeadRiskOrder(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeRecord, riskpolicy.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
			st := store.NewMemory()
			policy := riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
			task := core.Task{ID: "refresh-risk-order", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/refresh-risk-order", State: core.TaskApproved,
				SetupContract: config.PolicyContract{RefreshReview: config.RefreshReviewDelta, Risk: &policy}, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
				t.Fatal(err)
			}
			base := strings.Repeat("c", 40)
			earlier, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
				order, _, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: task.ID,
					Subject: core.RiskOrderSubject{HeadSHA: "approved-head", BaseSHA: base}, ChangedPaths: []string{"internal/old.go"}})
				return order, err
			})
			if err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}},
				Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}}, nil)
			d.DisableMemoryQueueForTest()
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", BaseSHA: base, HeadSHA: "new-head"}, nil
			}
			d.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
				return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
			}
			d.DiscoverRiskDeclaration = func(context.Context, string, string) riskpolicy.DeclarationInput {
				return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
			}
			d.ReviewChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
				return []string{"internal/server.go"}, nil
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = d.MergeApprovedTaskAutomatically(ctx, current)

			orders, err := st.ListTaskRiskOrders(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			head, ok := store.HeadRiskOrderFor(orders, "new-head")
			if !ok || head.Subject.BaseSHA != base || head.Order.State != core.WorkOrderQueued || len(head.ChangedPaths) != 1 || head.ChangedPaths[0] != "internal/server.go" {
				t.Fatalf("refresh head risk order=%+v ok=%t", head, ok)
			}
			if order, getErr := st.GetWorkOrder(ctx, earlier.ID); getErr != nil || order.State != core.WorkOrderCancelled {
				t.Fatalf("earlier head risk order=%+v err=%v", order, getErr)
			}
			after, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ApprovalStale || after.RefreshHeadSHA != "new-head" {
				t.Fatalf("refresh did not begin: %+v", after)
			}
			seats := func() int {
				t.Helper()
				all, listErr := st.ListTaskWorkOrders(ctx, task.ID)
				if listErr != nil {
					t.Fatal(listErr)
				}
				n := 0
				for _, order := range all {
					if order.Stage == core.StageReview {
						n++
					}
				}
				return n
			}
			if mode == riskpolicy.ModeRecord {
				if seats() == 0 {
					t.Fatal("record refresh opened no round")
				}
				return
			}
			if seats() != 0 {
				t.Fatalf("enforcing refresh opened %d seats while the head's risk order is queued", seats())
			}
			// A second observation of the same pair creates nothing.
			current, _ = st.GetTask(ctx, task.ID)
			_ = d.MergeApprovedTaskAutomatically(ctx, current)
			if again, _ := st.ListTaskRiskOrders(ctx, task.ID); len(again) != len(orders) {
				t.Fatalf("repeated refresh created risk orders: %+v", again)
			}
			claim := core.WorkOrderClaim{ClaimantID: "run:risk-agent", SessionID: "risk-agent", ClientToken: "risk-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
			if _, err = storetest.For(st).ClaimWorkOrder(ctx, head.Order.ID, claim); err != nil {
				t.Fatal(err)
			}
			if _, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdSubmitRiskRating, func(lease taskops.TaskLease) (core.RiskRating, error) {
				return st.CompleteRiskOrderCommand(ctx, lease, store.RiskOrderCompleteRequest{WorkOrderID: head.Order.ID,
					Claim: core.WorkOrderClaimIdentity{ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}, Tier: riskpolicy.TierLow, Reason: "small change"})
			}); err != nil {
				t.Fatal(err)
			}
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if seats() == 0 {
				t.Fatal("the refresh round did not open after the risk order completed")
			}
		})
	}
}
