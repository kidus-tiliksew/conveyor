package dispatch

import (
	"context"
	"encoding/json"
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
// delta refresh reviews compare the frozen approved head and replacement head.
func RecordedReviewComparisonContext(task core.Task, events []core.Event) (ReviewComparison, error) {
	comparison, err := RecordedReviewComparison(task, events)
	if err != nil {
		return ReviewComparison{}, err
	}
	scope := config.RefreshReviewFull
	if task.ApprovalStale && task.RefreshReviewScope == config.RefreshReviewDelta {
		scope = config.RefreshReviewDelta
	}
	return ReviewComparison{
		Source:      ReviewComparisonSourceGitHub,
		BaselineSHA: comparison.BaseBranch,
		HeadSHA:     comparison.ReviewedHeadSHA,
		Scope:       scope,
	}, nil
}

// RecordedReviewComparison projects the verified commit pair, never mutable
// branch names, into a transient task used solely for review input reads.
func RecordedReviewComparison(task core.Task, events []core.Event) (core.Task, error) {
	if task.ApprovalStale && task.RefreshBaselineSHA != "" && task.RefreshHeadSHA != "" && task.RefreshReviewScope == config.RefreshReviewDelta {
		task.BaseBranch, task.ReviewedHeadSHA = task.RefreshBaselineSHA, task.RefreshHeadSHA
		return task, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var target struct {
			Base string `json:"base_sha"`
			Head string `json:"head_sha"`
		}
		if json.Unmarshal(events[i].Payload, &target) == nil && target.Base != "" && target.Head != "" {
			task.BaseBranch, task.ReviewedHeadSHA = target.Base, target.Head
			return task, nil
		}
	}
	return core.Task{}, fmt.Errorf("task %s has no recorded review comparison", task.ID)
}

func validateDoneCriteriaCoverage(result *pipeline.Review, hasPlan bool) error {
	return store.ValidateDoneCriteriaCoverage(&result.DoneCriteriaCoverage, result.Verdict, hasPlan)
}
