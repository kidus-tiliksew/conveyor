package dispatch

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// HeadPathRiskInput is what RecordHeadPathRisk needs to rate one submitted
// head. Declaration and ChangedPaths are read only when the task's frozen
// risk mode is record or enforce and the head has no path rating yet, so a
// task with mode off and a replayed head make no risk read.
type HeadPathRiskInput struct {
	Task core.Task
	// Repository is the GitHub slug; empty when the repository has none.
	Repository string
	// BaseSHA is the pull request's observed base commit. The declaration is
	// read there and never at the head (req-risk-based-review AC-3.2).
	BaseSHA      string
	HeadSHA      string
	Declaration  func(ctx context.Context, slug, revision string) riskpolicy.DeclarationInput
	ChangedPaths func(ctx context.Context) ([]string, error)
}

// RecordHeadPathRisk records a head's path minimums once, before the head's
// review handoff (req-risk-based-review AC-5.1, AC-5.4, AC-3.4, AC-3.5;
// feature-risk-based-review RR-3). Submission and refresh share it. A
// declaration or changed-path list that cannot be read gives high with a
// recorded reason and never refuses the caller; only a store failure returns
// an error. No model is called. It reports the recorded rating, and false
// when the task is not rated or the head already had a rating.
func RecordHeadPathRisk(ctx context.Context, st store.Store, in HeadPathRiskInput) (core.RiskRating, bool, error) {
	if !in.Task.SetupContract.Risk.Enabled() || strings.TrimSpace(in.HeadSHA) == "" {
		return core.RiskRating{}, false, nil
	}
	ratings, err := st.ListTaskRiskRatings(ctx, in.Task.ID)
	if err != nil {
		return core.RiskRating{}, false, fmt.Errorf("read risk ratings: %w", err)
	}
	key := core.PathRiskKey(in.HeadSHA)
	for _, rating := range ratings {
		if rating.Key == key {
			return rating, false, nil
		}
	}
	declaration := riskpolicy.DeclarationInput{State: riskpolicy.DeclarationUnknown}
	switch {
	case in.Repository == "":
		declaration.Reason = "the repository has no GitHub repository to read the declaration from"
	case in.BaseSHA == "":
		declaration.Reason = "the pull request base commit is unknown"
	case in.Declaration == nil:
		declaration.Reason = "no declaration reader is configured"
	default:
		declaration = in.Declaration(ctx, in.Repository, in.BaseSHA)
	}
	if err := ctx.Err(); err != nil {
		return core.RiskRating{}, false, err
	}
	pathInput := riskpolicy.PathInput{Declaration: declaration, ChangedPathsComplete: true}
	if in.ChangedPaths == nil {
		pathInput.ChangedPathsComplete, pathInput.ChangedPathsReason = false, "no changed-path reader is configured"
	} else if paths, pathErr := in.ChangedPaths(ctx); pathErr != nil {
		if err := ctx.Err(); err != nil {
			return core.RiskRating{}, false, err
		}
		pathInput.ChangedPathsComplete, pathInput.ChangedPathsReason = false, riskReasonText(pathErr.Error())
	} else {
		pathInput.ChangedPaths = paths
	}
	minimum := riskpolicy.PathMinimums(pathInput)
	rating := core.RiskRating{
		TaskID: in.Task.ID, Key: key, Source: core.RiskSourcePathRule, Tier: minimum.Tier,
		HeadSHA: in.HeadSHA, BaseSHA: in.BaseSHA, DeclarationState: declaration.State,
		Reason: pathRiskReason(minimum), MatchedRules: minimum.Matches, HumanApprovals: minimum.HumanApprovals,
	}
	recorded, created, err := st.RecordRiskRating(ctx, rating)
	if err != nil {
		return core.RiskRating{}, false, fmt.Errorf("record path risk rating: %w", err)
	}
	return recorded, created, nil
}

// pathRiskReason states why a head has its path minimum: the high-fallback
// reasons, the matched rules, or that no sensitive path matched.
func pathRiskReason(minimum riskpolicy.PathMinimum) string {
	parts := make([]string, 0, len(minimum.Reasons)+1)
	for _, reason := range minimum.Reasons {
		parts = append(parts, riskReasonText(reason))
	}
	if len(minimum.Matches) > 0 {
		names := make([]string, 0, len(minimum.Matches))
		for _, match := range minimum.Matches {
			names = append(names, fmt.Sprintf("%s (%s)", riskReasonText(match.Description), match.MinTier))
		}
		parts = append(parts, "matched path rules: "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return "no sensitive path rule matched the changed paths"
	}
	return riskReasonText(strings.Join(parts, "; "))
}

// riskReasonText makes recorded text one line of bounded length.
func riskReasonText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > core.MaxRiskReasonRunes {
		runes := []rune(text)
		text = string(runes[:core.MaxRiskReasonRunes-1]) + "…"
	}
	if text == "" {
		return "no reason was given"
	}
	return text
}

// recordRefreshPathRisk rates a refresh head before its approval is marked
// stale and its refresh is dispatched or skipped (req-risk-based-review
// AC-5.1; feature-risk-based-review RR-3).
func (d *Dispatcher) recordRefreshPathRisk(ctx context.Context, cfg *config.Config, task core.Task, observed github.PullRequest) error {
	if !task.SetupContract.Risk.Enabled() {
		return nil
	}
	repo, _ := cfg.Repo(task.Repo)
	_, _, err := RecordHeadPathRisk(ctx, d.Store, HeadPathRiskInput{
		Task: task, Repository: repo.GitHub, BaseSHA: observed.BaseSHA, HeadSHA: observed.HeadSHA,
		Declaration: d.DiscoverRiskDeclaration,
		ChangedPaths: func(ctx context.Context) ([]string, error) {
			changedPaths := d.ReviewChangedPaths
			if changedPaths == nil {
				changedPaths = ReviewBranchChangedPaths
			}
			comparison := task
			comparison.ReviewedHeadSHA, comparison.BaseBranch = observed.HeadSHA, observed.BaseSHA
			if observed.BaseSHA == "" {
				return nil, fmt.Errorf("the pull request base commit is unknown")
			}
			return changedPaths(ctx, cfg, comparison)
		},
	})
	return err
}
