package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func riskTask(t *testing.T, ctx context.Context, st store.Store, id string, mode riskpolicy.Mode) core.Task {
	t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now()}
	if mode != "" {
		policy := riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
		task.SetupContract.Risk = &policy
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestRecordHeadPathRisk covers the shared path-recording operation that
// submission and refresh call (req-risk-based-review AC-1.1, AC-3.2, AC-3.3,
// AC-3.4, AC-3.5, AC-5.1, AC-5.4; feature-risk-based-review RR-2, RR-3).
func TestRecordHeadPathRisk(t *testing.T) {
	base, head := strings.Repeat("b", 40), strings.Repeat("a", 40)
	declared := []byte("schema_version: 1\nrules:\n  - paths: [\"internal/billing/**\"]\n    min_tier: high\n    description: billing\n    min_human_approvals: 2\n")
	for _, tc := range []struct {
		name         string
		mode         riskpolicy.Mode
		repository   string
		declaration  riskpolicy.DeclarationInput
		paths        []string
		pathErr      error
		wantTier     riskpolicy.Tier
		wantApproval int
		wantReason   string
		wantReads    int
		wantRecorded bool
	}{
		{name: "AbsentDeclarationNoMatch", mode: riskpolicy.ModeRecord, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent},
			paths: []string{"internal/server.go"}, wantTier: riskpolicy.TierLow, wantReason: "no sensitive path rule matched", wantReads: 1, wantRecorded: true},
		{name: "BuiltinWorkflowMinimum", mode: riskpolicy.ModeRecord, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent},
			paths: []string{".github/workflows/ci.yml"}, wantTier: riskpolicy.TierMedium, wantApproval: 1, wantReason: "GitHub Actions workflows (medium)", wantReads: 1, wantRecorded: true},
		{name: "DeclaredRule", mode: riskpolicy.ModeEnforce, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationPresent, Bytes: declared},
			paths: []string{"internal/billing/charge.go"}, wantTier: riskpolicy.TierHigh, wantApproval: 2, wantReason: "billing (high)", wantReads: 1, wantRecorded: true},
		{name: "MalformedDeclarationGivesHigh", mode: riskpolicy.ModeRecord, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationPresent, Bytes: []byte("schema_version: 1.5\nrules: []\n")},
			paths: []string{"README.md"}, wantTier: riskpolicy.TierHigh, wantReason: "declaration is malformed", wantReads: 1, wantRecorded: true},
		{name: "UnavailableDeclarationGivesHigh", mode: riskpolicy.ModeRecord, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationUnavailable, Reason: "the declaration blob could not be read"},
			paths: []string{"README.md"}, wantTier: riskpolicy.TierHigh, wantReason: "declaration could not be read: the declaration blob could not be read", wantReads: 1, wantRecorded: true},
		{name: "UnreadPathListGivesHigh", mode: riskpolicy.ModeRecord, repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent},
			pathErr: errors.New("compare lists 300 or more files;\ncompleteness is unknown"), wantTier: riskpolicy.TierHigh, wantReason: "changed paths could not be read in full: compare lists 300 or more files; completeness is unknown", wantReads: 1, wantRecorded: true},
		{name: "NoRepositoryGivesHigh", mode: riskpolicy.ModeRecord, paths: []string{"README.md"},
			wantTier: riskpolicy.TierHigh, wantReason: "declaration state is unknown: the repository has no GitHub repository", wantReads: 0, wantRecorded: true},
		{name: "OffReadsNothing", mode: "", repository: "acme/app", declaration: riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}, paths: []string{".github/workflows/ci.yml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
			st := store.NewMemory()
			task := riskTask(t, ctx, st, "risk-"+strings.ToLower(tc.name), tc.mode)
			reads, pathReads := 0, 0
			input := HeadPathRiskInput{Task: task, Repository: tc.repository, BaseSHA: base, HeadSHA: head,
				Declaration: func(_ context.Context, slug, revision string) riskpolicy.DeclarationInput {
					reads++
					if slug != tc.repository || revision != base {
						t.Fatalf("declaration read at %s@%s, want the base commit %s", slug, revision, base)
					}
					return tc.declaration
				},
				ChangedPaths: func(context.Context) ([]string, error) { pathReads++; return tc.paths, tc.pathErr },
			}
			rating, created, err := RecordHeadPathRisk(ctx, st, input)
			if err != nil {
				t.Fatal(err)
			}
			listed, err := st.ListTaskRiskRatings(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantRecorded {
				if created || reads != 0 || pathReads != 0 || len(listed) != 0 {
					t.Fatalf("mode off: created=%t declaration reads=%d path reads=%d ratings=%d, want none", created, reads, pathReads, len(listed))
				}
				return
			}
			if !created || reads != tc.wantReads || pathReads != 1 || len(listed) != 1 {
				t.Fatalf("created=%t declaration reads=%d path reads=%d ratings=%d", created, reads, pathReads, len(listed))
			}
			if rating.Tier != tc.wantTier || rating.HumanApprovals != tc.wantApproval || !strings.Contains(rating.Reason, tc.wantReason) ||
				rating.Source != core.RiskSourcePathRule || rating.HeadSHA != head || rating.BaseSHA != base || strings.ContainsAny(rating.Reason, "\n\r") {
				t.Fatalf("rating=%+v, want tier %s approvals %d reason containing %q", rating, tc.wantTier, tc.wantApproval, tc.wantReason)
			}
			// A replayed head reuses its record with no risk read and no event.
			again, created, err := RecordHeadPathRisk(ctx, st, input)
			if err != nil || created || again.Seq != rating.Seq || reads != tc.wantReads || pathReads != 1 {
				t.Fatalf("replay: rating=%+v created=%t err=%v reads=%d path reads=%d", again, created, err, reads, pathReads)
			}
			if count, _ := st.CountEvents(ctx, task.ID, core.RiskRatedEvent); count != 1 {
				t.Fatalf("risk events=%d, want 1", count)
			}
		})
	}
}

// TestRefreshRatesHeadBeforeStaleMark proves a refresh head's path minimums
// are recorded at the pull request's base before the approval is marked
// stale and the refresh round opens, and that the rating changes no refresh
// scope, seat, or stage (req-risk-based-review AC-5.1, AC-1.6).
func TestRefreshRatesHeadBeforeStaleMark(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{"", riskpolicy.ModeRecord} {
		t.Run("mode="+string(mode), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
			st := store.NewMemory()
			task := core.Task{ID: "refresh-risk", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/refresh-risk", State: core.TaskApproved,
				SetupContract: config.PolicyContract{RefreshReview: config.RefreshReviewDelta}, CreatedAt: time.Now()}
			if mode != "" {
				policy := riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}
				task.SetupContract.Risk = &policy
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}},
				Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}}, nil)
			base := strings.Repeat("c", 40)
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", BaseSHA: base, HeadSHA: "new-head"}, nil
			}
			d.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
				return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
			}
			reads := 0
			d.DiscoverRiskDeclaration = func(_ context.Context, slug, revision string) riskpolicy.DeclarationInput {
				reads++
				if slug != "acme/app" || revision != base {
					t.Fatalf("declaration read at %s@%s, want base %s", slug, revision, base)
				}
				return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
			}
			d.ReviewChangedPaths = func(_ context.Context, _ *config.Config, comparison core.Task) ([]string, error) {
				if comparison.BaseBranch != base || comparison.ReviewedHeadSHA != "new-head" {
					t.Fatalf("comparison=%s..%s", comparison.BaseBranch, comparison.ReviewedHeadSHA)
				}
				return nil, errors.New("compare lists 300 or more files")
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.MergeApprovedTaskAutomatically(ctx, current); err == nil || !strings.Contains(err.Error(), "refresh review dispatched") {
				t.Fatalf("err=%v", err)
			}
			after, _ := st.GetTask(ctx, task.ID)
			orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
			if !after.ApprovalStale || after.RefreshHeadSHA != "new-head" || len(orders) != 1 || orders[0].ReviewKind != "refresh" || orders[0].ReviewScope != "delta" {
				t.Fatalf("refresh changed: task=%+v orders=%+v", after, orders)
			}
			ratings, err := st.ListTaskRiskRatings(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "" {
				if reads != 0 || len(ratings) != 0 {
					t.Fatalf("mode off: reads=%d ratings=%+v", reads, ratings)
				}
				return
			}
			if reads != 1 || len(ratings) != 1 || ratings[0].HeadSHA != "new-head" || ratings[0].Tier != riskpolicy.TierHigh ||
				!strings.Contains(ratings[0].Reason, "changed paths could not be read in full") {
				t.Fatalf("reads=%d ratings=%+v", reads, ratings)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			rated, firstOrder := -1, -1
			for i, event := range events {
				if event.Kind == core.RiskRatedEvent && rated < 0 {
					rated = i
				}
				if strings.HasPrefix(event.Kind, "work_order.") && firstOrder < 0 {
					firstOrder = i
				}
			}
			if rated < 0 || firstOrder < 0 || rated > firstOrder {
				t.Fatalf("risk.rated at %d, first work order event at %d; the head must be rated before the refresh round opens", rated, firstOrder)
			}
		})
	}
}
