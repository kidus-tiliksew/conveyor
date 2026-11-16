package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ErrReviewedHeadUnavailable marks an approval conflict that the operator can
// resolve by publishing and reviewing a concrete task head (component-work-orders).
var ErrReviewedHeadUnavailable = errors.New("reviewed head SHA is unavailable")

// ReviewBranchChangedPaths reads filenames from the same pushed branch/base
// comparison that supplies the review patch.
func ReviewBranchChangedPaths(ctx context.Context, cfg *config.Config, task core.Task) ([]string, error) {
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return nil, fmt.Errorf("repository %q is not configured", task.Repo)
	}
	return github.CompareChangedPaths(ctx, repo.GitHub, task.BaseBranch, task.ReviewedHeadSHA)
}

// reviewBranchDiff reads the immutable comparison recorded at submission.
func reviewBranchDiff(ctx context.Context, cfg *config.Config, task core.Task) (string, error) {
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return "", fmt.Errorf("repository %q is not configured", task.Repo)
	}
	return github.DiffBetween(ctx, repo.GitHub, task.BaseBranch, task.ReviewedHeadSHA)
}

const ReviewComparisonSourceGitHub = "github_base_head_compare"

// ReviewComparison identifies the immutable GitHub comparison that defines a
// review's scope. The diff bytes remain a separate field so an empty successful
// comparison cannot be confused with unavailable comparison context.
type ReviewComparison struct {
	Source      string `json:"source"`
	BaselineSHA string `json:"baseline_sha"`
	HeadSHA     string `json:"head_sha"`
	Scope       string `json:"scope"`
}

// RecordedReviewComparisonContext labels the same recorded SHA pair consumed
// by review diff resolution. Full reviews compare the submitted base and head;
// delta refresh reviews compare the frozen approved head and replacement head;
// an ordinary head whose submission fixed a delta scope compares the judged
// head and the head (feature-delta-review-rounds DR-4).
func RecordedReviewComparisonContext(task core.Task, events []core.Event) (ReviewComparison, error) {
	comparison, scope, err := RecordedReviewComparison(task, events)
	if err != nil {
		return ReviewComparison{}, err
	}
	return ReviewComparison{
		Source:      ReviewComparisonSourceGitHub,
		BaselineSHA: comparison.BaseBranch,
		HeadSHA:     comparison.ReviewedHeadSHA,
		Scope:       scope,
	}, nil
}

// RecordedReviewComparison projects the verified commit pair, never mutable
// branch names, into a transient task used solely for review input reads, and
// returns the review scope of that pair. While the approval is stale, the
// refresh scope alone decides it (req-review-gates-evidence AC-12.6): a delta
// refresh compares the refresh pair, and a full refresh compares the recorded
// base commit and head. Otherwise the scope and baseline are the ones the
// head's submission recorded, and a record without a scope reads as full
// (feature-delta-review-rounds DR-3, DR-4; AC-12.4).
func RecordedReviewComparison(task core.Task, events []core.Event) (core.Task, string, error) {
	if task.ApprovalStale && task.RefreshBaselineSHA != "" && task.RefreshHeadSHA != "" && task.RefreshReviewScope == config.RefreshReviewDelta {
		task.BaseBranch, task.ReviewedHeadSHA = task.RefreshBaselineSHA, task.RefreshHeadSHA
		return task, config.RefreshReviewDelta, nil
	}
	if comparison, ok := store.LatestSubmissionComparison(events); ok {
		if task.ApprovalStale {
			// The stale label is the one this read has always given.
			scope := config.RefreshReviewFull
			if task.RefreshReviewScope == config.RefreshReviewDelta {
				scope = config.RefreshReviewDelta
			}
			task.BaseBranch, task.ReviewedHeadSHA = comparison.BaseSHA, comparison.HeadSHA
			return task, scope, nil
		}
		task.BaseBranch, task.ReviewedHeadSHA = comparison.ReviewBaselineSHA, comparison.HeadSHA
		return task, comparison.ReviewScope, nil
	}
	return core.Task{}, "", fmt.Errorf("task %s has no recorded review comparison", task.ID)
}

func validateDoneCriteriaCoverage(result *pipeline.Review, hasPlan bool) error {
	return store.ValidateDoneCriteriaCoverage(&result.DoneCriteriaCoverage, result.Verdict, hasPlan)
}
