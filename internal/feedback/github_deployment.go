package feedback

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Deployment reports from GitHub (component-production-feedback invariants 1,
// 9 to 11; feature-production-feedback-live FB-4; req-production-feedback
// AC-1.5 and AC-11.1; req-environments AC-1.4 to AC-1.6 and AC-1.9;
// DEC-149(1), (4), and (5)).

// DeploymentStore is the storage a deployment report reads and writes: the
// environments, the suggestions, and the deploy records.
type DeploymentStore interface {
	ListEnvironments(ctx context.Context, query store.EnvironmentListQuery) ([]store.Environment, error)
	RecordEnvironmentReport(ctx context.Context, report store.EnvironmentReport) (bool, error)
	RecordFeedbackGitHubDeploy(ctx context.Context, deploy store.FeedbackGitHubDeploy) (bool, error)
}

// DeploymentReports applies GitHub deployment_status deliveries. The
// workspace comes from the signed receiver through the context, never from
// the payload. It calls no dispatcher and writes nothing to GitHub.
type DeploymentReports struct {
	Store DeploymentStore
	// Repositories returns the workspace configuration's repositories with
	// their GitHub slugs.
	Repositories func(ctx context.Context) ([]Repository, error)
	// Forge returns the forge of the workspace's GitHub App installation
	// token, which the tracked-branch check reads through.
	Forge func(ctx context.Context, workspace string) Forge
	// Recorded hands a recorded deploy to the background resolving and
	// comparing, as an accepted signal is. Nil skips it.
	Recorded func(workspace string, signal store.FeedbackSignal)
	// Logf writes one line for each report that records nothing for a
	// reason a person can act on. Nil discards them.
	Logf func(format string, args ...any)
}

// DeploymentSuccess is the one state that records a deploy.
const DeploymentSuccess = "success"

// BranchCheckError is a GitHub read that failed during the tracked-branch
// check. It names the failure category of the GitHub boundary and nothing
// GitHub sent, so the applier's log line holds no credential or raw
// response. The delivery fails, and the applier retries it and forgets it
// when the retries run out, so a redelivery from GitHub is applied again.
type BranchCheckError struct {
	Category github.ForgeErrorCategory
	err      error
}

func (e *BranchCheckError) Error() string {
	return fmt.Sprintf("check the tracked branch of a deployment report: GitHub read failed with category %s", e.Category)
}

func (e *BranchCheckError) Unwrap() error { return e.err }

func branchCheckError(err error) error {
	return &BranchCheckError{Category: github.ErrorCategory(github.CategorizeError(err)), err: err}
}

func (d DeploymentReports) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Apply applies one deployment report. slug is the payload's
// repository.full_name. A repository that no configured repository names by
// its GitHub slug, compared without regard to case, records nothing.
//
// A GitHub environment name that no unarchived environment lists, compared
// without regard to case, goes to the suggestions whatever the state
// (invariant 11). A listed name records a deploy only for state success and,
// where the environment tracks a branch for the repository, only for a commit
// that is the head of that branch or an ancestor of it (invariant 10).
func (d DeploymentReports) Apply(ctx context.Context, slug string, status github.DeploymentStatus) error {
	workspace, _ := store.WorkspaceFromContext(ctx)
	repositories, err := d.Repositories(ctx)
	if err != nil {
		return err
	}
	var repository Repository
	for _, candidate := range repositories {
		if candidate.Slug != "" && strings.EqualFold(candidate.Slug, slug) {
			repository = candidate
			break
		}
	}
	if repository.Name == "" {
		return nil
	}
	unarchived := false
	environments, err := d.Store.ListEnvironments(ctx, store.EnvironmentListQuery{Archived: &unarchived})
	if err != nil {
		return err
	}
	environment, listed := store.EnvironmentListing(environments, status.Environment)
	if !listed {
		return d.suggest(ctx, workspace, repository, status)
	}
	if status.State != DeploymentSuccess {
		return nil
	}
	if branch := environment.TrackedBranches[repository.Name]; branch != "" {
		onBranch, err := d.onTrackedBranch(ctx, workspace, repository, branch, status.Commit)
		if err != nil {
			return err
		}
		if !onBranch {
			d.logf("feedback_deployment workspace=%s repository=%s environment_id=%s outcome=ignored reason=off_tracked_branch", workspace, repository.Name, environment.ID)
			return nil
		}
	}
	deploy := store.FeedbackGitHubDeploy{EnvironmentID: environment.ID, Repository: repository.Name, Commit: status.Commit, ReportedAt: status.ReportedAt}
	recorded, err := d.Store.RecordFeedbackGitHubDeploy(ctx, deploy)
	var refused *store.FeedbackFieldError
	if errors.As(err, &refused) {
		// The same report would be refused again, so it is not retried.
		d.logf("feedback_deployment workspace=%s repository=%s environment_id=%s outcome=refused field=%s", workspace, repository.Name, environment.ID, refused.Field)
		return nil
	}
	if err != nil || !recorded || d.Recorded == nil {
		return err
	}
	signal, err := store.FeedbackGitHubDeploySignal(deploy)
	if err != nil {
		return nil
	}
	d.Recorded(workspace, signal)
	return nil
}

// suggest hands an unlisted name to the suggestions. The store refuses a
// name that the rules for GitHub environment names refuse, such as one that
// looks like a credential. The same report would be refused again, so the
// refusal is logged without the name and the delivery is not retried.
func (d DeploymentReports) suggest(ctx context.Context, workspace string, repository Repository, status github.DeploymentStatus) error {
	_, err := d.Store.RecordEnvironmentReport(ctx, store.EnvironmentReport{GitHubEnvironment: status.Environment, Repository: repository.Name, ReportedAt: status.ReportedAt})
	var refused *store.EnvironmentFieldError
	if errors.As(err, &refused) {
		d.logf("feedback_deployment workspace=%s repository=%s outcome=refused field=%s", workspace, repository.Name, refused.Field)
		return nil
	}
	return err
}

// onTrackedBranch reports whether commit is the head of the branch or an
// ancestor of it. It reads the branch head through the workspace App and
// compares the deployment commit as base with that head: identical and ahead
// hold the commit, and behind and diverged do not. Every failed read is an
// error, a head that does not resolve included.
func (d DeploymentReports) onTrackedBranch(ctx context.Context, workspace string, repository Repository, branch, commit string) (bool, error) {
	if d.Forge == nil {
		return false, branchCheckError(ErrNoGitHubApp)
	}
	forge := d.Forge(ctx, workspace)
	// A head that does not resolve, a missing branch included, is a failed
	// read like any other. The delivery fails, so the applier retries it and
	// then forgets it, and a redelivery after the head returns records the
	// deploy. Only a comparison answer proves that the commit is off the
	// branch.
	head, err := forge.ReadCommit(ctx, repository.Slug, "heads/"+branch)
	if err != nil {
		return false, branchCheckError(err)
	}
	result, err := forge.Compare(ctx, repository.Slug, commit, head, 0)
	if err != nil {
		return false, branchCheckError(err)
	}
	switch result.Status {
	case store.FeedbackCompareIdentical, store.FeedbackCompareAhead:
		return true, nil
	case store.FeedbackCompareBehind, store.FeedbackCompareDiverged:
		return false, nil
	}
	return false, branchCheckError(fmt.Errorf("unknown comparison status"))
}
